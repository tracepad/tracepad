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
	work := store.PasswordWork()
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
	if spent := store.PasswordWork() - work; spent > loginFailureLimit {
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

	// Failures age out of the window, and an aged record is the first to go.
	aged := newLoginLimiter()
	attempt, _ := aged.reserve("old@x", now.Add(-2*loginFailureWindow))
	attempt.failed(now.Add(-2 * loginFailureWindow))
	if _, wait := aged.reserve("old@x", now); wait != 0 {
		t.Error("a failure outside the window still counts")
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

	work := store.PasswordWork()
	for i := range 100 {
		expectError(t, accept(fmt.Sprintf("no-such-token-%d", i)), http.StatusForbidden, store.ErrBadToken.Error())
	}
	if spent := store.PasswordWork() - work; spent != 0 {
		t.Errorf("a hundred bogus tokens spent %d hashes, want none", spent)
	}

	// The real link still works, and pays for its one hash.
	work = store.PasswordWork()
	expectStatus(t, accept("token-for-new@example.com"), http.StatusOK)
	if spent := store.PasswordWork() - work; spent != 1 {
		t.Errorf("accepting an invitation spent %d hashes, want one", spent)
	}
	// Spent is spent: the second use is refused before any hash too.
	work = store.PasswordWork()
	expectStatus(t, accept("token-for-new@example.com"), http.StatusForbidden)
	if spent := store.PasswordWork() - work; spent != 0 {
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
	if _, log := h.server.passwordLog.allow(time.Now()); !log {
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
	if answer := decodeJSON[struct {
		Required *bool `json:"required"`
		Enabled  *bool `json:"enabled"`
	}](t, rec); answer.Required == nil || !*answer.Required || answer.Enabled == nil || *answer.Enabled {
		t.Errorf("GET /setup = %s, want required and not enabled", rec.Body.String())
	}
	work := store.PasswordWork()
	expectError(t, h.call(t, "POST", "/api/v1/setup", mustJSON(t, map[string]any{
		"token": "anything", "email": "founder@example.com", "password": testAccountPassword,
	}), anonymous, asJSON), http.StatusForbidden, "TRACEPAD_SETUP=off")
	// Whatever the body says: the answer comes before it is read.
	expectError(t, h.call(t, "POST", "/api/v1/setup", []byte(`{"token": `), anonymous, asJSON),
		http.StatusForbidden, "TRACEPAD_SETUP=off")
	if store.PasswordWork() != work {
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
