package server

import (
	"net/http"
	"testing"

	"github.com/tracepad/tracepad/internal/store"
)

// The system endpoint counts the arrays refused for their length, one counter
// per route because the limits are different ones (spec 043 #40): a write at
// its limit is stored and counts nothing, one value over is refused and counts
// once, in its own project only, and a refusal for anything else is not this
// counter's.

type batchCapCounters struct {
	Scores       int64 `json:"scores_over_row_cap"`
	DatasetItems int64 `json:"dataset_items_over_row_cap"`
	QueueAdds    int64 `json:"queue_adds_over_target_cap"`
}

func (h *harness) batchCapCounters(t *testing.T, secret string) batchCapCounters {
	t.Helper()
	rec := h.call(t, "GET", "/api/v1/system", nil, asKey(secret))
	expectStatus(t, rec, 200)
	return decodeJSON[struct {
		Counters batchCapCounters `json:"counters"`
	}](t, rec).Counters
}

func TestArraysOverTheirLimitAreCounted(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.second(t, "other", "tp-sk-other")
	h.putConfigs(t, "accuracy")
	expectStatus(t, h.putQueue(t, "review", "accuracy"), 201)

	targets := func(n int) []map[string]any {
		batch := make([]map[string]any, n)
		for i := range batch {
			batch[i] = map[string]any{"trace_id": traceHex(i + 1)}
		}
		return batch
	}
	writes := []struct {
		name   string
		path   string
		limit  int
		batch  func(n int) []map[string]any
		over   int
		status int
		of     func(batchCapCounters) int64
	}{
		{"scores", "/api/v1/scores", maxItemsPerWrite, batchRoutes()[0].batch, http.StatusRequestEntityTooLarge,
			http.StatusCreated, func(c batchCapCounters) int64 { return c.Scores }},
		{"dataset items", "/api/v1/datasets/capped/items", maxItemsPerWrite, batchRoutes()[1].batch, http.StatusRequestEntityTooLarge,
			http.StatusCreated, func(c batchCapCounters) int64 { return c.DatasetItems }},
		{"queue targets", "/api/v1/queues/review/items", maxItemsPerAdd, targets, http.StatusBadRequest,
			http.StatusCreated, func(c batchCapCounters) int64 { return c.QueueAdds }},
	}
	for _, w := range writes {
		t.Run(w.name, func(t *testing.T) {
			// At the limit: stored, and not counted.
			expectStatus(t, h.send(t, "POST", w.path, w.batch(w.limit)), w.status)
			if got := h.batchCapCounters(t, testSecret); w.of(got) != 0 {
				t.Fatalf("counters after a write at the limit = %+v, want none", got)
			}

			// A body that is refused for something else is not this counter's.
			expectStatus(t, h.call(t, "POST", w.path, []byte(`[1,2`)), http.StatusBadRequest)
			if got := h.batchCapCounters(t, testSecret); w.of(got) != 0 {
				t.Fatalf("counters after a malformed body = %+v, want none", got)
			}

			// One over: refused, and counted once, in this project alone.
			expectStatus(t, h.send(t, "POST", w.path, w.batch(w.limit+1)), w.over)
			got := h.batchCapCounters(t, testSecret)
			if w.of(got) != 1 {
				t.Errorf("counters after one write over the limit = %+v, want this route's at 1", got)
			}
			if theirs := h.batchCapCounters(t, "tp-sk-other"); theirs != (batchCapCounters{}) {
				t.Errorf("another project's counters = %+v, want none of this project's refusals", theirs)
			}
		})
	}

	// Each route kept to its own counter.
	if got := h.batchCapCounters(t, testSecret); got != (batchCapCounters{Scores: 1, DatasetItems: 1, QueueAdds: 1}) {
		t.Errorf("counters = %+v, want one each", got)
	}
}
