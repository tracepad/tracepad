package tracepad

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
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
