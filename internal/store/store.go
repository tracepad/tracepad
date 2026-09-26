// Package store owns the embedded SQLite database: opening, migrating,
// and the project/API-key model. It is the only package that talks SQL;
// everything above it uses its narrow interface (design: swappable driver).
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store wraps the SQLite database.
type Store struct {
	db   *sql.DB
	path string
	// fresh is true when Open found no pre-existing database file; a fresh
	// database is never backed up before migrations. Determined *before*
	// the first statement touches the file, because creating
	// schema_migrations already materializes it.
	fresh bool
	// mediaUploadKey signs the Langfuse channel's upload URLs (spec 041,
	// Decision 22), read — or minted — once at open.
	mediaUploadKey []byte
}

// Open opens (creating if needed) the database at path and applies pending
// migrations. The data directory is 0700 and the database files 0600, made so
// or tightened to it on every start (spec 044 #13).
func Open(path string) (*Store, error) {
	info, statErr := os.Stat(path)
	fresh := statErr != nil || info.Size() == 0
	if err := secureFiles(path); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	// modernc.org/sqlite accepts pragmas in the DSN, applied in order per
	// new pool connection — busy_timeout must come first so every later
	// pragma (journal_mode included) already waits out lock contention
	// instead of failing with SQLITE_BUSY. WAL for concurrent readers with
	// the single writer; incremental auto_vacuum so retention deletes
	// return disk space (spec 005 #5). auto_vacuum only takes on a
	// database with no tables yet, which is why migration 0005 carries the
	// fallback for a file that predates the pragma.
	//
	// _txlock=immediate makes every transaction take the write lock at
	// BEGIN. Every transaction this binary opens is a write (migrations,
	// project creation, the writer's commit windows), and a deferred one
	// that reads before it writes — which prompt jobs do, to number a
	// version — takes a read snapshot at its first SELECT and is then
	// refused with SQLITE_BUSY_SNAPSHOT when it tries to upgrade, a
	// failure busy_timeout cannot wait out (spec 003 Decision 24).
	//
	// secure_delete(ON) zeroes what a delete frees — the cells inside live
	// pages and the pages that go to the freelist, where raw bodies and
	// large payloads live — instead of leaving the bytes for anyone who can
	// read the file (spec 044 #10). FAST would leave the freed overflow
	// pages intact. There is no setting to turn it off: a guarantee that
	// depends on configuration is not one (spec 005 #9).
	dsn := "file:" + path + "?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=auto_vacuum(INCREMENTAL)&_pragma=synchronous(NORMAL)&_pragma=secure_delete(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	s := &Store{db: db, path: path, fresh: fresh}
	// sql.Open is lazy: real open failures (corrupt file, permissions)
	// surface from the first statement inside migrate, so the recovery
	// hint naming the DB path and the newest backup belongs here.
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w (latest backup, if any: %s)", path, err, latestBackup(path))
	}
	// Not fatal: without incremental auto-vacuum, retention still deletes
	// rows and the file merely stops shrinking. Refusing to start over
	// that — a full VACUUM wants room for a second copy of the database —
	// would turn a disk-space problem into an outage.
	if err := s.ensureIncrementalVacuum(); err != nil {
		logger().Warn("could not enable incremental vacuum; retention will free rows but not disk", "err", err)
	}
	// Fatal, unlike the vacuum above: a half-built index answers a search
	// with traces that are not there and hides the ones that are, and there
	// is no way for a reader to tell (spec 011 #8). It runs once, before the
	// server listens, and finds nothing to do on every start after that.
	if err := s.backfillSearchIndex(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if s.mediaUploadKey, err = s.serverKey("media_upload"); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// GraceWindow is how long a soft-deleted project's data survives before the
// sweeper purges it (spec 005 #9). Fixed, not configurable: the point of a
// safety net is knowing it is there, and a knob would make the guarantee
// depend on how the deployment was configured.
const GraceWindow = 7 * 24 * time.Hour

// Project is a tenant-lite unit of isolation: its own keys and retention.
type Project struct {
	ID   string
	Name string
	// RetentionDays is how long a trace is kept counting from its arrival.
	// Nil means forever, and forever is the default (spec 005 #1, #2): a
	// self-hosted tool must not discard the data of someone who installed
	// it and configured nothing.
	RetentionDays *int
	// RawRetentionDays is the window for stored OTLP bodies. Nil means it
	// follows RetentionDays, so raw lives exactly as long as the parsed
	// data it can rebuild (#6).
	RawRetentionDays *int
	// StatsRetentionDays is the window for the hourly rollup. Nil means
	// forever, which is the default and what every project takes on the
	// upgrade: the aggregates are the cheap thing, and "no trace older
	// than 30 days" and "no *record* older than 30 days" are different
	// promises (spec 013 #6).
	StatsRetentionDays *int
	// DeletedAt is when the project was soft-deleted (Unix nanoseconds),
	// nil while it is live (#9).
	DeletedAt *int64
	CreatedAt string
	// Media is what ingest does with an image or a file it takes out of a
	// payload (spec 041 #6): MediaStore keeps the body, MediaPlaceholder
	// keeps only a reference that says it was not stored.
	Media string
}

// The two values of a project's media setting (spec 041 #6).
const (
	MediaStore       = "store"
	MediaPlaceholder = "placeholder"
)

// Deleted reports a project inside its grace window.
func (p *Project) Deleted() bool { return p != nil && p.DeletedAt != nil }

// PurgeAt is when the sweeper will destroy a deleted project's data.
func (p *Project) PurgeAt() int64 {
	if p.DeletedAt == nil {
		return 0
	}
	return *p.DeletedAt + int64(GraceWindow)
}

// RawWindowDays resolves the window raw batches are actually swept by: their
// own, or the project's when they have none (#6).
func (p *Project) RawWindowDays() *int {
	if p.RawRetentionDays != nil {
		return p.RawRetentionDays
	}
	return p.RetentionDays
}

// KeyPair is a project's API credentials. Secret is only present right after
// generation — the store keeps only its hash (spec 001 #4).
type KeyPair struct {
	PublicKey string
	Secret    string
}

// projectColumns is the one SELECT list every project read shares, so a column
// added to the table is added to every reader at once.
const projectColumns = `id, name, retention_days, raw_retention_days, stats_retention_days, deleted_at, created_at, media`

func scanProject(row interface{ Scan(...any) error }) (*Project, error) {
	var (
		p         Project
		retention sql.NullInt64
		raw       sql.NullInt64
		stats     sql.NullInt64
		deleted   sql.NullInt64
	)
	if err := row.Scan(&p.ID, &p.Name, &retention, &raw, &stats, &deleted, &p.CreatedAt, &p.Media); err != nil {
		return nil, err
	}
	if stats.Valid {
		days := int(stats.Int64)
		p.StatsRetentionDays = &days
	}
	if retention.Valid {
		days := int(retention.Int64)
		p.RetentionDays = &days
	}
	if raw.Valid {
		days := int(raw.Int64)
		p.RawRetentionDays = &days
	}
	if deleted.Valid {
		at := deleted.Int64
		p.DeletedAt = &at
	}
	return &p, nil
}

// CreateProject inserts a project with the given key pair, outside the
// group-commit writer: it runs at startup, from the bootstrap, before the
// writer exists, so the key is the server's own (spec 045 #8). The API creates
// projects as a job like every other write (see ProjectCreate).
func (s *Store) CreateProject(name string, keys KeyPair) (*Project, error) {
	id, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	project, err := insertProject(tx, id, name, keys, KeyOrigin{Via: MintedAtStartup})
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return project, nil
}

// ProjectByName returns the project or nil if absent. A soft-deleted project
// is returned like any other: its name stays reserved through the grace
// window, so that restore always has its name to come back to (spec 005 #9).
func (s *Store) ProjectByName(name string) (*Project, error) {
	return s.oneProject(`SELECT `+projectColumns+` FROM projects WHERE name = ?`, name)
}

// ProjectByID returns the project or nil if absent, deleted ones included.
func (s *Store) ProjectByID(id string) (*Project, error) {
	return s.oneProject(`SELECT `+projectColumns+` FROM projects WHERE id = ?`, id)
}

// ListProjects returns every project, oldest first. Deleted ones are left out
// unless asked for, which is what makes a deleted project vanish from listings
// while an admin can still see it and its purge date (spec 005 #9).
func (s *Store) ListProjects(includeDeleted bool) ([]*Project, error) {
	query := `SELECT ` + projectColumns + ` FROM projects`
	if !includeDeleted {
		query += ` WHERE deleted_at IS NULL`
	}
	query += ` ORDER BY created_at, id`

	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Project
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, project)
	}
	return out, rows.Err()
}

// CountProjects returns the number of projects, deleted ones included: it
// answers "is this database empty" for the bootstrap, and a name inside its
// grace window is still taken.
func (s *Store) CountProjects() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&n)
	return n, err
}

func (s *Store) oneProject(query string, args ...any) (*Project, error) {
	project, err := scanProject(s.db.QueryRow(query, args...))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return project, nil
}

// GenerateKeyPair mints a fresh random key pair (spec 001 #8).
func GenerateKeyPair() (KeyPair, error) {
	pk, err := randomHex(8)
	if err != nil {
		return KeyPair{}, err
	}
	sk, err := randomHex(32)
	if err != nil {
		return KeyPair{}, err
	}
	return KeyPair{PublicKey: "tp-pk-" + pk, Secret: "tp-sk-" + sk}, nil
}

// NewID mints the identifier the API generates for a client that sent none:
// 32 lower-case hex characters, the same shape and the same generator as a
// project id (spec 003 #3).
func NewID() (string, error) { return randomHex(16) }

func randomHex(nbytes int) (string, error) {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// latestBackup names the newest .bak next to the DB for error messages.
func latestBackup(dbPath string) string {
	matches, _ := filepath.Glob(dbPath + ".pre-*.bak")
	if len(matches) == 0 {
		return "none"
	}
	newest := matches[0]
	for _, m := range matches[1:] {
		ni, _ := os.Stat(newest)
		mi, _ := os.Stat(m)
		if ni != nil && mi != nil && mi.ModTime().After(ni.ModTime()) {
			newest = m
		}
	}
	return newest
}

var logger = slog.Default
