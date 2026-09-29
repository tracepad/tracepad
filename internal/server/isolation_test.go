package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// Cross-project isolation, route by route (spec 004 Decision 33, spec 029).
//
// TestPermissionMatrix asks who may call a route; this asks what a caller of
// one project meets when it names another's rows. Project B is given a row of
// every kind that a route addresses by id or by name, and every route that
// takes one is called with A's credentials and B's identifiers. A key of A, and
// an editor and a viewer signed in to A, meet 404 (or 403 for a project they
// are not in) — never B's data, never a change to it. A route that creates by
// name on first write (a dataset, a config, a prompt) makes A its own row and
// leaves B's alone.
//
// The table below is the whole of what a route's path values mean, and a route
// whose values it does not know fails the test: a new route is classified
// before it is trusted.

const (
	bProjectName = "other"
	bSecret      = "tp-sk-other"
	// bMarker is in the names of B's rows, which a route takes in its path and
	// which A may be given the same name of on its own; bContent is in what
	// they hold, which nothing of A's can echo.
	bMarker  = "b-marker"
	bContent = "SECRET-OF-B"
)

// tenantB is what project B holds, by the names a route takes.
type tenantB struct {
	project   *store.Project
	trace     string
	span      string
	score     string
	run       string
	otherRun  string
	item      string
	queueItem string
	markers   []string
}

func (h *harness) seedTenantB(t *testing.T) *tenantB {
	t.Helper()
	b := &tenantB{project: h.second(t, bProjectName, bSecret), trace: traceHex(101), span: spanHex(101)}
	asB := asKey(bSecret)

	if err := h.writer.Submit(t.Context(), &store.IngestBatch{
		ProjectID: b.project.ID,
		Traces: []*model.Trace{{ID: b.trace, Name: bContent + " trace", UserID: bMarker + "-user",
			SessionID: bMarker + "-session"}},
		Observations: []*model.Observation{{TraceID: b.trace, ID: b.span, Type: model.TypeSpan,
			Level: model.LevelDefault, Name: bContent + " span", Input: bContent + " input", StartTime: 1, EndTime: 2}},
		IngestedAt: h.arrival,
	}); err != nil {
		t.Fatal(err)
	}

	rec := h.call(t, "POST", "/api/v1/scores", mustJSON(t, map[string]any{
		"trace_id": b.trace, "name": bContent + "-score", "value": 1}), asB, asJSON)
	expectStatus(t, rec, http.StatusCreated)
	b.score = decodeJSON[struct {
		IDs []string `json:"ids"`
	}](t, rec).IDs[0]

	expectStatus(t, h.call(t, "POST", "/api/v1/prompts/"+bMarker+"-prompt/versions", mustJSON(t,
		chatBody(bContent+" prompt text", map[string]any{"labels": []string{"production"}})), asB, asJSON), http.StatusCreated)

	b.item = itemHex(101)
	expectStatus(t, h.call(t, "POST", "/api/v1/datasets/"+bMarker+"-dataset/items", mustJSON(t,
		[]map[string]any{{"id": b.item, "input": map[string]any{"q": bContent}}}), asB, asJSON), http.StatusCreated)
	b.run = runHex(101)
	expectStatus(t, h.call(t, "POST", "/api/v1/datasets/"+bMarker+"-dataset/runs", mustJSON(t,
		map[string]any{"id": b.run}), asB, asJSON), http.StatusCreated)

	b.otherRun = runHex(102)
	expectStatus(t, h.call(t, "POST", "/api/v1/datasets/"+bMarker+"-dataset/runs", mustJSON(t,
		map[string]any{"id": b.otherRun}), asB, asJSON), http.StatusCreated)

	expectStatus(t, h.call(t, "PUT", "/api/v1/score-configs/"+bMarker+"-config", mustJSON(t,
		map[string]any{"data_type": "numeric", "direction": "higher"}), asB, asJSON), http.StatusOK)
	expectStatus(t, h.call(t, "PUT", "/api/v1/queues/"+bMarker+"-queue", mustJSON(t,
		map[string]any{"description": bContent, "score_configs": []string{bMarker + "-config"}}), asB, asJSON), http.StatusCreated)
	rec = h.call(t, "POST", "/api/v1/queues/"+bMarker+"-queue/items", mustJSON(t,
		map[string]any{"trace_id": b.trace}), asB, asJSON)
	expectStatus(t, rec, http.StatusCreated)
	b.queueItem = decodeJSON[struct {
		IDs []string `json:"ids"`
	}](t, rec).IDs[0]

	b.markers = []string{bContent, b.trace, b.span, b.score, b.item, b.run, b.otherRun, b.queueItem, b.project.ID, "tp-pk-" + bProjectName}
	return b
}

// values names what each path value means on a route: B's row of the kind the
// route is about. ok is false for a route the table does not know.
func (b *tenantB) fill(pattern string) (string, bool) {
	kind := strings.TrimPrefix(pattern, "/api/v1/")
	first, _, _ := strings.Cut(kind, "/")
	names := map[string]string{
		"prompts": bMarker + "-prompt", "datasets": bMarker + "-dataset",
		"score-configs": bMarker + "-config", "queues": bMarker + "-queue",
	}
	ids := map[string]string{
		"traces": b.trace, "observations": b.span, "sessions": bMarker + "-session",
		"users": bMarker + "-user", "scores": b.score, "runs": b.run,
		"raw": b.trace, "projects": b.project.ID,
	}
	path := pattern
	set := func(param, value string) { path = strings.ReplaceAll(path, "{"+param+"}", value) }
	if name, ok := names[first]; ok {
		set("name", name)
	}
	switch {
	case first == "datasets" && strings.Contains(pattern, "/items/{id}"):
		set("id", b.item)
	case first == "queues" && strings.Contains(pattern, "/items/{id}"):
		set("id", b.queueItem)
	case ids[first] != "":
		set("id", ids[first])
	}
	set("a", b.run)
	set("b", b.otherRun)
	set("label", "production")
	set("user_id", bMarker+"-user")
	set("erasure_id", b.trace)
	set("public_key", "tp-pk-"+bProjectName)
	set("project_id", b.project.ID)
	set("sha256", strings.Repeat("0", 64))
	set("mediaId", bMarker+"-media")
	// The queries a route insists on, so that it looks the row up.
	if strings.HasSuffix(pattern, "/diff") {
		path += "?from=1&to=2"
	}
	if strings.HasSuffix(pattern, "/next") {
		path += "?annotator=ada"
	}
	return path, !strings.Contains(path, "{")
}

// bodyFor is a valid-enough body for a write route, so that the handler goes
// past its decoder and looks the row up: a 400 for an empty body would prove
// nothing about isolation.
func (b *tenantB) bodyFor(rt route) []byte {
	switch rt.Method + " " + rt.Path {
	case "POST /api/v1/prompts/{name}/versions":
		return mustJSONBytes(chatBody("x", nil))
	case "PUT /api/v1/prompts/{name}/labels/{label}":
		return mustJSONBytes(map[string]any{"version": 1})
	case "POST /api/v1/datasets/{name}/items":
		return mustJSONBytes([]map[string]any{{"input": map[string]any{"x": 1}}})
	case "PUT /api/v1/score-configs/{name}":
		return mustJSONBytes(map[string]any{"data_type": "numeric", "direction": "higher"})
	case "PUT /api/v1/queues/{name}":
		return mustJSONBytes(map[string]any{"score_configs": []string{bMarker + "-config"}})
	case "POST /api/v1/queues/{name}/items":
		return mustJSONBytes(map[string]any{"trace_id": b.trace})
	case "POST /api/v1/queues/{name}/items/{id}/complete",
		"POST /api/v1/queues/{name}/items/{id}/skip", "POST /api/v1/queues/{name}/items/{id}/reopen":
		return mustJSONBytes(map[string]any{"annotator": "ada"})
	case "POST /api/v1/datasets/{name}/runs":
		return mustJSONBytes(map[string]any{"id": runHex(7)})
	}
	return []byte(`{}`)
}

// mayCreate says a route makes the caller's own row of that name on first
// write, so a 2xx is not a leak — B's row is what must stay as it was.
var mayCreate = map[string]bool{
	"PUT /api/v1/datasets/{name}":          true,
	"POST /api/v1/datasets/{name}/items":   true,
	"PUT /api/v1/score-configs/{name}":     true,
	"POST /api/v1/prompts/{name}/versions": true,
	"PUT /api/v1/queues/{name}":            true,
}

// notProjectScoped are the routes that are about people and the process, not
// a project's rows: they are the permission matrix's.
func notProjectScoped(rt route) bool {
	for _, prefix := range []string{"/api/v1/accounts", "/api/v1/auth", "/api/v1/setup", "/api/v1/invite"} {
		if strings.HasPrefix(rt.Path, prefix) {
			return true
		}
	}
	return rt.Policy == public || rt.Policy == presigned || rt.Policy == session
}

// refusesAsAbsent says a status is the answer of a route that has no such row
// for the caller: 404, or 403 for a project they are not in. Two families
// answer otherwise on purpose: the Langfuse media channel takes only a key
// (401 for a person), and its report on an upload it has never heard of is a
// 204 whoever asks, which reveals nothing (spec 041 #9).
func refusesAsAbsent(rt route, code int) bool {
	switch code {
	case http.StatusNotFound, http.StatusForbidden:
		return true
	case http.StatusUnauthorized:
		return rt.Policy == ingest
	case http.StatusNoContent:
		return rt.Method == "PATCH" && strings.HasPrefix(rt.Path, "/api/public/media/")
	}
	return false
}

func mustJSONBytes(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestNoRouteReachesAnotherProjectsRows(t *testing.T) {
	h := newAccountHarness(t)
	h.seed(t, &model.Trace{ID: traceHex(1), Name: "ours"})
	b := h.seedTenantB(t)
	before, err := h.store.TableCounts(t.Context(), b.project.ID)
	if err != nil {
		t.Fatal(err)
	}

	editor, viewer := h.editor(t), h.viewer(t)
	callers := []struct {
		name   string
		mutate []func(*http.Request)
	}{
		{"a key of A", nil},
		{"an editor of A", []func(*http.Request){asSession(editor), inProject(h.project.ID)}},
		{"a viewer of A", []func(*http.Request){asSession(viewer), inProject(h.project.ID)}},
	}

	// Two passes: first every route that cannot make the caller a row of B's
	// name, which must meet 404 or 403 wherever it names one of B's rows; then
	// the routes that create by name on first write, which by then may find A
	// their own. A leak is a leak in either: B's content in an answer.
	var plain, creating []route
	for _, rt := range h.server.routes() {
		if notProjectScoped(rt) || (rt.Method != "GET" && !strings.Contains(rt.Path, "{")) {
			continue
		}
		if _, known := b.fill(rt.Path); !known {
			t.Errorf("%s %s: the isolation table does not know what its path values mean; add it to tenantB.fill", rt.Method, rt.Path)
			continue
		}
		if mayCreate[rt.Method+" "+rt.Path] {
			creating = append(creating, rt)
		} else {
			plain = append(plain, rt)
		}
	}
	if len(plain)+len(creating) < 40 {
		t.Fatalf("walked %d routes, want the whole table (about 60)", len(plain)+len(creating))
	}
	walk := func(routes []route, strict bool) {
		for _, rt := range routes {
			path, _ := b.fill(rt.Path)
			for _, caller := range callers {
				rec := h.call(t, rt.Method, path, b.bodyFor(rt), append(caller.mutate, asJSON)...)
				body := rec.Body.String()
				for _, marker := range b.markers {
					if strings.Contains(body, marker) && !strings.Contains(path, marker) {
						t.Errorf("%s: %s %s answered %d with B's %q in it: %s", caller.name, rt.Method, path, rec.Code, marker, body)
					}
				}
				if strict && strings.Contains(rt.Path, "{") && !refusesAsAbsent(rt, rec.Code) {
					t.Errorf("%s: %s %s = %d (%s), want 404 or 403", caller.name, rt.Method, path, rec.Code, strings.TrimSpace(body))
				}
			}
		}
	}
	walk(plain, true)
	walk(creating, false)

	// Naming B's project in the header is a refusal for every route, for a
	// person who is not a member of it.
	for _, rt := range h.server.routes() {
		// The ingest routes take a key and no person, so a session is
		// refused before it names a project at all.
		if notProjectScoped(rt) || rt.Policy == ingest || rt.Path == "/api/v1/projects" {
			continue
		}
		path, known := b.fill(rt.Path)
		if !known {
			continue
		}
		rec := h.call(t, rt.Method, path, b.bodyFor(rt), asSession(editor), inProject(b.project.ID), asJSON)
		if rec.Code != http.StatusForbidden {
			t.Errorf("an editor of A naming B: %s %s = %d (%s), want 403", rt.Method, path, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}

	// B's rows are as they were, by the store's own count and by B's own
	// eyes.
	after, err := h.store.TableCounts(t.Context(), b.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before, after) {
		t.Errorf("B's rows changed: %v before, %v after", before, after)
	}
	for _, path := range []string{
		"/api/v1/traces/" + b.trace, "/api/v1/scores/" + b.score, "/api/v1/prompts/" + bMarker + "-prompt",
		"/api/v1/datasets/" + bMarker + "-dataset/items/" + b.item, "/api/v1/runs/" + b.run,
		"/api/v1/score-configs/" + bMarker + "-config", "/api/v1/queues/" + bMarker + "-queue/items/" + b.queueItem,
	} {
		expectStatus(t, h.call(t, "GET", path, nil, asKey(bSecret)), http.StatusOK)
	}
}
