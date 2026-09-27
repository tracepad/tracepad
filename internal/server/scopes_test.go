package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/mcpserver"
	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// Scopes (spec 045, PR 2; Testing #1–#4, #8, #9, #11, #12). The matrix itself
// is in policy_test.go, which walks every route as the four keys beside the
// five other callers.

// TestEveryRouteDeclaresAScope is the scope column's parity check (Testing #1):
// no route is unset, and the rules that follow from the policy column hold
// wherever the two columns speak about the same thing.
func TestEveryRouteDeclaresAScope(t *testing.T) {
	h := newAccountHarness(t)
	for _, rt := range append(h.server.routes(), h.server.mcpRoute(nil)) {
		name := rt.Method + " " + rt.Path
		switch {
		case rt.Scope == scopeUnset:
			t.Errorf("%s declares no scope", name)
		case (rt.Policy == public || rt.Policy == presigned) && rt.Scope != scopeAny:
			t.Errorf("%s is public and asks a key for %s, want any", name, rt.Scope)
		case (rt.Policy == owner || rt.Policy == session) && rt.Scope != scopeNone:
			t.Errorf("%s is %s and admits a key with %s, want none", name, rt.Policy, rt.Scope)
		case strings.HasPrefix(rt.Path, "/api/v1/projects/{id}/keys") && rt.Scope != scopeNone:
			t.Errorf("%s manages keys and admits a key with %s, want none (#4)", name, rt.Scope)
		case rt.Policy == ingest && rt.Scope != scopeIngest:
			t.Errorf("%s is ingest and asks a key for %s", name, rt.Scope)
		case rt.Policy == stream:
			// The one route of no method: the transport's own requests
			// all carry reads, which the guard judges again one by one.
			if rt.Scope != scopeRead {
				t.Errorf("%s is the MCP stream and asks for %s; every tool reads", name, rt.Scope)
			}
		case rt.Scope == scopeRead && rt.Method != http.MethodGet:
			t.Errorf("%s changes something and asks only for read", name)
		case strings.HasSuffix(rt.Path, "/next") && rt.Scope != scopeWrite:
			t.Errorf("%s claims an item and asks for %s, want write (#3)", name, rt.Scope)
		}
	}
}

// keyScopesOfTheSpec is the API contract's table, as test data: the scope
// every route asks of a key (spec 045, API contract). It is the oracle — a
// route added, removed or re-scoped fails TestKeyScopesMatchTheSpec until this
// list, and the spec it is copied from, move with it.
var keyScopesOfTheSpec = map[string][]string{
	"ingest": {
		"POST /v1/traces", "POST /api/public/otel/v1/traces", "POST /api/public/media",
		"PATCH /api/public/media/{mediaId}", "GET /api/public/media/{mediaId}", "POST /api/v1/scores",
	},
	"read": {
		"GET /api/v1/system", "GET /api/v1/traces", "GET /api/v1/traces/last", "GET /api/v1/traces/{id}",
		"GET /api/v1/observations/{id}/io", "GET /api/v1/media/{sha256}", "GET /api/v1/raw",
		"GET /api/v1/raw/{id}", "GET /api/v1/sessions", "GET /api/v1/sessions/{id}", "GET /api/v1/stats",
		"GET /api/v1/stats/scores", "GET /api/v1/facets", "GET /api/v1/users", "GET /api/v1/users/{id}",
		"GET /api/v1/scores", "GET /api/v1/scores/{id}", "GET /api/v1/prompts",
		"GET /api/v1/prompts/{name}/versions", "GET /api/v1/prompts/{name}/diff", "GET /api/v1/datasets",
		"GET /api/v1/datasets/{name}", "GET /api/v1/datasets/{name}/items",
		"GET /api/v1/datasets/{name}/items/{id}", "GET /api/v1/datasets/{name}/items/{id}/versions",
		"GET /api/v1/datasets/{name}/runs", "GET /api/v1/runs", "GET /api/v1/runs/{id}",
		"GET /api/v1/runs/{id}/items", "GET /api/v1/runs/{a}/compare/{b}", "GET /api/v1/score-configs",
		"GET /api/v1/score-configs/{name}", "GET /api/v1/queues", "GET /api/v1/queues/{name}",
		"GET /api/v1/queues/{name}/items", "GET /api/v1/queues/{name}/items/{id}",
	},
	"write": {
		"DELETE /api/v1/traces/{id}", "DELETE /api/v1/traces", "DELETE /api/v1/scores/{id}",
		"POST /api/v1/prompts/{name}/versions", "DELETE /api/v1/prompts/{name}",
		"PUT /api/v1/prompts/{name}/labels/{label}", "DELETE /api/v1/prompts/{name}/labels/{label}",
		"PUT /api/v1/datasets/{name}", "DELETE /api/v1/datasets/{name}", "POST /api/v1/datasets/{name}/items",
		"DELETE /api/v1/datasets/{name}/items/{id}", "POST /api/v1/datasets/{name}/runs",
		"POST /api/v1/runs/{id}/finish", "DELETE /api/v1/runs/{id}", "PUT /api/v1/score-configs/{name}",
		"DELETE /api/v1/score-configs/{name}", "PUT /api/v1/queues/{name}", "DELETE /api/v1/queues/{name}",
		"POST /api/v1/queues/{name}/items", "POST /api/v1/queues/{name}/items/from-traces",
		"GET /api/v1/queues/{name}/next", "POST /api/v1/queues/{name}/items/{id}/complete",
		"POST /api/v1/queues/{name}/items/{id}/skip", "POST /api/v1/queues/{name}/items/{id}/reopen",
		"DELETE /api/v1/queues/{name}/items/{id}", "PATCH /api/v1/projects/{id}",
		"DELETE /api/v1/projects/{id}/users/{user_id}/data",
	},
	"any": {
		"GET /health", "PUT /api/public/media/{mediaId}/upload", "GET /api/v1", "GET /api/v1/openapi.json",
		"GET /api/v1/setup", "POST /api/v1/setup", "POST /api/v1/auth/login", "POST /api/v1/auth/accept-invite",
		"GET /api/v1/projects", "GET /api/v1/projects/{id}", "GET /api/v1/prompts/{name}",
	},
	"none": {
		"POST /api/v1/auth/logout", "GET /api/v1/auth/me", "PATCH /api/v1/auth/me", "GET /api/v1/auth/sessions",
		"DELETE /api/v1/auth/sessions",
		"POST /api/v1/projects", "DELETE /api/v1/projects/{id}", "POST /api/v1/projects/{id}/restore",
		"GET /api/v1/projects/{id}/members", "GET /api/v1/accounts", "POST /api/v1/accounts",
		"GET /api/v1/accounts/{id}", "PATCH /api/v1/accounts/{id}", "DELETE /api/v1/accounts/{id}",
		"POST /api/v1/accounts/{id}/invite", "PUT /api/v1/accounts/{id}/projects/{project_id}",
		"DELETE /api/v1/accounts/{id}/projects/{project_id}",
		"GET /api/v1/projects/{id}/keys", "POST /api/v1/projects/{id}/keys",
		"DELETE /api/v1/projects/{id}/keys/{public_key}",
	},
}

// TestKeyScopesMatchTheSpec is the golden list (Testing #2), compared with the
// route table both ways, so that the spec and not the code is what says which
// scope a route asks for.
func TestKeyScopesMatchTheSpec(t *testing.T) {
	h := newAccountHarness(t)
	want := map[string]string{}
	for word, routes := range keyScopesOfTheSpec {
		for _, rt := range routes {
			if previous, twice := want[rt]; twice {
				t.Errorf("the golden list has %s under both %s and %s", rt, previous, word)
			}
			want[rt] = word
		}
	}
	if len(want) != 100 {
		t.Errorf("the golden list has %d routes, the spec's table 100", len(want))
	}
	served := map[string]bool{}
	for _, rt := range h.server.routes() {
		name := rt.Method + " " + rt.Path
		served[name] = true
		if got, ok := want[name]; !ok {
			t.Errorf("%s is served and asks for %s, and the spec does not list it", name, rt.Scope)
		} else if got != rt.Scope.String() {
			t.Errorf("%s asks a key for %s, the spec says %s", name, rt.Scope, got)
		}
	}
	for name := range want {
		if !served[name] {
			t.Errorf("the spec lists %s, which is not served", name)
		}
	}
}

// TestAnIngestKeyCannotRead is red without the fix (Testing #4): a key minted
// for ingest alone does what a production application does — sends a span,
// writes a score, fetches a prompt — and is refused a read with the scope it
// lacks, in the body and in RFC 6750's header.
func TestAnIngestKeyCannotRead(t *testing.T) {
	h := newAccountHarness(t)
	expectStatus(t, h.send(t, "POST", "/api/v1/prompts/greeting/versions", chatBody("Hello.", nil)),
		http.StatusCreated)
	editor := h.editor(t)
	rec := h.call(t, "POST", "/api/v1/projects/"+h.project.ID+"/keys",
		mustJSON(t, map[string]any{"scopes": []string{"ingest"}}), asSession(editor), inProject(h.project.ID))
	expectStatus(t, rec, http.StatusCreated)
	ingestOnly := asKey(decodeJSON[minted](t, rec).SecretKey)

	expectStatus(t, h.post(t, "/v1/traces", fixtureBody(t, "001-langfuse-sdk-generation"), ingestOnly),
		http.StatusOK)
	expectStatus(t, h.call(t, "POST", "/api/v1/scores", mustJSON(t, map[string]any{
		"trace_id": traceHex(1), "name": "thumbs", "value": 1,
	}), ingestOnly), http.StatusCreated)
	expectStatus(t, h.call(t, "GET", "/api/v1/prompts/greeting", nil, ingestOnly), http.StatusOK)

	// HEAD asks what GET asks.
	for _, method := range []string{"GET", "HEAD"} {
		rec = h.call(t, method, "/api/v1/traces", nil, ingestOnly)
		expectStatus(t, rec, http.StatusForbidden)
		if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer error="insufficient_scope", scope="read"` {
			t.Errorf("%s: WWW-Authenticate = %q, want the scope it lacks", method, got)
		}
	}
	expectError(t, h.call(t, "GET", "/api/v1/traces/"+traceHex(1), nil, ingestOnly), http.StatusForbidden,
		"this key's scopes are ingest; GET /api/v1/traces/{id} needs read")
	expectError(t, h.call(t, "DELETE", "/api/v1/traces?confirm=test&to=2099-01-01T00:00:00Z", nil, ingestOnly),
		http.StatusForbidden, "needs write")
}

// TestMintingTakesScopes: `scopes` is required, one to three of the three
// words, duplicates collapsed and spelled in the one order; the answer and
// the listing say what was minted (Testing #8, Decision 6).
func TestMintingTakesScopes(t *testing.T) {
	h := newAccountHarness(t)
	keys := "/api/v1/projects/" + h.project.ID + "/keys"

	for _, body := range []string{``, `{}`, `{"name": "no scopes"}`, `{"scopes": []}`, `{"scopes": null}`,
		`{"scopes": ["admin"]}`, `{"scopes": ["read", "Read"]}`, `{"scopes": "read"}`} {
		rec := h.call(t, "POST", keys, []byte(body), asAdmin)
		expectStatus(t, rec, http.StatusBadRequest)
		if message, _ := decodeJSON[map[string]any](t, rec)["error"].(string); body != `{"scopes": "read"}` &&
			!(strings.Contains(message, `"ingest"`) && strings.Contains(message, `"read"`) && strings.Contains(message, `"write"`)) {
			t.Errorf("%s: the refusal %q does not name the three", body, message)
		}
	}

	rec := h.call(t, "POST", keys, mustJSON(t, map[string]any{
		"scopes": []string{"write", "ingest", "write"}, "name": "judge",
	}), asAdmin)
	expectStatus(t, rec, http.StatusCreated)
	got := decodeJSON[minted](t, rec)
	if !slices.Equal(got.Scopes, []string{"ingest", "write"}) || got.Name != "judge" {
		t.Errorf("minted %+v, want ingest and write, once each, in that order, named judge", got)
	}
	if listed := h.keysOf(t, h.project.ID)[got.PublicKey]; !slices.Equal(listed.Scopes, []string{"ingest", "write"}) {
		t.Errorf("the listing has %+v", listed)
	}
	// The key does what it says and no more.
	expectStatus(t, h.call(t, "GET", "/api/v1/traces", nil, asKey(got.SecretKey)), http.StatusForbidden)
	if rec := h.call(t, "PUT", "/api/v1/queues/review", mustJSON(t, map[string]any{}), asKey(got.SecretKey)); refusedBy(rec) {
		t.Errorf("an ingest and write key was refused a write: %d %s", rec.Code, rec.Body)
	}
}

// TestTheLastIngestKeyAsksForTheEcho: revoking the last key that carries
// `ingest` is a dry run until the project's name is echoed, as revoking the
// last key is; revoking a key the project can ingest without is not (Testing
// #9, Decision 11).
func TestTheLastIngestKeyAsksForTheEcho(t *testing.T) {
	h := newAccountHarness(t)
	keys := "/api/v1/projects/" + h.project.ID + "/keys/"
	reader := h.mint(t, "read")

	rec := h.call(t, "DELETE", keys+testPublic, nil, asAdmin)
	expectStatus(t, rec, http.StatusOK)
	preview := decodeJSON[struct {
		DryRun  bool   `json:"dry_run"`
		Confirm string `json:"confirm"`
		Note    string `json:"note"`
	}](t, rec)
	if !preview.DryRun || preview.Confirm != h.project.Name || !strings.Contains(preview.Note, "ingest") {
		t.Errorf("revoking the last ingest key answered %+v, want a dry run asking for the name", preview)
	}
	if _, still := h.keysOf(t, h.project.ID)[testPublic]; !still {
		t.Fatal("the dry run revoked the key")
	}
	// A wrong echo is refused inside the write.
	expectStatus(t, h.call(t, "DELETE", keys+testPublic+"?confirm=nope", nil, asAdmin), http.StatusBadRequest)

	// The read key goes without one: ingest is untouched by it.
	rec = h.call(t, "DELETE", keys+reader.PublicKey, nil, asAdmin)
	expectStatus(t, rec, http.StatusOK)
	if dry, _ := decodeJSON[map[string]any](t, rec)["dry_run"].(bool); dry {
		t.Error("revoking a read key beside an ingest key asked for the echo")
	}

	// A second ingest key makes the first one revocable without it.
	spare := h.mint(t, "ingest")
	h.mint(t, "read")
	rec = h.call(t, "DELETE", keys+testPublic, nil, asAdmin)
	if dry, _ := decodeJSON[map[string]any](t, rec)["dry_run"].(bool); dry || rec.Code != http.StatusOK {
		t.Errorf("revoking one of two ingest keys = %d %s, want it done", rec.Code, rec.Body)
	}
	// And now the spare is the last: the echo applies it.
	rec = h.call(t, "DELETE", keys+spare.PublicKey, nil, asAdmin)
	if dry, _ := decodeJSON[map[string]any](t, rec)["dry_run"].(bool); !dry {
		t.Fatalf("revoking the last ingest key = %s, want a dry run", rec.Body)
	}
	expectStatus(t, h.call(t, "DELETE", keys+spare.PublicKey+"?confirm="+h.project.Name, nil, asAdmin),
		http.StatusOK)
	if _, still := h.keysOf(t, h.project.ID)[spare.PublicKey]; still {
		t.Error("the echo did not revoke the key")
	}
}

// TestAKeyReadsItsOwnScopes: the project listing and the project read answer a
// key with the key itself — which it is and what it may do — so that a program
// that met a 403 can learn what it holds (Testing #12's premise, Decision 12).
func TestAKeyReadsItsOwnScopes(t *testing.T) {
	h := newAccountHarness(t)
	reader := h.mint(t, "read")
	type ownKey struct {
		PublicKey string   `json:"public_key"`
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
	}
	type row struct {
		ID  string  `json:"id"`
		Key *ownKey `json:"key"`
	}
	for _, c := range []struct {
		as     func(*http.Request)
		public string
		scopes []string
	}{
		{asKey(reader.SecretKey), reader.PublicKey, []string{"read"}},
		{func(*http.Request) {}, testPublic, allScopes},
	} {
		listed := decodeJSON[struct {
			Projects []row `json:"projects"`
		}](t, h.call(t, "GET", "/api/v1/projects", nil, c.as)).Projects
		one := decodeJSON[row](t, h.call(t, "GET", "/api/v1/projects/"+h.project.ID, nil, c.as))
		for _, got := range append(listed, one) {
			if got.Key == nil || got.Key.PublicKey != c.public || !slices.Equal(got.Key.Scopes, c.scopes) {
				t.Errorf("a key's own project row = %+v, want its key %s with %v", got, c.public, c.scopes)
			}
		}
	}
	// A person is not a key, and is not told about one.
	owner := h.owner(t)
	rec := h.call(t, "GET", "/api/v1/projects/"+h.project.ID, nil, asSession(owner))
	if strings.Contains(rec.Body.String(), `"key"`) {
		t.Errorf("an owner's project read carries a key: %s", rec.Body)
	}
}

// TestADeletedProjectsKeyIsNotToldItsScopes: a soft-deleted project's key keeps
// the answers it had before scopes — `401` everywhere but reading the project —
// rather than learning which scope it lacks (Decision 13).
func TestADeletedProjectsKeyIsNotToldItsScopes(t *testing.T) {
	h := newAccountHarness(t)
	reader := asKey(h.mint(t, "read").SecretKey)
	expectStatus(t, h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"?confirm="+h.project.Name, nil, asAdmin),
		http.StatusAccepted)

	for _, request := range []struct{ method, path string }{
		{"PATCH", "/api/v1/projects/" + h.project.ID},
		{"DELETE", "/api/v1/projects/" + h.project.ID + "/users/u1/data"},
		{"POST", "/v1/traces"},
		{"GET", "/api/v1/traces"},
	} {
		rec := h.call(t, request.method, request.path, nil, reader)
		if rec.Code != http.StatusUnauthorized || strings.Contains(rec.Header().Get("WWW-Authenticate"), "scope") {
			t.Errorf("%s %s = %d (%s), want a plain 401", request.method, request.path, rec.Code, rec.Body)
		}
	}
	expectError(t, h.call(t, "GET", "/api/v1/projects/"+h.project.ID+"/keys", nil, reader),
		http.StatusForbidden, "cannot list, mint or revoke keys")
	expectStatus(t, h.call(t, "GET", "/api/v1/projects/"+h.project.ID, nil, reader), http.StatusOK)
}

// mcpHarness is an account harness that serves MCP.
func mcpHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, &config.Config{Listen: ":0", StoreRaw: true, MaxBodyBytes: config.DefaultMaxBodyBytes,
		AdminToken: adminToken, MCP: true}, store.WriterOptions{})
}

// mcpCall posts one JSON-RPC message to the MCP stream.
func (h *harness) mcpCall(t *testing.T, body string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	return h.call(t, "POST", mcpserver.Path, []byte(body), append([]func(*http.Request){func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
	}}, mutate...)...)
}

const (
	mcpInitialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
		`"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	mcpListTraces = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_traces","arguments":{}}}`
)

// TestTheStreamIsJudgedByTheGuard: the MCP stream is a row of the guard's
// table (policy `stream`, scope `read`), so the nine callers get the guard's
// answers there — a key that may read is served, a key that may not is told
// which scope it lacks before its stream lifts a deadline, and everyone else
// is `401` with a Bearer challenge (Testing #12, the review of #93).
func TestTheStreamIsJudgedByTheGuard(t *testing.T) {
	h := mcpHarness(t)
	h.seed(t, &model.Trace{ID: traceHex(1), Name: "chat"})
	viewer, editor, owner := h.viewer(t), h.editor(t), h.owner(t)
	keys := map[who]string{
		ingestKey: h.mint(t, store.ScopeIngest).SecretKey,
		readKey:   h.mint(t, store.ScopeRead).SecretKey,
		writeKey:  h.mint(t, store.ScopeWrite).SecretKey,
	}
	for _, w := range nineCallers {
		rec := h.mcpCall(t, mcpListTraces, callerOf(w, keys, owner, editor, viewer))
		switch {
		case w == projectKey || w == readKey:
			expectStatus(t, rec, http.StatusOK)
			if !strings.Contains(rec.Body.String(), "chat") {
				t.Errorf("%s: list_traces = %s, want the trace", w, rec.Body)
			}
		case w == ingestKey || w == writeKey:
			expectError(t, rec, http.StatusForbidden, "; POST /mcp needs read")
			if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer error="insufficient_scope", scope="read"` {
				t.Errorf("%s: WWW-Authenticate = %q", w, got)
			}
		default:
			expectStatus(t, rec, http.StatusUnauthorized)
			if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="tracepad"` {
				t.Errorf("%s: WWW-Authenticate = %q, want the Bearer challenge", w, got)
			}
		}
		// A refusal is the guard's and says what the guard says; a stream
		// that is served says what the transport sets for it.
		if got := rec.Header().Get("Cache-Control"); rec.Code != http.StatusOK && got != callerCacheControl {
			t.Errorf("%s: Cache-Control = %q", w, got)
		}
	}
}

// TestAToolCallLooksTheKeyUpOnce: the tools reach the read API through the
// loopback carrying the caller the stream was admitted with, so one MCP request
// is one key lookup however many reads its tools make — and a tool call from a
// key that has since lost its project would still be judged by the guard's
// project and scope steps, which run on the carried caller as on any other.
func TestAToolCallLooksTheKeyUpOnce(t *testing.T) {
	h := mcpHarness(t)
	h.seed(t, &model.Trace{ID: traceHex(1), Name: "chat"})
	var lookups atomic.Int64
	t.Cleanup(func() { keyBySecret = (*store.Store).KeyBySecret })
	keyBySecret = func(st *store.Store, ctx context.Context, secret string) (*store.Project, *store.KeyInfo, error) {
		lookups.Add(1)
		return st.KeyBySecret(ctx, secret)
	}

	rec := h.mcpCall(t, mcpListTraces)
	expectStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "chat") {
		t.Fatalf("list_traces = %s", rec.Body)
	}
	if n := lookups.Load(); n != 1 {
		t.Errorf("one tool call looked the key up %d times, want once", n)
	}

	// The marker is the loopback's alone: a request from the network that
	// carries a caller-less context and no credential is refused as ever.
	expectStatus(t, h.call(t, "GET", "/api/v1/traces", nil, anonymous), http.StatusUnauthorized)
}
