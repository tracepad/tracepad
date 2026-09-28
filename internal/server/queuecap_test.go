package server

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/store"
)

// A queue add takes at most maxItemsPerAdd targets (spec 024 #25), and answers
// an add of more with a 400 and the text it always had. What changed is the
// order (spec 024 #25): the targets are counted before any is decoded, so the
// answer costs no memory per target, as the answer to a batch of scores does
// (spec 043 #36).
// The most `{},` targets a body under the default body cap holds, the `[` and
// the closing bracket counted: what a request of that size can carry to the
// decoder.
const queueCapTargets = (config.DefaultMaxBodyBytes - 2) / 3

const queueAddOverTheLimit = "an add takes at most 1000 targets; use items/from-traces for a filter"

func TestQueueAddKeepsItsLimitAndItsAnswer(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.putConfigs(t, "accuracy")
	expectStatus(t, h.putQueue(t, "review", "accuracy"), 201)

	targets := func(n int) []map[string]any {
		batch := make([]map[string]any, n)
		for i := range batch {
			batch[i] = map[string]any{"trace_id": traceHex(i + 1)}
		}
		return batch
	}
	// At the limit: written.
	rec := h.send(t, "POST", "/api/v1/queues/review/items", targets(maxItemsPerAdd))
	expectStatus(t, rec, http.StatusCreated)
	if got := h.queueCounts(t, "review"); got.Pending != maxItemsPerAdd {
		t.Fatalf("pending = %d, want %d", got.Pending, maxItemsPerAdd)
	}

	// One over: a 400 with the text it always had, and nothing written.
	over := targets(maxItemsPerAdd + 1)
	expectError(t, h.send(t, "POST", "/api/v1/queues/review/items", over), http.StatusBadRequest, queueAddOverTheLimit)
	// Counted before decoded, so whatever a row holds does not change the
	// answer — a malformed target, a null, a field the API does not know.
	for name, bad := range map[string]string{
		"a malformed target": `{"trace_id":"not-an-id"}`,
		"a null":             `null`,
		"an unknown field":   `{"no_such_field":1}`,
	} {
		body := []byte(`[` + strings.Repeat(`{"trace_id":"`+traceHex(1)+`"},`, maxItemsPerAdd) + bad + `]`)
		rec := h.call(t, "POST", "/api/v1/queues/review/items", body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), queueAddOverTheLimit) {
			t.Errorf("%s in an add over the limit = %d %s, want the limit's own answer", name, rec.Code, rec.Body)
		}
	}
	// The count wins over a malformed middle (spec 024 #25): the limit's
	// targets and a comma before the bracket are counted as one more.
	stray := []byte(`[` + strings.Repeat(`{"trace_id":"`+traceHex(1)+`"},`, maxItemsPerAdd) + `]`)
	expectError(t, h.call(t, "POST", "/api/v1/queues/review/items", stray), http.StatusBadRequest, queueAddOverTheLimit)
	if got := h.queueCounts(t, "review"); got.Pending != maxItemsPerAdd {
		t.Errorf("pending = %d, want a refused add to have written nothing", got.Pending)
	}

	// A body that is not one array keeps its answers, and so does one with
	// something after the array.
	for _, c := range []struct{ body, want string }{
		{`{}`, "trace_id"},
		{`[{"trace_id":"` + traceHex(1) + `"}] ]`, "the body must carry exactly one JSON value"},
		{`[{"trace_id":"` + traceHex(1) + `"},`, "malformed JSON body"},
		{`[]`, "no targets in the request"},
	} {
		expectError(t, h.call(t, "POST", "/api/v1/queues/review/items", []byte(c.body)), http.StatusBadRequest, c.want)
	}
}

// The three shapes an over-long add can arrive in, at the size of the body
// cap: 20 MiB of `{},` is seven million targets (queueCapTargets, the most
// that fit under the cap), and decoding them to count them cost 600 MB and
// seven million allocations before the 400.
func TestQueueAddOverTheLimitIsAnsweredWithoutMemoryPerTarget(t *testing.T) {
	whole := append([]byte("["), bytes.Repeat([]byte("{},"), queueCapTargets)...)
	whole[len(whole)-1] = ']'
	for name, c := range map[string]struct {
		body []byte
		want func(error) bool
	}{
		"counted": {whole, func(err error) bool {
			var over *overItemCap
			return errors.As(err, &over) && over.count == queueCapTargets
		}},
		"with a value after it": {append(bytes.Clone(whole), " 0"...), func(err error) bool { return errors.Is(err, errTrailingValue) }},
		"cut short":             {whole[:len(whole)-1], func(err error) bool { return errors.Is(err, errMalformedBody) }},
	} {
		t.Run(name, func(t *testing.T) {
			err, allocs, held := heldBy(func() error {
				_, err := decodeTargets(c.body)
				return err
			})
			if !c.want(err) {
				t.Fatalf("err = %v", err)
			}
			if allocs > 100 {
				t.Errorf("answering took %v allocations, want a handful", allocs)
			}
			if held > 1<<20 {
				t.Errorf("answering allocated %d bytes, want under 1 MiB for any number of targets", held)
			}
		})
	}
}

// The queue's limit is its own, and so are the two gates on the scan (spec 043
// #36): a body that cannot hold more than 1,000 values, by its length or by
// its commas, is not scanned, and one that can is. An add of a hundred
// targets — what a person or a script sends — pays nothing.
func TestQueueAddScansOnlyABodyThatCanBeOverItsLimit(t *testing.T) {
	var scans int
	was := scanBody
	scanBody = func(body []byte) arrayScan { scans++; return was(body) }
	t.Cleanup(func() { scanBody = was })

	targets := func(n int) []byte {
		return []byte("[" + strings.TrimSuffix(strings.Repeat(`{"trace_id":"`+traceHex(1)+`"},`, n), ",") + "]")
	}
	empties := func(n int) []byte { return []byte("[" + strings.TrimSuffix(strings.Repeat("{},", n), ",") + "]") }
	for name, c := range map[string]struct {
		body  []byte
		scans int
	}{
		"a hundred targets": {targets(100), 0},
		"the limit itself: one comma short of being over": {empties(maxItemsPerAdd), 0},
		"a thousand targets with their trace ids":         {targets(maxItemsPerAdd), 0},
		"one over the limit":                              {empties(maxItemsPerAdd + 1), 1},
		"a thousand and one targets, whole":               {targets(maxItemsPerAdd + 1), 1},
		"a hundred thousand":                              {empties(100 * maxItemsPerAdd), 1},
	} {
		scans = 0
		_, _ = decodeTargets(c.body)
		if scans != c.scans {
			t.Errorf("%s: scanned %d times, want %d", name, scans, c.scans)
		}
	}
}

// What a queue add of the size of the body cap costs to answer, on the counted
// body of the test above (the other two shapes end the same scan sooner).
func BenchmarkQueueAddOverTheLimit20MiB(b *testing.B) {
	whole := append([]byte("["), bytes.Repeat([]byte("{},"), queueCapTargets)...)
	whole[len(whole)-1] = ']'
	b.SetBytes(int64(len(whole)))
	b.ReportAllocs()
	for b.Loop() {
		_, _ = decodeTargets(whole)
	}
}
