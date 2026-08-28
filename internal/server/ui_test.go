package server

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/store"
	"github.com/tracepad/tracepad/internal/ui"
)

// The web interface as the server sees it (spec 006): a catch-all that must
// serve deep links, must never answer an API path with HTML, and must not
// appear anywhere the API describes itself.

// TestSPARoutesServeTheDocument: every client-side route is one HTML entry
// served with a 200, because the router in the browser is what decides whether
// a path exists (Decision 1).
func TestSPARoutesServeTheDocument(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	for _, path := range []string{
		"/",
		"/login",
		"/traces",
		"/traces/0123456789abcdef0123456789abcdef",
		"/traces/0123456789abcdef0123456789abcdef?obs=0123456789abcdef",
		"/somewhere-the-app-does-not-know",
	} {
		rec := h.get(t, path)
		expectStatus(t, rec, 200)
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
			t.Errorf("%s: Content-Type = %q, want HTML", path, got)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s: Cache-Control = %q, want no-cache", path, got)
		}
	}
}

// TestSPARoutesNeedNoKey: the document is a static bundle; it carries no data,
// and requiring a credential for it would make the login screen unreachable.
func TestSPARoutesNeedNoKey(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.call(t, "GET", "/traces", nil, func(r *http.Request) {
		r.Header.Del("Authorization")
	})
	expectStatus(t, rec, 200)
}

// TestAPIPathsAreNeverAnsweredWithHTML is the reason the catch-all knows the
// route table: an agent that mistypes an endpoint must get JSON saying so, not
// a 200 and a web page it will try to parse.
func TestAPIPathsAreNeverAnsweredWithHTML(t *testing.T) {
	for _, mcp := range []bool{true, false} {
		cfg := &config.Config{Listen: ":0", StoreRaw: true,
			MaxBodyBytes: config.DefaultMaxBodyBytes, MCP: mcp}
		h := newHarness(t, cfg, store.WriterOptions{})

		// `/mcp` is the API's transport even on a server that is not
		// serving it (the enabled case is dispatched before the
		// catch-all sees it): turning MCP off must answer as the API,
		// not hand the path to the interface.
		paths := []string{
			"/api/v1/tracez",
			"/api/v2/traces",
			"/api/v1/traces/0123456789abcdef0123456789abcdef/nope",
			"/v1/nope",
			"/health/nope",
			"/mcp/nope",
		}
		if !mcp {
			paths = append(paths, "/mcp")
		}
		for _, path := range paths {
			rec := h.get(t, path)
			expectError(t, rec, 404, "GET /api/v1")
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Errorf("mcp=%v %s: Content-Type = %q, want JSON", mcp, path, got)
			}
		}
	}
}

// TestMethodMismatchStillAnswers405: the catch-all matches every path, so the
// mux can no longer tell "no such endpoint" from "wrong verb". Reconstructing
// the difference from the table keeps the answer the API always gave.
func TestMethodMismatchStillAnswers405(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	for _, probe := range []struct{ method, path, allow string }{
		{"GET", "/v1/traces", "POST"},
		{"DELETE", "/api/v1/traces", "GET"},
		{"POST", "/api/v1/traces/0123456789abcdef0123456789abcdef", "GET"},
		{"PUT", "/api/v1/scores", "GET, POST"},
	} {
		rec := h.call(t, probe.method, probe.path, nil)
		expectStatus(t, rec, 405)
		if got := rec.Header().Get("Allow"); got != probe.allow {
			t.Errorf("%s %s: Allow = %q, want %q", probe.method, probe.path, got, probe.allow)
		}
	}
}

// TestInterfaceRefusesWrites: the SPA is static. A POST to a client-side route
// is a client that thinks it found an endpoint, and saying so beats handing it
// the document.
func TestInterfaceRefusesWrites(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.call(t, "POST", "/traces", nil)
	expectError(t, rec, 405, "only answers GET")
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow = %q, want %q", got, "GET, HEAD")
	}
}

// TestInterfaceIsNotAnAPIRoute guards the surface the parity culture protects
// (spec 004 #9, #27): the catch-all is delivery, not an endpoint, so it must be
// absent from the route table, from the endpoint map and from openapi.json. A
// route that answers HTML advertised to an agent is a trap.
func TestInterfaceIsNotAnAPIRoute(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	for _, route := range h.server.routes() {
		if route.Path == "/" {
			t.Fatalf("the interface catch-all is in the route table")
		}
	}

	var index struct {
		Endpoints []struct {
			Path string `json:"path"`
		} `json:"endpoints"`
	}
	rec := h.get(t, "/api/v1")
	expectStatus(t, rec, 200)
	if err := json.Unmarshal(rec.Body.Bytes(), &index); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range index.Endpoints {
		if endpoint.Path == "/" {
			t.Fatalf("the endpoint map advertises the interface")
		}
	}

	var document struct {
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openAPIDocument, &document); err != nil {
		t.Fatal(err)
	}
	if _, described := document.Paths["/"]; described {
		t.Fatalf("openapi.json describes the interface as an endpoint")
	}
}

// TestHealthStillAnswers: the catch-all matches every path in the mux, so the
// cheapest thing it could break is the one route with no prefix of its own.
func TestHealthStillAnswers(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.get(t, "/health")
	expectStatus(t, rec, 200)
	if got := decodeJSON[map[string]string](t, rec)["status"]; got != "ok" {
		t.Fatalf("status = %q, want ok", got)
	}
}

// TestStubExplainsWhyThereIsNoInterface: a build without the `ui` tag is a
// complete server whose one missing part is the browser interface (Decision 9),
// and the page has to say that rather than 404.
func TestStubExplainsWhyThereIsNoInterface(t *testing.T) {
	if ui.Enabled {
		t.Skip("this build embeds the interface")
	}
	h := newHarness(t, nil, store.WriterOptions{})

	body := h.get(t, "/traces").Body.String()
	for _, fragment := range []string{"no web interface", "/api/v1"} {
		if !strings.Contains(body, fragment) {
			t.Errorf("the stub page does not mention %q", fragment)
		}
	}

}

// TestMissingFileIsNotTheDocument: a path that names a file is never a
// client-side route. Answering `…/chunk.js` with HTML — which is what a tab
// left open across an upgrade asks for — turns a plain 404 into a syntax
// error inside the browser, and the same holds whether the bundle is absent
// or merely no longer carries that name.
func TestMissingFileIsNotTheDocument(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	for _, path := range []string{
		"/favicon.ico",
		"/_app/immutable/chunks/from-a-previous-build.js",
		"/_app/immutable/assets/gone.css",
	} {
		rec := h.get(t, path)
		expectStatus(t, rec, 404)
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Errorf("%s: Content-Type = %q, want JSON", path, got)
		}
	}
}

// TestBundledFilesAreStillServed guards the check above from over-reaching:
// the entry document is a real file and must keep coming back.
func TestBundledFilesAreStillServed(t *testing.T) {
	if !ui.Enabled {
		t.Skip("this build carries no bundle to serve from")
	}
	h := newHarness(t, nil, store.WriterOptions{})

	// Whichever hashed file this build happens to carry; the names change
	// with every bundle, so the test finds one rather than naming it.
	var asset string
	fs.WalkDir(ui.Assets(), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || asset != "" || name == uiIndex {
			return err
		}
		asset = name
		return fs.SkipAll
	})
	if asset == "" {
		t.Fatal("the embedded bundle carries no files")
	}

	rec := h.get(t, "/"+asset)
	expectStatus(t, rec, 200)
	if rec.Body.Len() == 0 {
		t.Fatalf("%s came back empty", asset)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("%s: Cache-Control = %q, want the hashed-asset policy", asset, got)
	}
}
