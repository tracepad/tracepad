package store

import (
	"context"
	"database/sql"
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
//
// Everything runs on one pinned connection with foreign keys disabled. A
// migration that rebuilds a table has to drop the old one, and DROP TABLE with
// foreign keys on performs an implicit DELETE FROM: dropping `projects` would
// fire every ON DELETE CASCADE hanging off it and empty the database. This is
// SQLite's own documented procedure for such a change, and its other half is
// the `PRAGMA foreign_key_check` below, which refuses to commit a migration
// that left a dangling reference behind.
func (s *Store) migrate() error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate: reserve connection: %w", err)
	}
	// The connection goes back to the pool afterwards, so the pragma is
	// restored before it does (defers run last in, first out).
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return fmt.Errorf("migrate: disable foreign keys: %w", err)
	}
	defer conn.ExecContext(ctx, `PRAGMA foreign_keys=ON`)

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		filename   TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
	) STRICT`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[string]bool{}
	rows, err := conn.QueryContext(ctx, `SELECT filename FROM schema_migrations`)
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
		if err := applyMigration(ctx, conn, name, string(body)); err != nil {
			return err
		}
		logger().Info("applied migration", "migration", name)
		if after := afterMigration[name]; after != nil {
			if err := after(s); err != nil {
				return fmt.Errorf("finish migration %s: %w", name, err)
			}
		}
	}
	return nil
}

// applyMigration runs one file and records it, in a single transaction.
func applyMigration(ctx context.Context, conn *sql.Conn, name, body string) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("apply migration %s: %w", name, err)
	}
	if err := checkForeignKeys(ctx, tx); err != nil {
		return fmt.Errorf("apply migration %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (filename) VALUES (?)`, name); err != nil {
		return fmt.Errorf("record migration %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", name, err)
	}
	return nil
}

// checkForeignKeys refuses to commit a migration that left a row pointing at
// something that is not there. Foreign keys are off while migrations run, so
// this is the only thing standing between a mistake in a table rebuild and a
// database that looks fine until a read joins.
func checkForeignKeys(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	defer rows.Close()

	var violations []string
	for rows.Next() {
		var (
			table, parent sql.NullString
			rowID         sql.NullInt64
			constraint    sql.NullInt64
		)
		if err := rows.Scan(&table, &rowID, &parent, &constraint); err != nil {
			return fmt.Errorf("foreign key check: %w", err)
		}
		if len(violations) < 5 {
			violations = append(violations,
				fmt.Sprintf("%s row %d references a missing %s", table.String, rowID.Int64, parent.String))
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	if len(violations) > 0 {
		return fmt.Errorf("left dangling references: %s", strings.Join(violations, "; "))
	}
	return nil
}

// afterMigration holds the work a migration needs that its own SQL cannot do,
// keyed by filename. VACUUM is why this exists: it cannot run inside a
// transaction, and every migration file runs in one.
var afterMigration = map[string]func(*Store) error{
	"0005_retention_admin.sql": (*Store).ensureIncrementalVacuum,
}

// ensureIncrementalVacuum puts the database into incremental auto-vacuum mode
// so that retention deletes can hand pages back to the filesystem (spec 005
// #5). Deleting rows without it returns nothing to the OS, and "the file
// actually shrinks" is the operator-visible half of retention.
//
// The mode is a property of the file, and the only way to change it on a
// populated database is a full VACUUM. Databases created by this binary have
// been incremental since spec 001 (the DSN sets the pragma before the first
// table exists), so the check is what keeps the expensive branch unreached in
// practice — and correct for a file that arrived from somewhere else.
func (s *Store) ensureIncrementalVacuum() error {
	const incremental = 2
	var mode int
	if err := s.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return fmt.Errorf("read auto_vacuum mode: %w", err)
	}
	if mode == incremental {
		return nil
	}
	logger().Info("rewriting the database to enable incremental vacuum", "from_mode", mode)
	// The pragma records the wanted mode; the VACUUM is what applies it.
	if _, err := s.db.Exec(`PRAGMA auto_vacuum=INCREMENTAL`); err != nil {
		return fmt.Errorf("set auto_vacuum mode: %w", err)
	}
	if _, err := s.db.Exec(`VACUUM`); err != nil {
		return fmt.Errorf("vacuum to change auto_vacuum mode: %w", err)
	}
	return nil
}

// backupBefore snapshots the database before the first pending migration.
// A fresh database (no file before Open) is not backed up.
func (s *Store) backupBefore(firstPending string) error {
	if s.fresh {
		return nil // fresh database, nothing worth backing up
	}
	tag := strings.TrimSuffix(strings.TrimPrefix(firstPending, "migrations/"), ".sql")
	dst := s.path + ".pre-" + tag + ".bak"
	// VACUUM INTO refuses to overwrite; the state before *this* run is what
	// matters after a crashed earlier attempt (spec 001, edge cases), so a
	// stale backup is dropped first.
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("backup before migration: %w", err)
	}
	// VACUUM INTO instead of a file copy (spec 001 #11): the snapshot is a
	// complete, checkpointed database — committed rows still sitting in the
	// WAL are included (a plain copy of the main file silently loses them
	// after an unclean shutdown), and it streams without loading the
	// database into memory.
	if _, err := s.db.Exec(`VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("backup before migration: %w", err)
	}
	logger().Info("database backed up before migration", "backup", dst)
	return nil
}
