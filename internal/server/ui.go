package server

import (
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"

	"github.com/tracepad/tracepad/internal/mcpserver"
)

// The web interface is served from the same origin as the API (spec 006): one
// port, one credential story, no CORS. Everything here is delivery — reading
// files out of `embed.FS` and deciding what an unmatched path means — because
// the SPA is a client of the read API and the server gained no endpoint for
// it.
//
// This handler is deliberately *not* in the route table. The table is the API
// surface: what the mux serves, what `GET /api/v1` advertises and what
// openapi.json promises, all three kept in step by the parity test (spec 004
// #9, #27). A catch-all that answers "here is the app, or here is why this
// path is nothing" is not an endpoint, and listing it would tell an agent to
// call something that returns HTML.

// uiIndex is the SPA's single HTML entry: every deep link is served this file
// and routed in the browser (spec 006 Decision 1).
const uiIndex = "index.html"

// uiDocument is an HTML document the server hands out, with the policy it goes
// out under. The two are made together, from the same bytes, once: the hash in
// the policy is of the text that is sent, by construction, and a document with
// no policy cannot be built (spec 051 #3, #4).
type uiDocument struct {
	body   []byte
	policy string
}

func newUIDocument(body []byte) *uiDocument {
	return &uiDocument{body: body, policy: documentPolicy(body)}
}

// useBundle sets the files the interface is served from, and reads its entry
// once. A nil bundle is a build without the interface, which serves the stub;
// a bundle with no entry serves none, and answers `500` where the entry would
// be (serveIndex).
func (s *Server) useBundle(assets fs.FS) {
	s.assets = assets
	s.index = nil
	if assets == nil {
		return
	}
	if body, err := fs.ReadFile(assets, uiIndex); err == nil {
		s.index = newUIDocument(body)
	}
}

// handleUI answers everything the API did not claim.
//
// Four outcomes, in order:
//  1. the path belongs to the API — a real route reached under the wrong
//     method, or a misspelling under an API prefix — and gets a JSON answer,
//     never HTML. An agent that mistypes an endpoint must not get a 200 and a
//     web page;
//  2. the path names a file in the bundle — serve it;
//  3. a subresource or a script asked for a file the bundle does not carry —
//     a JSON 404, never the page (see refusesDocument);
//  4. anything else is a client-side route — serve the SPA entry. A dot in it
//     does not make it a file: ids are the application's own.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if s.reserved[firstSegment(r.URL.Path)] {
		s.writeAPIMiss(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "the web interface only answers GET")
		return
	}
	// The entry is a page and not a file to be fetched by name: asked for as
	// `/index.html`, with or without a trailing slash, it is answered with a
	// redirect to the root, which is what the file server did for the first
	// spelling and sent as a bare file, under no policy, for the second (spec
	// 051 #4). The router in the browser never sees the name.
	if path.Clean(r.URL.Path) == "/"+uiIndex {
		http.Redirect(w, r, "/", http.StatusMovedPermanently)
		return
	}
	// A request that is not a page load must not be answered with the page.
	// Serving HTML for a missing `…/chunk.js` — a tab left open across an
	// upgrade asking this binary for a bundle it no longer carries — turns a
	// plain 404 into a syntax error inside the browser, which is a much worse
	// thing to debug. What tells a script from a reload is how it was asked
	// for, not a dot in the path: a route segment is whatever the application
	// named its thing (`alice@example.com`, `v1.2`), and a reload on it has to
	// open. The cheap tests come first; the bundle is opened only for a path
	// that would otherwise be refused.
	if refusesDocument(r) && !s.hasAsset(r.URL.Path) {
		writeError(w, http.StatusNotFound, s.noSuchFile())
		return
	}
	if s.assets == nil {
		// A build without the `ui` tag (spec 006 Decision 9): every interface
		// route gets the one page that explains itself.
		serveUIDocument(w, r, s.stub)
		return
	}
	s.serveAsset(w, r)
}

func (s *Server) noSuchFile() string {
	if s.assets == nil {
		return "this build has no web interface"
	}
	return "no such file in the web interface"
}

// staticDir is where the SPA build puts everything it generates.
const staticDir = "_app"

// refusesDocument reports whether the SPA entry is the wrong answer for this
// request if the bundle carries no such file: anything under the bundle's own
// `_app/` directory, where every name is generated, and a path that ends in an
// extension asked for by anything but a page load. A page load is a request
// with `Sec-Fetch-Dest: document`, or — from a client that sends no such header
// — one that accepts `text/html`; a script, a stylesheet, an image or a bare
// fetch is not, and gets the 404 whatever it is called. A path with no
// extension is a route however it is asked for, so `curl /traces` still
// returns the page.
func refusesDocument(r *http.Request) bool {
	name := assetName(r.URL.Path)
	if name == uiIndex {
		// `/` itself, which a build without a bundle answers with its stub.
		return false
	}
	if name == staticDir || strings.HasPrefix(name, staticDir+"/") {
		return true
	}
	return path.Ext(name) != "" && !isPageLoad(r)
}

// isPageLoad reports whether a browser is navigating to this URL, as opposed to
// loading a subresource or calling it from a script.
func isPageLoad(r *http.Request) bool {
	if dest := r.Header.Get("Sec-Fetch-Dest"); dest != "" {
		return dest == "document"
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// hasAsset reports whether the bundle carries this path as a file.
func (s *Server) hasAsset(urlPath string) bool {
	if s.assets == nil {
		return false
	}
	file, err := s.assets.Open(assetName(urlPath))
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	return err == nil && !info.IsDir()
}

// serveAsset serves one file of the bundle, falling back to the SPA entry for
// paths that are routes rather than files.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request) {
	name := assetName(r.URL.Path)
	file, err := s.assets.Open(name)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrInvalid) {
			writeError(w, http.StatusInternalServerError, "failed to read the web interface")
			return
		}
		s.serveIndex(w, r)
		return
	}
	info, err := file.Stat()
	file.Close()
	// The entry is a document, and a document goes out under its policy: the
	// root names it (`/` is `index.html` here) and so must not be sent as a
	// file (spec 051 #4).
	if err != nil || info.IsDir() || name == uiIndex {
		s.serveIndex(w, r)
		return
	}
	// Everything Vite emits under `_app/immutable/` carries a content hash
	// in its name, so it can be cached forever; anything else in the bundle
	// is a stable name whose bytes change with the binary.
	if strings.HasPrefix(name, "_app/immutable/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFileFS(w, r, s.assets, name)
}

// assetName turns a URL path into the name it would have inside the bundle.
func assetName(urlPath string) string {
	name := strings.TrimPrefix(path.Clean(urlPath), "/")
	if name == "" || name == "." {
		return uiIndex
	}
	return name
}

// serveIndex hands back the SPA entry for a client-side route. It is
// deliberately a 200: the router in the browser decides whether the path
// exists, and a 404 status on the document would make every deep link look
// broken to anything reading status codes.
func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	if s.index == nil {
		writeError(w, http.StatusInternalServerError, "the web interface has no entry point")
		return
	}
	serveUIDocument(w, r, s.index)
}

// serveUIDocument writes an HTML document that must never be cached: it names
// the hashed assets of exactly this build, and a stale copy would ask a new
// binary for a bundle it no longer has. It goes out under its own policy
// (csp.go), which replaces the one every response carries rather than adding a
// second beside it (spec 051 #4).
func serveUIDocument(w http.ResponseWriter, r *http.Request, document *uiDocument) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Security-Policy", document.policy)
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	w.Write(document.body)
}

// writeAPIMiss answers a request that landed on an API prefix but on no route.
// The catch-all swallowed the mux's own 404 and 405, so both are reconstructed
// here from the same route table the mux was built from.
func (s *Server) writeAPIMiss(w http.ResponseWriter, r *http.Request) {
	allowed := s.allowedMethods(r.URL.Path)
	if len(allowed) > 0 && !slices.Contains(allowed, r.Method) {
		w.Header().Set("Allow", strings.Join(allowed, ", "))
		writeError(w, http.StatusMethodNotAllowed,
			"this endpoint answers "+strings.Join(allowed, ", ")+", not "+r.Method)
		return
	}
	writeError(w, http.StatusNotFound,
		"no such endpoint; GET /api/v1 lists everything this server serves")
}

// allowedMethods returns the methods a path is served under, if any.
func (s *Server) allowedMethods(urlPath string) []string {
	pattern := s.paths.match(urlPath)
	if pattern == "" {
		return nil
	}
	var allowed []string
	for _, route := range s.routes() {
		if route.Path == pattern {
			allowed = append(allowed, route.Method)
		}
	}
	slices.Sort(allowed)
	return allowed
}

// pathMatcher answers "which route pattern does this path belong to", using
// net/http's own routing so that `{id}` means exactly what the mux means by
// it. The methods are stripped on purpose: the mux already dispatched
// everything that matched a method, so the only question left here is about
// the path.
type pathMatcher struct{ mux *http.ServeMux }

func newPathMatcher(routes []route) pathMatcher {
	mux := http.NewServeMux()
	registered := map[string]bool{}
	for _, route := range routes {
		if registered[route.Path] {
			// Two methods on one path (`GET`/`POST /api/v1/scores`)
			// are one pattern here, and registering it twice panics.
			continue
		}
		registered[route.Path] = true
		mux.HandleFunc(route.Path, func(http.ResponseWriter, *http.Request) {})
	}
	return pathMatcher{mux: mux}
}

// match returns the route pattern urlPath resolves to, or "".
func (m pathMatcher) match(urlPath string) string {
	probe := &http.Request{Method: http.MethodGet, URL: &url.URL{Path: urlPath}}
	_, pattern := m.mux.Handler(probe)
	return pattern
}

// reservedSegments is the set of first path segments the API owns. Anything
// under one of them is an API path however badly spelled, and never a route of
// the interface — which is what keeps the interface's own routes (`/traces`,
// `/login`) from ever shadowing, or being shadowed by, an endpoint.
//
// `/mcp` is reserved whether or not MCP is being served: with it on the mux
// dispatches the path before this handler sees it, and with it off the honest
// answer is still the API's 404 rather than a web page (spec 004 #15).
func reservedSegments(routes []route) map[string]bool {
	// `/.well-known` too: it is where a client asks the server about itself
	// — an MCP client looking for OAuth metadata after a 401 — and the answer
	// has to be the API's JSON 404, not the interface's HTML.
	reserved := map[string]bool{firstSegment(mcpserver.Path): true, ".well-known": true}
	for _, route := range routes {
		reserved[firstSegment(route.Path)] = true
	}
	return reserved
}

func firstSegment(urlPath string) string {
	trimmed := strings.TrimPrefix(urlPath, "/")
	if slash := strings.IndexByte(trimmed, '/'); slash >= 0 {
		return trimmed[:slash]
	}
	return trimmed
}
