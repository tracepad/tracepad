package tracepad

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// collector counts the OTLP exports it received, and checks their auth.
func collector(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var exports atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" || r.Header.Get("Authorization") != "Bearer "+testKey {
			t.Errorf("export to %s with %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		exports.Add(1)
		w.WriteHeader(200)
	}))
	t.Cleanup(server.Close)
	return server, &exports
}

func TestInitAdoptsTheApplicationsProvider(t *testing.T) {
	fresh(t)
	server, exports := collector(t)
	application := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(application)
	logs := &bytes.Buffer{}
	shutdown, err := Init(context.Background(), WithHost(server.URL), WithKey(testKey),
		WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() != application {
		t.Fatal("the application's provider was replaced")
	}
	_, step := Span(context.Background(), "s")
	step.End()
	if err := Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exports.Load() != 1 {
		t.Errorf("exports = %d, want the exporter registered on the application's provider", exports.Load())
	}
	// shutdown flushes but leaves an adopted provider running.
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, span := application.Tracer("x").Start(context.Background(), "after")
	if !span.IsRecording() {
		t.Error("an adopted provider must not be shut down")
	}
	span.End()
	if logs.Len() != 0 {
		t.Errorf("logs = %q, want none", logs.String())
	}
}

func TestEnvironmentAndReleaseAreRefusedOnAnAdoptedProvider(t *testing.T) {
	r := setup(t, WithEnvironment("staging"), WithRelease("2026.9.4"))
	if !strings.Contains(r.logs.String(), "environment and release are resource attributes") {
		t.Errorf("logs = %q", r.logs.String())
	}
}

func TestASecondInitIsANoOp(t *testing.T) {
	r := setup(t)
	first := def.shutdown
	shutdown, err := Init(context.Background(), WithHost("http://elsewhere.test"), WithKey("tp-sk-other"))
	if err != nil || def.config.host != testHost {
		t.Errorf("err = %v, host = %s", err, def.config.host)
	}
	if shutdown == nil || first == nil || !strings.Contains(r.logs.String(), "already run") {
		t.Errorf("logs = %q", r.logs.String())
	}
}

func TestNoHostAndNoKeyIsErrConfig(t *testing.T) {
	fresh(t)
	_, err := Init(context.Background())
	if !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "no host and no key") {
		t.Errorf("err = %v", err)
	}
	t.Setenv("TRACEPAD_URL", testHost+"/")
	if _, err := Init(context.Background()); !errors.Is(err, ErrConfig) || !strings.Contains(err.Error(), "no key") {
		t.Errorf("err = %v", err)
	}
	t.Setenv("TRACEPAD_API_KEY", testKey)
	t.Setenv("TRACEPAD_ENVIRONMENT", "staging")
	if _, err := Init(context.Background(), WithExport(false), WithTracerProvider(sdktrace.NewTracerProvider())); err != nil {
		t.Fatal(err)
	}
	if c := *def.config; c.host != testHost || c.key != testKey || c.environment != "staging" {
		t.Errorf("config = %+v, want it read from the environment, the host without its slash", c)
	}
}

func TestAProviderThatIsNotTheSDKsIsUsedButWarnedAbout(t *testing.T) {
	r := setup(t, WithTracerProvider(noop.NewTracerProvider()))
	if !strings.Contains(r.logs.String(), "not the OpenTelemetry SDK's") {
		t.Errorf("logs = %q", r.logs.String())
	}
	_, step := Span(context.Background(), "s")
	step.End()
	if len(r.spans()) != 0 {
		t.Error("spans went to the wrong provider")
	}
}

// TestInitBuildsAProviderWhenThereIsNone runs in a child process: the API's
// default provider exists only until something sets one, and every other
// test in this package has.
func TestInitBuildsAProviderWhenThereIsNone(t *testing.T) {
	if os.Getenv("TRACEPAD_TEST_BUILD") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestInitBuildsAProviderWhenThereIsNone$", "-test.v")
		cmd.Env = append(os.Environ(), "TRACEPAD_TEST_BUILD=1", "OTEL_SERVICE_NAME=support-bot",
			"TRACEPAD_URL=", "TRACEPAD_HOST=", "TRACEPAD_API_KEY=", "TRACEPAD_ENVIRONMENT=", "TRACEPAD_RELEASE=")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		return
	}
	pristine := otel.GetTracerProvider()
	if !isDefault(pristine) {
		t.Fatalf("the global provider is already %T", pristine)
	}
	server, exports := collector(t)
	shutdown, err := Init(context.Background(), WithHost(server.URL), WithKey(testKey),
		WithEnvironment("staging"), WithRelease("2026.9.4"))
	if err != nil {
		t.Fatal(err)
	}
	built, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider)
	if !ok {
		t.Fatalf("global provider = %T, want the one Init built", otel.GetTracerProvider())
	}
	exporter := tracetest.NewInMemoryExporter()
	built.RegisterSpanProcessor(sdktrace.NewSimpleSpanProcessor(exporter))
	_, probe := Span(context.Background(), "probe")
	probe.End()
	resource := map[string]string{}
	for _, kv := range exporter.GetSpans()[0].Resource.Attributes() {
		resource[string(kv.Key)] = kv.Value.Emit()
	}
	for key, want := range map[string]string{attrServiceName: "support-bot", attrEnvironment: "staging", attrServiceVersion: "2026.9.4"} {
		if resource[key] != want {
			t.Errorf("resource %s = %q, want %q", key, resource[key], want)
		}
	}
	if otel.GetTextMapPropagator().Fields()[0] != "traceparent" {
		t.Errorf("propagator fields = %v", otel.GetTextMapPropagator().Fields())
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if exports.Load() != 1 {
		t.Errorf("exports = %d", exports.Load())
	}
	// Built by the package, so shut down by it.
	if _, span := built.Tracer("x").Start(context.Background(), "after"); span.IsRecording() {
		t.Error("the provider Init built must be shut down by its shutdown")
	}
}

// A score written before Init is queued; Init keeps that queue rather than
// replacing it, so the shutdown it returns sends the score (review of PR
// #69).
func TestAScoreBeforeInitIsSentAtShutdown(t *testing.T) {
	fresh(t)
	var posted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/scores" {
			posted.Add(1)
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(server.Close)
	t.Setenv("TRACEPAD_URL", server.URL)
	t.Setenv("TRACEPAD_API_KEY", testKey)
	if err := Score(context.Background(), "early", WithValue(1), WithTraceID(strings.Repeat("a", 32))); err != nil {
		t.Fatal(err)
	}
	shutdown, err := Init(context.Background(), WithExport(false), WithTracerProvider(sdktrace.NewTracerProvider()))
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posted.Load() != 1 {
		t.Errorf("score batches posted at shutdown = %d, want 1", posted.Load())
	}
}

// The store away at shutdown: the score goroutine is still retrying, and
// shutdown keeps to the context's deadline rather than waiting it out
// (review of PR #69, round two).
func TestShutdownKeepsToTheDeadlineWhenTheStoreIsAway(t *testing.T) {
	setup(t)
	blocked := make(chan struct{})
	q := newScoreQueue(func(ctx context.Context, batch []map[string]any) error {
		<-blocked
		return nil
	})
	q.after = func(time.Duration) <-chan time.Time { return nil }
	def.mu.Lock()
	def.scores = q
	def.mu.Unlock()
	_ = Score(context.Background(), "s", WithValue(1), WithTraceID(strings.Repeat("a", 32)))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := def.shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 2*time.Second {
		t.Errorf("shutdown returned %v after %v", err, time.Since(started))
	}
	close(blocked)
}

// A score written before Init, with the store named by options rather than
// the environment, waits for Init and is sent at shutdown.
func TestAScoreBeforeInitWaitsForTheOptions(t *testing.T) {
	fresh(t)
	var posted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posted.Add(1)
		w.WriteHeader(200)
	}))
	t.Cleanup(server.Close)
	q := newScoreQueue(postScores)
	q.interval = time.Millisecond
	def.mu.Lock()
	def.scores = q
	def.mu.Unlock()
	_ = Score(context.Background(), "early", WithValue(1), WithTraceID(strings.Repeat("a", 32)))
	time.Sleep(20 * time.Millisecond) // past the interval: nothing must have been posted nowhere
	shutdown, err := Init(context.Background(), WithHost(server.URL), WithKey(testKey), WithExport(false),
		WithTracerProvider(sdktrace.NewTracerProvider()))
	if err != nil {
		t.Fatal(err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if posted.Load() != 1 {
		t.Errorf("posted = %d, want the early score sent once Init named the store", posted.Load())
	}
}

// A propagator the application composed before Init is kept.
func TestInitKeepsThePropagatorItFinds(t *testing.T) {
	fresh(t)
	composed := propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{})
	otel.SetTextMapPropagator(composed)
	t.Cleanup(func() { otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator()) })
	if _, err := Init(context.Background(), WithHost(testHost), WithKey(testKey), WithExport(false)); err != nil {
		t.Fatal(err)
	}
	if fields := otel.GetTextMapPropagator().Fields(); !strings.Contains(strings.Join(fields, " "), "baggage") {
		t.Errorf("propagator fields = %v, want the application's TraceContext and Baggage kept", fields)
	}
}

// Spans started while Init runs: no data race between the two (run under
// -race).
func TestSpansDuringInitDoNotRace(t *testing.T) {
	fresh(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			_, step := Span(context.Background(), "racing")
			step.End()
			def.log().Debug("racing")
		}
	}()
	_, err := Init(context.Background(), WithHost(testHost), WithKey(testKey), WithExport(false),
		WithTracerProvider(sdktrace.NewTracerProvider()), WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	<-done
	if err != nil {
		t.Fatal(err)
	}
}

// The export timeout's resolution (spec 042 #3): the option, then
// TRACEPAD_EXPORT_TIMEOUT in seconds, then five seconds — and nothing of the
// package's own when OpenTelemetry's variable is set, which then keeps its
// own meaning: in Go, the bound of one attempt. cost_test.go proves the
// value reaches the exporter against a store that never answers.
func TestTheExportTimeoutResolves(t *testing.T) {
	for _, variable := range []string{"TRACEPAD_EXPORT_TIMEOUT", "OTEL_EXPORTER_OTLP_TRACES_TIMEOUT", "OTEL_EXPORTER_OTLP_TIMEOUT"} {
		t.Setenv(variable, "")
	}
	if got := exportTimeout(0); got != 5*time.Second {
		t.Errorf("default = %v, want 5s", got)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_TIMEOUT", "7000")
	if got := exportTimeout(0); got != 0 {
		t.Errorf("under OpenTelemetry's variable = %v, want none of ours", got)
	}
	t.Setenv("TRACEPAD_EXPORT_TIMEOUT", "2.5")
	if got := exportTimeout(0); got != 2500*time.Millisecond {
		t.Errorf("from the environment = %v, want 2.5s", got)
	}
	if got := exportTimeout(1500 * time.Millisecond); got != 1500*time.Millisecond {
		t.Errorf("the option = %v, want it to win", got)
	}
}

// TestURLIsTheHostVariableAndHostIsTheDeprecatedSynonym pins spec 033 #20: the
// address of the store is TRACEPAD_URL, as the CLI and the server have it;
// TRACEPAD_HOST still works, says so once, and loses to TRACEPAD_URL.
func TestURLIsTheHostVariableAndHostIsTheDeprecatedSynonym(t *testing.T) {
	fresh(t)
	logs := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	t.Setenv("TRACEPAD_API_KEY", testKey)

	t.Setenv("TRACEPAD_URL", "http://from-url:4318/")
	c, err := resolve("", "", "", "")
	if err != nil || c.host != "http://from-url:4318" || logs.Len() != 0 {
		t.Fatalf("URL alone: host = %q, err = %v, logs = %q", c.host, err, logs)
	}

	t.Setenv("TRACEPAD_HOST", "http://from-host:4318")
	if c, _ = resolve("", "", "", ""); c.host != "http://from-url:4318" || logs.Len() != 0 {
		t.Errorf("URL and HOST: host = %q, logs = %q", c.host, logs)
	}

	t.Setenv("TRACEPAD_URL", "")
	for range 2 {
		if c, _ = resolve("", "", "", ""); c.host != "http://from-host:4318" {
			t.Errorf("HOST alone: host = %q", c.host)
		}
	}
	if n := strings.Count(logs.String(), "TRACEPAD_HOST is deprecated"); n != 1 {
		t.Errorf("the warning appeared %d times, want once: %q", n, logs)
	}

	if c, _ = resolve("http://argument:4318", "", "", ""); c.host != "http://argument:4318" {
		t.Errorf("an argument beats both: host = %q", c.host)
	}
}

// The warning goes to the logger Init was handed, not to the default one it
// falls back to before the options are applied.
func TestDeprecatedHostWarningUsesTheInitLogger(t *testing.T) {
	fresh(t)
	defaults := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(defaults, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	t.Setenv("TRACEPAD_HOST", "http://from-host:4318")
	t.Setenv("TRACEPAD_API_KEY", testKey)
	logs := &bytes.Buffer{}

	_, err := Init(context.Background(), WithExport(false), WithTracerProvider(sdktrace.NewTracerProvider()),
		WithLogger(slog.New(slog.NewTextHandler(logs, nil))))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "TRACEPAD_HOST is deprecated") || defaults.Len() != 0 {
		t.Errorf("configured logger = %q, default logger = %q", logs, defaults)
	}
}

// A call that resolves the environment before Init — a Prompt at start-up —
// says it to the default logger and leaves Init its own chance to say it to
// the application's; a configuration that fails to resolve says nothing.
func TestDeprecatedHostWarningIsNotSpentBeforeInit(t *testing.T) {
	fresh(t)
	defaults := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(defaults, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	t.Setenv("TRACEPAD_HOST", "http://from-host:4318")

	if _, err := current(); !errors.Is(err, ErrConfig) || defaults.Len() != 0 {
		t.Fatalf("no key: err = %v, default log = %q", err, defaults)
	}
	t.Setenv("TRACEPAD_API_KEY", testKey)
	if _, err := current(); err != nil || !strings.Contains(defaults.String(), "TRACEPAD_HOST is deprecated") {
		t.Fatalf("before Init: err = %v, default log = %q", err, defaults)
	}
	logs := &bytes.Buffer{}
	if _, err := Init(context.Background(), WithExport(false), WithTracerProvider(sdktrace.NewTracerProvider()),
		WithLogger(slog.New(slog.NewTextHandler(logs, nil)))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "TRACEPAD_HOST is deprecated") {
		t.Errorf("Init's logger = %q", logs)
	}
}
