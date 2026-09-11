package server

import (
	"log"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/tracepad/tracepad/internal/store"
)

// TestMain pays two costs once for the binary that the tests used to pay each.
//
// The bcrypt work factor: cost 12 is a quarter of a second by design (spec 028
// Decision 1), and these tests sign in and accept invitations dozens of times
// to prove things that have nothing to do with how long a hash takes. The
// store's own suite hashes at the production cost and asserts it; the one
// test here that is about the quarter of a second,
// TestLoginSpendsTheComparisonWhateverTheAnswer, sets it back for its own
// duration.
//
// The schema: every harness opens an empty database, and migrating one from
// nothing is fifteen milliseconds — a third of what a typical test here takes
// altogether. So one is migrated here and each harness starts from a copy of
// it, which store.Open finds up to date. Whether the migrations apply to an
// empty file is the store's test, not this package's; what a test here needs
// is a store with the schema and no rows, which a copy is.
func TestMain(m *testing.M) {
	store.SetPasswordCost(bcrypt.MinCost)
	// The shared hash is made here, while the cost is what it was just set
	// to, rather than by whichever test asks first: made inside the one
	// test's cost-12 window it would be a quarter-second hash for every
	// login in the binary, and nothing would say so.
	if _, err := accountHash(); err != nil {
		log.Fatal(err)
	}

	dir, err := os.MkdirTemp("", "tracepad-server-test-")
	if err != nil {
		log.Fatal(err)
	}
	templateDB = filepath.Join(dir, "template.db")
	st, err := store.Open(templateDB)
	if err != nil {
		log.Fatal(err)
	}
	// Close checkpoints the WAL into the file, so the copy is the file alone
	// — checked, because a copy that missed the WAL would still open, and
	// every harness would silently be migrating from nothing again.
	if err := st.Close(); err != nil {
		log.Fatal(err)
	}
	if _, err := os.Stat(templateDB + "-wal"); err == nil {
		log.Fatal("the template database still has a WAL after Close; the copies would not carry the schema")
	}

	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// templateDB is the migrated, empty database every harness copies.
var templateDB string

// freshDB is a copy of the template at a path of the test's own.
func freshDB(t *testing.T) string {
	t.Helper()
	migrated, err := os.ReadFile(templateDB)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tracepad.db")
	if err := os.WriteFile(path, migrated, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
