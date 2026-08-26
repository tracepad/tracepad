package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Shared plumbing for the JSON API tests (spec 003, Testing #2): the same
// in-process harness the ingest tests use, driven with JSON bodies.
//
// None of these helpers sets a Content-Type. That is deliberate: the API does
// not require one (Decision 19), and a test suite that always sent the perfect
// header would never notice if it started to.

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
func expectError(t *testing.T, rec *httptest.ResponseRecorder, want int, fragment string) {
	t.Helper()
	expectStatus(t, rec, want)
	message := decodeJSON[map[string]string](t, rec)["error"]
	if !strings.Contains(message, fragment) {
		t.Fatalf("error = %q, want it to mention %q", message, fragment)
	}
}
