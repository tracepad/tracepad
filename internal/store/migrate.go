package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"
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

	backup, err := s.backupBefore(pending[0])
	if err != nil {
		return err
	}

	for _, path := range pending {
		body, err := migrationFS.ReadFile(path)
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(path, "migrations/")
		// A line before as well as after: a migration that rewrites
		// rows runs before the server listens, and on a large store a
		// start with nothing in the log looks like a hang (spec 043 #24).
		logger().Info("applying migration", "migration", name)
		start := time.Now()
		written, err := applyMigration(ctx, conn, name, string(body))
		if err != nil {
			return err
		}
		logger().Info("applied migration", "migration", name,
			"rows_written", written, "took", time.Since(start).Round(time.Millisecond))
	}
	// Every migration this run's backup guards has committed, so the older
	// backups stop being the way back from anything: each is a full copy of
	// the database as it was, everything erased or swept since included
	// (spec 044 #12). Not before this point — a migration that fails keeps
	// every file for the one case they exist for.
	if backup != "" {
		removeBackups(s.path, backup)
	}
	return nil
}

// applyMigration runs one file and records it, in a single transaction, and
// reports how many rows it wrote — SQLite's own count of rows the connection
// inserted, updated or deleted, taken before and after. That is every
// statement's rows, a scratch table's included, not the rows the migration
// repaired (spec 043 #24 u).
func applyMigration(ctx context.Context, conn *sql.Conn, name, body string) (int64, error) {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var before, after int64
	if err := tx.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&before); err != nil {
		return 0, fmt.Errorf("apply migration %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx, body); err != nil {
		return 0, fmt.Errorf("apply migration %s: %w", name, err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&after); err != nil {
		return 0, fmt.Errorf("apply migration %s: %w", name, err)
	}
	if err := checkForeignKeys(ctx, tx); err != nil {
		return 0, fmt.Errorf("apply migration %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (filename) VALUES (?)`, name); err != nil {
		return 0, fmt.Errorf("record migration %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit migration %s: %w", name, err)
	}
	return after - before, nil
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

// incrementalVacuumMode is SQLite's numbering for auto_vacuum=INCREMENTAL.
const incrementalVacuumMode = 2

// ensureIncrementalVacuum puts the database into incremental auto-vacuum mode
// so that retention deletes can hand pages back to the filesystem (spec 005
// #5). Deleting rows without it returns nothing to the OS, and "the file
// actually shrinks" is the operator-visible half of retention.
//
// The mode is a property of the file, and the only way to change it on a
// populated one is a full VACUUM — which must run on the same connection that
// asked for the new mode, since the pragma is a per-connection intention until
// the VACUUM applies it. Two `db.Exec` calls take two pooled connections and
// leave the mode exactly as it was, quietly.
//
// It runs on every Open rather than once, as the tail of a migration: keyed to
// a migration it would run after that migration was already recorded as
// applied, so a VACUUM that failed for want of disk space would be skipped
// forever afterwards. Asked every time, it is one PRAGMA read on a database
// that is already right, and it heals one that is not.
func (s *Store) ensureIncrementalVacuum() error {
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve connection for vacuum: %w", err)
	}
	defer conn.Close()

	var mode int
	if err := conn.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return fmt.Errorf("read auto_vacuum mode: %w", err)
	}
	if mode == incrementalVacuumMode {
		return nil
	}

	logger().Info("rewriting the database to enable incremental vacuum", "from_mode", mode)
	if _, err := conn.ExecContext(ctx, `PRAGMA auto_vacuum=INCREMENTAL`); err != nil {
		return fmt.Errorf("set auto_vacuum mode: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum to change auto_vacuum mode: %w", err)
	}
	// Believing the VACUUM rather than checking it is how this failed
	// silently the first time.
	if err := conn.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return fmt.Errorf("read auto_vacuum mode: %w", err)
	}
	if mode != incrementalVacuumMode {
		return fmt.Errorf("auto_vacuum is still mode %d after a full vacuum", mode)
	}
	return nil
}

// vacuumInto writes the snapshot; a seam for the test that makes it fail.
var vacuumInto = func(db *sql.DB, dst string) error {
	_, err := db.Exec(`VACUUM INTO ?`, dst)
	return err
}

// backupBefore snapshots the database before the first pending migration and
// returns the backup's path. A fresh database (no file before Open) is not
// backed up, and the path is empty.
func (s *Store) backupBefore(firstPending string) (string, error) {
	if s.fresh {
		return "", nil // fresh database, nothing worth backing up
	}
	tag := strings.TrimSuffix(strings.TrimPrefix(firstPending, "migrations/"), ".sql")
	dst := s.path + ".pre-" + tag + ".bak"
	// VACUUM INTO refuses to overwrite a database; the state before *this*
	// run is what matters after a crashed earlier attempt (spec 001, edge
	// cases), so a stale backup is dropped first.
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("backup before migration: %w", err)
	}
	// Created empty at 0600 first: VACUUM INTO writes into an empty file it
	// finds and keeps its mode, and the copy holds everything the database
	// does (spec 044 #12).
	if err := createOwnerOnly(dst); err != nil {
		return "", fmt.Errorf("backup before migration: %w", err)
	}
	// VACUUM INTO instead of a file copy (spec 001 #11): the snapshot is a
	// complete, checkpointed database — committed rows still sitting in the
	// WAL are included (a plain copy of the main file silently loses them
	// after an unclean shutdown), and it streams without loading the
	// database into memory.
	if err := vacuumInto(s.db, dst); err != nil {
		// The file created above is empty or half-written, and left
		// behind it would be the newest backup — the one the recovery
		// hint names, and the one an operator swaps in.
		if removeErr := os.Remove(dst); removeErr != nil && !os.IsNotExist(removeErr) {
			err = errors.Join(err, removeErr)
		}
		return "", fmt.Errorf("backup before migration: %w", err)
	}
	logger().Info("database backed up before migration", "backup", dst)
	return dst, nil
}
