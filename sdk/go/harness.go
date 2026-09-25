package tracepad

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"iter"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// The eval harness: a run, the context that stamps it, and the scores (spec
// 018, spec 033 #10).
//
// Spec 014 made an eval a loop any script can run with curl. This is that
// loop in Go, and the one thing it owns that a curl recipe cannot is the
// stamping: inside the context run.Item hands back, every span the
// *application* starts — its own, a framework's, another SDK's — carries the
// run and the item, because a SpanProcessor writes them at OnStart rather
// than the harness touching a span it does not hold. In Go a context *is*
// the block, so the processor reads it the way the Python one reads its
// ContextVar. The store executes nothing, and neither does this: running the
// cases is the user's program.

// attemptKey is where the current attempt sits in a context. It travels
// exactly as the current span does — into every call that takes the context,
// every goroutine handed it — and no further (spec 018 #3).
type attemptKey struct{}

const page = 500

// runContextProcessor writes the run and the item on every span started
// inside an item context. The code under test is the application, and its
// root span is the framework's, another SDK's or the package's own — never
// the harness's. An attribute written at OnStart lands on all of them, the
// way a Resource does but scoped to the context.
type runContextProcessor struct{}

func (runContextProcessor) OnStart(parent context.Context, span sdktrace.ReadWriteSpan) {
	attempt, _ := parent.Value(attemptKey{}).(*Attempt)
	if attempt == nil {
		return
	}
	span.SetAttributes(attribute.String(attrRunID, attempt.RunID), attribute.String(attrItemID, attempt.ItemID))
	attempt.saw(span)
}

func (runContextProcessor) OnEnd(sdktrace.ReadOnlySpan)      {}
func (runContextProcessor) Shutdown(context.Context) error   { return nil }
func (runContextProcessor) ForceFlush(context.Context) error { return nil }

// Attempt is one case, once: what the context stamped and what it produced.
type Attempt struct {
	RunID, ItemID string

	mu     sync.Mutex
	traces []string
	// The spans stamped so far, so that a child can be told from a beginning.
	spans map[trace.SpanID]bool
}

// saw records a span the processor stamped, and the trace it began.
//
// A case begins where the *context* does, not where the trace does (spec
// 018 #13): a span whose parent is not itself inside the context starts the
// case, so a harness whose loop already runs under a span of its own still
// has a trace to score. One trace is recorded once, however many spans of
// it the context opened.
func (a *Attempt) saw(span sdktrace.ReadWriteSpan) {
	a.mu.Lock()
	defer a.mu.Unlock()
	parent := span.Parent()
	inside := parent.IsValid() && a.spans[parent.SpanID()]
	if a.spans == nil {
		a.spans = map[trace.SpanID]bool{}
	}
	a.spans[span.SpanContext().SpanID()] = true
	if inside {
		return
	}
	id := span.SpanContext().TraceID().String()
	for _, seen := range a.traces {
		if seen == id {
			return
		}
	}
	a.traces = append(a.traces, id)
}

// Traces is every trace that started inside the context, in order. A case
// run three times is three traces of one item, and the run's summary counts
// them all (spec 014 #2).
func (a *Attempt) Traces() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.traces...)
}

// TraceID is the last trace to start — the one a harness would quote — or
// "" before anything has.
func (a *Attempt) TraceID() string {
	traces := a.Traces()
	if len(traces) == 0 {
		return ""
	}
	return traces[len(traces)-1]
}

// Attributes are the two attributes, for a service the context cannot reach:
// they do not propagate with the trace context, by design (spec 014 #2), so
// a harness calling another process forwards these by its own means.
func (a *Attempt) Attributes() map[string]string {
	return map[string]string{attrRunID: a.RunID, attrItemID: a.ItemID}
}

// Score scores the trace this attempt produced (spec 018 #4) — the last one
// to start. Before anything has, the error is ErrNoTrace.
func (a *Attempt) Score(ctx context.Context, name string, opts ...ScoreOption) error {
	id := a.TraceID()
	if id == "" {
		return fmt.Errorf("%w: nothing has started inside the item context yet (%s); "+
			"score after the application has run, or pass WithTraceID to Score", ErrNoTrace, a.ItemID)
	}
	return Score(ctx, name, append([]ScoreOption{WithTraceID(id)}, opts...)...)
}

// Run is an open run: the version it pinned, the contexts it stamps, and its
// close.
type Run struct {
	ID   string
	Name string
	// DatasetVersion is the version the harness must fetch by — one number
	// by construction (spec 014 #7).
	DatasetVersion int
	dataset        *Dataset
	unstamped      atomic.Bool
}

// Item begins one case: everything started under the returned context
// carries the run and the item. It opens no span of its own — a root span
// the harness opened would make every eval trace look like a trace of the
// harness — and it reaches exactly as far as the context does: a goroutine
// handed it is stamped, one started with context.Background() is not. A
// case with no id is logged and stamps nothing.
func (r *Run) Item(ctx context.Context, item Item) (context.Context, *Attempt) {
	attempt := &Attempt{RunID: r.ID, ItemID: item.ID}
	if item.ID == "" {
		def.log().Warn("tracepad: run.Item needs an item id; nothing will be stamped", "run", r.ID)
		return ctx, attempt
	}
	// The REST half works without Init — the environment is read on the
	// first call — but the processor that stamps is registered by Init and
	// by nothing else, so a run without it closes over nothing. Said once.
	if r.unstamped.CompareAndSwap(false, true) && !initialized() {
		def.log().Warn("tracepad: Init has not run; spans inside the item context will not carry the run "+
			"and the item, and the run will cover no case", "run", r.ID)
	}
	return context.WithValue(ctx, attemptKey{}, attempt), attempt
}

// initialized reports whether Init ran.
func initialized() bool { return def.ready.Load() }

// tracingOff reports a process that never called Init and has no provider of
// its own: every span there is the API's no-op — an invalid context, or a
// propagated caller's echoed (spec 039 #1, #7).
func tracingOff() bool {
	if initialized() {
		return false
	}
	provider := otel.GetTracerProvider()
	_, disabled := provider.(noop.TracerProvider)
	// The deprecated no-op of the trace package is one comparable value.
	return disabled || isDefault(provider) || provider == trace.NewNoopTracerProvider() //nolint:staticcheck
}

// Finish delivers everything the run produced, then closes it as finished.
// The flush comes first so that Get on the next line is over every trace and
// score the run produced (spec 018 #5); a late span still links, so a flush
// that ran out of time is a number read early, and the close still posts.
func (r *Run) Finish(ctx context.Context) (map[string]any, error) {
	return r.close(ctx, map[string]any{})
}

// Fail closes the run as failed, with the error's message. A run left
// running is reported as such forever (spec 014 #8).
func (r *Run) Fail(ctx context.Context, cause error) (map[string]any, error) {
	message := "failed, no error given"
	if cause != nil {
		message = cause.Error()
	}
	return r.close(ctx, map[string]any{"status": "failed", "error": message})
}

func (r *Run) close(ctx context.Context, body map[string]any) (map[string]any, error) {
	if err := Flush(ctx); err != nil {
		def.log().Warn("tracepad: the run's flush did not finish; closing it anyway", "run", r.ID, "error", err)
	}
	return post(ctx, r.path()+"/finish", body)
}

// Get is the run with its summary, as the server computes it (spec 018 #8).
func (r *Run) Get(ctx context.Context) (map[string]any, error) {
	return get(ctx, r.path(), nil)
}

// Items is the run's cases with the attempts made at each. Unlike a
// dataset's items these inline their payloads and are budget-checked, so
// the page is the server's own size unless WithLimit names one;
// WithUnknown adds the traces that link to no case.
func (r *Run) Items(ctx context.Context, opts ...RunItemsOption) iter.Seq2[map[string]any, error] {
	params := url.Values{}
	for _, opt := range opts {
		opt(params)
	}
	return pages(ctx, r.path()+"/items", params, "items")
}

// path is the run's own. The server's hex needs no escaping and is escaped
// all the same: a path is built one way whoever supplied the segment (spec
// 033 #18).
func (r *Run) path() string { return "/api/v1/runs/" + url.PathEscape(r.ID) }

// RunItemsOption configures Run.Items.
type RunItemsOption func(url.Values)

// WithUnknown adds the run's traces that link to no case.
func WithUnknown() RunItemsOption { return func(p url.Values) { p.Set("unknown", "true") } }

// WithLimit is the page size, for a caller who knows its rows are small.
func WithLimit(limit int) RunItemsOption {
	return func(p url.Values) { p.Set("limit", strconv.Itoa(limit)) }
}

// ScoreConfig is what a score name means (docs/scores.md).
type ScoreConfig struct {
	Name        string   `json:"-"`
	DataType    string   `json:"data_type"`
	Direction   string   `json:"direction,omitempty"`
	Min         *float64 `json:"min,omitempty"`
	Max         *float64 `json:"max,omitempty"`
	Categories  []string `json:"categories,omitempty"`
	Description string   `json:"description,omitempty"`
}

// ScoreConfigs declares what the run's score names mean, before it runs a
// case. Synchronous and loud (spec 018 #6): a numeric without a direction
// should stop the job at the top rather than fail every score batch quietly
// in the queue. PUT is idempotent, so this is safe on every CI run. The
// first refusal is returned, with the name in the message and the
// *HTTPError kept for errors.As.
func ScoreConfigs(ctx context.Context, configs []ScoreConfig) error {
	c, err := current()
	if err != nil {
		return err
	}
	for _, config := range configs {
		if _, err := request(ctx, c, "PUT", "/api/v1/score-configs/"+url.PathEscape(config.Name), config, nil); err != nil {
			return fmt.Errorf("tracepad: score config %q: %w", config.Name, err)
		}
	}
	return nil
}

// ItemID is a stable item or run id from a natural key (spec 018 #7): the
// same derivation docs/scores.md shows for score ids, so that two harnesses
// hashing the same key agree.
func ItemID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:32]
}

// Compare is two runs side by side, exactly as the server computes it (spec
// 014 #18).
func Compare(ctx context.Context, a, b string) (map[string]any, error) {
	return get(ctx, "/api/v1/runs/"+url.PathEscape(a)+"/compare/"+url.PathEscape(b), nil)
}

func get(ctx context.Context, path string, params url.Values) (map[string]any, error) {
	c, err := current()
	if err != nil {
		return nil, err
	}
	answer, err := request(ctx, c, "GET", path, nil, params)
	if err != nil {
		return nil, err
	}
	return object(answer), nil
}

func post(ctx context.Context, path string, body any) (map[string]any, error) {
	c, err := current()
	if err != nil {
		return nil, err
	}
	answer, err := request(ctx, c, "POST", path, body, nil)
	if err != nil {
		return nil, err
	}
	return object(answer), nil
}

// pages walks a cursor-paged listing to its end. The loop lives here because
// this is where docs/datasets.md warns a hand-written harness goes wrong: a
// pass that silently stopped at the first page would be recorded as a whole
// run over a fraction of the cases. The first request omits `cursor` rather
// than sending it empty, which is a 400 everywhere in this API. An error
// ends the sequence with it as the second value.
func pages(ctx context.Context, path string, params url.Values, key string) iter.Seq2[map[string]any, error] {
	return func(yield func(map[string]any, error) bool) {
		query := url.Values{}
		for k, v := range params {
			query[k] = v
		}
		for {
			answer, err := get(ctx, path, query)
			if err != nil {
				yield(nil, err)
				return
			}
			rows, _ := answer[key].([]any)
			for _, row := range rows {
				item, _ := row.(map[string]any)
				if !yield(item, nil) {
					return
				}
			}
			cursor, _ := answer["next_cursor"].(string)
			if cursor == "" {
				return
			}
			query.Set("cursor", cursor)
		}
	}
}
