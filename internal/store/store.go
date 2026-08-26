// Package store owns the embedded SQLite database: opening, migrating,
// and the project/API-key model. It is the only package that talks SQL;
// everything above it uses its narrow interface (design: swappable driver).
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

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
}

// Open opens (creating if needed) the database at path and applies pending
// migrations. The parent directory is created with 0700.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	// modernc.org/sqlite accepts pragmas in the DSN, applied in order per
	// new pool connection — busy_timeout must come first so every later
	// pragma (journal_mode included) already waits out lock contention
	// instead of failing with SQLITE_BUSY. WAL for concurrent readers with
	// the single writer; incremental auto_vacuum so retention deletes
	// (later stage) can actually return disk space.
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=auto_vacuum(INCREMENTAL)&_pragma=synchronous(NORMAL)"
	info, statErr := os.Stat(path)
	fresh := statErr != nil || info.Size() == 0
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
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Project is a tenant-lite unit of isolation: its own keys and retention.
type Project struct {
	ID            string
	Name          string
	RetentionDays int
}

// KeyPair is a project's API credentials. Secret is only present right after
// generation — the store keeps only its hash (spec 001 #4).
type KeyPair struct {
	PublicKey string
	Secret    string
}

// CreateProject inserts a project with the given key pair.
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
	// RETURNING keeps the returned Project in sync with schema defaults
	// instead of duplicating them as Go literals.
	var retention int
	if err := tx.QueryRow(`INSERT INTO projects (id, name) VALUES (?, ?) RETURNING retention_days`, id, name).
		Scan(&retention); err != nil {
		return nil, fmt.Errorf("create project %q: %w", name, err)
	}
	hash := sha256.Sum256([]byte(keys.Secret))
	if _, err := tx.Exec(`INSERT INTO api_keys (public_key, secret_hash, project_id) VALUES (?, ?, ?)`,
		keys.PublicKey, hash[:], id); err != nil {
		return nil, fmt.Errorf("create key for project %q: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Project{ID: id, Name: name, RetentionDays: retention}, nil
}

// ProjectByName returns the project or nil if absent.
func (s *Store) ProjectByName(name string) (*Project, error) {
	var p Project
	err := s.db.QueryRow(`SELECT id, name, retention_days FROM projects WHERE name = ?`, name).
		Scan(&p.ID, &p.Name, &p.RetentionDays)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// CountProjects returns the number of projects.
func (s *Store) CountProjects() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM projects`).Scan(&n)
	return n, err
}

// ProjectBySecret resolves an API secret to its project, or nil if unknown.
// Lookup is by sha256(secret) against a unique index (spec 001 #8).
func (s *Store) ProjectBySecret(secret string) (*Project, error) {
	hash := sha256.Sum256([]byte(secret))
	var p Project
	err := s.db.QueryRow(
		`SELECT p.id, p.name, p.retention_days FROM projects p
		 JOIN api_keys k ON k.project_id = p.id WHERE k.secret_hash = ?`, hash[:]).
		Scan(&p.ID, &p.Name, &p.RetentionDays)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
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
