package store

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// What every fixture in this package pays once rather than per test. The
// store's clients get the same two things from internal/storetest, which this
// package cannot import without a cycle, so they are stated here again.

// quickWrites is the writer a fixture wants when nothing it asserts is about
// group commit: a lone submission waits out the whole window before it is
// flushed, and at the default fifty milliseconds — times every one-row seed
// in the suite — the window was most of the runtime. The writer's own tests
// name their windows and are not on this.
var quickWrites = WriterOptions{CommitWindow: time.Millisecond}

// migratedTemplate is an empty database with every migration applied, read
// into memory once so that no directory has to outlive the tests. Migrating
// from nothing is about fifteen milliseconds, which was a third of what a
// typical test here took. The migration tests open their own files, from
// nothing or from an older schema, because for them the migrating is the
// point.
var migratedTemplate = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "tracepad-store-test-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "template.db")
	s, err := Open(path)
	if err != nil {
		return nil, err
	}
	// Close checkpoints the WAL into the file, so the file alone is the
	// database. Checked, because a copy that missed the WAL would still
	// open, and every test would silently be migrating from nothing again.
	if err := s.Close(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path + "-wal"); err == nil {
		return nil, errors.New("the template still has a WAL after Close; a copy would not carry the schema")
	}
	return os.ReadFile(path)
})

// freshDB is a migrated, empty database at a path of the test's own.
func freshDB(t testing.TB) string {
	t.Helper()
	migrated, err := migratedTemplate()
	if err != nil {
		t.Fatalf("build the template database: %v", err)
	}
	path := filepath.Join(t.TempDir(), "tracepad.db")
	if err := os.WriteFile(path, migrated, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// openFresh is a migrated, empty store, closed when the test ends.
func openFresh(t testing.TB) *Store {
	t.Helper()
	s, err := Open(freshDB(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
