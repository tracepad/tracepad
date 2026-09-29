package tracepad

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// sender records every batch it was handed and refuses the first `failures`.
type sender struct {
	mu       sync.Mutex
	batches  [][]map[string]any
	failures int
}

func (s *sender) send(_ context.Context, batch []map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batches = append(s.batches, append([]map[string]any(nil), batch...))
	if len(s.batches) <= s.failures {
		return &HTTPError{Status: 400, Body: `{"error":"score 3: unknown field \"commet\""}`}
	}
	return nil
}

func (s *sender) sizes() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int
	for _, b := range s.batches {
		out = append(out, len(b))
	}
	return out
}

// queue replaces the default's queue with one over the sender; `after` is
// the interval clock, nil for one that never fires.
func queue(t *testing.T, s *sender, after func(time.Duration) <-chan time.Time) *scoreQueue {
	t.Helper()
	q := newScoreQueue(s.send)
	if after == nil {
		after = func(time.Duration) <-chan time.Time { return nil }
	}
	q.after = after
	// The old queue is closed outside the lock, as reset does: its goroutine
	// reads the configuration under it.
	def.mu.Lock()
	previous := def.scores
	def.scores = q
	def.mu.Unlock()
	if previous != nil {
		_ = previous.close(context.Background())
	}
	return q
}

func flushed(t *testing.T, q *scoreQueue) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := q.flush(ctx); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

func TestAScoreInsideASpanTargetsItsTrace(t *testing.T) {
	setup(t)
	s := &sender{}
	q := queue(t, s, nil)
	ctx, step := Span(context.Background(), "handler")
	if err := Score(ctx, "helpful", WithValue(0.9), WithComment("cited the source")); err != nil {
		t.Fatal(err)
	}
	step.End()
	flushed(t, q)

	if len(s.batches) != 1 || len(s.batches[0]) != 1 {
		t.Fatalf("batches = %v", s.batches)
	}
	got := s.batches[0][0]
	if got["name"] != "helpful" || got["trace_id"] != step.TraceID() || got["value"] != 0.9 ||
		got["comment"] != "cited the source" || len(got) != 4 {
		t.Errorf("score = %v", got)
	}
}

func TestOnObservationAddsTheSpanID(t *testing.T) {
	setup(t)
	s := &sender{}
	q := queue(t, s, nil)
	ctx, step := Span(context.Background(), "handler")
	_ = Score(ctx, "grounded", WithValue(1), WithDataType("boolean"), OnObservation())
	step.End()
	flushed(t, q)
	got := s.batches[0][0]
	if got["observation_id"] != step.SpanID() || got["data_type"] != "boolean" {
		t.Errorf("score = %v", got)
	}
}

func TestAnExplicitTargetNeedsNoSpan(t *testing.T) {
	setup(t)
	s := &sender{}
	q := queue(t, s, nil)
	err := Score(context.Background(), "verdict", WithStringValue("pass"), WithDataType("categorical"),
		WithTraceID(strings.Repeat("a", 32)), WithID(strings.Repeat("b", 32)), WithObservationID("c"))
	if err != nil {
		t.Fatal(err)
	}
	flushed(t, q)
	got := s.batches[0][0]
	if got["trace_id"] != strings.Repeat("a", 32) || got["id"] != strings.Repeat("b", 32) ||
		got["string_value"] != "pass" || got["observation_id"] != "c" {
		t.Errorf("score = %v", got)
	}
}

func TestOutsideASpanWithoutATraceIDIsErrNoTrace(t *testing.T) {
	setup(t)
	if err := Score(context.Background(), "helpful", WithValue(1)); !errors.Is(err, ErrNoTrace) {
		t.Errorf("err = %v", err)
	}
}

// withoutInit is a process that never called Init (spec 039): the global
// provider is a no-op, and the default logger — where the package logs before
// Init — is recorded at debug level. A global still at the API's default is
// left alone: the first SetTracerProvider in a binary delegates that default
// for good, and a test should not be what spends it.
func withoutInit(t *testing.T) *bytes.Buffer {
	t.Helper()
	fresh(t)
	if !tracingOff() {
		previous := otel.GetTracerProvider()
		otel.SetTracerProvider(noop.NewTracerProvider())
		t.Cleanup(func() { otel.SetTracerProvider(previous) })
	}
	logger, logs := slog.Default(), &bytes.Buffer{}
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(logger) })
	return logs
}

func TestWithTracingOffAScoreWithoutATargetIsDropped(t *testing.T) {
	logs := withoutInit(t)
	s := &sender{}
	q := queue(t, s, nil)
	ctx, step := Span(context.Background(), "handler")
	if err := Score(ctx, "helpful", WithValue(1)); err != nil {
		t.Errorf("inside a span: err = %v, want nil", err)
	}
	step.End()
	if err := Score(context.Background(), "helpful", WithValue(1), OnObservation()); err != nil {
		t.Errorf("outside every span: err = %v, want nil", err)
	}
	flushed(t, q)
	if len(s.batches) != 0 {
		t.Errorf("batches = %v, want nothing enqueued", s.batches)
	}
	if n := strings.Count(logs.String(), "level=DEBUG msg=\"tracepad.Score: tracing is off"); n != 2 {
		t.Errorf("logs = %q, want two debug lines", logs.String())
	}
	// No trace behind the span, no id (spec 039 #3).
	if step.TraceID() != "" || step.SpanID() != "" {
		t.Errorf("ids = %q, %q, want empty", step.TraceID(), step.SpanID())
	}
}

// A request came in with a traceparent: the no-op tracer hands the caller's
// context to every child. Those are the caller's ids, not the step's, and
// nothing of this process is stored under them (spec 039 #3).
func TestWithTracingOffAPropagatedParentIsNotATrace(t *testing.T) {
	logs := withoutInit(t)
	s := &sender{}
	q := queue(t, s, nil)
	caller := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{0xa0, 0xb1}, SpanID: trace.SpanID{0xc2, 0xd3},
		TraceFlags: trace.FlagsSampled, Remote: true,
	})
	ctx, step := Span(trace.ContextWithRemoteSpanContext(context.Background(), caller), "handler")
	if step.Span().SpanContext().SpanID() != caller.SpanID() {
		t.Fatal("the no-op tracer did not echo the caller: the test proves nothing")
	}
	if err := Score(ctx, "helpful", WithValue(1)); err != nil {
		t.Errorf("err = %v", err)
	}
	step.End()
	flushed(t, q)
	if step.TraceID() != "" || step.SpanID() != "" {
		t.Errorf("ids = %q, %q, want empty", step.TraceID(), step.SpanID())
	}
	if len(s.batches) != 0 || !strings.Contains(logs.String(), "tracing is off") {
		t.Errorf("batches = %v, logs = %q", s.batches, logs.String())
	}
}

// The spec's first edge case: tracing is on, just not through us — the spans
// record, the ids are real, and a score inside one is enqueued.
func TestWithoutInitTheApplicationsOwnProviderStillScores(t *testing.T) {
	withoutInit(t)
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	otel.SetTracerProvider(provider)
	t.Setenv("TRACEPAD_URL", testHost)
	t.Setenv("TRACEPAD_API_KEY", testKey)
	s := &sender{}
	q := queue(t, s, nil)
	ctx, step := Span(context.Background(), "handler")
	if err := Score(ctx, "helpful", WithValue(1)); err != nil {
		t.Fatal(err)
	}
	step.End()
	// After the span ended its trace is as real as before (spec 039 #7).
	if err := Score(ctx, "late", WithValue(1)); err != nil {
		t.Fatal(err)
	}
	flushed(t, q)
	if len(step.TraceID()) != 32 || len(s.batches) != 1 || len(s.batches[0]) != 2 ||
		s.batches[0][0]["trace_id"] != step.TraceID() || s.batches[0][1]["trace_id"] != step.TraceID() {
		t.Errorf("trace id = %q, batches = %v", step.TraceID(), s.batches)
	}
	// Tracing is on here, so outside every span is the programming error it
	// always was.
	if err := Score(context.Background(), "helpful", WithValue(1)); !errors.Is(err, ErrNoTrace) {
		t.Errorf("outside every span: err = %v, want ErrNoTrace", err)
	}
}

// A sampler that drops a trace leaves its ids real: they are propagated and
// correlated whether or not the spans are kept (spec 039 #7).
func TestWithoutInitASampledOutSpanKeepsItsIDs(t *testing.T) {
	withoutInit(t)
	provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sdktrace.NeverSample()))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	otel.SetTracerProvider(provider)
	_, step := Span(context.Background(), "handler")
	step.End()
	if step.Span().IsRecording() || len(step.TraceID()) != 32 || len(step.SpanID()) != 16 {
		t.Errorf("recording = %v, ids = %q, %q", step.Span().IsRecording(), step.TraceID(), step.SpanID())
	}
}

// With tracing off an Observation's TraceID is "", and handing it back is
// naming no target: dropped, as in the other two packages.
func TestWithTracingOffAnEmptyTraceIDIsNoTarget(t *testing.T) {
	withoutInit(t)
	t.Setenv("TRACEPAD_URL", testHost)
	t.Setenv("TRACEPAD_API_KEY", testKey)
	s := &sender{}
	q := queue(t, s, nil)
	_, step := Span(context.Background(), "handler")
	step.End()
	if err := Score(context.Background(), "x", WithValue(1), WithTraceID(step.TraceID()), WithObservationID(step.SpanID())); err != nil {
		t.Fatal(err)
	}
	flushed(t, q)
	if len(s.batches) != 0 {
		t.Errorf("batches = %v, want nothing", s.batches)
	}
}

// A provider the application wired into its framework without registering it
// globally: the handler's span records, so a score in its context goes to
// that trace — tracing is on here, whatever the global says (spec 039 #8).
func TestWithoutInitAnUnregisteredProvidersSpanStillScores(t *testing.T) {
	withoutInit(t)
	t.Setenv("TRACEPAD_URL", testHost)
	t.Setenv("TRACEPAD_API_KEY", testKey)
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	s := &sender{}
	q := queue(t, s, nil)
	ctx, request := provider.Tracer("the.framework").Start(context.Background(), "GET /answer")
	if err := Score(ctx, "helpful", WithValue(1)); err != nil {
		t.Fatal(err)
	}
	request.End()
	flushed(t, q)
	if len(s.batches) != 1 || s.batches[0][0]["trace_id"] != request.SpanContext().TraceID().String() {
		t.Errorf("batches = %v", s.batches)
	}
}

// The trace package's deprecated no-op is as off as the noop package's.
func TestTheDeprecatedNoopProviderIsTracingOff(t *testing.T) {
	withoutInit(t)
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(trace.NewNoopTracerProvider()) //nolint:staticcheck
	t.Cleanup(func() { otel.SetTracerProvider(previous) })
	ctx, step := Span(context.Background(), "handler")
	if err := Score(ctx, "helpful", WithValue(1)); err != nil || step.TraceID() != "" {
		t.Errorf("err = %v, trace id = %q", err, step.TraceID())
	}
}

// With tracing on, an empty WithTraceID is a target that names nothing — an
// id stored while tracing was off, a missing column — and not a licence to
// score the context's trace instead.
func TestWithTracingOnAnEmptyTraceIDIsErrNoTrace(t *testing.T) {
	setup(t)
	ctx, step := Span(context.Background(), "handler")
	defer step.End()
	if err := Score(ctx, "judge", WithValue(1), WithTraceID("")); !errors.Is(err, ErrNoTrace) {
		t.Errorf("err = %v, want ErrNoTrace", err)
	}
}

func TestWithTracingOffAScoreByIDIsSent(t *testing.T) {
	withoutInit(t)
	// By id it is REST, not tracing (spec 039 #2): posted with the store the
	// environment names, as a judge script that traces nothing does.
	t.Setenv("TRACEPAD_URL", testHost)
	t.Setenv("TRACEPAD_API_KEY", testKey)
	s := &sender{}
	q := queue(t, s, nil)
	if err := Score(context.Background(), "helpful", WithValue(1), WithTraceID(strings.Repeat("a", 32))); err != nil {
		t.Fatal(err)
	}
	flushed(t, q)
	if len(s.batches) != 1 || s.batches[0][0]["trace_id"] != strings.Repeat("a", 32) {
		t.Errorf("batches = %v", s.batches)
	}
}

func TestTheBatchClosesAtAHundred(t *testing.T) {
	setup(t)
	s := &sender{}
	q := queue(t, s, nil)
	for i := 0; i < 150; i++ {
		_ = Score(context.Background(), "s", WithValue(1), WithTraceID(strings.Repeat("a", 32)))
	}
	flushed(t, q)
	if got := s.sizes(); len(got) != 2 || got[0] != 100 || got[1] != 50 {
		t.Errorf("batch sizes = %v, want [100 50]", got)
	}
}

func TestTheBatchClosesAtTheInterval(t *testing.T) {
	setup(t)
	s := &sender{}
	q := queue(t, s, time.After)
	q.interval = 20 * time.Millisecond
	_ = Score(context.Background(), "first", WithValue(1), WithTraceID(strings.Repeat("a", 32)))
	_ = Score(context.Background(), "second", WithValue(1), WithTraceID(strings.Repeat("a", 32)))
	// No flush: the interval alone closes the batch, with both in it.
	deadline := time.Now().Add(2 * time.Second)
	for len(s.sizes()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := s.sizes(); len(got) != 1 || got[0] != 2 {
		t.Errorf("batch sizes = %v, want [2]", got)
	}
}

func TestARejectedBatchIsRetriedOnceThenDropped(t *testing.T) {
	r := setup(t)
	s := &sender{failures: 2}
	q := queue(t, s, nil)
	_ = Score(context.Background(), "helpful", WithValue(1), WithTraceID(strings.Repeat("a", 32)))
	flushed(t, q)
	if got := s.sizes(); len(got) != 2 {
		t.Fatalf("batches = %v, want the attempt and one retry", got)
	}
	if logs := r.logs.String(); !strings.Contains(logs, "scores dropped") || !strings.Contains(logs, "commet") {
		t.Errorf("logs = %q, want the drop and the server's message", logs)
	}
}

func TestOneFailureIsSurvived(t *testing.T) {
	r := setup(t)
	s := &sender{failures: 1}
	q := queue(t, s, nil)
	_ = Score(context.Background(), "helpful", WithValue(1), WithTraceID(strings.Repeat("a", 32)))
	flushed(t, q)
	if got := s.sizes(); len(got) != 2 || strings.Contains(r.logs.String(), "dropped") {
		t.Errorf("batches = %v, logs = %q", got, r.logs.String())
	}
}

func TestAScoreAfterTheQueueClosedIsLoggedAsDropped(t *testing.T) {
	r := setup(t)
	s := &sender{}
	q := queue(t, s, nil)
	_ = q.close(context.Background())
	_ = Score(context.Background(), "late", WithValue(1), WithTraceID(strings.Repeat("a", 32)))
	if !strings.Contains(r.logs.String(), "the queue is closed") || len(s.batches) != 0 {
		t.Errorf("logs = %q, batches = %v", r.logs.String(), s.batches)
	}
}

func TestFlushDrainsTheScoresThenTheSpans(t *testing.T) {
	r := setup(t)
	s := &sender{}
	queue(t, s, nil)
	_ = Score(context.Background(), "s", WithValue(1), WithTraceID(strings.Repeat("a", 32)))
	_, step := Span(context.Background(), "s")
	step.End()
	if err := Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.batches) != 1 || len(r.spans()) != 1 {
		t.Errorf("batches = %d, spans = %d", len(s.batches), len(r.spans()))
	}
}

func TestFlushGivesUpWhenTheContextDoes(t *testing.T) {
	setup(t)
	blocked := make(chan struct{})
	s := &sender{}
	q := newScoreQueue(func(ctx context.Context, batch []map[string]any) error {
		<-blocked
		return s.send(ctx, batch)
	})
	q.after = func(time.Duration) <-chan time.Time { return nil }
	def.mu.Lock()
	def.scores = q
	def.mu.Unlock()
	_ = Score(context.Background(), "s", WithValue(1), WithTraceID(strings.Repeat("a", 32)))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := Flush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
	close(blocked)
	_ = q.close(context.Background())
}
