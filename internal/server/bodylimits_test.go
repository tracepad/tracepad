package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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
// that takes a body, bar the presigned media upload, whose token in the URL is
// its credential and which has limits of its own (spec 041 #14; asserted in
// TestMediaUploadChecksTheTokenBeforeTheBody).
func publicBodyRoutes(t *testing.T, h *harness) []route {
	t.Helper()
	var routes []route
	for _, rt := range h.server.routes() {
		if rt.Policy != public || rt.Method == "GET" || rt.Method == "HEAD" {
			continue
		}
		if rt.Path == "/api/public/media/{mediaId}/upload" {
			continue
		}
		routes = append(routes, rt)
	}
	if len(routes) < 3 {
		t.Fatalf("found %d public routes with a body, want at least setup, login and accept-invite", len(routes))
	}
	return routes
}

// TestPublicRoutesRefuseCompressedBodies walks the table, so a public route
// added later is held to the rule without anybody remembering to list it.
func TestPublicRoutesRefuseCompressedBodies(t *testing.T) {
	h := newAccountHarness(t)
	body := gzipped(t, mustJSON(t, map[string]any{
		"email": "someone@example.com", "password": testAccountPassword, "token": "t",
	}))
	for _, rt := range publicBodyRoutes(t, h) {
		for _, encoding := range []string{"gzip", "GZIP", "deflate", "br"} {
			rec := h.call(t, rt.Method, rt.Path, body, anonymous,
				func(r *http.Request) { r.Header.Set("Content-Encoding", encoding) })
			expectError(t, rec, http.StatusUnsupportedMediaType, "uncompressed")
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

func TestAccountNameHasACeiling(t *testing.T) {
	h := newAccountHarness(t)
	token := strings.TrimPrefix(h.server.SetupURL(), h.server.originForPrint()+"/setup#token=")
	rec := h.call(t, "POST", "/api/v1/setup", mustJSON(t, map[string]any{
		"token": token, "email": "founder@example.com", "password": testAccountPassword,
		"name": strings.Repeat("n", maxAccountNameLength+1),
	}), anonymous)
	expectError(t, rec, http.StatusUnprocessableEntity, "name must be at most")

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

	// At the cap exactly it is read; zeros are not an export.
	expectStatus(t, h.post(t, "/v1/traces", gzipped(t, make([]byte, limit)), gzipHeader), http.StatusBadRequest)

	// A real compressed export goes through.
	expectStatus(t, h.post(t, "/v1/traces", gzipped(t, fixtureBody(t, "002-genai-semconv-chat")), gzipHeader), http.StatusOK)

	// The authenticated JSON API shares the reader, and so the bound.
	expectError(t, h.call(t, "POST", "/api/v1/scores", gzipped(t, make([]byte, limit+1)), gzipHeader),
		http.StatusRequestEntityTooLarge, "too large")
}
