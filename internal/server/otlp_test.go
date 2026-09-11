package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/otlptest"
	"github.com/tracepad/tracepad/internal/store"
	"github.com/tracepad/tracepad/internal/storetest"
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
	sweeper *store.Sweeper
	project *store.Project
}

func newHarness(t *testing.T, cfg *config.Config, writerOpts store.WriterOptions) *harness {
	t.Helper()
	captureLogs(t)

	st := storetest.Open(t)
	project, err := st.CreateProject("test", store.KeyPair{PublicKey: testPublic, Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	// A lone submission waits the whole commit window before it is flushed,
	// and these tests write one row at a time: the default fifty
	// milliseconds, times every write in the package, was most of the
	// suite's runtime. Nothing here is about group commit — the store's own
	// tests are — so the window is shortened unless a test asks for one.
	if writerOpts.CommitWindow == 0 {
		writerOpts.CommitWindow = storetest.Writes.CommitWindow
	}
	writer, err := st.NewWriter(writerOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })

	if cfg == nil {
		cfg = &config.Config{Listen: ":0", StoreRaw: true, MaxBodyBytes: config.DefaultMaxBodyBytes}
	}
	// The sweeper is built but never started: the tests that care drive a
	// pass by hand, and the rest must not have rows disappear under them.
	sweeper := st.NewSweeper(writer, store.SweepOptions{Interval: cfg.SweepInterval})
	return &harness{
		server:  New(cfg, "test", st, writer, sweeper),
		store:   st,
		writer:  writer,
		sweeper: sweeper,
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

// archived is the whole of a project's raw archive, oldest first, each row
// with the body it holds. The listing and the body endpoint are what the API
// exposes (spec 019 #3); this is the same pair read straight from the store,
// so an ingest test can assert what was kept without going back through HTTP.
type archivedBatch struct {
	*store.RawBatchRow
	Body []byte
}

func archived(t *testing.T, h *harness) []archivedBatch {
	t.Helper()
	rows, err := h.store.RawBatches(h.project.ID, store.RawFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]archivedBatch, 0, len(rows))
	for _, row := range rows {
		body, err := h.store.RawBatchBody(h.project.ID, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if body == nil {
			t.Fatalf("raw batch %d vanished between the listing and the body", row.ID)
		}
		out = append(out, archivedBatch{RawBatchRow: row, Body: body.Body})
	}
	return out
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

// A non-finite double on an attribute the mapping reads as a number costs
// that attribute its column and nothing else (spec 030 #6, found in review of
// PR #53).
//
// The failure this guards is not the one attribute. `usage` and `cost_details`
// are encoded to JSON inside the ingest transaction, `json.Marshal` refuses
// NaN, and the refusal fails the write — so one poisoned span used to answer
// `500` for the whole batch, taking every healthy span in the body with it,
// and a conforming exporter would retry the same body until it gave up.
// Asserted at this boundary because that is where the cost was paid.
func TestIngestSurvivesNonFiniteNumbers(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	const traceID = "00112233445566778899aabbccddeeff" // the probe span's own
	poisoned := otlptest.ProbeSpan("gen_ai.system", "anthropic")
	poisoned.Attributes = append(poisoned.Attributes,
		otlptest.Double("input_tokens", math.NaN()),
		otlptest.Double("gen_ai.usage.cost", math.Inf(1)),
		otlptest.Int("output_tokens", 41),
	)
	body, err := mapping.EncodeExportRequest(otlptest.Export(poisoned))
	if err != nil {
		t.Fatal(err)
	}

	rec := h.post(t, "/v1/traces", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s; one unusable attribute must not cost the export",
			rec.Code, rec.Body)
	}

	observations, err := h.store.Observations(h.project.ID, traceID, store.WithIO)
	if err != nil || len(observations) != 1 {
		t.Fatalf("observations = %d, err = %v", len(observations), err)
	}
	observation := observations[0]
	// The count beside it is still a count.
	if observation.Usage["output_tokens"] != float64(41) {
		t.Errorf("usage = %v, want the healthy count kept", observation.Usage)
	}
	if _, counted := observation.Usage["input_tokens"]; counted {
		t.Errorf("usage = %v, want NaN refused rather than stored", observation.Usage)
	}
	if observation.CostDetails != nil {
		t.Errorf("cost_details = %v, want an infinite price refused", observation.CostDetails)
	}
	// And refused is not dropped: both are in metadata, as the text
	// `anyValue` kept them as (spec 002 #11).
	for _, key := range []string{"input_tokens", "gen_ai.usage.cost"} {
		if observation.Metadata[key] == nil {
			t.Errorf("metadata = %v, want %q preserved", observation.Metadata, key)
		}
	}
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

	observations, err := h.store.Observations(h.project.ID, traceID, store.WithIO)
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
	observations, err := h.store.Observations(h.project.ID, "dd44ee55ff6677008899001122aabb33", store.WithIO)
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

	observations, err := h.store.Observations(h.project.ID, "cc33dd44ee55ff6677008899001122aa", store.WithIO)
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

	batches := archived(t, h)
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
	if batches := archived(t, h); len(batches) != 0 {
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

// A content type this endpoint does not speak is a 415, and the message names
// the two it does. `application/json` used to be one of these; since spec 019
// #7 it is an encoding, and a body that lies about being it is a 400 about the
// body rather than a 415 about the header.
func TestIngestRejectsWrongContentType(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.post(t, "/v1/traces", fixtureBody(t, "002-genai-semconv-chat"), func(r *http.Request) {
		r.Header.Set("Content-Type", "application/yaml")
	})
	expectStatus(t, rec, http.StatusUnsupportedMediaType)
	message := decodeJSON[map[string]string](t, rec)["error"]
	if !strings.Contains(message, "x-protobuf") || !strings.Contains(message, "json") {
		t.Errorf("the refusal = %q, want both accepted encodings named", message)
	}

	protobufAsJSON := h.post(t, "/v1/traces", fixtureBody(t, "002-genai-semconv-chat"), func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json")
	})
	expectStatus(t, protobufAsJSON, http.StatusBadRequest)
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
	if batches := archived(t, h); len(batches) != 0 {
		t.Fatalf("raw batches = %d", len(batches))
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

// setRetention moves a project's trace window directly, for tests that need
// data to be expired without waiting for it. The API path is exercised by the
// admin tests; this is the shortcut for everyone else — and it still carries
// the echo, because shortening a window demands one wherever it is asked from
// (spec 005 #8).
func (h *harness) setRetention(projectID string, days int) error {
	project, err := h.store.ProjectByID(projectID)
	if err != nil {
		return err
	}
	return h.writer.Submit(context.Background(), &store.ProjectUpdate{
		ProjectID: projectID,
		Retention: store.OptionalDays{Set: true, Value: &days},
		Confirm:   project.Name,
	})
}
