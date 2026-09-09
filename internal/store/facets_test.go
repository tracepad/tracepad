package store

import (
	"sort"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
)

// The facets at the store layer (spec 027): the name rollup beside the
// traffic, and the two halves of the read seam that answer over it.

// namedFixture seeds one hour of three trace names across two environments,
// with one nameless trace among them — the shape the roll and the read side
// both have to leave out.
func namedFixture(t *testing.T, s *Store, projectID string) {
	t.Helper()
	base := rollupHour * 1e9
	seed := func(n int, name, environment, release string, errored bool) {
		start := base + int64(n)*int64(1e9)
		trace := &model.Trace{ID: hexTrace(n), Name: name,
			Environment: environment, Release: release}
		observation := &model.Observation{
			TraceID: trace.ID, ID: hexSpan(n), Type: model.TypeSpan, Name: "step",
			Level: model.LevelDefault, StartTime: start, EndTime: start + 1e6,
		}
		if errored {
			observation.Level = model.LevelError
		}
		seedTrace(t, s, projectID, trace, observation)
	}
	seed(1, "chat", "production", "2026.8.30", false)
	seed(2, "chat", "production", "2026.8.30", true)
	seed(3, "chat", "staging", "", false)
	seed(4, "summarize", "production", "2026.8.31", false)
	seed(5, "", "production", "", false) // no name: not a value to pick
}

// rolledNames reads `names_hourly` for one hour.
func rolledNames(t *testing.T, s *Store, projectID string, hour int64) map[string]int64 {
	t.Helper()
	rows, err := s.db.Query(
		`SELECT name, count, error_count FROM names_hourly
		 WHERE project_id = ? AND hour = ? ORDER BY name`, projectID, hour)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	names := map[string]int64{}
	for rows.Next() {
		var (
			name              string
			count, errorCount int64
		)
		if err := rows.Scan(&name, &count, &errorCount); err != nil {
			t.Fatal(err)
		}
		names[name] = count
		names[name+"!"] = errorCount
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}

// facetsOf folds a yield into `column|value → count`, which is what both halves
// of the seam produce and what the server sums.
func facetsOf(read func(func(FacetRow)) error, t *testing.T) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	if err := read(func(row FacetRow) { out[row.Column+"|"+row.Value] += row.Count }); err != nil {
		t.Fatal(err)
	}
	return out
}

// One hour rolled: the names the hour holds, with their counts and errors, and
// the nameless trace absent.
func TestNamesHourIsRolled(t *testing.T) {
	s, project := readStore(t)
	namedFixture(t, s, project.ID)
	job := roll(t, s, project.ID, rollupHour)

	if job.NameRows != 2 {
		t.Errorf("the job wrote %d name rows, want 2", job.NameRows)
	}
	names := rolledNames(t, s, project.ID, rollupHour)
	if names["chat"] != 3 || names["chat!"] != 1 {
		t.Errorf("chat = %d traces, %d errored; want 3 and 1", names["chat"], names["chat!"])
	}
	if names["summarize"] != 1 {
		t.Errorf("summarize = %d traces, want 1", names["summarize"])
	}
	if _, held := names[""]; held {
		t.Error("a trace with no name became a name to filter by")
	}
}

// A late trace lands in a rolled hour: re-rolling it recomputes the hour whole
// rather than adding to it, which is what makes the roll idempotent
// (spec 013 #3).
func TestNamesHourIsReRolled(t *testing.T) {
	s, project := readStore(t)
	namedFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)

	start := rollupHour*1e9 + 30*int64(1e9)
	seedTrace(t, s, project.ID,
		&model.Trace{ID: hexTrace(9), Name: "summarize", Environment: "production"},
		&model.Observation{TraceID: hexTrace(9), ID: hexSpan(9), Type: model.TypeSpan,
			Name: "step", Level: model.LevelDefault, StartTime: start, EndTime: start + 1e6})

	roll(t, s, project.ID, rollupHour)
	if names := rolledNames(t, s, project.ID, rollupHour); names["summarize"] != 2 {
		t.Errorf("summarize = %d after the late trace, want 2", names["summarize"])
	}
	// And twice more changes nothing.
	roll(t, s, project.ID, rollupHour)
	if names := rolledNames(t, s, project.ID, rollupHour); names["summarize"] != 2 {
		t.Errorf("summarize = %d after a second re-roll, want 2", names["summarize"])
	}
}

// The property the read seam rests on: the rolled half and the live half must
// answer the same thing about the same hour (spec 027 #3).
func TestFacetRowsEqualTheLiveTail(t *testing.T) {
	s, project := readStore(t)
	namedFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)

	rolled := facetsOf(func(yield func(FacetRow)) error {
		return s.FacetRows(project.ID, rollupHour, rollupHour+SecondsPerHour, yield)
	}, t)
	live := facetsOf(func(yield func(FacetRow)) error {
		return s.FacetTail(project.ID, rollupHour*1e9, (rollupHour+SecondsPerHour)*1e9, yield)
	}, t)

	if len(rolled) == 0 {
		t.Fatal("the rollup answered nothing; the roll did not run")
	}
	for key, count := range live {
		if rolled[key] != count {
			t.Errorf("%s: rollup %d, live scan %d", key, rolled[key], count)
		}
	}
	for key := range rolled {
		if _, held := live[key]; !held {
			t.Errorf("%s is in the rollup and not in the live scan", key)
		}
	}
	// And what both halves leave out, stated rather than implied.
	for _, absent := range []string{"release|", "name|"} {
		if _, held := rolled[absent]; held {
			t.Errorf("%q is a value to pick, and it is not", absent)
		}
	}
	if rolled["environment|production"] != 4 || rolled["name|chat"] != 3 ||
		rolled["release|2026.8.30"] != 2 {
		t.Errorf("the rolled facets are wrong: %v", sortedKeys(rolled))
	}
}

func sortedKeys(rows map[string]int64) []string {
	out := make([]string, 0, len(rows))
	for key := range rows {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// The name rollup is frozen by *its own* rows (spec 027 #3, on spec 025 #21's
// rule): past the window, an hour whose name rows already stand is left alone,
// and one it holds nothing for is still fillable — which is what makes
// migration 0016's backfill work on an install that had already rolled its
// history.
func TestTheNameRollupIsFrozenByItsOwnRows(t *testing.T) {
	s, project := readStore(t)
	namedFixture(t, s, project.ID)
	stampTraces(t, s, project.ID, rollupHour*1e9)

	// Rolled before the name table existed: the statistics hold the hour and
	// `names_hourly` does not, which is what an upgrade finds.
	passAt(t, s, afterTheHour())
	if _, err := s.db.Exec(`DELETE FROM names_hourly WHERE project_id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	if rows := rolledRows(t, s, project.ID, rollupHour); len(rows) == 0 {
		t.Fatal("the statistics were not rolled, so the test proves nothing")
	}
	// A window the fixture's hour is long past by the client's clock, with
	// nothing swept: `ingested_at` is a moment ago.
	if _, err := s.db.Exec(
		`UPDATE projects SET retention_days = 1 WHERE id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}

	// The migration's own line, and the pass it buys.
	if _, err := s.db.Exec(`UPDATE stats_rollup SET last_pass = 0`); err != nil {
		t.Fatal(err)
	}
	passAt(t, s, pastTheWindow())

	filled := rolledNames(t, s, project.ID, rollupHour)
	if filled["chat"] != 3 {
		t.Fatalf("the backfill left a frozen hour empty although its traces are intact: %v", filled)
	}

	// The other half of the rule: with rows standing, a re-roll past the
	// window must not touch them — even once the raw traces are gone.
	for _, statement := range []string{
		`DELETE FROM observations WHERE project_id = ?`,
		`DELETE FROM traces WHERE project_id = ?`,
	} {
		if _, err := s.db.Exec(statement, project.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE stats_rollup SET last_pass = 0`); err != nil {
		t.Fatal(err)
	}
	passAt(t, s, pastTheWindow())
	if after := rolledNames(t, s, project.ID, rollupHour); after["chat"] != 3 {
		t.Errorf("the frozen hour was demolished: %v", after)
	}
}

// And the case the rule does not change: an hour past the window whose traces
// retention really did take writes nothing, because the scan finds nothing —
// which is the truth.
func TestASweptHourGetsNoNameRows(t *testing.T) {
	s, project := readStore(t)
	namedFixture(t, s, project.ID)
	stampTraces(t, s, project.ID, rollupHour*1e9)

	passAt(t, s, afterTheHour())
	if _, err := s.db.Exec(`DELETE FROM names_hourly WHERE project_id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DELETE FROM observations WHERE project_id = ?`,
		`DELETE FROM traces WHERE project_id = ?`,
		`UPDATE projects SET retention_days = 1 WHERE id = ?`,
	} {
		if _, err := s.db.Exec(statement, project.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE stats_rollup SET last_pass = 0`); err != nil {
		t.Fatal(err)
	}
	passAt(t, s, pastTheWindow())

	if names := rolledNames(t, s, project.ID, rollupHour); len(names) != 0 {
		t.Errorf("a swept hour grew name rows out of nothing: %v", names)
	}
	if rows := rolledRows(t, s, project.ID, rollupHour); len(rows) == 0 {
		t.Error("the statistics of a frozen hour were recomputed away")
	}
}

// `stats_retention_days` governs the fourth table as it does the other three
// (spec 027, config additions).
func TestTheNameRollupIsSwept(t *testing.T) {
	s, project := readStore(t)
	namedFixture(t, s, project.ID)
	passAt(t, s, afterTheHour())
	if names := rolledNames(t, s, project.ID, rollupHour); len(names) == 0 {
		t.Fatal("the pass wrote no name rows")
	}

	if _, err := s.db.Exec(
		`UPDATE projects SET stats_retention_days = 1 WHERE id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	passAt(t, s, pastTheWindow())

	if names := rolledNames(t, s, project.ID, rollupHour); len(names) != 0 {
		t.Errorf("the window did not reach the name rollup: %v", names)
	}
	if hours, err := s.NamesRollupHours(project.ID); err != nil || len(hours) != 0 {
		t.Errorf("hours = %v, err = %v; want none left", hours, err)
	}
}

// The roll is bounded by `idx_traces_timestamp`, which is what makes a
// backfill one indexed hour at a time rather than a scan of the project per
// hour (the method of spec 003 #25).
func TestTheNameRollSeeksItsIndex(t *testing.T) {
	s, project := readStore(t)
	namedFixture(t, s, project.ID)

	plan, err := s.explainQueryPlan(nameRollQuery,
		project.ID, rollupHour, project.ID, rollupHour*1e9, (rollupHour+SecondsPerHour)*1e9)
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(plan, "\n"); !strings.Contains(joined, "idx_traces_timestamp") {
		t.Errorf("the name roll scans the project:\n%s", joined)
	}
}

// The three columns take a list, and a trace matches when its column equals
// any item (spec 027 #1). The fixture is one hour of three names across two
// environments and three releases, which is enough for every shape.
func TestManyValuedFilters(t *testing.T) {
	s, project := readStore(t)
	namedFixture(t, s, project.ID)

	for _, tc := range []struct {
		name   string
		filter TraceFilter
		want   []string
	}{
		{"one environment is what it always was",
			TraceFilter{Environment: []string{"production"}},
			[]string{hexTrace(5), hexTrace(4), hexTrace(2), hexTrace(1)}},
		{"two environments keep either",
			TraceFilter{Environment: []string{"staging", "production"}},
			[]string{hexTrace(5), hexTrace(4), hexTrace(3), hexTrace(2), hexTrace(1)}},
		{"one name",
			TraceFilter{Name: []string{"summarize"}}, []string{hexTrace(4)}},
		{"two names",
			TraceFilter{Name: []string{"summarize", "chat"}},
			[]string{hexTrace(4), hexTrace(3), hexTrace(2), hexTrace(1)}},
		{"a name nothing carries drops out of the list",
			TraceFilter{Name: []string{"summarize", "nope"}}, []string{hexTrace(4)}},
		{"two releases",
			TraceFilter{Release: []string{"2026.8.30", "2026.8.31"}},
			[]string{hexTrace(4), hexTrace(2), hexTrace(1)}},
		{"the lists compose, as every pair of filters does",
			TraceFilter{Environment: []string{"production", "staging"},
				Name: []string{"chat"}},
			[]string{hexTrace(3), hexTrace(2), hexTrace(1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.filter.Limit = 50
			rows, err := s.Traces(project.ID, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, row := range rows {
				ids = append(ids, row.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("ids = %v, want %v", ids, tc.want)
			}
		})
	}
}

// The session listing and the statistics' live half take the same list, so a
// screen that carries one filter between them cannot mean two things by it.
func TestManyValuedEnvironmentOnTheOtherReads(t *testing.T) {
	s, project := readStore(t)
	namedFixture(t, s, project.ID)
	seedTrace(t, s, project.ID,
		&model.Trace{ID: hexTrace(7), Name: "chat", SessionID: "s7", Environment: "eval"},
		&model.Observation{TraceID: hexTrace(7), ID: hexSpan(7), Type: model.TypeSpan,
			Name: "step", Level: model.LevelDefault,
			StartTime: rollupHour*1e9 + 7e9, EndTime: rollupHour*1e9 + 8e9})

	t.Run("sessions", func(t *testing.T) {
		rows, err := s.Sessions(project.ID, SessionFilter{
			Environment: []string{"eval", "staging"}, Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].ID != "s7" {
			t.Fatalf("sessions = %v, want s7 alone (the others carry no session)", rows)
		}
	})

	t.Run("the statistics' live half", func(t *testing.T) {
		keys := map[string]int{}
		from, to := rollupHour*1e9, (rollupHour+SecondsPerHour)*1e9
		err := s.StatsSamples(project.ID, StatsFilter{
			From: &from, To: &to, GroupBy: GroupByEnvironment,
			Environment: []string{"staging", "eval"},
		}, func(sample StatsSample) { keys[sample.Key]++ })
		if err != nil {
			t.Fatal(err)
		}
		if keys["staging"] != 1 || keys["eval"] != 1 || len(keys) != 2 {
			t.Errorf("buckets = %v, want one staging and one eval", keys)
		}
	})
}

// A list must still seek the index a single value seeks — otherwise "any of
// three environments" is three times the work of a scan (spec 027 #1). The
// listing itself keeps the keyset index, as it does under every other filter
// (spec 012 #8); the count is where an environment index can pay off.
func TestAnEnvironmentListSeeksItsIndex(t *testing.T) {
	s, project := readStore(t)
	namedFixture(t, s, project.ID)

	query, args := traceCountQuery(project.ID,
		TraceFilter{Environment: []string{"production", "staging"}}, 1000)
	plan, err := s.explainQueryPlan(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "idx_traces_environment") {
		t.Errorf("counting an environment list scans the project:\n%s", joined)
	}

	query, args = traceQuery(project.ID, TraceFilter{
		Environment: []string{"production", "staging"},
		Limit:       50,
		After:       &TraceCursor{Timestamp: rollupHour * 1e9, ID: hexTrace(1)},
	})
	plan, err = s.explainQueryPlan(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(plan, "\n")
	if !strings.Contains(joined, "idx_traces_timestamp") || strings.Contains(joined, "TEMP B-TREE") {
		t.Errorf("an environment list cost the listing its keyset seek:\n%s", joined)
	}
}

// One value is the condition it always was, spelled the way the plan tests of
// spec 004 #4 and spec 012 #8 assert; beyond one it is an `IN` over the same
// index.
func TestMatchAnyRendersOneConditionPerShape(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		clause string
		args   int
	}{
		{"none", nil, "", 0},
		{"one", []string{"a"}, "environment = ?", 1},
		{"three", []string{"a", "b", "c"}, "environment IN (?, ?, ?)", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clause, bound := matchAny("environment", tc.values)
			if clause != tc.clause || len(bound) != tc.args {
				t.Errorf("matchAny = %q with %d args, want %q with %d",
					clause, len(bound), tc.clause, tc.args)
			}
		})
	}
}

// Which values a facet list may carry (spec 027 #19): only the ones the filter
// can be given. The list form is comma-separated and its items are trimmed, so
// offering a value with a comma or with space at either end would be offering
// a filter that does not work.
func TestOnlyExpressibleValuesAreOffered(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"production", true},
		{"", false},
		{"search,web", false},
		{" padded", false},
		{"padded ", false},
		{"two words", true},
		{"a-b_c.d/e", true},
	} {
		if got := expressible(tc.value); got != tc.want {
			t.Errorf("expressible(%q) = %v, want %v", tc.value, got, tc.want)
		}
	}
}
