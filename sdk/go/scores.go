package tracepad

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Scores: a queue, a goroutine and a batch (spec 017 #6, #7; spec 033 #6).
//
// Scores are written from inside request handlers, and the OTel exporter has
// already shown what the right shape is there. A scoring failure must not
// fail the request that produced the trace, so a batch the server refused is
// logged and dropped rather than returned — the same asymmetry the exporter
// has. One retry, not more: a 400 is deterministic, and a queue that retries
// forever is a memory leak with a log line.
const (
	scoreBatchSize = 100
	scoreInterval  = 2 * time.Second
	// scoreQueueDepth bounds the queue the way the batch processor bounds
	// its own: a score that cannot be enqueued is logged and dropped rather
	// than blocking the handler that scored.
	scoreQueueDepth = 4096
)

type queueItem struct {
	score map[string]any
	// flush, when set, is a marker: everything queued before it has been
	// sent when it is closed.
	flush chan struct{}
}

// scoreQueue is a background sender: up to batchSize scores every interval.
type scoreQueue struct {
	send      func(context.Context, []map[string]any) error
	batchSize int
	interval  time.Duration
	// after is the clock the interval is read from; tests hand in their own.
	after func(time.Duration) <-chan time.Time

	mu      sync.RWMutex
	items   chan queueItem
	started bool
	closed  bool
	done    chan struct{}
	// keep, when set, is tracepadtest's capture: each score is handed to it
	// on the caller's goroutine, and nothing is posted or started.
	keep    func(map[string]any)
	dropped atomic.Bool
}

func newScoreQueue(send func(context.Context, []map[string]any) error) *scoreQueue {
	return &scoreQueue{
		send: send, batchSize: scoreBatchSize, interval: scoreInterval, after: time.After,
		items: make(chan queueItem, scoreQueueDepth), done: make(chan struct{}),
	}
}

func (q *scoreQueue) start() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.started && !q.closed && q.keep == nil {
		q.started = true
		go q.run()
	}
}

func (q *scoreQueue) submit(score map[string]any) {
	if q.keep != nil {
		q.keep(score)
		return
	}
	// The goroutine starts once there is a store to post to: a score written
	// before Init — with the host and the key coming as options — waits in
	// the queue rather than being posted nowhere and dropped. Init starts it.
	if _, err := current(); err == nil {
		q.start()
	}
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		// After shutdown the goroutine is gone. Saying so is the point: a
		// score that went nowhere quietly is the failure this API is worst
		// at surfacing.
		def.log().Warn("tracepad: score dropped, the queue is closed", "name", score["name"])
		return
	}
	select {
	case q.items <- queueItem{score: score}:
	default:
		def.log().Warn("tracepad: score dropped, the queue is full", "name", score["name"])
	}
}

// flush waits until everything queued before this call has been sent, or
// the context is done.
func (q *scoreQueue) flush(ctx context.Context) error {
	marker := make(chan struct{})
	q.mu.RLock()
	if !q.started || q.closed {
		q.mu.RUnlock()
		return nil
	}
	select {
	case q.items <- queueItem{flush: marker}:
		q.mu.RUnlock()
	case <-ctx.Done():
		q.mu.RUnlock()
		return ctx.Err()
	}
	select {
	case <-marker:
		return nil
	case <-ctx.Done():
		def.log().Warn("tracepad: the score queue did not drain in time", "error", ctx.Err())
		return ctx.Err()
	}
}

// close stops the goroutine once it has sent what was queued, or gives up
// waiting when the context does — the goroutine then finishes on its own.
// A queue that never started is holding scores nobody configured a store
// for, which is said out loud.
func (q *scoreQueue) close(ctx context.Context) error {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return nil
	}
	q.closed = true
	started, left := q.started, len(q.items)
	close(q.items)
	q.mu.Unlock()
	if !started {
		if left > 0 {
			def.log().Warn("tracepad: scores dropped, no store was configured", "count", left)
		}
		return nil
	}
	select {
	case <-q.done:
		return nil
	case <-ctx.Done():
		def.log().Warn("tracepad: the score queue did not stop in time; scores may be dropped", "error", ctx.Err())
		return ctx.Err()
	}
}

// drop stops the goroutine without sending what is queued, and without
// waiting for it: a batch already on the wire is past recalling, and is not
// retried (spec 040 #14).
func (q *scoreQueue) drop() {
	q.dropped.Store(true)
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		q.closed = true
		close(q.items)
	}
}

func (q *scoreQueue) run() {
	defer close(q.done)
	for {
		batch, flushes, stopping := q.collect()
		if len(batch) > 0 && !q.dropped.Load() {
			q.deliver(batch)
		}
		for _, marker := range flushes {
			close(marker)
		}
		if stopping {
			return
		}
	}
}

// collect fills one batch: it closes when it is full, when the interval is
// up, when a flush cuts it, or when the queue is closing.
func (q *scoreQueue) collect() (batch []map[string]any, flushes []chan struct{}, stopping bool) {
	var deadline <-chan time.Time // nil until the first score: blocks forever
	for len(batch) < q.batchSize {
		select {
		case item, ok := <-q.items:
			if !ok {
				return batch, flushes, true
			}
			if item.flush != nil {
				return batch, append(flushes, item.flush), false
			}
			batch = append(batch, item.score)
			if deadline == nil {
				deadline = q.after(q.interval)
			}
		case <-deadline:
			return batch, flushes, false
		}
	}
	return batch, flushes, false
}

func (q *scoreQueue) deliver(batch []map[string]any) {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if q.dropped.Load() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = q.send(ctx, batch)
		cancel()
		if err == nil {
			return
		}
	}
	def.log().Warn("tracepad: scores dropped", "count", len(batch), "error", err)
}

func postScores(ctx context.Context, batch []map[string]any) error {
	c, err := current()
	if err != nil {
		return err
	}
	_, err = request(ctx, c, "POST", "/api/v1/scores", batch, nil)
	return err
}

// queueOf is the process-wide queue, made on first use so that a score
// written before Init — or by a script that never calls it and configures
// through the environment — is still queued and sent.
func queueOf() *scoreQueue {
	d := def
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.scores == nil {
		d.scores = newScoreQueue(postScores)
	}
	return d.scores
}

// ScoreOption configures Score.
type ScoreOption func(map[string]any, *scoreTarget)

type scoreTarget struct{ onObservation, emptyTraceID bool }

// WithValue is the number, for a numeric or a boolean (0 or 1) score.
func WithValue(value float64) ScoreOption {
	return func(body map[string]any, _ *scoreTarget) { body["value"] = value }
}

// WithStringValue is the string, for a categorical or a text score.
func WithStringValue(value string) ScoreOption {
	return func(body map[string]any, _ *scoreTarget) { body["string_value"] = value }
}

// WithDataType is numeric, boolean, categorical or text; inferred by the
// store when absent (docs/scores.md).
func WithDataType(dataType string) ScoreOption {
	return func(body map[string]any, _ *scoreTarget) { body["data_type"] = dataType }
}

// WithComment is a judge's rationale, a reviewer's note.
func WithComment(comment string) ScoreOption {
	return func(body map[string]any, _ *scoreTarget) { body["comment"] = comment }
}

// WithID is the score's own id, 32 lower-case hex digits: writing the same
// id again replaces the score, which is the idempotency the API offers.
func WithID(id string) ScoreOption {
	return func(body map[string]any, _ *scoreTarget) { body["id"] = id }
}

// WithTraceID names the trace to score instead of the context's.
func WithTraceID(traceID string) ScoreOption {
	return func(body map[string]any, t *scoreTarget) {
		if traceID == "" { // an Observation's TraceID with tracing off
			t.emptyTraceID = true
			return
		}
		body["trace_id"] = traceID
	}
}

// WithObservationID names the observation to score, inside its trace.
func WithObservationID(observationID string) ScoreOption {
	return func(body map[string]any, _ *scoreTarget) {
		if observationID != "" {
			body["observation_id"] = observationID
		}
	}
}

// OnObservation scores the context's span rather than its trace alone.
func OnObservation() ScoreOption {
	return func(_ map[string]any, t *scoreTarget) { t.onObservation = true }
}

// Score scores the trace — or the observation — in flight (spec 017 #7).
//
// With no target given, the target is the trace of the context's span, and
// its span too with OnObservation. With no span and no WithTraceID the
// error is ErrNoTrace, and so is an empty WithTraceID — unless nothing traces
// here: no Init, a no-op global provider and no recording span in ctx. Then
// the call is dropped with a debug line (spec 039 #1, #8). Score does not
// call the server: it enqueues, and a
// goroutine posts POST /api/v1/scores in batches of up to 100 every two
// seconds; a rejected batch is retried once, then logged with the server's
// own message and dropped.
func Score(ctx context.Context, name string, opts ...ScoreOption) error {
	body := map[string]any{"name": name}
	var target scoreTarget
	for _, opt := range opts {
		opt(body, &target)
	}
	if _, given := body["trace_id"]; !given {
		// A no-op global echoes a propagated parent, which never records; a
		// live span from a provider the application wired itself does.
		if tracingOff() && !trace.SpanFromContext(ctx).IsRecording() {
			def.log().Debug("tracepad.Score: tracing is off (no Init); the score was dropped", "name", name)
			return nil
		}
		span := trace.SpanContextFromContext(ctx)
		if !span.IsValid() || target.emptyTraceID {
			return ErrNoTrace
		}
		body["trace_id"] = span.TraceID().String()
		if _, given := body["observation_id"]; target.onObservation && !given {
			body["observation_id"] = span.SpanID().String()
		}
	}
	queueOf().submit(body)
	return nil
}
