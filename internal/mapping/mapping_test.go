package mapping_test

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/otlptest"
)

var update = flag.Bool("update", false, "regenerate testdata/otlp bodies and testdata/golden files")

const (
	fixtureDir = "../../testdata/otlp"
	goldenDir  = "../../testdata/golden"
)

// TestGoldenFixtures replays every fixture body through decode → map and
// diffs the result against its golden file (spec 002, Testing #1). The
// fixtures on disk are the contract: `-update` rewrites both sides from the
// builders in internal/otlptest, and the diff is what gets reviewed.
func TestGoldenFixtures(t *testing.T) {
	if *update {
		writeFixtures(t)
	}

	bodies, err := filepath.Glob(filepath.Join(fixtureDir, "*.pb"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bodies) == 0 {
		t.Fatalf("no fixtures in %s; run: go test ./internal/mapping -update", fixtureDir)
	}

	for _, body := range bodies {
		name := strings.TrimSuffix(filepath.Base(body), ".pb")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(body)
			if err != nil {
				t.Fatal(err)
			}
			resourceSpans, unreadable, err := mapping.DecodeExportRequest(raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if unreadable != 0 {
				t.Fatalf("fixture has %d unreadable resource spans", unreadable)
			}
			got, err := mapping.Map(resourceSpans).DebugJSON()
			if err != nil {
				t.Fatalf("encode result: %v", err)
			}

			goldenPath := filepath.Join(goldenDir, name+".json")
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden (run with -update to create): %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("mapping of %s changed.\n--- got ---\n%s\n--- want ---\n%s",
					name, got, want)
			}
		})
	}
}

// writeFixtures regenerates the corpus. Both the bodies and the goldens come
// from one run so they can never disagree about what was mapped.
func writeFixtures(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(goldenDir, 0o755); err != nil {
		t.Fatal(err)
	}
	built := map[string]bool{}
	for _, fixture := range otlptest.Fixtures() {
		body, err := mapping.EncodeExportRequest(fixture.ResourceSpans)
		if err != nil {
			t.Fatalf("encode %s: %v", fixture.Name, err)
		}
		if err := os.WriteFile(filepath.Join(fixtureDir, fixture.Name+".pb"), body, 0o644); err != nil {
			t.Fatal(err)
		}
		golden, err := mapping.Map(fixture.ResourceSpans).DebugJSON()
		if err != nil {
			t.Fatalf("map %s: %v", fixture.Name, err)
		}
		if err := os.WriteFile(filepath.Join(goldenDir, fixture.Name+".json"), golden, 0o644); err != nil {
			t.Fatal(err)
		}
		built[fixture.Name] = true
	}
	writeForeignGoldens(t, built)
}

// writeForeignGoldens regenerates the goldens of bodies no builder produced.
//
// One body in the corpus is not synthetic: `010-tracepad-sdk.pb` is what our
// own Python package puts on the wire, written by `scripts/fixtures` (spec
// 017, Testing). Its bytes are the evidence, so they are kept as they arrived
// and only the golden beside them is rewritten — which is what makes a change
// to the mapper visible against a real export rather than against a builder
// that would have been edited to agree with it.
func writeForeignGoldens(t *testing.T, built map[string]bool) {
	t.Helper()
	bodies, err := filepath.Glob(filepath.Join(fixtureDir, "*.pb"))
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range bodies {
		name := strings.TrimSuffix(filepath.Base(body), ".pb")
		if built[name] {
			continue
		}
		raw, err := os.ReadFile(body)
		if err != nil {
			t.Fatal(err)
		}
		resourceSpans, _, err := mapping.DecodeExportRequest(raw)
		if err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
		golden, err := mapping.Map(resourceSpans).DebugJSON()
		if err != nil {
			t.Fatalf("map %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(goldenDir, name+".json"), golden, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, _, err := mapping.DecodeExportRequest([]byte{0xff, 0xff, 0xff}); err == nil {
		t.Fatal("expected a decode error for a non-protobuf body")
	}
}

// An empty body is a valid, empty export: OTLP says so, and answering 400
// would make idle exporters look broken.
func TestDecodeEmptyBody(t *testing.T) {
	spans, unreadable, err := mapping.DecodeExportRequest(nil)
	if err != nil || len(spans) != 0 || unreadable != 0 {
		t.Fatalf("spans=%v unreadable=%d err=%v", spans, unreadable, err)
	}
}

// One undecodable ResourceSpans must not cost the export the ones that
// decoded: it is counted and reported, not raised (Decision 18, #13).
func TestDecodeSkipsMalformedResourceSpans(t *testing.T) {
	good, err := mapping.EncodeExportRequest(otlptest.Fixtures()[0].ResourceSpans)
	if err != nil {
		t.Fatal(err)
	}
	// A second resource_spans field (tag 0x0a) holding undecodable bytes.
	body := append(append([]byte{}, good...), 0x0a, 0x03, 0xff, 0xff, 0xff)

	spans, unreadable, err := mapping.DecodeExportRequest(body)
	if err != nil {
		t.Fatalf("a partly readable body must still decode: %v", err)
	}
	if len(spans) != len(otlptest.Fixtures()[0].ResourceSpans) {
		t.Errorf("decoded %d resource spans, want the readable ones", len(spans))
	}
	if unreadable != 1 {
		t.Errorf("unreadable = %d, want 1", unreadable)
	}

	result := mapping.Map(spans)
	result.NoteUnreadable(unreadable)
	if !strings.Contains(result.SkipReason, "unreadable resource spans: 1") {
		t.Errorf("SkipReason = %q, must tell the exporter what was dropped", result.SkipReason)
	}
	if result.Skipped != 0 {
		t.Errorf("Skipped = %d: rejected_spans counts spans, and an unreadable block has no known span count", result.Skipped)
	}
}

// Spec 002 #11 is an invariant, not a best effort: an attribute a rule looked
// at and then rejected must still be visible. Each case below is an attribute
// the mapper inspects and declines to use — it belongs in metadata, not
// nowhere.
func TestRejectedAttributesSurviveInMetadata(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{"level spelling the alias table does not know", "langfuse.observation.level", "SEVERE"},
		{"cost_details that is not a JSON object", "langfuse.observation.cost_details", "0.15 usd"},
		{"usage_details that is not a JSON object", "langfuse.observation.usage_details", "lots"},
		{"model parameters that are not a JSON object", "langfuse.observation.model.parameters", "temp=0.2"},
		{"trace metadata that is not a JSON object", "langfuse.trace.metadata", "opaque"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := mapping.Map(otlptest.SpanWith(c.key, c.value))
			metadata := result.Observations[0].Metadata
			if metadata[c.key] != c.value {
				t.Errorf("%s = %#v, want it preserved in metadata; mapping must degrade, not drop",
					c.key, metadata[c.key])
			}
		})
	}
}

// The loser of a priority chain is a different fact from the winner: the
// model a client asked for and the model the provider answered with are both
// worth keeping.
func TestChainLosersSurviveInMetadata(t *testing.T) {
	result := mapping.Map(otlptest.SpanWith(
		"gen_ai.request.model", "gpt-4o-mini",
		"gen_ai.response.model", "gpt-4o-mini-2026-04-01",
	))
	observation := result.Observations[0]
	if observation.Model != "gpt-4o-mini" {
		t.Errorf("model = %q, want the requested one", observation.Model)
	}
	if observation.Metadata["gen_ai.response.model"] != "gpt-4o-mini-2026-04-01" {
		t.Errorf("metadata = %v, want the answering model preserved", observation.Metadata)
	}
}

// A payload that is not JSON stays the string it was. `gen_ai.prompt` carries
// plain text often enough — a system prompt written by hand, an answer logged
// as it was streamed — and the whole chain from here to the screen rests on
// the mapper not wrapping it in anything: the interface shows a bare string as
// text precisely because the API answers with one (spec 015 Decision 12).
// Fixture 011 is the corpus's example, so it is what this asserts on.
func TestPlainTextPayloadStaysAString(t *testing.T) {
	var fixture otlptest.Fixture
	for _, candidate := range otlptest.Fixtures() {
		if candidate.Name == "011-plain-text-prompt" {
			fixture = candidate
		}
	}
	if fixture.Name == "" {
		t.Fatal("the plain-text fixture is gone from the corpus")
	}

	// Counted before it is indexed: a rule that stopped mapping the span at
	// all would panic here rather than fail, and a panic takes the package's
	// other tests — TestGoldenFixtures among them — down with it.
	result := mapping.Map(fixture.ResourceSpans)
	if len(result.Observations) != 1 {
		t.Fatalf("mapped %d observations, want the fixture's one", len(result.Observations))
	}
	observation := result.Observations[0]

	input, ok := observation.Input.(string)
	if !ok {
		t.Fatalf("input = %#v, want the string the span sent", observation.Input)
	}
	if !strings.Contains(input, "\n") {
		t.Errorf("input = %q, want its newlines intact", input)
	}
	if strings.HasPrefix(input, `"`) {
		t.Errorf("input = %q, want the text itself and not a JSON string of it", input)
	}
	if _, ok := observation.Output.(string); !ok {
		t.Fatalf("output = %#v, want a string as well", observation.Output)
	}
}

// Span events carry the stack traces of a plain-OTel app, and a span with an
// exception is a failed span even when the exporter left the status unset —
// otherwise it would miss error_count and every error filter built on it
// (spec 002 Decision 26). An explicit level still outranks the promotion.
func TestExceptionEvents(t *testing.T) {
	cases := []struct {
		name              string
		span              func() *tracepb.Span
		wantLevel         string
		wantStatusMessage string
	}{
		{
			name: "exception event without a span status",
			span: func() *tracepb.Span {
				s := otlptest.ProbeSpan()
				s.Events = []*tracepb.Span_Event{
					otlptest.ExceptionEvent("TimeoutError", "upstream timed out", "line 1\nline 2"),
				}
				return s
			},
			wantLevel:         "ERROR",
			wantStatusMessage: "upstream timed out",
		},
		{
			name: "exception event with a span status",
			span: func() *tracepb.Span {
				s := otlptest.ProbeSpan()
				s.Status = otlptest.ErrorStatus("status says so")
				s.Events = []*tracepb.Span_Event{
					otlptest.ExceptionEvent("TimeoutError", "upstream timed out", ""),
				}
				return s
			},
			wantLevel: "ERROR",
			// The span's own status message is the more deliberate
			// of the two, so it keeps the column.
			wantStatusMessage: "status says so",
		},
		{
			name: "exception event under an explicit level",
			span: func() *tracepb.Span {
				s := otlptest.ProbeSpan("langfuse.observation.level", "DEBUG")
				s.Events = []*tracepb.Span_Event{
					otlptest.ExceptionEvent("Retryable", "retrying", ""),
				}
				return s
			},
			wantLevel:         "DEBUG",
			wantStatusMessage: "retrying",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			observation := mapping.Map(otlptest.Export(c.span())).Observations[0]
			if observation.Level != c.wantLevel {
				t.Errorf("level = %q, want %q", observation.Level, c.wantLevel)
			}
			if observation.StatusMessage != c.wantStatusMessage {
				t.Errorf("status_message = %q, want %q", observation.StatusMessage, c.wantStatusMessage)
			}
			events, ok := observation.Metadata["events"].([]any)
			if !ok || len(events) != 1 {
				t.Fatalf("metadata events = %#v, want the event preserved", observation.Metadata["events"])
			}
			event := events[0].(map[string]any)
			if event["name"] != "exception" || event["exception.type"] == "" {
				t.Errorf("event = %v, want the exception preserved whole", event)
			}
		})
	}
}

// Every event is kept, not just exceptions.
func TestNonExceptionEventsReachMetadata(t *testing.T) {
	span := otlptest.ProbeSpan()
	span.Events = []*tracepb.Span_Event{{
		Name:         "cache.lookup",
		TimeUnixNano: 42,
		Attributes:   nil,
	}}
	observation := mapping.Map(otlptest.Export(span)).Observations[0]

	if observation.Level != "DEFAULT" {
		t.Errorf("level = %q, an ordinary event must not raise it", observation.Level)
	}
	events, _ := observation.Metadata["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("metadata events = %#v", observation.Metadata["events"])
	}
	if event := events[0].(map[string]any); event["name"] != "cache.lookup" || event["time"] != int64(42) {
		t.Errorf("event = %v", event)
	}
}

// A fully accepted export answers with an empty message; only a partial
// success carries a body.
func TestEncodeExportResponse(t *testing.T) {
	if body := mapping.EncodeExportResponse(0, ""); len(body) != 0 {
		t.Fatalf("full success should encode to an empty message, got %d bytes", len(body))
	}
	if body := mapping.EncodeExportResponse(2, "skipped spans (invalid span id: 2)"); len(body) == 0 {
		t.Fatal("partial success should encode a body")
	}
}
