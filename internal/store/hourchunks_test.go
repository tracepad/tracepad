package store

import (
	"slices"
	"strings"
	"testing"
)

// TestHourChunks: whole hours in the order given, newest first as a bulk
// round reads them or oldest first as an erasure does; the first hour always,
// cut at the limit; later hours only while the traces and the roll budget
// hold.
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
		{"hours no roll touches are free", []int64{1, 2, 3}, map[int64]int64{}, 500, 0, []int{3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := HourChunks(tc.hours, tc.costs, tc.limit, tc.budget); !slices.Equal(got, tc.ends) {
				t.Errorf("chunks end at %v, want %v", got, tc.ends)
			}
		})
	}
}

// TestAnEraseChunkSeeksTheUsersHours is spec 023's EXPLAIN rule for the chunk's
// selection: it walks `idx_traces_user` in its own order — the start — with
// no sort, and the roll costs seek `stats_hourly`'s key.
func TestAnEraseChunkSeeksTheUsersHours(t *testing.T) {
	s, project := readStore(t)
	for _, tc := range []struct {
		name, query string
		args        []any
		want        string
	}{
		{"the chunk's traces", userTracesByStart, []any{project.ID, "u", 501},
			"SEARCH traces USING INDEX idx_traces_user (project_id=? AND user_id=?)"},
		{"the roll costs", rollCostsQuery(2), []any{project.ID, 1, 2},
			"SEARCH stats_hourly USING INDEX sqlite_autoindex_stats_hourly_1 (project_id=? AND hour=?)"},
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
