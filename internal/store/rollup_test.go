package store

import (
	"context"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
)

// The rollup at the store layer (spec 013): one hour recomputed from the raw
// rows must equal what the live scan says about that hour, and recomputing it
// again must change nothing.

const rollupHour = int64(1787738400) // 2026-08-26T10:00:00Z, the fixtures' hour

// rollupFixture seeds one hour of two environments, two releases and two
// models, with an error and an uncosted trace among them.
func rollupFixture(t *testing.T, s *Store, projectID string) {
	t.Helper()
	base := rollupHour * 1e9

	// A trace has no timestamp of its own: it is the earliest start among
	// its observations, maintained by ingest (spec 004 #26). Placing the
	// observation in the hour is what places the trace in it.
	seed := func(n int, environment, release string, errored bool, cost *float64, latency int64, name string) {
		start := base + int64(n)*int64(1e9)
		trace := &model.Trace{
			ID: hexTrace(n), Name: "run", Environment: environment, Release: release,
		}
		observation := &model.Observation{
			TraceID: trace.ID, ID: hexSpan(n), Type: model.TypeGeneration, Name: "call",
			Level:     model.LevelDefault,
			StartTime: start, EndTime: start + latency*1e6,
			Model: name,
		}
		if errored {
			observation.Level = model.LevelError
		}
		if cost != nil {
			observation.CostDetails = map[string]any{"total": *cost}
		}
		seedTrace(t, s, projectID, trace, observation)
	}

	cost := func(v float64) *float64 { return &v }
	seed(1, "production", "2026.8.30", false, cost(0.01), 250, "claude-sonnet-5")
	seed(2, "production", "2026.8.30", true, cost(0.02), 900, "claude-sonnet-5")
	seed(3, "production", "2026.8.31", false, nil, 120, "gpt-4o-mini")
	seed(4, "staging", "", false, cost(0.005), 40, "")
}

// roll runs the job the aggregator submits, through the same writer every
// durable write goes through.
func roll(t *testing.T, s *Store, projectID string, hour int64) *statsRoll {
	t.Helper()
	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	job := &statsRoll{ProjectID: projectID, Hour: hour}
	if err := writer.Submit(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	return job
}

func rolledRows(t *testing.T, s *Store, projectID string, hour int64) map[string]StatsRow {
	t.Helper()
	rows := map[string]StatsRow{}
	err := s.StatsRollupRows(projectID, hour, hour+SecondsPerHour, nil, func(row StatsRow) {
		rows[row.Environment+"|"+row.Release+"|"+row.Model] = row
	})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// The rolled hour must say what the live scan says about it — the same
// dimension tuples, the same counts, the same costs. This is the property the
// read seam rests on (spec 013 #5): a reader must not be able to tell which
// half answered.
func TestRolledHourEqualsTheLiveScan(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)

	from, to := rollupHour*1e9, (rollupHour+SecondsPerHour)*1e9
	for _, tc := range []struct {
		groupBy string
		key     func(StatsRow) (string, bool)
	}{
		{GroupByEnvironment, func(r StatsRow) (string, bool) { return r.Environment, r.Model == "" }},
		{GroupByRelease, func(r StatsRow) (string, bool) { return r.Release, r.Model == "" }},
		{GroupByModel, func(r StatsRow) (string, bool) { return r.Model, r.Model != "" }},
	} {
		t.Run(tc.groupBy, func(t *testing.T) {
			live := map[string]*StatsRow{}
			err := s.StatsSamples(project.ID, StatsFilter{
				From: &from, To: &to, GroupBy: tc.groupBy,
			}, func(sample StatsSample) {
				row := live[sample.Key]
				if row == nil {
					row = &StatsRow{}
					live[sample.Key] = row
				}
				row.Count++
				if sample.Errored {
					row.ErrorCount++
				}
				if sample.Cost != nil {
					total := *sample.Cost
					if row.TotalCost != nil {
						total += *row.TotalCost
					}
					row.TotalCost = &total
				}
			})
			if err != nil {
				t.Fatal(err)
			}

			rolled := map[string]*StatsRow{}
			for _, row := range rolledRows(t, s, project.ID, rollupHour) {
				key, ok := tc.key(row)
				if !ok {
					continue
				}
				merged := rolled[key]
				if merged == nil {
					merged = &StatsRow{}
					rolled[key] = merged
				}
				merged.Count += row.Count
				merged.ErrorCount += row.ErrorCount
				if row.TotalCost != nil {
					total := *row.TotalCost
					if merged.TotalCost != nil {
						total += *merged.TotalCost
					}
					merged.TotalCost = &total
				}
			}

			if len(rolled) != len(live) {
				t.Fatalf("rolled %d buckets, the live scan has %d: %v vs %v",
					len(rolled), len(live), keysOf(rolled), keysOf(live))
			}
			for key, want := range live {
				got := rolled[key]
				if got == nil {
					t.Errorf("bucket %q is missing from the rollup", key)
					continue
				}
				if got.Count != want.Count || got.ErrorCount != want.ErrorCount {
					t.Errorf("bucket %q: count %d/%d, errors %d/%d (rolled/live)",
						key, got.Count, want.Count, got.ErrorCount, want.ErrorCount)
				}
				switch {
				case (got.TotalCost == nil) != (want.TotalCost == nil):
					t.Errorf("bucket %q: cost %v rolled, %v live — absent is not zero",
						key, got.TotalCost, want.TotalCost)
				case got.TotalCost != nil && !nearly(*got.TotalCost, *want.TotalCost):
					t.Errorf("bucket %q: cost %f rolled, %f live",
						key, *got.TotalCost, *want.TotalCost)
				}
			}
		})
	}
}

// The latency the rollup answers is the live one to within the bucket ratio,
// which is the whole promise of the histogram (spec 013 #2).
func TestRolledLatencyIsWithinABucketOfTheLiveScan(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)

	merged := NewHistogram()
	for _, row := range rolledRows(t, s, project.ID, rollupHour) {
		if row.Model == "" {
			merged.Merge(row.Latency)
		}
	}
	// The exact answer, by the same nearest-rank rule the API has always
	// used, over the latencies the fixture seeded. Computed rather than
	// written down: a hardcoded expectation here would be a second opinion
	// about the rule, and the first draft of this test got it wrong.
	exact := nearestRank([]int64{40, 120, 250, 900}, 50)

	got, ok := merged.Percentile(50)
	if !ok {
		t.Fatal("the rolled hour reports no latency at all")
	}
	if ratio := float64(got) / float64(exact); ratio < 0.88 || ratio > 1.13 {
		t.Errorf("p50 = %d, exact = %d (ratio %.3f), want within a bucket", got, exact, ratio)
	}
}

// nearestRank is the rule the statistics endpoint has always used, restated
// here so the rolled answer is compared against it rather than against a
// number somebody typed.
func nearestRank(sorted []int64, p int) int64 {
	rank := (p*len(sorted) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

// Re-rolling is how a late span is corrected (spec 013 #4), so it has to be
// idempotent: the same hour rolled twice leaves one set of rows, not two.
func TestRollingAnHourTwiceLeavesOneSetOfRows(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)

	first := roll(t, s, project.ID, rollupHour)
	before := rolledRows(t, s, project.ID, rollupHour)
	second := roll(t, s, project.ID, rollupHour)
	after := rolledRows(t, s, project.ID, rollupHour)

	if first.Rows != second.Rows {
		t.Errorf("the first roll wrote %d rows and the second %d", first.Rows, second.Rows)
	}
	if len(before) != len(after) {
		t.Fatalf("%d rows became %d on a re-roll", len(before), len(after))
	}
	for key, was := range before {
		is := after[key]
		if is.Count != was.Count || is.ErrorCount != was.ErrorCount {
			t.Errorf("row %q: %d/%d became %d/%d on a re-roll",
				key, was.Count, was.ErrorCount, is.Count, is.ErrorCount)
		}
	}
}

// A span that arrives for an hour already rolled changes that hour's numbers
// when it is rolled again, and changes nothing until then.
func TestARolledHourAbsorbsALateSpan(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)

	before := rolledRows(t, s, project.ID, rollupHour)["production|2026.8.30|"]

	lateStart := rollupHour*1e9 + 30*1e9
	late := &model.Trace{ID: hexTrace(9), Name: "late", Environment: "production",
		Release: "2026.8.30"}
	seedTrace(t, s, project.ID, late, &model.Observation{
		TraceID: late.ID, ID: hexSpan(9), Type: model.TypeSpan, Name: "step",
		Level: model.LevelDefault, StartTime: lateStart, EndTime: lateStart + 5e6})

	unchanged := rolledRows(t, s, project.ID, rollupHour)["production|2026.8.30|"]
	if unchanged.Count != before.Count {
		t.Errorf("the rollup moved without a pass: %d became %d", before.Count, unchanged.Count)
	}

	roll(t, s, project.ID, rollupHour)
	after := rolledRows(t, s, project.ID, rollupHour)["production|2026.8.30|"]
	if after.Count != before.Count+1 {
		t.Errorf("count = %d after the re-roll, want %d", after.Count, before.Count+1)
	}
}

// An hour in which nothing carried a cost has no cost. Zero is a different
// claim, and the column is nullable so that the difference survives storage
// (spec 002 #14, spec 013 edge cases).
func TestAnHourWithNoCostReportsNone(t *testing.T) {
	s, project := readStore(t)
	start := rollupHour * 1e9
	trace := &model.Trace{ID: hexTrace(1), Name: "free", Environment: "default"}
	seedTrace(t, s, project.ID, trace, &model.Observation{
		TraceID: trace.ID, ID: hexSpan(1), Type: model.TypeSpan, Name: "step",
		Level: model.LevelDefault, StartTime: start, EndTime: start + 1e6})
	roll(t, s, project.ID, rollupHour)

	row := rolledRows(t, s, project.ID, rollupHour)["default||"]
	if row.Count != 1 {
		t.Fatalf("count = %d, want 1", row.Count)
	}
	if row.TotalCost != nil {
		t.Errorf("total_cost = %f, want NULL: nobody said what it cost", *row.TotalCost)
	}
}

// The watermark is the read path's seam, and it never moves backwards: a pass
// that rolled less than the one before leaves it where it was (spec 013 #4).
func TestTheWatermarkNeverRegresses(t *testing.T) {
	s, project := readStore(t)
	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	advance := func(until, pass int64) {
		if err := writer.Submit(context.Background(), &statsRollupAdvance{
			ProjectID: project.ID, RolledUntil: until, LastPass: pass}); err != nil {
			t.Fatal(err)
		}
	}

	if state, err := s.RollupState(project.ID); err != nil || state.RolledUntil != 0 {
		t.Fatalf("a project nobody rolled reports %+v, %v — want a zero state", state, err)
	}

	advance(rollupHour, 100)
	advance(rollupHour-SecondsPerHour, 200)

	state, err := s.RollupState(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.RolledUntil != rollupHour {
		t.Errorf("rolled_until = %d, want %d: the watermark must not go back",
			state.RolledUntil, rollupHour)
	}
	if state.LastPass != 200 {
		t.Errorf("last_pass = %d, want 200: the cutoff always follows the pass", state.LastPass)
	}
}

// The primary key is the query (spec 013, data contract): a range read must
// seek it rather than scan the table.
func TestRollupRangeReadSeeksThePrimaryKey(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)

	rows, err := s.db.Query(
		`EXPLAIN QUERY PLAN
		 SELECT hour, environment, release, model, count, error_count, total_cost, latency
		 FROM stats_hourly WHERE project_id = ? AND hour >= ? AND hour < ? ORDER BY hour`,
		project.ID, rollupHour, rollupHour+SecondsPerHour)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// SQLite calls the primary key of a rowid table by its autoindex name,
	// so the assertion is on the seek and on which index does it — `SCAN`
	// anywhere in the plan would mean the key is not the query after all.
	if !strings.Contains(plan.String(), "SEARCH stats_hourly USING INDEX sqlite_autoindex_stats_hourly_1") {
		t.Errorf("the range read does not seek the primary key:\n%s", plan.String())
	}
	if strings.Contains(plan.String(), "SCAN") {
		t.Errorf("the range read scans:\n%s", plan.String())
	}
	if strings.Contains(plan.String(), "TEMP B-TREE") {
		t.Errorf("the range read sorts after the fact, so the key is not the order:\n%s",
			plan.String())
	}
}

func TestHourOfBucketsByTheClientClock(t *testing.T) {
	for _, tc := range []struct {
		nanos int64
		hour  int64
	}{
		{rollupHour * 1e9, rollupHour},
		{rollupHour*1e9 + 59*60*1e9 + 999_999_999, rollupHour},
		{(rollupHour + SecondsPerHour) * 1e9, rollupHour + SecondsPerHour},
		{0, 0},
	} {
		if got := HourOf(tc.nanos); got != tc.hour {
			t.Errorf("HourOf(%d) = %d, want %d", tc.nanos, got, tc.hour)
		}
	}
}

func keysOf[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}

func nearly(a, b float64) bool {
	diff := a - b
	return diff < 1e-9 && diff > -1e-9
}
