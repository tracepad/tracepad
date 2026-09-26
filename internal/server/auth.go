package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

/*
One door (spec 028 Decision 7).

Until this spec there were three ways in and each knew a different part of the
truth: `authenticate` in otlp.go resolved a key for ingest, `authorize` in
admin.go resolved a key or the admin token for the project routes, and
`apiProject` wrapped the first for the read API. A permission matrix spread
over three functions and sixty handlers is a matrix nobody can check.

Now the route table carries a **policy** and one guard reads it. The guard runs
before every handler, resolves who is asking, decides whether the policy admits
them, works out which project the request is about, and puts all of it on the
request's context. A handler asks the context and never the headers.

The order is fixed, because it is what the six-caller test asserts status by
status:

 1. the credential — an `Authorization` header beats a cookie, always
    (Decision 5), and no credential at all is `401`;
 2. cross-origin, for a cookie on an unsafe method (Decision 5);
 3. the policy against the kind of caller;
 4. the project the request is about, for a session (Decision 6).
*/

// policy is what a route requires of its caller. It is the whole of the
// permission matrix (Decision 3): a route's policy is checked here and
// nowhere else, so the matrix has one place to be wrong in.
type policy uint8

const (
	// unset is the zero value, which is what a route that declares no
	// policy has. It admits nobody, and the parity test fails for any route
	// that carries it: a permission that defaults is a permission nobody
	// chose.
	unset policy = iota
	// public needs no credential: the endpoint map, the OpenAPI document,
	// health, and the three ways in (setup, login, accept an invitation).
	public
	// ingest is a project key and nothing else. A person's session never
	// writes spans, and the admin token never touches the data plane.
	ingest
	// member is a project key, or a session whose account is an owner or
	// has any role in the project: every data-plane read, plus the
	// annotation writes, because annotating is a viewer's job.
	member
	// editor is a project key, or an owner or `editor` session: the things
	// that change what a project is made of.
	editor
	// owner is the admin token or an owner session: the projects
	// themselves and the accounts.
	owner
	// presigned is public to every caller — nothing in the headers is
	// asked for — but the credential is a signed token in the URL that the
	// handler checks before it reads a byte: the Langfuse media upload
	// (spec 041 #14). It is its own policy so that the table says, rather
	// than a path comparison somewhere, that this route keeps its own body
	// rules instead of the small plain body every other public route gets
	// (spec 028 Decision 26). The endpoint map calls it `public`, because
	// that is what it is to a caller.
	presigned
	// session is a route only a cookie reaches — the six of `/api/v1/auth`
	// that are about the person signed in. A key or the admin token is
	// told "not a session" rather than "unauthorized", because it is a
	// perfectly good credential asking a question it cannot have.
	session
)

// String names a policy for the endpoint map and for test failures.
func (p policy) String() string {
	switch p {
	case public, presigned:
		return "public"
	case ingest:
		return "ingest"
	case member:
		return "member"
	case editor:
		return "editor"
	case owner:
		return "owner"
	case session:
		return "session"
	}
	return "unset"
}

// caller is who is asking. Exactly one of the three credentials is set.
type caller struct {
	// admin is the deployment's own token (spec 005 #11).
	admin bool
	// project is the key's project, or — for a session — the project this
	// request is about, resolved from the path or the header (Decision 6).
	project *store.Project
	// account and session are set together, for a browser cookie.
	account *store.Account
	session *store.AccountSession
	// role is what the account may do in `project`: `owner`, `editor` or
	// `viewer`. Empty for a key and for the admin token, whose reach the
	// policy already settled.
	role string
}

// isSession reports a cookie-authenticated caller.
func (c *caller) isSession() bool { return c != nil && c.account != nil }

// isKey reports a project key.
func (c *caller) isKey() bool { return c != nil && !c.admin && c.account == nil && c.project != nil }

// callerKey is the context key the guard stores the caller under.
type callerKey struct{}

func withCaller(ctx context.Context, c *caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// callerFrom reads back what the guard resolved. A handler reached through the
// mux always has one; the nil answer is for a handler called directly in a
// test, which is a programming error the caller sites turn into a 500 rather
// than a panic.
func callerFrom(ctx context.Context) *caller {
	c, _ := ctx.Value(callerKey{}).(*caller)
	return c
}

// The session cookie (Decision 4).
const (
	sessionCookie = "tracepad_session"
	// sessionSlideAfter is how stale `last_seen_at` may get before a
	// request moves the expiry forward. A day, so that the writer stays
	// out of every read and a person who opens this daily never signs in
	// again.
	sessionSlideAfter = 24 * time.Hour
	// inviteWindow is how long an invitation or a reset link is good for
	// (Decision 10).
	inviteWindow = 7 * 24 * time.Hour
)

// guard wraps one route's handler with the policy the table declares.
func (s *Server) guard(rt route) http.HandlerFunc {
	if rt.Policy == unset {
		// The parity test is what keeps this from ever shipping; this is
		// what happens if it somehow does. Refusing is the only safe
		// reading of "nobody said who may call this".
		return func(w http.ResponseWriter, r *http.Request) {
			slog.Error("route has no policy", "method", rt.Method, "path", rt.Path)
			writeError(w, http.StatusInternalServerError, "this route declares no policy")
		}
	}
	if rt.Policy == presigned {
		return rt.handler
	}
	if rt.Policy == public {
		if rt.Method == http.MethodGet || rt.Method == http.MethodHead {
			return rt.handler
		}
		return smallPlainBody(rt.handler)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := s.resolve(w, r, rt)
		if !ok {
			return
		}
		rt.handler(w, r.WithContext(withCaller(r.Context(), c)))
	}
}

// resolve is steps one to four above. It answers the client itself on every
// refusal, so a handler that runs is a handler whose caller is allowed.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request, rt route) (*caller, bool) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "the API is not available")
		return nil, false
	}
	c, ok := s.identify(w, r)
	if !ok {
		return nil, false
	}
	if c.isSession() && !s.sameOrigin(r) {
		// SameSite=Lax already stops the classic cross-site POST, but a
		// cookie that authorises `DELETE /api/v1/projects/{id}` deserves
		// a check the server makes itself (Decision 5).
		writeError(w, http.StatusForbidden, "cross-origin request refused")
		return nil, false
	}
	if !s.admits(w, rt, c) {
		return nil, false
	}
	if !s.scope(w, r, rt, c) {
		return nil, false
	}
	// Last, so that only a request that is actually served extends the
	// session it was made with. A refused one — wrong origin, wrong role —
	// has no business writing anything.
	if c.isSession() {
		s.slide(w, r, c.session)
	}
	return c, true
}

// credentialDeadline bounds each lookup the guard makes (spec 043 #1): a
// lookup that waits for a connection or a lock is a credential the server
// could not check, and it says so rather than holding the request.
const credentialDeadline = 5 * time.Second

// cannotCheck is the answer for a credential the server could not check
// (spec 043 #1). It is never `401`: an exporter treats `401` as final and drops
// the batch, and its operator starts checking a key that was never wrong.
// `503` with `Retry-After` is what an exporter retries, and it is the truth.
func cannotCheck(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	writeError(w, http.StatusServiceUnavailable, "cannot check credentials right now; retry shortly")
}

// identify resolves the credential. An `Authorization` header wins over a
// cookie when both are present: an explicit credential beats an ambient one,
// which is what keeps a command-line tool's behaviour untouched next to a
// browser (Decision 5).
func (s *Server) identify(w http.ResponseWriter, r *http.Request) (*caller, bool) {
	if header := r.Header.Get("Authorization"); header != "" {
		secret, ok := credential(header)
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return nil, false
		}
		// Constant time, because this one compares a whole shared secret
		// rather than looking a hash up in an index.
		if s.adminToken != "" &&
			subtle.ConstantTimeCompare([]byte(secret), []byte(s.adminToken)) == 1 {
			return &caller{admin: true}, true
		}
		ctx, cancel := context.WithTimeout(r.Context(), credentialDeadline)
		project, err := s.store.ProjectBySecret(ctx, secret)
		cancel()
		if err != nil {
			slog.Error("key lookup failed", "err", err)
			cannotCheck(w)
			return nil, false
		}
		if project == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return nil, false
		}
		return &caller{project: project}, true
	}

	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	now := time.Now().UnixNano()
	ctx, cancel := context.WithTimeout(r.Context(), credentialDeadline)
	found, account, err := s.store.SessionByCookie(ctx, cookie.Value, now)
	cancel()
	if err != nil {
		// Not a sign-out: the cookie was never judged, so it stays.
		slog.Error("session lookup failed", "err", err)
		cannotCheck(w)
		return nil, false
	}
	if found == nil {
		// Expired, signed out elsewhere, or the account is gone: the
		// cookie is dead, so it is cleared rather than left to be sent
		// with every request until it expires on its own.
		s.clearSessionCookie(w, r)
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	return &caller{account: account, session: found}, true
}

// slide moves a session's expiry forward when it has not been seen for a day,
// and re-sets the cookie so the browser's copy agrees with the row
// (Decision 4). More often than that would put a write in front of every read
// for no gain a person could notice.
func (s *Server) slide(w http.ResponseWriter, r *http.Request, current *store.AccountSession) {
	now := time.Now()
	if s.writer == nil || now.UnixNano()-current.LastSeenAt < int64(sessionSlideAfter) {
		return
	}
	expires := now.Add(s.sessionLife)
	if err := s.writer.Submit(r.Context(), &store.SessionSlide{
		SessionID: current.ID, ExpiresAt: expires.UnixNano(), Now: now.UnixNano(),
	}); err != nil {
		// A slide that could not be written is not a reason to refuse the
		// request: the session is live either way, and the next request
		// tries again.
		slog.Warn("could not slide a session", "err", err)
		return
	}
	current.ExpiresAt = expires.UnixNano()
	current.LastSeenAt = now.UnixNano()
	http.SetCookie(w, s.sessionCookieFor(r, cookieValueUnchanged, expires))
}

// admits is step three: the policy against the kind of caller (Decision 3).
func (s *Server) admits(w http.ResponseWriter, rt route, c *caller) bool {
	switch rt.Policy {
	case ingest:
		if !c.isKey() {
			// A session and the admin token are both "unauthorized"
			// here rather than "forbidden": ingest is the one surface
			// where a credential that is not a project key is not a
			// credential at all.
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return false
		}
		return true

	case session:
		if !c.isSession() {
			writeError(w, http.StatusBadRequest, "not a session")
			return false
		}
		return true

	case owner:
		if c.admin {
			return true
		}
		if c.isSession() {
			if c.account.Owner {
				return true
			}
			writeError(w, http.StatusForbidden, "this needs an owner account")
			return false
		}
		return s.requireAdmin(w, c)

	case member, editor:
		if c.isKey() {
			// A project key is the administrator of its own project
			// (spec 005 #11), which is both of these.
			return true
		}
		if c.admin {
			// The admin token keeps exactly the powers spec 005 #11
			// gave it — the project's own administration — and still
			// reaches no data-plane route (Decision 3).
			if projectRoute(rt.Path) {
				return true
			}
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return false
		}
		return true

	default:
		return true
	}
}

// scope is step four: which project the request is about, and what the account
// may do there (Decision 6).
//
// A key carries its project and a header cannot move it. The admin token has
// no project. Only a session needs this, and it takes the project from the
// path on the routes under `/api/v1/projects` and from `X-Tracepad-Project`
// everywhere else.
func (s *Server) scope(w http.ResponseWriter, r *http.Request, rt route, c *caller) bool {
	if c.isKey() {
		// A soft-deleted project's key still reaches the two endpoints
		// that undo the deletion, and nothing else (spec 005 #10). The
		// project routes make that distinction themselves, in `target`,
		// where the reach of each one is known.
		if c.project.Deleted() && !projectRoute(rt.Path) {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return false
		}
		return true
	}
	if !c.isSession() {
		return true
	}

	var id string
	switch {
	case pathScoped(rt.Path):
		id = r.PathValue("id")
	case projectRoute(rt.Path):
		// The listing and the create are the two routes under
		// `/api/v1/projects` that are about no single project, so there
		// is nothing to scope and no header to ask for.
		return true
	case rt.Policy == member || rt.Policy == editor:
		id = r.Header.Get(projectHeader)
		if id == "" {
			writeError(w, http.StatusBadRequest,
				"a session must name the project: "+projectHeader)
			return false
		}
	default:
		// A `public` or `owner` route has no project, and the header is
		// ignored there (spec 028, edge cases).
		return true
	}

	ctx, cancel := context.WithTimeout(r.Context(), credentialDeadline)
	project, err := s.store.ProjectByIDContext(ctx, id)
	cancel()
	if err != nil {
		slog.Error("project lookup failed", "err", err)
		cannotCheck(w)
		return false
	}
	ctx, cancel = context.WithTimeout(r.Context(), credentialDeadline)
	role, err := s.store.ProjectRole(ctx, c.account, id)
	cancel()
	if err != nil {
		slog.Error("membership lookup failed", "err", err)
		cannotCheck(w)
		return false
	}
	if role == "" {
		// 403 rather than 404 for a project you are not in: ids are
		// random, so there is nothing to enumerate, and "not a member" is
		// the message that tells a person what to ask the owner for.
		writeError(w, http.StatusForbidden, "not a member of this project")
		return false
	}
	if project == nil {
		writeError(w, http.StatusNotFound, "no such project")
		return false
	}
	// A soft-deleted project is gone as far as the data plane is concerned.
	// On the project routes the answer depends on which one it is, so it is
	// `target` that decides (spec 005 #10).
	if project.Deleted() && !pathScoped(rt.Path) {
		writeError(w, http.StatusNotFound, "no such project")
		return false
	}
	if rt.Policy == editor && role == store.RoleViewer {
		writeError(w, http.StatusForbidden, "your role in this project is viewer")
		return false
	}
	c.project = project
	c.role = role
	return true
}

// projectHeader is how a session names the project it is asking about
// (Decision 6). A header rather than a query parameter, because `?project=`
// would ride on every listing URL beside the filters and be copied into shared
// links.
const projectHeader = "X-Tracepad-Project"

// projectRoutePrefix is the administration surface a project key and the admin
// token reach (spec 005 #11).
const projectRoutePrefix = "/api/v1/projects"

// projectRoute reports a route under `/api/v1/projects`: the listing, the
// create, and everything about one project.
func projectRoute(path string) bool {
	return path == projectRoutePrefix || strings.HasPrefix(path, projectRoutePrefix+"/")
}

// pathScoped reports a route that names its project in the path, which is
// every route under `/api/v1/projects` but the listing and the create — and
// those two are about no single project, so a session needs no header on them
// either (Decision 6).
func pathScoped(path string) bool {
	return strings.HasPrefix(path, projectRoutePrefix+"/{id}")
}

// sameOrigin is the cross-site check of Decision 5: a cookie-authenticated
// request with an unsafe method must carry an `Origin` — or, failing that, a
// `Referer` — whose host is one this server answers to.
//
// Which hosts those are is Decision 23. `Host` alone is right for a direct
// connection and wrong behind a proxy that rewrites it: the browser sends the
// address the person typed, the server compares it to the internal name it was
// reached by, and every cookie write answers 403 while a project key goes on
// working — which reads as a broken interface rather than a misconfiguration.
// So two more are accepted, and both are the operator's own statement of what
// the address is: what the proxy says it was asked for, and what
// `TRACEPAD_URL` says people type — the value this spec already trusts for the
// links it prints (Decision 11).
func (s *Server) sameOrigin(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	stated := r.Header.Get("Origin")
	if stated == "" || stated == "null" {
		stated = r.Header.Get("Referer")
	}
	if stated == "" {
		return false
	}
	parsed, err := url.Parse(stated)
	if err != nil || parsed.Host == "" {
		return false
	}
	for _, host := range s.ownHosts(r) {
		if host != "" && strings.EqualFold(parsed.Host, host) {
			return true
		}
	}
	return false
}

// ownHosts is every host this request could legitimately have been addressed
// to (Decision 23).
func (s *Server) ownHosts(r *http.Request) [3]string {
	// Only the first value of `X-Forwarded-For`'s sibling: the header is a
	// list when requests cross more than one proxy, and the first entry is
	// the one the browser was talking to.
	forwarded, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ",")

	configured := ""
	if origin := s.configuredOrigin(); origin != "" {
		if parsed, err := url.Parse(origin); err == nil {
			configured = parsed.Host
		}
	}
	return [3]string{r.Host, strings.TrimSpace(forwarded), configured}
}

// --- The cookie -------------------------------------------------------------

// cookieValueUnchanged re-sets a cookie's attributes without knowing its
// value: the browser is sending the value already, and the server never has to
// read it back out of the row to extend it.
const cookieValueUnchanged = ""

// sessionCookieFor builds the cookie, with `Secure` following the request's
// actual scheme rather than a flag (Decision 4): the common deployments are
// plain localhost and a TLS proxy, and a flag defaults wrong for one of them.
//
// A value of cookieValueUnchanged re-sets the attributes of the cookie the
// browser already holds, which is how a slide extends `Max-Age`.
func (s *Server) sessionCookieFor(r *http.Request, value string, expires time.Time) *http.Cookie {
	cookie := &http.Cookie{
		Name:     sessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   overTLS(r),
		MaxAge:   int(time.Until(expires).Seconds()),
	}
	if value == cookieValueUnchanged {
		// Without a value the browser would store an empty cookie, so
		// the caller that has one passes it; this is only reached from
		// the slide, which reads the request's own.
		if sent, err := r.Cookie(sessionCookie); err == nil {
			cookie.Value = sent.Value
		}
	}
	return cookie
}

// clearSessionCookie expires the cookie in the browser.
func (s *Server) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   overTLS(r),
		MaxAge:   -1,
	})
}

// overTLS reports whether the person is on https, directly or through a proxy
// that says so.
func overTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// newSessionValue mints the 32 random bytes a cookie carries. The value is in
// the cookie and nowhere else: the row is keyed by its sha256.
func newSessionValue() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// clientIP is what the session list shows beside a user agent. Behind a proxy
// this is the proxy unless it forwards; the first hop of `X-Forwarded-For` is
// the best guess there is, and it is shown to a person deciding whether they
// recognise a session rather than used for any decision of ours.
func clientIP(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		first, _, _ := strings.Cut(forwarded, ",")
		if first = strings.TrimSpace(first); first != "" {
			return first
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

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
	// loginTrackedEmails bounds the map. A run against thousands of
	// invented addresses must not be a way to spend the server's memory;
	// past the bound the oldest-touched entries are dropped, which throttles
	// nothing that was not already being throttled.
	loginTrackedEmails = 4096
)

type loginLimiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{failures: map[string][]time.Time{}}
}

// retryAfter reports how long an email must wait, or zero when it may try.
func (l *loginLimiter) retryAfter(email string, now time.Time) time.Duration {
	key := strings.ToLower(strings.TrimSpace(email))
	l.mu.Lock()
	defer l.mu.Unlock()
	recent := l.recent(key, now)
	if len(recent) < loginFailureLimit {
		return 0
	}
	// The oldest failure still inside the window is what has to age out.
	wait := loginFailureWindow - now.Sub(recent[0])
	if wait < time.Second {
		wait = time.Second
	}
	return wait
}

// failed records one wrong answer.
func (l *loginLimiter) failed(email string, now time.Time) {
	key := strings.ToLower(strings.TrimSpace(email))
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures[key] = append(l.recent(key, now), now)
	if len(l.failures) > loginTrackedEmails {
		l.prune(now)
	}
}

// succeeded forgets an email's failures: a person who mistyped twice and then
// got it right is not somebody to throttle.
func (l *loginLimiter) succeeded(email string) {
	key := strings.ToLower(strings.TrimSpace(email))
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, key)
}

// recent is one email's failures still inside the window. The caller holds the
// lock.
func (l *loginLimiter) recent(key string, now time.Time) []time.Time {
	kept := l.failures[key][:0:0]
	for _, at := range l.failures[key] {
		if now.Sub(at) < loginFailureWindow {
			kept = append(kept, at)
		}
	}
	if len(kept) == 0 {
		delete(l.failures, key)
		return nil
	}
	l.failures[key] = kept
	return kept
}

// prune drops every email whose failures have all aged out. The caller holds
// the lock.
func (l *loginLimiter) prune(now time.Time) {
	for key := range l.failures {
		l.recent(key, now)
	}
	// Still over the bound after aging: the map is being filled faster than
	// the window empties it, so it is emptied. Everyone gets their five
	// tries back, which is the failure mode to prefer over unbounded
	// memory.
	if len(l.failures) > loginTrackedEmails {
		clear(l.failures)
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
