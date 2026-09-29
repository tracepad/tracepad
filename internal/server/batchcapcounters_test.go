package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
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

// A body refused for its shape is not refused for its length, even when it is
// long enough to be scanned (spec 043 #36, #40): an array that never closes or
// is followed by another value, with as many commas as the cap has values,
// takes the scan's 400 and moves no counter.
func TestScannedBodiesRefusedForTheirShapeAreNotCounted(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.putConfigs(t, "accuracy")
	expectStatus(t, h.putQueue(t, "review", "accuracy"), 201)

	for _, route := range []struct {
		name, path string
		limit      int
	}{
		{"scores", "/api/v1/scores", maxItemsPerWrite},
		{"dataset items", "/api/v1/datasets/capped/items", maxItemsPerWrite},
		{"queue targets", "/api/v1/queues/review/items", maxItemsPerAdd},
	} {
		zeros := func(n int) string { return "[" + strings.Repeat("0,", n-1) + "0]" }
		unclosed := "[" + strings.Repeat("0,", route.limit+5)
		trailing := zeros(route.limit+1) + " 1"
		for name, body := range map[string]string{"never closed": unclosed, "a value after the array": trailing} {
			if len(body) < minBodyOver(route.limit) || strings.Count(body, ",") < route.limit {
				t.Fatalf("%s/%s: %d bytes and %d commas would skip the scan", route.name, name, len(body), strings.Count(body, ","))
			}
			expectStatus(t, h.call(t, "POST", route.path, []byte(body)), http.StatusBadRequest)
		}
	}
	if got := h.batchCapCounters(t, testSecret); got != (batchCapCounters{}) {
		t.Errorf("counters after bodies refused for their shape = %+v, want none", got)
	}
}

// The limits the /system documentation names are the limits the server
// applies, as the documented maxItems are (TestOpenAPIDescribesTheItemCap): a
// changed cap fails here rather than leaving the old number in the document.
func TestOpenAPINamesTheLimitsOfTheCounters(t *testing.T) {
	var document any
	if err := json.Unmarshal(openAPIDocument, &document); err != nil {
		t.Fatal(err)
	}
	var find func(node any, key string) map[string]any
	find = func(node any, key string) map[string]any {
		switch n := node.(type) {
		case map[string]any:
			if v, ok := n[key].(map[string]any); ok {
				return v
			}
			for _, child := range n {
				if found := find(child, key); found != nil {
					return found
				}
			}
		case []any:
			for _, child := range n {
				if found := find(child, key); found != nil {
					return found
				}
			}
		}
		return nil
	}
	thousands := func(n int) string {
		s := fmt.Sprint(n)
		for i := len(s) - 3; i > 0; i -= 3 {
			s = s[:i] + "," + s[i:]
		}
		return s
	}
	for key, limit := range map[string]int{
		"scores_over_row_cap":        maxItemsPerWrite,
		"dataset_items_over_row_cap": maxItemsPerWrite,
		"queue_adds_over_target_cap": maxItemsPerAdd,
	} {
		counter := find(document, key)
		if counter == nil {
			t.Errorf("openapi.json describes no %s", key)
			continue
		}
		if description, _ := counter["description"].(string); !strings.Contains(description, "more than "+thousands(limit)+" ") {
			t.Errorf("%s says %q, the server takes at most %d", key, description, limit)
		}
	}
}

// /system says what the process recovered from, and the asking project's own
// retention, so that a panic that is handled is not a panic that is hidden
// (spec 043 #42).
func TestSystemReportsRecoveredPanics(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	rec := h.call(t, "GET", "/api/v1/system", nil)
	expectStatus(t, rec, 200)
	body := decodeJSON[struct {
		WorkerPanics map[string]any `json:"worker_panics"`
		Sweeper      map[string]any `json:"sweeper"`
	}](t, rec)
	for _, key := range []string{"recovered", "given_up", "last_where", "last_at"} {
		if _, ok := body.WorkerPanics[key]; !ok {
			t.Errorf("worker_panics has no %q: %v", key, body.WorkerPanics)
		}
	}
}
