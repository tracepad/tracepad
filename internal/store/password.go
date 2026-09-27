package store

import (
	"context"
	"crypto/rand"
	"errors"
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
// symbol") measurably make passwords worse. The ceiling is where `bcrypt`
// stops reading the input, measured the way it measures: in bytes. The
// library refuses anything longer outright, so a ceiling above it was a
// password the server accepted as valid and then failed to hash (spec 028
// #31).
const (
	// PasswordCost is the bcrypt work factor. Twelve is about a quarter of
	// a second on the hardware this runs on, which is the point: a login is
	// one request a person makes twice a month, and a dictionary run is
	// millions.
	PasswordCost = 12

	// MinPasswordLength and MaxPasswordLength bound what may be set, in
	// bytes of UTF-8.
	MinPasswordLength = 10
	MaxPasswordLength = 72
)

// ErrPasswordLength is a password outside the bounds above. The message is
// what the person setting it reads, so it says both ends — and says bytes,
// because 72 of them is 72 Latin letters and 36 Cyrillic ones (spec 028 #31).
var ErrPasswordLength = fmt.Errorf(
	"a password must be between %d and %d bytes; a character outside plain ASCII takes two to four",
	MinPasswordLength, MaxPasswordLength)

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

// CheckPasswordLength is the rule alone, for a caller that has more to check
// before it may spend a hash on the answer (an invitation's token).
func CheckPasswordLength(password string) error {
	if len(password) < MinPasswordLength || len(password) > MaxPasswordLength {
		return ErrPasswordLength
	}
	return nil
}

// PasswordGate bounds the `bcrypt` computations in flight: a few at once, a
// short queue behind them, and a refusal for whatever arrives past that (spec
// 028 #31). A queue with no bound would only move the exhaustion from the CPU
// to the connections waiting on it.
//
// It lives here, beside the only two functions that run `bcrypt`, because
// both take the PasswordSlot only the gate hands out: a caller that forgot the
// gate does not compile, and one that passes nil or a slot it already gave
// back panics — rather than quietly spending the CPU the gate exists to
// protect. The server owns the gate — sizing it, and answering a refusal with
// a `503` — so it is a value and not a package-level variable, and so are its
// numbers (Spent, Waiting).
type PasswordGate struct {
	slots chan struct{}
	queue chan struct{}
	spent atomic.Int64
}

// ErrPasswordsBusy is the gate turning a caller away: the slots and the queue
// behind them are full.
var ErrPasswordsBusy = errors.New("the server is busy checking passwords; try again in a moment")

// NewPasswordGate makes a gate of slots places with room for queue callers to
// wait behind them.
func NewPasswordGate(slots, queue int) *PasswordGate {
	return &PasswordGate{slots: make(chan struct{}, max(slots, 1)), queue: make(chan struct{}, max(queue, 0))}
}

// Slots and Queue report the gate's size, for the log line that says it was
// full.
func (g *PasswordGate) Slots() int { return cap(g.slots) }
func (g *PasswordGate) Queue() int { return cap(g.queue) }

// Waiting reports how many callers are queued for a place right now.
func (g *PasswordGate) Waiting() int { return len(g.queue) }

// Spent reports how many hashes and comparisons this gate has let through —
// never the decoy's one-off construction. "Did this request run bcrypt" is a
// count, and a stopwatch on a loaded machine answers it wrongly.
func (g *PasswordGate) Spent() int64 { return g.spent.Load() }

// PasswordSlot is a place at the gate: what HashPassword and Verify demand, and
// what only Enter makes. Release gives it back; releasing twice is harmless,
// using it after is a panic.
type PasswordSlot struct {
	gate     *PasswordGate
	once     sync.Once
	released atomic.Bool
}

// Release gives the place back.
func (s *PasswordSlot) Release() {
	s.once.Do(func() {
		s.released.Store(true)
		<-s.gate.slots
	})
}

// Enter takes a place, waiting in the queue if there is room in it. It answers
// ErrPasswordsBusy when the queue is full, and the context's error when the
// caller gave up while it waited — which is not the gate being full, and is
// not to be reported as though it were.
func (g *PasswordGate) Enter(ctx context.Context) (*PasswordSlot, error) {
	select {
	case g.slots <- struct{}{}:
		return &PasswordSlot{gate: g}, nil
	default:
	}
	select {
	case g.queue <- struct{}{}:
	default:
		return nil, ErrPasswordsBusy
	}
	defer func() { <-g.queue }()
	select {
	case g.slots <- struct{}{}:
		return &PasswordSlot{gate: g}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// spend insists on a place at the gate held right now, and counts the work.
// Reaching bcrypt without one — nil, or a slot already given back — is a
// mistake in the code, not in the request, so it is a panic, which a test
// meets at once.
func (s *PasswordSlot) spend() {
	if s == nil || s.gate == nil || s.released.Load() {
		panic("store: bcrypt without a place held at the password gate")
	}
	s.gate.spent.Add(1)
}

// HashPassword checks the length and hashes. The two are one call because a
// caller that hashed first and validated afterwards would have spent a quarter
// of a second on a password it was going to refuse.
func HashPassword(slot *PasswordSlot, password string) ([]byte, error) {
	if err := CheckPasswordLength(password); err != nil {
		return nil, err
	}
	slot.spend()
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
func (a *Account) Verify(slot *PasswordSlot, password string) bool {
	slot.spend()
	if a == nil || len(a.hash) == 0 {
		bcrypt.CompareHashAndPassword(decoy(), []byte(password))
		return false
	}
	// Any error is a refusal, mismatch or unreadable hash alike: there is
	// nothing a caller could do with the difference, and saying which it
	// was would say whether the account has a password at all.
	return bcrypt.CompareHashAndPassword(a.hash, []byte(password)) == nil
}
