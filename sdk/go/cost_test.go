package tracepad_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	tracepad "github.com/tracepad/tracepad/sdk/go"
	"github.com/tracepad/tracepad/sdk/go/tracepadtest"
)

// What the package costs when nothing records, and how long it waits (spec 042).

// counted is a value whose every serialisation is counted.
type counted struct{ n *int }

func (c counted) MarshalJSON() ([]byte, error) {
	*c.n++
	return []byte(`"<counted>"`), nil
}

// serialiseEverything goes through every door that serialises, each handed
// one counted value, and says how many serialisations there were.
func serialiseEverything(ctx context.Context) int {
	n := 0
	c := counted{&n}
	ctx, step := tracepad.Span(ctx, "s", tracepad.WithInput(c), tracepad.WithMetadata(map[string]any{"k": c}))
	step.Update(tracepad.WithOutput(c), tracepad.WithMetadata(map[string]any{"j": c}))
	tracepad.Update(ctx, tracepad.WithInput(c))
	tracepad.UpdateTrace(ctx, tracepad.WithTraceMetadata(map[string]any{"t": c}))
	_, call := tracepad.Generation(ctx, "g", tracepad.WithModel("m"),
		tracepad.WithModelParameters(map[string]any{"p": c}), tracepad.WithInput(c))
	call.End(tracepad.Result{Output: c})
	step.End()
	return n
}

// sampledOut is under a caller that chose not to sample: the default sampler
// drops every span.
func sampledOut() context.Context {
	return trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, Remote: true,
	}))
}

func TestASpanSampledOutSerialisesNothing(t *testing.T) {
	rec := tracepadtest.Capture(t)
	if n := serialiseEverything(sampledOut()); n != 0 {
		t.Errorf("serialised %d times, want none", n)
	}
	if len(rec.Spans()) != 0 {
		t.Errorf("spans = %d, want none", len(rec.Spans()))
	}
}

func TestWithTracingOffNothingIsSerialised(t *testing.T) {
	tracepadtest.Reset(t)
	if n := serialiseEverything(context.Background()); n != 0 {
		t.Errorf("serialised %d times, want none", n)
	}
}

func TestASpanThatRecordsSerialisesEachValueOnce(t *testing.T) {
	rec := tracepadtest.Capture(t)
	if n := serialiseEverything(context.Background()); n != 9 {
		t.Errorf("serialised %d times, want the nine values once each", n)
	}
	if got := rec.Attributes(t, "s")["gen_ai.input.messages"].AsString(); got != `"<counted>"` {
		t.Errorf("input = %q", got)
	}
}

// seeing is a sampler that keeps what each span starts with.
type seeing struct{ seen *[]map[string]string }

func (s seeing) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	start := map[string]string{}
	for _, kv := range p.Attributes {
		start[string(kv.Key)] = kv.Value.Emit()
	}
	*s.seen = append(*s.seen, start)
	return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample}
}

func (seeing) Description() string { return "seeing" }

func TestASamplerStillSeesTheCheapAttributesAtStart(t *testing.T) {
	tracepadtest.Reset(t)
	var seen []map[string]string
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(seeing{&seen}))
	if _, err := tracepad.Init(context.Background(), tracepad.WithHost("http://tracepad.test:4318"), tracepad.WithKey("tp-sk-test"),
		tracepad.WithExport(false), tracepad.WithTracerProvider(provider)); err != nil {
		t.Fatal(err)
	}
	_, call := tracepad.Generation(context.Background(), "chat", tracepad.WithModel("gpt-4o-mini"),
		tracepad.WithInput("hello"), tracepad.WithMetadata(map[string]any{"a": 1}))
	call.End(tracepad.Result{})
	want := []map[string]string{{"tracepad.observation.type": "generation", "gen_ai.request.model": "gpt-4o-mini"}}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("a sampler saw %v, want %v", seen, want)
	}
}

// logs is what the package logs to the default logger, debug included.
func logs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &out
}

func levels(out *bytes.Buffer) []string {
	var found []string
	for line := range strings.Lines(out.String()) {
		for _, level := range []string{"level=DEBUG", "level=WARN"} {
			if strings.Contains(line, level) {
				found = append(found, strings.TrimPrefix(level, "level="))
			}
		}
	}
	return found
}

func TestWithTracingOffUpdateInABlockSaysSoAtDebug(t *testing.T) {
	tracepadtest.Reset(t)
	out := logs(t)
	ctx, step := tracepad.Span(context.Background(), "off")
	tracepad.Update(ctx, tracepad.WithOutput("x"))
	tracepad.UpdateTrace(ctx, tracepad.WithUserID("u-1"))
	step.End()
	if got := levels(out); !reflect.DeepEqual(got, []string{"DEBUG", "DEBUG"}) {
		t.Errorf("logged %v, want two debug lines:\n%s", got, out)
	}
}

func TestUnderASampledOutSpanUpdateSaysSoAtDebug(t *testing.T) {
	rec := tracepadtest.Capture(t)
	out := logs(t)
	ctx, step := tracepad.Span(sampledOut(), "dropped")
	tracepad.Update(ctx, tracepad.WithOutput("x"))
	step.End()
	if got := levels(out); !reflect.DeepEqual(got, []string{"DEBUG"}) {
		t.Errorf("logged %v, want one debug line:\n%s", got, out)
	}
	if len(rec.Spans()) != 0 {
		t.Errorf("spans = %d, want none", len(rec.Spans()))
	}
}

func TestWithTracingOffUpdateOutsideEveryBlockIsQuiet(t *testing.T) {
	tracepadtest.Reset(t)
	out := logs(t)
	tracepad.Update(context.Background(), tracepad.WithOutput("x"))
	if strings.Contains(out.String(), "level=WARN") {
		t.Errorf("warned:\n%s", out)
	}
}

func TestAProcessThatTracesStillWarnsOutsideEveryBlock(t *testing.T) {
	tracepadtest.Capture(t)
	out := logs(t)
	tracepad.Update(context.Background(), tracepad.WithOutput("x"))
	tracepad.UpdateTrace(context.Background(), tracepad.WithUserID("u-1"))
	if got := levels(out); !reflect.DeepEqual(got, []string{"WARN", "WARN"}) ||
		!strings.Contains(out.String(), "tracepad.Update outside a span") ||
		!strings.Contains(out.String(), "tracepad.UpdateTrace outside a span") {
		t.Errorf("logged %v, want two warnings:\n%s", got, out)
	}
}

// silentStore is a store that takes the request and does not answer it
// until the test is over.
func silentStore(t *testing.T) string {
	t.Helper()
	over := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-over:
		}
	}))
	t.Cleanup(func() {
		close(over)
		server.Close()
	})
	return server.URL
}

// exportTakes inits against a store that never answers, and times a flush
// with ten seconds of patience: how long one export is allowed to take.
func exportTakes(t *testing.T, opts ...tracepad.Option) time.Duration {
	t.Helper()
	opts = append([]tracepad.Option{tracepad.WithHost(silentStore(t)), tracepad.WithKey("tp-sk-test")}, opts...)
	if _, err := tracepad.Init(context.Background(), opts...); err != nil {
		t.Fatal(err)
	}
	_, step := tracepad.Span(context.Background(), "pending")
	step.End()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	if err := tracepad.Flush(ctx); err == nil {
		t.Error("Flush against a store that never answers returned no error")
	}
	return time.Since(started)
}

func TestTheExportTimeoutReachesTheExporter(t *testing.T) {
	tracepadtest.Reset(t)
	if took := exportTakes(t, tracepad.WithExportTimeout(100*time.Millisecond)); took > 2*time.Second {
		t.Errorf("the export took %v, want about 100ms", took)
	}
}

func TestTheEnvironmentNamesIt(t *testing.T) {
	tracepadtest.Reset(t)
	t.Setenv("TRACEPAD_EXPORT_TIMEOUT", "0.1")
	if took := exportTakes(t); took > 2*time.Second {
		t.Errorf("the export took %v, want about 100ms", took)
	}
}

func TestTheOptionWinsOverTheEnvironment(t *testing.T) {
	tracepadtest.Reset(t)
	t.Setenv("TRACEPAD_EXPORT_TIMEOUT", "30")
	if took := exportTakes(t, tracepad.WithExportTimeout(100*time.Millisecond)); took > 2*time.Second {
		t.Errorf("the export took %v, want about 100ms", took)
	}
}

func TestAVariableThatIsNotSecondsIsIgnoredWithAWarning(t *testing.T) {
	tracepadtest.Reset(t)
	t.Setenv("TRACEPAD_EXPORT_TIMEOUT", "5s")
	out := logs(t)
	if _, err := tracepad.Init(context.Background(), tracepad.WithHost("http://tracepad.test:4318"), tracepad.WithKey("tp-sk-test")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "TRACEPAD_EXPORT_TIMEOUT is not a number of seconds") {
		t.Errorf("logged:\n%s", out)
	}
}

func TestTheExportTimeoutIsIgnoredWithAWarningWhenNothingIsExported(t *testing.T) {
	tracepadtest.Reset(t)
	out := logs(t)
	if _, err := tracepad.Init(context.Background(), tracepad.WithHost("http://tracepad.test:4318"), tracepad.WithKey("tp-sk-test"),
		tracepad.WithExport(false), tracepad.WithExportTimeout(time.Second)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "WithExportTimeout is ignored with WithExport(false)") {
		t.Errorf("logged:\n%s", out)
	}
}

// Flush keeps to its context against a store that never answers (Decision
// 4): the export goes on under its own timeout, and the caller has the error.
func TestFlushReturnsWithinItsContextAgainstAStoreThatNeverAnswers(t *testing.T) {
	tracepadtest.Reset(t)
	if _, err := tracepad.Init(context.Background(), tracepad.WithHost(silentStore(t)), tracepad.WithKey("tp-sk-test")); err != nil {
		t.Fatal(err)
	}
	_, step := tracepad.Span(context.Background(), "pending")
	step.End()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := tracepad.Flush(ctx)
	if took := time.Since(started); took > 200*time.Millisecond+300*time.Millisecond {
		t.Errorf("Flush took %v, want at most its 200ms and a margin", took)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want the context's deadline", err)
	}
}

func TestMetadataMergesByKey(t *testing.T) {
	rec := tracepadtest.Capture(t)
	ctx, step := tracepad.Span(context.Background(), "step",
		tracepad.WithMetadata(map[string]any{"a": 1, "keep": "x"}))
	step.Update(tracepad.WithMetadata(map[string]any{"b": map[string]bool{"nested": true}}))
	tracepad.Update(ctx, tracepad.WithMetadata(map[string]any{"a": 3, "keep": nil}))
	step.End()
	_, typed := tracepad.Span(context.Background(), "typed", tracepad.WithMetadata(struct {
		Attempt int    `json:"attempt"`
		Region  string `json:"region"`
	}{2, "eu"}))
	typed.End()

	metadata := func(name string) map[string]attribute.Value {
		out := map[string]attribute.Value{}
		for key, value := range rec.Attributes(t, name) {
			if strings.HasPrefix(key, "tracepad.observation.metadata") {
				out[key] = value
			}
		}
		return out
	}
	if got, want := metadata("step"), map[string]attribute.Value{
		"tracepad.observation.metadata.a":    attribute.Int64Value(3),
		"tracepad.observation.metadata.keep": attribute.StringValue("x"), // nil writes nothing, and deletes nothing
		"tracepad.observation.metadata.b":    attribute.StringValue(`{"nested":true}`),
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("step metadata = %v, want %v", got, want)
	}
	if got, want := metadata("typed"), map[string]attribute.Value{
		"tracepad.observation.metadata.attempt": attribute.Int64Value(2),
		"tracepad.observation.metadata.region":  attribute.StringValue("eu"),
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("typed metadata = %v, want %v", got, want)
	}
}

// Found in review of PR #83.
func TestMetadataValuesKeepTheirTypeAndNothingIsNothing(t *testing.T) {
	rec := tracepadtest.Capture(t)
	var unset *string
	_, step := tracepad.Span(context.Background(), "step", tracepad.WithMetadata(map[string]any{
		"reason": unset, // a typed nil: nothing, not "null"
		"at":     time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC),
	}))
	step.End()
	_, typed := tracepad.Span(context.Background(), "typed", tracepad.WithMetadata(struct {
		TenantID int64 `json:"tenant_id"`
	}{9007199254740993}))
	typed.End()

	got := rec.Attributes(t, "step")
	if _, ok := got["tracepad.observation.metadata.reason"]; ok {
		t.Errorf("a nil *string was written: %v", got["tracepad.observation.metadata.reason"])
	}
	if at := got["tracepad.observation.metadata.at"].AsString(); at != "2026-09-23T10:00:00Z" {
		t.Errorf("at = %q, want the time without its quotes", at)
	}
	if id := rec.Attributes(t, "typed")["tracepad.observation.metadata.tenant_id"]; id != attribute.Int64Value(9007199254740993) {
		t.Errorf("tenant_id = %v, want the exact integer", id.Emit())
	}
}

func TestANegativeExportTimeoutIsIgnoredWithAWarning(t *testing.T) {
	tracepadtest.Reset(t)
	out := logs(t)
	if _, err := tracepad.Init(context.Background(), tracepad.WithHost("http://tracepad.test:4318"), tracepad.WithKey("tp-sk-test"),
		tracepad.WithExportTimeout(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "WithExportTimeout is not a positive duration") {
		t.Errorf("logged:\n%s", out)
	}
}

// Found in the second review of PR #83.
func TestMetadataPastTheKeyCapOrWithAnEmptyKeyIsWrittenWhole(t *testing.T) {
	rec := tracepadtest.Capture(t)
	many := map[string]any{}
	for i := range 33 {
		many[fmt.Sprintf("k%02d", i)] = i
	}
	_, call := tracepad.Generation(context.Background(), "chat", tracepad.WithModel("m"), tracepad.WithMetadata(many))
	call.End(tracepad.Result{Output: "done"})
	_, empty := tracepad.Span(context.Background(), "empty", tracepad.WithMetadata(map[string]any{"": 1, "a": 2}))
	empty.End()

	chat := rec.Attributes(t, "chat")
	if _, ok := chat["tracepad.observation.metadata.k00"]; ok {
		t.Error("33 keys were split, want them written whole")
	}
	if got := chat["tracepad.observation.metadata"].AsString(); !strings.HasPrefix(got, `{"k00":0,`) {
		t.Errorf("metadata = %q", got)
	}
	if chat["gen_ai.output.messages"].AsString() != "done" {
		t.Error("End's output was dropped")
	}
	if got := rec.Attributes(t, "empty")["tracepad.observation.metadata"].AsString(); got != `{"":1,"a":2}` {
		t.Errorf("metadata = %q", got)
	}
}

// The batch processor's own variable is the operator's, and stays in force.
func TestOpenTelemetrysBatchExportTimeoutIsKept(t *testing.T) {
	tracepadtest.Reset(t)
	t.Setenv("OTEL_BSP_EXPORT_TIMEOUT", "100") // milliseconds
	if took := exportTakes(t, tracepad.WithExportTimeout(5*time.Second)); took > 2*time.Second {
		t.Errorf("the export took %v, want the batch processor's 100ms", took)
	}
}
