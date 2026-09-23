package tracepadtest_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	tracepad "github.com/tracepad/tracepad/sdk/go"
	"github.com/tracepad/tracepad/sdk/go/tracepadtest"
)

func TestACaptureRecordsTheSpansInOrderAndTheScores(t *testing.T) {
	rec := tracepadtest.Capture(t)
	ctx, handler := tracepad.Span(context.Background(), "handler")
	_, retrieve := tracepad.Span(ctx, "retrieve")
	retrieve.End()
	if err := tracepad.Score(ctx, "helpful", tracepad.WithValue(0.9)); err != nil {
		t.Fatal(err)
	}
	handler.End()

	var names []string
	for _, s := range rec.Spans() {
		names = append(names, s.Name)
	}
	if !reflect.DeepEqual(names, []string{"retrieve", "handler"}) {
		t.Errorf("spans = %v, want retrieve then handler", names)
	}
	if got := rec.Attributes(t, "handler")["tracepad.observation.type"].AsString(); got != "span" {
		t.Errorf("type = %q, want span", got)
	}
	want := []map[string]any{{"name": "helpful", "trace_id": handler.TraceID(), "value": 0.9}}
	if got := rec.Scores(); !reflect.DeepEqual(got, want) {
		t.Errorf("scores = %v, want %v", got, want)
	}
}

// fatal is a testing.TB whose Fatalf is recorded; the helper's goroutine
// ends there, as it would in a test.
type fatal struct {
	testing.TB
	message string
}

func (f *fatal) Helper() {}

func (f *fatal) Fatalf(format string, args ...any) {
	f.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func failure(t *testing.T, call func(testing.TB)) string {
	f := &fatal{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		call(f)
	}()
	<-done
	return f.message
}

func TestOneFailsNamingTheSpansThereWere(t *testing.T) {
	rec := tracepadtest.Capture(t)
	for _, name := range []string{"a", "a", "b"} {
		_, s := tracepad.Span(context.Background(), name)
		s.End()
	}
	if got := failure(t, func(tb testing.TB) { rec.One(tb, "a") }); got != `want one span named "a", got ["a" "a" "b"]` {
		t.Errorf("failure = %s", got)
	}
	if got := failure(t, func(tb testing.TB) { rec.One(tb, "c") }); !strings.Contains(got, `named "c"`) {
		t.Errorf("failure = %s", got)
	}
}

type refuse struct{ t *testing.T }

func (r refuse) RoundTrip(req *http.Request) (*http.Response, error) {
	r.t.Errorf("a capture made a network call: %s %s", req.Method, req.URL)
	return nil, errors.New("refused")
}

func TestNothingReachesTheNetwork(t *testing.T) {
	previous := http.DefaultTransport
	http.DefaultTransport = refuse{t}
	t.Cleanup(func() { http.DefaultTransport = previous })
	rec := tracepadtest.Capture(t)
	ctx, call := tracepad.Generation(context.Background(), "chat", tracepad.WithModel("gpt-4o-mini"))
	if err := tracepad.Score(ctx, "helpful", tracepad.WithValue(1), tracepad.OnObservation()); err != nil {
		t.Fatal(err)
	}
	call.End(tracepad.Result{})
	if err := tracepad.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec.One(t, "chat")
	if len(rec.Scores()) != 1 {
		t.Errorf("scores = %v, want one", rec.Scores())
	}
}

// tracingOff asserts spec 039's state: a score without a target inside a
// span is dropped without an error, and the span has no ids.
func tracingOff(t *testing.T) {
	t.Helper()
	ctx, step := tracepad.Span(context.Background(), "after")
	defer step.End()
	if err := tracepad.Score(ctx, "helpful", tracepad.WithValue(1)); err != nil {
		t.Errorf("Score = %v, want nil with tracing off", err)
	}
	if step.TraceID() != "" || step.SpanID() != "" {
		t.Errorf("ids = %q %q, want none", step.TraceID(), step.SpanID())
	}
}

func TestLeavingACaptureLeavesAProcessThatNeverInitialised(t *testing.T) {
	t.Run("capture", func(t *testing.T) { tracepadtest.Capture(t) })
	tracingOff(t)
}

func TestCapturesInSequentialTestsSeeOnlyTheirOwn(t *testing.T) {
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			rec := tracepadtest.Capture(t)
			ctx, s := tracepad.Span(context.Background(), name)
			_ = tracepad.Score(ctx, name)
			s.End()
			if len(rec.Spans()) != 1 || rec.Spans()[0].Name != name || len(rec.Scores()) != 1 {
				t.Errorf("%s saw %v and %v", name, rec.Spans(), rec.Scores())
			}
		})
	}
}

// appTracer is taken when the binary starts, before any provider is set, as
// an application's package-level tracer is.
var appTracer = otel.Tracer("app")

func TestATracerTakenAtStartRecordsInEveryCapture(t *testing.T) {
	for _, name := range []string{"first", "second"} {
		t.Run(name, func(t *testing.T) {
			rec := tracepadtest.Capture(t)
			ctx, root := appTracer.Start(context.Background(), name)
			_, step := tracepad.Span(ctx, "step")
			step.End()
			root.End()
			if got := rec.One(t, "step").Parent.SpanID(); got != rec.One(t, name).SpanContext.SpanID() {
				t.Errorf("step's parent = %s, want the application's span", got)
			}
		})
	}
}

// The reset replaces the global provider and takes back what Init set (spec
// 040 #3, #9, #14): an Init after it builds a provider, as in a fresh
// process, and the next reset shuts that one down.
func TestAfterTheResetInitBuildsAProviderAndTheNextResetShutsItDown(t *testing.T) {
	t.Run("capture", func(t *testing.T) { tracepadtest.Capture(t) })
	tracepadtest.Reset(t)
	if _, err := tracepad.Init(context.Background(), tracepad.WithHost("http://tracepad.test:4318"),
		tracepad.WithKey("tp-sk-test"), tracepad.WithExport(false)); err != nil {
		t.Fatal(err)
	}
	built, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider)
	if !ok {
		t.Fatalf("global provider = %T, want the one Init built", otel.GetTracerProvider())
	}
	if _, span := appTracer.Start(context.Background(), "app"); !span.IsRecording() {
		t.Error("the tracer taken at start does not follow the provider Init built")
	}
	tracepadtest.Reset(t)
	if _, span := built.Tracer("app").Start(context.Background(), "after"); span.IsRecording() {
		t.Error("the provider Init built is still running after the reset")
	}
	if fields := otel.GetTextMapPropagator().Fields(); len(fields) != 0 {
		t.Errorf("propagator fields = %v, want Init's taken back", fields)
	}
}

func TestWhatWasCapturedStaysReadableAfterTheTest(t *testing.T) {
	var rec *tracepadtest.Recorder
	t.Run("capture", func(t *testing.T) {
		rec = tracepadtest.Capture(t)
		_, s := tracepad.Span(context.Background(), "kept")
		s.End()
	})
	rec.One(t, "kept")
}

func TestACapturePropagatesTheTraceContext(t *testing.T) {
	tracepadtest.Capture(t)
	ctx, s := tracepad.Span(context.Background(), "call")
	defer s.End()
	headers := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, headers)
	if headers["traceparent"] == "" {
		t.Errorf("headers = %v, want a traceparent", headers)
	}
}

func TestACaptureReadsNothingFromTheEnvironment(t *testing.T) {
	t.Setenv("TRACEPAD_ENVIRONMENT", "ci")
	t.Setenv("TRACEPAD_RELEASE", "1.2.3")
	logs := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	tracepadtest.Capture(t)
	if logs.Len() != 0 {
		t.Errorf("the capture logged:\n%s", logs)
	}
}

func TestTheResetDropsWhatTheQueueHeld(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { posts.Add(1) }))
	t.Cleanup(server.Close)
	t.Setenv("TRACEPAD_HOST", server.URL)
	t.Setenv("TRACEPAD_API_KEY", "tp-sk-test")
	tracepadtest.Reset(t)
	if err := tracepad.Score(context.Background(), "pending", tracepad.WithTraceID(strings.Repeat("a", 32))); err != nil {
		t.Fatal(err)
	}
	tracepadtest.Reset(t)
	if err := tracepad.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 0 {
		t.Errorf("posts = %d, want the pending score dropped", posts.Load())
	}
}

func TestTheResetNeitherWaitsForASendInFlightNorWarns(t *testing.T) {
	arrived, release := make(chan struct{}, 2), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		arrived <- struct{}{}
		<-release
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	t.Setenv("TRACEPAD_HOST", server.URL)
	t.Setenv("TRACEPAD_API_KEY", "tp-sk-test")
	logs := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	tracepadtest.Reset(t)
	_ = tracepad.Score(context.Background(), "in flight", tracepad.WithTraceID(strings.Repeat("a", 32)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = tracepad.Flush(ctx) }()
	<-arrived
	started := time.Now()
	tracepadtest.Reset(t)
	if waited := time.Since(started); waited > time.Second {
		t.Errorf("the reset waited %s for a send in flight", waited)
	}
	if logs.Len() != 0 {
		t.Errorf("the reset logged:\n%s", logs)
	}
}

// A TestMain that initialises before the first capture binds a tracer taken
// at start-up to the provider it builds, unless the follower came first.
func TestAnInitBeforeTheFirstCaptureDoesNotKeepTheTracers(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestChildInitFirst$", "-test.v")
	cmd.Env = append(os.Environ(), "TRACEPADTEST_CHILD=TestChildInitFirst")
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "--- PASS: TestChildInitFirst") {
		t.Fatalf("child: %v\n%s", err, out)
	}
}

func TestChildInitFirst(t *testing.T) {
	only(t)
	if _, err := tracepad.Init(context.Background(), tracepad.WithHost("http://tracepad.test:4318"),
		tracepad.WithKey("tp-sk-test"), tracepad.WithExport(false)); err != nil {
		t.Fatal(err)
	}
	_, s := appTracer.Start(context.Background(), "under init")
	s.End()
	rec := tracepadtest.Capture(t)
	_, s = appTracer.Start(context.Background(), "captured")
	s.End()
	rec.One(t, "captured")
}

func TestResetAloneIsTracingOff(t *testing.T) {
	if _, err := tracepad.Init(context.Background(), tracepad.WithHost("http://tracepad.test:4318"),
		tracepad.WithKey("tp-sk-test"), tracepad.WithExport(false)); err != nil {
		t.Fatal(err)
	}
	tracepadtest.Reset(t)
	tracingOff(t)
}

// child runs one test of this binary in a process of its own — what it
// checks must fail a test, and fails that one.
func child(t *testing.T, name string) string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+name+"$", "-test.v")
	cmd.Env = append(os.Environ(), "TRACEPADTEST_CHILD="+name)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("%s passed; want it to fail\n%s", name, out)
	}
	return string(out)
}

func only(t *testing.T) {
	if os.Getenv("TRACEPADTEST_CHILD") != t.Name() {
		t.Skip("run by its parent in a process of its own")
	}
}

func TestAParallelTestIsFailed(t *testing.T) {
	out := child(t, "TestChildParallel")
	if !strings.Contains(out, "tracepadtest.Capture: the capture is process-wide and this test runs in parallel") {
		t.Errorf("child output:\n%s", out)
	}
}

func TestChildParallel(t *testing.T) {
	only(t)
	t.Parallel()
	tracepadtest.Capture(t)
}

func TestTheCleanupRunsOnFatal(t *testing.T) {
	out := child(t, "TestChildFatal")
	if !strings.Contains(out, "--- FAIL: TestChildFatal/fatal") || !strings.Contains(out, "--- PASS: TestChildFatal/after") {
		t.Errorf("child output:\n%s", out)
	}
}

func TestChildFatal(t *testing.T) {
	only(t)
	t.Run("fatal", func(t *testing.T) {
		tracepadtest.Capture(t)
		t.Fatal("the test fails with the capture open")
	})
	t.Run("after", tracingOff)
}
