package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/store"
)

// HTTP hardening (spec 001 Decisions 14–17, spec 028 Decisions 29 and 30, spec 003
// Decision 26): what every response says about framing and sniffing, who may
// post to the public ways in, what a shared cache may keep, and how long and
// how many connections the transport holds.

// TestEveryResponseRefusesToBeFramed: the interface, the API, an API miss,
// health, and a media body, which carries a policy of its own — exactly one
// Content-Security-Policy, and that one refusing frames too.
func TestEveryResponseRefusesToBeFramed(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	picture := testPicture(20000, 3)
	expectStatus(t, h.post(t, "/v1/traces", encodeExport(t, imageExport(t, picture))), 200)

	for _, path := range []string{
		"/", "/traces", "/health", "/api/v1", "/api/v1/traces", "/api/v1/nowhere",
		"/api/v1/media/" + hexSHA(picture),
	} {
		rec := h.get(t, path)
		header := rec.Header()
		policies := header.Values("Content-Security-Policy")
		if len(policies) != 1 || !strings.Contains(policies[0], "frame-ancestors 'none'") {
			t.Errorf("%s (%d): Content-Security-Policy = %q, want one policy with frame-ancestors 'none'",
				path, rec.Code, policies)
		}
		for name, want := range map[string]string{
			"X-Frame-Options":        "DENY",
			"X-Content-Type-Options": "nosniff",
			"Referrer-Policy":        "same-origin",
		} {
			if got := header.Values(name); len(got) != 1 || got[0] != want {
				t.Errorf("%s: %s = %q, want exactly %q", path, name, got, want)
			}
		}
	}
	// The media body's own sandbox is intact under it.
	media := h.get(t, "/api/v1/media/"+hexSHA(picture))
	expectStatus(t, media, 200)
	if got := media.Header().Get("Content-Security-Policy"); got != mediaSandbox ||
		!strings.Contains(got, "sandbox") {
		t.Errorf("media policy = %q, want %q", got, mediaSandbox)
	}
}

// TestPublicRoutesRefuseOtherOriginsAndOtherBodies is login CSRF: a page on
// another origin that posts a form at the sign-in route. The form cannot set
// Content-Type to JSON without a preflight, and the browser names its origin;
// either is enough, and the guard refuses both before the handler runs. A
// client with no Origin at all — the CLI, an SDK, curl — is not a browser
// being driven by somebody else's page, and passes.
func TestPublicRoutesRefuseOtherOriginsAndOtherBodies(t *testing.T) {
	h := newAccountHarness(t)
	body := mustJSON(t, map[string]any{"email": "a@b.c", "password": testAccountPassword})
	for _, rt := range publicBodyRoutes(t, h) {
		try := func(name string, want int, mutate func(*http.Request)) {
			t.Helper()
			var reached bool
			var read error
			req := httptest.NewRequest(rt.Method, rt.Path, bytes.NewReader(body))
			req.Host = "tracepad.local:4318"
			mutate(req)
			rec := httptest.NewRecorder()
			guardedStub(h, rt, &reached, &read).ServeHTTP(rec, req)
			passed := reached && rec.Code == http.StatusOK
			switch {
			case want == http.StatusOK && !passed:
				t.Errorf("%s %s, %s: refused with %d %s, want it through", rt.Method, rt.Path, name, rec.Code, rec.Body)
			case want != http.StatusOK && (reached || rec.Code != want):
				t.Errorf("%s %s, %s: status %d, handler reached = %v; want %d from the guard",
					rt.Method, rt.Path, name, rec.Code, reached, want)
			}
		}
		json := func(r *http.Request) { r.Header.Set("Content-Type", "application/json") }
		with := func(fs ...func(*http.Request)) func(*http.Request) {
			return func(r *http.Request) {
				for _, f := range fs {
					f(r)
				}
			}
		}
		origin := func(v string) func(*http.Request) {
			return func(r *http.Request) { r.Header.Set("Origin", v) }
		}

		try("no Origin, JSON", http.StatusOK, json)
		try("JSON with a charset", http.StatusOK,
			func(r *http.Request) { r.Header.Set("Content-Type", "application/json; charset=utf-8") })
		try("this origin", http.StatusOK, with(json, origin("http://tracepad.local:4318")))
		try("forwarded host", http.StatusOK, with(json, origin("https://tracepad.example"), viaLocalProxy,
			func(r *http.Request) { r.Header.Set("X-Forwarded-Host", "tracepad.example") }))
		try("forwarded host from a peer nobody trusts", http.StatusForbidden, with(json, origin("https://tracepad.example"),
			func(r *http.Request) { r.Header.Set("X-Forwarded-Host", "tracepad.example") }))

		referer := func(v string) func(*http.Request) {
			return func(r *http.Request) { r.Header.Set("Referer", v) }
		}
		try("an opaque origin with this server's referer", http.StatusOK,
			with(json, origin("null"), referer("http://tracepad.local:4318/login")))
		try("this server's referer alone", http.StatusOK, with(json, referer("http://tracepad.local:4318/login")))
		// No Origin is not a browser, whatever it forwards as a Referer.
		try("another site's referer alone", http.StatusOK, with(json, referer("https://evil.example/page")))

		try("another port of the same host", http.StatusForbidden, with(json, origin("http://tracepad.local:8080")))
		try("another site", http.StatusForbidden, with(json, origin("https://evil.example")))
		try("an opaque origin", http.StatusForbidden, with(json, origin("null")))
		try("an opaque origin with another site's referer", http.StatusForbidden,
			with(json, origin("null"), referer("https://evil.example/page")))

		try("a text/plain form", http.StatusUnsupportedMediaType,
			func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") })
		try("a urlencoded form", http.StatusUnsupportedMediaType,
			func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") })
		try("a multipart form", http.StatusUnsupportedMediaType,
			func(r *http.Request) { r.Header.Set("Content-Type", "multipart/form-data; boundary=x") })
		try("no Content-Type", http.StatusUnsupportedMediaType, func(*http.Request) {})
	}

	// End to end: the form a page elsewhere would post, whose body is
	// valid JSON, does not sign anybody in.
	h.owner(t)
	form := httptest.NewRequest("POST", "/api/v1/auth/login", strings.NewReader(
		`{"email":"owner@example.com","password":"`+testAccountPassword+`","x":"="}`))
	form.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, form)
	expectStatus(t, rec, http.StatusUnsupportedMediaType)
	if sessionCookieOf(rec) != nil {
		t.Error("a text/plain form set a session cookie")
	}
	// The ordinary JSON-API rule is untouched: no Content-Type needed.
	expectStatus(t, h.send(t, "POST", "/api/v1/prompts/p/versions", chatBody("x", nil)), http.StatusCreated)
}

// TestPromptReadsAreOneCallers: every prompt read may be cached by the client
// that asked, and by nothing shared between callers.
func TestPromptReadsAreOneCallers(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	for range 2 {
		expectStatus(t, h.send(t, "POST", "/api/v1/prompts/summarize/versions",
			chatBody("You are terse.", nil)), http.StatusCreated)
	}
	for _, path := range []string{
		"/api/v1/prompts",
		"/api/v1/prompts/summarize",
		"/api/v1/prompts/summarize/versions",
		"/api/v1/prompts/summarize/diff?from=1&to=2",
	} {
		rec := h.get(t, path)
		expectStatus(t, rec, http.StatusOK)
		if got := rec.Header().Get("Cache-Control"); got != "private, max-age=60" {
			t.Errorf("%s: Cache-Control = %q, want private, max-age=60", path, got)
		}
		if got := rec.Header().Get("Vary"); got != "Authorization, Cookie, X-Tracepad-Project" {
			t.Errorf("%s: Vary = %q, want the credential and the project header", path, got)
		}
	}
}

// TestCallersResponsesAreNotKept: a route that needs a caller answers as one
// caller's and keeps nothing, for a session as for a key, refusals included;
// the reads that may be kept (prompts above, media in media_test.go) say so
// themselves, and the routes anyone may call are left alone.
func TestCallersResponsesAreNotKept(t *testing.T) {
	h := newAccountHarness(t)
	viewer := h.viewer(t)
	private := func(name string, rec *httptest.ResponseRecorder, want int) {
		t.Helper()
		expectStatus(t, rec, want)
		if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
			t.Errorf("%s: Cache-Control = %q, want private, no-store", name, got)
		}
		if got := rec.Header().Get("Vary"); got != "Authorization, Cookie, X-Tracepad-Project" {
			t.Errorf("%s: Vary = %q, want the credential and the project header", name, got)
		}
	}
	for _, path := range []string{"/api/v1/traces", "/api/v1/sessions"} {
		private(path+" by a session", h.call(t, "GET", path, nil, asSession(viewer), inProject(h.project.ID)), http.StatusOK)
		private(path+" by a key", h.get(t, path), http.StatusOK)
		private(path+" refused", h.call(t, "GET", path, nil, anonymous), http.StatusUnauthorized)
	}
	for _, path := range []string{"/health", "/api/v1/setup", "/api/v1"} {
		rec := h.call(t, "GET", path, nil, anonymous)
		expectStatus(t, rec, http.StatusOK)
		if got := rec.Header().Get("Cache-Control"); got != "" {
			t.Errorf("%s: Cache-Control = %q on a route anyone may call, want none", path, got)
		}
	}
}

func mustListen(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

// TestOnlyAKeyedStreamOutlivesTheWriteDeadline: a document that takes longer
// than the write deadline is cut off, and so is MCP for a caller with no key —
// answered 401 before anything runs, under the ordinary deadline. With a key,
// the same slow handler behind mcpStream is written whole.
func TestOnlyAKeyedStreamOutlivesTheWriteDeadline(t *testing.T) {
	h := newHarness(t, &config.Config{Listen: ":0", StoreRaw: true, MaxBodyBytes: config.DefaultMaxBodyBytes,
		MCP: true, AdminToken: adminToken}, store.WriterOptions{})
	if h.server.http.WriteTimeout != writeTimeout {
		t.Errorf("WriteTimeout = %s, want %s", h.server.http.WriteTimeout, writeTimeout)
	}

	var ran atomic.Int64
	slow := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran.Add(1)
		io.ReadAll(r.Body)
		select {
		case <-time.After(1500 * time.Millisecond):
			io.WriteString(w, "finished")
		case <-r.Context().Done():
			io.WriteString(w, "cancelled")
		}
	})
	mux := http.NewServeMux()
	mux.Handle("/document", slow)
	mux.Handle("/stream", h.server.mcpStream(slow))
	server := httptest.NewUnstartedServer(mux)
	// Both deadlines far under the handler's time: the write deadline is
	// the stream's to lift, and the read deadline net/http clears itself
	// once the body is read, so neither cancels a keyed call.
	// The margins leave room for the key lookup and the body read on a
	// machine busy with other gates.
	server.Config.WriteTimeout = 500 * time.Millisecond
	server.Config.ReadTimeout = 500 * time.Millisecond
	server.Start()
	defer server.Close()

	get := func(path, key string) (int, string, error) {
		request, _ := http.NewRequest("POST", server.URL+path, strings.NewReader(`{"jsonrpc":"2.0"}`))
		if key != "" {
			request.Header.Set("Authorization", "Bearer "+key)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return 0, "", err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		return response.StatusCode, string(body), err
	}
	if _, body, err := get("/document", ""); err == nil && body == "finished" {
		t.Error("a document outlived the write deadline")
	}
	if code, body, err := get("/stream", testSecret); err != nil || code != 200 || body != "finished" {
		t.Errorf("the stream with a project key = %d %q, %v; want it whole and not cancelled", code, body, err)
	}
	before := ran.Load()
	// The admin token reaches no data-plane route, and every MCP tool is
	// one: it is sent for a key up front rather than after an initialize
	// that leads nowhere.
	for _, key := range []string{"", "tp-sk-not-a-key", adminToken} {
		if code, _, err := get("/stream", key); err != nil || code != http.StatusUnauthorized {
			t.Errorf("the stream with key %q = %d, %v; want 401", key, code, err)
		}
	}
	if ran.Load() != before {
		t.Error("the stream's handler ran for a caller with no key")
	}

	// The real route: 401 without a key, served with one.
	initialize := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
		`"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	accept := func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
	}
	refused := h.call(t, "POST", "/mcp", initialize, anonymous, accept)
	expectStatus(t, refused, http.StatusUnauthorized)
	if got := refused.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
	}
	expectStatus(t, h.call(t, "POST", "/mcp", initialize, accept), http.StatusOK)
	// Where a client that met the 401 goes looking for OAuth metadata, it
	// finds the API's JSON 404 rather than the interface's page.
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-authorization-server"} {
		rec := h.call(t, "GET", path, nil, anonymous)
		expectStatus(t, rec, http.StatusNotFound)
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Errorf("%s: Content-Type = %q, want JSON", path, got)
		}
	}
}

// failingClose is a listener whose Close reports an error, the one way
// http.Server.Shutdown fails other than by running out of time.
type failingClose struct{ net.Listener }

var errCloseFailed = errors.New("the listener would not close")

func (f failingClose) Close() error {
	f.Listener.Close()
	return errCloseFailed
}

// serveTracked runs a handler behind the server's in-flight count on a real
// listener, the way ListenAndServe would, and starts one request on it.
func serveTracked(t *testing.T, h *harness, handler http.HandlerFunc) (started chan struct{}, served chan error) {
	return serveTrackedOn(t, h, mustListen(t), handler)
}

func serveTrackedOn(t *testing.T, h *harness, listener net.Listener, handler http.HandlerFunc) (started chan struct{}, served chan error) {
	t.Helper()
	started = make(chan struct{})
	h.server.http.Handler = h.server.running.track(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		handler(w, r)
	}))
	served = make(chan error, 1)
	go func() { served <- h.server.http.Serve(listener) }()

	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	io.WriteString(conn, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
	<-started
	return started, served
}

// TestShutdownWaitsForTheHandlersItInterrupts: past the drain window the
// connection is closed, but its handler — here one still on its way to the
// writer — is waited for, so the writer its owner closes next is one it has
// already finished with. The stop is a clean one.
func TestShutdownWaitsForTheHandlersItInterrupts(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	var submitted atomic.Value
	_, served := serveTracked(t, h, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		err := h.writer.Submit(context.Background(), &store.SessionsEnd{SessionID: store.SessionID("gone")})
		submitted.Store(fmt.Sprint(err))
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.server.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown = %v, want a clean stop", err)
	}
	// What serve does next: close the writer.
	h.writer.Close()
	got, _ := submitted.Load().(string)
	if got == "" {
		t.Fatal("Shutdown returned before the handler did")
	}
	if strings.Contains(got, store.ErrWriterClosed.Error()) {
		t.Errorf("the handler met a closed writer: %s", got)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Serve = %v, want ErrServerClosed", err)
	}
}

// TestShutdownReportsAHandlerItCouldNotWaitFor: one still running after the
// grace period makes the stop an unclean one, which the caller turns into a
// non-zero exit.
func TestShutdownReportsAHandlerItCouldNotWaitFor(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.server.handlerGrace = 100 * time.Millisecond
	release := make(chan struct{})
	defer close(release)
	serveTracked(t, h, func(w http.ResponseWriter, r *http.Request) { <-release })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.server.Shutdown(ctx); !errors.Is(err, errHandlersStuck) {
		t.Errorf("Shutdown = %v, want errHandlersStuck", err)
	}
}

// TestAnOriginMayNotDowngrade: where the server knows it is served over TLS —
// the proxy says so, or TRACEPAD_URL does for its host — an `http://` origin on
// the same host is a page an on-path attacker can write, and is refused on the
// public routes and on cookie writes alike (spec 028 Decision 30). Where it
// knows nothing, either scheme passes, or a TLS proxy that says nothing would
// break every write from the interface. The port a scheme implies is no part
// of the comparison, and a proxy chain's scheme is its first.
func TestAnOriginMayNotDowngrade(t *testing.T) {
	const host = "tracepad.example"
	const https = "https"
	cases := []struct {
		name       string
		configured string
		proto      string // X-Forwarded-Proto
		arrivedAt  string // Host, when not host
		origin     string
		ours       bool
	}{
		{"https proxy, https origin", "", https, "", "https://" + host, true},
		{"https proxy, http origin", "", https, "", "http://" + host, false},
		{"https configured, http origin", "https://" + host, "", "", "http://" + host, false},
		{"https configured, https origin", "https://" + host, "", "", "https://" + host, true},
		{"http configured, https origin", "http://" + host, "", "", "https://" + host, true},
		{"http configured, http origin", "http://" + host, "", "", "http://" + host, true},
		{"http configured behind an https proxy, http origin", "http://" + host, https, "", "http://" + host, false},
		{"nothing known, http origin", "", "", "", "http://" + host, true},
		{"nothing known, https origin", "", "", "", "https://" + host, true},
		{"another scheme", "", "", "", "ftp://" + host, false},

		// Behind two proxies the header is a list; its first entry is
		// the browser's scheme.
		{"two proxies, the first https, http origin", "", "https, http", "", "http://" + host, false},
		{"two proxies, both https, http origin", "", "https,https", "", "http://" + host, false},
		{"two proxies, both https, https origin", "", "https,https", "", "https://" + host, true},
		{"two proxies, the first http, http origin", "", "http, https", "", "http://" + host, true},

		// A port the scheme implies is not part of the origin a browser
		// writes, whichever side spells it.
		{"https configured with :443, Host rewritten", "https://" + host + ":443", "", "backend:8080", "https://" + host, true},
		{"http configured with :80, Host rewritten", "http://" + host + ":80", "", "backend:8080", "http://" + host, true},
		{"an origin spelling :443", "https://" + host, "", "backend:8080", "https://" + host + ":443", true},
		{"Host carrying :443 behind an https proxy", "", https, host + ":443", "https://" + host, true},
		{"Host carrying :443 behind a proxy that says nothing", "", "", host + ":443", "https://" + host, true},
		{"Host carrying :80", "", "", host + ":80", "http://" + host, true},
		{"another port stays another origin", "https://" + host + ":8443", "", "backend:8080", "https://" + host, false},
		{":443 is not http's default", "http://" + host + ":443", "", "backend:8080", "http://" + host, false},
		{"an IPv6 host with its default port", "https://[::1]:443", "", "backend:8080", "https://[::1]", true},

		// An https TRACEPAD_URL vouches for TLS on its name, whatever port
		// an http origin spells; and for its name only.
		{"https configured, http origin spelling :443, Host :443", "https://" + host, "", host + ":443", "http://" + host + ":443", false},
		{"https configured, http origin on another port of its name", "https://" + host, "", host + ":8080", "http://" + host + ":8080", false},
		{"https configured for another name, http origin", "https://elsewhere.example", "", "", "http://" + host, true},

		// Plain http on the https port is nobody's address but an
		// attacker's on the path, unless TRACEPAD_URL names exactly it.
		{"nothing configured, http origin spelling :443, Host :443", "", "", host + ":443", "http://" + host + ":443", false},
		{"http configured as :443 exactly, http origin :443", "http://" + host + ":443", "", "backend:8080", "http://" + host + ":443", true},
	}
	for _, c := range cases {
		h := newHarness(t, &config.Config{Listen: ":0", StoreRaw: true,
			MaxBodyBytes: config.DefaultMaxBodyBytes, AdminToken: adminToken, URL: c.configured},
			store.WriterOptions{})
		request := func(method, path string) *http.Request {
			r := httptest.NewRequest(method, path, strings.NewReader("{}"))
			r.Host = host
			if c.arrivedAt != "" {
				r.Host = c.arrivedAt
			}
			r.Header.Set("Origin", c.origin)
			if c.proto != "" {
				r.Header.Set("X-Forwarded-Proto", c.proto)
			}
			viaLocalProxy(r)
			return r
		}
		if got := h.server.ownOrigin(request("POST", "/"), c.origin); got != c.ours {
			t.Errorf("%s: ownOrigin = %v, want %v", c.name, got, c.ours)
		}
		// The public routes: the guard's answer.
		for _, rt := range publicBodyRoutes(t, h) {
			var reached bool
			var read error
			r := request(rt.Method, rt.Path)
			asJSON(r)
			rec := httptest.NewRecorder()
			guardedStub(h, rt, &reached, &read).ServeHTTP(rec, r)
			if reached != c.ours || (!c.ours && rec.Code != http.StatusForbidden) {
				t.Errorf("%s, %s: status %d, reached %v; want through = %v", c.name, rt.Path, rec.Code, reached, c.ours)
			}
		}
		// A cookie write: the same rule.
		if got := h.server.sameOrigin(request("POST", "/api/v1/auth/logout")); got != c.ours {
			t.Errorf("%s: sameOrigin = %v, want %v", c.name, got, c.ours)
		}
	}
}

// TestEveryOriginAgainstEveryArrival walks the whole matrix of spec 028
// Decision 30 — the origin's scheme and port, how the request arrived, what
// X-Forwarded-Proto says, which header names the host and with what port, and
// what TRACEPAD_URL is — and holds ownOrigin, the cookie writes and a public
// route to one rule, computed here in its own words and in port numbers
// rather than in spellings:
//
//   - A port is the one written, or the scheme's standard one where none
//     is (80 for http, 443 for https).
//   - TLS is known when the connection is TLS, the first value of
//     X-Forwarded-Proto is `https`, or TRACEPAD_URL is https for the
//     origin's name, whatever port either spells.
//   - TRACEPAD_URL names the origin's site when both are on their scheme's
//     standard port, or both on the same other port. Then it is the whole
//     answer: https passes, and http passes only where TRACEPAD_URL is http
//     and TLS is not known.
//   - Otherwise an http origin that writes port 443 does not pass: plain
//     http on the https port is a page an attacker on the path answers.
//   - Otherwise, Host or X-Forwarded-Host names the origin when its port is
//     the origin's port, or when it writes none and the origin is on its
//     standard port. Then https passes, and http passes where TLS is not
//     known.
//   - Nothing else passes.
func TestEveryOriginAgainstEveryArrival(t *testing.T) {
	const name = "tracepad.example"
	standardPort := map[string]int{"http": 80, "https": 443}
	port := func(scheme string, written int) int {
		if written == 0 {
			return standardPort[scheme]
		}
		return written
	}
	onStandard := func(scheme string, written int) bool {
		return port(scheme, written) == standardPort[scheme]
	}
	written := func(p int) string {
		if p == 0 {
			return ""
		}
		return fmt.Sprintf(":%d", p)
	}

	type configured struct {
		scheme string // "" for none
		port   int
	}
	type carrier struct {
		label    string
		host     string
		forward  bool // the name rides in X-Forwarded-Host, not Host
		names    bool
		portSent int
	}
	var carriers []carrier
	for _, p := range []int{0, 80, 443} {
		carriers = append(carriers,
			carrier{"Host " + name + written(p), name + written(p), false, true, p},
			carrier{"X-Forwarded-Host " + name + written(p), name + written(p), true, true, p})
	}
	carriers = append(carriers, carrier{label: "neither header names it"})
	forwardedProto := map[string]bool{"": false, "https": true, "http": false, "https, http": true}

	failures, passes, rows := 0, 0, 0
	for _, url := range []configured{{}, {"http", 0}, {"https", 0}, {"https", 443}} {
		configuredURL := ""
		if url.scheme != "" {
			configuredURL = url.scheme + "://" + name + written(url.port)
		}
		h := newHarness(t, &config.Config{Listen: ":0", StoreRaw: true,
			MaxBodyBytes: config.DefaultMaxBodyBytes, AdminToken: adminToken, URL: configuredURL},
			store.WriterOptions{})
		public := publicBodyRoutes(t, h)[0]
		for _, originScheme := range []string{"http", "https"} {
			for _, originPort := range []int{0, 80, 443, 8443} {
				for _, overTLSConn := range []bool{false, true} {
					for proto, protoSaysTLS := range forwardedProto {
						for _, c := range carriers {
							// Every row's origin has TRACEPAD_URL's name.
							tlsKnown := overTLSConn || protoSaysTLS || url.scheme == "https"
							want := func() bool {
								if url.scheme != "" {
									same := onStandard(originScheme, originPort) && onStandard(url.scheme, url.port) ||
										!onStandard(originScheme, originPort) && !onStandard(url.scheme, url.port) &&
											port(originScheme, originPort) == port(url.scheme, url.port)
									if same {
										return originScheme == "https" || url.scheme == "http" && !tlsKnown
									}
								}
								if originScheme == "http" && originPort == 443 {
									return false
								}
								if c.names && (c.portSent == 0 && onStandard(originScheme, originPort) ||
									c.portSent != 0 && c.portSent == port(originScheme, originPort)) {
									return originScheme == "https" || !tlsKnown
								}
								return false
							}()

							rows++
							if want {
								passes++
							}
							origin := originScheme + "://" + name + written(originPort)
							request := func(path string) *http.Request {
								r := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
								r.Host = "backend:8080"
								if c.names && !c.forward {
									r.Host = c.host
								}
								if c.forward {
									r.Header.Set("X-Forwarded-Host", c.host)
								}
								if proto != "" {
									r.Header.Set("X-Forwarded-Proto", proto)
								}
								if overTLSConn {
									r.TLS = &tls.ConnectionState{}
								}
								viaLocalProxy(r)
								r.Header.Set("Origin", origin)
								asJSON(r)
								return r
							}
							var reached bool
							var read error
							rec := httptest.NewRecorder()
							guardedStub(h, public, &reached, &read).ServeHTTP(rec, request(public.Path))
							got := [3]bool{
								h.server.ownOrigin(request("/"), origin),
								h.server.sameOrigin(request("/api/v1/auth/logout")),
								reached,
							}
							if got != [3]bool{want, want, want} {
								failures++
								if failures <= 20 {
									t.Errorf("TRACEPAD_URL %q, Origin %s, TLS connection %v, X-Forwarded-Proto %q, %s: "+
										"ownOrigin, cookie write, %s = %v, want %v",
										configuredURL, origin, overTLSConn, proto, c.label,
										public.Path, got, want)
								}
							}
						}
					}
				}
			}
		}
	}
	if failures > 20 {
		t.Errorf("... and %d more", failures-20)
	}
	// A rule that passed everything, or nothing, would make the walk vacuous.
	if passes == 0 || passes == rows {
		t.Errorf("the rule passes %d of %d rows; want some of each", passes, rows)
	}
	t.Logf("%d rows, %d passing", rows, passes)
}

// TestAnEmptyListenAddressIsHTTPs: binding the listener itself keeps
// net/http's reading of an empty address, rather than net.Listen's.
func TestAnEmptyListenAddressIsHTTPs(t *testing.T) {
	for configured, want := range map[string]string{"": ":http", ":4318": ":4318", "127.0.0.1:0": "127.0.0.1:0"} {
		if got := listenAddr(configured); got != want {
			t.Errorf("listenAddr(%q) = %q, want %q", configured, got, want)
		}
	}
}

// TestARefusedOriginSaysWhatToSet: the commonest 403 for an origin is a proxy
// that forwards neither the browser's address nor anything configured, met on
// the sign-in form. The answer names what to set, and the log names what was
// compared — once a minute, however many are refused.
func TestARefusedOriginSaysWhatToSet(t *testing.T) {
	h := newAccountHarness(t)
	var (
		mu     sync.Mutex
		logged bytes.Buffer
	)
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, out: &logged}, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	behindAProxy := func(r *http.Request) {
		r.Host = "backend:8080"
		r.Header.Set("Origin", "https://traces.example.com")
	}
	for range 3 {
		rec := h.call(t, "POST", "/api/v1/auth/login",
			mustJSON(t, map[string]any{"email": "a@b.c", "password": testAccountPassword}),
			anonymous, asJSON, behindAProxy)
		expectError(t, rec, http.StatusForbidden, "set TRACEPAD_URL to the public address, or forward Host / X-Forwarded-Host")
	}
	// A cookie write says the same.
	who := h.owner(t)
	rec := h.call(t, "POST", "/api/v1/auth/logout", nil, asSession(who), behindAProxy)
	expectError(t, rec, http.StatusForbidden, "set TRACEPAD_URL")
	// A page elsewhere refused over and over does not use up the line
	// the next origin gets.
	h.call(t, "POST", "/api/v1/auth/login",
		mustJSON(t, map[string]any{"email": "a@b.c", "password": testAccountPassword}),
		anonymous, asJSON, func(r *http.Request) { r.Header.Set("Origin", "https://another.example") })

	// An opaque origin is read from the Referer, a whole URL; here the
	// invitation page reached over http behind a TLS proxy, its token in
	// the query. The log names where it came from, never the token.
	rec = h.call(t, "POST", "/api/v1/auth/accept-invite",
		mustJSON(t, map[string]any{"token": "t", "password": testAccountPassword}),
		anonymous, asJSON, func(r *http.Request) {
			r.Header.Set("Origin", "null")
			r.Header.Set("Referer", "http://"+r.Host+"/invite?token=invitation-secret-in-a-referer")
			r.Header.Set("X-Forwarded-Proto", "https")
		}, viaLocalProxy)
	expectStatus(t, rec, http.StatusForbidden)

	// A value the sender chose is cut short in the log, however long.
	long := strings.Repeat("a", 100_000)
	h.call(t, "POST", "/api/v1/auth/login",
		mustJSON(t, map[string]any{"email": "a@b.c", "password": testAccountPassword}),
		anonymous, asJSON, func(r *http.Request) {
			r.Header.Set("Origin", "https://"+long+".example")
			r.Header.Set("X-Forwarded-Host", long)
		}, viaLocalProxy)
	mu.Lock()
	defer mu.Unlock()
	if strings.Contains(logged.String(), "invitation-secret-in-a-referer") || strings.Contains(logged.String(), "/invite?") {
		t.Errorf("a refused Referer reached the log with its path and query:\n%s", logged.String())
	}
	if !strings.Contains(logged.String(), "origin=http://example.com ") {
		t.Errorf("the refused Referer's origin is not in the log:\n%s", logged.String())
	}
	for _, line := range strings.Split(logged.String(), "\n") {
		if len(line) > 2048 {
			t.Errorf("a refusal logged a line of %d bytes, want the sender's values cut short", len(line))
		}
	}
	if !strings.Contains(logged.String(), "…(truncated)") {
		t.Error("the long origin was not marked as cut short")
	}

	lines := strings.Count(logged.String(), "origin is none of this server's addresses")
	if lines != 4 {
		t.Errorf("seven refusals from four origins were logged %d times, want once for each:\n%s", lines, logged.String())
	}
	if !strings.Contains(logged.String(), "origin=https://another.example") {
		t.Errorf("the second origin was not logged:\n%s", logged.String())
	}
	for _, field := range []string{"level=WARN", "origin=https://traces.example.com", "host=backend:8080", "path=/api/v1/auth/login"} {
		if !strings.Contains(logged.String(), field) {
			t.Errorf("the warning does not say %s:\n%s", field, logged.String())
		}
	}
}

// TestAResponseTheWriteDeadlineCutIsLogged: a response the client did not
// read before the write deadline is one WARN line — its route and how much of
// it was written — once a minute per route, for a raw body and for writeJSON
// alike, and writeJSON does not log it a second time (spec 001 #19).
func TestAResponseTheWriteDeadlineCutIsLogged(t *testing.T) {
	h := newHarness(t, &config.Config{Listen: ":0", StoreRaw: true, MaxBodyBytes: config.DefaultMaxBodyBytes},
		store.WriterOptions{})
	var (
		mu     sync.Mutex
		logged bytes.Buffer
	)
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, out: &logged}, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	lines := func(substring string) []string {
		mu.Lock()
		defer mu.Unlock()
		var found []string
		for line := range strings.Lines(logged.String()) {
			if strings.Contains(line, substring) {
				found = append(found, line)
			}
		}
		return found
	}

	// Far more than the socket buffers of both ends hold, so the handler's
	// write is still waiting on the client when the deadline passes.
	const size = 16 << 20
	export := fmt.Sprintf(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"%032x","spanId":"%016x",`+
		`"name":"big","startTimeUnixNano":"1700000000000000000","endTimeUnixNano":"1700000001000000000",`+
		`"attributes":[{"key":"pad","value":{"stringValue":"%s"}}]}]}]}]}`, 1, 1, strings.Repeat("a", size))
	if response := h.postJSON(t, []byte(export)); response.StatusCode != http.StatusOK {
		t.Fatalf("the export = %d", response.StatusCode)
	}
	listing := decodeJSON[struct {
		Batches []struct {
			ID int64 `json:"id"`
		} `json:"batches"`
	}](t, h.get(t, "/api/v1/raw"))
	if len(listing.Batches) != 1 {
		t.Fatalf("%d raw batches, want 1", len(listing.Batches))
	}

	finished := make(chan struct{}, 1)
	serve := func(next http.Handler) *httptest.Server {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r)
			finished <- struct{}{}
		}))
		server.Config.WriteTimeout = 300 * time.Millisecond
		server.Start()
		t.Cleanup(server.Close)
		return server
	}
	// slowGet asks and does not read, until the handler has returned.
	slowGet := func(server *httptest.Server, path string) {
		t.Helper()
		conn, err := net.Dial("tcp", server.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.(*net.TCPConn).SetReadBuffer(4 << 10)
		fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: tracepad\r\nAuthorization: Bearer %s\r\n\r\n", path, testSecret)
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Fatalf("the handler of %s was still writing after ten seconds", path)
		}
	}

	api := serve(h.server.Handler())
	path := fmt.Sprintf("/api/v1/raw/%d", listing.Batches[0].ID)
	slowGet(api, path)
	cut := lines("a response was cut off")
	if len(cut) != 1 {
		t.Fatalf("%d lines for the cut raw body, want 1:\n%s", len(cut), strings.Join(cut, ""))
	}
	line := cut[0]
	if !strings.Contains(line, "level=WARN") || !strings.Contains(line, `route="GET /api/v1/raw/{id}"`) {
		t.Errorf("the line is not a warning naming the route:\n%s", line)
	}
	// Zero is possible: the deadline counts from the request's headers,
	// and a slow machine may reach it before the handler first writes.
	// TestCutWriterCountsWhatWasWritten pins the count itself.
	var written int64
	if _, err := fmt.Sscan(line[strings.Index(line, "bytes_written=")+len("bytes_written="):], &written); err != nil ||
		written < 0 || written >= size {
		t.Errorf("bytes_written = %d (%v), want part of the %d-byte body:\n%s", written, err, size, line)
	}
	// Within the minute the route's next cut is counted, not logged.
	slowGet(api, path)
	if n := len(lines("a response was cut off")); n != 1 {
		t.Errorf("%d lines after a second cut within the minute, want 1", n)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /big", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"pad": strings.Repeat("a", size)})
	})
	slowGet(serve(h.server.reportCutResponses(mux)), "/big")
	if cut := lines(`route="GET /big"`); len(cut) != 1 {
		t.Errorf("%d lines for the cut JSON body, want 1", len(cut))
	}
	if failed := lines("failed to write response"); len(failed) != 0 {
		t.Errorf("writeJSON logged the cut again:\n%s", strings.Join(failed, ""))
	}
}

// TestCutWriterCountsWhatWasWritten: the count is what the connection took,
// and a write or a flush the deadline stopped marks the response cut; any
// other failure does not.
func TestCutWriterCountsWhatWasWritten(t *testing.T) {
	deadline := fmt.Errorf("write tcp: %w", os.ErrDeadlineExceeded)
	for _, tc := range []struct {
		name    string
		under   *deadlineStub
		flush   bool
		written int64
		cut     bool
	}{
		{"a write the deadline stopped", &deadlineStub{take: 3, err: deadline}, false, 7, true},
		{"a flush the deadline stopped", &deadlineStub{take: 4, flushErr: deadline}, true, 8, true},
		{"a client that hung up", &deadlineStub{take: 3, err: syscall.EPIPE}, false, 7, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &cutWriter{ResponseWriter: tc.under}
			c.Write([]byte("four"))
			tc.under.failing = true
			c.Write([]byte("four"))
			if tc.flush {
				http.NewResponseController(c).Flush()
			}
			if c.written != tc.written || c.cut != tc.cut {
				t.Errorf("written = %d, cut = %v; want %d, %v", c.written, c.cut, tc.written, tc.cut)
			}
		})
	}
}

// deadlineStub takes whole writes until it is failing, then take bytes of one
// and err; its flush answers flushErr.
type deadlineStub struct {
	httptest.ResponseRecorder
	failing  bool
	take     int
	err      error
	flushErr error
}

func (s *deadlineStub) Write(body []byte) (int, error) {
	if s.failing && s.err != nil {
		return s.take, s.err
	}
	return len(body), nil
}

func (s *deadlineStub) FlushError() error { return s.flushErr }

// TestAMalformedURLIsReportedOnce: TRACEPAD_URL is read when the server is
// built, so a value that will not parse is said once, not on every sign-in
// and cookie write that consults it.
func TestAMalformedURLIsReportedOnce(t *testing.T) {
	h := newAccountHarness(t)
	var (
		mu     sync.Mutex
		logged bytes.Buffer
	)
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, out: &logged}, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	h.server.setPublicURL("tracepad.example.com")
	for range 3 {
		h.call(t, "POST", "/api/v1/auth/login",
			mustJSON(t, map[string]any{"email": "a@b.c", "password": testAccountPassword}),
			anonymous, asJSON, func(r *http.Request) { r.Header.Set("Origin", "https://elsewhere.example") })
		r := httptest.NewRequest("POST", "/", nil)
		h.server.ownOrigin(r, "http://"+r.Host)
		h.server.originFor(r)
		if got := h.server.configuredOrigin(); got != "" {
			t.Errorf("configuredOrigin = %q for a malformed TRACEPAD_URL, want none", got)
		}
	}
	mu.Lock()
	if n := strings.Count(logged.String(), "TRACEPAD_URL is not an http or https URL"); n != 1 {
		t.Errorf("a malformed TRACEPAD_URL was reported %d times, want once:\n%s", n, logged.String())
	}
	mu.Unlock()

	// A scheme a browser does not load this server over is not an address
	// of it: ignored, and an http page on the host is judged as it would
	// be with nothing configured.
	for _, raw := range []string{"htps://tracepad.example", "ws://tracepad.example"} {
		h.server.setPublicURL(raw)
		if got := h.server.configuredOrigin(); got != "" {
			t.Errorf("TRACEPAD_URL %q was taken as %q, want it ignored", raw, got)
		}
		r := httptest.NewRequest("POST", "/", nil)
		r.Host = "tracepad.example"
		if !h.server.ownOrigin(r, "http://tracepad.example") {
			t.Errorf("TRACEPAD_URL %q: an http page on the host was refused", raw)
		}
	}
}

// TestTheStreamRefusesWhatTheReadAPIWould: a soft-deleted project's key names
// a project, but not one the read API serves, so the stream is 401 for it as
// for no key at all, without lifting a deadline or running anything. A store
// that cannot answer is 503 and no challenge: the key may be fine.
func TestTheStreamRefusesWhatTheReadAPIWould(t *testing.T) {
	h := newAccountHarness(t)
	var ran bool
	stream := h.server.mcpStream(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true }))
	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
		r.Header.Set("Authorization", "Bearer "+testSecret)
		rec := httptest.NewRecorder()
		stream.ServeHTTP(rec, r)
		return rec
	}

	expectStatus(t, h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"?confirm="+h.project.Name, nil, asAdmin), http.StatusAccepted)
	if stored, _ := h.store.ProjectByID(t.Context(), h.project.ID); stored == nil || !stored.Deleted() {
		t.Fatal("the project was not soft-deleted")
	}
	rec := call()
	expectStatus(t, rec, http.StatusUnauthorized)
	if rec.Header().Get("WWW-Authenticate") == "" || ran {
		t.Errorf("a deleted project's key: challenge %q, handler ran = %v; want a challenge and nothing run",
			rec.Header().Get("WWW-Authenticate"), ran)
	}

	h.store.Close()
	rec = call()
	expectStatus(t, rec, http.StatusServiceUnavailable)
	if rec.Header().Get("WWW-Authenticate") != "" || ran {
		t.Errorf("a store that cannot answer: challenge %q, handler ran = %v; want neither",
			rec.Header().Get("WWW-Authenticate"), ran)
	}
}

// TestTheStreamWantsAKeyThatMayRead: every MCP tool is a read, so a key minted
// without the read scope is refused before its stream lifts a deadline, not on
// each tool call after — with the guard's answer, which names the scope it
// lacks (spec 045 #7, #13).
func TestTheStreamWantsAKeyThatMayRead(t *testing.T) {
	h := newAccountHarness(t)
	ingestOnly := h.mint(t, "ingest").SecretKey
	var ran bool
	stream := h.server.mcpStream(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true }))
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+ingestOnly)
	rec := httptest.NewRecorder()
	stream.ServeHTTP(rec, r)
	expectError(t, rec, http.StatusForbidden, "this key's scopes are ingest; POST /mcp needs read")
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer error="insufficient_scope", scope="read"` || ran {
		t.Errorf("an ingest-only key: challenge %q, handler ran = %v; want the scope named and nothing run", got, ran)
	}
}

// TestNoHandlerStartsOnceTheStopIsWaiting: a connection goroutine that had
// read its request before the stop closed the connection reaches the handler
// late; once the wait has begun it is answered 503 and never runs, so the
// wait's zero stays zero.
func TestNoHandlerStartsOnceTheStopIsWaiting(t *testing.T) {
	var f inflight
	var ran bool
	handler := f.track(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true }))
	if err := f.wait(t.Context()); err != nil {
		t.Fatalf("wait with nothing running = %v", err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/traces", nil))
	expectStatus(t, rec, http.StatusServiceUnavailable)
	if ran || f.count() != 0 {
		t.Errorf("after the wait began: handler ran = %v, running = %d; want neither", ran, f.count())
	}

	// The server's own: that 503 is a response like any other, and
	// carries the headers every response does.
	h := newHarness(t, nil, store.WriterOptions{})
	if err := h.server.running.wait(t.Context()); err != nil {
		t.Fatalf("wait with nothing running = %v", err)
	}
	rec = h.get(t, "/api/v1/traces")
	expectStatus(t, rec, http.StatusServiceUnavailable)
	for _, pair := range securityHeaders {
		if got := rec.Header().Get(pair[0]); got != pair[1] {
			t.Errorf("the stopping 503: %s = %q, want %q", pair[0], got, pair[1])
		}
	}
}

// TestAStopEndsTheMCPStreamAtOnce: an MCP client holding its event stream open
// does not keep a stop waiting for the whole drain window — the stream's
// context ends when the stop begins — while a request that is not a stream is
// left to finish with its context intact.
func TestAStopEndsTheMCPStreamAtOnce(t *testing.T) {
	h := newHarness(t, &config.Config{Listen: ":0", StoreRaw: true, MaxBodyBytes: config.DefaultMaxBodyBytes,
		MCP: true}, store.WriterOptions{})
	streaming := make(chan struct{})
	streamEnded := make(chan struct{})
	working := make(chan struct{})
	var workCtxErr atomic.Value
	mux := http.NewServeMux()
	mux.Handle("/mcp", h.server.mcpStream(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		http.NewResponseController(w).Flush()
		close(streaming)
		<-r.Context().Done()
		close(streamEnded)
	})))
	mux.HandleFunc("/work", func(w http.ResponseWriter, r *http.Request) {
		close(working)
		time.Sleep(300 * time.Millisecond)
		workCtxErr.Store(fmt.Sprint(r.Context().Err()))
	})
	h.server.http.Handler = h.server.withResponseHeaders(h.server.running.track(mux))
	listener := mustListen(t)
	go h.server.http.Serve(listener)
	base := "http://" + listener.Addr().String()

	stream, _ := http.NewRequest("GET", base+"/mcp", nil)
	stream.Header.Set("Authorization", "Bearer "+testSecret)
	response, err := http.DefaultClient.Do(stream)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	for _, pair := range [][2]string{{"Cache-Control", "private, no-store"}, {"Vary", "Authorization, Cookie, X-Tracepad-Project"}} {
		if got := response.Header.Get(pair[0]); got != pair[1] {
			t.Errorf("the MCP stream: %s = %q, want %q", pair[0], got, pair[1])
		}
	}
	<-streaming
	go http.Get(base + "/work")
	<-working

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	begun := time.Now()
	if err := h.server.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown = %v, want a clean stop", err)
	}
	if took := time.Since(begun); took > 2500*time.Millisecond {
		t.Errorf("the stop took %s with a stream open, want well inside the five-second drain", took)
	}
	select {
	case <-streamEnded:
	default:
		t.Error("the MCP stream was still open after the stop")
	}
	if got := workCtxErr.Load(); got != "<nil>" {
		t.Errorf("the request that was not a stream saw its context end with %v, want it left to finish", got)
	}
}

// acceptOnce is a listener whose second Accept fails for good, the way a
// listener that has lost its socket does: Serve returns while the connection
// it accepted first is still being served.
type acceptOnce struct {
	net.Listener
	accepted atomic.Bool
}

var errAcceptFailed = errors.New("the listener stopped accepting")

func (a *acceptOnce) Accept() (net.Conn, error) {
	if a.accepted.Swap(true) {
		return nil, errAcceptFailed
	}
	return a.Listener.Accept()
}

// TestAServeThatFailsStillWaitsForItsHandlers: when Serve returns on its own
// with a handler still running, the stop that follows (what the process does
// on that path out) waits for the handler, so the writer closed after it is
// one the handler has finished with.
func TestAServeThatFailsStillWaitsForItsHandlers(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	release := make(chan struct{})
	var finished atomic.Bool
	_, served := serveTrackedOn(t, h, &acceptOnce{Listener: mustListen(t)}, func(w http.ResponseWriter, r *http.Request) {
		<-release
		finished.Store(true)
	})
	if err := <-served; !errors.Is(err, errAcceptFailed) {
		t.Fatalf("Serve = %v, want the accept failure", err)
	}
	time.AfterFunc(200*time.Millisecond, func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.server.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown after a failed Serve = %v, want a clean stop once the handler returned", err)
	}
	if !finished.Load() {
		t.Error("Shutdown returned before the handler Serve left running had finished")
	}
}

// TestTheLastKeyUseWriteKeepsToTheStopsBudget: at most a second, never more
// than the handlers' grace has left, and never under a quarter of a second.
func TestTheLastKeyUseWriteKeepsToTheStopsBudget(t *testing.T) {
	within := func(left time.Duration) time.Duration {
		ctx, cancel := context.WithTimeout(context.Background(), left)
		defer cancel()
		return keyUseFlushTime(ctx)
	}
	if got := within(3 * time.Second); got != time.Second {
		t.Errorf("with the whole grace left: %s, want a second", got)
	}
	if got := within(500 * time.Millisecond); got > 500*time.Millisecond || got < 400*time.Millisecond {
		t.Errorf("with half a second left: %s, want what is left", got)
	}
	if got := within(-time.Second); got != 250*time.Millisecond {
		t.Errorf("with the grace spent: %s, want a quarter of a second", got)
	}
}

// TestShutdownWaitsOnEveryPathOut: a Shutdown that fails for a reason of its
// own still waits for the handlers before it returns, and reports both.
func TestShutdownWaitsOnEveryPathOut(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	var finished atomic.Bool
	serveTrackedOn(t, h, failingClose{mustListen(t)}, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		finished.Store(true)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The request is in flight, so Shutdown fails on the listener first and
	// only then would wait for the connection; either way, not before the
	// handler is done.
	err := h.server.Shutdown(ctx)
	if !finished.Load() {
		t.Error("Shutdown returned before the handler did")
	}
	if !errors.Is(err, errCloseFailed) {
		t.Errorf("Shutdown = %v, want the listener's error", err)
	}
}

// TestInflightWaitsForTheLastHandler: wait returns the moment the last running
// handler does (at zero it is immediate: TestNoHandlerStartsOnceTheStopIsWaiting).
func TestInflightWaitsForTheLastHandler(t *testing.T) {
	var f inflight
	release := make(chan struct{})
	handler := f.track(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	for range 2 {
		go handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}
	for f.count() < 2 {
		time.Sleep(time.Millisecond)
	}
	short, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := f.wait(short); !errors.Is(err, errHandlersStuck) {
		t.Fatalf("wait with two running = %v, want errHandlersStuck", err)
	}
	close(release)
	done, cancelDone := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelDone()
	if err := f.wait(done); err != nil {
		t.Fatalf("wait after both returned = %v", err)
	}
}
