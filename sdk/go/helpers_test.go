package tracepad

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const (
	testHost = "http://tracepad.test:4318"
	testKey  = "tp-sk-test"
)

// recorder is a provider of our own, handed to Init explicitly, exporting
// into memory — and the log the package wrote to.
type recorder struct {
	exporter *tracetest.InMemoryExporter
	provider *sdktrace.TracerProvider
	logs     *bytes.Buffer
}

// fresh forgets the default between tests: Init is process-wide by design.
func fresh(t *testing.T) {
	t.Helper()
	for _, variable := range []string{"TRACEPAD_HOST", "TRACEPAD_API_KEY", "TRACEPAD_ENVIRONMENT", "TRACEPAD_RELEASE"} {
		t.Setenv(variable, "")
	}
	reset()
	t.Cleanup(reset)
}

// setup is a fresh default over an in-memory provider, with no exporter.
func setup(t *testing.T, opts ...Option) *recorder {
	t.Helper()
	fresh(t)
	r := &recorder{exporter: tracetest.NewInMemoryExporter(), logs: &bytes.Buffer{}}
	r.provider = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sdktrace.NewSimpleSpanProcessor(r.exporter)))
	t.Cleanup(func() { _ = r.provider.Shutdown(context.Background()) })
	opts = append([]Option{
		WithHost(testHost), WithKey(testKey), WithExport(false),
		WithTracerProvider(r.provider), WithLogger(slog.New(slog.NewTextHandler(r.logs, nil))),
	}, opts...)
	if _, err := Init(context.Background(), opts...); err != nil {
		t.Fatalf("Init: %v", err)
	}
	return r
}

func (r *recorder) spans() tracetest.SpanStubs { return r.exporter.GetSpans() }

// one is the single exported span of that name; the test fails otherwise.
func (r *recorder) one(t *testing.T, name string) tracetest.SpanStub {
	t.Helper()
	var found []tracetest.SpanStub
	for _, s := range r.spans() {
		if s.Name == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		var names []string
		for _, s := range r.spans() {
			names = append(names, s.Name)
		}
		t.Fatalf("want one span named %q, got %v", name, names)
	}
	return found[0]
}

func (r *recorder) attrs(t *testing.T, name string) map[string]attribute.Value {
	t.Helper()
	return attrsOf(r.one(t, name))
}

func attrsOf(s tracetest.SpanStub) map[string]attribute.Value {
	out := map[string]attribute.Value{}
	for _, kv := range s.Attributes {
		out[string(kv.Key)] = kv.Value
	}
	return out
}

func str(t *testing.T, attrs map[string]attribute.Value, key string) string {
	t.Helper()
	value, ok := attrs[key]
	if !ok {
		t.Fatalf("attribute %q is not set; have %v", key, keys(attrs))
	}
	return value.Emit()
}

func keys(attrs map[string]attribute.Value) []string {
	var out []string
	for k := range attrs {
		out = append(out, k)
	}
	return out
}
