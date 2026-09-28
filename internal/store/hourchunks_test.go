package store

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
)

// TestHourChunks: whole hours in the order given, newest first as a bulk
// round reads them or oldest first as an erasure does; the first hour always,
// cut at the limit; later hours only while the traces and the roll budget,
// the first hour's cost included, hold. A budget of zero is the default, and
// a cost is asked only of an hour a chunk considers.
func TestHourChunks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		hours  []int64
		costs  map[int64]int64
		limit  int
		budget int64
		ends   []int
	}{
		{"nothing", nil, nil, 500, 10, nil},
		{"one hour", []int64{1, 1, 1}, nil, 500, 10, []int{3}},
		{"hours that fit together", []int64{1, 1, 2, 3, 3}, map[int64]int64{1: 3, 2: 3, 3: 3}, 500, 10, []int{5}},
		{"newest first", []int64{3, 3, 2, 1, 1}, map[int64]int64{1: 3, 2: 3, 3: 3}, 500, 10, []int{5}},
		{"the limit ends at an hour", []int64{1, 1, 2, 2, 3}, nil, 3, 10, []int{2, 5}},
		{"an hour past the limit is cut", []int64{1, 1, 1, 1, 1, 2}, nil, 2, 10, []int{2, 4, 6}},
		{"the budget ends at an hour", []int64{1, 2, 3}, map[int64]int64{1: 6, 2: 4, 3: 1}, 500, 10, []int{2, 3}},
		{"a first hour over the budget is taken", []int64{1, 2}, map[int64]int64{1: 50, 2: 1}, 500, 10, []int{1, 2}},
		{"the first hour's cost counts", []int64{1, 2}, map[int64]int64{1: 8, 2: 3}, 500, 10, []int{1, 2}},
		{"hours no roll touches are free", []int64{1, 2, 3}, map[int64]int64{}, 500, 1, []int{3}},
		{"a zero budget is the default", []int64{1, 2}, map[int64]int64{1: DeleteRollBudget / 2, 2: DeleteRollBudget / 2}, 500, 0, []int{2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cost := func(hour int64) (int64, error) { return tc.costs[hour], nil }
			got, err := HourChunks(tc.hours, nil, cost, tc.limit, tc.budget, 0)
			if err != nil || !slices.Equal(got, tc.ends) {
				t.Errorf("chunks end at %v (%v), want %v", got, err, tc.ends)
			}
		})
	}

	// What deleting the chunk's own traces costs counts as well: two light
	// hours, each one trace too heavy to share a chunk.
	free := func(int64) (int64, error) { return 0, nil }
	if got, err := HourChunks([]int64{1, 2}, []int64{6, 6}, free, 500, 10, 0); err != nil || !slices.Equal(got, []int{1, 2}) {
		t.Errorf("heavy traces end at %v (%v), want a chunk each", got, err)
	}
	if got, err := HourChunks([]int64{1, 2}, nil, free, 500, 10, 0); err != nil || !slices.Equal(got, []int{2}) {
		t.Errorf("light traces end at %v (%v), want one chunk", got, err)
	}

	// The cost of a thousand hours is asked of the ones the chunks reach,
	// each once per chunk that considers it: a chunk of one hour asks its
	// first and the one that did not fit.
	var hours []int64
	for hour := range int64(1000) {
		hours = append(hours, hour)
	}
	asked := 0
	count := func(int64) (int64, error) { asked++; return 10, nil }
	if _, err := HourChunks(hours[:3], nil, count, 500, 10, 0); err != nil {
		t.Fatal(err)
	}
	if asked != 5 {
		t.Errorf("asked %d costs for three hours of one chunk each, want 5", asked)
	}
	asked = 0
	if _, err := HourChunks(hours, nil, count, 2, 1000, 0); err != nil {
		t.Fatal(err)
	}
	if asked != 1000 {
		t.Errorf("asked %d costs for chunks of two traces, want each hour once, 1000", asked)
	}

	// A caller that runs one chunk, or a round of a few, prices only the
	// hours those consider, however long the run it read (spec 047 #22).
	for _, tc := range []struct{ chunks, ends, asked int }{{1, 1, 2}, {3, 3, 6}} {
		asked = 0
		ends, err := HourChunks(hours, nil, count, 500, 10, tc.chunks)
		if err != nil {
			t.Fatal(err)
		}
		if len(ends) != tc.ends || asked != tc.asked {
			t.Errorf("%d chunks of a thousand hours: %d ends, %d costs asked; want %d and %d",
				tc.chunks, len(ends), asked, tc.ends, tc.asked)
		}
	}

	// A first hour cut at the limit ends its chunk without a price.
	asked = 0
	if _, err := HourChunks([]int64{1, 1, 1, 2}, nil, count, 2, 10, 1); err != nil || asked != 0 {
		t.Errorf("a cut first hour asked %d costs (%v), want none", asked, err)
	}
}

// TestAnEraseChunkSeeksTheUsersHours is spec 023's EXPLAIN rule for the chunk's
// selection: it walks `idx_traces_user` in its own order — the start — with
// no sort, and an hour's roll cost reads the hour's range of
// `idx_traces_timestamp`, as the roll does.
func TestAnEraseChunkSeeksTheUsersHours(t *testing.T) {
	s, project := readStore(t)
	for _, tc := range []struct {
		name, query string
		args        []any
		want        string
	}{
		{"the chunk's traces", userTracesByStart, []any{project.ID, "u", 501},
			"SEARCH traces USING INDEX idx_traces_user (project_id=? AND user_id=?)"},
		{"an hour's roll cost", rollCostQuery, []any{project.ID, 1, 2},
			"SEARCH traces USING INDEX idx_traces_timestamp (project_id=? AND timestamp>? AND timestamp<?)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := s.explainQueryPlan(tc.query, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(plan, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("plan does not say %q:\n%s", tc.want, joined)
			}
			if strings.Contains(joined, "TEMP B-TREE") {
				t.Errorf("plan sorts:\n%s", joined)
			}
		})
	}
}

// TestARollCostsWhatTheRollReads (spec 047 #22): an hour costs its traces and
// every observation they hold, a span without a model included, counted from
// the rows themselves, so an hour the aggregator has not rolled since its
// traces arrived is not free. An hour at or past the watermark is not rolled
// and costs nothing.
func TestARollCostsWhatTheRollReads(t *testing.T) {
	s, project := readStore(t)
	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	// One agent run in the fixtures' hour: a model call and 29 steps
	// without a model. Nothing has rolled it.
	start := (rollupHour + 60) * 1_000_000_000
	batch := &IngestBatch{ProjectID: project.ID,
		Traces: []*model.Trace{{ID: fmt.Sprintf("%032x", 1), Name: "run"}}}
	for i := range 30 {
		o := &model.Observation{TraceID: fmt.Sprintf("%032x", 1), ID: fmt.Sprintf("%016x", i+1),
			Type: model.TypeSpan, Level: model.LevelDefault,
			StartTime: start + int64(i)*1_000_000, EndTime: start + int64(i+1)*1_000_000}
		if i == 0 {
			o.Model = "claude-sonnet-5"
		}
		batch.Observations = append(batch.Observations, o)
	}
	if err := writer.Submit(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	advance(t, s, project.ID, rollupHour)
	cost, err := rollCosts(t.Context(), s.db, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(traceRollWeight + 30)
	if got, err := cost(rollupHour); err != nil || got != want {
		t.Errorf("the hour costs %d (%v), want its trace's weight and its 30 observations, %d", got, err, want)
	}
	if got, err := cost(rollupHour + SecondsPerHour); err != nil || got != 0 {
		t.Errorf("the hour at the watermark costs %d (%v), want 0", got, err)
	}
}
