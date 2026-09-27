package storetest_test

import (
	"testing"

	"github.com/tracepad/tracepad/internal/store"
	"github.com/tracepad/tracepad/internal/storetest"
)

// A copy carries the schema and nothing else, and two copies share nothing:
// what a client's test relies on without saying so.
func TestEveryStoreIsMigratedAndItsOwn(t *testing.T) {
	first := storetest.Open(t)
	if _, err := first.CreateProject("first", store.KeyPair{PublicKey: "tp-pk-a", Secret: "tp-sk-a"}); err != nil {
		t.Fatalf("the copy did not carry the schema: %v", err)
	}

	second := storetest.Open(t)
	projects, err := second.ListProjects(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 0 {
		t.Fatalf("a fresh store holds %d projects; the copies are not independent", len(projects))
	}
}
