package server

import (
	"container/list"
	"errors"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

/*
What an unauthenticated caller can make this server spend on passwords.

Two limits, for two different attacks. The login limiter counts wrong answers
per email, which is what stops a dictionary run against one account. The
password gate (store.PasswordGate) bounds how many `bcrypt` computations run at
once, whoever asks for them, which is what stops a flood of requests — each one
a quarter of a second of CPU before anything knows who is calling — from taking
the machine from the ingest and the reads it exists for (spec 028 #31).
*/

// --- The login limiter ------------------------------------------------------

// Login throttling (Decision 8): five failures for one email inside fifteen
// minutes and the sixth is refused, counted in memory.
//
// In memory on purpose: this is a speed bump against a dictionary run, not an
// account lockout, so it costs no write, it forgets on a restart, and it can
// never be the reason somebody cannot sign in tomorrow.
const (
	loginFailureLimit  = 5
	loginFailureWindow = 15 * time.Minute
	// loginTrackedEmails bounds the map, so that a run against thousands of
	// invented addresses is not a way to spend the server's memory. Past
	// it, the record that counts the fewest attempts goes, the one touched
	// longest ago among those (evict). To push out an email with n failures,
	// a caller has to bring every other record to n first: 4096 × 4
	// comparisons through the password gate for an email one guess from
	// locked, which at the gate's widest (four at a time, a quarter of a
	// second each) is seventeen minutes — longer than the window that would
	// have let the email go anyway (spec 028 #31).
	loginTrackedEmails = 4096
)

type loginLimiter struct {
	mu       sync.Mutex
	capacity int
	entries  map[string]*loginRecord
	// byCount[n] holds the records that count n attempts — failures in the
	// window and attempts in flight, at most the limit — the most recently
	// touched at the front. Eviction takes the back of the lowest one that
	// is not empty, in constant time.
	byCount [loginFailureLimit + 1]*list.List
}

// loginRecord is one email: its failures inside the window, and how many
// attempts on it are being checked right now.
type loginRecord struct {
	key      string
	failures []time.Time
	pending  int
	// count and element place the record in byCount.
	count   int
	element *list.Element
}

func newLoginLimiter() *loginLimiter {
	l := &loginLimiter{capacity: loginTrackedEmails, entries: map[string]*loginRecord{}}
	for i := range l.byCount {
		l.byCount[i] = list.New()
	}
	return l
}

func loginKey(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// loginAttempt is a reservation: an attempt counted against its email from
// before the password is compared until the answer is known. Exactly one of
// failed, succeeded or cancel ends it.
type loginAttempt struct {
	limiter *loginLimiter
	key     string
}

// reserve counts an attempt on an email before its password is compared, or
// reports how long the email must wait.
//
// Counting first is what makes the limit a limit. Checking the count and
// recording the failure afterwards let every request of a burst read the same
// "four so far" and go on to compare: fifty at once were fifty guesses. Here
// an attempt in flight counts as a failure until it is known not to be one.
func (l *loginLimiter) reserve(email string, now time.Time) (*loginAttempt, time.Duration) {
	key := loginKey(email)
	l.mu.Lock()
	defer l.mu.Unlock()
	record := l.touch(key, now)
	if len(record.failures)+record.pending >= loginFailureLimit {
		wait := time.Second
		if len(record.failures) >= loginFailureLimit {
			// The oldest failure still inside the window is what has
			// to age out. Attempts in flight wait a second instead:
			// they are about to become failures or to clear the rest.
			wait = max(loginFailureWindow-now.Sub(record.failures[0]), time.Second)
		}
		l.settle(record)
		return nil, wait
	}
	record.pending++
	l.settle(record)
	l.evict(key)
	return &loginAttempt{limiter: l, key: key}, 0
}

// failed turns the reservation into a failure.
func (a *loginAttempt) failed(now time.Time) {
	l := a.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	// A record evicted while this attempt was in flight comes back: the
	// failure is real whatever happened to the map meanwhile.
	record := l.touch(a.key, now)
	record.pending = max(record.pending-1, 0)
	record.failures = append(record.failures, now)
	l.settle(record)
	l.evict(a.key)
}

// succeeded forgets the email's failures: a person who mistyped twice and then
// got it right is not somebody to throttle. Other attempts still in flight
// keep their reservations.
func (a *loginAttempt) succeeded() {
	a.end(func(record *loginRecord) { record.failures = nil })
}

// cancel gives the reservation back: the password was never compared, so the
// attempt was not one (the password gate turned it away).
func (a *loginAttempt) cancel() {
	a.end(func(*loginRecord) {})
}

func (a *loginAttempt) end(change func(*loginRecord)) {
	l := a.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	if record, ok := l.entries[a.key]; ok {
		record.pending = max(record.pending-1, 0)
		change(record)
		l.settle(record)
	}
}

// touch finds or makes an email's record and ages its failures. The caller
// holds the lock, and settles the record once it has changed it.
func (l *loginLimiter) touch(key string, now time.Time) *loginRecord {
	record, ok := l.entries[key]
	if !ok {
		record = &loginRecord{key: key}
		l.entries[key] = record
	}
	record.age(now)
	return record
}

// age drops the failures that have left the window.
func (r *loginRecord) age(now time.Time) {
	kept := r.failures[:0]
	for _, at := range r.failures {
		if now.Sub(at) < loginFailureWindow {
			kept = append(kept, at)
		}
	}
	r.failures = kept
}

// settle files a record under what it now counts, at the front of that list,
// or forgets it when it counts nothing. The caller holds the lock.
func (l *loginLimiter) settle(record *loginRecord) {
	if record.element != nil {
		l.byCount[record.count].Remove(record.element)
		record.element = nil
	}
	if len(record.failures) == 0 && record.pending == 0 {
		delete(l.entries, record.key)
		return
	}
	record.count = min(len(record.failures)+record.pending, loginFailureLimit)
	record.element = l.byCount[record.count].PushFront(record)
}

// evict brings the map back under its bound: the record counting the fewest
// attempts goes, and among those the one touched longest ago — never keep, the
// record the caller is working on. The caller holds the lock.
//
// Fewest first is the defence. Dropping the whole map when it filled let a
// caller who had spent four guesses on an email buy four more for 4097
// invented addresses, one guess each; so did dropping whichever record was
// oldest, locked out or not (spec 028 #31). A record's count is the one it was
// filed under when last touched, so failures that have aged out since keep it
// a little longer than they should: a bound on memory, not a lock on anybody.
func (l *loginLimiter) evict(keep string) {
	for len(l.entries) > l.capacity {
		victim := l.fewest(keep)
		if victim == nil {
			return
		}
		l.byCount[victim.count].Remove(victim.element)
		delete(l.entries, victim.key)
	}
}

// fewest is the record evict takes: the back of the lowest list, skipping keep.
func (l *loginLimiter) fewest(keep string) *loginRecord {
	for _, records := range l.byCount {
		for element := records.Back(); element != nil; element = element.Prev() {
			if record := element.Value.(*loginRecord); record.key != keep {
				return record
			}
		}
	}
	return nil
}

// retryAfterSeconds renders a wait for the header.
func retryAfterSeconds(wait time.Duration) string {
	seconds := int(wait.Round(time.Second) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	return strconv.Itoa(seconds)
}

// --- The password gate ------------------------------------------------------

// newPasswordGate sizes the gate for this machine: half its processors, at
// least one and at most four, so that ingest and reads keep at least half the
// CPU however many passwords are being checked (spec 028 #31). Four at a
// quarter of a second each is sixteen sign-ins a second, far past what any
// team does by hand. The queue holds four rounds of that — about a second of
// waiting — and a sign-in that would wait longer is told to come back.
func newPasswordGate() *store.PasswordGate {
	slots := min(max(runtime.GOMAXPROCS(0)/2, 1), 4)
	return store.NewPasswordGate(slots, 4*slots)
}

// enterPasswordGate takes a place for the request's `bcrypt` work, or answers
// itself: `503` with `Retry-After` when the gate is full, logged at most once a
// minute with the count it stands for — an operator should learn that
// sign-ins are being turned away, and a flood should not learn to fill the
// log. A caller that gave up while it waited in the queue is not the gate
// being full: nothing is logged or counted, and nothing is written to a
// connection that is gone.
func (s *Server) enterPasswordGate(w http.ResponseWriter, r *http.Request) (*store.PasswordSlot, bool) {
	slot, err := s.passwords.Enter(r.Context())
	if err == nil {
		return slot, true
	}
	if !errors.Is(err, store.ErrPasswordsBusy) {
		return nil, false
	}
	if skipped, log := s.passwordLog.allow(time.Now()); log {
		slog.Warn("password checks turned away: more were asked for at once than the gate holds",
			"slots", s.passwords.Slots(), "queue", s.passwords.Queue(), "also_turned_away", skipped)
	}
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusServiceUnavailable, store.ErrPasswordsBusy.Error())
	return nil, false
}
