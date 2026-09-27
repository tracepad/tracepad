package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// A trace's tree has a node, depth and size ceiling and renders in linear
// time (spec 043 #18–#20).

// treeSpans builds n observations of one trace, a millisecond apart. parent
// names each one's parent by index, -1 for none.
func treeSpans(trace string, n int, parent func(i int) int) []*model.Observation {
	spans := make([]*model.Observation, n)
	for i := range n {
		spans[i] = &model.Observation{TraceID: trace, ID: spanHex(i + 1), Type: model.TypeSpan,
			Name: "step", Level: model.LevelDefault,
			StartTime: seedBase + int64(i)*ms, EndTime: seedBase + int64(i)*ms + ms}
		if p := parent(i); p >= 0 {
			spans[i].ParentObservationID = spanHex(p + 1)
		}
	}
	return spans
}

// seedTree writes a trace in exports of a thousand observations, as a client
// would send a large one.
func (h *harness) seedTree(t *testing.T, trace string, spans []*model.Observation) {
	t.Helper()
	for first := 0; first < len(spans); first += 1000 {
		h.seed(t, &model.Trace{ID: trace, Name: "tree", Environment: "production"},
			spans[first:min(first+1000, len(spans))]...)
	}
}

// seedShape writes a trace of n bare spans a millisecond apart, the i-th
// (from 0) named spanHex(i+1) under the parent the SQL expression parent
// gives for `i`, NULL for none. The first goes through the writer; the rest
// are copies made in the file, because the writer spends seconds on ten
// thousand rows and the tree reads nothing it would compute but the count.
func (h *harness) seedShape(t *testing.T, trace string, n int, parent string) {
	t.Helper()
	h.seed(t, &model.Trace{ID: trace, Name: "tree", Environment: "production"},
		treeSpans(trace, 1, func(int) int { return -1 })...)
	db := h.file(t)
	for _, statement := range []string{
		`CREATE TEMP TABLE copies AS
		   WITH RECURSIVE k(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM k WHERE i < ` + fmt.Sprint(n-1) + `)
		 SELECT o.* FROM observations o, k WHERE o.project_id = '` + h.project.ID + `' AND o.trace_id = '` + trace + `'`,
		`UPDATE copies SET id = printf('%016x', rowid + 1),
		        start_time = start_time + rowid * 1000000, end_time = end_time + rowid * 1000000,
		        parent_observation_id = (SELECT printf('%016x', p + 1)
		                                   FROM (SELECT ` + parent + ` AS p FROM (SELECT copies.rowid AS i))
		                                  WHERE p IS NOT NULL)`,
		`INSERT INTO observations SELECT * FROM copies`,
		`DROP TABLE copies`,
		`UPDATE traces SET observation_count = ` + fmt.Sprint(n) + ` WHERE project_id = '` + h.project.ID + `' AND id = '` + trace + `'`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// file opens the harness's database file beside the store, for a test that
// writes what the writer would take too long to.
func (h *harness) file(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+h.dbPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// treeBody is what the tests read out of a trace answer.
type treeBody struct {
	ObservationCount int              `json:"observation_count"`
	Omitted          *int             `json:"observations_omitted"`
	Observations     []treeNodeAnswer `json:"observations"`
}

type treeNodeAnswer struct {
	ID       string           `json:"id"`
	Parent   string           `json:"parent_observation_id"`
	Children []treeNodeAnswer `json:"children"`
}

// ids lists every observation of a tree, depth first.
func ids(nodes []treeNodeAnswer) []string {
	var out []string
	for _, node := range nodes {
		out = append(out, node.ID)
		out = append(out, ids(node.Children)...)
	}
	return out
}

// nesting is how deep a JSON document nests.
func nesting(t *testing.T, body []byte) int {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(body))
	depth, deepest := 0, 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return deepest
		}
		if err != nil {
			t.Fatal(err)
		}
		switch token {
		case json.Delim('{'), json.Delim('['):
			depth++
			deepest = max(deepest, depth)
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
	}
}

// fastest is the quickest of a few runs of a request, the least noisy
// measure of what it costs.
func (h *harness) fastest(t *testing.T, path string) time.Duration {
	t.Helper()
	best := time.Duration(1<<63 - 1)
	for range 3 {
		start := time.Now()
		expectStatus(t, h.get(t, path), 200)
		best = min(best, time.Since(start))
	}
	return best
}

// A chain of 6,000 spans answers every span once, nests at most 100
// observations deep, and costs a small multiple of a flat trace of as many
// spans — a ratio, so CI's speed does not decide it.
func TestTreeChainIsBoundedAndLinear(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	const n = 6000
	chain, flat := traceHex(1), traceHex(2)
	h.seedShape(t, chain, n, "i - 1")
	h.seedShape(t, flat, n, "NULL")

	rec := h.get(t, "/api/v1/traces/"+chain)
	expectStatus(t, rec, 200)
	body := decodeJSON[treeBody](t, rec)
	got := ids(body.Observations)
	if len(got) != n {
		t.Fatalf("the chain answered %d observations, want %d", len(got), n)
	}
	sort.Strings(got)
	if len(slices.Compact(got)) != n {
		t.Fatal("an observation appears more than once")
	}
	if body.Omitted != nil {
		t.Errorf("observations_omitted = %d on a whole tree", *body.Omitted)
	}
	// Two levels an observation, and the trace and its list around them.
	if depth := nesting(t, rec.Body.Bytes()); depth > 2*maxTreeDepth+4 {
		t.Errorf("the answer nests %d levels, want at most %d", depth, 2*maxTreeDepth+4)
	}
	// Detached roots keep their parent's id, and each starts a chain of 100.
	if len(body.Observations) != n/maxTreeDepth {
		t.Errorf("the chain has %d roots, want %d chains of %d", len(body.Observations), n/maxTreeDepth, maxTreeDepth)
	}
	if second := body.Observations[1]; second.Parent != spanHex(maxTreeDepth) || second.ID != spanHex(maxTreeDepth+1) {
		t.Errorf("the second root is %s under %s, want %s detached from %s",
			second.ID, second.Parent, spanHex(maxTreeDepth+1), spanHex(maxTreeDepth))
	}

	deep, wide := h.fastest(t, "/api/v1/traces/"+chain), h.fastest(t, "/api/v1/traces/"+flat)
	if deep > 3*wide {
		t.Errorf("the chain renders in %s, the flat trace in %s: more than three times as long", deep, wide)
	}
}

// Past 10,000 observations the tree is the first 10,000 by start time and
// says how many it left out; an observation whose parent was left out sits at
// the root with its parent's id.
func TestTreeObservationCeiling(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	trace := traceHex(1)
	const n = store.MaxTreeObservations + 1
	// The last span to start is the parent of the fifth.
	h.seedShape(t, trace, n, fmt.Sprintf("CASE i WHEN 4 THEN %d END", n-1))

	body := decodeJSON[treeBody](t, h.get(t, "/api/v1/traces/"+trace))
	if body.Omitted == nil || *body.Omitted != 1 || body.ObservationCount != n {
		t.Fatalf("observations_omitted = %v of %d, want 1 of %d", body.Omitted, body.ObservationCount, n)
	}
	got := ids(body.Observations)
	if len(got) != store.MaxTreeObservations || slices.Contains(got, spanHex(n)) {
		t.Fatalf("the tree holds %d observations, want the first %d by start time", len(got), store.MaxTreeObservations)
	}
	orphan := body.Observations[4]
	if orphan.ID != spanHex(5) || orphan.Parent != spanHex(n) {
		t.Errorf("root 5 is %s under %q, want %s at the root under the left-out %s",
			orphan.ID, orphan.Parent, spanHex(5), spanHex(n))
	}

	// A trace still being written: its count read before the span that took
	// it past the ceiling. The tree is cut all the same, and says so.
	db := h.file(t)
	if _, err := db.Exec(`UPDATE traces SET observation_count = ? WHERE id = ?`, store.MaxTreeObservations, trace); err != nil {
		t.Fatal(err)
	}
	stale := decodeJSON[treeBody](t, h.get(t, "/api/v1/traces/"+trace))
	if stale.Omitted == nil || *stale.Omitted != 1 {
		t.Errorf("with a count read before the last span, observations_omitted = %v, want 1", stale.Omitted)
	}
	if _, err := db.Exec(`UPDATE traces SET observation_count = ? WHERE id = ?`, n, trace); err != nil {
		t.Fatal(err)
	}

	// The same through the shortcut, which renders exactly as the trace.
	last := decodeJSON[treeBody](t, h.get(t, "/api/v1/traces/last"))
	if last.Omitted == nil || *last.Omitted != 1 {
		t.Errorf("traces/last observations_omitted = %v, want 1", last.Omitted)
	}
}

// Observations few but heavy stop at 32 MiB of their own fields.
func TestTreeSizeCeiling(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	trace := traceHex(1)
	spans := treeSpans(trace, 50, func(int) int { return -1 })
	for i, span := range spans {
		span.Usage = map[string]any{"input": 1, "note": strings.Repeat(string(rune('a'+i%26)), 1<<20)}
	}
	for _, span := range spans {
		h.seed(t, &model.Trace{ID: trace, Environment: "production"}, span)
	}

	rec := h.get(t, "/api/v1/traces/"+trace)
	body := decodeJSON[treeBody](t, rec)
	shown := len(ids(body.Observations))
	// Each observation's own fields are a little over 1 MiB, so 31 fit.
	if body.Omitted == nil || shown+*body.Omitted != 50 || shown != 31 {
		t.Fatalf("the tree shows %d and omits %v of 50, want the prefix within 32 MiB and the rest counted",
			shown, body.Omitted)
	}
	if rec.Body.Len() > maxTreeBytes+64<<10 {
		t.Errorf("the answer is %d bytes, past the 32 MiB its tree may hold", rec.Body.Len())
	}
}

// Under `?expand=io` a tree reads its payloads one at a time, and answers the
// bytes the old way — every payload read up front — answered.
func TestTreeExpandHoldsOnePayloadAtATime(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	trace := traceHex(1)
	const n = 2000
	spans := treeSpans(trace, n, func(i int) int {
		if i == 0 {
			return -1
		}
		return (i - 1) / 4
	})
	for i, span := range spans {
		// Large enough to be cut to a preview, different per span so a
		// payload written under the wrong observation shows.
		span.Input = fmt.Sprintf("%04d <%s>", i, strings.Repeat("input & ", 512))
		if i%3 == 0 {
			span.Output = map[string]any{"answer": i, "tags": []any{"a", "b"}}
		}
		if i%5 == 0 {
			span.Metadata = map[string]any{"step": i}
		}
	}
	h.seedTree(t, trace, spans)

	held, most, reads := 0, 0, 0
	previousRead, previousDone := readPayload, payloadDone
	readPayload = func(p *store.PayloadReader, ctx context.Context, row *store.ObservationRow, kind store.PayloadKind) (any, error) {
		held++
		reads++
		most = max(most, held)
		return previousRead(p, ctx, row, kind)
	}
	payloadDone = func() { held-- }
	t.Cleanup(func() { readPayload, payloadDone = previousRead, previousDone })

	path := "/api/v1/traces/" + trace + "?expand=io&budget=5242880"
	rec := h.get(t, path)
	expectStatus(t, rec, 200)
	if reads != n+n/3+1+n/5 {
		t.Errorf("the tree read %d payloads, want every one of the trace's %d", reads, n+n/3+1+n/5)
	}
	if most != 1 {
		t.Errorf("the tree held %d payloads at once, want one", most)
	}
	if want := referenceTrace(t, h, trace, 5242880); !bytes.Equal(rec.Body.Bytes(), want) {
		t.Errorf("the answer differs from the one built with every payload read first\n got: %.300s\nwant: %.300s",
			rec.Body.Bytes(), want)
	}
}

// referenceTrace is `GET /traces/{id}?expand=io` as it was built before
// spec 043: every payload read with the observations, the tree nested, the
// skeleton measured and the payloads inlined. It knows no ceiling, which the
// trace it is asked about does not reach.
func referenceTrace(t *testing.T, h *harness, id string, budgetBytes int) []byte {
	t.Helper()
	trace, err := h.store.Trace(t.Context(), h.project.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := h.store.Observations(t.Context(), h.project.ID, id, store.WithIO)
	if err != nil {
		t.Fatal(err)
	}
	type node struct {
		row      *store.ObservationRow
		children []*node
	}
	index := map[string]*node{}
	var roots []*node
	for _, row := range rows {
		index[row.ID] = &node{row: row}
	}
	for _, row := range rows {
		if parent, ok := index[row.ParentObservationID]; ok {
			parent.children = append(parent.children, index[row.ID])
		} else {
			roots = append(roots, index[row.ID])
		}
	}
	var render func(nodes []*node, budget payloadBudget, expand bool) []object
	render = func(nodes []*node, budget payloadBudget, expand bool) []object {
		out := make([]object, 0, len(nodes))
		for _, n := range nodes {
			o := renderOwn(n.row)
			if expand {
				for _, payload := range []struct {
					key   string
					value any
				}{{"input", n.row.Input}, {"output", n.row.Output}, {"metadata", asAny(n.row.Metadata)}} {
					if payload.value != nil {
						o = o.put(payload.key, budget.render(payload.value, n.row.TraceID, n.row.ID))
					}
				}
			}
			if len(n.children) > 0 {
				o = o.put("children", render(n.children, budget, expand))
			}
			out = append(out, o)
		}
		return out
	}
	detail := func(observations []object) object {
		return renderTraceRow(trace).putSome("metadata", trace.Metadata).put("observations", observations)
	}
	skeleton, err := json.Marshal(detail(render(roots, payloadBudget{}, false)))
	if err != nil {
		t.Fatal(err)
	}
	slots := 0
	for _, row := range rows {
		for _, payload := range []any{row.Input, row.Output, asAny(row.Metadata)} {
			if payload != nil {
				slots++
			}
		}
	}
	budget := newPayloadBudget(budgetBytes, len(skeleton), slots)
	if !budget.affordable {
		t.Fatal("the reference trace cannot afford its payloads; pick a larger budget")
	}
	out, err := json.Marshal(detail(render(roots, budget, true)))
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

// A whole tree says nothing omitted even when the trace's count, read a moment
// before, says more — a deletion between the two reads.
func TestWholeTreeOmitsNothing(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	trace := traceHex(1)
	h.seedShape(t, trace, 10, "NULL")
	if _, err := h.file(t).Exec(`UPDATE traces SET observation_count = 12 WHERE id = ?`, trace); err != nil {
		t.Fatal(err)
	}
	if body := decodeJSON[treeBody](t, h.get(t, "/api/v1/traces/"+trace)); body.Omitted != nil {
		t.Errorf("observations_omitted = %d on a whole tree", *body.Omitted)
	}
}

// An expansion the budget refuses answers the skeleton with one more field,
// written after it rather than by rendering the tree again: the bytes are the
// unexpanded answer's up to its closing brace.
func TestRefusedExpansionIsTheSkeletonAndOneField(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	trace := traceHex(1)
	spans := treeSpans(trace, 400, func(int) int { return -1 })
	for i, span := range spans {
		span.Input = fmt.Sprintf("input %d", i)
	}
	h.seedTree(t, trace, spans)

	plain := h.get(t, "/api/v1/traces/"+trace).Body.Bytes()
	rec := h.get(t, "/api/v1/traces/"+trace+"?expand=io&budget=4096")
	expectStatus(t, rec, 200)
	refused := rec.Body.Bytes()
	head := plain[:len(plain)-2] // without the closing brace and the newline
	if !bytes.HasPrefix(refused, append(append([]byte{}, head...), []byte(`,"expansion":{"expanded":false,`)...)) {
		t.Fatalf("the refused expansion is not the skeleton and one field:\n%.200s", refused[len(head)-20:])
	}
	var body struct {
		Expansion struct {
			Expanded bool `json:"expanded"`
			Payloads int  `json:"payloads"`
		} `json:"expansion"`
	}
	if err := json.Unmarshal(refused, &body); err != nil || body.Expansion.Payloads != 400 {
		t.Fatalf("expansion = %+v, %v; want 400 payloads refused", body.Expansion, err)
	}
}
