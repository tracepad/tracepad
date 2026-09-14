// Command fixture writes testdata/otlp/014-tracepad-sdk-go.pb from the
// package's own exporter (spec 033 #12).
//
// Every other fixture in the corpus is built by hand in internal/otlptest,
// which is the right shape for reproducing somebody else's SDK. This one is
// the export our own package produces: the bytes come off the wire of a real
// BatchSpanProcessor and OTLP/HTTP exporter pointed at a collector in this
// process, so the golden beside them is evidence that the attribute names
// the package writes are the ones the mapper's table reads. Run it through
// `make fixtures`, which then regenerates the golden like any other body.
//
// Two things are replaced so that a re-run is a no-op rather than a diff:
// the ids, by a fixed generator on the provider — which is also the
// adaptation path of Decision 2, an application that already has a provider
// — and the clocks, by ranking every instant of the batch as the exporter
// reads it and laying the ranks out on a fixed grid, which preserves the
// order and the zero-duration event exactly. The completion start, an
// attribute rather than a span field, is re-stamped from the same grid.
// Everything else — the names, the values, the resource, the scope, the
// protobuf framing — is what the package and the OTel SDK actually emitted.
//
// It lives in the SDK module rather than under scripts/fixtures because a
// program there would belong to the server's module, which does not and
// must not depend on this one (Decision 1).
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	tracepad "github.com/tracepad/tracepad/sdk/go"
)

// base is 2026-08-26T10:00:00Z, the instant every fixture in the corpus
// shares; step is the gap between one instant and the next.
var (
	base = time.Unix(1787738400, 0).UTC()
	step = 10 * time.Millisecond
)

// fixedIDs counts ids up from one, so the fixture names the same spans
// forever. The prefixes differ from the Python fixture's: two exports
// sharing a trace id would be one trace in the store.
type fixedIDs struct{ traces, spans uint64 }

func (g *fixedIDs) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	g.traces++
	var id trace.TraceID
	binary.BigEndian.PutUint64(id[:8], 0xb1c2d3e4f5a6b7c8)
	binary.BigEndian.PutUint64(id[8:], 0xd9eafb0000000000+g.traces)
	return id, g.NewSpanID(ctx, id)
}

func (g *fixedIDs) NewSpanID(context.Context, trace.TraceID) trace.SpanID {
	g.spans++
	var id trace.SpanID
	binary.BigEndian.PutUint64(id[:], 0x2b3c4d5e6f700000+g.spans)
	return id
}

// gridded is the exporter with the clocks replaced, on the way to the real
// one: each span of a batch is read at instants ranked over the whole batch.
type gridded struct{ next sdktrace.SpanExporter }

func (g gridded) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	var instants []time.Time
	for _, s := range spans {
		instants = append(instants, s.StartTime(), s.EndTime())
	}
	sort.Slice(instants, func(i, j int) bool { return instants[i].Before(instants[j]) })
	grid := map[time.Time]time.Time{}
	for _, at := range instants {
		if _, seen := grid[at]; !seen {
			grid[at] = base.Add(time.Duration(len(grid)) * step)
		}
	}
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = onGrid{ReadOnlySpan: s, start: grid[s.StartTime()], end: grid[s.EndTime()]}
	}
	return g.next.ExportSpans(ctx, out)
}

func (g gridded) Shutdown(ctx context.Context) error { return g.next.Shutdown(ctx) }

type onGrid struct {
	sdktrace.ReadOnlySpan
	start, end time.Time
}

func (s onGrid) StartTime() time.Time { return s.start }
func (s onGrid) EndTime() time.Time   { return s.end }

// Attributes re-stamps the completion start half a step into the call: the
// fixture's time to first token.
func (s onGrid) Attributes() []attribute.KeyValue {
	attrs := append([]attribute.KeyValue(nil), s.ReadOnlySpan.Attributes()...)
	for i, kv := range attrs {
		if kv.Key == "tracepad.observation.completion_start_time" {
			attrs[i] = attribute.String(string(kv.Key), s.start.Add(step/2).Format("2006-01-02T15:04:05.000Z"))
		}
	}
	return attrs
}

// collector keeps every body posted to /v1/traces.
type collector struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.Header.Get("Authorization") != "Bearer tp-sk-fixture" {
		http.Error(w, "unauthorized", 401)
		return
	}
	c.mu.Lock()
	c.bodies = append(c.bodies, body)
	c.mu.Unlock()
	w.WriteHeader(200)
}

// application is the two traces the fixture is of: every row of the Ingest
// contract. The question, the answer, the prompt, the observation kind, the
// user and the session are this fixture's own: the corpus is seeded whole
// into the interface's end-to-end suite, which searches it for one
// fixture's phrase, filters it by another's prompt and counts the traces of
// a third's kind, so a value shared with a neighbour would change what that
// suite counts.
func application(root context.Context) {
	faq := &tracepad.PromptVersion{Name: "tracepad-faq", Version: 2, Type: "text", Text: "Answer {topic}."}
	question := "how does a score reach a trace?"

	ctx, answer := tracepad.Span(root, "answer-question", tracepad.WithInput(map[string]any{"question": question}))
	tracepad.UpdateTrace(ctx, tracepad.WithTraceName("faq-answer"), tracepad.WithUserID("user-7310"),
		tracepad.WithSessionID("session-58"), tracepad.WithTags("faq", "go"),
		tracepad.WithTraceMetadata(map[string]any{"channel": "cli"}))

	searchCtx, search := tracepad.Span(ctx, "docs-search", tracepad.WithType("retriever"),
		tracepad.WithInput(map[string]any{"query": "score"}))
	tracepad.Update(searchCtx, tracepad.WithLevel("WARNING"), tracepad.WithStatusMessage("one result"),
		tracepad.WithMetadata(map[string]any{"attempt": 2}), tracepad.WithOutput([]string{"the scores page"}))
	search.End()

	_, miss := tracepad.Event(ctx, "cache.miss", tracepad.WithMetadata(map[string]any{"key": "tracepad-faq"}))
	miss.End()

	_, call := tracepad.Generation(ctx, "chat-completion", tracepad.WithModel("claude-opus-5"),
		tracepad.WithPrompt(faq), tracepad.WithModelParameters(map[string]any{"temperature": 0.2, "max_tokens": 512}),
		tracepad.WithInput([]tracepad.Message{{Role: "user", Content: question}}))
	call.FirstToken()
	cost := 0.0017
	call.End(tracepad.Result{
		Model:  "claude-opus-5-2026-08-01",
		Usage:  tracepad.Usage{"input_tokens": 131, "output_tokens": 44, "cache_read_input_tokens": 96, "reasoning_tokens": 12},
		Cost:   &cost,
		Output: "By its trace id, in a batch the queue posts.",
	})
	answer.End()

	// A second trace: a generation with no request model, and a nil cost.
	ctx, prepare := tracepad.Span(root, "prepare-question")
	tracepad.UpdateTrace(ctx, tracepad.WithTraceName("faq-rewrite"), tracepad.WithUserID("user-7310"),
		tracepad.WithSessionID("session-58"))
	_, rewrite := tracepad.Generation(ctx, "rewrite-question", tracepad.WithInput("how does score reach trace"))
	rewrite.End(tracepad.Result{Model: "gpt-5-mini", Usage: tracepad.Usage{"input_tokens": 19, "output_tokens": 9},
		Output: "How does a score reach a trace?"})
	prepare.End()
}

func run(target string) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	sink := &collector{}
	server := &http.Server{Handler: sink}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	// The application's own provider, adopted rather than replaced (spec 017
	// #2). Its resource is written out rather than discovered, so that a
	// host name and an SDK version do not churn the golden on every run.
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithIDGenerator(&fixedIDs{}),
		sdktrace.WithResource(resource.NewSchemaless(
			attribute.String("service.name", "faq-bot"),
			attribute.String("service.version", "2026.9.14"),
			attribute.String("deployment.environment.name", "canary"),
			attribute.String("telemetry.sdk.language", "go"),
			attribute.String("telemetry.sdk.name", "opentelemetry"),
		)))
	otel.SetTracerProvider(provider)

	ctx := context.Background()
	host := "http://" + listener.Addr().String()
	shutdown, err := tracepad.Init(ctx, tracepad.WithHost(host), tracepad.WithKey("tp-sk-fixture"), tracepad.WithExport(false))
	if err != nil {
		return err
	}
	// The exporter Init attaches, configured the same way, behind the grid:
	// the endpoint and the auth header are what the unit suite checks.
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(host+"/v1/traces"),
		otlptracehttp.WithHeaders(map[string]string{"Authorization": "Bearer tp-sk-fixture"}))
	if err != nil {
		return err
	}
	provider.RegisterSpanProcessor(sdktrace.NewBatchSpanProcessor(gridded{next: exporter}))

	application(ctx)
	if err := shutdown(ctx); err != nil {
		return err
	}
	if err := provider.Shutdown(ctx); err != nil {
		return err
	}
	if len(sink.bodies) != 1 {
		return fmt.Errorf("expected one export, got %d", len(sink.bodies))
	}
	if err := os.WriteFile(target, sink.bodies[0], 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d bytes)\n", target, len(sink.bodies[0]))
	return nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: fixture <path of 014-tracepad-sdk-go.pb>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
