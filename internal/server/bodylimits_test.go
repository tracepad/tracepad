package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/store"
)

// Body limits (spec 028 Decision 26, spec 002 Decision 27). A route anyone can
// call takes a few KiB of uncompressed JSON and nothing else; ingest caps the
// body the parser gets, decompressed, at the configured cap.

// countingReader is a body that never ends and remembers how much of it was
// read — the proof that a refusal came before the body was taken in whole.
type countingReader struct{ read int64 }

func (c *countingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	c.read += int64(len(p))
	return len(p), nil
}

func gzipped(t *testing.T, body []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// publicBodyRoutes is every route the guard lets through without a credential
// that can carry a body. The presigned media upload is not one of them: its
// policy says its token is its credential and its limits are its own (spec
// 041 #14; asserted in TestMediaUploadChecksTheTokenBeforeTheBody).
func publicBodyRoutes(t *testing.T, h *harness) []route {
	t.Helper()
	var routes []route
	for _, rt := range h.server.routes() {
		if rt.Policy != public || rt.Method == "GET" || rt.Method == "HEAD" {
			continue
		}
		routes = append(routes, rt)
	}
	if len(routes) < 3 {
		t.Fatalf("found %d public routes with a body, want at least setup, login and accept-invite", len(routes))
	}
	return routes
}

// guardedStub is a public route's guard with a handler that only records
// whether it ran, so a refusal can be shown to be the router's own.
func guardedStub(h *harness, rt route, reached *bool, read *error) http.Handler {
	rt.handler = func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		_, *read = io.ReadAll(r.Body)
	}
	return h.server.guard(rt)
}

// TestPublicRoutesRefuseCompressedBodies walks the table, so a public route
// added later is held to the rule without anybody remembering to list it. The
// refusal is the guard's: the handler behind it never runs.
func TestPublicRoutesRefuseCompressedBodies(t *testing.T) {
	h := newAccountHarness(t)
	body := gzipped(t, mustJSON(t, map[string]any{
		"email": "someone@example.com", "password": testAccountPassword, "token": "t",
	}))
	encodings := map[string][]string{
		"gzip":               {"gzip"},
		"upper case":         {"GZIP"},
		"deflate":            {"deflate"},
		"br":                 {"br"},
		"a list":             {"identity, gzip"},
		"a second header":    {"identity", "gzip"},
		"identity then junk": {"identity", "x-custom"},
	}
	for _, rt := range publicBodyRoutes(t, h) {
		for name, values := range encodings {
			set := func(r *http.Request) {
				r.Header.Del("Content-Encoding")
				for _, v := range values {
					r.Header.Add("Content-Encoding", v)
				}
			}
			rec := h.call(t, rt.Method, rt.Path, body, anonymous, set)
			expectError(t, rec, http.StatusUnsupportedMediaType, "uncompressed")

			var reached bool
			var read error
			req := httptest.NewRequest(rt.Method, rt.Path, bytes.NewReader(body))
			set(req)
			rec = httptest.NewRecorder()
			guardedStub(h, rt, &reached, &read).ServeHTTP(rec, req)
			if rec.Code != http.StatusUnsupportedMediaType || reached {
				t.Errorf("%s %s, %s: status %d, handler reached = %v; want 415 from the guard",
					rt.Method, rt.Path, name, rec.Code, reached)
			}
		}
		// The one coding that is not one passes the guard.
		var reached bool
		var read error
		req := httptest.NewRequest(rt.Method, rt.Path, bytes.NewReader([]byte("{}")))
		req.Header.Add("Content-Encoding", "identity")
		req.Header.Add("Content-Encoding", "Identity, identity")
		rec := httptest.NewRecorder()
		guardedStub(h, rt, &reached, &read).ServeHTTP(rec, req)
		if !reached || read != nil {
			t.Errorf("%s %s: identity did not reach the handler whole (reached %v, read error %v)",
				rt.Method, rt.Path, reached, read)
		}
	}
}

func TestPublicRoutesRefuseLargeBodiesUnread(t *testing.T) {
	h := newAccountHarness(t)
	for _, rt := range publicBodyRoutes(t, h) {
		body := &countingReader{}
		req := httptest.NewRequest(rt.Method, rt.Path, body)
		rec := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(rec, req)
		expectError(t, rec, http.StatusRequestEntityTooLarge, "too large")
		// MaxBytesReader reads one byte past the cap to know it is past
		// it; the reads come in the buffer sizes io.ReadAll asks for, so
		// the bound is the cap plus one buffer, never the body.
		if body.read > 2*maxPublicBodyBytes {
			t.Errorf("%s %s read %d bytes of an endless body, want at most about %d",
				rt.Method, rt.Path, body.read, maxPublicBodyBytes)
		}

		// The cap is the guard's: whatever the handler reads with is cut
		// off at it, even a reader of its own.
		var reached bool
		var read error
		endless := &countingReader{}
		guardedStub(h, rt, &reached, &read).ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(rt.Method, rt.Path, endless))
		var tooLarge *http.MaxBytesError
		if !errors.As(read, &tooLarge) || tooLarge.Limit != maxPublicBodyBytes {
			t.Errorf("%s %s: a handler behind the guard read with error %v, want the %d-byte cap",
				rt.Method, rt.Path, read, maxPublicBodyBytes)
		}

		// Just under the cap is read and judged on its content: the
		// padding is JSON whitespace, so the answer is the route's own.
		padded := append(mustJSON(t, map[string]any{"email": "a@b.c", "password": "x"}),
			bytes.Repeat([]byte(" "), maxPublicBodyBytes-100)...)
		rec = h.call(t, rt.Method, rt.Path, padded, anonymous)
		if rec.Code == http.StatusRequestEntityTooLarge || rec.Code == http.StatusUnsupportedMediaType {
			t.Errorf("%s %s refused a body under the cap: %d %s", rt.Method, rt.Path, rec.Code, rec.Body)
		}
	}
}

// TestPublicRoutesStillSignIn: the three ways in work as before, including
// with the one encoding that is not an encoding.
func TestPublicRoutesStillSignIn(t *testing.T) {
	h := newAccountHarness(t)
	token := strings.TrimPrefix(h.server.SetupURL(), h.server.originForPrint()+"/setup#token=")
	rec := h.call(t, "POST", "/api/v1/setup", mustJSON(t, map[string]any{
		"token": token, "email": "founder@example.com", "password": testAccountPassword,
		"name": strings.Repeat("n", maxAccountNameLength),
	}), anonymous, func(r *http.Request) { r.Header.Set("Content-Encoding", "identity") })
	expectStatus(t, rec, http.StatusCreated)

	rec = h.call(t, "POST", "/api/v1/auth/login", mustJSON(t, map[string]any{
		"email": "founder@example.com", "password": testAccountPassword,
	}), anonymous, func(r *http.Request) { r.Header.Set("Content-Encoding", "identity") })
	expectStatus(t, rec, http.StatusOK)

	h.invited(t, "invitee@example.com", false)
	rec = h.call(t, "POST", "/api/v1/auth/accept-invite", mustJSON(t, map[string]any{
		"token": "token-for-invitee@example.com", "password": testAccountPassword,
	}), anonymous)
	expectStatus(t, rec, http.StatusOK)
	if sessionCookieOf(rec) == nil {
		t.Error("accepting an invitation must sign the person in")
	}
}

// TestLoginKeepsNoOverlongKey: an email no account can have is answered
// before the limiter, so the limiter never holds a string the caller sized.
func TestLoginKeepsNoOverlongKey(t *testing.T) {
	h := newAccountHarness(t)
	h.owner(t)
	long := strings.Repeat("a", maxEmailLength) + "@example.com"
	for range loginFailureLimit + 1 {
		expectError(t, h.login(t, long, testAccountPassword), http.StatusUnauthorized, wrongCredentials)
	}
	expectError(t, h.login(t, "owner@example.com", strings.Repeat("p", store.MaxPasswordLength+1)),
		http.StatusUnauthorized, wrongCredentials)
	h.server.limiter.mu.Lock()
	defer h.server.limiter.mu.Unlock()
	for key := range h.server.limiter.failures {
		if len(key) > maxEmailLength {
			t.Errorf("the limiter keeps a %d-byte key", len(key))
		}
	}
}

// TestAccountNameHasACeiling: 200 characters, counted as characters — a
// Cyrillic name has the room a Latin one has — after trimming, on every
// route that sets a name.
func TestAccountNameHasACeiling(t *testing.T) {
	h := newAccountHarness(t)
	token := strings.TrimPrefix(h.server.SetupURL(), h.server.originForPrint()+"/setup#token=")
	setup := func(name string) *httptest.ResponseRecorder {
		return h.call(t, "POST", "/api/v1/setup", mustJSON(t, map[string]any{
			"token": token, "email": "founder@example.com", "password": testAccountPassword,
			"name": name,
		}), anonymous)
	}
	expectError(t, setup(strings.Repeat("n", maxAccountNameLength+1)),
		http.StatusUnprocessableEntity, "name must be at most")
	expectError(t, setup(strings.Repeat("\u0436", maxAccountNameLength+1)),
		http.StatusUnprocessableEntity, "name must be at most")
	cyrillic := strings.Repeat("\u0436", maxAccountNameLength)
	rec := setup("  " + cyrillic + "  ")
	expectStatus(t, rec, http.StatusCreated)
	if got := decodeJSON[struct {
		Account struct {
			Name string `json:"name"`
		} `json:"account"`
	}](t, rec).Account.Name; got != cyrillic {
		t.Errorf("setup stored a %d-character name, want the trimmed %d", len([]rune(got)), maxAccountNameLength)
	}

	owner := h.owner(t)
	long := strings.Repeat("n", maxAccountNameLength+1)
	expectError(t, h.call(t, "POST", "/api/v1/accounts",
		mustJSON(t, map[string]any{"email": "new@example.com", "name": long}), asSession(owner)),
		http.StatusUnprocessableEntity, "name must be at most")
	expectError(t, h.call(t, "PATCH", "/api/v1/accounts/"+owner.account.ID,
		mustJSON(t, map[string]any{"name": long}), asSession(owner)),
		http.StatusUnprocessableEntity, "name must be at most")
	expectError(t, h.call(t, "PATCH", "/api/v1/auth/me",
		mustJSON(t, map[string]any{"name": long}), asSession(owner)),
		http.StatusUnprocessableEntity, "name must be at most")

	// Both PATCH routes trim before they measure and before they store,
	// as create and setup do: the spaces are not the name.
	padded := "   " + strings.Repeat("n", maxAccountNameLength-1) + "   "
	for _, path := range []string{"/api/v1/accounts/" + owner.account.ID, "/api/v1/auth/me"} {
		rec := h.call(t, "PATCH", path, mustJSON(t, map[string]any{"name": padded}), asSession(owner))
		expectStatus(t, rec, http.StatusOK)
		got := decodeJSON[struct {
			Account struct {
				Name string `json:"name"`
			} `json:"account"`
		}](t, rec).Account.Name
		if got != strings.TrimSpace(padded) {
			t.Errorf("PATCH %s stored %q, want it trimmed", path, got)
		}
	}
}

// TestMediaUploadChecksTheTokenBeforeTheBody: the presigned PUT is public,
// and the token in its URL is the whole of its authorization — so a PUT
// without a good one must be refused having read nothing, and a compressed
// body is bytes to hash, not something to expand (spec 041 #14).
func TestMediaUploadChecksTheTokenBeforeTheBody(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	picture := testPicture(30000, 7)
	sum := sha256.Sum256(picture)
	hash := base64.StdEncoding.EncodeToString(sum[:])
	mediaID, upload := h.langfuseAsk(t, picture, probeTrace, testSecret)
	if upload == nil {
		t.Fatal("no upload URL")
	}

	for name, path := range map[string]string{
		"no token":     "/api/public/media/" + mediaID + "/upload",
		"forged token": "/api/public/media/" + mediaID + "/upload?token=" + url.QueryEscape("forged"),
	} {
		body := &countingReader{}
		req := httptest.NewRequest("PUT", path, body)
		rec := httptest.NewRecorder()
		h.server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusForbidden {
			t.Errorf("%s: status = %d", name, rec.Code)
		}
		if body.read != 0 {
			t.Errorf("%s: read %d bytes of the body before refusing", name, body.read)
		}
	}

	// A good token caps the body at the declared length, not at the
	// server's cap.
	parsed, _ := url.Parse(*upload)
	body := &countingReader{}
	req := httptest.NewRequest("PUT", parsed.RequestURI(), body)
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	expectStatus(t, rec, http.StatusRequestEntityTooLarge)
	if body.read > 2*int64(len(picture)) {
		t.Errorf("read %d bytes for a %d-byte grant", body.read, len(picture))
	}

	// gzip is not undone: the compressed bytes do not hash to the grant.
	req, _ = http.NewRequest("PUT", parsed.RequestURI(), bytes.NewReader(gzipped(t, picture)))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("x-amz-checksum-sha256", hash)
	rec = httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	expectStatus(t, rec, http.StatusBadRequest)
	if h.mediaHeld(t, hexSHA(picture)) {
		t.Error("a compressed upload was stored")
	}
	if code := h.langfusePut(t, *upload, picture, hash); code != http.StatusOK {
		t.Errorf("the plain upload = %d, want 200", code)
	}
}

// TestIngestCapsTheDecompressedBody: the cap is on what the parser gets. A
// gzip body a thousand times smaller than the cap still may not expand past
// it, and one that expands to exactly the cap is read (and judged by the
// decoder).
func TestIngestCapsTheDecompressedBody(t *testing.T) {
	const limit = 1 << 20
	h := newHarness(t, &config.Config{Listen: ":0", StoreRaw: true, MaxBodyBytes: limit},
		store.WriterOptions{})
	gzipHeader := func(r *http.Request) { r.Header.Set("Content-Encoding", "gzip") }

	bomb := gzipped(t, make([]byte, limit+1))
	if len(bomb) > limit/100 {
		t.Fatalf("the bomb is %d bytes on the wire; the test wants it far under the cap", len(bomb))
	}
	expectError(t, h.post(t, "/v1/traces", bomb, gzipHeader), http.StatusRequestEntityTooLarge, "too large")
	expectError(t, h.post(t, "/api/public/otel/v1/traces", bomb, gzipHeader), http.StatusRequestEntityTooLarge, "too large")
	// Two bombs inside the minute: one warning, and the second is
	// counted for the next one to report.
	h.server.inflatedLog.mu.Lock()
	held := h.server.inflatedLog.skipped
	h.server.inflatedLog.mu.Unlock()
	if held != 1 {
		t.Errorf("warnings held back = %d after two refusals in a minute, want 1", held)
	}

	// At the cap exactly it is read; zeros are not an export.
	expectStatus(t, h.post(t, "/v1/traces", gzipped(t, make([]byte, limit)), gzipHeader), http.StatusBadRequest)

	// A real compressed export goes through.
	expectStatus(t, h.post(t, "/v1/traces", gzipped(t, fixtureBody(t, "002-genai-semconv-chat")), gzipHeader), http.StatusOK)

	// The authenticated JSON API shares the reader, and so the bound.
	expectError(t, h.call(t, "POST", "/api/v1/scores", gzipped(t, make([]byte, limit+1)), gzipHeader),
		http.StatusRequestEntityTooLarge, "too large")
}

func TestLogLimiterPacesAndCounts(t *testing.T) {
	l := &logLimiter{every: time.Minute}
	start := time.Unix(1_000_000, 0)
	if skipped, ok := l.allow(start); !ok || skipped != 0 {
		t.Fatalf("first = (%d, %v), want (0, true)", skipped, ok)
	}
	for i := range 3 {
		if _, ok := l.allow(start.Add(time.Duration(i+1) * time.Second)); ok {
			t.Fatalf("call %d inside the minute was let through", i+2)
		}
	}
	if skipped, ok := l.allow(start.Add(time.Minute)); !ok || skipped != 3 {
		t.Fatalf("a minute later = (%d, %v), want (3, true)", skipped, ok)
	}
}
