package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// anySlot is a place at a gate of the tests' own, for a test that hashes or
// compares outside any request.
func anySlot() *PasswordSlot {
	slot, err := NewPasswordGate(1, 0).Enter(context.Background())
	if err != nil {
		panic(err)
	}
	return slot
}

// TestPasswordGateQueues: behind a full gate there is a short queue, a caller
// in it goes through when a place frees, one past it is turned away at once,
// and one that gives up leaves the queue with its own error, not the gate's
// (spec 028 #31).
func TestPasswordGateQueues(t *testing.T) {
	g := NewPasswordGate(1, 1)
	first, err := g.Enter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan *PasswordSlot, 1)
	go func() {
		slot, err := g.Enter(context.Background())
		if err == nil {
			entered <- slot
		}
		close(entered)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for g.Waiting() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err := g.Enter(context.Background()); !errors.Is(err, ErrPasswordsBusy) {
		t.Fatalf("err = %v, want a full gate with a full queue to turn the caller away", err)
	}
	first.Release()
	second, ok := <-entered
	if !ok {
		t.Fatal("the queued caller must get the place that freed")
	}
	second.Release()
	second.Release() // twice is harmless

	blocker, _ := g.Enter(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.Enter(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the caller's own cancellation", err)
	}
	if g.Waiting() != 0 {
		t.Error("a caller that gave up must leave the queue")
	}
	blocker.Release()
	if again, err := g.Enter(context.Background()); err != nil {
		t.Errorf("a released gate must let the next one in: %v", err)
	} else {
		again.Release()
	}
}

// TestBcryptNeedsAPlace: the two functions that run bcrypt take a slot only
// the gate makes, so a caller that forgot the gate does not compile; one that
// passes nil meets a panic in its first test rather than spending the CPU the
// gate protects.
func TestBcryptNeedsAPlace(t *testing.T) {
	for name, call := range map[string]func(){
		"HashPassword": func() { _, _ = HashPassword(nil, testAccountPassword) },
		"Verify":       func() { (&Account{}).Verify(nil, testAccountPassword) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s without a slot did not panic", name)
				}
			}()
			call()
		}()
	}
}

// TestPasswordChangeSpendsNoBcryptInTheWriter: the current password is compared
// and the new one hashed before the job is submitted; the job, which holds the
// one writer, only checks that the hash that was compared against is still
// the stored one (spec 028 #31).
func TestPasswordChangeSpendsNoBcryptInTheWriter(t *testing.T) {
	f := newAccountFixture(t)
	account := f.invite(t, "helper@example.com", false)
	newHash := hashOnce(t)

	// A change checked against a hash that is no longer stored is refused:
	// the password changed between the read and the write.
	stale := *account
	stale.hash = []byte("$2a$04$a hash that was replaced meanwhile")
	err := f.writer.Submit(t.Context(), &PasswordChange{
		AccountID: account.ID, Checked: &stale, NewHash: newHash, Keep: "none",
	})
	if !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("err = %v, want a stale check refused as a wrong password", err)
	}
	if err := f.writer.Submit(t.Context(), &PasswordChange{
		AccountID: account.ID, NewHash: newHash, Keep: "none",
	}); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("err = %v, want a change with nothing checked refused", err)
	}

	work := PasswordWork()
	change := &PasswordChange{AccountID: account.ID, Checked: account, NewHash: newHash, Keep: "none"}
	f.submit(t, change)
	if spent := PasswordWork() - work; spent != 0 {
		t.Errorf("the job spent %d bcrypt operations inside the writer, want none", spent)
	}
	if !change.Account.Verify(anySlot(), testAccountPassword) {
		t.Error("the new hash was not stored")
	}
}
