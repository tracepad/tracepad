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
	"github.com/tracepad/tracepad/internal/ui"
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

// handleUI answers everything the API did not claim.
//
// Three outcomes, in order:
//  1. the path belongs to the API — a real route reached under the wrong
//     method, or a misspelling under an API prefix — and gets a JSON answer,
//     never HTML. An agent that mistypes an endpoint must not get a 200 and a
//     web page;
//  2. the path names a file in the bundle — serve it;
//  3. anything else is a client-side route — serve the SPA entry.
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
	if s.assets == nil {
		// A build without the `ui` tag (spec 006 Decision 9). Every
		// interface route gets the one page that explains itself;
		// asset paths get a 404, because a bundle file that resolved to
		// an HTML apology would fail in stranger ways than missing.
		if path.Ext(r.URL.Path) != "" {
			writeError(w, http.StatusNotFound, "this build has no web interface")
			return
		}
		serveUIDocument(w, r, ui.Stub)
		return
	}
	s.serveAsset(w, r)
}

// serveAsset serves one file of the bundle, falling back to the SPA entry for
// paths that are routes rather than files.
func (s *Server) serveAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name == "" || name == "." {
		name = uiIndex
	}
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
	if err != nil || info.IsDir() {
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

// serveIndex hands back the SPA entry for a client-side route. It is
// deliberately a 200: the router in the browser decides whether the path
// exists, and a 404 status on the document would make every deep link look
// broken to anything reading status codes.
func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	index, err := fs.ReadFile(s.assets, uiIndex)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "the web interface has no entry point")
		return
	}
	serveUIDocument(w, r, index)
}

// serveUIDocument writes an HTML document that must never be cached: it names
// the hashed assets of exactly this build, and a stale copy would ask a new
// binary for a bundle it no longer has.
func serveUIDocument(w http.ResponseWriter, r *http.Request, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	w.Write(body)
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
	reserved := map[string]bool{firstSegment(mcpserver.Path): true}
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
