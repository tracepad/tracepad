package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/store"
)

// What an unauthenticated caller can make the server spend on passwords (spec
// 028 #31): the limit is a limit under a burst, a spray of invented emails does
// not buy a locked-out one its tries back, a link that does not exist costs no
// hash, and the gate turns away what it cannot hold.

// TestLoginBurstStaysInsideTheLimit: fifty wrong passwords for one email at
// once are five guesses, not fifty. Checking the count and recording the
// failure afterwards let every request of the burst read the same count.
func TestLoginBurstStaysInsideTheLimit(t *testing.T) {
	h := newAccountHarness(t)
	h.owner(t)
	// Wide enough that nothing here is turned away at the gate: the
	// question is the limiter's.
	h.server.passwords = store.NewPasswordGate(64, 64)

	const burst = 50
	var (
		start sync.WaitGroup
		done  sync.WaitGroup
		mu    sync.Mutex
		codes = map[int]int{}
	)
	start.Add(1)
	work := h.server.passwords.Spent()
	for range burst {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			rec := h.login(t, "owner@example.com", "not the password")
			mu.Lock()
			codes[rec.Code]++
			mu.Unlock()
		}()
	}
	start.Done()
	done.Wait()

	if codes[http.StatusUnauthorized] > loginFailureLimit {
		t.Errorf("%d of %d guesses were compared, want at most %d (codes %v)",
			codes[http.StatusUnauthorized], burst, loginFailureLimit, codes)
	}
	if spent := h.server.passwords.Spent() - work; spent > loginFailureLimit {
		t.Errorf("the burst spent %d comparisons, want at most %d", spent, loginFailureLimit)
	}
	if codes[http.StatusUnauthorized]+codes[http.StatusTooManyRequests] != burst {
		t.Errorf("codes = %v, want only 401 and 429", codes)
	}
	// And the email stays locked for the right password too.
	expectStatus(t, h.login(t, "owner@example.com", testAccountPassword), http.StatusTooManyRequests)
}

// TestEmailSprayKeepsTheLockout: filling the limiter with invented addresses
// used to empty it, and a locked-out email had its five tries back for the
// price of 4097 requests. Eviction now takes what is not locked out first.
func TestEmailSprayKeepsTheLockout(t *testing.T) {
	l := newLoginLimiter()
	now := time.Now()
	fail := func(email string) bool {
		attempt, wait := l.reserve(email, now)
		if wait > 0 {
			return false
		}
		attempt.failed(now)
		return true
	}
	for range loginFailureLimit {
		if !fail("victim@example.com") {
			t.Fatal("the first five attempts must be let through")
		}
	}
	for i := range loginTrackedEmails + 1 {
		fail(fmt.Sprintf("spray-%d@example.com", i))
	}
	if _, wait := l.reserve("victim@example.com", now); wait == 0 {
		t.Fatal("a spray of invented emails gave a locked-out email its tries back")
	}
	if filed := l.filed(); len(l.entries) > loginTrackedEmails || filed != len(l.entries) {
		t.Errorf("the limiter holds %d entries (%d filed), want at most %d",
			len(l.entries), filed, loginTrackedEmails)
	}

	// One guess short of locked is kept too: dropping the oldest record,
	// locked or not, let four guesses and a spray buy four more.
	l = newLoginLimiter()
	for range loginFailureLimit - 1 {
		fail("victim@example.com")
	}
	for i := range loginTrackedEmails + 1 {
		fail(fmt.Sprintf("spray-%d@example.com", i))
	}
	if !fail("victim@example.com") {
		t.Fatal("the fifth attempt must be let through")
	}
	if _, wait := l.reserve("victim@example.com", now); wait == 0 {
		t.Fatal("a spray of invented emails reset an email one guess from locked")
	}

	// Past the bound with every entry locked, the oldest goes: memory is
	// bounded whatever is being attacked.
	small := newLoginLimiter()
	small.capacity = 3
	for _, email := range []string{"a@x", "b@x", "c@x", "d@x"} {
		for range loginFailureLimit {
			attempt, _ := small.reserve(email, now)
			if attempt != nil {
				attempt.failed(now)
			}
		}
	}
	if len(small.entries) != 3 {
		t.Fatalf("the limiter holds %d entries, want its capacity", len(small.entries))
	}
	if _, kept := small.entries["a@x"]; kept {
		t.Error("with every entry locked, the one touched longest ago must be the one dropped")
	}

	// A failure outside the window does not count on the email's next
	// attempt. (That an aged record is also the first to be evicted is
	// TestEvictionReadsTheCountNow's.)
	aged := newLoginLimiter()
	attempt, _ := aged.reserve("old@x", now.Add(-2*loginFailureWindow))
	attempt.failed(now.Add(-2 * loginFailureWindow))
	if _, wait := aged.reserve("old@x", now); wait != 0 {
		t.Error("a failure outside the window still counts")
	}
}

// TestEvictionReadsTheCountNow: a record is ranked for eviction by the
// failures it has inside the window now, not by the count it was filed under
// when last touched. Ranked by the stale count, a map filled once with records
// at four failures — long aged out — outranked an email with three live ones
// for ever after: three guesses, one request for a new address, and the
// email's count was gone, before any comparison and whether or not the gate
// then let that request in.
func TestEvictionReadsTheCountNow(t *testing.T) {
	l := newLoginLimiter()
	then := time.Now().Add(-2 * loginFailureWindow)
	for i := range loginTrackedEmails {
		for range loginFailureLimit - 1 {
			attempt, _ := l.reserve(fmt.Sprintf("stale-%d@example.com", i), then)
			attempt.failed(then)
		}
	}
	now := time.Now()
	guess := func(email string) time.Duration {
		attempt, wait := l.reserve(email, now)
		if wait == 0 {
			attempt.failed(now)
		}
		return wait
	}
	for round := range 3 {
		for range 3 {
			if wait := guess("victim@example.com"); wait != 0 && round == 0 {
				t.Fatal("the first three guesses must be let through")
			}
		}
		// A new address arrives, and the gate turns it away: the
		// reservation alone is what evicts.
		attempt, _ := l.reserve(fmt.Sprintf("new-%d@example.com", round), now)
		if attempt != nil {
			attempt.cancel()
		}
	}
	// Nine guesses were asked for; five are allowed in a window.
	if record, ok := l.entries["victim@example.com"]; !ok || len(record.failures) != loginFailureLimit {
		t.Fatalf("the victim holds %v, want exactly %d failures and a lockout", record, loginFailureLimit)
	}
	if _, wait := l.reserve("victim@example.com", now); wait == 0 {
		t.Error("the victim's lockout was evicted by records whose failures had aged out")
	}
	if filed := l.filed(); filed != len(l.entries) || len(l.entries) > loginTrackedEmails {
		t.Errorf("the limiter holds %d entries (%d filed), want at most %d", len(l.entries), filed, loginTrackedEmails)
	}
}

// TestLoginReservationEndsEveryWay: a reservation that was neither a failure
// nor a success — the gate was full — leaves nothing behind, and a success
// clears the failures without touching other attempts in flight.
func TestLoginReservationEndsEveryWay(t *testing.T) {
	l := newLoginLimiter()
	now := time.Now()
	attempt, _ := l.reserve("a@x", now)
	attempt.cancel()
	if len(l.entries) != 0 {
		t.Errorf("a cancelled reservation left %d entries", len(l.entries))
	}

	for range loginFailureLimit - 1 {
		attempt, _ := l.reserve("a@x", now)
		attempt.failed(now)
	}
	inFlight, _ := l.reserve("a@x", now)
	if _, wait := l.reserve("a@x", now); wait == 0 {
		t.Fatal("four failures and one attempt in flight are five; the sixth must wait")
	}
	inFlight.succeeded()
	if _, ok := l.entries["a@x"]; ok {
		t.Error("a success must forget the email's failures")
	}
}

// TestBogusInviteSpendsNoHash: an invitation token is checked before the
// password is hashed, so a request with no link at all costs a lookup, not a
// quarter of a second.
func TestBogusInviteSpendsNoHash(t *testing.T) {
	h := newAccountHarness(t)
	h.invited(t, "new@example.com", false)
	accept := func(token string) *httptest.ResponseRecorder {
		return h.call(t, "POST", "/api/v1/auth/accept-invite",
			mustJSON(t, map[string]any{"token": token, "password": testAccountPassword}),
			anonymous, func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) }, asJSON)
	}

	work := h.server.passwords.Spent()
	for i := range 100 {
		expectError(t, accept(fmt.Sprintf("no-such-token-%d", i)), http.StatusForbidden, store.ErrBadToken.Error())
	}
	if spent := h.server.passwords.Spent() - work; spent != 0 {
		t.Errorf("a hundred bogus tokens spent %d hashes, want none", spent)
	}

	// The real link still works, and pays for its one hash.
	work = h.server.passwords.Spent()
	expectStatus(t, accept("token-for-new@example.com"), http.StatusOK)
	if spent := h.server.passwords.Spent() - work; spent != 1 {
		t.Errorf("accepting an invitation spent %d hashes, want one", spent)
	}
	// Spent is spent: the second use is refused before any hash too.
	work = h.server.passwords.Spent()
	expectStatus(t, accept("token-for-new@example.com"), http.StatusForbidden)
	if spent := h.server.passwords.Spent() - work; spent != 0 {
		t.Errorf("a spent token spent %d hashes, want none", spent)
	}
}

// TestPasswordLongerThanBcryptReads: bcrypt reads 72 bytes and refuses more,
// so a longer password is a 422 that says so on every route that sets one —
// it used to pass validation and fail the hash with a 500.
func TestPasswordLongerThanBcryptReads(t *testing.T) {
	h := newAccountHarness(t)
	tooLong := strings.Repeat("p", store.MaxPasswordLength+1)
	// 37 two-byte letters: 37 characters, 74 bytes.
	tooLongCyrillic := strings.Repeat("\u0436", 37)
	exactly := strings.Repeat("p", store.MaxPasswordLength)

	token := strings.TrimPrefix(h.server.SetupURL(), h.server.originForPrint()+"/setup#token=")
	setup := func(password string) *httptest.ResponseRecorder {
		return h.call(t, "POST", "/api/v1/setup", mustJSON(t, map[string]any{
			"token": token, "email": "founder@example.com", "password": password,
		}), anonymous, asJSON)
	}
	expectError(t, setup(tooLong), http.StatusUnprocessableEntity, "72 bytes")
	expectError(t, setup(tooLongCyrillic), http.StatusUnprocessableEntity, "72 bytes")

	h.invited(t, "new@example.com", false)
	expectError(t, h.call(t, "POST", "/api/v1/auth/accept-invite",
		mustJSON(t, map[string]any{"token": "token-for-new@example.com", "password": tooLong}),
		anonymous, asJSON), http.StatusUnprocessableEntity, "72 bytes")

	expectStatus(t, setup(exactly), http.StatusCreated)
	owner := h.owner(t)
	expectError(t, h.call(t, "PATCH", "/api/v1/auth/me", mustJSON(t, map[string]any{
		"password": map[string]any{"current": testAccountPassword, "new": tooLong},
	}), asSession(owner)), http.StatusUnprocessableEntity, "72 bytes")
}

// TestPasswordGateTurnsAway: past what the gate holds, a request that would
// spend bcrypt is told to come back — 503 with Retry-After — rather than
// queued without a bound; and a login turned away there was never an attempt.
func TestPasswordGateTurnsAway(t *testing.T) {
	h := newAccountHarness(t)
	h.owner(t)
	h.server.passwords = store.NewPasswordGate(1, 0)
	slot, err := h.server.passwords.Enter(context.Background())
	if err != nil {
		t.Fatal("an empty gate must let one in")
	}

	for range loginFailureLimit + 1 {
		rec := h.login(t, "owner@example.com", "not the password")
		expectError(t, rec, http.StatusServiceUnavailable, store.ErrPasswordsBusy.Error())
		if rec.Header().Get("Retry-After") != "1" {
			t.Errorf("Retry-After = %q, want 1", rec.Header().Get("Retry-After"))
		}
	}
	if len(h.server.limiter.entries) != 0 {
		t.Error("logins turned away at the gate were counted as attempts")
	}
	// Setup's hash waits at the same gate.
	token := strings.TrimPrefix(h.server.SetupURL(), h.server.originForPrint()+"/setup#token=")
	expectError(t, h.call(t, "POST", "/api/v1/setup", mustJSON(t, map[string]any{
		"token": token, "email": "founder@example.com", "password": testAccountPassword,
	}), anonymous, asJSON), http.StatusServiceUnavailable, store.ErrPasswordsBusy.Error())

	slot.Release()
	expectStatus(t, h.login(t, "owner@example.com", testAccountPassword), http.StatusOK)
}

// TestPasswordGateLetsAGiveUpGo: a caller that hung up while it waited in the
// queue is not the gate being full. Nothing is logged, nothing counted, and
// nothing written to a connection that is gone.
func TestPasswordGateLetsAGiveUpGo(t *testing.T) {
	h := newAccountHarness(t)
	h.server.passwords = store.NewPasswordGate(1, 1)
	slot, _ := h.server.passwords.Enter(context.Background())
	defer slot.Release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	if _, ok := h.server.enterPasswordGate(rec, httptest.NewRequest("POST", "/", nil).WithContext(ctx)); ok {
		t.Fatal("a caller that gave up must not be let in")
	}
	if rec.Body.Len() != 0 || rec.Header().Get("Retry-After") != "" {
		t.Errorf("wrote %q with Retry-After %q to a caller that is gone", rec.Body.String(), rec.Header().Get("Retry-After"))
	}
	if _, log := h.server.passwordLog.Allow("", time.Now()); !log {
		t.Error("a caller that gave up used up the gate's log line")
	}
}

// TestSetupLinkExpires: the printed link works for a day, not for the life of
// the process (Decision 32).
func TestSetupLinkExpires(t *testing.T) {
	h := newAccountHarness(t)
	token := strings.TrimPrefix(h.server.SetupURL(), h.server.originForPrint()+"/setup#token=")
	if token == "" {
		t.Fatal("no setup link")
	}
	h.server.setupMu.Lock()
	if left := time.Until(h.server.setupExpires); left < setupTokenLife-time.Minute || left > setupTokenLife {
		t.Errorf("the link expires in %s, want %s", left, setupTokenLife)
	}
	h.server.setupExpires = time.Now().Add(-time.Second)
	h.server.setupMu.Unlock()

	if got := h.server.SetupURL(); got != "" {
		t.Errorf("SetupURL = %q after expiry, want none", got)
	}
	// The interface can say so before anybody fills in the form.
	if got := h.call(t, "GET", "/api/v1/setup", nil, anonymous).Body.String(); !strings.Contains(got, `"expired":true`) ||
		!strings.Contains(got, `"enabled":true`) {
		t.Errorf("GET /setup = %s after expiry, want expired and enabled", got)
	}
	expectError(t, h.call(t, "POST", "/api/v1/setup", mustJSON(t, map[string]any{
		"token": token, "email": "founder@example.com", "password": testAccountPassword,
	}), anonymous, asJSON), http.StatusForbidden, "restart the server")
}

// TestSetupOff: TRACEPAD_SETUP=off mints no token and refuses the endpoint
// with what to do instead (Decision 32).
func TestSetupOff(t *testing.T) {
	h := newHarness(t, &config.Config{
		Listen: ":0", MaxBodyBytes: config.DefaultMaxBodyBytes,
		AdminToken: adminToken, SetupDisabled: true,
	}, store.WriterOptions{})
	if got := h.server.SetupURL(); got != "" {
		t.Errorf("SetupURL = %q with setup off, want none", got)
	}
	// The interface is told, so it can say what to do rather than ask for
	// a link that will never come.
	rec := h.call(t, "GET", "/api/v1/setup", nil, anonymous)
	expectStatus(t, rec, 200)
	if got := rec.Body.String(); !strings.Contains(got, `"required":true`) ||
		!strings.Contains(got, `"enabled":false`) || !strings.Contains(got, `"expired":false`) {
		t.Errorf("GET /setup = %s, want required, not enabled, not expired", got)
	}
	work := h.server.passwords.Spent()
	expectError(t, h.call(t, "POST", "/api/v1/setup", mustJSON(t, map[string]any{
		"token": "anything", "email": "founder@example.com", "password": testAccountPassword,
	}), anonymous, asJSON), http.StatusForbidden, "TRACEPAD_SETUP=off")
	// The command it names runs as printed: the CLI's credential is
	// TRACEPAD_API_KEY, and here that is the admin token.
	if got := decodeJSON[struct {
		Error string `json:"error"`
	}](t, h.call(t, "POST", "/api/v1/setup", mustJSON(t, map[string]any{"token": "x"}), anonymous, asJSON)).Error; !strings.Contains(got, "TRACEPAD_API_KEY=<the admin token> tracepad accounts create") {
		t.Errorf("setup-off answer = %q, want the command with its credential", got)
	}
	// Whatever the body says: the answer comes before it is read.
	expectError(t, h.call(t, "POST", "/api/v1/setup", []byte(`{"token": `), anonymous, asJSON),
		http.StatusForbidden, "TRACEPAD_SETUP=off")
	if h.server.passwords.Spent() != work {
		t.Error("a refused setup spent a hash")
	}
	// The admin token is the way in instead.
	expectStatus(t, h.call(t, "POST", "/api/v1/accounts",
		mustJSON(t, map[string]any{"email": "founder@example.com", "owner": true}),
		asAdmin), http.StatusCreated)
}

// filed counts the records in the limiter's eviction lists, which must be every
// record it holds and nothing else.
func (l *loginLimiter) filed() int {
	n := 0
	for _, records := range l.byCount {
		n += records.Len()
	}
	return n
}

// TestWrongCurrentPasswordIsAGuess: the current password on PATCH /auth/me is
// a password guess like a login's, counted before the comparison — so a
// session held by somebody else guesses no faster than the login allows, and
// twenty at once are five comparisons, not a gate kept full for everybody
// signing in. It is counted per account and apart from the login's count by
// email: a stranger failing at the login cannot stop the person who holds the
// session from changing the password (spec 028 #31).
func TestWrongCurrentPasswordIsAGuess(t *testing.T) {
	h := newAccountHarness(t)
	who := h.owner(t)
	h.server.passwords = store.NewPasswordGate(64, 64)
	change := func(current string) *httptest.ResponseRecorder {
		return h.call(t, "PATCH", "/api/v1/auth/me", mustJSON(t, map[string]any{
			"password": map[string]any{"current": current, "new": "a brand new password"},
		}), asSession(who))
	}

	const burst = 20
	var (
		done  sync.WaitGroup
		mu    sync.Mutex
		codes = map[int]int{}
	)
	work := h.server.passwords.Spent()
	for i := range burst {
		done.Add(1)
		go func() {
			defer done.Done()
			rec := change(fmt.Sprintf("guess number %d", i))
			mu.Lock()
			codes[rec.Code]++
			mu.Unlock()
		}()
	}
	done.Wait()
	if codes[http.StatusForbidden] > loginFailureLimit || codes[http.StatusForbidden]+codes[http.StatusTooManyRequests] != burst {
		t.Errorf("codes = %v, want at most %d wrong-password answers and the rest 429", codes, loginFailureLimit)
	}
	if spent := h.server.passwords.Spent() - work; spent > loginFailureLimit {
		t.Errorf("the burst spent %d comparisons, want at most %d", spent, loginFailureLimit)
	}
	// Locked here, the right current password waits too — and the login,
	// which is counted apart, is untouched.
	expectStatus(t, change(testAccountPassword), http.StatusTooManyRequests)
	expectStatus(t, h.login(t, "owner@example.com", testAccountPassword), http.StatusOK)

	// And the other way round: a stranger locking the login out does not
	// stop the session's owner changing the password.
	other := h.account(t, "other@example.com", true)
	for range loginFailureLimit {
		h.login(t, "other@example.com", "not the password")
	}
	expectStatus(t, h.login(t, "other@example.com", testAccountPassword), http.StatusTooManyRequests)
	expectStatus(t, h.call(t, "PATCH", "/api/v1/auth/me", mustJSON(t, map[string]any{
		"password": map[string]any{"current": testAccountPassword, "new": "a brand new password"},
	}), asSession(other)), http.StatusOK)
}

// TestAPanicGivesThePlaceBack: whatever runs under the gate, the place comes
// back when it ends — a panic included, which net/http recovers from and a
// place taken by hand and given back by hand did not survive.
func TestAPanicGivesThePlaceBack(t *testing.T) {
	h := newAccountHarness(t)
	h.server.passwords = store.NewPasswordGate(1, 0)
	func() {
		defer func() { _ = recover() }()
		h.server.underPasswordGate(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil),
			func(*store.PasswordSlot) { panic("a mistake in the code") })
	}()
	slot, err := h.server.passwords.Enter(context.Background())
	if err != nil {
		t.Fatalf("the place a panic held is gone: %v", err)
	}
	slot.Release()
}

// TestAnAttemptEndsOnlyItsOwnPlace: when an email's record is evicted while
// an attempt on it is in flight and the email comes back, the record under the
// key is a newer attempt's. The older one ending must not take the newer one's
// place in flight — the burst limit would let one more comparison through for
// each — though its failure still counts.
func TestAnAttemptEndsOnlyItsOwnPlace(t *testing.T) {
	l := newLoginLimiter()
	l.capacity = 1
	now := time.Now()
	older, _ := l.reserve("a@x", now)
	// Another email with a failure outranks a's one attempt in flight, so a
	// is the record evicted.
	for range 2 {
		attempt, _ := l.reserve("b@x", now)
		attempt.failed(now)
	}
	if _, ok := l.entries["a@x"]; ok {
		t.Fatal("the probe needs a's record evicted")
	}
	newer, _ := l.reserve("a@x", now)
	older.failed(now)
	record := l.entries["a@x"]
	if record == nil || record.pending != 1 || len(record.failures) != 1 {
		t.Fatalf("a's record is %+v, want the newer attempt still in flight and the older one's failure", record)
	}
	older.cancel() // ended already: nothing more
	if record.pending != 1 {
		t.Error("ending an attempt twice took another's place")
	}
	newer.cancel()
	if record.pending != 0 {
		t.Error("the newer attempt could not end its own place")
	}

	// A success clears the email's failures whichever record holds them:
	// the right password proves it, though the record was made again
	// while the attempt was in flight.
	l = newLoginLimiter()
	l.capacity = 1
	right, _ := l.reserve("c@x", now)
	for range 2 {
		attempt, _ := l.reserve("d@x", now)
		attempt.failed(now)
	}
	for range 3 {
		attempt, _ := l.reserve("c@x", now)
		attempt.failed(now)
	}
	right.succeeded()
	if record, ok := l.entries["c@x"]; ok && len(record.failures) != 0 {
		t.Errorf("a right password left %d failures on the email", len(record.failures))
	}
}
