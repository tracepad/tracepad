package tracepad

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
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
	t.Setenv("TRACEPAD_HOST", testHost+"/")
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
			"TRACEPAD_HOST=", "TRACEPAD_API_KEY=", "TRACEPAD_ENVIRONMENT=", "TRACEPAD_RELEASE=")
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
