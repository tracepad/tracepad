package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// Shared plumbing for the JSON API tests (spec 003, Testing #2): the same
// in-process harness the ingest tests use, driven with JSON bodies.
//
// None of these helpers sets a Content-Type. That is deliberate: the API does
// not require one (Decision 19), and a test suite that always sent the perfect
// header would never notice if it started to.

// captureLogs redirects slog for the duration of one test and replays what was
// logged if it fails. A handler answers a failed write with a flat 500 — on
// purpose, since the client can do nothing with the detail — so the reason a
// write failed lives only in the log, which is exactly what a rare failure
// needs to leave behind. Tests within a package run sequentially, so swapping
// the default logger is safe here.
func captureLogs(t testing.TB) {
	t.Helper()
	var (
		mu       sync.Mutex
		recorded bytes.Buffer
	)
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{mu: &mu, out: &recorded}, nil)))
	t.Cleanup(func() {
		slog.SetDefault(previous)
		mu.Lock()
		defer mu.Unlock()
		if t.Failed() && recorded.Len() > 0 {
			t.Logf("server log:\n%s", recorded.String())
		}
	})
}

type lockedWriter struct {
	mu  *sync.Mutex
	out *bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out.Write(p)
}

// call sends a raw body, so that malformed JSON can be tested too.
func (h *harness) call(t *testing.T, method, path string, body []byte, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Authorization", "Bearer "+testSecret)
	for _, m := range mutate {
		m(req)
	}
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	return rec
}

// send marshals a value and sends it.
func (h *harness) send(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	if body == nil {
		return h.call(t, method, path, nil)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return h.call(t, method, path, raw)
}

// get is the read half of the same thing.
func (h *harness) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	return h.call(t, "GET", path, nil)
}

// decodeJSON reads a response body into T, failing the test if it does not fit.
func decodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not the expected JSON: %v (body: %s)", err, rec.Body)
	}
	return out
}

// expectStatus fails with the body attached, which is where the reason is.
func expectStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, want, rec.Body)
	}
}

// expectError asserts a status and that the message names what went wrong.
//
// The envelope is read loosely because a refusal may carry more than its
// sentence: an optimistic append answers `409` with the version the name is
// actually at beside the `error` (spec 021 #14), and a helper that insisted on
// a map of strings would fail on the number rather than on the assertion.
func expectError(t *testing.T, rec *httptest.ResponseRecorder, want int, fragment string) {
	t.Helper()
	expectStatus(t, rec, want)
	message, _ := decodeJSON[map[string]any](t, rec)["error"].(string)
	if !strings.Contains(message, fragment) {
		t.Fatalf("error = %q, want it to mention %q", message, fragment)
	}
}
