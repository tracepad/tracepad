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
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	_ "github.com/tracepad/tracepad/sdk/go" // its init sets the hook
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

// A test binary that links this package starts with the follower as the
// global provider, before a TestMain or an init of the application's sets
// one: every OpenTelemetry tracer taken at start-up binds to the first
// provider set, for good, and bound to the follower it reaches every
// capture (spec 040 #14).
func init() { hook.Reset() }

// Capture resets the process, has the global tracer provider follow one that
// records into memory — a tracer the application took at start-up included —
// and initialises the package against it with export off, nothing read from
// the environment, and a score queue that keeps what it is given (spec 040
// #1, #14). The test's cleanup resets the process again.
func Capture(t testing.TB) *Recorder {
	t.Helper()
	serial(t, "Capture")
	r := &Recorder{exporter: tracetest.NewInMemoryExporter()}
	// Not shut down at the cleanup — the reset detaches it — since shutting
	// down the exporter would empty it, and what was captured stays readable.
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(r.exporter))
	t.Cleanup(hook.Reset)
	if err := hook.Capture(host, key, provider, r.keep); err != nil {
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
