package mapping_test

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/otlptest"
)

// The OTLP/JSON encoding (spec 019 #7). The claim under test is that the
// encoding is transport and nothing else: the same body in either spelling
// maps to the same rows.

// TestJSONEncodingMapsLikeProtobuf replays the whole corpus through the JSON
// door and diffs the mapping against the protobuf one — not against a golden
// written for JSON, which could be wrong in the same way twice, but against
// what the protobuf path produced from the very same bytes.
func TestJSONEncodingMapsLikeProtobuf(t *testing.T) {
	bodies, err := filepath.Glob(filepath.Join(fixtureDir, "*.pb"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) == 0 {
		t.Fatalf("no fixtures in %s", fixtureDir)
	}

	for _, path := range bodies {
		name := strings.TrimSuffix(filepath.Base(path), ".pb")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			fromProtobuf, _, err := mapping.DecodeExportRequest(raw)
			if err != nil {
				t.Fatal(err)
			}

			body, err := mapping.EncodeExportRequestJSON(fromProtobuf)
			if err != nil {
				t.Fatalf("encode as OTLP/JSON: %v", err)
			}
			fromJSON, unreadable, err := mapping.DecodeExportRequestJSON(body)
			if err != nil {
				t.Fatalf("decode OTLP/JSON: %v", err)
			}
			if unreadable != 0 {
				t.Fatalf("unreadable resource spans = %d", unreadable)
			}

			want, err := mapping.Map(fromProtobuf).DebugJSON()
			if err != nil {
				t.Fatal(err)
			}
			got, err := mapping.Map(fromJSON).DebugJSON()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("the JSON encoding of %s maps differently.\n--- json ---\n%s\n--- protobuf ---\n%s",
					name, got, want)
			}
		})
	}
}

// The ids are hex on the wire, which is where OTLP/JSON departs from
// protobuf-JSON and the one thing a hand-written encoder gets wrong.
func TestJSONEncodingWritesHexIDs(t *testing.T) {
	body, err := mapping.EncodeExportRequestJSON(otlptest.Fixtures()[0].ResourceSpans)
	if err != nil {
		t.Fatal(err)
	}
	const traceID = "4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f"
	if !strings.Contains(string(body), `"`+traceID+`"`) {
		t.Errorf("the encoded body does not carry the trace id as hex: %s", body)
	}
	// The same id as protobuf-JSON would have written it. Computed rather
	// than pasted, so the test cannot pass by comparing against a typo.
	raw, err := hex.DecodeString(traceID)
	if err != nil {
		t.Fatal(err)
	}
	if base64ID := base64.StdEncoding.EncodeToString(raw); strings.Contains(string(body), base64ID) {
		t.Errorf("the encoded body carries %s, the base64 id; OTLP/JSON prescribes hex", base64ID)
	}
}

// A body that is protobuf-JSON rather than OTLP/JSON differs in exactly one
// place, and the refusal has to say which: accepting base64 as well would make
// one body mean two things (spec 019 #7).
func TestJSONRejectsBase64IDsNamingTheField(t *testing.T) {
	body := []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[
	  {"traceId":"T4wdLjpbbH2Onwobwj1OXw==","spanId":"0102030405060708","name":"probe"}]}]}]}`)
	_, _, err := mapping.DecodeExportRequestJSON(body)
	if err == nil {
		t.Fatal("a base64 trace id must be refused")
	}
	if !strings.Contains(err.Error(), "traceId") {
		t.Errorf("error = %q, must name the field a client has to fix", err)
	}
}

// A nanosecond instant sent as a JSON number is past what a float64 holds
// exactly. Reading it through `any` the ordinary way rounds it by a few hundred
// nanoseconds — a wrong value that still looks right, which is the worst kind.
func TestJSONKeepsNanosecondPrecision(t *testing.T) {
	const start = 1787738400123456789
	body := []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[
	  {"traceId":"00112233445566778899aabbccddeeff","spanId":"0011223344556677",
	   "name":"probe","startTimeUnixNano":1787738400123456789,
	   "endTimeUnixNano":"1787738400123456799"}]}]}]}`)
	spans, _, err := mapping.DecodeExportRequestJSON(body)
	if err != nil {
		t.Fatal(err)
	}
	got := spans[0].ScopeSpans[0].Spans[0]
	if got.StartTimeUnixNano != start {
		t.Errorf("startTimeUnixNano = %d, want %d", got.StartTimeUnixNano, start)
	}
	// The other accepted spelling of a 64-bit integer, per the encoding.
	if got.EndTimeUnixNano != start+10 {
		t.Errorf("endTimeUnixNano = %d, want the string form read exactly", got.EndTimeUnixNano)
	}
}

// An empty body and an empty envelope are both valid, empty exports, as they
// are on the protobuf path: an idle exporter must not look broken.
// A body is one JSON value. json.Decoder.More answers false at a `]` or `}`
// nobody opened, so a body with one after the object was an export, and the
// strict decoders of the API refuse the same shape (found by
// FuzzDecodeExportRequestJSON).
func TestJSONRefusesMoreThanOneValue(t *testing.T) {
	for _, body := range []string{
		`{"resourceSpans":[]}]`, `{"resourceSpans":[]}}`, `{"resourceSpans":[]} {}`, `{"resourceSpans":[]} x`,
		`{"resourceSpans":[]}]]`, `{"resourceSpans":[]} ]`,
	} {
		if _, _, err := mapping.DecodeExportRequestJSON([]byte(body)); err == nil {
			t.Errorf("%s was accepted as an export", body)
		}
	}
	for _, body := range []string{`{"resourceSpans":[]}`, ` {"resourceSpans":[]} `, "{}\n"} {
		if _, _, err := mapping.DecodeExportRequestJSON([]byte(body)); err != nil {
			t.Errorf("%q was refused: %v", body, err)
		}
	}
}

func TestJSONEmptyExports(t *testing.T) {
	for _, body := range []string{"", "{}", `{"resourceSpans":[]}`, `{"resourceSpans":null}`} {
		spans, unreadable, err := mapping.DecodeExportRequestJSON([]byte(body))
		if err != nil || len(spans) != 0 || unreadable != 0 {
			t.Errorf("%q: spans=%d unreadable=%d err=%v", body, len(spans), unreadable, err)
		}
	}
}

// One unreadable element must not cost the export the ones that decoded — the
// same bargain the protobuf envelope strikes (spec 002 #13).
func TestJSONSkipsMalformedResourceSpans(t *testing.T) {
	good, err := mapping.EncodeExportRequestJSON(otlptest.Fixtures()[0].ResourceSpans)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string][]json.RawMessage
	if err := json.Unmarshal(good, &envelope); err != nil {
		t.Fatal(err)
	}
	envelope["resourceSpans"] = append(envelope["resourceSpans"], json.RawMessage(`{"scopeSpans":7}`))
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}

	spans, unreadable, err := mapping.DecodeExportRequestJSON(body)
	if err != nil {
		t.Fatalf("a partly readable body must still decode: %v", err)
	}
	if len(spans) != 1 || unreadable != 1 {
		t.Errorf("spans = %d, unreadable = %d, want the readable one kept and the other counted",
			len(spans), unreadable)
	}
}

// `null` is proto3 JSON's spelling of "the default value", so an exporter that
// writes `"parentSpanId": null` on a root span is writing valid OTLP/JSON. It
// used to fail the *whole* batch with a 400 — one root span costing every span
// beside it, which is the opposite of the bargain the decoder is built on.
func TestJSONAcceptsANullID(t *testing.T) {
	body := []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[
	  {"traceId":"00112233445566778899aabbccddeeff","spanId":"0011223344556677",
	   "parentSpanId":null,"name":"probe","startTimeUnixNano":"1787738400000000000",
	   "endTimeUnixNano":"1787738400001000000"}]}]}]}`)
	spans, unreadable, err := mapping.DecodeExportRequestJSON(body)
	if err != nil {
		t.Fatalf("a null parent span id must not fail the batch: %v", err)
	}
	if len(spans) != 1 || unreadable != 0 {
		t.Fatalf("spans = %d, unreadable = %d", len(spans), unreadable)
	}
	span := spans[0].ScopeSpans[0].Spans[0]
	if len(span.ParentSpanId) != 0 {
		t.Errorf("parentSpanId = %x, want the empty default", span.ParentSpanId)
	}
	if got := mapping.Map(spans); len(got.Observations) != 1 {
		t.Errorf("observations = %d, want the span mapped", len(got.Observations))
	}
}

// An id that is neither a string nor null is a document protojson refuses on
// its own: that costs the one ResourceSpans and is counted, rather than the
// batch (spec 002 #13).
func TestJSONCountsAnIDOfTheWrongType(t *testing.T) {
	body := []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[
	  {"traceId":12345,"spanId":"0011223344556677","name":"probe"}]}]}]}`)
	spans, unreadable, err := mapping.DecodeExportRequestJSON(body)
	if err != nil {
		t.Fatalf("err = %v, want the element counted rather than the batch refused", err)
	}
	if len(spans) != 0 || unreadable != 1 {
		t.Errorf("spans = %d, unreadable = %d, want the one element counted", len(spans), unreadable)
	}
}

// An unknown field is skipped, not refused: that is what the protobuf path
// does, and an exporter on a newer OTLP version has to keep ingesting.
func TestJSONIgnoresUnknownFields(t *testing.T) {
	body := []byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[
	  {"traceId":"00112233445566778899aabbccddeeff","spanId":"0011223344556677",
	   "name":"probe","somethingFromNextYear":{"a":1}}]}],"futureField":3}]}`)
	spans, unreadable, err := mapping.DecodeExportRequestJSON(body)
	if err != nil || unreadable != 0 || len(spans) != 1 {
		t.Fatalf("spans=%d unreadable=%d err=%v", len(spans), unreadable, err)
	}
}

// The response mirrors the request's encoding (spec 019 #7): a client that
// sent JSON can parse what comes back, and a full acceptance is the empty
// message rather than zero bytes, which is not JSON.
func TestJSONExportResponse(t *testing.T) {
	if got := string(mapping.EncodeExportResponseJSON(0, "")); got != "{}" {
		t.Errorf("a fully accepted export answered %q, want the empty message", got)
	}
	var partial struct {
		PartialSuccess struct {
			RejectedSpans string `json:"rejectedSpans"`
			ErrorMessage  string `json:"errorMessage"`
		} `json:"partialSuccess"`
	}
	body := mapping.EncodeExportResponseJSON(2, "skipped spans (invalid span id: 2)")
	if err := json.Unmarshal(body, &partial); err != nil {
		t.Fatalf("the response is not JSON: %v (%s)", err, body)
	}
	if partial.PartialSuccess.RejectedSpans != "2" {
		t.Errorf("rejectedSpans = %q, want the protojson spelling of a 64-bit integer",
			partial.PartialSuccess.RejectedSpans)
	}
	if !strings.Contains(partial.PartialSuccess.ErrorMessage, "invalid span id") {
		t.Errorf("errorMessage = %q", partial.PartialSuccess.ErrorMessage)
	}
}
