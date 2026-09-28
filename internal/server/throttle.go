package server

import (
	"container/heap"
	"container/list"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

/*
What an unauthenticated caller can make this server spend on passwords.

Three limits, for three different attacks. The login limiter counts wrong
answers per email, which is what stops a dictionary run against one account.
The password gate (store.PasswordGate) bounds how many `bcrypt` computations
run at once, whoever asks for them, which is what stops a flood of requests —
each one a quarter of a second of CPU before anything knows who is calling —
from taking the machine from the ingest and the reads it exists for (spec 028
#31). And the source limiter asks, at the gate's door, where the request
comes from, so that a flood from one address spends one comparison every three
seconds and the people signing in from anywhere else never meet the gate full
(spec 046).
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
	// invented addresses is not a way to spend the server's memory — at
	// most about 4 MB, a 254-byte email and five timestamps apiece. Past
	// it, the record that counts the fewest attempts goes, the one touched
	// longest ago among those (evict), and the counts are exact: a failure
	// leaves its record's count the moment it leaves the window (expire).
	//
	// So pushing out an email with c failures takes every other record held
	// at c or more, inside one window: c × 8192 comparisons in fifteen
	// minutes. The gate lets through at most four at a time; at a quarter
	// of a second each that is 14,400 a window, short of the 16,384 even
	// c = 2 needs, and where bcrypt takes a tenth of a second, 36,000 — c
	// = 4 then costs 32,768, over thirteen minutes of the whole gate, to
	// win four guesses the window would have given back two minutes later.
	// Either way the spray buys fewer guesses than waiting does (spec 028
	// #31).
	loginTrackedEmails = 8192
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
	// marks holds every failure recorded in the last window, oldest first,
	// with the record it was counted on, so that expire can take each one
	// off its record's count when it ages out — without which a record
	// kept the count it was filed under when it was last touched, and a
	// map full of records whose failures had long aged out outranked an
	// email with live ones (spec 028 #31). It holds at most what the gate
	// let through in one window.
	marks *list.List
}

// failureMark is one failure in marks.
type failureMark struct {
	record *loginRecord
	at     time.Time
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
	l := &loginLimiter{capacity: loginTrackedEmails, entries: map[string]*loginRecord{}, marks: list.New()}
	for i := range l.byCount {
		l.byCount[i] = list.New()
	}
	return l
}

func loginKey(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// loginAttempt is a reservation: an attempt counted against its email from
// before the password is compared until the answer is known. The first of
// failed, succeeded or cancel ends it and the rest do nothing, so a handler
// can defer cancel as soon as it holds one — a panic between the two is then
// an attempt given back rather than one in flight for ever.
//
// It remembers the record it was counted on: once that record has been
// evicted and the email has come back, the record under the key is another
// attempt's, and this one must not take that one's place in flight.
type loginAttempt struct {
	limiter *loginLimiter
	key     string
	record  *loginRecord
	done    bool
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
	l.expire(now)
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
	return &loginAttempt{limiter: l, key: key, record: record}, 0
}

// failed turns the reservation into a failure.
func (a *loginAttempt) failed(now time.Time) {
	if a.done {
		return
	}
	a.done = true
	l := a.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	l.expire(now)
	// A record evicted while this attempt was in flight comes back: the
	// failure is real whatever happened to the map meanwhile. Only its own
	// record's place in flight is this attempt's to give back.
	if a.owns() {
		a.record.pending = max(a.record.pending-1, 0)
	}
	record := l.touch(a.key, now)
	record.failures = append(record.failures, now)
	l.marks.PushBack(failureMark{record: record, at: now})
	l.settle(record)
	l.evict(a.key)
}

// expire takes every failure that has left the window off its record's count,
// oldest first, and files the record again under what it now counts. A mark
// whose record has been evicted or forgotten since is dropped: the record it
// names is not the one the map holds. The caller holds the lock.
func (l *loginLimiter) expire(now time.Time) {
	for front := l.marks.Front(); front != nil; front = l.marks.Front() {
		mark := front.Value.(failureMark)
		if now.Sub(mark.at) < loginFailureWindow {
			return
		}
		l.marks.Remove(front)
		if record, ok := l.entries[mark.record.key]; ok && record == mark.record {
			record.age(now)
			l.settle(record)
		}
	}
}

// succeeded forgets the email's failures: a person who mistyped twice and then
// got it right is not somebody to throttle. The right password proves it
// whichever record now holds the email's failures — one evicted while this
// attempt was in flight and made again carries them too — so those go
// whatever record it is; only this attempt's own place in flight is given
// back, and other attempts keep theirs.
func (a *loginAttempt) succeeded() {
	if a.done {
		return
	}
	a.done = true
	l := a.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	record, ok := l.entries[a.key]
	if !ok {
		return
	}
	if record == a.record {
		record.pending = max(record.pending-1, 0)
	}
	record.failures = nil
	l.settle(record)
}

// cancel gives the reservation back: the password was never compared, so the
// attempt was not one (the password gate turned it away).
func (a *loginAttempt) cancel() {
	if a.done {
		return
	}
	a.done = true
	l := a.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	if a.owns() {
		a.record.pending = max(a.record.pending-1, 0)
		l.settle(a.record)
	}
}

// owns reports whether the record under the attempt's key is still the one it
// was counted on. The caller holds the lock.
func (a *loginAttempt) owns() bool {
	record, ok := a.limiter.entries[a.key]
	return ok && record == a.record
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
// Fewest first is the defence, and it holds because the counts are exact.
// Dropping the whole map when it filled let a caller who had spent four
// guesses on an email buy four more for 4097 invented addresses, one guess
// each; so did dropping whichever record was oldest, locked out or not; and so
// did ranking records by a count nothing lowered as their failures aged out
// (spec 028 #31).
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
	return strconv.Itoa(waitSeconds(wait))
}

// waitSeconds is a wait in whole seconds, rounded up and at least one: a
// client that waits that long is not turned away again for waiting too little
// (spec 046 #16). Rounded to the nearest, a wait of 2.4 s said 2.
func waitSeconds(wait time.Duration) int {
	return max(int((wait+time.Second-1)/time.Second), 1)
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
	// The source first (spec 046 #6): every request that would run bcrypt
	// passes here, whatever its route, so the limit is exact — a source
	// spends at most its rate in comparisons — and a route added later that
	// hashes a password is limited without anybody listing it. A caller's
	// reservation (the email's, the account's) is given back on this way
	// out as on any other that compared nothing (#8).
	if !s.admitSource(w, r) {
		return nil, false
	}
	slot, err := s.passwords.Enter(r.Context())
	if err == nil {
		return slot, true
	}
	if !errors.Is(err, store.ErrPasswordsBusy) {
		return nil, false
	}
	if skipped, log := s.passwordLog.Allow("", time.Now()); log {
		slog.Warn("password checks turned away: more were asked for at once than the gate holds",
			"slots", s.passwords.Slots(), "queue", s.passwords.Queue(), "also_turned_away", skipped.SameKey)
	}
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusServiceUnavailable, store.ErrPasswordsBusy.Error())
	return nil, false
}

// underPasswordGate runs work holding a place at the password gate, and gives
// the place back however work ends — a panic included: net/http recovers a
// handler that panics, and a place it had taken would be gone until a restart,
// a few of them a gate that answers every sign-in 503. It reports false,
// having answered the client, when the gate turned the request away.
func (s *Server) underPasswordGate(w http.ResponseWriter, r *http.Request, work func(*store.PasswordSlot)) bool {
	slot, ok := s.enterPasswordGate(w, r)
	if !ok {
		return false
	}
	defer slot.Release()
	work(slot)
	return true
}

// hashUnderGate hashes a password whose length the caller has already checked
// — the one check that answers the person, with a 422 — under a place at the
// gate, answering 503 or 500 itself.
func (s *Server) hashUnderGate(w http.ResponseWriter, r *http.Request, password string) ([]byte, bool) {
	var (
		hash []byte
		ok   bool
	)
	if !s.underPasswordGate(w, r, func(slot *store.PasswordSlot) { hash, ok = hashPassword(w, slot, password) }) {
		return nil, false
	}
	return hash, ok
}

// --- The source limiter -----------------------------------------------------

// The limit on password checks per source (spec 046 #7, #9): twenty at once,
// then one every three seconds, for at most 32,768 sources. Twenty covers a
// team arriving at once from one office's NAT; one every three seconds is
// more than a person types, and at a quarter of a second of bcrypt each, a
// twelfth of one core for a source that never stops. Constants, as the
// email's five in fifteen minutes are: they shape what a client meets, and
// a deployment that wants other numbers has a proxy that can say so.
const (
	sourceBurst   = 20
	sourceEvery   = 3 * time.Second
	sourceTracked = 32768
)

// sourceRefused is the one sentence a source over its limit gets, with the
// seconds its Retry-After carries. "Network" rather than "address": an IPv6
// source is a /64.
const sourceRefused = "too many password checks from your network; try again in %d seconds"

// sourceLimiter is GCRA — a token bucket kept as one instant per source, its
// theoretical arrival time (TAT). A request at now is admitted when
// max(TAT, now) − now ≤ τ, τ being (burst − 1) × every, and TAT then moves to
// max(TAT, now) + every. A refused request leaves TAT where it was, so a
// source that keeps asking gets exactly the sustained rate rather than a
// lockout it extends itself.
//
// Sources are held in a map and in a min-heap by TAT. A source whose TAT is
// not after now holds a full bucket, which is the same as not being held, so
// every operation first takes those off the heap: they leave without changing
// any answer. Past the capacity the source with the least debt goes — the
// one that would be whole again soonest. Unlike a count in a window, an
// order by TAT cannot go stale as time passes (spec 028 #31 c's lesson):
// pushing out a source that owes d takes holding every other one owing more,
// inside the minute any debt lasts — tens of thousands of admitted checks a
// minute, against a gate that passes under a thousand.
//
// Times are time.Now's, and their differences are the monotonic clock's, so
// a step of the wall clock neither fills nor drains every bucket at once.
type sourceLimiter struct {
	mu       sync.Mutex
	burst    int
	every    time.Duration
	capacity int
	entries  map[netip.Prefix]*sourceEntry
	byTAT    sourceHeap
}

type sourceEntry struct {
	source netip.Prefix
	tat    time.Time
	index  int
}

func newSourceLimiter() *sourceLimiter {
	return &sourceLimiter{burst: sourceBurst, every: sourceEvery, capacity: sourceTracked,
		entries: map[netip.Prefix]*sourceEntry{}}
}

// take spends one of the source's tokens, or reports how long until one is
// back.
func (l *sourceLimiter) take(source netip.Prefix, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for len(l.byTAT) > 0 && !l.byTAT[0].tat.After(now) {
		l.drop(heap.Pop(&l.byTAT).(*sourceEntry))
	}
	// Every source still held owes something: the ones that did not
	// were taken off above.
	entry := l.entries[source]
	start := now
	if entry != nil {
		start = entry.tat
	}
	tolerance := time.Duration(l.burst-1) * l.every
	if owed := start.Sub(now); owed > tolerance {
		return owed - tolerance, false
	}
	if entry != nil {
		entry.tat = start.Add(l.every)
		heap.Fix(&l.byTAT, entry.index)
		return 0, true
	}
	if len(l.entries) >= l.capacity {
		l.drop(heap.Pop(&l.byTAT).(*sourceEntry))
	}
	entry = &sourceEntry{source: source, tat: start.Add(l.every)}
	l.entries[source] = entry
	heap.Push(&l.byTAT, entry)
	return 0, true
}

func (l *sourceLimiter) drop(entry *sourceEntry) { delete(l.entries, entry.source) }

// sourceHeap orders sources by TAT, the least debt first.
type sourceHeap []*sourceEntry

func (h sourceHeap) Len() int           { return len(h) }
func (h sourceHeap) Less(i, j int) bool { return h[i].tat.Before(h[j].tat) }
func (h sourceHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = i, j
}
func (h *sourceHeap) Push(x any) {
	entry := x.(*sourceEntry)
	entry.index = len(*h)
	*h = append(*h, entry)
}
func (h *sourceHeap) Pop() any {
	old := *h
	entry := old[len(old)-1]
	old[len(old)-1] = nil
	*h = old[:len(old)-1]
	return entry
}

// admitSource spends a token of the request's source, or answers `429` with
// the seconds until one is back (spec 046 #8), logged at most once a minute
// per source and for at most sixteen sources a minute, each line with the
// count it stands for (#10).
func (s *Server) admitSource(w http.ResponseWriter, r *http.Request) bool {
	now := time.Now()
	client := s.clientAddress(r)
	source := sourceOf(client)
	wait, ok := s.sources.take(source, now)
	if ok {
		return true
	}
	seconds := waitSeconds(wait)
	text := sourceText(source)
	if held, log := s.sourceLog.Allow(text, now); log {
		attrs := []any{"source", text, "burst", sourceBurst, "every", sourceEvery.String(),
			"not_logged_since_last", held.SameKey, "not_logged_over_cap", held.OverCap}
		// A source that is itself a trusted proxy forwarded no client:
		// every client behind it is this one source, which is a proxy's
		// setting to fix, not a flood (#18). nginx needs telling.
		if s.trusted.trusts(client) {
			attrs = append(attrs, "hint", "this source is a trusted proxy that sent no client address in X-Forwarded-For, "+
				"so every client behind it counts as this one source; configure the proxy to append X-Forwarded-For")
		}
		slog.Warn("password checks refused: a source asked for more than its limit", attrs...)
	}
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	writeError(w, http.StatusTooManyRequests, fmt.Sprintf(sourceRefused, seconds))
	return false
}
