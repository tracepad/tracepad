package server

import (
	"container/list"
	"context"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

/*
What an unauthenticated caller can make this server spend on passwords.

Two limits, for two different attacks. The login limiter counts wrong answers
per email, which is what stops a dictionary run against one account. The
password gate bounds how many `bcrypt` computations run at once, whoever asks
for them, which is what stops a flood of requests — each one a quarter of a
second of CPU before anything knows who is calling — from taking the machine
from the ingest and the reads it exists for (spec 028 #31).
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
	// it, the entry touched longest ago is dropped — but an email that is
	// locked out goes last (evict). To push one out, a caller has to lock
	// out every other entry first: 4096 × 5 comparisons through the password
	// gate, which at its widest (four at a time, a quarter of a second
	// each) is over twenty minutes — longer than the window that would
	// have let the email go anyway (spec 028 #31).
	loginTrackedEmails = 4096
)

type loginLimiter struct {
	mu       sync.Mutex
	capacity int
	// entries finds an email's record; order holds the same records,
	// the most recently touched at the front, for eviction.
	entries map[string]*list.Element
	order   *list.List
}

// loginRecord is one email: its failures inside the window, and how many
// attempts on it are being checked right now.
type loginRecord struct {
	key      string
	failures []time.Time
	pending  int
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{capacity: loginTrackedEmails, entries: map[string]*list.Element{}, order: list.New()}
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
		l.forgetIfEmpty(record)
		return nil, wait
	}
	record.pending++
	l.evict(now)
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
	l.evict(now)
}

// succeeded forgets the email's failures: a person who mistyped twice and then
// got it right is not somebody to throttle. Other attempts still in flight
// keep their reservations.
func (a *loginAttempt) succeeded() {
	l := a.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	if element, ok := l.entries[a.key]; ok {
		record := element.Value.(*loginRecord)
		record.pending = max(record.pending-1, 0)
		record.failures = nil
		l.forgetIfEmpty(record)
	}
}

// cancel gives the reservation back: the password was never compared, so the
// attempt was not one (the password gate was full).
func (a *loginAttempt) cancel() {
	l := a.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	if element, ok := l.entries[a.key]; ok {
		record := element.Value.(*loginRecord)
		record.pending = max(record.pending-1, 0)
		l.forgetIfEmpty(record)
	}
}

// touch finds or makes an email's record, ages its failures and moves it to
// the front. The caller holds the lock.
func (l *loginLimiter) touch(key string, now time.Time) *loginRecord {
	element, ok := l.entries[key]
	if !ok {
		element = l.order.PushFront(&loginRecord{key: key})
		l.entries[key] = element
	} else {
		l.order.MoveToFront(element)
	}
	record := element.Value.(*loginRecord)
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

// forgetIfEmpty drops a record that counts nothing. The caller holds the lock.
func (l *loginLimiter) forgetIfEmpty(record *loginRecord) {
	if len(record.failures) == 0 && record.pending == 0 {
		l.remove(record.key)
	}
}

func (l *loginLimiter) remove(key string) {
	if element, ok := l.entries[key]; ok {
		l.order.Remove(element)
		delete(l.entries, key)
	}
}

// evict brings the map back under its bound, from the end touched longest ago:
// first whatever has aged out, then emails that are not locked out and have no
// attempt in flight, and only when every record left is one of those, the
// oldest of them. The caller holds the lock.
//
// The order is the defence. Dropping the whole map when it filled — what this
// did before — gave a locked-out email its five tries back for the price of
// 4097 invented addresses, one guess each (spec 028 #31).
func (l *loginLimiter) evict(now time.Time) {
	for len(l.entries) > l.capacity {
		var victim *loginRecord
		for element := l.order.Back(); element != nil; element = element.Prev() {
			record := element.Value.(*loginRecord)
			record.age(now)
			if len(record.failures) < loginFailureLimit && record.pending == 0 {
				victim = record
				break
			}
		}
		if victim == nil {
			victim = l.order.Back().Value.(*loginRecord)
		}
		l.remove(victim.key)
	}
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

// passwordGate bounds the `bcrypt` computations in flight: a few at once, a
// short queue behind them, and a `503` for whatever arrives past that. A
// queue with no bound would only move the exhaustion from the CPU to the
// connections waiting on it.
type passwordGate struct {
	slots chan struct{}
	queue chan struct{}
}

// newPasswordGate sizes the gate for this machine: half its processors, at
// least one and at most four, so that ingest and reads keep at least half the
// CPU however many passwords are being checked (spec 028 #31). Four at a
// quarter of a second each is sixteen sign-ins a second, far past what any
// team does by hand. The queue holds four rounds of that — about a second of
// waiting — and a sign-in that would wait longer is told to come back.
func newPasswordGate() *passwordGate {
	slots := min(max(runtime.GOMAXPROCS(0)/2, 1), 4)
	return newPasswordGateOf(slots, 4*slots)
}

func newPasswordGateOf(slots, queue int) *passwordGate {
	return &passwordGate{slots: make(chan struct{}, slots), queue: make(chan struct{}, queue)}
}

// enter takes a slot, waiting behind the queue if there is room in it, and
// returns what gives the slot back. It reports false when the queue is full or
// the caller gave up while waiting.
func (g *passwordGate) enter(ctx context.Context) (release func(), ok bool) {
	release = func() { <-g.slots }
	select {
	case g.slots <- struct{}{}:
		return release, true
	default:
	}
	select {
	case g.queue <- struct{}{}:
	default:
		return nil, false
	}
	defer func() { <-g.queue }()
	select {
	case g.slots <- struct{}{}:
		return release, true
	case <-ctx.Done():
		return nil, false
	}
}

// passwordBusy is the sentence a request turned away at the gate reads.
const passwordBusy = "the server is busy checking passwords; try again in a moment"

// enterPasswordGate takes a slot for the request's `bcrypt` work, or answers
// `503` with `Retry-After` itself. The refusal is logged at most once a minute,
// with the count it stands for: an operator should learn that sign-ins are
// being turned away, and a flood should not learn to fill the log.
func (s *Server) enterPasswordGate(w http.ResponseWriter, r *http.Request) (release func(), ok bool) {
	release, ok = s.passwords.enter(r.Context())
	if ok {
		return release, true
	}
	if skipped, log := s.passwordLog.allow(time.Now()); log {
		slog.Warn("password checks turned away: more were asked for at once than the gate holds",
			"slots", cap(s.passwords.slots), "queue", cap(s.passwords.queue), "also_turned_away", skipped)
	}
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusServiceUnavailable, passwordBusy)
	return nil, false
}
