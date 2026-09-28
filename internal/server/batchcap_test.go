package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/tracepad/tracepad/internal/store"
)

// A batch write carries at most maxItemsPerWrite rows (spec 003 #28, spec
// 014 #34, spec 043 #36): one over is 413 naming the cap, answered before the
// write is queued — a writer that would refuse anything it is handed never
// sees it — and before any item is validated, so the answer is the same
// whatever the array holds.
func TestBatchWritesOverTheCapAreRefusedBeforeTheQueue(t *testing.T) {
	for _, route := range batchRoutes() {
		t.Run(route.kind, func(t *testing.T) {
			h := newHarness(t, nil, store.WriterOptions{})
			h.server.writer = stubWriter{err: store.ErrWriterBusy}

			batch := route.batch(maxItemsPerWrite + 1)
			// The last row is invalid: the count is the answer, not it.
			batch[len(batch)-1] = map[string]any{}
			rec := h.send(t, "POST", route.path, batch)
			expectError(t, rec, http.StatusRequestEntityTooLarge, fmt.Sprintf(
				"this request carries %d %s; the server takes at most %d per request",
				maxItemsPerWrite+1, route.kind, maxItemsPerWrite))
		})
	}
}

// However its rows are malformed — a field the API does not know, a null, a
// value of the wrong type — an array over the cap is answered by its count:
// the rows are counted before any of them is decoded into a request.
func TestBatchWritesOverTheCapAreCountedBeforeTheyAreDecoded(t *testing.T) {
	for _, route := range batchRoutes() {
		for name, bad := range map[string]string{
			"unknown field": `{"no_such_field": 1}`,
			"null":          `null`,
			"wrong type":    `{"id": 7}`,
		} {
			t.Run(route.kind+"/"+name, func(t *testing.T) {
				h := newHarness(t, nil, store.WriterOptions{})
				rows, err := json.Marshal(route.batch(maxItemsPerWrite))
				if err != nil {
					t.Fatal(err)
				}
				body := append(append(rows[:len(rows)-1:len(rows)-1], ","+bad...), ']')
				expectError(t, h.call(t, "POST", route.path, body), http.StatusRequestEntityTooLarge,
					fmt.Sprintf("this request carries %d %s", maxItemsPerWrite+1, route.kind))
			})
		}
	}
}

// Only an array is counted: a body of any other shape, or one that is not a
// single well-formed array, gets the 400 it got before the cap existed.
func TestBatchWritesThatAreNotOneArrayKeepTheirAnswers(t *testing.T) {
	for _, route := range batchRoutes() {
		over, err := json.Marshal(route.batch(maxItemsPerWrite + 1))
		if err != nil {
			t.Fatal(err)
		}
		required := map[string]string{"scores": `"name" is required`, "items": `"input" is required`}[route.kind]
		for _, c := range []struct{ body, want string }{
			{`{}`, required},
			{`null`, required},
			{`"x"`, "malformed JSON body"},
			{`7`, "malformed JSON body"},
			{`[1,2`, "malformed JSON body"},
			{string(over[:len(over)-1]), "malformed JSON body"},
			{string(over) + ` []`, "the body must carry exactly one JSON value"},
		} {
			rec := newHarness(t, nil, store.WriterOptions{}).call(t, "POST", route.path, []byte(c.body))
			expectError(t, rec, http.StatusBadRequest, c.want)
		}
	}
}

// The cap is inclusive and refuses whole: an array at the cap is written, one
// over it writes nothing.
func TestBatchWritesAtTheCapAreWritten(t *testing.T) {
	for _, route := range batchRoutes() {
		t.Run(route.kind, func(t *testing.T) {
			h := newHarness(t, nil, store.WriterOptions{})

			rec := h.send(t, "POST", route.path, route.batch(maxItemsPerWrite+1))
			expectStatus(t, rec, http.StatusRequestEntityTooLarge)
			if n := route.stored(t, h); n != 0 {
				t.Fatalf("stored %d after a refused batch, want none", n)
			}

			rec = h.send(t, "POST", route.path, route.batch(maxItemsPerWrite))
			expectStatus(t, rec, http.StatusCreated)
			if ids := decodeJSON[scoreIDsResponse](t, rec).IDs; len(ids) != maxItemsPerWrite {
				t.Fatalf("ids = %d, want %d", len(ids), maxItemsPerWrite)
			}
		})
	}
}

type batchRoute struct {
	kind   string
	path   string
	batch  func(n int) []map[string]any
	stored func(t *testing.T, h *harness) int
}

func batchRoutes() []batchRoute {
	return []batchRoute{
		{
			kind: "scores",
			path: "/api/v1/scores",
			batch: func(n int) []map[string]any {
				batch := make([]map[string]any, n)
				for i := range batch {
					batch[i] = map[string]any{"trace_id": scoreTraceID, "name": "helpfulness", "value": float64(i)}
				}
				return batch
			},
			stored: func(t *testing.T, h *harness) int {
				return len(decodeJSON[scoreListResponse](t, h.get(t, "/api/v1/scores?limit=1")).Scores)
			},
		},
		{
			kind: "items",
			path: "/api/v1/datasets/capped/items",
			batch: func(n int) []map[string]any {
				batch := make([]map[string]any, n)
				for i := range batch {
					batch[i] = map[string]any{"input": map[string]any{"n": i}}
				}
				return batch
			},
			stored: func(t *testing.T, h *harness) int {
				rec := h.get(t, "/api/v1/datasets/capped")
				if rec.Code == http.StatusNotFound {
					return 0
				}
				return int(decodeJSON[datasetResponse](t, rec).ItemCount)
			},
		},
	}
}
