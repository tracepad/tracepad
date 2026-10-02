package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/otlptest"
	"github.com/tracepad/tracepad/internal/rawid"
	"github.com/tracepad/tracepad/internal/store"
)

// OTLP/JSON ingest end-to-end (spec 019 #7, #8): the same corpus through the
// other door, landing as the same rows, kept as it arrived.

// jsonBody renders a fixture in the OTLP/JSON encoding.
func jsonBody(t *testing.T, name string) []byte {
	t.Helper()
	for _, fixture := range otlptest.Fixtures() {
		if fixture.Name != name {
			continue
		}
		body, err := otlptest.JSONBody(fixture.ResourceSpans)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	t.Fatalf("unknown fixture %q", name)
	return nil
}

// postJSON sends a body the way an SDK on OTEL_EXPORTER_OTLP_PROTOCOL=http/json
// would.
func (h *harness) postJSON(t *testing.T, body []byte, mutate ...func(*http.Request)) *http.Response {
	t.Helper()
	rec := h.post(t, "/v1/traces", body, append([]func(*http.Request){
		func(r *http.Request) { r.Header.Set("Content-Type", "application/json") },
	}, mutate...)...)
	return rec.Result()
}

// TestJSONIngestLandsTheSameRows is Decision 7's claim, checked rather than
// asserted: the encoding is transport. The whole corpus goes in as protobuf on
// one server and as OTLP/JSON on another, and the two databases are compared
// row by row — every column, every payload, both ways.
func TestJSONIngestLandsTheSameRows(t *testing.T) {
	protobuf := newHarness(t, nil, store.WriterOptions{})
	viaJSON := newHarness(t, nil, store.WriterOptions{})

	for _, fixture := range otlptest.Fixtures() {
		body, err := mapping.EncodeExportRequest(fixture.ResourceSpans)
		if err != nil {
			t.Fatal(err)
		}
		if rec := protobuf.post(t, "/v1/traces", body); rec.Code != http.StatusOK {
			t.Fatalf("%s as protobuf: status = %d (%s)", fixture.Name, rec.Code, rec.Body)
		}
		encoded, err := otlptest.JSONBody(fixture.ResourceSpans)
		if err != nil {
			t.Fatal(err)
		}
		if response := viaJSON.postJSON(t, encoded); response.StatusCode != http.StatusOK {
			t.Fatalf("%s as OTLP/JSON: status = %d", fixture.Name, response.StatusCode)
		}
	}

	want := storedRows(t, protobuf)
	got := storedRows(t, viaJSON)
	if !bytes.Equal(got, want) {
		t.Errorf("the corpus lands differently through the JSON door.\n--- json ---\n%s\n--- protobuf ---\n%s",
			got, want)
	}
}

// storedRows renders everything one project holds about its traces, in a fixed
// order, so two databases can be compared as text.
func storedRows(t *testing.T, h *harness) []byte {
	t.Helper()
	traces, err := h.store.Traces(t.Context(), h.project.ID, store.TraceFilter{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	type entry struct {
		Trace        *store.TraceRow
		Observations []*store.ObservationRow
	}
	rows := make([]entry, 0, len(traces))
	for _, row := range traces {
		trace, err := h.store.Trace(t.Context(), h.project.ID, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		observations, err := h.store.Observations(t.Context(), h.project.ID, row.ID, store.WithIO)
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, entry{Trace: trace, Observations: observations})
	}
	encoded, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// The project id is minted per store and is the one column that is
	// allowed to differ between two servers holding the same corpus.
	return bytes.ReplaceAll(encoded, []byte(h.project.ID), []byte("<project>"))
}

// The response mirrors the request's encoding, partial success included: a
// client that sent JSON has to be able to parse what it gets back (spec 019
// #7). Fixture 004 is the one with spans the mapper cannot use.
func TestJSONIngestAnswersWithJSONPartialSuccess(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	response := h.postJSON(t, jsonBody(t, "004-partial-invalid-spans"))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want the request's encoding", got)
	}
	var body struct {
		PartialSuccess struct {
			RejectedSpans string `json:"rejectedSpans"`
			ErrorMessage  string `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("the response is not JSON: %v", err)
	}
	if body.PartialSuccess.RejectedSpans != "2" {
		t.Errorf("rejectedSpans = %q, want the two unmappable spans",
			body.PartialSuccess.RejectedSpans)
	}
	if !strings.Contains(body.PartialSuccess.ErrorMessage, "span") {
		t.Errorf("errorMessage = %q, want it to say what was skipped", body.PartialSuccess.ErrorMessage)
	}
	// And a fully accepted export answers with the empty message rather
	// than zero bytes, which is not JSON.
	full := h.postJSON(t, jsonBody(t, "002-genai-semconv-chat"))
	whole, _ := readAll(t, full)
	if string(whole) != "{}" {
		t.Errorf("a fully accepted JSON export answered %q, want the empty message", whole)
	}
}

// A JSON batch is kept as it arrived: JSON, under its own content type, with
// the decoded bytes of the request (spec 019 #8). A conversion at ingest would
// make the archive the converter's output, and a bug in it unfixable.
func TestJSONIngestIsArchivedAsReceived(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	sent := jsonBody(t, "001-langfuse-sdk-generation")
	if response := h.postJSON(t, sent); response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}

	batches := archived(t, h)
	if len(batches) != 1 {
		t.Fatalf("raw batches = %d", len(batches))
	}
	if batches[0].ContentType != "application/json" {
		t.Errorf("content_type = %q, want the encoding it arrived in", batches[0].ContentType)
	}
	if !bytes.Equal(batches[0].Body, sent) {
		t.Error("the archived body is not the JSON the client sent")
	}
	// And the API replays it under that type, which is what makes the
	// export's `--to` work against a receiver that took it once.
	rec := h.get(t, rawPath(t, h))
	expectStatus(t, rec, 200)
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type of the replayed body = %q", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), sent) {
		t.Error("the replayed body is not the JSON the client sent")
	}
}

// gzip applies to the JSON encoding as it does to the protobuf one, and the
// archive keeps the decoded body either way.
func TestJSONIngestAcceptsGzip(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	sent := jsonBody(t, "002-genai-semconv-chat")
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	zw.Write(sent)
	zw.Close()

	response := h.postJSON(t, compressed.Bytes(), func(r *http.Request) {
		r.Header.Set("Content-Encoding", "gzip")
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	batches := archived(t, h)
	if len(batches) != 1 || !bytes.Equal(batches[0].Body, sent) {
		t.Fatal("the archived body is not the decoded JSON")
	}
	if batches[0].ContentEncoding != "gzip" || batches[0].ContentType != "application/json" {
		t.Errorf("raw batch = %+v", batches[0].RawBatchRow)
	}
	if trace, _ := h.store.Trace(t.Context(), h.project.ID, "aa11bb22cc33dd44ee55ff6677889900"); trace == nil {
		t.Error("the spans of a gzipped JSON export were not stored")
	}
}

// protobuf-JSON in place of OTLP/JSON differs in one field, and the refusal
// names it. Nothing was accepted, so nothing is archived (spec 002 #13's
// "accepted request body").
func TestJSONIngestRejectsBase64IDs(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	body := []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[
	  {"traceId":"T4wdLjpbbH2OnwobLD1OXw==","spanId":"0102030405060708","name":"probe"}]}]}]}`)

	response := h.postJSON(t, body)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.StatusCode)
	}
	message, _ := readAll(t, response)
	if !strings.Contains(string(message), "traceId") {
		t.Errorf("the refusal = %s, must name the field a client has to fix", message)
	}
	if batches := archived(t, h); len(batches) != 0 {
		t.Errorf("raw batches = %d, want none: nothing was accepted", len(batches))
	}
}

// A charset parameter is a fact about one request, not about the bytes, so it
// does not follow them into the archive and onto the wire of a replay.
func TestJSONIngestNormalisesTheContentType(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	response := h.postJSON(t, jsonBody(t, "002-genai-semconv-chat"), func(r *http.Request) {
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if got := archived(t, h)[0].ContentType; got != "application/json" {
		t.Errorf("content_type = %q, want the media type alone", got)
	}
}

// rawPath is the body endpoint of the project's one archived batch.
func rawPath(t *testing.T, h *harness) string {
	t.Helper()
	batches := archived(t, h)
	if len(batches) != 1 {
		t.Fatalf("raw batches = %d, want exactly one", len(batches))
	}
	return "/api/v1/raw/" + rawid.ID(batches[0].Number)
}

func readAll(t *testing.T, response *http.Response) ([]byte, error) {
	t.Helper()
	defer response.Body.Close()
	var out bytes.Buffer
	if _, err := out.ReadFrom(response.Body); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), nil
}
