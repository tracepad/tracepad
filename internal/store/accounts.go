package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Accounts (spec 028): the people who sign in, the projects they are members
// of, the sessions their browsers carry and the single-use tokens that set a
// password.
//
// Two rules from the rest of this package hold here too. Every change is a
// WriteJob (spec 003 #9), and every check that reads stored state — "is this
// the last owner", "is this token still live", "is that the current password"
// — happens inside the write transaction, because between a read and a commit
// another owner can be demoted, a token can be used and a password can change
// (spec 003 Decision 20).
//
// The third rule is this spec's own: a secret is stored as its hash and never
// otherwise. A key is (spec 001 #8), so a session and an invitation are too —
// a read of the database hands out no live credential.

// Project roles (Decision 2). Two and no third: `viewer` looks and annotates,
// `editor` changes what the project is made of.
const (
	RoleViewer = "viewer"
	RoleEditor = "editor"
	// RoleOwner is never stored in `memberships`. It is what an owner's
	// projects report as their role, because the flag *is* the membership.
	RoleOwner = "owner"
)

// ValidRole reports whether a string is a role a membership may carry.
func ValidRole(role string) bool { return role == RoleViewer || role == RoleEditor }

// Account is one person who signs in.
//
// The password hash is deliberately unexported: nothing above this package
// needs it, and a field that is never reachable is a field that cannot be
// rendered into a response by accident. Verify is the whole of its interface.
type Account struct {
	ID    string
	Email string
	Name  string
	Owner bool
	// Disabled is access taken away reversibly (Decision 12). A disabled
	// account cannot sign in and its sessions are ended at once, but its
	// memberships stay, so enabling it puts everything back.
	Disabled bool
	// Pending is an account that has been invited and has not accepted:
	// it has no password yet, so it cannot sign in (Decision 10).
	Pending     bool
	CreatedAt   int64
	LastLoginAt *int64

	hash []byte
}

// accountColumns is the one SELECT list every account read shares.
const accountColumns = `id, email, name, password_hash, owner, disabled, created_at, last_login_at`

func scanAccount(row interface{ Scan(...any) error }) (*Account, error) {
	var (
		a         Account
		hash      []byte
		owner     int64
		disabled  int64
		lastLogin sql.NullInt64
	)
	if err := row.Scan(&a.ID, &a.Email, &a.Name, &hash, &owner, &disabled, &a.CreatedAt, &lastLogin); err != nil {
		return nil, err
	}
	a.hash = hash
	a.Owner = owner != 0
	a.Disabled = disabled != 0
	a.Pending = len(hash) == 0
	if lastLogin.Valid {
		at := lastLogin.Int64
		a.LastLoginAt = &at
	}
	return &a, nil
}

// AccountByID reads one account, or nil when there is none.
func (s *Store) AccountByID(id string) (*Account, error) {
	return s.oneAccount(`SELECT `+accountColumns+` FROM accounts WHERE id = ?`, id)
}

// AccountByEmail reads one account by its sign-in name. The lookup is
// case-insensitive because the column is `COLLATE NOCASE`: a person who
// capitalised their email in a different mood still signs in to their own
// account, and the stored spelling stays the one the owner typed.
func (s *Store) AccountByEmail(email string) (*Account, error) {
	return s.oneAccount(`SELECT `+accountColumns+` FROM accounts WHERE email = ?`, strings.TrimSpace(email))
}

func (s *Store) oneAccount(query string, args ...any) (*Account, error) {
	account, err := scanAccount(s.db.QueryRow(query, args...))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return account, nil
}

// ListAccounts returns every account sorted by email, which is the order the
// Accounts table shows and the order `tracepad accounts ls` prints.
func (s *Store) ListAccounts() ([]*Account, error) {
	rows, err := s.db.Query(`SELECT ` + accountColumns + ` FROM accounts ORDER BY email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	accounts := []*Account{}
	for rows.Next() {
		account, err := scanAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

// standingOwner is what "an owner" means everywhere it is counted: the flag,
// switched on, on an account that can actually sign in (spec 028 #22).
//
// Both halves of the tail are the same rule. A *disabled* owner is refused at
// the login, and a *pending* one — invited and never accepted — has no
// password to be refused against. Counting either would let the invariant this
// predicate exists for be satisfied by somebody who cannot open the door:
// promote a second owner, watch them not accept, stand down, and the server
// has nobody who can run it and prints no setup link because it believes it
// has an owner.
const standingOwner = `owner = 1 AND disabled = 0 AND password_hash IS NOT NULL`

// EnabledOwners counts the owners that can sign in today. It is what decides
// whether the server still needs setting up (Decision 9) and what "the last
// owner" is measured against (Decision 2).
func (s *Store) EnabledOwners() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM accounts WHERE ` + standingOwner).Scan(&n)
	return n, err
}

// Standing reports an owner who can sign in — the state the last-owner rule
// protects. An owner who is disabled or has never accepted an invitation is
// an owner on paper and nobody at the door.
func (a *Account) Standing() bool {
	return a != nil && a.Owner && !a.Disabled && !a.Pending
}

// Membership is one project an account can reach, with the role it has there.
// The project's name rides along because every reader of this — `me`, the
// accounts listing, the CLI — shows the name and never the id alone.
type Membership struct {
	ProjectID string
	Name      string
	Role      string
}

// Member is the project side of the same row: who can see this project
// (Decision 12). Owners are not listed — they are not membership rows, and the
// screen says "and every owner" rather than pretending they are.
type Member struct {
	AccountID string
	Email     string
	Name      string
	Role      string
}

// AccountProjects lists what an account can reach, sorted by project name.
//
// An owner reaches every live project with the role `owner`; everyone else
// reaches their memberships. Soft-deleted projects are left out of both: they
// are not there to work in, and the Server tab lists them separately with
// `?include=deleted` as before (spec 028, edge cases).
func (s *Store) AccountProjects(account *Account) ([]Membership, error) {
	if account.Owner {
		rows, err := s.db.Query(
			`SELECT id, name FROM projects WHERE deleted_at IS NULL ORDER BY name, id`)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		projects := []Membership{}
		for rows.Next() {
			m := Membership{Role: RoleOwner}
			if err := rows.Scan(&m.ProjectID, &m.Name); err != nil {
				return nil, err
			}
			projects = append(projects, m)
		}
		return projects, rows.Err()
	}
	return s.memberships(account.ID)
}

// Memberships lists an account's rows in `memberships`, whatever its standing.
// This is what an owner reads on the Accounts table — the projects a person
// would have if the owner flag came off — and it is empty for an owner.
func (s *Store) Memberships(accountID string) ([]Membership, error) {
	return s.memberships(accountID)
}

func (s *Store) memberships(accountID string) ([]Membership, error) {
	rows, err := s.db.Query(
		`SELECT m.project_id, p.name, m.role
		   FROM memberships m JOIN projects p ON p.id = m.project_id
		  WHERE m.account_id = ? AND p.deleted_at IS NULL
		  ORDER BY p.name, p.id`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Membership{}
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.ProjectID, &m.Name, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ProjectMembers lists who has a role in one project, by email.
func (s *Store) ProjectMembers(projectID string) ([]Member, error) {
	rows, err := s.db.Query(
		`SELECT a.id, a.email, a.name, m.role
		   FROM memberships m JOIN accounts a ON a.id = m.account_id
		  WHERE m.project_id = ?
		  ORDER BY a.email`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.AccountID, &m.Email, &m.Name, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ProjectRole answers what one account may do in one project: `owner` for an
// owner, the membership's role for a member, and "" for somebody who is
// neither (Decision 3).
func (s *Store) ProjectRole(account *Account, projectID string) (string, error) {
	if account.Owner {
		return RoleOwner, nil
	}
	var role string
	err := s.db.QueryRow(
		`SELECT role FROM memberships WHERE account_id = ? AND project_id = ?`,
		account.ID, projectID).Scan(&role)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read membership: %w", err)
	}
	return role, nil
}

// AccountSession is one browser's sign-in (Decision 4). The name carries
// `Account` for the reason the table's does: a *session* in this API is
// already spec 007's roll-up of the traces one conversation produced, and the
// two must not be mistakable for each other in a signature.
type AccountSession struct {
	// ID is sha256 of the cookie's value, hex. The value itself is in the
	// cookie and nowhere else.
	ID         string
	AccountID  string
	CreatedAt  int64
	LastSeenAt int64
	ExpiresAt  int64
	UserAgent  string
	IP         string
}

const sessionColumns = `id, account_id, created_at, last_seen_at, expires_at, user_agent, ip`

func scanSession(row interface{ Scan(...any) error }) (*AccountSession, error) {
	var s AccountSession
	if err := row.Scan(&s.ID, &s.AccountID, &s.CreatedAt, &s.LastSeenAt,
		&s.ExpiresAt, &s.UserAgent, &s.IP); err != nil {
		return nil, err
	}
	return &s, nil
}

// SessionID is the row id a cookie value resolves to.
func SessionID(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// SessionByCookie resolves a cookie value to its row and the account behind it, or
// nil when there is no such session.
//
// An expired row answers nil before the sweeper reaches it (spec 028, Data
// contract): the sweep is a cadence, not a guarantee, and a session that has
// run out must stop working at the moment it does.
func (s *Store) SessionByCookie(value string, now int64) (*AccountSession, *Account, error) {
	session, err := scanSession(s.db.QueryRow(
		`SELECT `+sessionColumns+` FROM account_sessions WHERE id = ? AND expires_at > ?`,
		SessionID(value), now))
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read session: %w", err)
	}
	account, err := s.AccountByID(session.AccountID)
	if err != nil {
		return nil, nil, err
	}
	// The cascade takes a deleted account's sessions with it, so this is
	// belt and braces; a disabled account is the real case, and its
	// sessions are ended at the moment it is disabled.
	if account == nil || account.Disabled {
		return nil, nil, nil
	}
	return session, account, nil
}

// AccountSessions lists one account's live sessions, newest first.
func (s *Store) AccountSessions(accountID string, now int64) ([]*AccountSession, error) {
	rows, err := s.db.Query(
		`SELECT `+sessionColumns+` FROM account_sessions
		  WHERE account_id = ? AND expires_at > ?
		  ORDER BY created_at DESC, id`, accountID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*AccountSession{}
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

// --- Writes -----------------------------------------------------------------

// AccountCreate is an invitation: an account with no password and, with it,
// the single-use token that sets one (Decision 10). The owner is handed the
// link once and carries it to the person themselves.
type AccountCreate struct {
	Email       string
	Name        string
	Owner       bool
	Memberships []Membership
	// TokenID is sha256 of the invitation token, which the caller minted
	// and will render into the link. Only the hash is stored.
	TokenID   string
	ExpiresAt int64
	Now       int64

	Account *Account
}

func (a *AccountCreate) apply(tx *sql.Tx) error {
	// The uniqueness check is inside the transaction, because the answer to
	// "is this email free" changes and a UNIQUE violation would reach the
	// caller as a 500 rather than as the 409 that names the conflict.
	existing, err := accountByEmail(tx, a.Email)
	if err != nil {
		return err
	}
	if existing != nil {
		return &Rejection{Kind: RejectConflict,
			Message: fmt.Sprintf("an account with the email %q already exists", existing.Email)}
	}
	id, err := NewID()
	if err != nil {
		return err
	}
	owner := 0
	if a.Owner {
		owner = 1
	}
	if _, err := tx.Exec(
		`INSERT INTO accounts (id, email, name, owner, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, a.Email, a.Name, owner, a.Now); err != nil {
		return fmt.Errorf("create account %q: %w", a.Email, err)
	}
	// An owner has every project (Decision 2), so it never carries
	// memberships; the handler refuses the combination before it gets here
	// and this is the second half of that one rule.
	if !a.Owner {
		for _, m := range a.Memberships {
			// The project is looked up here as well as in the handler,
			// because between the two a project can be purged: without
			// this the foreign key fails and a request that deserves a
			// 404 gets a 500.
			project, err := projectByID(tx, m.ProjectID)
			if err != nil {
				return err
			}
			if project == nil {
				return &Rejection{Kind: RejectNotFound,
					Message: "no such project: " + m.ProjectID}
			}
			if err := putMembership(tx, id, m.ProjectID, m.Role, a.Now); err != nil {
				return err
			}
		}
	}
	if err := insertToken(tx, a.TokenID, id, a.Now, a.ExpiresAt); err != nil {
		return err
	}
	a.Account, err = accountByID(tx, id)
	return err
}

// AccountUpdate changes a name, the owner flag or the disabled flag.
type AccountUpdate struct {
	AccountID string
	Name      *string
	Owner     *bool
	Disabled  *bool
	Now       int64

	Account *Account
}

func (a *AccountUpdate) apply(tx *sql.Tx) error {
	account, err := accountByID(tx, a.AccountID)
	if err != nil {
		return err
	}
	if account == nil {
		return &Rejection{Kind: RejectNotFound, Message: "no such account"}
	}

	// Losing owner standing — by demotion or by being switched off — is
	// refused for the last one that has it. The count and the write are one
	// transaction, which is what makes two owners demoting each other at
	// once answer 409 to the second rather than to neither (spec 028, edge
	// cases).
	demoted := a.Owner != nil && !*a.Owner
	switchedOff := a.Disabled != nil && *a.Disabled
	if account.Standing() && (demoted || switchedOff) {
		if err := requireAnotherOwner(tx, account.ID); err != nil {
			return err
		}
	}

	if a.Name != nil {
		if _, err := tx.Exec(`UPDATE accounts SET name = ? WHERE id = ?`, *a.Name, account.ID); err != nil {
			return fmt.Errorf("rename account: %w", err)
		}
	}
	if a.Owner != nil {
		owner := 0
		if *a.Owner {
			owner = 1
		}
		if _, err := tx.Exec(`UPDATE accounts SET owner = ? WHERE id = ?`, owner, account.ID); err != nil {
			return fmt.Errorf("change owner standing: %w", err)
		}
		// Promoting drops the memberships, because an owner has every
		// project and a stale row would come back as a demotion's
		// surprise (Decision 12).
		if *a.Owner {
			if _, err := tx.Exec(`DELETE FROM memberships WHERE account_id = ?`, account.ID); err != nil {
				return fmt.Errorf("drop the memberships of a new owner: %w", err)
			}
		}
	}
	if a.Disabled != nil {
		disabled := 0
		if *a.Disabled {
			disabled = 1
		}
		if _, err := tx.Exec(`UPDATE accounts SET disabled = ? WHERE id = ?`, disabled, account.ID); err != nil {
			return fmt.Errorf("change account standing: %w", err)
		}
		// Taking access away has to take it away now, not at the end of
		// the month a session had left (Decision 12).
		if *a.Disabled {
			if _, err := tx.Exec(`DELETE FROM account_sessions WHERE account_id = ?`, account.ID); err != nil {
				return fmt.Errorf("end the sessions of a disabled account: %w", err)
			}
		}
	}
	a.Account, err = accountByID(tx, account.ID)
	return err
}

// AccountDelete removes an account with its memberships, sessions and tokens.
// It takes nothing else with it: a score does not name its author and nothing
// else references an account (Decision 12).
type AccountDelete struct {
	AccountID string
	Confirm   string

	Account *Account
}

func (a *AccountDelete) apply(tx *sql.Tx) error {
	account, err := accountByID(tx, a.AccountID)
	if err != nil {
		return err
	}
	if account == nil {
		return &Rejection{Kind: RejectNotFound, Message: "no such account"}
	}
	// The echo is the email, the one thing about an account a person means
	// (spec 005 #8, applied by Decision 12), and it is checked here rather
	// than in the handler so that a rename between the preview and the
	// confirmation cannot slip past it.
	if a.Confirm != account.Email {
		return &Rejection{Kind: RejectInvalid, Message: fmt.Sprintf(
			"confirm must be the account's email, %q, for this to happen", account.Email)}
	}
	if account.Standing() {
		if err := requireAnotherOwner(tx, account.ID); err != nil {
			return err
		}
	}
	// The memberships, sessions and tokens go by cascade.
	if _, err := tx.Exec(`DELETE FROM accounts WHERE id = ?`, account.ID); err != nil {
		return fmt.Errorf("delete account: %w", err)
	}
	a.Account = account
	return nil
}

// MembershipPut gives an account a role in a project, or changes the one it
// has.
type MembershipPut struct {
	AccountID string
	ProjectID string
	Role      string
	Now       int64
}

func (m *MembershipPut) apply(tx *sql.Tx) error {
	account, err := accountByID(tx, m.AccountID)
	if err != nil {
		return err
	}
	if account == nil {
		return &Rejection{Kind: RejectNotFound, Message: "no such account"}
	}
	if account.Owner {
		return &Rejection{Kind: RejectConflict, Message: "an owner has every project"}
	}
	project, err := projectByID(tx, m.ProjectID)
	if err != nil {
		return err
	}
	if project == nil {
		return &Rejection{Kind: RejectNotFound, Message: "no such project"}
	}
	return putMembership(tx, m.AccountID, m.ProjectID, m.Role, m.Now)
}

// MembershipDelete takes one project away from one account. Removing a
// membership that is not there is not an error: the state the caller asked for
// is the state that holds.
type MembershipDelete struct {
	AccountID string
	ProjectID string
}

func (m *MembershipDelete) apply(tx *sql.Tx) error {
	_, err := tx.Exec(`DELETE FROM memberships WHERE account_id = ? AND project_id = ?`,
		m.AccountID, m.ProjectID)
	if err != nil {
		return fmt.Errorf("remove membership: %w", err)
	}
	return nil
}

// SessionSeed is what a browser session is made of. Three jobs open one — a
// login, the setup of the first owner, and an accepted invitation — and they
// carry it by embedding rather than repeating five fields, so the handler that
// fills them in is one function (`signIn`) and not three.
type SessionSeed struct {
	// SessionID is sha256 of the cookie value the caller minted. The value
	// itself never reaches the store.
	SessionID string
	ExpiresAt int64
	UserAgent string
	IP        string
	Now       int64
}

// Seed exposes the embedded fields to the one caller that fills them in.
func (s *SessionSeed) Seed() *SessionSeed { return s }

// open writes the session row and stamps the login. Shared by the three ways
// in, so "signed in" means the same thing however it happened.
func (s *SessionSeed) open(tx *sql.Tx, accountID string) error {
	if _, err := tx.Exec(
		`INSERT INTO account_sessions
		   (id, account_id, created_at, last_seen_at, expires_at, user_agent, ip)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		s.SessionID, accountID, s.Now, s.Now, s.ExpiresAt, s.UserAgent, s.IP); err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	if _, err := tx.Exec(`UPDATE accounts SET last_login_at = ? WHERE id = ?`,
		s.Now, accountID); err != nil {
		return fmt.Errorf("stamp the login: %w", err)
	}
	return nil
}

// SessionOpen signs an account in: the session row, and the login stamp that
// says when it last happened. This is the login's job; setup and an accepted
// invitation do more first and then the same thing.
type SessionOpen struct {
	SessionSeed
	AccountID string
}

func (o *SessionOpen) apply(tx *sql.Tx) error {
	return o.open(tx, o.AccountID)
}

// SessionSlide moves a session's expiry forward. It runs at most once a day
// per session (Decision 4), which is what keeps the writer out of every read.
type SessionSlide struct {
	SessionID string
	ExpiresAt int64
	Now       int64
}

func (s *SessionSlide) apply(tx *sql.Tx) error {
	_, err := tx.Exec(
		`UPDATE account_sessions SET last_seen_at = ?, expires_at = ? WHERE id = ?`,
		s.Now, s.ExpiresAt, s.SessionID)
	if err != nil {
		return fmt.Errorf("slide session: %w", err)
	}
	return nil
}

// SessionsEnd deletes sessions: one (sign out), every one of an account
// (disable, re-invite), or every one but the caller's ("sign out everywhere").
type SessionsEnd struct {
	// SessionID ends exactly one session when set.
	SessionID string
	// AccountID ends every session of one account when set. Keep spares
	// the caller's own, which is what "sign out everywhere" means to the
	// person pressing it.
	AccountID string
	Keep      string

	Ended int64
}

func (s *SessionsEnd) apply(tx *sql.Tx) error {
	s.Ended = 0
	var (
		result sql.Result
		err    error
	)
	switch {
	case s.SessionID != "":
		result, err = tx.Exec(`DELETE FROM account_sessions WHERE id = ?`, s.SessionID)
	case s.Keep != "":
		result, err = tx.Exec(`DELETE FROM account_sessions WHERE account_id = ? AND id <> ?`,
			s.AccountID, s.Keep)
	default:
		result, err = tx.Exec(`DELETE FROM account_sessions WHERE account_id = ?`, s.AccountID)
	}
	if err != nil {
		return fmt.Errorf("end sessions: %w", err)
	}
	s.Ended, _ = result.RowsAffected()
	return nil
}

// ErrWrongPassword is what a password change answers when the current one does
// not match. It is a sentinel rather than a Rejection because the status it
// earns — 403, not 400 — is this one endpoint's, and the handler is what knows
// that.
var ErrWrongPassword = errors.New("wrong current password")

// PasswordChange replaces an account's password after checking the current
// one, and ends every other session of that account (Decision 4).
//
// The check is inside the transaction for the ordinary reason: between reading
// the hash and writing the new one the password can change, and a check
// against a hash that is no longer stored is a check against nothing.
type PasswordChange struct {
	AccountID string
	// Current is the password the caller typed to prove it is them.
	Current string
	NewHash []byte
	// Keep is the caller's own session, which a password change does not
	// end.
	Keep string
	// Name rides along when one `PATCH` carries both, so that a wrong
	// current password leaves neither changed. Two jobs would commit the
	// rename and then answer 403, and the person would be looking at an
	// error message beside their new display name.
	Name *string

	Account *Account
}

func (p *PasswordChange) apply(tx *sql.Tx) error {
	account, err := accountByID(tx, p.AccountID)
	if err != nil {
		return err
	}
	if account == nil {
		return &Rejection{Kind: RejectNotFound, Message: "no such account"}
	}
	// Checked before `Verify`, which would otherwise spend its decoy
	// comparison here — a quarter of a second holding the one writer, on a
	// path only a signed-in session reaches and a pending account therefore
	// cannot. The timing this endpoint could leak is nothing: the caller
	// already knows whose account it is.
	if account.Pending {
		return ErrWrongPassword
	}
	if !account.Verify(p.Current) {
		return ErrWrongPassword
	}
	if err := setPassword(tx, account.ID, p.NewHash); err != nil {
		return err
	}
	if p.Name != nil {
		if _, err := tx.Exec(`UPDATE accounts SET name = ? WHERE id = ?`, *p.Name, account.ID); err != nil {
			return fmt.Errorf("rename account: %w", err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM account_sessions WHERE account_id = ? AND id <> ?`,
		account.ID, p.Keep); err != nil {
		return fmt.Errorf("end the other sessions: %w", err)
	}
	p.Account, err = accountByID(tx, account.ID)
	return err
}

// InviteMint issues a fresh single-use token for an existing account and ends
// that account's sessions. This is the password reset (Decision 10): the old
// password goes on working until the link is used, so an owner cannot lock
// somebody out by pressing the button.
//
// Any token the account already had is replaced. A reset whose previous link
// stayed live would be a reset that did not close anything, which is the one
// thing a reset is for.
type InviteMint struct {
	AccountID string
	TokenID   string
	ExpiresAt int64
	Now       int64

	Account *Account
}

func (i *InviteMint) apply(tx *sql.Tx) error {
	account, err := accountByID(tx, i.AccountID)
	if err != nil {
		return err
	}
	if account == nil {
		return &Rejection{Kind: RejectNotFound, Message: "no such account"}
	}
	if _, err := tx.Exec(`DELETE FROM account_tokens WHERE account_id = ?`, account.ID); err != nil {
		return fmt.Errorf("clear the earlier invitations: %w", err)
	}
	if err := insertToken(tx, i.TokenID, account.ID, i.Now, i.ExpiresAt); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM account_sessions WHERE account_id = ?`, account.ID); err != nil {
		return fmt.Errorf("end the sessions of a reset account: %w", err)
	}
	i.Account = account
	return nil
}

// ErrBadToken is a setup, invitation or reset token that is unknown, spent or
// expired. One error for all three, because the difference is not something
// the person holding a link can act on.
var ErrBadToken = errors.New("this link is not valid any more")

// InviteAccept spends a token: it sets the password, deletes the token, opens
// a session and stamps the login. All of it in one transaction, so a token can
// be spent exactly once however many browsers race for it.
type InviteAccept struct {
	SessionSeed
	// TokenID is sha256 of the token in the link.
	TokenID string
	NewHash []byte

	Account *Account
}

func (i *InviteAccept) apply(tx *sql.Tx) error {
	var accountID string
	err := tx.QueryRow(`SELECT account_id FROM account_tokens WHERE id = ? AND expires_at > ?`,
		i.TokenID, i.Now).Scan(&accountID)
	if err == sql.ErrNoRows {
		return ErrBadToken
	}
	if err != nil {
		return fmt.Errorf("read invitation: %w", err)
	}
	account, err := accountByID(tx, accountID)
	if err != nil {
		return err
	}
	if account == nil || account.Disabled {
		return ErrBadToken
	}
	if _, err := tx.Exec(`DELETE FROM account_tokens WHERE id = ?`, i.TokenID); err != nil {
		return fmt.Errorf("spend the invitation: %w", err)
	}
	// An invitation for an account that already had a password replaces the
	// hash rather than blanking it (spec 028, edge cases) — which is what
	// `setPassword` does, and the reason the reset can leave the old
	// password live until this moment.
	if err := setPassword(tx, account.ID, i.NewHash); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM account_sessions WHERE account_id = ?`, account.ID); err != nil {
		return fmt.Errorf("end the earlier sessions: %w", err)
	}
	if err := i.open(tx, account.ID); err != nil {
		return err
	}
	i.Account, err = accountByID(tx, account.ID)
	return err
}

// SetupOwner creates the first owner and signs it in (Decision 9). It refuses
// once an enabled owner exists, inside the transaction, so that two browsers
// holding the same setup link cannot both create one.
type SetupOwner struct {
	SessionSeed
	Email string
	Name  string
	Hash  []byte

	Account *Account
}

// ErrSetupDone is the answer to a setup request on a server that already has
// an owner.
var ErrSetupDone = errors.New("this server already has an owner")

func (s *SetupOwner) apply(tx *sql.Tx) error {
	var owners int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM accounts WHERE ` + standingOwner).
		Scan(&owners); err != nil {
		return fmt.Errorf("count owners: %w", err)
	}
	if owners > 0 {
		return ErrSetupDone
	}
	// The email may already belong to a disabled or non-owner account —
	// which is exactly the "the only owner was switched off" recovery. The
	// account is then given the password and the flag rather than being
	// duplicated, because the column is unique and a second row is not a
	// thing the schema can hold.
	account, err := accountByEmail(tx, s.Email)
	if err != nil {
		return err
	}
	if account == nil {
		id, err := NewID()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO accounts (id, email, name, owner, created_at) VALUES (?, ?, ?, 1, ?)`,
			id, s.Email, s.Name, s.Now); err != nil {
			return fmt.Errorf("create the first owner: %w", err)
		}
		account, err = accountByID(tx, id)
		if err != nil {
			return err
		}
	} else {
		name := account.Name
		if s.Name != "" {
			name = s.Name
		}
		if _, err := tx.Exec(
			`UPDATE accounts SET owner = 1, disabled = 0, name = ? WHERE id = ?`,
			name, account.ID); err != nil {
			return fmt.Errorf("promote the first owner: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM memberships WHERE account_id = ?`, account.ID); err != nil {
			return fmt.Errorf("drop the memberships of a new owner: %w", err)
		}
	}
	if err := setPassword(tx, account.ID, s.Hash); err != nil {
		return err
	}
	if err := s.open(tx, account.ID); err != nil {
		return err
	}
	s.Account, err = accountByID(tx, account.ID)
	return err
}

// AccountSweep removes the sessions and invitations that have run out. It is
// the retention sweeper's pass, once per pass rather than once per project:
// an account belongs to the deployment and not to a project (spec 005).
type AccountSweep struct {
	Now   int64
	Limit int

	Sessions int64
	Tokens   int64
}

func (a *AccountSweep) apply(tx *sql.Tx) error {
	a.Sessions, a.Tokens = 0, 0
	sessions, err := tx.Exec(
		`DELETE FROM account_sessions WHERE id IN (
		   SELECT id FROM account_sessions WHERE expires_at <= ? LIMIT ?)`, a.Now, a.Limit)
	if err != nil {
		return fmt.Errorf("sweep expired sessions: %w", err)
	}
	a.Sessions, _ = sessions.RowsAffected()
	tokens, err := tx.Exec(
		`DELETE FROM account_tokens WHERE id IN (
		   SELECT id FROM account_tokens WHERE expires_at <= ? LIMIT ?)`, a.Now, a.Limit)
	if err != nil {
		return fmt.Errorf("sweep expired invitations: %w", err)
	}
	a.Tokens, _ = tokens.RowsAffected()
	return nil
}

// --- Shared statements ------------------------------------------------------

func accountByID(tx *sql.Tx, id string) (*Account, error) {
	return txAccount(tx, `SELECT `+accountColumns+` FROM accounts WHERE id = ?`, id)
}

func accountByEmail(tx *sql.Tx, email string) (*Account, error) {
	return txAccount(tx, `SELECT `+accountColumns+` FROM accounts WHERE email = ?`, email)
}

func txAccount(tx *sql.Tx, query string, args ...any) (*Account, error) {
	account, err := scanAccount(tx.QueryRow(query, args...))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return account, nil
}

// requireAnotherOwner refuses the change that would leave the server with
// nobody able to run it (Decision 2).
func requireAnotherOwner(tx *sql.Tx, exceptID string) error {
	var others int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM accounts WHERE `+standingOwner+` AND id <> ?`, exceptID).
		Scan(&others); err != nil {
		return fmt.Errorf("count owners: %w", err)
	}
	if others == 0 {
		return &Rejection{Kind: RejectConflict, Message: "this is the last owner who can sign in; " +
			"make somebody else an owner — and have them accept the invitation — first, " +
			"or this server has nobody who can run it"}
	}
	return nil
}

func putMembership(tx *sql.Tx, accountID, projectID, role string, now int64) error {
	_, err := tx.Exec(
		`INSERT INTO memberships (account_id, project_id, role, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(account_id, project_id) DO UPDATE SET role = excluded.role`,
		accountID, projectID, role, now)
	if err != nil {
		return fmt.Errorf("grant %s on project %s: %w", role, projectID, err)
	}
	return nil
}

func insertToken(tx *sql.Tx, id, accountID string, now, expiresAt int64) error {
	_, err := tx.Exec(
		`INSERT INTO account_tokens (id, account_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		id, accountID, now, expiresAt)
	if err != nil {
		return fmt.Errorf("create invitation: %w", err)
	}
	return nil
}

func setPassword(tx *sql.Tx, accountID string, hash []byte) error {
	if _, err := tx.Exec(`UPDATE accounts SET password_hash = ? WHERE id = ?`, hash, accountID); err != nil {
		return fmt.Errorf("set password: %w", err)
	}
	return nil
}
