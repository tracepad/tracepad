package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/otlptest"
	"github.com/tracepad/tracepad/internal/store"
)

// Ingest end-to-end (spec 002, Testing #2): real protobuf bodies through the
// real handler into a real database, asserted by reading the rows back.

const (
	testSecret = "tp-sk-test-secret"
	testPublic = "tp-pk-test"
)

type harness struct {
	server  *Server
	store   *store.Store
	writer  *store.Writer
	project *store.Project
}

func newHarness(t *testing.T, cfg *config.Config, writerOpts store.WriterOptions) *harness {
	t.Helper()

	st, err := store.Open(filepath.Join(t.TempDir(), "tracepad.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	project, err := st.CreateProject("test", store.KeyPair{PublicKey: testPublic, Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := st.NewWriter(writerOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })

	if cfg == nil {
		cfg = &config.Config{Listen: ":0", StoreRaw: true, MaxBodyBytes: config.DefaultMaxBodyBytes}
	}
	return &harness{
		server:  New(cfg, "test", st, writer),
		store:   st,
		writer:  writer,
		project: project,
	}
}

// post sends a body the way an OTLP exporter would.
func (h *harness) post(t *testing.T, path string, body []byte, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Authorization", "Bearer "+testSecret)
	for _, m := range mutate {
		m(req)
	}
	rec := httptest.NewRecorder()
	h.server.Handler().ServeHTTP(rec, req)
	return rec
}

func fixtureBody(t *testing.T, name string) []byte {
	t.Helper()
	for _, fixture := range otlptest.Fixtures() {
		if fixture.Name != name {
			continue
		}
		body, err := mapping.EncodeExportRequest(fixture.ResourceSpans)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	t.Fatalf("unknown fixture %q", name)
	return nil
}

// A well-formed export lands as rows, with the trace fields merged out of the
// spans that carried them and the aggregates computed in the same commit.
func TestIngestStoresTraceAndObservations(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.post(t, "/v1/traces", fixtureBody(t, "001-langfuse-sdk-generation"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/x-protobuf" {
		t.Errorf("Content-Type = %q", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a fully accepted export should answer with an empty message, got %d bytes", rec.Body.Len())
	}

	const traceID = "4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f"
	trace, err := h.store.Trace(h.project.ID, traceID)
	if err != nil || trace == nil {
		t.Fatalf("trace = %v, err = %v", trace, err)
	}
	if trace.Name != "support-chat" || trace.UserID != "user-4821" || trace.SessionID != "session-77" {
		t.Errorf("trace fields = %+v", trace)
	}
	if trace.Environment != "production" {
		t.Errorf("environment = %q", trace.Environment)
	}
	if len(trace.Tags) != 2 || trace.Tags[0] != "support" {
		t.Errorf("tags = %v", trace.Tags)
	}
	if trace.Metadata["channel"] != "web" {
		t.Errorf("metadata = %v", trace.Metadata)
	}
	if trace.ObservationCount != 2 || trace.ErrorCount != 0 {
		t.Errorf("counts = %d observations, %d errors", trace.ObservationCount, trace.ErrorCount)
	}
	// Aggregates come from the observations written in the same commit.
	if trace.TotalCost == nil || *trace.TotalCost < 0.0009 || *trace.TotalCost > 0.0011 {
		t.Errorf("total_cost = %v", trace.TotalCost)
	}
	if trace.LatencyMs == nil || *trace.LatencyMs != 820 {
		t.Errorf("latency_ms = %v", trace.LatencyMs)
	}
	if trace.Timestamp != 1787738400000000000 {
		t.Errorf("timestamp = %d", trace.Timestamp)
	}

	observations, err := h.store.Observations(h.project.ID, traceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 2 {
		t.Fatalf("observations = %d", len(observations))
	}
	generation := observations[1]
	if generation.Type != "generation" || generation.Model != "claude-sonnet-5" {
		t.Errorf("generation = %+v", generation)
	}
	if !generation.ProvidedCost {
		t.Error("provided_cost should be set when the client sent cost details")
	}
	if generation.Usage["input"] != float64(128) {
		t.Errorf("usage = %v", generation.Usage)
	}
	input, ok := generation.Input.([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input payload = %#v", generation.Input)
	}
	if generation.ParentObservationID != "1a2b3c4d5e6f7a8b" {
		t.Errorf("parent = %q", generation.ParentObservationID)
	}
}

// Re-delivery is normal, not exceptional: exporters retry, and the natural
// key makes the second delivery a no-op for the counts (spec 002 #5, #7).
func TestIngestIsIdempotent(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	body := fixtureBody(t, "001-langfuse-sdk-generation")

	for i := range 3 {
		if rec := h.post(t, "/v1/traces", body); rec.Code != http.StatusOK {
			t.Fatalf("delivery %d: status = %d", i, rec.Code)
		}
	}

	trace, err := h.store.Trace(h.project.ID, "4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f")
	if err != nil || trace == nil {
		t.Fatalf("trace = %v, err = %v", trace, err)
	}
	if trace.ObservationCount != 2 {
		t.Errorf("observation_count after 3 deliveries = %d, want 2", trace.ObservationCount)
	}
	if trace.TotalCost == nil || *trace.TotalCost > 0.0011 {
		t.Errorf("total_cost after 3 deliveries = %v, want the single-delivery value", trace.TotalCost)
	}
}

// Spans arriving in separate exports, children before parents, still
// assemble: trace fields merge field-wise and the tree is built at read time
// (spec 002 #6, edge cases).
func TestIngestMergesAcrossBatches(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	fixture := otlptest.Fixtures()[4] // 005-cross-resource-tree
	if fixture.Name != "005-cross-resource-tree" {
		t.Fatalf("fixture order changed: %s", fixture.Name)
	}
	for _, rs := range fixture.ResourceSpans {
		body, err := mapping.EncodeExportRequest(rs2slice(rs))
		if err != nil {
			t.Fatal(err)
		}
		if rec := h.post(t, "/v1/traces", body); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
		}
	}

	trace, err := h.store.Trace(h.project.ID, "dd44ee55ff6677008899001122aabb33")
	if err != nil || trace == nil {
		t.Fatalf("trace = %v, err = %v", trace, err)
	}
	// The children arrived first; the name came with the root, later.
	if trace.Name != "answer-question" {
		t.Errorf("name = %q", trace.Name)
	}
	if trace.Environment != "prod" {
		t.Errorf("environment = %q", trace.Environment)
	}
	// Two errors, and only one of them said so in its span status: the
	// other is a span carrying an exception event, promoted so that
	// error_count and the filters over it can see it (Decision 26).
	if trace.ObservationCount != 5 || trace.ErrorCount != 2 {
		t.Errorf("counts = %d observations, %d errors", trace.ObservationCount, trace.ErrorCount)
	}
	observations, err := h.store.Observations(h.project.ID, "dd44ee55ff6677008899001122aabb33")
	if err != nil {
		t.Fatal(err)
	}
	var promoted *store.ObservationRow
	for _, o := range observations {
		if o.Name == "tool.parse" {
			promoted = o
		}
	}
	if promoted == nil || promoted.Level != "ERROR" {
		t.Fatalf("tool.parse = %+v, want it promoted to ERROR by its exception event", promoted)
	}
	if promoted.StatusMessage != "unparseable response" {
		t.Errorf("status_message = %q, want the exception's message", promoted.StatusMessage)
	}
	events, _ := promoted.Metadata["events"].([]any)
	if len(events) != 1 {
		t.Errorf("metadata events = %#v, want the stack trace preserved", promoted.Metadata["events"])
	}
	if trace.TotalCost != nil {
		t.Errorf("total_cost = %v, want no data when nobody provided cost", trace.TotalCost)
	}
}

// One unusable span is skipped and reported, never fatal to the batch
// (spec 002 #13).
func TestIngestPartialSuccess(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.post(t, "/v1/traces", fixtureBody(t, "004-partial-invalid-spans"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("a partial success must carry an ExportTraceServiceResponse body")
	}

	observations, err := h.store.Observations(h.project.ID, "cc33dd44ee55ff6677008899001122aa")
	if err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 {
		t.Fatalf("observations = %d, want the one mappable span", len(observations))
	}
}

// Every accepted body is kept, compressed, for a later remap (spec 002 #9).
func TestIngestStoresRawBody(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	body := fixtureBody(t, "002-genai-semconv-chat")

	var gzipped bytes.Buffer
	zw := gzip.NewWriter(&gzipped)
	zw.Write(body)
	zw.Close()

	rec := h.post(t, "/v1/traces", gzipped.Bytes(), func(r *http.Request) {
		r.Header.Set("Content-Encoding", "gzip")
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}

	batches, err := h.store.RawBatches(h.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 {
		t.Fatalf("raw batches = %d", len(batches))
	}
	// The decoded protobuf is what is stored; the header records how it
	// arrived, so a replay does not have to unwrap two layers.
	if !bytes.Equal(batches[0].Body, body) {
		t.Error("stored raw body is not the decoded protobuf")
	}
	if batches[0].ContentEncoding != "gzip" || batches[0].Dialect != mapping.DialectGenAI {
		t.Errorf("raw batch = %+v", batches[0])
	}
}

func TestIngestRawStorageCanBeDisabled(t *testing.T) {
	cfg := &config.Config{Listen: ":0", StoreRaw: false, MaxBodyBytes: config.DefaultMaxBodyBytes}
	h := newHarness(t, cfg, store.WriterOptions{})

	if rec := h.post(t, "/v1/traces", fixtureBody(t, "002-genai-semconv-chat")); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	batches, err := h.store.RawBatches(h.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(batches) != 0 {
		t.Errorf("raw batches with TRACEPAD_STORE_RAW=off = %d", len(batches))
	}
	// Ingest itself still works.
	if trace, _ := h.store.Trace(h.project.ID, "aa11bb22cc33dd44ee55ff6677889900"); trace == nil {
		t.Error("spans should still be stored when raw storage is off")
	}
}

// Both auth schemes work on both routes: one key pair serves the native and
// the Langfuse-SDK wire formats (spec 002 #2).
func TestIngestAuthSchemesAndRoutes(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	body := fixtureBody(t, "002-genai-semconv-chat")
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(testPublic+":"+testSecret))

	for _, path := range []string{"/v1/traces", "/api/public/otel/v1/traces"} {
		for name, header := range map[string]string{"bearer": "Bearer " + testSecret, "basic": basic} {
			rec := h.post(t, path, body, func(r *http.Request) {
				r.Header.Set("Authorization", header)
			})
			if rec.Code != http.StatusOK {
				t.Errorf("%s on %s: status = %d", name, path, rec.Code)
			}
		}
	}
}

func TestIngestRejectsBadCredentials(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	body := fixtureBody(t, "002-genai-semconv-chat")

	cases := map[string]string{
		"missing":      "",
		"wrong secret": "Bearer tp-sk-nope",
		"wrong basic":  "Basic " + base64.StdEncoding.EncodeToString([]byte("tp-pk-test:tp-sk-nope")),
		"unknown type": "Token " + testSecret,
	}
	for name, header := range cases {
		rec := h.post(t, "/v1/traces", body, func(r *http.Request) {
			r.Header.Set("Authorization", header)
		})
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", name, rec.Code)
		}
		var payload map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || payload["error"] != "unauthorized" {
			t.Errorf("%s: body = %s", name, rec.Body)
		}
	}
}

func TestIngestRejectsWrongContentType(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	rec := h.post(t, "/v1/traces", fixtureBody(t, "002-genai-semconv-chat"), func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
	})
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415", rec.Code)
	}
}

func TestIngestRejectsUndecodableBody(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	rec := h.post(t, "/v1/traces", []byte{0xff, 0xff, 0xff, 0xff})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestIngestRejectsOversizedBody(t *testing.T) {
	cfg := &config.Config{Listen: ":0", StoreRaw: true, MaxBodyBytes: 64}
	h := newHarness(t, cfg, store.WriterOptions{})

	rec := h.post(t, "/v1/traces", fixtureBody(t, "001-langfuse-sdk-generation"))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

// An empty export is a successful no-op per the OTLP spec, and leaves
// nothing behind — not even a raw body to replay.
func TestIngestEmptyBatch(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.post(t, "/v1/traces", nil)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("status = %d, body = %d bytes", rec.Code, rec.Body.Len())
	}
	batches, err := h.store.RawBatches(h.project.ID)
	if err != nil || len(batches) != 0 {
		t.Fatalf("raw batches = %d, err = %v", len(batches), err)
	}
}

// stubWriter answers every submission with a fixed error, so the handler's
// side of the write pipeline can be tested without racing a real one.
type stubWriter struct{ err error }

func (s stubWriter) Submit(context.Context, store.WriteJob) error { return s.err }

// A saturated writer answers 429 with Retry-After rather than stalling the
// exporter or dropping spans silently (spec 002 #15).
func TestIngestBackpressureReturns429(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.server.writer = stubWriter{err: store.ErrWriterBusy}

	rec := h.post(t, "/v1/traces", fixtureBody(t, "001-langfuse-sdk-generation"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a 429 must tell the exporter when to come back")
	}
}

// A storage failure is a 500, never a 200: a successful answer means the
// spans are on disk (spec 002 #15).
func TestIngestReportsWriteFailure(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.server.writer = stubWriter{err: errors.New("disk on fire")}

	rec := h.post(t, "/v1/traces", fixtureBody(t, "001-langfuse-sdk-generation"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func rs2slice[T any](v T) []T { return []T{v} }
