package server

import (
	"log"
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/tracepad/tracepad/internal/store"
)

// TestMain lowers the bcrypt work factor for the whole binary. Cost 12 is a
// quarter of a second by design (spec 028 Decision 1), and these tests sign in
// and accept invitations dozens of times to prove things that have nothing to
// do with how long a hash takes. The store's own suite hashes at the
// production cost and asserts it; the one test here that is about the quarter
// of a second, TestLoginSpendsTheComparisonWhateverTheAnswer, sets it back for
// its own duration.
//
// The other cost every harness used to pay — migrating an empty database from
// nothing — is paid once by internal/storetest, which newHarness opens.
func TestMain(m *testing.M) {
	store.SetPasswordCost(bcrypt.MinCost)
	// The shared hash is made here, while the cost is what it was just set
	// to, rather than by whichever test asks first: made inside the one
	// test's cost-12 window it would be a quarter-second hash for every
	// login in the binary, and nothing would say so.
	if _, err := accountHash(); err != nil {
		log.Fatal(err)
	}
	os.Exit(m.Run())
}
