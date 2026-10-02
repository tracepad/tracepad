package mapping_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/otlptest"
)

// Fuzz targets for the ingest decoders: the bodies a stranger sends over the
// network. `make fuzz` runs each for a few seconds; a longer run is
// `go test ./internal/mapping -run '^$' -fuzz FuzzDecodeExportRequest$ -fuzztime 3m`.
//
// The invariants are the ones the handler leans on, not new rules: a body is
// either refused with a message that says what was wrong, or it decodes into
// something the rest of ingest can walk, map and marshal without a panic —
// and a panic in a handler is a dropped connection for whoever sent the body.
// Inputs are capped far under the server's own body limit (20 MiB): size is
// the body cap's business and has its own tests (spec 043), and what a fuzzer
// finds in a megabyte it finds in a kilobyte.
const (
	maxFuzzBody = 64 << 10
	// maxSeedBody keeps the large-payload fixtures out of the seeds: a mutation
	// of a 200 KB body costs half a second a run, which is the whole fuzzing
	// budget spent on bytes that are one long string.
	maxSeedBody = 16 << 10
)

// seedBodies adds the committed synthetic corpus, in both encodings, to a
// fuzz target. Everything under testdata/otlp is invented content (spec 002,
// Testing #1); the raw captures live outside this repository and are never
// seeded.
func seedBodies(f *testing.F, asJSON bool) {
	f.Helper()
	files, err := filepath.Glob(filepath.Join(fixtureDir, "*.pb"))
	if err != nil || len(files) == 0 {
		f.Fatalf("no fixture bodies under %s: %v", fixtureDir, err)
	}
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			f.Fatal(err)
		}
		if len(body) > maxSeedBody {
			continue
		}
		if !asJSON {
			f.Add(body)
			continue
		}
		spans, _, err := mapping.DecodeExportRequest(body)
		if err != nil {
			f.Fatalf("%s: %v", file, err)
		}
		encoded, err := mapping.EncodeExportRequestJSON(spans)
		if err != nil {
			f.Fatalf("%s: %v", file, err)
		}
		f.Add(encoded)
	}
	// What an exporter sends when it has nothing, and two bodies that
	// are one byte from valid.
	f.Add([]byte(nil))
	if asJSON {
		f.Add([]byte(`{}`))
		f.Add([]byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"00112233445566778899aabbccddeeff","spanId":"0011223344556677","name":"x"}]}]}]}`))
		f.Add([]byte(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"zz"}]}]}]}`))
	} else {
		f.Add([]byte{0x0a, 0x00})
		f.Add([]byte{0x0a, 0x03, 0xff, 0xff, 0xff})
	}
}

// checkExport is everything ingest does with a decoded export, in the order
// it does it, asserting that none of it panics and that what comes out is
// serializable: a trace or observation that json.Marshal cannot encode fails
// inside the ingest transaction and loses the whole batch (asNumber's note on
// NaN is the instance that taught this).
func checkExport(t *testing.T, decoded *mapping.ExportBody) {
	t.Helper()
	spans := mapping.CountSpans(decoded.ResourceSpans)

	result := mapping.Map(decoded.ResourceSpans)
	if got := int64(len(result.Observations)) + result.Skipped; got != int64(spans) {
		t.Fatalf("%d observations and %d skipped for %d spans", len(result.Observations), result.Skipped, spans)
	}
	for _, trace := range result.Traces {
		if _, err := json.Marshal(trace); err != nil {
			t.Fatalf("trace %q does not marshal: %v", trace.ID, err)
		}
	}
	for _, observation := range result.Observations {
		if _, err := json.Marshal(observation); err != nil {
			t.Fatalf("observation %q does not marshal: %v", observation.ID, err)
		}
	}
	result.NoteUnreadable(decoded.Unreadable)
}

// checkMedia runs the walk both ways over a freshly decoded body and asserts
// that the body it re-encodes is one the server could read back.
func checkMedia(t *testing.T, body []byte, asJSON bool) {
	t.Helper()
	for _, placeholder := range []bool{false, true} {
		decoded, err := mapping.DecodeExportBody(body, asJSON)
		if err != nil {
			t.Fatalf("a body that decoded once does not decode again: %v", err)
		}
		media := mapping.ExtractMedia(decoded.ResourceSpans, mapping.MediaOptions{
			Placeholder: placeholder,
			Resolve:     func(string) (string, int64, bool) { return strings.Repeat("a", 64), 1, true },
			Warn:        func(string, string) {},
		})
		_ = media.SHAs()
		encoded, err := decoded.Encode(media.Rewrites)
		if err != nil {
			t.Fatalf("encode after extracting media: %v", err)
		}
		again, err := mapping.DecodeExportBody(encoded, asJSON)
		if err != nil {
			t.Fatalf("the body re-encoded after extracting media does not decode: %v", err)
		}
		if got, want := mapping.CountSpans(again.ResourceSpans), mapping.CountSpans(decoded.ResourceSpans); got != want {
			t.Fatalf("extracting media changed the span count from %d to %d", want, got)
		}
		_ = mapping.MediaReferences(again.ResourceSpans)
		inlined := mapping.InlineMedia(again.ResourceSpans, func(string) (string, []byte, bool) {
			return "image/png", []byte("inline"), true
		})
		if _, err := again.Encode(inlined); err != nil {
			t.Fatalf("encode after inlining media: %v", err)
		}
	}
}

// checkErasure cuts every trace of a body out of it, which is what erasure
// does to the raw archive, and asserts that what is left decodes and holds
// none of them.
func checkErasure(t *testing.T, body []byte, asJSON bool) {
	t.Helper()
	decoded, err := mapping.DecodeExportBody(body, asJSON)
	if err != nil {
		t.Fatalf("a body that decoded once does not decode again: %v", err)
	}
	ids := map[string]bool{}
	for _, trace := range mapping.Map(decoded.ResourceSpans).Traces {
		ids[trace.ID] = true
	}
	if len(ids) == 0 {
		return
	}
	cut, removed, err := decoded.Without(ids)
	if err != nil {
		t.Fatalf("cut: %v", err)
	}
	left, err := mapping.DecodeExportBody(cut, asJSON)
	if err != nil {
		t.Fatalf("what is left of a body after an erasure does not decode: %v", err)
	}
	if removed > 0 && len(mapping.Map(left.ResourceSpans).Traces) != 0 {
		t.Fatalf("an erasure of every trace left traces behind")
	}
}

func FuzzDecodeExportRequest(f *testing.F) {
	seedBodies(f, false)
	f.Fuzz(checkProtoBody)
}

func checkProtoBody(t *testing.T, body []byte) {
	{
		if len(body) > maxFuzzBody {
			t.Skip()
		}
		decoded, err := mapping.DecodeExportBody(body, false)
		if err != nil {
			if !errors.Is(err, mapping.ErrMalformedBody) || err.Error() == "" {
				t.Fatalf("a refusal must be ErrMalformedBody with a message, got %v", err)
			}
			return
		}
		checkExport(t, decoded)
		checkMedia(t, body, false)
		checkErasure(t, body, false)

		// Round trip: what decoded encodes to a body that decodes to the
		// same mapping.
		encoded, err := mapping.EncodeExportRequest(decoded.ResourceSpans)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		again, _, err := mapping.DecodeExportRequest(encoded)
		if err != nil {
			t.Fatalf("a re-encoded export does not decode: %v", err)
		}
		if got, want := marshalMapping(t, again), marshalMapping(t, decoded.ResourceSpans); got != want {
			t.Fatalf("the mapping changed across a round trip:\n got %s\nwant %s", got, want)
		}
	}
}

func FuzzDecodeExportRequestJSON(f *testing.F) {
	seedBodies(f, true)
	f.Fuzz(checkJSONBody)
}

func checkJSONBody(t *testing.T, body []byte) {
	{
		if len(body) > maxFuzzBody {
			t.Skip()
		}
		decoded, err := mapping.DecodeExportBody(body, true)
		if err != nil {
			if !errors.Is(err, mapping.ErrMalformedBody) || err.Error() == "" {
				t.Fatalf("a refusal must be ErrMalformedBody with a message, got %v", err)
			}
			return
		}
		checkExport(t, decoded)
		checkMedia(t, body, true)
		checkErasure(t, body, true)

		encoded, err := mapping.EncodeExportRequestJSON(decoded.ResourceSpans)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		again, _, err := mapping.DecodeExportRequestJSON(encoded)
		if err != nil {
			t.Fatalf("a re-encoded export does not decode: %v\n%s", err, encoded)
		}
		if got, want := marshalMapping(t, again), marshalMapping(t, decoded.ResourceSpans); got != want {
			t.Fatalf("the mapping changed across a round trip:\n got %s\nwant %s", got, want)
		}
	}
}

func marshalMapping(t *testing.T, resourceSpans []*tracepb.ResourceSpans) string {
	t.Helper()
	result := mapping.Map(resourceSpans)
	out, err := json.Marshal(struct {
		Traces       any
		Observations any
		Skipped      int64
	}{result.Traces, result.Observations, result.Skipped})
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// The two response decoders read what a receiver answers us; they take a
// stranger's bytes too (the CLI and the SDK tests export to whatever is
// configured). They never fail, and what the encoder writes they read back.
func FuzzDecodeExportResponse(f *testing.F) {
	f.Add([]byte(nil), int64(0), "")
	f.Add(mapping.EncodeExportResponse(3, "three spans had no trace id"), int64(3), "three spans had no trace id")
	f.Add([]byte{0x0a, 0x04, 0x08, 0x01, 0x12}, int64(-1), "\xff")
	f.Add([]byte(`{"partialSuccess":{"rejectedSpans":"7","errorMessage":"x"}}`), int64(7), "x")
	f.Add([]byte(`{"partialSuccess":{"rejectedSpans":1e400}}`), int64(0), "")
	f.Fuzz(func(t *testing.T, body []byte, rejected int64, message string) {
		if len(body) > maxFuzzBody {
			t.Skip()
		}
		mapping.DecodeExportResponse(body)
		mapping.DecodeExportResponseJSON(body)

		if !utf8.ValidString(message) || rejected < 0 {
			return
		}
		gotRejected, gotMessage := mapping.DecodeExportResponse(mapping.EncodeExportResponse(rejected, message))
		if gotRejected != rejected || gotMessage != message {
			t.Fatalf("protobuf response: wrote (%d, %q), read (%d, %q)", rejected, message, gotRejected, gotMessage)
		}
		gotRejected, gotMessage = mapping.DecodeExportResponseJSON(mapping.EncodeExportResponseJSON(rejected, message))
		if gotRejected != rejected || gotMessage != message {
			t.Fatalf("JSON response: wrote (%d, %q), read (%d, %q)", rejected, message, gotRejected, gotMessage)
		}
	})
}

// FuzzMapAttribute puts one attribute of every value type on a span and maps
// it. The keys the mapper reads are a closed set the engine cannot guess from
// the corpus alone, so they are the seeds; what it mutates is the value.
//
// Beyond "does not panic" and "marshals" it asserts the one promise every
// number column rests on: a usage or cost figure the mapper claims is a finite
// number (spec 002 #11, spec 030 #6).
func FuzzMapAttribute(f *testing.F) {
	keys := []string{
		"langfuse.trace.name", "langfuse.trace.tags", "langfuse.trace.metadata",
		"langfuse.user.id", "langfuse.session.id", "langfuse.environment",
		"langfuse.release", "langfuse.version", "tracepad.run_id", "tracepad.item_id",
		"tracepad.trace.tags", "tracepad.observation.type",
		"langfuse.observation.type", "langfuse.observation.level",
		"langfuse.observation.metadata", "langfuse.observation.input",
		"langfuse.observation.usage_details", "langfuse.observation.cost_details",
		"langfuse.observation.model.parameters", "langfuse.observation.model_parameters",
		"langfuse.observation.completion_start_time",
		"langfuse.observation.prompt.name", "langfuse.observation.prompt.version",
		"gen_ai.usage.input_tokens", "gen_ai.usage.cost", "gen_ai.request.model",
		"gen_ai.input.messages", "gen_ai.prompt.0.content", "gen_ai.completion",
		"input_tokens", "total_tokens", "cache_read_input_tokens",
		"service.version", "session.id", "deployment.environment",
	}
	values := []string{
		"", "0", "-1", "1.5", "1e400", "NaN", "+Inf", "0x10", "007", " 42 ",
		`{}`, `{"input":1}`, `{"input":"7","total":null}`, `{"a":{"b":[1,2]}}`,
		`["a","b"]`, `"2026-10-02T10:00:00Z"`, "2026-10-02T10:00:00Z",
		"9223372036854775807", "-9223372036854775808", "1e30",
		"data:image/png;base64,AAAA", "@@@langfuseMedia:type=image/png|id=abc|source=bytes@@@",
	}
	for _, key := range keys {
		for _, value := range values {
			f.Add(key, value, uint8(0), int64(1), float64(1))
		}
	}
	f.Add("gen_ai.usage.input_tokens", "", uint8(2), int64(1<<62), float64(0))
	f.Add("gen_ai.usage.input_tokens", "", uint8(3), int64(0), float64(1.7976931348623157e308))
	f.Add("langfuse.observation.cost_details", "", uint8(3), int64(0), float64(-1))

	f.Fuzz(func(t *testing.T, key, text string, kind uint8, integer int64, double float64) {
		if len(key)+len(text) > maxFuzzBody || !utf8.ValidString(key) || !utf8.ValidString(text) {
			t.Skip()
		}
		var value *commonpb.AnyValue
		switch kind % 6 {
		case 0:
			value = &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: text}}
		case 1:
			value = &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: integer%2 == 0}}
		case 2:
			value = &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: integer}}
		case 3:
			value = &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: double}}
		case 4:
			value = &commonpb.AnyValue{Value: &commonpb.AnyValue_ArrayValue{ArrayValue: &commonpb.ArrayValue{Values: []*commonpb.AnyValue{
				{Value: &commonpb.AnyValue_StringValue{StringValue: text}},
				{Value: &commonpb.AnyValue_IntValue{IntValue: integer}},
				{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: double}},
			}}}}
		case 5:
			value = &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{
				{Key: text, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: double}}},
				{Key: "n", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: integer}}},
			}}}}
		}
		span := otlptest.ProbeSpan()
		span.Attributes = append(span.Attributes, &commonpb.KeyValue{Key: key, Value: value})

		// As a span attribute, and again as the resource's: the two
		// levels are separate code paths (spec 012 #7).
		export := otlptest.Export(span)
		export[0].Resource = nil
		resource := otlptest.Export(otlptest.ProbeSpan())
		resource[0].Resource.Attributes = append(resource[0].Resource.Attributes, &commonpb.KeyValue{Key: key, Value: value})

		for _, spans := range [][]*tracepb.ResourceSpans{export, resource} {
			body, err := mapping.EncodeExportRequest(spans)
			if err != nil {
				t.Skip() // invalid UTF-8 inside a nested key
			}
			decoded, err := mapping.DecodeExportBody(body, false)
			if err != nil {
				t.Fatalf("a body we encoded does not decode: %v", err)
			}
			checkExport(t, decoded)
			checkMedia(t, body, false)

			result := mapping.Map(decoded.ResourceSpans)
			for _, observation := range result.Observations {
				for name, figure := range observation.Usage {
					assertFigure(t, "usage", name, figure)
				}
				for name, figure := range observation.CostDetails {
					assertFigure(t, "cost", name, figure)
				}
			}
		}
	})
}

// assertFigure: a usage or cost figure the mapper claimed is a number a
// column can hold. Anything else it left in metadata, so a figure that is
// still a string or a structure here came from an explicit usage_details
// object, which is the client's own and stored as sent.
func assertFigure(t *testing.T, kind, name string, figure any) {
	t.Helper()
	switch n := figure.(type) {
	case float64:
		if n != n || n > 1.7976931348623157e308 || n < -1.7976931348623157e308 {
			t.Fatalf("%s %q holds %v, which no encoder can write", kind, name, n)
		}
	}
	if _, err := json.Marshal(figure); err != nil {
		t.Fatalf("%s %q does not marshal: %v", kind, name, err)
	}
}

// FuzzMediaDocument hands the media walk a string in each place it looks: as a
// whole attribute, and inside a JSON document under the keys of the three
// shapes it recognises. A match under the size floor is left alone, so the
// interesting seeds are the long ones.
func FuzzMediaDocument(f *testing.F) {
	big := strings.Repeat("QUJD", 1500) // 4500 bytes decoded: over the floor
	f.Add("data:image/png;base64," + big)
	f.Add("data:IMAGE/PNG;BASE64," + big)
	f.Add(big)
	f.Add("@@@langfuseMedia:type=image/png|id=abc|source=bytes@@@")
	f.Add(`{"type":"base64","media_type":"image/png","data":"` + big + `"}`)
	f.Add(`{"inline_data":{"mime_type":"image/png","data":"` + big + `"}}`)
	f.Add(`{"type":"blob","mime_type":"image/png","content":"` + big + `"}`)
	f.Add(`[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + big + `"}}]}]`)
	f.Add(`{"tracepad_media":"` + strings.Repeat("0", 64) + `","mime_type":"image/png","size":4500}`)

	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > maxFuzzBody || !utf8.ValidString(text) {
			t.Skip()
		}
		for _, key := range []string{"langfuse.observation.input", "gen_ai.input.messages", "anything.else"} {
			span := otlptest.ProbeSpan(key, text)
			body, err := mapping.EncodeExportRequest(otlptest.Export(span))
			if err != nil {
				t.Skip()
			}
			checkMedia(t, body, false)
			if jsonBody, err := mapping.EncodeExportRequestJSON(otlptest.Export(span)); err == nil {
				checkMedia(t, jsonBody, true)
			}
			decoded, _ := mapping.DecodeExportBody(body, false)
			media := mapping.ExtractMedia(decoded.ResourceSpans, mapping.MediaOptions{})
			for _, extracted := range media.Bodies {
				if len(extracted.Body) < mapping.MediaMinSize {
					t.Fatalf("extracted a %d-byte body, under the %d floor", len(extracted.Body), mapping.MediaMinSize)
				}
				if bytes.Contains(body, extracted.Body) && len(extracted.Body) > 0 {
					// The body came out of the attribute, so it
					// is no longer in the export.
					encoded, err := decoded.Encode(media.Rewrites)
					if err == nil && bytes.Contains(encoded, extracted.Body) {
						t.Fatalf("a body extracted as media is still inline in the re-encoded export")
					}
				}
			}
		}
	})
}
