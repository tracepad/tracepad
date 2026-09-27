package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/tracepad/tracepad/internal/logpace"
	"github.com/tracepad/tracepad/internal/mcpserver"
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
    (Decision 5), and no credential at all is `401`; a key's use is
    recorded here, admitted or refused (spec 045 #9);
 2. cross-origin, for a cookie on an unsafe method (Decision 5);
 3. the policy against the kind of caller;
 4. the project the request is about, for a session (Decision 6), and
    whether a key's project is still there;
 5. the scope a key must hold (spec 045 #13) — which is where a key is told
    it cannot manage keys (#4), or which scope it lacks (#7). Last, so that a
    soft-deleted project's key keeps the answers it had before scopes
    existed, and a session or the admin token never meets a scope at all.
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
	// stream is a project key and nothing else, answered with no write
	// deadline: the MCP transport, whose response lasts as long as the tool
	// it runs (spec 001 #15). Every refusal is `401` with a Bearer
	// challenge, so a client that follows the MCP authorization spec asks
	// for a key rather than going looking for OAuth. It is not an endpoint
	// of this API, so neither the endpoint map nor the OpenAPI document
	// lists it (spec 004 #27); it is in the guard's table all the same, so
	// that one guard decides its headers, its caller and its scope.
	stream
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
	case stream:
		return "stream"
	}
	return "unset"
}

// scope is what a route asks of a project key (spec 045 #2): one of the three
// things a key may be minted to do, or every key, or none. Sessions and the
// admin token never consult it — the policy and the role decide for them.
type scope uint8

const (
	// scopeUnset is the zero value. It admits no key, and the parity test
	// fails for any route that carries it, as it does for an unset policy.
	scopeUnset scope = iota
	// scopeAny admits every key: the public routes, the project listing and
	// read — how a key learns what it holds (#12) — and fetching one prompt,
	// which an application does at run time with the key it has (#3).
	scopeAny
	// scopeIngest is what a running application does: send spans, use the
	// Langfuse media channel, write scores (#1, #3).
	scopeIngest
	// scopeRead is every read of the project's data, and nothing that
	// changes it.
	scopeRead
	// scopeWrite is every change a key may make to the project.
	scopeWrite
	// scopeNone admits no key, whatever its scopes: the session and owner
	// routes, which the policy already closes to a key, and the three that
	// list, mint and revoke keys, which only this closes (#4).
	scopeNone
)

// String names a scope for the endpoint map, the OpenAPI document, the
// refusal and test failures. The three a key can hold are spelled as a key's
// `scopes` spells them.
func (s scope) String() string {
	switch s {
	case scopeAny:
		return "any"
	case scopeIngest:
		return "ingest"
	case scopeRead:
		return "read"
	case scopeWrite:
		return "write"
	case scopeNone:
		return "none"
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
	// key is the project key's own row, for a key: which key is asking and
	// what it may do. A handler that names the key — the media upload grant
	// of spec 041 #28 — reads its public key here, and the scope step of
	// spec 045 #13 reads its scopes.
	key *store.KeyInfo
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
func (c *caller) isKey() bool { return c != nil && c.key != nil }

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
		return s.publicBody(rt.handler)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		// Before the caller is known, so a refusal carries them too.
		callerHeaders(w)
		refusals := w
		if rt.Policy == stream {
			refusals = challenging{w}
		}
		c, ok := s.resolve(refusals, r, rt)
		if !ok {
			return
		}
		rt.handler(w, r.WithContext(withCaller(r.Context(), c)))
	}
}

// challenging answers every `401` the guard gives on a stream route with a
// Bearer challenge (policy `stream`). Only a `401`: a `503` for a store that
// cannot answer says nothing about the key, and a client that met a challenge
// there would go looking for another credential over a busy database (spec
// 043 #1); a scope refusal carries its own.
type challenging struct{ http.ResponseWriter }

func (c challenging) WriteHeader(status int) {
	if status == http.StatusUnauthorized {
		c.Header().Set("WWW-Authenticate", `Bearer realm="tracepad"`)
	}
	c.ResponseWriter.WriteHeader(status)
}

// callerCacheControl is what every response on a route that needs a caller
// says about caching, unless its handler says otherwise (spec 001 #17): it is
// one caller's, and nothing keeps it. A shared cache in front of the server —
// a proxy, a CDN told to cache everything — must not hand one project's
// traces to the next caller who asks for the same URL, and a cookie, unlike
// `Authorization`, does not stop such a cache from storing. The two reads
// that may be kept say so themselves: prompts for a minute (#26), media for
// a year (spec 041 #7). `Vary` (credentialVary) names the credential, for a
// cache that keeps private copies all the same.
const callerCacheControl = "private, no-store"

// callerHeaders sends what every response on a route that needs a caller says
// about caching.
func callerHeaders(w http.ResponseWriter) {
	header := w.Header()
	header.Set("Cache-Control", callerCacheControl)
	header.Set("Vary", credentialVary)
}

// headerCaller is who an `Authorization` header names — the admin token or a
// project key — or nil when it names nobody. It is the one reading of that
// header. An error is the store failing to answer, which is not the key being
// wrong: the guard runs it through guardLookup, which answers that `503` with
// `Retry-After` and no challenge (spec 043 #1), so no client goes looking for
// another credential over a busy database.
//
// A soft-deleted project's key still names its project here; what it may
// reach is the guard's project step's to decide. A key it finds is marked as
// used (spec 045 #9).
func (s *Server) headerCaller(ctx context.Context, header string) (*caller, error) {
	secret, ok := credential(header)
	if !ok {
		return nil, nil
	}
	// Constant time, because this one compares a whole shared secret rather
	// than looking a hash up in an index.
	if s.adminToken != "" &&
		subtle.ConstantTimeCompare([]byte(secret), []byte(s.adminToken)) == 1 {
		return &caller{admin: true}, nil
	}
	project, key, err := keyBySecret(s.store, ctx, secret)
	if err != nil {
		return nil, err
	}
	if project == nil {
		return nil, nil
	}
	// Here rather than once the request is admitted: the question last use
	// answers is whether anybody still holds the key, and a lost key
	// probing what it cannot reach is exactly that (spec 045 #9).
	s.keyUses.touch(key.PublicKey, time.Now().UnixNano())
	return &caller{project: project, key: key}, nil
}

// resolve is steps one to five above. It answers the client itself on every
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
		s.refuseOrigin(w, r)
		return nil, false
	}
	if !s.admits(w, rt, c) {
		return nil, false
	}
	if !s.inProject(w, r, rt, c) {
		return nil, false
	}
	if !permits(w, r, rt, c) {
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
// could not check, and it says so rather than holding the request. A variable
// for the test that runs one out.
var credentialDeadline = 5 * time.Second

// guardLookup runs one of the guard's lookups under a deadline of its own
// (spec 043 #1) and reports whether the request may go on. A lookup that
// failed because the client hung up answers nothing and logs nothing — there
// is no storage failure and nobody to tell, as ingest already treats a hang-up
// (#24). Any other failure is a credential the server could not check, which is
// never `401`: an exporter treats `401` as final and drops the batch, and its
// operator starts checking a key that was never wrong. `503` with
// `Retry-After` is what an exporter retries, and it is the truth; a cookie
// behind it was never judged, so it is not cleared.
func guardLookup[T any](w http.ResponseWriter, r *http.Request, what string, lookup func(context.Context) (T, error)) (T, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), credentialDeadline)
	defer cancel()
	value, err := lookup(ctx)
	if err == nil {
		return value, true
	}
	var zero T
	if hungUp(r) {
		return zero, false
	}
	// A lookup that ran out of its deadline is the same news as a condition:
	// whatever held it holds the next one too (spec 043 #24 u). A condition
	// the driver reported as the deadline passed keeps its own name.
	condition, ok := store.Condition(err)
	if !ok && ctx.Err() != nil {
		condition = "deadline"
	}
	logLookupFailure(what, err, condition)
	retryLater(w, "cannot check credentials right now; retry shortly")
	return zero, false
}

// lookupFailed answers a handler's own lookup that failed, the way the guard
// answers its lookups (spec 043 #24 u): a client that hung up gets nothing
// and nothing is logged; a database condition is `503` with `Retry-After`,
// logged once a minute per condition; anything else is `500`.
func lookupFailed(w http.ResponseWriter, r *http.Request, what string, err error) {
	answerFailedRead(w, r, what+" lookup failed", "failed to read the "+what, err)
}

// readFailed answers a read of the store that failed, the way lookupFailed
// answers a lookup: a request whose context ended — its client hung up, or the
// read deadline passed (spec 043 #15) — is not answered here, because the read
// gate answers the deadline and nobody is left to hear the other; a database
// condition is `503` with `Retry-After`, logged once a minute per condition;
// anything else is `500` with the message, logged.
func readFailed(w http.ResponseWriter, r *http.Request, message string, err error) {
	answerFailedRead(w, r, message, message, err)
}

// answerFailedRead is the one mapping from a failed read to its answer
// (spec 043 #24 u, #26): logged as line, answered `500` with message unless
// the failure was a database condition or the request's context ended.
func answerFailedRead(w http.ResponseWriter, r *http.Request, line, message string, err error) {
	if hungUp(r) {
		return
	}
	condition, ok := store.Condition(err)
	logReadFailure(line, err, condition)
	if ok {
		retryLater(w, storageUnavailable)
		return
	}
	writeError(w, http.StatusInternalServerError, message)
}

// readAfterWriteFailed answers a read that failed after this request's write
// committed: `500`, logged, whatever the cause — a `503` with `Retry-After`
// would ask the client to repeat a write that is already done (spec 043 #28).
// A client that hung up is answered by nobody.
func readAfterWriteFailed(w http.ResponseWriter, r *http.Request, message string, err error) {
	if hungUp(r) {
		return
	}
	slog.Error(message+" after the write committed", "err", err)
	writeError(w, http.StatusInternalServerError, message)
}

// logLookupFailure logs a lookup that failed. One that failed for a reason
// shared by every request until it passes — a lock held past the busy
// timeout, a full disk, a deadline — is logged once a minute per reason, as
// the writer logs a condition (spec 043 #24); anything else every time.
func logLookupFailure(what string, err error, condition string) {
	logReadFailure(what+" lookup failed", err, condition)
}

// logReadFailure logs a failed read under line, paced by condition as
// logLookupFailure says.
func logReadFailure(line string, err error, condition string) {
	if condition == "" {
		slog.Error(line, "err", err)
		return
	}
	if failed, now := lookupLog.Allow(condition, time.Now()); now {
		slog.Error(line, "err", err, "condition", condition, "failed_since_last_line", failed)
	}
}

// lookupLog paces the log line of a lookup a database condition failed.
var lookupLog = &logpace.Keyed{Every: time.Minute}

// projectByID is the store's, a seam for the test that makes it fail.
var projectByID = (*store.Store).ProjectByID

// keyBySecret is the store's, a seam for the test that counts the lookups an
// MCP tool call makes.
var keyBySecret = (*store.Store).KeyBySecret

// hungUp reports a request whose client is gone. A lookup that failed for that
// reason is no storage failure and has nobody to answer: nothing is logged and
// nothing is written, as ingest already treats a hang-up (spec 043 #24).
func hungUp(r *http.Request) bool {
	return r.Context().Err() != nil
}

// membership is what the scoping lookup finds: the project, and the account's
// role in it.
type membership struct {
	project *store.Project
	role    string
}

// signIn is what a session lookup finds: the row and the account behind it.
type signIn struct {
	session *store.AccountSession
	account *store.Account
}

// identify resolves the credential. An `Authorization` header wins over a
// cookie when both are present: an explicit credential beats an ambient one,
// which is what keeps a command-line tool's behaviour untouched next to a
// browser (Decision 5).
func (s *Server) identify(w http.ResponseWriter, r *http.Request) (*caller, bool) {
	if c := loopbackCaller(r); c != nil {
		return c, true
	}
	if header := r.Header.Get("Authorization"); header != "" {
		c, ok := guardLookup(w, r, "key", func(ctx context.Context) (*caller, error) {
			return s.headerCaller(ctx, header)
		})
		if !ok {
			return nil, false
		}
		if c == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return nil, false
		}
		return c, true
	}

	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	now := time.Now().UnixNano()
	in, ok := guardLookup(w, r, "session", func(ctx context.Context) (signIn, error) {
		found, account, err := s.store.SessionByCookie(ctx, cookie.Value, now)
		return signIn{found, account}, err
	})
	if !ok {
		return nil, false
	}
	found, account := in.session, in.account
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

// loopbackCaller is the caller an MCP tool call brings with it: the key the
// guard admitted on the stream the call arrived on, which the tool's request
// to the read API carries in its context (mcpserver.Loopback). Only a request
// the loopback made is read this way — a request from the network starts from
// a context net/http made, which never says so — and only a key, which is the
// one caller a stream admits. The copy is the loopback request's own, so that
// nothing a handler does to it reaches the stream's.
//
// One lookup per MCP request rather than one per tool call, and the same
// caller for both: the stream's scope and the tool's are judged of the same
// key by the same rule (spec 045 #13).
func loopbackCaller(r *http.Request) *caller {
	if !mcpserver.Looped(r.Context()) {
		return nil
	}
	c := callerFrom(r.Context())
	if !c.isKey() {
		return nil
	}
	copied := *c
	return &copied
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
		// tries again. A database condition is the writer's to log, once a
		// minute (spec 043 #24 u), and a hang-up is nobody's.
		if _, condition := store.Condition(err); !condition && !hungUp(r) {
			slog.Warn("could not slide a session", "err", err)
		}
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

	case stream:
		if !c.isKey() {
			// The admin token reaches no data-plane route, and every MCP
			// tool is one; a session is not what an MCP client holds.
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return false
		}
		return true

	case member, editor:
		if c.isKey() {
			// Both admit a key; what it may do among them is its scopes'
			// to say (step five, spec 045 #13).
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

// inProject is step four: which project the request is about, and what the
// account may do there (Decision 6).
//
// A key carries its project and a header cannot move it. The admin token has
// no project. Only a session needs this, and it takes the project from the
// path on the routes under `/api/v1/projects` and from `X-Tracepad-Project`
// everywhere else.
func (s *Server) inProject(w http.ResponseWriter, r *http.Request, rt route, c *caller) bool {
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

	found, ok := guardLookup(w, r, "membership", func(ctx context.Context) (membership, error) {
		project, role, err := s.store.ProjectWithRole(ctx, c.account, id)
		return membership{project, role}, err
	})
	if !ok {
		return false
	}
	project, role := found.project, found.role
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

// keyRouteRefusal is what a key on a `none` route is told (spec 045 #4, #7).
// Only the three key routes reach it: on every other `none` route the policy
// has already refused the key in terms of what the route needs.
const keyRouteRefusal = "a project key cannot list, mint or revoke keys; " +
	"that needs an owner or editor signed in, or the admin token"

// permits is step five: the route's scope against the key's (spec 045 #13).
// `any` admits every key and `none` refuses every key; a word admits a key
// that holds it, and otherwise the key is told which it holds and which the
// route needs, in the body and in RFC 6750's header, so a program can read the
// scope it lacks without parsing prose (#7). The route's pattern, never the
// request's path: the ids in it are the caller's, not the rule's.
//
// A soft-deleted project's key is dead for everything but reading the project
// (spec 005 #10), and it is told that rather than which scope it lacks: the
// answer it had before scopes existed (#13).
func permits(w http.ResponseWriter, r *http.Request, rt route, c *caller) bool {
	if !c.isKey() {
		return true
	}
	switch rt.Scope {
	case scopeAny:
		return true
	case scopeNone:
		writeError(w, http.StatusForbidden, keyRouteRefusal)
		return false
	case scopeUnset:
		// The parity test keeps this from shipping; refusing is the only
		// safe reading of "nobody said which key may call this".
		slog.Error("route has no scope", "method", rt.Method, "path", rt.Path)
		writeError(w, http.StatusInternalServerError, "this route declares no scope")
		return false
	}
	needed := rt.Scope.String()
	if slices.Contains(c.key.Scopes, needed) {
		return true
	}
	if c.project.Deleted() {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return false
	}
	w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="`+needed+`"`)
	method := rt.Method
	if method == "" {
		// The stream is served for every method; its refusal names the
		// one asked.
		method = r.Method
	}
	writeError(w, http.StatusForbidden, fmt.Sprintf("this key's scopes are %s; %s %s needs %s",
		strings.Join(c.key.Scopes, ", "), method, rt.Path, needed))
	return false
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
	return s.ownOrigin(r, statedOrigin(r))
}

// statedOrigin is where a browser says a request came from: its `Origin`, or,
// when that is missing or `null`, its `Referer`. One reading for every check
// that asks (Decision 30): cookie writes and the public routes alike, though a
// public route asks only when the request carries an `Origin` (publicBody).
func statedOrigin(r *http.Request) string {
	stated := r.Header.Get("Origin")
	if stated == "" || stated == "null" {
		stated = r.Header.Get("Referer")
	}
	return stated
}

// ownOrigin reports whether a stated `Origin` (or `Referer`) names this
// server: one of the hosts of Decision 23, under a scheme this server can be
// reached by (Decision 30). Empty, `null`, anything without a host and any
// scheme but http and https are not ours. Hosts are compared without the port
// their scheme implies, as a browser writes them.
//
// The scheme rule refuses a downgrade and nothing else. Where the server knows
// it is served over TLS — the connection itself, `X-Forwarded-Proto: https`, or
// an `https` TRACEPAD_URL for that URL's host name, on any port — an `http://`
// origin on the same host is a page an on-path attacker can write, and is
// refused. Where it knows nothing, either scheme is accepted: a TLS proxy that
// says nothing about itself sends an `https` origin to a plain-HTTP request,
// and refusing it would break every cookie write from the interface behind
// it.
func (s *Server) ownOrigin(r *http.Request, stated string) bool {
	if stated == "" || stated == "null" {
		return false
	}
	parsed, err := url.Parse(stated)
	if err != nil || parsed.Host == "" {
		return false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	named := withoutDefaultPort(scheme, parsed.Host)
	secure := overTLS(r)
	if own := s.configured; own != nil {
		if strings.EqualFold(named, own.host) {
			// The operator said what the address is, scheme and all; http
			// only where they said http and the request is not known to
			// have arrived over TLS.
			return scheme == "https" || own.scheme == "http" && !secure
		}
		// An https TRACEPAD_URL says TLS fronts that name, on whatever
		// port an origin spells: `http://host:443` is a page an on-path
		// attacker answers in plain text, not another address of ours.
		if own.scheme == "https" && strings.EqualFold(parsed.Hostname(), own.hostname) {
			secure = true
		}
	}
	// Plain http on the https port is not an address of this site but a
	// page an on-path attacker answers in plain text; only TRACEPAD_URL,
	// naming exactly that, makes it one (matched above).
	if scheme == "http" && parsed.Port() == "443" {
		return false
	}
	// A Host or X-Forwarded-Host is the address the browser typed, so its
	// port is read under the origin's own scheme: `host:443` is what a TLS
	// proxy that says nothing else forwards for `https://host`. An `http`
	// origin it matches is still refused below wherever TLS is known.
	for _, host := range s.ownHosts(r) {
		if host != "" && strings.EqualFold(named, withoutDefaultPort(scheme, host)) {
			return scheme == "https" || !secure
		}
	}
	return false
}

// withoutDefaultPort writes a host the way a browser writes it in an `Origin`:
// without the port its scheme implies. `TRACEPAD_URL=https://host:443` and a
// proxy that passes `Host: host:443` both name the address a browser calls
// `https://host`, and a comparison of the spellings would refuse every sign-in
// there. Any other port is a different origin and is left in place.
func withoutDefaultPort(scheme, host string) string {
	name, port, err := net.SplitHostPort(host)
	if err != nil {
		return host
	}
	if strings.EqualFold(scheme, "https") && port == "443" || strings.EqualFold(scheme, "http") && port == "80" {
		if strings.Contains(name, ":") {
			return "[" + name + "]"
		}
		return name
	}
	return host
}

// originRefused is the 403 for an origin that is not this server's, on the
// public routes and on cookie writes alike. It says what to set, because the
// commonest way to meet it is not an attack but a proxy that passes neither
// the address the browser used nor anything the server was told to expect
// (Decision 23), and the sign-in form is where that shows first.
const originRefused = "cross-origin request refused: the request's origin is not an address " +
	"this server knows; set TRACEPAD_URL to the public address, or forward Host / X-Forwarded-Host"

// refuseOrigin answers originRefused, and logs what was compared — once a
// minute per origin and for at most 64 origins, since a page elsewhere can
// send these as fast as it likes — so the operator looking at a failed sign-in
// sees which address to name. One sender cannot silence the line another
// needs; a sender with a stream of made-up origins can, which is why the 403
// itself carries the hint too.
func (s *Server) refuseOrigin(w http.ResponseWriter, r *http.Request) {
	origin := loggable(loggableOrigin(statedOrigin(r)))
	if held, ok := s.originLog.allowKey(origin, time.Now()); ok {
		slog.Warn("a browser request was refused because its origin is none of this server's addresses",
			"path", loggable(r.URL.Path), "origin", origin, "host", loggable(r.Host),
			"x_forwarded_host", loggable(r.Header.Get("X-Forwarded-Host")),
			"x_forwarded_proto", loggable(r.Header.Get("X-Forwarded-Proto")),
			"tracepad_url", s.configuredOrigin(),
			"not_logged_since_last", held.sameKey, "not_logged_over_cap", held.overCap)
	}
	writeError(w, http.StatusForbidden, originRefused)
}

// maxLoggedField is how much of a value the sender chose a log line keeps.
// The limiter bounds how many lines a sender gets; this bounds how long each
// one is, since a header may run to the server's megabyte.
const maxLoggedField = 256

// loggable is a value the sender chose, cut to maxLoggedField bytes.
func loggable(value string) string {
	if len(value) <= maxLoggedField {
		return value
	}
	return strings.ToValidUTF8(value[:maxLoggedField], "") + "…(truncated)"
}

// loggableOrigin is what the log may say about a stated origin: its scheme
// and host and nothing else. When the `Origin` is `null` the stated one is the
// `Referer`, a whole URL, and the page an invitation link opens carries its
// token in the query.
func loggableOrigin(stated string) string {
	parsed, err := url.Parse(stated)
	if err != nil || parsed.Host == "" {
		if stated == "" || stated == "null" {
			return stated
		}
		return "(not a URL)"
	}
	return parsed.Scheme + "://" + parsed.Host
}

// ownHosts is the two hosts a request itself says it was addressed to
// (Decision 23): its `Host`, and the first value of `X-Forwarded-Host`. The
// third, TRACEPAD_URL's, is read by ownOrigin with its scheme.
func (s *Server) ownHosts(r *http.Request) [2]string {
	// Only the first value: the header is a list when requests cross more
	// than one proxy, and the first entry is the one the browser was
	// talking to.
	forwarded, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Host"), ",")
	return [2]string{r.Host, strings.TrimSpace(forwarded)}
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
// that says so. Only the first value of `X-Forwarded-Proto` counts, as with
// `X-Forwarded-Host` (ownHosts): behind more than one proxy the header is a
// list, and its first entry is the scheme the browser used.
func overTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	proto, _, _ := strings.Cut(r.Header.Get("X-Forwarded-Proto"), ",")
	return strings.EqualFold(strings.TrimSpace(proto), "https")
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
