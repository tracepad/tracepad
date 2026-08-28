package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/tracepad/tracepad/internal/config"
)

func TestHealth(t *testing.T) {
	srv := New(&config.Config{Listen: ":0"}, "test-version", nil, nil, nil)
	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if body["status"] != "ok" || body["version"] != "test-version" {
		t.Fatalf("body = %v", body)
	}
}
