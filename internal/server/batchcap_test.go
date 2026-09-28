package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
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

// heldBy runs f and reports its error, its allocations and the bytes it
// allocated, which for a body of 20 MiB is the difference between holding
// nothing per value and holding a copy of it. The counters are the process's,
// so whatever else is allocating during the window is in them: f is measured
// three times and the least is kept. A background allocation lands in one
// window; an answer that allocates per value lands in every one.
func heldBy(f func() error) (err error, allocs float64, held uint64) {
	allocs, held = math.MaxFloat64, math.MaxUint64
	for range 3 {
		var before, after runtime.MemStats
		mallocs := testing.AllocsPerRun(1, func() {
			runtime.ReadMemStats(&before)
			err = f()
			runtime.ReadMemStats(&after)
		})
		allocs = min(allocs, mallocs)
		held = min(held, after.TotalAlloc-before.TotalAlloc)
	}
	return err, allocs, held
}

// An over-cap array costs no memory per value to answer (spec 043 #36), in
// each of the three shapes it can arrive in. A 20 MiB body of ten million
// zeros is the most values the default body cap holds. Decoding the values to
// count them cost 1.6 GB and ten million allocations a request; decoding the
// array before noticing a value after it cost 3.2 GB and 42 million; and
// handing a cut-short array to the decoder copied the body, 67 MB — all of
// it outside the body budget.
func TestBatchWritesOverTheCapAreAnsweredWithoutMemoryPerValue(t *testing.T) {
	whole := zeros20MiB()
	for name, c := range map[string]struct {
		body []byte
		want func(error) bool
	}{
		"counted": {whole, func(err error) bool {
			var over *overItemCap
			return errors.As(err, &over) && over.count == 10<<20
		}},
		"with a value after it": {append(bytes.Clone(whole), " 0"...), func(err error) bool { return errors.Is(err, errTrailingValue) }},
		"cut short":             {whole[:len(whole)-1], func(err error) bool { return errors.Is(err, errMalformedBody) }},
	} {
		t.Run(name, func(t *testing.T) {
			err, allocs, held := heldBy(func() error {
				_, err := decodeBatch[scoreRequest](c.body, "score")
				return err
			})
			if !c.want(err) {
				t.Fatalf("err = %v", err)
			}
			if allocs > 100 {
				t.Errorf("answering took %v allocations, want a handful", allocs)
			}
			if held > 1<<20 {
				t.Errorf("answering allocated %d bytes, want under 1 MiB for any number of values", held)
			}
		})
	}
}

// A body too short to hold more values than the cap is not scanned: the
// SDKs' batches of 100 scores pay for one decode, not two. The shortest body
// over the cap, 10,001 one-byte values, is exactly as long as the bound.
func TestBatchWritesUnderTheLengthBoundAreNotScanned(t *testing.T) {
	zeros := func(n int) []byte { return []byte("[" + strings.Repeat("0,", n-1) + "0]") }
	if len(zeros(maxItemsPerWrite+1)) != minBodyOverCap {
		t.Fatalf("len = %d, want %d", len(zeros(maxItemsPerWrite+1)), minBodyOverCap)
	}
	if rows, err := decodeBatch[json.RawMessage](zeros(maxItemsPerWrite), "row"); err != nil || len(rows) != maxItemsPerWrite {
		t.Errorf("at the cap: %d rows, err %v", len(rows), err)
	}
	var over *overItemCap
	if _, err := decodeBatch[json.RawMessage](zeros(maxItemsPerWrite+1), "row"); !errors.As(err, &over) || over.count != maxItemsPerWrite+1 {
		t.Errorf("the shortest body over the cap: err = %v", err)
	}
}

// The scan follows strings and nesting: a bracket, a comma or an escaped
// quote inside a string, and the values nested in an object or an array, are
// neither the array's end nor values of the batch — before the value that
// follows the array as much as inside it.
func TestScanArrayFollowsStringsAndNesting(t *testing.T) {
	repeat := func(value string, n int) string {
		return "[" + strings.TrimSuffix(strings.Repeat(value+" ,\n", n), " ,\n") + "]"
	}
	n := maxItemsPerWrite + 1
	for name, value := range map[string]string{
		"a closing bracket in a string":        `"]"`,
		"an escaped quote, then a bracket":     `"\"]"`,
		"an escaped backslash ends the string": `"\\"`,
		"escapes, then a bracket":              `"\\\"]"`,
		"a bracket, a brace and a comma":       `"x,]}[{, "`,
		"punctuation in an object":             `{"a":"x,]\\\"[}, {","b":[1,{"c":","},[2,3]],"d":null}`,
		"nested arrays":                        `[1,[2,3],"4,5"]`,
	} {
		t.Run(name, func(t *testing.T) {
			for tail, want := range map[string]arrayScan{
				"":     {count: n, closed: true},
				"  \n": {count: n, closed: true},
				" 0":   {count: n, closed: true, trailing: true},
				" ]":   {count: n, closed: true, trailing: true},
				` "]"`: {count: n, closed: true, trailing: true},
				" [0]": {count: n, closed: true, trailing: true},
			} {
				if got := scanArray([]byte(repeat(value, n) + tail)); got != want {
					t.Errorf("tail %q: %+v, want %+v", tail, got, want)
				}
			}
			// Through the reader: the count, or the value after the array.
			var over *overItemCap
			if _, err := decodeBatch[json.RawMessage]([]byte(repeat(value, n)), "row"); !errors.As(err, &over) || over.count != n {
				t.Errorf("counted: err = %v", err)
			}
			if _, err := decodeBatch[json.RawMessage]([]byte(repeat(value, n)+" 0"), "row"); !errors.Is(err, errTrailingValue) {
				t.Errorf("with a value after it: err = %v", err)
			}
		})
	}
	for name, c := range map[string]struct {
		body string
		want arrayScan
	}{
		"one long string":        {`["` + strings.Repeat(",", 3*maxItemsPerWrite) + `"]`, arrayScan{count: 1, closed: true}},
		"an empty array, padded": {"[" + strings.Repeat(" ", 3*maxItemsPerWrite) + "]", arrayScan{closed: true}},
		"never closed":           {repeat("0", n)[:len(repeat("0", n))-1], arrayScan{count: 0}},
		"closed by a string":     {`["` + strings.Repeat("]", 3*maxItemsPerWrite), arrayScan{}},
	} {
		if got := scanArray([]byte(c.body)); got != c.want {
			t.Errorf("%s: %+v, want %+v", name, got, c.want)
		}
	}
}

// A closed array over the cap is refused by its count whatever is wrong in
// its middle: the count wins over the malformedness (spec 003 #28).
func TestBatchWritesOverTheCapWithABrokenMiddleAreCounted(t *testing.T) {
	for _, route := range batchRoutes() {
		t.Run(route.kind, func(t *testing.T) {
			h := newHarness(t, nil, store.WriterOptions{})
			body := []byte("[" + strings.Repeat("0,", maxItemsPerWrite) + "@,,]")
			expectError(t, h.call(t, "POST", route.path, body), http.StatusRequestEntityTooLarge,
				"this request carries "+fmt.Sprint(maxItemsPerWrite+3)+" "+route.kind)
		})
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

// The two bodies an over-cap array's cost is measured on: ten million zeros
// with a value after the array, and the same array cut short. Neither may
// cost memory per value (spec 043 #36).
func BenchmarkBatchDecodeOverCapWithTail20MiB(b *testing.B) {
	body := append(zeros20MiB(), " 0"...)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := decodeBatch[scoreRequest](body, "score"); err == nil {
			b.Fatal("not refused")
		}
	}
}

func BenchmarkBatchDecodeOverCapTruncated20MiB(b *testing.B) {
	body := zeros20MiB()
	body = body[:len(body)-1]
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := decodeBatch[scoreRequest](body, "score"); err == nil {
			b.Fatal("not refused")
		}
	}
}

// What every array write of an SDK pays: a batch of 100 scores long enough
// (about 33 KB) to be past the length below which nothing is counted.
func BenchmarkBatchDecode100Scores33KB(b *testing.B) {
	batch := make([]map[string]any, 100)
	for i := range batch {
		batch[i] = map[string]any{"trace_id": scoreTraceID, "name": "helpfulness", "value": float64(i),
			"comment": strings.Repeat("a reason, in words. ", 12)}
	}
	body, err := json.Marshal(batch)
	if err != nil || len(body) < 2*(maxItemsPerWrite+1)+1 {
		b.Fatalf("body = %d bytes, err %v", len(body), err)
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if scores, err := decodeBatch[scoreRequest](body, "score"); err != nil || len(scores) != 100 {
			b.Fatal(err)
		}
	}
}

func zeros20MiB() []byte {
	body := append([]byte("["), bytes.Repeat([]byte("0,"), 10<<20)...)
	body[len(body)-1] = ']'
	return body
}

// The cap the API describes is the cap it applies (spec 043 #36): both array
// writes say `maxItems` in the OpenAPI document, which the clients are written
// against, and it is maxItemsPerWrite.
func TestOpenAPIDescribesTheItemCap(t *testing.T) {
	var document struct {
		Paths map[string]map[string]struct {
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						OneOf []struct {
							MaxItems *int `json:"maxItems"`
						} `json:"oneOf"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openAPIDocument, &document); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/scores", "/api/v1/datasets/{name}/items"} {
		described := 0
		for _, alternative := range document.Paths[path]["post"].RequestBody.Content["application/json"].Schema.OneOf {
			if alternative.MaxItems != nil {
				described = *alternative.MaxItems
			}
		}
		if described != maxItemsPerWrite {
			t.Errorf("openapi.json says POST %s takes at most %d, the server takes %d", path, described, maxItemsPerWrite)
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
