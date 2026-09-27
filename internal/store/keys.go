package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// Project keys and where they came from (spec 045).
//
// A key is a secret whose hash is stored, the project it belongs to, what it
// may do and who minted it. The secret half is never shown, because it is
// never held (spec 005 #12); the rest is what the listing answers, so that
// "which program holds this key, and is it still in use" has an answer before
// somebody revokes it.

// The three things a key may be minted to do (spec 045 #1).
const (
	ScopeIngest = "ingest"
	ScopeRead   = "read"
	ScopeWrite  = "write"
)

// Scopes is the three in their canonical order: the order the schema's CHECK
// spells every combination in, and the order every answer lists them in.
var Scopes = []string{ScopeIngest, ScopeRead, ScopeWrite}

// AllScopes is what a key the server makes by itself may do, and what every
// key that predates scopes was given (spec 045 #5): the three, spelled as the
// column stores them.
const AllScopes = "ingest read write"

// CanonicalScopes spells a set of scopes as the column stores it — the words
// in the canonical order, each once — or reports false for a set that is empty
// or holds a word that is not one of the three (spec 045 #6).
func CanonicalScopes(words []string) (string, bool) {
	held := map[string]bool{}
	for _, word := range words {
		if !slices.Contains(Scopes, word) {
			return "", false
		}
		held[word] = true
	}
	canonical := make([]string, 0, len(Scopes))
	for _, word := range Scopes {
		if held[word] {
			canonical = append(canonical, word)
		}
	}
	return strings.Join(canonical, " "), len(canonical) > 0
}

// How a key came to exist (spec 045 #8).
const (
	// MintedByAccount is a key an owner or editor minted while signed in.
	MintedByAccount = "account"
	// MintedByAdminToken is a key minted with TRACEPAD_ADMIN_TOKEN.
	MintedByAdminToken = "admin_token"
	// MintedAtStartup is a key the server created by itself: the first
	// start's `default` project, or one TRACEPAD_PROJECTS declares.
	MintedAtStartup = "startup"
	// MintedBeforeProvenance is a key that existed before schema 0023, when
	// nobody recorded who made it.
	MintedBeforeProvenance = "unknown"
)

// The minter's relation to the key's project now (spec 045 #8), computed when
// the listing is read so that it is never stale. RoleOwner, RoleEditor and
// RoleViewer are the other three.
const (
	// StandingRemoved is an account that has no role in the project any more.
	StandingRemoved = "removed"
	// StandingDisabled is an account whose access was taken away reversibly.
	StandingDisabled = "disabled"
	// StandingDeleted is an account that no longer exists; the email it had
	// when it minted is all that is left of it.
	StandingDeleted = "deleted"
)

// KeyOrigin is who is minting a key: how, and for an account which one and
// its email as it is now — kept on the key, because account deletion is
// exactly when "who minted this" matters and the account row is gone by then.
type KeyOrigin struct {
	Via       string
	AccountID string
	Email     string
}

// OriginAccount is a key minted by a signed-in account.
func OriginAccount(a *Account) KeyOrigin {
	return KeyOrigin{Via: MintedByAccount, AccountID: a.ID, Email: a.Email}
}

// KeyInfo is one stored key as the listing shows it.
type KeyInfo struct {
	PublicKey string
	Name      string
	// Scopes is what the key may do, in the canonical order (spec 045 #1).
	Scopes    []string
	CreatedAt string
	CreatedBy KeyOrigin
	// Standing is the minter's relation to the project now, for a key an
	// account minted; empty otherwise. Only the listing computes it.
	Standing string
	// LastUsedAt is when the key last authenticated a request, in Unix
	// nanoseconds, as far as the last flush wrote it (spec 045 #9); nil
	// until then.
	LastUsedAt *int64
}

// keyColumns is the one SELECT list a key read shares, for a query that names
// `api_keys` as `k`; keyRow is its one reader.
const keyColumns = `k.public_key, k.name, k.scopes, k.created_at, k.created_via,
	COALESCE(k.created_by, ''), k.created_by_email, k.last_used_at`

// keyRow holds a key's columns while a row is scanned.
type keyRow struct {
	key      KeyInfo
	scopes   string
	lastUsed sql.NullInt64
}

// dest is where keyColumns land, in their order.
func (r *keyRow) dest() []any {
	return []any{&r.key.PublicKey, &r.key.Name, &r.scopes, &r.key.CreatedAt,
		&r.key.CreatedBy.Via, &r.key.CreatedBy.AccountID, &r.key.CreatedBy.Email, &r.lastUsed}
}

// done is the key the scanned columns describe.
func (r *keyRow) done() KeyInfo {
	key := r.key
	key.Scopes = strings.Fields(r.scopes)
	if r.lastUsed.Valid {
		at := r.lastUsed.Int64
		key.LastUsedAt = &at
	}
	return key
}

// withTail scans a row whose leading columns another scanner reads, so that
// scanProject stays the one place that knows a project's columns.
type withTail struct {
	row  interface{ Scan(...any) error }
	tail []any
}

func (w withTail) Scan(dest ...any) error { return w.row.Scan(append(dest, w.tail...)...) }

// keyBySecretQuery is built once: it runs on every key-authenticated request.
var keyBySecretQuery = `SELECT ` + prefixed("p", projectColumns) + `, ` + keyColumns + `
	  FROM api_keys k JOIN projects p ON p.id = k.project_id
	 WHERE k.secret_hash = ?`

// KeyBySecret resolves an API secret to its key and the key's project, or two
// nils if the secret is unknown. Lookup is by sha256(secret) against a unique
// index (spec 001 #8), and the project comes back in the same query.
//
// A soft-deleted project resolves too, and the caller decides what its keys
// may still do: everything is refused during the grace window except reading
// the project and restoring it (spec 005 #10), so the deletion is undoable in
// a deployment that has no admin token to undo it with.
func (s *Store) KeyBySecret(ctx context.Context, secret string) (*Project, *KeyInfo, error) {
	hash := sha256.Sum256([]byte(secret))
	var row keyRow
	project, err := scanProject(withTail{
		row:  s.db.QueryRowContext(ctx, keyBySecretQuery, hash[:]),
		tail: row.dest(),
	})
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	key := row.done()
	return project, &key, nil
}

// prefixed qualifies every column of a SELECT list with a table alias.
func prefixed(alias, columns string) string {
	parts := strings.Split(columns, ",")
	for i, part := range parts {
		parts[i] = alias + "." + strings.TrimSpace(part)
	}
	return strings.Join(parts, ", ")
}

// ProjectKeys lists a project's active keys, oldest first (spec 005 #12), each
// with its minter's standing in the project as it is now (spec 045 #8).
func (s *Store) ProjectKeys(ctx context.Context, projectID string) ([]KeyInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+keyColumns+`,
	        a.id IS NOT NULL, COALESCE(a.owner, 0), COALESCE(a.disabled, 0), COALESCE(m.role, '')
	   FROM api_keys k
	   LEFT JOIN accounts a ON a.id = k.created_by
	   LEFT JOIN memberships m ON m.account_id = k.created_by AND m.project_id = k.project_id
	  WHERE k.project_id = ?
	  ORDER BY k.created_at, k.public_key`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := []KeyInfo{}
	for rows.Next() {
		var (
			row                     keyRow
			exists, owner, disabled bool
			role                    string
		)
		if err := rows.Scan(append(row.dest(), &exists, &owner, &disabled, &role)...); err != nil {
			return nil, err
		}
		key := row.done()
		if key.CreatedBy.Via == MintedByAccount {
			key.Standing = standing(exists, owner, disabled, role)
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// standing is the minter's relation to a project, most final first: an
// account that is gone or cannot sign in is that before it is anything else,
// and an owner has no membership row to read a role from (spec 028 #2).
func standing(exists, owner, disabled bool, role string) string {
	switch {
	case !exists:
		return StandingDeleted
	case disabled:
		return StandingDisabled
	case owner:
		return RoleOwner
	case role != "":
		return role
	}
	return StandingRemoved
}

// MintedKey is one key an account minted, as the account-deletion preview
// lists it: in every project, soft-deleted ones included, since a restore
// brings the key back with its project (spec 045 #10).
type MintedKey struct {
	ProjectID   string
	ProjectName string
	Key         KeyInfo
}

// KeysMintedBy lists the keys an account minted that still exist, oldest first.
func (s *Store) KeysMintedBy(ctx context.Context, accountID string) ([]MintedKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.id, p.name, `+keyColumns+`
	   FROM api_keys k JOIN projects p ON p.id = k.project_id
	  WHERE k.created_by = ?
	  ORDER BY k.created_at, k.public_key`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	minted := []MintedKey{}
	for rows.Next() {
		var (
			one MintedKey
			row keyRow
		)
		if err := rows.Scan(append([]any{&one.ProjectID, &one.ProjectName}, row.dest()...)...); err != nil {
			return nil, err
		}
		one.Key = row.done()
		minted = append(minted, one)
	}
	return minted, rows.Err()
}

// insertKey writes one key with everything the schema refuses to default: its
// scopes, spelled canonically, and who made it (spec 045, Data contract).
func insertKey(tx *sql.Tx, projectID string, keys KeyPair, name, scopes string, origin KeyOrigin) error {
	hash := sha256.Sum256([]byte(keys.Secret))
	var createdBy any
	if origin.AccountID != "" {
		createdBy = origin.AccountID
	}
	if _, err := tx.Exec(
		`INSERT INTO api_keys (public_key, secret_hash, project_id, name, scopes,
		                       created_via, created_by, created_by_email)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		keys.PublicKey, hash[:], projectID, name, scopes,
		origin.Via, createdBy, origin.Email); err != nil {
		return fmt.Errorf("create key for project %s: %w", projectID, err)
	}
	return nil
}

// KeyUse records when keys last authenticated a request (spec 045 #9): one job
// a minute for every key seen since the last, public key → Unix nanoseconds.
//
// `max`, so a late job never moves the time back, and a key revoked since it
// was seen matches no row, which is not an error: its use is simply dropped.
type KeyUse struct {
	Seen map[string]int64
}

func (k *KeyUse) apply(tx *sql.Tx) error {
	for publicKey, at := range k.Seen {
		if _, err := tx.Exec(
			`UPDATE api_keys SET last_used_at = MAX(COALESCE(last_used_at, 0), ?)
			  WHERE public_key = ?`, at, publicKey); err != nil {
			return fmt.Errorf("record key use: %w", err)
		}
	}
	return nil
}
