// Package storetest opens the store the way a test of one of its clients — the
// CLI, the MCP server — wants it: empty, migrated, and quick.
//
// Two costs are paid once here that a harness written from scratch pays on
// every test. Migrating an empty database from nothing is about fifteen
// milliseconds, which is a third of what a typical test in those packages
// takes altogether; so one is migrated per binary and every test starts from
// a copy of it, which store.Open finds up to date. And a lone write waits out
// the writer's whole commit window before it is flushed, which at the default
// fifty milliseconds — times every one-row seed in a suite — was most of the
// suites' runtime; Writes is the window that is not.
//
// Whether the migrations apply to an empty file is the store's own test. What
// a client's test needs is a store with the schema and no rows, which a copy
// is.
package storetest

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// Writes is the writer a client's tests want: nothing they assert is about
// group commit, so the window is shortened rather than waited out.
var Writes = store.WriterOptions{CommitWindow: time.Millisecond}

// template is the migrated, empty database, read into memory once so that no
// directory has to outlive the tests.
var template = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "tracepad-storetest-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "template.db")
	st, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	// Close checkpoints the WAL into the file, so the file alone is the
	// database. Checked, because a copy that missed the WAL would still
	// open, and every test would silently be migrating from nothing again.
	if err := st.Close(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path + "-wal"); err == nil {
		return nil, errors.New("the template still has a WAL after Close; a copy would not carry the schema")
	}
	return os.ReadFile(path)
})

// Path is a migrated, empty database at a path of the test's own, for a test
// that opens it itself — to reopen it, or to open it wrongly on purpose.
func Path(t testing.TB) string {
	t.Helper()
	migrated, err := template()
	if err != nil {
		t.Fatalf("storetest: build the template database: %v", err)
	}
	path := filepath.Join(t.TempDir(), "tracepad.db")
	if err := os.WriteFile(path, migrated, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Open is a migrated, empty store, closed when the test ends.
func Open(t testing.TB) *store.Store {
	t.Helper()
	st, err := store.Open(Path(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
