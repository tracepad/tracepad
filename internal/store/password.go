package store

import (
	"crypto/rand"
	"fmt"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/bcrypt"
)

// Passwords (spec 028 Decision 1). `bcrypt` is the boring choice, and the cost
// is stated here rather than taken from the library's default so that a change
// in either is a change in the diff.
//
// The rule is a length and nothing else: composition rules ("one digit, one
// symbol") measurably make passwords worse, and the ceiling is where `bcrypt`
// stops reading the input anyway.
const (
	// PasswordCost is the bcrypt work factor. Twelve is about a quarter of
	// a second on the hardware this runs on, which is the point: a login is
	// one request a person makes twice a month, and a dictionary run is
	// millions.
	PasswordCost = 12

	// MinPasswordLength and MaxPasswordLength bound what may be set.
	MinPasswordLength = 10
	MaxPasswordLength = 128
)

// ErrPasswordLength is a password outside the bounds above. The message is
// what the person setting it reads, so it says both ends.
var ErrPasswordLength = fmt.Errorf(
	"a password must be between %d and %d characters", MinPasswordLength, MaxPasswordLength)

// passwordCost is the work factor in force: PasswordCost unless a test binary
// has lowered it (see SetPasswordCost). Read on every hash and every decoy so
// that the two never disagree.
var passwordCost = func() *atomic.Int32 {
	var cost atomic.Int32
	cost.Store(PasswordCost)
	return &cost
}()

// SetPasswordCost changes the work factor for the process and reports what it
// was. It exists for test binaries alone, where a quarter of a second per
// account and per login — proving nothing about the code under test — was
// most of a suite's runtime; nothing in the server calls it, and PasswordCost
// is what is in force whenever it has not been. A test that is about the
// quarter of a second sets PasswordCost back for its own duration.
func SetPasswordCost(cost int) (previous int) {
	return int(passwordCost.Swap(int32(cost)))
}

// HashPassword checks the length and hashes. The two are one call because a
// caller that hashed first and validated afterwards would have spent a quarter
// of a second on a password it was going to refuse.
func HashPassword(password string) ([]byte, error) {
	if len(password) < MinPasswordLength || len(password) > MaxPasswordLength {
		return nil, ErrPasswordLength
	}
	return bcrypt.GenerateFromPassword([]byte(password), int(passwordCost.Load()))
}

// decoys holds, per work factor, a hash of a password nobody has, compared
// against when there is no stored hash to compare against.
//
// One 401 with one sentence (Decision 8) is only one answer if it also takes
// one length of time. `bcrypt` at cost 12 is about a quarter of a second, so a
// login that skipped it for an unknown email would answer "no account here" in
// its timing to anyone with a stopwatch — and the throttle does not help,
// because it counts attempts per email and one attempt is all this needs.
//
// Computed on the first login that needs it rather than written out as a
// constant: a literal hash in the source is a thing somebody eventually
// wonders whether they can log in with. Keyed by cost because the decoy has to
// cost what a real comparison does, and a test binary changes what that is;
// in production there is one entry, made once.
var decoys struct {
	sync.Mutex
	byCost map[int][]byte
}

func decoy() []byte {
	cost := int(passwordCost.Load())
	decoys.Lock()
	defer decoys.Unlock()
	if hash, ok := decoys.byCost[cost]; ok {
		return hash
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		// Only reachable if the system's randomness is broken, in which
		// case nothing else here works either. The comparison still costs
		// what it should, which is all this value is for.
		raw = []byte("tracepad has no randomness to spare")
	}
	hash, err := bcrypt.GenerateFromPassword(raw, cost)
	if err != nil {
		return nil
	}
	if decoys.byCost == nil {
		decoys.byCost = make(map[int][]byte)
	}
	decoys.byCost[cost] = hash
	return hash
}

// Verify reports whether the password is this account's.
//
// An account with no password — invited and not yet accepted — verifies
// nothing, which is what makes `pending` answer the login with the same 401 as
// a wrong password (Decision 8). It spends the comparison anyway, against the
// decoy, so that it answers in the same time as well.
func (a *Account) Verify(password string) bool {
	if a == nil || len(a.hash) == 0 {
		bcrypt.CompareHashAndPassword(decoy(), []byte(password))
		return false
	}
	// Any error is a refusal, mismatch or unreadable hash alike: there is
	// nothing a caller could do with the difference, and saying which it
	// was would say whether the account has a password at all.
	return bcrypt.CompareHashAndPassword(a.hash, []byte(password)) == nil
}
