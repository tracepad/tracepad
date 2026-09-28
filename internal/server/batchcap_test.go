package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
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

// Counting an array costs no memory per value (spec 043 #36): a 20 MiB body
// of ten million zeros — the most values the default body cap holds — is
// refused without a copy of its values, where decoding them into
// json.RawMessage cost 1.6 GB and ten million allocations a request, outside
// the body budget.
func TestBatchWritesOverTheCapAreCountedWithoutMemoryPerValue(t *testing.T) {
	body := zeros20MiB()
	var before, after runtime.MemStats
	var err error
	allocs := testing.AllocsPerRun(1, func() {
		runtime.ReadMemStats(&before)
		_, err = decodeBatch[scoreRequest](body, "score")
		runtime.ReadMemStats(&after)
	})
	var over *overItemCap
	if !errors.As(err, &over) || over.count != 10<<20 {
		t.Fatalf("err = %v, want the cap naming %d values", err, 10<<20)
	}
	if allocs > 100 {
		t.Errorf("counting took %v allocations, want a handful", allocs)
	}
	if held := after.TotalAlloc - before.TotalAlloc; held > 1<<20 {
		t.Errorf("counting allocated %d bytes, want under 1 MiB for any number of values", held)
	}
}

// A body too short to hold more values than the cap is not counted: the
// SDKs' batches of 100 scores pay for one decode, not two.
func TestBatchWritesUnderTheLengthBoundAreNotCounted(t *testing.T) {
	short := []byte("[" + strings.Repeat("0,", maxItemsPerWrite) + "0]")
	if len(short) != 2*(maxItemsPerWrite+1)+1 {
		t.Fatalf("len = %d", len(short))
	}
	under := []byte("[" + strings.Repeat("0,", maxItemsPerWrite-1) + "0]")
	if n := arrayLength(under); n != 0 {
		t.Errorf("a body under the bound was counted: %d", n)
	}
	if n := arrayLength(short); n != maxItemsPerWrite+1 {
		t.Errorf("the shortest body over the cap counts %d, want %d", n, maxItemsPerWrite+1)
	}
}

// The count is of top-level values: a comma, a bracket or an escaped quote
// inside a string, and the values nested in an object or an array, are not
// values of the batch.
func TestArrayLengthCountsTopLevelValuesOnly(t *testing.T) {
	const tricky = `{"a":"x,]\\\"[}, {","b":[1,{"c":","},[2,3]],"d":null}`
	repeat := func(value string, n int) []byte {
		return []byte("[" + strings.TrimSuffix(strings.Repeat(value+" ,\n", n), " ,\n") + "]")
	}
	for name, c := range map[string]struct {
		body []byte
		want int
	}{
		"objects with punctuation in strings": {repeat(tricky, maxItemsPerWrite+1), maxItemsPerWrite + 1},
		"nested arrays":                       {repeat(`[1,[2,3],"4,5"]`, maxItemsPerWrite+1), maxItemsPerWrite + 1},
		"strings":                             {repeat(`"\\\","`, maxItemsPerWrite+1), maxItemsPerWrite + 1},
		"one long string":                     {[]byte(`["` + strings.Repeat(",", 3*maxItemsPerWrite) + `"]`), 1},
		"an empty array, padded":              {[]byte("[" + strings.Repeat(" ", 3*maxItemsPerWrite) + "]"), 0},
		"an object":                           {[]byte(`{"a":"` + strings.Repeat(",", 3*maxItemsPerWrite) + `"}`), 0},
		"not well-formed":                     {repeat(`0`, maxItemsPerWrite+1)[1:], 0},
	} {
		if got := arrayLength(c.body); got != c.want {
			t.Errorf("%s: %d values, want %d", name, got, c.want)
		}
	}
}

func BenchmarkItemCapCount20MiBZeros(b *testing.B) {
	body := zeros20MiB()
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := decodeBatch[scoreRequest](body, "score"); err == nil {
			b.Fatal("not refused")
		}
	}
}

func zeros20MiB() []byte {
	body := append([]byte("["), bytes.Repeat([]byte("0,"), 10<<20)...)
	body[len(body)-1] = ']'
	return body
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
