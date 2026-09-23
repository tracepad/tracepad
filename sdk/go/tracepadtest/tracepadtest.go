// Package tracepadtest tests an application's instrumentation (spec 040), in
// the idiom of httptest:
//
//	func TestTheAnswerIsTraced(t *testing.T) {
//		rec := tracepadtest.Capture(t)
//		answer(context.Background(), "why is the sky blue")
//		if got := rec.Attributes(t, "answer")["tracepad.observation.type"].AsString(); got != "span" {
//			t.Errorf("type = %q", got)
//		}
//	}
//
// Capture gives a fresh, initialised process that records instead of
// exporting; Reset gives one that never initialised — tracing off, as spec
// 039 defines it. Both register their own cleanup, which resets the process
// again. The state is process-wide, so a test that runs in parallel cannot
// use either: they fail it, naming why.
package tracepadtest

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	tracepad "github.com/tracepad/tracepad/sdk/go"
	"github.com/tracepad/tracepad/sdk/go/internal/hook"
)

// A reserved name (RFC 2606), so nothing resolves it: export is off and the
// queue does not post, and a REST call a test forgot to stub fails loudly.
const (
	host = "http://tracepad.test:4318"
	key  = "tp-sk-test"
)

// Recorder is what the code under test traced and scored since Capture.
type Recorder struct {
	exporter *tracetest.InMemoryExporter
	mu       sync.Mutex
	scores   []map[string]any
}

// Capture resets the process, installs a global tracer provider that records
// into memory, and initialises the package against it with export off and a
// score queue that keeps what it is given (spec 040 #1). The test's cleanup
// resets the process again.
func Capture(t testing.TB) *Recorder {
	t.Helper()
	serial(t, "Capture")
	r := &Recorder{exporter: tracetest.NewInMemoryExporter()}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(r.exporter))
	t.Cleanup(func() {
		hook.Reset()
		_ = provider.Shutdown(context.Background())
	})
	hook.Reset()
	otel.SetTracerProvider(provider)
	hook.Keep(r.keep)
	if _, err := tracepad.Init(context.Background(),
		tracepad.WithHost(host), tracepad.WithKey(key), tracepad.WithExport(false)); err != nil {
		t.Fatalf("tracepadtest.Capture: %v", err)
	}
	return r
}

// Reset returns the process to never initialised for the rest of the test,
// and again at its cleanup (spec 040 #2).
func Reset(t testing.TB) {
	t.Helper()
	serial(t, "Reset")
	t.Cleanup(hook.Reset)
	hook.Reset()
}

// serial fails a parallel test: the one thing Go's testing package refuses
// to a parallel test is a change to the process's environment, so a probe of
// it asks the question — and makes a later t.Parallel refuse as well.
func serial(t testing.TB, helper string) {
	t.Helper()
	defer func() {
		if recover() != nil {
			t.Fatalf("tracepadtest.%s: the capture is process-wide and this test runs in parallel; "+
				"call it from a test that does not call t.Parallel", helper)
		}
	}()
	t.Setenv("TRACEPADTEST_SERIAL", "1")
}

func (r *Recorder) keep(score map[string]any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scores = append(r.scores, score)
}

// Spans is every finished span, in the order it ended.
func (r *Recorder) Spans() tracetest.SpanStubs { return r.exporter.GetSpans() }

// One is the single finished span of that name; the test fails naming the
// spans there were otherwise.
func (r *Recorder) One(t testing.TB, name string) tracetest.SpanStub {
	t.Helper()
	var found []tracetest.SpanStub
	var names []string
	for _, s := range r.Spans() {
		names = append(names, s.Name)
		if s.Name == name {
			found = append(found, s)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one span named %q, got %q", name, names)
	}
	return found[0]
}

// Attributes are the attributes of the single span of that name, by key.
func (r *Recorder) Attributes(t testing.TB, name string) map[string]attribute.Value {
	t.Helper()
	out := map[string]attribute.Value{}
	for _, kv := range r.One(t, name).Attributes {
		out[string(kv.Key)] = kv.Value
	}
	return out
}

// Scores are the bodies Score would have posted, in order.
func (r *Recorder) Scores() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.scores...)
}
