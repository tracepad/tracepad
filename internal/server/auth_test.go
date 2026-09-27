package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// Signing in (spec 028, Testing). The properties asserted here are the ones a
// person meets: one sentence for every login failure, a cookie a script cannot
// read, a session that slides rather than expiring under somebody, a
// cross-origin write that is refused, and a header that says which project a
// session is asking about.

const testAccountPassword = "correct horse battery"

// accountHash is one bcrypt hash for the whole test binary, at the cost
// TestMain lowered the binary to — and made there, so that no test is the
// first to ask. The one test that needs a hash at the production cost makes
// its own.
var accountHash = sync.OnceValues(func() ([]byte, error) {
	return store.HashPassword(testAccountPassword)
})

// newAccountHarness is the ordinary harness with the cross-project token
// configured — which the six-caller matrix needs and the rest is unaffected
// by.
func newAccountHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, &config.Config{
		Listen:       ":0",
		StoreRaw:     true,
		MaxBodyBytes: config.DefaultMaxBodyBytes,
		AdminToken:   adminToken,
	}, store.WriterOptions{})
}

// signedIn is an account and the cookie value its session carries.
type signedIn struct {
	account *store.Account
	cookie  string
}

// invited creates an account and leaves it as an invitation does: no password,
// so it is `pending` and cannot sign in.
func (h *harness) invited(t *testing.T, email string, owner bool, memberships ...store.Membership) *store.Account {
	t.Helper()
	now := time.Now().UnixNano()
	create := &store.AccountCreate{
		Email: email, Owner: owner, Memberships: memberships,
		TokenID:   store.SessionID("token-for-" + email),
		ExpiresAt: now + int64(inviteWindow), Now: now,
	}
	if err := h.writer.Submit(t.Context(), create); err != nil {
		t.Fatal(err)
	}
	return create.Account
}

// account creates an account and signs it in, through the same two jobs the
// invitation endpoints use.
func (h *harness) account(t *testing.T, email string, owner bool, memberships ...store.Membership) *signedIn {
	t.Helper()
	hash, err := accountHash()
	if err != nil {
		t.Fatal(err)
	}
	return h.accountWithHash(t, email, owner, hash, memberships...)
}

// accountWithHash is account with the password hash given rather than the
// shared one.
func (h *harness) accountWithHash(t *testing.T, email string, owner bool, hash []byte, memberships ...store.Membership) *signedIn {
	t.Helper()
	h.invited(t, email, owner, memberships...)
	now := time.Now().UnixNano()
	value := "cookie-for-" + email
	accept := &store.InviteAccept{
		SessionSeed: store.SessionSeed{
			SessionID: store.SessionID(value),
			ExpiresAt: now + int64(30*24*time.Hour),
			Now:       now,
		},
		TokenID: store.SessionID("token-for-" + email), NewHash: hash,
	}
	if err := h.writer.Submit(t.Context(), accept); err != nil {
		t.Fatal(err)
	}
	return &signedIn{account: accept.Account, cookie: value}
}

// resume opens another session for an account that already has a password.
// It is what lets a test go on being somebody after a sign-out, and it costs
// one insert and no hashing.
func (h *harness) resume(t *testing.T, who *signedIn, tag string) *signedIn {
	t.Helper()
	now := time.Now().UnixNano()
	value := "cookie-" + tag + "-" + who.account.Email
	if err := h.writer.Submit(t.Context(), &store.SessionOpen{
		SessionSeed: store.SessionSeed{
			SessionID: store.SessionID(value),
			ExpiresAt: now + int64(30*24*time.Hour),
			Now:       now,
		},
		AccountID: who.account.ID,
	}); err != nil {
		t.Fatal(err)
	}
	return &signedIn{account: who.account, cookie: value}
}

// owner, editor and viewer are the three session callers every test here and
// in the policy matrix uses.
func (h *harness) owner(t *testing.T) *signedIn {
	return h.account(t, "owner@example.com", true)
}

func (h *harness) editor(t *testing.T) *signedIn {
	return h.account(t, "editor@example.com", false,
		store.Membership{ProjectID: h.project.ID, Role: store.RoleEditor})
}

func (h *harness) viewer(t *testing.T) *signedIn {
	return h.account(t, "viewer@example.com", false,
		store.Membership{ProjectID: h.project.ID, Role: store.RoleViewer})
}

// asSession sends a request the way the interface does: the cookie instead of
// an Authorization header, and an Origin matching the host, which is what the
// browser attaches for free on a same-origin write (Decision 5).
func asSession(who *signedIn) func(*http.Request) {
	return func(r *http.Request) {
		r.Header.Del("Authorization")
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: who.cookie})
		r.Header.Set("Origin", "http://"+r.Host)
	}
}

// inProject is the header a session names its project with.
func inProject(id string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(projectHeader, id) }
}

// anonymous strips the harness's default credential.
func anonymous(r *http.Request) { r.Header.Del("Authorization") }

// login posts to the login endpoint and hands back the recorder.
func (h *harness) login(t *testing.T, email, password string) *httptest.ResponseRecorder {
	t.Helper()
	return h.call(t, "POST", "/api/v1/auth/login",
		mustJSON(t, map[string]any{"email": email, "password": password}),
		anonymous, func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) }, asJSON)
}

// sessionCookieOf reads the cookie a response set, or "" when it set none.
func sessionCookieOf(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, cookie := range (&http.Response{Header: rec.Header()}).Cookies() {
		if cookie.Name == sessionCookie {
			return cookie
		}
	}
	return nil
}

// TestLoginAnswersOneSentenceToEveryFailure is Decision 8's rule: any
// difference between "no such account", "wrong password", "disabled" and
// "never accepted the invitation" is a way to find out who has an account
// here.
func TestLoginAnswersOneSentenceToEveryFailure(t *testing.T) {
	h := newAccountHarness(t)
	h.owner(t)
	// Invited and not yet accepted: no password, so nothing verifies.
	h.invited(t, "pending@example.com", false)
	disabled := h.account(t, "disabled@example.com", false)
	yes := true
	if err := h.writer.Submit(t.Context(), &store.AccountUpdate{
		AccountID: disabled.account.ID, Disabled: &yes, Now: time.Now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}

	for _, attempt := range []struct{ email, password string }{
		{"nobody@example.com", testAccountPassword},
		{"owner@example.com", "not the password"},
		{"pending@example.com", testAccountPassword},
		{"disabled@example.com", testAccountPassword},
	} {
		rec := h.login(t, attempt.email, attempt.password)
		expectError(t, rec, http.StatusUnauthorized, wrongCredentials)
		if sessionCookieOf(rec) != nil {
			t.Errorf("%s: a failed login set a cookie", attempt.email)
		}
	}

	rec := h.login(t, "owner@example.com", testAccountPassword)
	expectStatus(t, rec, 200)
	body := decodeJSON[struct {
		Account struct {
			Email string `json:"email"`
			Owner bool   `json:"owner"`
		} `json:"account"`
	}](t, rec)
	if body.Account.Email != "owner@example.com" || !body.Account.Owner {
		t.Errorf("account = %+v", body.Account)
	}
}

// TestLoginSpendsTheComparisonWhateverTheAnswer: one sentence for every
// failure is only one answer if it takes one length of time. `bcrypt` at cost
// 12 is a quarter of a second, so an unknown address that skipped it would be
// distinguishable from a real one by a stopwatch — and the throttle counts
// per email, so one attempt is all that needs (Decision 8).
//
// A wall-clock assertion, deliberately loose: what it catches is the
// difference between running the hash and not running it, which is two orders
// of magnitude, not the tens of milliseconds a loaded machine adds. That gap
// only exists at the production cost — at the one TestMain lowered the binary
// to, a comparison is a millisecond and so is the noise — so this test, alone
// in the package, runs at cost 12 and pays for it: about a second, most of the
// suite's remaining runtime.
func TestLoginSpendsTheComparisonWhateverTheAnswer(t *testing.T) {
	lowered := store.SetPasswordCost(store.PasswordCost)
	t.Cleanup(func() { store.SetPasswordCost(lowered) })

	h := newAccountHarness(t)
	hash, err := store.HashPassword(testAccountPassword)
	if err != nil {
		t.Fatal(err)
	}
	h.accountWithHash(t, "owner@example.com", true, hash)
	h.invited(t, "pending@example.com", false)

	// The real comparison, to measure the others against. The decoy is
	// built on first use, so this also pays for that.
	known := timeLogin(t, h, "owner@example.com")

	for _, email := range []string{"nobody@example.com", "pending@example.com"} {
		took := timeLogin(t, h, email)
		if took < known/4 {
			t.Errorf("%s answered in %s against %s for an account that exists; "+
				"the difference is the answer", email, took, known)
		}
	}
}

// timeLogin measures one failed sign-in.
func timeLogin(t *testing.T, h *harness, email string) time.Duration {
	t.Helper()
	start := time.Now()
	expectError(t, h.login(t, email, "not the password"), http.StatusUnauthorized, wrongCredentials)
	return time.Since(start)
}

// TestSessionCookieAttributes: `HttpOnly` is the reason to use a cookie at all
// — the key in localStorage was readable by any script on the page — and
// `Secure` follows the request's actual scheme rather than a flag, because the
// common deployments are plain localhost and a TLS proxy (Decision 4).
func TestSessionCookieAttributes(t *testing.T) {
	h := newAccountHarness(t)
	h.owner(t)

	cookie := sessionCookieOf(h.login(t, "owner@example.com", testAccountPassword))
	if cookie == nil {
		t.Fatal("a successful login set no cookie")
	}
	if !cookie.HttpOnly || cookie.Path != "/" || cookie.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie = %+v, want HttpOnly, Path=/ and SameSite=Lax", cookie)
	}
	if cookie.Secure {
		t.Error("a plain http request must not get a Secure cookie; the browser would drop it")
	}
	if cookie.MaxAge < int((29 * 24 * time.Hour).Seconds()) {
		t.Errorf("Max-Age = %d, want about the configured thirty days", cookie.MaxAge)
	}
	// The value is the credential and never the row id: what is stored is
	// its sha256.
	if _, account, err := h.store.SessionByCookie(context.Background(), cookie.Value, time.Now().UnixNano()); err != nil || account == nil {
		t.Fatalf("the cookie does not resolve to its session: %v", err)
	}

	behindTLS := h.call(t, "POST", "/api/v1/auth/login",
		mustJSON(t, map[string]any{"email": "owner@example.com", "password": testAccountPassword}),
		anonymous, func(r *http.Request) {
			// Behind a TLS proxy the browser's origin is https: an http
			// one would be the downgrade Decision 30 refuses.
			r.Header.Set("Origin", "https://"+r.Host)
			r.Header.Set("X-Forwarded-Proto", "https")
		}, asJSON)
	expectStatus(t, behindTLS, 200)
	if cookie := sessionCookieOf(behindTLS); cookie == nil || !cookie.Secure {
		t.Errorf("cookie = %+v behind a TLS proxy, want Secure", cookie)
	}
}

// TestLoginThrottlesOneEmail: five failures inside fifteen minutes is enough
// to stop a dictionary run without a person ever seeing it (Decision 8).
func TestLoginThrottlesOneEmail(t *testing.T) {
	h := newAccountHarness(t)
	h.owner(t)

	for i := range loginFailureLimit {
		rec := h.login(t, "owner@example.com", "not the password")
		expectError(t, rec, http.StatusUnauthorized, wrongCredentials)
		_ = i
	}
	rec := h.login(t, "owner@example.com", "not the password")
	expectStatus(t, rec, http.StatusTooManyRequests)
	after, err := strconv.Atoi(rec.Header().Get("Retry-After"))
	if err != nil || after < 1 {
		t.Errorf("Retry-After = %q, want the seconds to wait", rec.Header().Get("Retry-After"))
	}
	// Even the right password waits: this counts attempts on an email, and
	// letting the correct one through would be a way to test one.
	expectStatus(t, h.login(t, "owner@example.com", testAccountPassword), http.StatusTooManyRequests)

	// Another email is untouched, so one person's typos never lock out
	// anybody else.
	h.account(t, "other@example.com", true)
	expectStatus(t, h.login(t, "other@example.com", testAccountPassword), 200)
}

// TestSessionSlides: a request seen more than a day after `last_seen_at` moves
// the expiry forward and re-sets the cookie, so "about once a month" is what a
// person who opens this daily is asked for (Decision 4).
func TestSessionSlides(t *testing.T) {
	h := newAccountHarness(t)
	who := h.owner(t)
	id := store.SessionID(who.cookie)

	// Age it by writing a slide as though it had happened two days ago:
	// `last_seen_at` is what the rule reads, and it is the slide that sets
	// it.
	if err := h.writer.Submit(t.Context(), &store.SessionSlide{
		SessionID: id,
		ExpiresAt: time.Now().Add(24 * time.Hour).UnixNano(),
		Now:       time.Now().Add(-2 * sessionSlideAfter).UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	before, _, err := h.store.SessionByCookie(context.Background(), who.cookie, time.Now().UnixNano())
	if err != nil || before == nil {
		t.Fatal("the session should still be live")
	}

	rec := h.call(t, "GET", "/api/v1/auth/me", nil, asSession(who))
	expectStatus(t, rec, 200)
	if sessionCookieOf(rec) == nil {
		t.Error("a slide must re-set the cookie, or the browser's Max-Age stays where it was")
	}
	after, _, _ := h.store.SessionByCookie(context.Background(), who.cookie, time.Now().UnixNano())
	if after.ExpiresAt <= before.ExpiresAt {
		t.Errorf("expires_at did not move: %d -> %d", before.ExpiresAt, after.ExpiresAt)
	}

	// A second request the same day writes nothing more: the once-a-day
	// rule is what keeps the writer out of every read.
	expectStatus(t, h.call(t, "GET", "/api/v1/auth/me", nil, asSession(who)), 200)
	again, _, _ := h.store.SessionByCookie(context.Background(), who.cookie, time.Now().UnixNano())
	if again.ExpiresAt != after.ExpiresAt {
		t.Error("a session slid twice in one day")
	}

	// And a request that is refused does not slide: a page on another
	// origin has no business extending somebody's session.
	if err := h.writer.Submit(t.Context(), &store.SessionSlide{
		SessionID: id,
		ExpiresAt: again.ExpiresAt,
		Now:       time.Now().Add(-2 * sessionSlideAfter).UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	rec = h.call(t, "POST", "/api/v1/auth/logout", nil, asSession(who), func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example")
	})
	expectStatus(t, rec, http.StatusForbidden)
	refused, _, _ := h.store.SessionByCookie(context.Background(), who.cookie, time.Now().UnixNano())
	if refused.ExpiresAt != again.ExpiresAt {
		t.Error("a cross-origin request slid the session it was refused for")
	}
}

// TestExpiredCookieIsRefusedAndCleared: the cookie is dead, so it is cleared
// rather than left to be sent with every request until it expires on its own.
func TestExpiredCookieIsRefusedAndCleared(t *testing.T) {
	h := newAccountHarness(t)
	who := h.owner(t)
	if err := h.writer.Submit(t.Context(), &store.SessionSlide{
		SessionID: store.SessionID(who.cookie),
		ExpiresAt: time.Now().Add(-time.Hour).UnixNano(),
		Now:       time.Now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}

	rec := h.call(t, "GET", "/api/v1/auth/me", nil, asSession(who))
	expectStatus(t, rec, http.StatusUnauthorized)
	cookie := sessionCookieOf(rec)
	if cookie == nil || cookie.MaxAge >= 0 {
		t.Errorf("cookie = %+v, want it expired in the browser too", cookie)
	}
}

// TestCrossOriginWriteIsRefused is Decision 5. `SameSite=Lax` already stops
// the classic cross-site POST, but a login cookie that authorises
// `DELETE /api/v1/projects/{id}` deserves a check the server makes itself.
func TestCrossOriginWriteIsRefused(t *testing.T) {
	h := newAccountHarness(t)
	who := h.owner(t)

	// No Origin and no Referer at all.
	rec := h.call(t, "POST", "/api/v1/auth/logout", nil, func(r *http.Request) {
		r.Header.Del("Authorization")
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: who.cookie})
	})
	expectError(t, rec, http.StatusForbidden, "cross-origin")

	// A foreign Origin.
	rec = h.call(t, "POST", "/api/v1/auth/logout", nil, asSession(who), func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example")
	})
	expectError(t, rec, http.StatusForbidden, "cross-origin")

	// A Referer stands in when there is no Origin, which is what an older
	// browser sends.
	rec = h.call(t, "GET", "/api/v1/auth/me", nil, asSession(who), func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example")
	})
	expectStatus(t, rec, 200) // a GET is not what CSRF is about

	rec = h.call(t, "POST", "/api/v1/auth/logout", nil, func(r *http.Request) {
		r.Header.Del("Authorization")
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: who.cookie})
		r.Header.Set("Referer", "http://"+r.Host+"/settings/account")
	})
	expectStatus(t, rec, http.StatusNoContent)

	// Behind a proxy the browser sends the address the person typed and
	// `Host` is the internal name, so two more hosts are accepted — both
	// the operator's own statement of what the address is (Decision 23).
	// Each case signs out, so each needs a session of its own.
	who = h.resume(t, who, "forwarded")
	behindProxy := func(origin, forwarded string) *httptest.ResponseRecorder {
		return h.call(t, "POST", "/api/v1/auth/logout", nil, func(r *http.Request) {
			r.Header.Del("Authorization")
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: who.cookie})
			r.Header.Set("Origin", origin)
			if forwarded != "" {
				r.Header.Set("X-Forwarded-Host", forwarded)
			}
		})
	}

	// What the proxy says it was asked for. A list when a request crossed
	// more than one, and the first entry is the one the browser used.
	expectStatus(t, behindProxy("https://traces.example.com",
		"traces.example.com, inner.internal"), http.StatusNoContent)

	// What the operator says people type.
	who = h.resume(t, who, "configured")
	h.server.setPublicURL("https://tracepad.example.com")
	expectStatus(t, behindProxy("https://tracepad.example.com", ""), http.StatusNoContent)

	// And neither of those is a way in for anybody else.
	who = h.resume(t, who, "stranger")
	expectError(t, behindProxy("https://evil.example", "traces.example.com"),
		http.StatusForbidden, "cross-origin")
	h.server.setPublicURL("")

	// A Bearer credential is exempt, and the header wins when both are
	// present: an explicit credential beats an ambient one.
	who = h.resume(t, who, "bearer")
	rec = h.call(t, "POST", "/api/v1/scores",
		mustJSON(t, map[string]any{"name": "q", "value": 1, "trace_id": traceHex(1)}),
		func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: who.cookie})
			r.Header.Set("Origin", "https://evil.example")
		})
	if rec.Code == http.StatusForbidden {
		t.Errorf("a Bearer POST was refused for its Origin: %s", rec.Body)
	}
}

// TestProjectHeader is Decision 6: a key carries its project, a session does
// not, so something must.
func TestProjectHeader(t *testing.T) {
	h := newAccountHarness(t)
	other := h.second(t, "other", "tp-sk-other")
	viewer := h.viewer(t)
	h.owner(t)

	// Missing: a 400 that names the header, not a 401 or an empty listing.
	rec := h.call(t, "GET", "/api/v1/traces", nil, asSession(viewer))
	expectError(t, rec, http.StatusBadRequest, projectHeader)

	// A project the account cannot reach: 403, because ids are random and
	// "not a member" is what tells a person what to ask the owner for.
	rec = h.call(t, "GET", "/api/v1/traces", nil, asSession(viewer), inProject(other.ID))
	expectError(t, rec, http.StatusForbidden, "not a member")

	// Their own: through.
	rec = h.call(t, "GET", "/api/v1/traces", nil, asSession(viewer), inProject(h.project.ID))
	expectStatus(t, rec, 200)

	// A key ignores the header — it is its own project (spec 028, edge
	// cases) — so a header naming another project serves the key's own.
	h.seed(t, &model.Trace{ID: traceHex(7), Name: "chat"})
	rec = h.call(t, "GET", "/api/v1/traces", nil, inProject(other.ID))
	expectStatus(t, rec, 200)
	listing := decodeJSON[struct {
		Traces []struct {
			ID string `json:"id"`
		} `json:"traces"`
	}](t, rec)
	if len(listing.Traces) != 1 || listing.Traces[0].ID != traceHex(7) {
		t.Errorf("a key with a foreign project header saw %+v, want its own project's rows", listing.Traces)
	}
}

// TestSessionCannotReachASoftDeletedProject: a deleted project is not there as
// far as a person is concerned (spec 028, edge cases).
func TestSessionCannotReachASoftDeletedProject(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.owner(t)
	if err := h.writer.Submit(t.Context(), &store.ProjectDelete{
		ProjectID: h.project.ID, Confirm: "test", Now: time.Now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}

	rec := h.call(t, "GET", "/api/v1/traces", nil, asSession(owner), inProject(h.project.ID))
	expectError(t, rec, http.StatusNotFound, "no such project")

	// It is off the listing, and an owner asking for it gets it with the
	// purge date — which is the Server tab's table.
	rec = h.call(t, "GET", "/api/v1/projects", nil, asSession(owner))
	expectStatus(t, rec, 200)
	if listed := decodeJSON[struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}](t, rec).Projects; len(listed) != 0 {
		t.Errorf("projects = %+v, want a deleted one off the listing", listed)
	}
	rec = h.call(t, "GET", "/api/v1/projects?include=deleted", nil, asSession(owner))
	expectStatus(t, rec, 200)
	deleted := decodeJSON[struct {
		Projects []struct {
			ID      string  `json:"id"`
			Role    string  `json:"role"`
			PurgeAt *string `json:"purge_at"`
		} `json:"projects"`
	}](t, rec).Projects
	if len(deleted) != 1 || deleted[0].PurgeAt == nil || deleted[0].Role != store.RoleOwner {
		t.Errorf("projects = %+v, want the deleted one with its purge date", deleted)
	}

	// A member cannot ask for that at all: for anyone but an owner the
	// question can only be about somebody else's project.
	viewer := h.account(t, "helper@example.com", false)
	expectError(t, h.call(t, "GET", "/api/v1/projects?include=deleted", nil, asSession(viewer)),
		http.StatusForbidden, "owner account")

	// The two routes that undo it still answer, because an owner about to
	// restore has to be able to see what it is restoring (spec 005 #10).
	rec = h.call(t, "GET", "/api/v1/projects/"+h.project.ID, nil, asSession(owner))
	expectStatus(t, rec, 200)
	rec = h.call(t, "POST", "/api/v1/projects/"+h.project.ID+"/restore", nil, asSession(owner))
	expectStatus(t, rec, 200)
}

// TestMeCarriesTheProjects: the one call the interface makes on load, and the
// project list rides in it because the shell needs both at once (Decision 8).
func TestMeCarriesTheProjects(t *testing.T) {
	h := newAccountHarness(t)
	h.second(t, "other", "tp-sk-other")
	owner := h.owner(t)
	viewer := h.viewer(t)

	type meBody struct {
		Account struct {
			Email string `json:"email"`
			Owner bool   `json:"owner"`
		} `json:"account"`
		Projects []struct {
			Name string `json:"name"`
			Role string `json:"role"`
		} `json:"projects"`
	}

	rec := h.call(t, "GET", "/api/v1/auth/me", nil, asSession(owner))
	expectStatus(t, rec, 200)
	body := decodeJSON[meBody](t, rec)
	if len(body.Projects) != 2 || body.Projects[0].Name != "other" || body.Projects[0].Role != store.RoleOwner {
		t.Errorf("an owner's projects = %+v, want every live project by name with the role owner", body.Projects)
	}

	rec = h.call(t, "GET", "/api/v1/auth/me", nil, asSession(viewer))
	expectStatus(t, rec, 200)
	body = decodeJSON[meBody](t, rec)
	if len(body.Projects) != 1 || body.Projects[0].Name != "test" || body.Projects[0].Role != store.RoleViewer {
		t.Errorf("a viewer's projects = %+v, want its memberships", body.Projects)
	}
	if body.Account.Owner {
		t.Error("a viewer is not an owner")
	}

	// A key and the admin token are not sessions, and are told so rather
	// than "unauthorized": they are perfectly good credentials asking a
	// question they cannot have.
	expectError(t, h.get(t, "/api/v1/auth/me"), http.StatusBadRequest, "not a session")
	expectStatus(t, h.call(t, "GET", "/api/v1/auth/me", nil, anonymous), http.StatusUnauthorized)
}

// TestPasswordChangeEndsTheOtherSessions: changing it is what a person does
// when they think somebody else has it (Decision 4).
func TestPasswordChangeEndsTheOtherSessions(t *testing.T) {
	h := newAccountHarness(t)
	who := h.owner(t)

	// A second session for the same account: sign in again.
	second := sessionCookieOf(h.login(t, "owner@example.com", testAccountPassword))
	if second == nil {
		t.Fatal("the second sign-in set no cookie")
	}

	rec := h.call(t, "GET", "/api/v1/auth/sessions", nil, asSession(who))
	expectStatus(t, rec, 200)
	listed := decodeJSON[struct {
		Sessions []struct {
			ID      string `json:"id"`
			Current bool   `json:"current"`
		} `json:"sessions"`
	}](t, rec)
	if len(listed.Sessions) != 2 {
		t.Fatalf("sessions = %d, want both", len(listed.Sessions))
	}
	current := 0
	for _, one := range listed.Sessions {
		if one.Current {
			current++
			if one.ID != store.SessionID(who.cookie) {
				t.Error("the wrong session is marked current")
			}
		}
	}
	if current != 1 {
		t.Errorf("%d sessions are marked current, want exactly the caller's", current)
	}

	// The current password is what proves it is them — and a body carrying
	// both leaves neither changed when it is wrong, because they are one
	// transaction. Two jobs would have committed the rename and then
	// answered 403.
	rec = h.call(t, "PATCH", "/api/v1/auth/me", mustJSON(t, map[string]any{
		"name":     "Somebody Else",
		"password": map[string]any{"current": "not it", "new": "a brand new password"},
	}), asSession(who))
	expectError(t, rec, http.StatusForbidden, "wrong current password")

	rec = h.call(t, "GET", "/api/v1/auth/me", nil, asSession(who))
	expectStatus(t, rec, 200)
	if name := decodeJSON[struct {
		Account struct {
			Name string `json:"name"`
		} `json:"account"`
	}](t, rec).Account.Name; name != "" {
		t.Errorf("name = %q after a refused password change, want it untouched", name)
	}

	rec = h.call(t, "PATCH", "/api/v1/auth/me", mustJSON(t, map[string]any{
		"name":     "The Founder",
		"password": map[string]any{"current": testAccountPassword, "new": "a brand new password"},
	}), asSession(who))
	expectStatus(t, rec, 200)

	// The caller keeps its session; the other one is gone.
	expectStatus(t, h.call(t, "GET", "/api/v1/auth/me", nil, asSession(who)), 200)
	rec = h.call(t, "GET", "/api/v1/auth/me", nil, func(r *http.Request) {
		r.Header.Del("Authorization")
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: second.Value})
	})
	expectStatus(t, rec, http.StatusUnauthorized)

	// And the new password is the one that works.
	expectError(t, h.login(t, "owner@example.com", testAccountPassword),
		http.StatusUnauthorized, wrongCredentials)
	expectStatus(t, h.login(t, "owner@example.com", "a brand new password"), 200)

	// Too short is a 422 and changes nothing.
	rec = h.call(t, "PATCH", "/api/v1/auth/me", mustJSON(t, map[string]any{
		"password": map[string]any{"current": "a brand new password", "new": "short"},
	}), asSession(who))
	expectStatus(t, rec, http.StatusUnprocessableEntity)
}

// TestSignOutEverywhere keeps the session the person is pressing it with.
func TestSignOutEverywhere(t *testing.T) {
	h := newAccountHarness(t)
	who := h.owner(t)
	other := sessionCookieOf(h.login(t, "owner@example.com", testAccountPassword))

	rec := h.call(t, "DELETE", "/api/v1/auth/sessions", nil, asSession(who))
	expectStatus(t, rec, 200)
	if ended := decodeJSON[struct {
		Ended int `json:"ended"`
	}](t, rec); ended.Ended != 1 {
		t.Errorf("ended = %d, want the one that is not the caller's", ended.Ended)
	}
	expectStatus(t, h.call(t, "GET", "/api/v1/auth/me", nil, asSession(who)), 200)
	expectStatus(t, h.call(t, "GET", "/api/v1/auth/me", nil, func(r *http.Request) {
		r.Header.Del("Authorization")
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: other.Value})
	}), http.StatusUnauthorized)

	// Signing out deletes the row and clears the cookie.
	rec = h.call(t, "POST", "/api/v1/auth/logout", nil, asSession(who))
	expectStatus(t, rec, http.StatusNoContent)
	if cookie := sessionCookieOf(rec); cookie == nil || cookie.MaxAge >= 0 {
		t.Errorf("cookie = %+v after a sign-out, want it cleared", cookie)
	}
	expectStatus(t, h.call(t, "GET", "/api/v1/auth/me", nil, asSession(who)), http.StatusUnauthorized)
}

// TestSetup is Decision 9: the one moment the operator is provably at the
// console is the moment the server prints a link, so the link is the
// credential.
func TestSetup(t *testing.T) {
	h := newAccountHarness(t)

	rec := h.call(t, "GET", "/api/v1/setup", nil, anonymous)
	expectStatus(t, rec, 200)
	if !decodeJSON[struct {
		Required bool `json:"required"`
	}](t, rec).Required {
		t.Fatal("a server with no owner must say it needs setting up")
	}
	if h.server.SetupURL() == "" || !strings.Contains(h.server.SetupURL(), "/setup#token=") {
		t.Fatalf("SetupURL = %q, want a link with the token in the fragment", h.server.SetupURL())
	}
	token := strings.TrimPrefix(h.server.SetupURL(), h.server.originForPrint()+"/setup#token=")

	setup := func(body map[string]any) *httptest.ResponseRecorder {
		return h.call(t, "POST", "/api/v1/setup", mustJSON(t, body), anonymous,
			func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) }, asJSON)
	}

	expectStatus(t, setup(map[string]any{
		"token": "not the token", "email": "founder@example.com", "password": testAccountPassword,
	}), http.StatusForbidden)
	expectStatus(t, setup(map[string]any{
		"token": token, "email": "not an email", "password": testAccountPassword,
	}), http.StatusUnprocessableEntity)
	expectStatus(t, setup(map[string]any{
		"token": token, "email": "founder@example.com", "password": "short",
	}), http.StatusUnprocessableEntity)

	rec = setup(map[string]any{
		"token": token, "email": "founder@example.com",
		"password": testAccountPassword, "name": "The Founder",
	})
	expectStatus(t, rec, http.StatusCreated)
	if sessionCookieOf(rec) == nil {
		t.Error("setup must sign the owner in")
	}
	created := decodeJSON[struct {
		Account struct {
			Email string `json:"email"`
			Name  string `json:"name"`
			Owner bool   `json:"owner"`
		} `json:"account"`
	}](t, rec)
	if !created.Account.Owner || created.Account.Name != "The Founder" {
		t.Errorf("account = %+v", created.Account)
	}

	// It flips, and there is nothing left to replay.
	rec = h.call(t, "GET", "/api/v1/setup", nil, anonymous)
	expectStatus(t, rec, 200)
	if decodeJSON[struct {
		Required bool `json:"required"`
	}](t, rec).Required {
		t.Error("setup must be done once an owner exists")
	}
	expectStatus(t, setup(map[string]any{
		"token": token, "email": "second@example.com", "password": testAccountPassword,
	}), http.StatusForbidden)
	if h.server.SetupURL() != "" {
		t.Error("the setup link must be gone once it has been used")
	}
}

// TestConcurrentSetupMakesOneOwner: two browsers can hold the same link — a
// double click is enough. Exactly one wins, the other is told the server
// already has an owner, and the token is read and cleared under a lock so
// `-race` has nothing to find.
func TestConcurrentSetupMakesOneOwner(t *testing.T) {
	h := newAccountHarness(t)
	token := strings.TrimPrefix(h.server.SetupURL(), h.server.originForPrint()+"/setup#token=")

	const tries = 4
	var wait sync.WaitGroup
	codes := make([]int, tries)
	for i := range tries {
		wait.Add(1)
		go func() {
			defer wait.Done()
			rec := h.call(t, "POST", "/api/v1/setup", mustJSON(t, map[string]any{
				"token": token, "email": "founder@example.com", "password": testAccountPassword,
			}), anonymous, func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) }, asJSON)
			codes[i] = rec.Code
		}()
	}
	wait.Wait()

	created := 0
	for i, code := range codes {
		switch code {
		case http.StatusCreated:
			created++
		case http.StatusForbidden:
		default:
			t.Errorf("attempt %d = %d, want 201 or 403", i, code)
		}
	}
	if created != 1 {
		t.Errorf("%d of %d attempts created an owner, want exactly one", created, tries)
	}
	owners, err := h.store.EnabledOwners(t.Context())
	if err != nil || owners != 1 {
		t.Errorf("owners = %d, err = %v", owners, err)
	}
}

// TestSetupLinkHost is Decision 11: the operator's TRACEPAD_URL wins over a
// guess, because behind a proxy the listen address is not an address anybody
// can use.
func TestSetupLinkHost(t *testing.T) {
	h := newAccountHarness(t)
	if got := h.server.SetupURL(); !strings.HasPrefix(got, "http://localhost:") &&
		!strings.HasPrefix(got, "http://") {
		t.Errorf("SetupURL = %q, want the listen address", got)
	}

	h.server.setPublicURL("https://traces.example.com/")
	if got := h.server.SetupURL(); !strings.HasPrefix(got, "https://traces.example.com/setup#token=") {
		t.Errorf("SetupURL = %q, want TRACEPAD_URL to win", got)
	}

	// A value that will not parse is ignored rather than fatal: it is the
	// CLI's "which server" as well.
	h.server.setPublicURL("not a url")
	if got := h.server.SetupURL(); strings.Contains(got, "not a url") {
		t.Errorf("SetupURL = %q, want the guess when TRACEPAD_URL is not a URL", got)
	}
}

// asJSON declares the body JSON, which the three public routes require of a
// caller (spec 028 Decision 29) and no other route does (spec 003 #19).
func asJSON(r *http.Request) { r.Header.Set("Content-Type", "application/json") }
