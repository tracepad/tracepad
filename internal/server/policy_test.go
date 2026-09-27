package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

/*
The permission matrix, checked (spec 028 Decisions 3 and 7).

A matrix that lives in prose is checked by nobody. This file is the other
half of the policy and scope columns: one test walks the route table and fails
for a route that declares no policy, and one hits every non-public route as
each of the nine callers and asserts Decision 3's answer — and, for the four
keys, spec 045's (Testing #3).

Nine callers times every route is several hundred cheap requests, and it is
what turns "a viewer cannot revoke keys" and "a read key changes nothing" from
beliefs into facts — the spec 004 #33 isolation test, applied to roles and
scopes.
*/

// TestEveryRouteHasAPolicy is the parity check. The zero value of `policy` is
// `unset`, so a route added without one is caught here rather than quietly
// inheriting whatever the constant order happens to be.
func TestEveryRouteHasAPolicy(t *testing.T) {
	h := newAccountHarness(t)
	for _, rt := range h.server.routes() {
		if rt.Policy == unset {
			t.Errorf("%s %s declares no policy", rt.Method, rt.Path)
		}
	}
}

// TestEndpointMapPublishesThePolicy: `GET /api/v1` is where an agent that has
// never seen this API finds out what it would need to call each route (spec
// 028, API contract).
func TestEndpointMapPublishesThePolicy(t *testing.T) {
	h := newAccountHarness(t)
	rec := h.call(t, "GET", "/api/v1", nil, anonymous)
	expectStatus(t, rec, 200)

	listed := decodeJSON[struct {
		Endpoints []struct {
			Method string `json:"method"`
			Path   string `json:"path"`
			Policy string `json:"policy"`
			Scope  string `json:"scope"`
		} `json:"endpoints"`
	}](t, rec)
	if len(listed.Endpoints) != len(h.server.routes()) {
		t.Fatalf("the map lists %d endpoints, the table has %d",
			len(listed.Endpoints), len(h.server.routes()))
	}
	scopes := map[string]string{}
	for _, rt := range h.server.routes() {
		scopes[rt.Method+" "+rt.Path] = rt.Scope.String()
	}
	for _, endpoint := range listed.Endpoints {
		switch endpoint.Policy {
		case "public", "ingest", "member", "editor", "owner", "session":
		default:
			t.Errorf("%s %s has policy %q", endpoint.Method, endpoint.Path, endpoint.Policy)
		}
		// Spec 045 #2: the word a key needs, beside the policy.
		if want := scopes[endpoint.Method+" "+endpoint.Path]; endpoint.Scope != want || want == "unset" {
			t.Errorf("%s %s has scope %q, want the table's %q", endpoint.Method, endpoint.Path, endpoint.Scope, want)
		}
	}
}

// who is one of the nine callers: spec 028 Decision 7's six, with the project
// key split into the four keys spec 045 tells apart (Testing #3).
type who int

const (
	noCredential who = iota
	projectKey       // all three scopes, which is what every key was before
	ingestKey
	readKey
	writeKey
	deploymentToken
	viewerSession
	editorSession
	ownerSession
)

var nineCallers = []who{noCredential, projectKey, ingestKey, readKey, writeKey,
	deploymentToken, viewerSession, editorSession, ownerSession}

func (w who) String() string {
	return [...]string{"no credential", "a key with all three scopes", "an ingest key", "a read key",
		"a write key", "the admin token", "a viewer session", "an editor session", "an owner session"}[w]
}

// isKey reports the four project keys.
func (w who) isKey() bool { return w >= projectKey && w <= writeKey }

// holds reports whether one of the four keys carries a scope.
func (w who) holds(word string) bool {
	switch w {
	case projectKey:
		return true
	case ingestKey:
		return word == store.ScopeIngest
	case readKey:
		return word == store.ScopeRead
	case writeKey:
		return word == store.ScopeWrite
	}
	return false
}

// verdict is what the matrix says a caller gets on a route: either the request
// reaches the handler, or it is refused with a named status. `needs` is the
// scope a key was refused for, which the answer's `WWW-Authenticate` names.
type verdict struct {
	status   int
	fragment string
	needs    string
}

var admitted = verdict{}

// decision is Decision 3, then spec 045 #13's fifth step, as code: the policy
// against the kind of caller, and for a key the policy admits, the scope
// against the key's. `rt.Path` is the route's pattern, because two of the rules
// turn on whether the route is one of the project's own.
func decision(rt route, w who) verdict {
	byPolicy := policyDecision(rt.Policy, w, rt.Path)
	if byPolicy != admitted || !w.isKey() {
		return byPolicy
	}
	switch rt.Scope {
	case scopeAny:
		return admitted
	case scopeNone:
		// No key manages keys, whatever it may do elsewhere (#4).
		return verdict{status: http.StatusForbidden, fragment: "cannot list, mint or revoke keys"}
	}
	if w.holds(rt.Scope.String()) {
		return admitted
	}
	return verdict{http.StatusForbidden, rt.Method + " " + rt.Path + " needs " + rt.Scope.String(), rt.Scope.String()}
}

// policyDecision is Decision 3 alone, which is the whole of the answer for
// every caller but a key.
func policyDecision(p policy, w who, path string) verdict {
	if w == noCredential {
		return verdict{status: http.StatusUnauthorized, fragment: "unauthorized"}
	}
	switch p {
	case ingest:
		// A project key and nothing else: a person's browser never writes
		// spans, and the admin token never touches the data plane.
		if w.isKey() {
			return admitted
		}
		return verdict{status: http.StatusUnauthorized, fragment: "unauthorized"}

	case session:
		// A key or the admin token is a perfectly good credential asking
		// a question it cannot have.
		if w.isKey() || w == deploymentToken {
			return verdict{status: http.StatusBadRequest, fragment: "not a session"}
		}
		return admitted

	case owner:
		if w == deploymentToken || w == ownerSession {
			return admitted
		}
		return verdict{status: http.StatusForbidden, fragment: "owner account"}

	case member:
		if w == deploymentToken && !projectRoute(path) {
			// The admin token keeps exactly the powers spec 005 #11
			// gave it and still reaches no data-plane route.
			return verdict{status: http.StatusUnauthorized, fragment: "unauthorized"}
		}
		return admitted

	case editor:
		if w == deploymentToken && !projectRoute(path) {
			return verdict{status: http.StatusUnauthorized, fragment: "unauthorized"}
		}
		if w == viewerSession {
			return verdict{status: http.StatusForbidden, fragment: "viewer"}
		}
		return admitted
	}
	return admitted
}

// TestPermissionMatrix hits every non-public route as each of the nine callers
// and asserts Decision 3 and spec 045's scopes status by status. A scope
// refusal is asserted by its status, the message's naming of the route and the
// scope it needs, and the `WWW-Authenticate` header that names the scope too.
//
// "Admitted" is asserted rather than a status, because past the guard a route
// answers whatever it thinks of the request body and the ids in the path: what
// this test is about is that the guard let it through, which is exactly "not
// one of the refusals the guard produces". Every body is empty and every name
// in a path is one nothing answers to, so an admitted write stops at the
// decoder and an admitted read stops at a 404 — one server serves the whole
// matrix without a route changing what the next one is answered.
//
// The exception is the session itself: `POST /api/v1/auth/logout` ends the one
// it was called with, so each route opens fresh ones. That costs three inserts
// and no hashing, which is why the whole matrix is seconds rather than the
// minute a database per route took.
func TestPermissionMatrix(t *testing.T) {
	h := newAccountHarness(t)
	h.seed(t, &model.Trace{ID: traceHex(1), Name: "chat", UserID: "u1"})
	people := []*signedIn{h.viewer(t), h.editor(t), h.owner(t)}
	// An account to aim the account routes at, which is not one of the
	// three doing the asking: deleting or demoting a caller mid-matrix
	// would be a different test.
	subject := h.invited(t, "subject@example.com", false,
		store.Membership{ProjectID: h.project.ID, Role: store.RoleViewer})

	viewer, editor, owner := people[0], people[1], people[2]
	keys := map[who]string{
		ingestKey: h.mint(t, store.ScopeIngest).SecretKey,
		readKey:   h.mint(t, store.ScopeRead).SecretKey,
		writeKey:  h.mint(t, store.ScopeWrite).SecretKey,
	}
	for _, rt := range h.server.routes() {
		if rt.Policy == public || rt.Policy == presigned {
			continue
		}
		if rt.Policy == session {
			// The `/api/v1/auth` routes are the ones that act on the
			// session itself — one of them ends it — so these three get
			// a fresh one each. Everywhere else the sessions are
			// untouched, and re-opening them per route would be three
			// commit windows a route for nothing.
			viewer = h.resume(t, viewer, rt.Path+"viewer")
			editor = h.resume(t, editor, rt.Path+"editor")
			owner = h.resume(t, owner, rt.Path+"owner")
		}
		t.Run(rt.Method+" "+rt.Path, func(t *testing.T) {
			path := fill(rt.Path, h.project.ID, subject.ID)
			for _, w := range nineCallers {
				want := decision(rt, w)
				rec := h.call(t, rt.Method, path, nil, callerOf(w, keys, owner, editor, viewer),
					inProject(h.project.ID))

				if want == admitted {
					if refusedBy(rec) {
						t.Errorf("%s: %s %s = %d (%s), want the handler to be reached",
							w, rt.Method, path, rec.Code, strings.TrimSpace(rec.Body.String()))
					}
					continue
				}
				if rec.Code != want.status {
					t.Errorf("%s: %s %s = %d (%s), want %d",
						w, rt.Method, path, rec.Code, strings.TrimSpace(rec.Body.String()), want.status)
					continue
				}
				if message, _ := decodeJSON[map[string]any](t, rec)["error"].(string); !strings.Contains(message, want.fragment) {
					t.Errorf("%s: %s %s said %q, want it to mention %q",
						w, rt.Method, path, message, want.fragment)
				}
				challenge := rec.Header().Get("WWW-Authenticate")
				if want.needs != "" {
					if wantHeader := `Bearer error="insufficient_scope", scope="` + want.needs + `"`; challenge != wantHeader {
						t.Errorf("%s: %s %s: WWW-Authenticate = %q, want %q", w, rt.Method, path, challenge, wantHeader)
					}
				} else if strings.Contains(challenge, "insufficient_scope") {
					t.Errorf("%s: %s %s: WWW-Authenticate = %q on a refusal that is not about scope",
						w, rt.Method, path, challenge)
				}
			}
		})
	}
}

// callerOf turns one of the nine into the request mutator that is it. `keys`
// holds the secrets of the three narrow keys.
func callerOf(w who, keys map[who]string, owner, editor, viewer *signedIn) func(*http.Request) {
	switch w {
	case noCredential:
		return anonymous
	case projectKey:
		return func(*http.Request) {} // the harness sends the key by default
	case ingestKey, readKey, writeKey:
		return asKey(keys[w])
	case deploymentToken:
		return asAdmin
	case viewerSession:
		return asSession(viewer)
	case editorSession:
		return asSession(editor)
	default:
		return asSession(owner)
	}
}

// refusedBy reports one of the guard's own refusals. Everything else — a 404
// for an id that is not there, a 400 for an empty body, a 415 for a missing
// content type — means the handler ran, which is what "admitted" is.
func refusedBy(rec *httptest.ResponseRecorder) bool {
	switch rec.Code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	case http.StatusBadRequest:
		// The guard has two 400s of its own and they are named, so they
		// are told apart from a handler's complaint about the body.
		body := rec.Body.String()
		return strings.Contains(body, "not a session") ||
			strings.Contains(body, projectHeader)
	}
	return false
}

// fill substitutes a route pattern's path values with something that exists
// where existence is the point, and something plausible where it is not.
func fill(pattern, projectID, accountID string) string {
	path := pattern
	if strings.HasPrefix(pattern, "/api/v1/projects/{id}") {
		path = strings.Replace(path, "{id}", projectID, 1)
	} else if strings.HasPrefix(pattern, "/api/v1/accounts/{id}") {
		path = strings.Replace(path, "{id}", accountID, 1)
	}
	replacements := [][2]string{
		{"{project_id}", projectID},
		// A key that is not there: naming a real one would revoke a key
		// the matrix is still asking with.
		{"{public_key}", "tp-pk-nothing-by-this-name"},
		{"{user_id}", "u1"},
		{"{name}", "nothing-by-this-name"},
		{"{label}", "production"},
		{"{id}", traceHex(1)},
		{"{a}", traceHex(2)},
		{"{b}", traceHex(3)},
	}
	for _, pair := range replacements {
		path = strings.ReplaceAll(path, pair[0], pair[1])
	}
	return path
}

// TestSessionsAreIsolatedByProject is spec 004 #33 for roles: a member of one
// project never sees a row of another, whatever the header says.
func TestSessionsAreIsolatedByProject(t *testing.T) {
	h := newAccountHarness(t)
	other := h.second(t, "other", "tp-sk-other")
	h.seed(t, &model.Trace{ID: traceHex(1), Name: "ours"})
	if err := h.writer.Submit(t.Context(), &store.IngestBatch{
		ProjectID: other.ID,
		Traces:    []*model.Trace{{ID: traceHex(2), Name: "theirs"}},
	}); err != nil {
		t.Fatal(err)
	}

	viewer := h.viewer(t)
	rec := h.call(t, "GET", "/api/v1/traces", nil, asSession(viewer), inProject(h.project.ID))
	expectStatus(t, rec, 200)
	listing := decodeJSON[struct {
		Traces []struct {
			ID string `json:"id"`
		} `json:"traces"`
	}](t, rec)
	if len(listing.Traces) != 1 || listing.Traces[0].ID != traceHex(1) {
		t.Fatalf("traces = %+v, want only this project's", listing.Traces)
	}
	// The other project's trace is not reachable by id either, even though
	// the id is known: the scope is the project, not the row.
	rec = h.call(t, "GET", "/api/v1/traces/"+traceHex(2), nil,
		asSession(viewer), inProject(h.project.ID))
	expectStatus(t, rec, http.StatusNotFound)

	// And naming the other project is a 403 rather than an empty listing.
	rec = h.call(t, "GET", "/api/v1/traces", nil, asSession(viewer), inProject(other.ID))
	expectError(t, rec, http.StatusForbidden, "not a member")
}

// TestKeyLoginKeepsWorking is what makes this a change the interface can
// follow one PR later: every route a key reached before it still reaches —
// but for the three that manage keys, which no key reaches since spec 045 #4.
// The harness's key is the server's own, made with all three scopes (#5), as
// every key that predates scopes was (Testing #5).
func TestKeyLoginKeepsWorking(t *testing.T) {
	// Deliberately without an admin token: this is the deployment the
	// interface has today, and every route it reaches must go on answering.
	h := newHarness(t, &config.Config{
		Listen: ":0", StoreRaw: true, MaxBodyBytes: config.DefaultMaxBodyBytes,
	}, store.WriterOptions{})
	h.seed(t, &model.Trace{ID: traceHex(1), Name: "chat"})

	for _, path := range []string{
		"/api/v1/traces", "/api/v1/sessions", "/api/v1/stats", "/api/v1/system",
		"/api/v1/prompts", "/api/v1/datasets", "/api/v1/queues", "/api/v1/score-configs",
		"/api/v1/projects", "/api/v1/projects/" + h.project.ID,
	} {
		if rec := h.get(t, path); rec.Code != 200 {
			t.Errorf("GET %s with a project key = %d (%s)", path, rec.Code, rec.Body)
		}
	}
	keys := "/api/v1/projects/" + h.project.ID + "/keys"
	expectError(t, h.get(t, keys), http.StatusForbidden, "cannot list, mint or revoke keys")

	// And the project listing still answers a key with its own project and
	// no role, which is the shape the interface reads today.
	rec := h.get(t, "/api/v1/projects")
	projects := decodeJSON[struct {
		Projects []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
		} `json:"projects"`
	}](t, rec)
	if len(projects.Projects) != 1 || projects.Projects[0].ID != h.project.ID {
		t.Fatalf("projects = %+v", projects.Projects)
	}
	if projects.Projects[0].Role != "" {
		t.Errorf("role = %q for a key, want none: a key is not a person", projects.Projects[0].Role)
	}
}
