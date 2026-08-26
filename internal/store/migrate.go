package store

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrate applies pending embedded migrations in lexicographic order,
// forward-only, tracked by filename (spec 001 #6). Before applying anything
// to an existing database it copies the file to <db>.pre-<first-pending>.bak.
func (s *Store) migrate() error {
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		filename   TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
	) STRICT`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[string]bool{}
	rows, err := s.db.Query(`SELECT filename FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			rows.Close()
			return err
		}
		applied[f] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	all, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(all)
	known := map[string]bool{}
	for _, path := range all {
		known[strings.TrimPrefix(path, "migrations/")] = true
	}

	// A recorded migration this binary does not ship means the DB was
	// written by a newer version: refuse rather than guess (spec 001,
	// edge cases).
	for f := range applied {
		if !known[f] {
			return fmt.Errorf("database at %s was migrated by a newer tracepad (unknown migration %s); upgrade the binary", s.path, f)
		}
	}

	var pending []string
	for _, path := range all {
		if !applied[strings.TrimPrefix(path, "migrations/")] {
			pending = append(pending, path)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	if err := s.backupBefore(pending[0]); err != nil {
		return err
	}

	for _, path := range pending {
		body, err := migrationFS.ReadFile(path)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(path, "migrations/")
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (filename) VALUES (?)`, name); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
		logger().Info("applied migration", "migration", name)
	}
	return nil
}

// backupBefore copies the DB file aside before the first pending migration.
// A fresh database (no file before Open) is not backed up.
func (s *Store) backupBefore(firstPending string) error {
	if s.fresh {
		return nil // fresh database, nothing worth backing up
	}
	if _, err := os.Stat(s.path); err != nil {
		return nil
	}
	tag := strings.TrimSuffix(strings.TrimPrefix(firstPending, "migrations/"), ".sql")
	dst := s.path + ".pre-" + tag + ".bak"
	data, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("backup before migration: %w", err)
	}
	// Overwrite an existing backup: the state before *this* run is what
	// matters after a crashed earlier attempt (spec 001, edge cases).
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return fmt.Errorf("backup before migration: %w", err)
	}
	logger().Info("database backed up before migration", "backup", dst)
	return nil
}
