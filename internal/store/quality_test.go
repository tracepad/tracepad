package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
)

// The score rollup at the store layer (spec 025): one hour recomputed from the
// raw rows must equal what a live scan says about that hour, the score's own
// arrival has to dirty it, and a deletion has to be able to name the hour it
// corrects.

// The names the corpus below files.
const (
	nameHallucination = "hallucination"
	nameHelpful       = "helpful"
	nameVerdict       = "verdict"
)

// scoreFixture puts the hour of `rollupFixture` under judgement: a numeric
// name on two traces of two releases, a boolean one, a categorical one, an
// observation-level score, a session-only score and a text score — the last two
// being the ones the table must not hold.
func scoreFixture(t *testing.T, s *Store, projectID string) {
	t.Helper()
	writeScores(t, s, projectID, rollupHour*1e9,
		numericScore(1, hexTrace(1), "", nameHallucination, 0.2),
		numericScore(2, hexTrace(2), "", nameHallucination, 0.6),
		numericScore(3, hexTrace(3), "", nameHallucination, 0.4),
		// On the observation, so it lands in that observation's model row.
		numericScore(4, hexTrace(1), hexSpan(1), nameHelpful, 1),
		booleanScore(5, hexTrace(1), 1),
		booleanScore(6, hexTrace(2), 0),
		categoricalScore(7, hexTrace(1), "pass"),
		categoricalScore(8, hexTrace(2), "fail"),
		categoricalScore(9, hexTrace(3), "pass"),
		// Neither of these is in the table: one has no trace to borrow an
		// hour and a tuple from, the other has nothing to add up.
		sessionScore(10, "sess-1"),
		textScore(11, hexTrace(1)),
	)
}

func writeScores(t *testing.T, s *Store, projectID string, at int64, scores ...*Score) {
	t.Helper()
	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	for _, score := range scores {
		if score.Timestamp == 0 {
			score.Timestamp = at
		}
		if score.CreatedAt == 0 {
			score.CreatedAt = at
		}
	}
	if err := writer.Submit(t.Context(), &ScoreWrite{ProjectID: projectID, Scores: scores}); err != nil {
		t.Fatal(err)
	}
}

func scoreID(n int) string { return fmt.Sprintf("%032x", 1000+n) }

// stampTraces dates the seeded traces on the fixture's own clock.
//
// Ingest stamps `updated_at` from the real one, so a test that drives passes on
// a simulated clock years behind it finds every trace changed on every pass —
// every hour dirty, and every assertion about *what* made it dirty vacuous.
// That is the exact shape spec 013 #15 was written about, and the reason these
// tests set the column rather than trusting it.
func stampTraces(t *testing.T, s *Store, projectID string, at int64) {
	t.Helper()
	if _, err := s.db.Exec(
		`UPDATE traces SET updated_at = ? WHERE project_id = ?`, at, projectID); err != nil {
		t.Fatal(err)
	}
}

func numericScore(n int, trace, observation, name string, value float64) *Score {
	return &Score{ID: scoreID(n), TraceID: trace, ObservationID: observation,
		Name: name, DataType: ScoreNumeric, Value: &value}
}

func booleanScore(n int, trace string, value float64) *Score {
	return &Score{ID: scoreID(n), TraceID: trace, Name: nameHelpful + "-yes",
		DataType: ScoreBoolean, Value: &value}
}

func categoricalScore(n int, trace, value string) *Score {
	return &Score{ID: scoreID(n), TraceID: trace, Name: nameVerdict,
		DataType: ScoreCategorical, StringValue: &value}
}

func sessionScore(n int, session string) *Score {
	value := 1.0
	return &Score{ID: scoreID(n), SessionID: session, Name: nameHallucination,
		DataType: ScoreNumeric, Value: &value}
}

func textScore(n int, trace string) *Score {
	value := "the model repeated itself"
	return &Score{ID: scoreID(n), TraceID: trace, Name: "rationale",
		DataType: ScoreText, StringValue: &value}
}

// scoreKey is the tuple, spelled the way the assertions read it.
func scoreKey(row ScoreStatsRow) string {
	return strings.Join([]string{row.Environment, row.Release, row.Model,
		row.Name, row.DataType, row.Category}, "|")
}

func rolledScoreRows(t *testing.T, s *Store, projectID string, hour int64) map[string]ScoreStatsRow {
	t.Helper()
	rows := map[string]ScoreStatsRow{}
	if err := s.ScoresRollupRows(projectID, hour, hour+SecondsPerHour, nil, "",
		func(row ScoreStatsRow) { rows[scoreKey(row)] = row }); err != nil {
		t.Fatal(err)
	}
	return rows
}

// liveScoreRows folds the raw rows of a range the way the read seam's live half
// does, so the two can be compared tuple by tuple.
func liveScoreRows(t *testing.T, s *Store, projectID string, from, to int64) map[string]ScoreStatsRow {
	t.Helper()
	rows := map[string]ScoreStatsRow{}
	if err := s.ScoreSamples(projectID, from, to, nil, "", func(sample ScoreStatsRow) {
		row, held := rows[scoreKey(sample)]
		if !held {
			row = ScoreStatsRow{Hour: sample.Hour, Environment: sample.Environment,
				Release: sample.Release, Model: sample.Model, Name: sample.Name,
				DataType: sample.DataType, Category: sample.Category}
		}
		row.Count += sample.Count
		row.Sum += sample.Sum
		if sample.Min != nil && (row.Min == nil || *sample.Min < *row.Min) {
			row.Min = sample.Min
		}
		if sample.Max != nil && (row.Max == nil || *sample.Max > *row.Max) {
			row.Max = sample.Max
		}
		rows[scoreKey(sample)] = row
	}); err != nil {
		t.Fatal(err)
	}
	return rows
}

func keysSorted[V any](rows map[string]V) []string {
	out := make([]string, 0, len(rows))
	for key := range rows {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// The property the read seam rests on: a reader must not be able to tell which
// half of the watermark answered (spec 025 #7).
func TestRolledScoreHourEqualsTheLiveScan(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	scoreFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)

	rolled := rolledScoreRows(t, s, project.ID, rollupHour)
	live := liveScoreRows(t, s, project.ID, rollupHour*1e9, (rollupHour+SecondsPerHour)*1e9)

	if !strings.EqualFold(strings.Join(keysSorted(rolled), ","), strings.Join(keysSorted(live), ",")) {
		t.Fatalf("tuples differ:\n rolled %v\n live   %v", keysSorted(rolled), keysSorted(live))
	}
	for key, want := range live {
		got := rolled[key]
		if got.Count != want.Count || got.Sum != want.Sum {
			t.Errorf("%s: count/sum = %d/%v, live says %d/%v",
				key, got.Count, got.Sum, want.Count, want.Sum)
		}
		if !sameFloat(got.Min, want.Min) || !sameFloat(got.Max, want.Max) {
			t.Errorf("%s: extremes = %v..%v, live says %v..%v",
				key, got.Min, got.Max, want.Min, want.Max)
		}
	}
}

func sameFloat(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Decision 1's two placements, and the two kinds of score the table does not
// hold at all.
func TestScoreRollPlacesScoresByTheirTarget(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	scoreFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)
	rows := rolledScoreRows(t, s, project.ID, rollupHour)

	// The observation-level score sits under that observation's model; the
	// trace-level ones under the empty model, which is spec 013 #1's
	// discriminator read the same way.
	if row, ok := rows["production|2026.8.30|claude-sonnet-5|"+nameHelpful+"|numeric|"]; !ok || row.Count != 1 {
		t.Errorf("the observation-level score is not in its model's row: %v", keysSorted(rows))
	}
	// Two of them, on the two traces that share the tuple: one row is one
	// cell of the hour, not one score.
	if row, ok := rows["production|2026.8.30||"+nameHallucination+"|numeric|"]; !ok || row.Count != 2 {
		t.Errorf("the trace-level scores are not in the model = '' row: %v", keysSorted(rows))
	}

	// A categorical name is one row per value seen.
	for _, want := range []struct {
		key   string
		count int64
	}{
		{"production|2026.8.30||" + nameVerdict + "|categorical|pass", 1},
		{"production|2026.8.30||" + nameVerdict + "|categorical|fail", 1},
		{"production|2026.8.31||" + nameVerdict + "|categorical|pass", 1},
	} {
		if row, ok := rows[want.key]; !ok || row.Count != want.count {
			t.Errorf("%s = %+v, want a row of %d", want.key, row, want.count)
		}
	}

	// And neither the session-only score nor the text one is anywhere.
	for key := range rows {
		if strings.Contains(key, "|text|") || strings.Contains(key, "rationale") {
			t.Errorf("a text score is in the table: %s", key)
		}
	}
	var total int64
	for _, row := range rows {
		total += row.Count
	}
	// Nine of the eleven scores: the session-only one and the text one are
	// out, and every other score is counted exactly once.
	if total != 9 {
		t.Errorf("the hour counts %d scores, want the fixture's 9", total)
	}
}

// The four numbers of Decision 2, read off one row.
func TestScoreRowCarriesTheFourNumbers(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	scoreFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)
	rows := rolledScoreRows(t, s, project.ID, rollupHour)

	// The two hallucination scores of production/2026.8.30 are on different
	// traces, so they are two rows; the boolean pair is one row per release.
	boolTrue := rows["production|2026.8.30||"+nameHelpful+"-yes|boolean|"]
	if boolTrue.Count != 2 || boolTrue.Sum != 1 {
		t.Errorf("boolean row = %+v, want 2 graded and 1 true", boolTrue)
	}
	if boolTrue.Min != nil || boolTrue.Max != nil {
		t.Errorf("boolean row carries extremes %v..%v, want neither", boolTrue.Min, boolTrue.Max)
	}
	verdictRow := rows["production|2026.8.30||"+nameVerdict+"|categorical|pass"]
	if verdictRow.Sum != 0 || verdictRow.Min != nil || verdictRow.Max != nil {
		t.Errorf("categorical row = %+v, want the count alone", verdictRow)
	}
	// 0.2 and 0.6: the sum is what a mean is read out of, and the extremes
	// are the two values themselves.
	low, high := 0.2, 0.6
	numericRow := rows["production|2026.8.30||"+nameHallucination+"|numeric|"]
	if numericRow.Count != 2 || numericRow.Sum != 0.8 ||
		!sameFloat(numericRow.Min, &low) || !sameFloat(numericRow.Max, &high) {
		t.Errorf("numeric row = %+v, want 2 scores summing to 0.8 across 0.2..0.6", numericRow)
	}
}

// A re-roll recomputes the hour whole, so it converges rather than doubling —
// the property spec 013 #3 buys by refusing deltas.
func TestScoreRollIsIdempotent(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	scoreFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)
	first := rolledScoreRows(t, s, project.ID, rollupHour)

	// The same scores delivered again — an upsert, which is what a
	// correction is (spec 003 #3) — and the hour rolled again.
	scoreFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)
	second := rolledScoreRows(t, s, project.ID, rollupHour)

	if len(first) != len(second) {
		t.Fatalf("rows = %d after a re-roll, were %d", len(second), len(first))
	}
	for key, want := range first {
		if got := second[key]; got.Count != want.Count || got.Sum != want.Sum {
			t.Errorf("%s = %d/%v after a re-roll, was %d/%v",
				key, got.Count, got.Sum, want.Count, want.Sum)
		}
	}
}

// A score written after the pass dirties the hour of the trace it names, which
// is the whole reason `idx_scores_created` exists: the trace did not move, so
// no other question in the dirty set hears about it (spec 025 #3).
//
// The passes here are dated against the *real* clock, and that is the point.
// `seedTrace` stamps `updated_at` from `time.Now()`, so a pass dated in the
// fixture's own past finds every trace changed and every hour dirty — and a
// test of what makes an hour dirty would then prove nothing, which is the shape
// spec 013 #15 was written about.
func TestAScoreDirtiesItsTraceHour(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	stampTraces(t, s, project.ID, rollupHour*1e9)

	base := afterTheHour()
	passAt(t, s, base)

	if rows := rolledScoreRows(t, s, project.ID, rollupHour); len(rows) != 0 {
		t.Fatalf("the first pass wrote %d score rows over a corpus with no scores", len(rows))
	}
	before, err := s.RollupState(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dirty, err := s.dirtyHours(project.ID, before.LastPass, before.RolledUntil); err != nil {
		t.Fatal(err)
	} else if len(dirty) != 0 {
		t.Fatalf("hours %v are dirty before anything changed; the clock is lying", dirty)
	}

	// A judge grading the rolled hour, now.
	writeScores(t, s, project.ID, base.UnixNano(),
		numericScore(1, hexTrace(1), "", nameHallucination, 0.5))

	dirty, err := s.dirtyHours(project.ID, before.LastPass, before.RolledUntil)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 1 || dirty[0] != rollupHour {
		t.Fatalf("dirty hours = %v, want just the graded hour %d", dirty, rollupHour)
	}

	passAt(t, s, base.Add(2*DefaultRollupInterval))
	rows := rolledScoreRows(t, s, project.ID, rollupHour)
	if row, ok := rows["production|2026.8.30||"+nameHallucination+"|numeric|"]; !ok || row.Count != 1 {
		t.Fatalf("the next pass did not pick the score up: %v", keysSorted(rows))
	}

	// And a correction — the same id, a new value — moves `created_at`, so
	// the same question finds it again (spec 003 #3).
	writeScores(t, s, project.ID, base.Add(2*DefaultRollupInterval).UnixNano(),
		numericScore(1, hexTrace(1), "", nameHallucination, 0.9))
	passAt(t, s, base.Add(4*DefaultRollupInterval))

	rows = rolledScoreRows(t, s, project.ID, rollupHour)
	row := rows["production|2026.8.30||"+nameHallucination+"|numeric|"]
	if row.Count != 1 || row.Sum != 0.9 {
		t.Errorf("after the correction the row is %+v, want the one corrected value", row)
	}
}

// A trace whose own hour moved carries its scores with it, for free: they are
// keyed by *its* hour, and spec 013 #16's rule already re-rolls both hours.
func TestATraceMovingHourCarriesItsScores(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	scoreFixture(t, s, project.ID)
	stampTraces(t, s, project.ID, rollupHour*1e9)

	base := afterTheHour()
	passAt(t, s, base)
	if rows := rolledScoreRows(t, s, project.ID, rollupHour); len(rows) == 0 {
		t.Fatal("the backfill wrote no score rows")
	}

	// A late span of trace 1, starting an hour earlier: a trace's timestamp
	// is the earliest start among its observations, so the trace — and with
	// it every score filed under it — moves to the earlier hour.
	earlier := rollupHour - SecondsPerHour
	seedTrace(t, s, project.ID,
		&model.Trace{ID: hexTrace(1), Name: "run", Environment: "production", Release: "2026.8.30"},
		&model.Observation{TraceID: hexTrace(1), ID: hexSpan(99), Type: model.TypeGeneration,
			Name: "call", Level: model.LevelDefault,
			StartTime: earlier * 1e9, EndTime: earlier*1e9 + 1e8, Model: "claude-sonnet-5"})
	stampTraces(t, s, project.ID, base.UnixNano())

	passAt(t, s, base.Add(2*DefaultRollupInterval))

	moved := rolledScoreRows(t, s, project.ID, earlier)
	if row, ok := moved["production|2026.8.30||"+nameVerdict+"|categorical|pass"]; !ok || row.Count != 1 {
		t.Errorf("the earlier hour did not gain the moved trace's scores: %v", keysSorted(moved))
	}
	left := rolledScoreRows(t, s, project.ID, rollupHour)
	if row, ok := left["production|2026.8.30||"+nameVerdict+"|categorical|pass"]; ok {
		t.Errorf("the old hour still counts the moved trace's score: %+v", row)
	}
}

// A deletion corrects its hour inside its own transaction, and corrects
// nothing else (spec 025 #22).
//
// The two tables the correction must not touch are wrong on purpose before it
// runs. The full `RollHour` job would repair them — rewriting `stats_hourly`,
// `users_hourly` and then the whole-history summary of every user of the hour —
// and that is precisely the fan-out a single `DELETE /scores/{id}` must not
// carry: it holds the one writer for as long as it takes. Substitute
// `RollHour` back into `correctScoreHours` and the two survival assertions
// below fail.
func TestDeletingAScoreCorrectsOnlyTheScoreRollup(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	// A graded trace with a user on it, so the hour has a summary the full
	// job would rewrite.
	start := rollupHour*1e9 + 5e9
	seedTrace(t, s, project.ID,
		&model.Trace{ID: hexTrace(5), Name: "run", UserID: "u-1",
			Environment: "production", Release: "2026.8.30"},
		&model.Observation{TraceID: hexTrace(5), ID: hexSpan(5), Type: model.TypeGeneration,
			Name: "call", Level: model.LevelDefault, StartTime: start, EndTime: start + 1e8,
			Model: "claude-sonnet-5"})
	scoreFixture(t, s, project.ID)
	stampTraces(t, s, project.ID, rollupHour*1e9)
	passAt(t, s, afterTheHour())

	numeric := "production|2026.8.30||" + nameHallucination + "|numeric|"
	if row := rolledScoreRows(t, s, project.ID, rollupHour)[numeric]; row.Count != 2 {
		t.Fatalf("the pass rolled %+v, want the two scores of that release", row)
	}

	for _, statement := range []string{
		`UPDATE stats_hourly SET count = 999 WHERE project_id = ?`,
		`UPDATE users SET traces = 999 WHERE project_id = ?`,
	} {
		if _, err := s.db.Exec(statement, project.ID); err != nil {
			t.Fatal(err)
		}
	}

	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	// Score 1 is `hallucination` 0.2 on trace 1; score 2 is 0.6 on trace 2,
	// the same tuple. Retracting the first leaves the second.
	if err := writer.Submit(context.Background(),
		&ScoreDelete{ProjectID: project.ID, ID: scoreID(1)}); err != nil {
		t.Fatal(err)
	}

	row := rolledScoreRows(t, s, project.ID, rollupHour)[numeric]
	if row.Count != 1 || row.Sum != 0.6 {
		t.Errorf("after the retraction the row is %+v, want the one score that is left", row)
	}
	for _, want := range []struct {
		what  string
		query string
	}{
		{"stats_hourly", `SELECT MIN(count) FROM stats_hourly WHERE project_id = ?`},
		{"the user summaries", `SELECT MIN(traces) FROM users WHERE project_id = ?`},
	} {
		var value sql.NullInt64
		if err := s.db.QueryRow(want.query, project.ID).Scan(&value); err != nil {
			t.Fatal(err)
		}
		if !value.Valid {
			t.Fatalf("%s holds no rows; the assertion below would be vacuous", want.what)
		}
		if value.Int64 != 999 {
			t.Errorf("%s was rewritten by a score retraction (%d): only the score rollup is its business",
				want.what, value.Int64)
		}
	}
}

// The three scores a deletion has no hour to correct, and must not fail over:
// one with no trace, one whose trace has not arrived, and a `text` one.
func TestDeletingAScoreTheRollupNeverHeld(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	scoreFixture(t, s, project.ID)
	stampTraces(t, s, project.ID, rollupHour*1e9)
	passAt(t, s, afterTheHour())
	before := rolledScoreRows(t, s, project.ID, rollupHour)

	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	for _, tc := range []struct {
		name string
		id   string
	}{
		{"a session-only score", scoreID(10)},
		{"a text score", scoreID(11)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := writer.Submit(context.Background(),
				&ScoreDelete{ProjectID: project.ID, ID: tc.id}); err != nil {
				t.Fatal(err)
			}
		})
	}
	if after := rolledScoreRows(t, s, project.ID, rollupHour); len(after) != len(before) {
		t.Errorf("the rollup went from %d rows to %d over scores it never held",
			len(before), len(after))
	}
}

// Receive time is the transaction's, not the handler's (spec 025 #23).
//
// The dirty set asks `created_at > last_pass`, and `commitMargin` is a second
// — sized for a stamp taken *inside* the write transaction, as
// `traces.updated_at` is. Stamped before the group-commit queue instead, a
// write that waited longer than that in it carried a `created_at` already
// behind the next pass's cutoff, and no pass ever looked at that hour again.
func TestAScoreTakesItsReceiveTimeFromTheTransaction(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)

	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()

	// One score the caller stamped and one it did not.
	stamped := numericScore(1, hexTrace(1), "", nameHallucination, 0.2)
	stamped.Timestamp, stamped.CreatedAt = rollupHour*1e9, rollupHour*1e9
	fresh := numericScore(2, hexTrace(2), "", nameHallucination, 0.6)
	fresh.Timestamp = rollupHour * 1e9

	before := time.Now().UnixNano()
	if err := writer.Submit(context.Background(), &ScoreWrite{
		ProjectID: project.ID, Scores: []*Score{stamped, fresh}}); err != nil {
		t.Fatal(err)
	}
	after := time.Now().UnixNano()

	created := func(id string) int64 {
		t.Helper()
		var at int64
		if err := s.db.QueryRow(
			`SELECT created_at FROM scores WHERE project_id = ? AND id = ?`,
			project.ID, id).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	if at := created(scoreID(2)); at < before || at > after {
		t.Errorf("created_at = %d, want the moment the transaction stored it, in [%d, %d]",
			at, before, after)
	}
	// A caller with a clock of its own keeps it: every fixture in this
	// package dates its rows, and the sweep reads the column.
	if at := created(scoreID(1)); at != rollupHour*1e9 {
		t.Errorf("created_at = %d, want the %d the caller stamped", at, rollupHour*1e9)
	}
}

// The backfill of Decision 5, exercised where it can be: a database whose
// hours are rolled and whose score table is empty is exactly the position an
// upgrade leaves, and `last_pass = 0` is the one line that repairs it.
func TestResettingLastPassBackfillsTheScoreRows(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	scoreFixture(t, s, project.ID)
	stampTraces(t, s, project.ID, rollupHour*1e9)

	base := afterTheHour()
	passAt(t, s, base)

	// The state migration 0015 finds: every hour rolled, nothing dirty, and
	// no score rows at all.
	if _, err := s.db.Exec(`DELETE FROM scores_hourly WHERE project_id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	passAt(t, s, base.Add(2*DefaultRollupInterval))
	if rows := rolledScoreRows(t, s, project.ID, rollupHour); len(rows) != 0 {
		t.Fatalf("a pass over a clean watermark wrote %d rows; the hour is not dirty", len(rows))
	}

	// The migration's own line.
	if _, err := s.db.Exec(`UPDATE stats_rollup SET last_pass = 0`); err != nil {
		t.Fatal(err)
	}
	passAt(t, s, base.Add(4*DefaultRollupInterval))

	if rows := rolledScoreRows(t, s, project.ID, rollupHour); len(rows) == 0 {
		t.Fatal("the backfill pass left the score rollup empty")
	}
}

// TestScoreQueriesSeekTheirIndexes is the EXPLAIN check Decision 14 asks each
// new query for by name. This store never runs ANALYZE, so an index that is not
// actually chosen is an index that is not there — spec 023 lost 29× to exactly
// that, and the fix was found by a test that reads the plan.
func TestScoreQueriesSeekTheirIndexes(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	scoreFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)

	rangeQuery, rangeArgs := scoreRollupQuery(project.ID, rollupHour, rollupHour+SecondsPerHour, nil, "")
	namedQuery, namedArgs := scoreRollupQuery(project.ID, rollupHour, rollupHour+SecondsPerHour, nil, nameHallucination)
	liveQuery, liveArgs := scoreLiveQuery(project.ID, rollupHour*1e9, (rollupHour+SecondsPerHour)*1e9, nil, "")
	liveNamed, liveNamedArgs := scoreLiveQuery(project.ID, rollupHour*1e9,
		(rollupHour+SecondsPerHour)*1e9, []string{"production"}, nameHallucination)

	for _, tc := range []struct {
		name    string
		query   string
		args    []any
		indexes []string
	}{
		{
			name:  "the hour roll",
			query: scoreRollQuery,
			args: []any{project.ID, rollupHour, project.ID,
				rollupHour * 1e9, (rollupHour + SecondsPerHour) * 1e9},
			// Bounded by the hour's traces and joined to their scores;
			// driving off `scores` instead would read every score the
			// project holds, once per hour of a backfill.
			indexes: []string{"idx_traces_timestamp", "idx_scores_trace"},
		},
		{
			name:    "the score-side dirty query",
			query:   dirtyScoreHoursQuery,
			args:    []any{SecondsPerHour, SecondsPerHour, project.ID, int64(0)},
			indexes: []string{"idx_scores_created"},
		},
		{
			name:  "the seam's range read",
			query: rangeQuery,
			args:  rangeArgs,
			// The primary key is the query (Decision 1): its leading
			// columns are `(project_id, hour)`.
			indexes: []string{"sqlite_autoindex_scores_hourly_1"},
		},
		{
			name:    "the seam's range read for one name",
			query:   namedQuery,
			args:    namedArgs,
			indexes: []string{"idx_scores_hourly_name"},
		},
		{
			name:    "the live tail",
			query:   liveQuery,
			args:    liveArgs,
			indexes: []string{"idx_traces_timestamp", "idx_scores_trace"},
		},
		{
			// The one the unary `+` in `scoreLiveQuery` exists for: with
			// `idx_scores_name` driving the join instead, every trace of
			// the range walks every score of that name in the project.
			name:    "the live tail for one name",
			query:   liveNamed,
			args:    liveNamedArgs,
			indexes: []string{"idx_traces_timestamp", "idx_scores_trace"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := s.explainQueryPlan(tc.query, tc.args...)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(plan, "\n")
			for _, index := range tc.indexes {
				if !strings.Contains(joined, index) {
					t.Errorf("does not use %s:\n%s", index, joined)
				}
			}
			if strings.Contains(joined, "SCAN scores") || strings.Contains(joined, "SCAN traces") {
				t.Errorf("scans a raw table:\n%s", joined)
			}
			if strings.Contains(joined, "idx_scores_name") {
				t.Errorf("drives off idx_scores_name, which carries no trace id:\n%s", joined)
			}
		})
	}
}

// The trace sweep deletes traces, not history — the whole point of spec 013 #6,
// applied to the third table it now leaves standing.
func TestTheTraceSweepLeavesTheScoreRollup(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	scoreFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)

	before := len(rolledScoreRows(t, s, project.ID, rollupHour))
	if before == 0 {
		t.Fatal("nothing was rolled, so nothing is being proved")
	}

	// A window that has long since passed over the fixture's own hour.
	if _, err := s.db.Exec(
		`UPDATE projects SET retention_days = 1 WHERE id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	// The sweep deletes by *arrival*, and ingest stamped these rows a
	// moment ago, so the clock moves rather than the window.
	sweeper := s.NewSweeper(writer, SweepOptions{
		Now: func() time.Time { return time.Now().Add(10 * 24 * time.Hour) }})
	if err := sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}

	var traces int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM traces WHERE project_id = ?`, project.ID).Scan(&traces); err != nil {
		t.Fatal(err)
	}
	if traces != 0 {
		t.Fatalf("traces = %d, want the window to have taken them", traces)
	}
	if after := len(rolledScoreRows(t, s, project.ID, rollupHour)); after != before {
		t.Errorf("score rows = %d after the trace sweep, want the %d it left alone", after, before)
	}
}

// `stats_retention_days` owns all three tables and nothing else (spec 025,
// config additions).
func TestStatsRetentionSweepsTheScoreRollup(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	scoreFixture(t, s, project.ID)
	stampTraces(t, s, project.ID, rollupHour*1e9)
	passAt(t, s, afterTheHour())

	if len(rolledScoreRows(t, s, project.ID, rollupHour)) == 0 {
		t.Fatal("the pass wrote no score rows")
	}
	if _, err := s.db.Exec(
		`UPDATE projects SET stats_retention_days = 1 WHERE id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	passAt(t, s, time.Unix(rollupHour, 0).Add(10*24*time.Hour))

	hours, err := s.ScoresRollupHours(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hours) != 0 {
		t.Errorf("rolled score hours = %v, want none past the stats window", hours)
	}
	var scores int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM scores WHERE project_id = ?`, project.ID).Scan(&scores); err != nil {
		t.Fatal(err)
	}
	if scores == 0 {
		t.Error("the stats window deleted raw scores; it owns the rollup and nothing else")
	}
}

// A user-data erasure corrects the score rollup through the re-roll it already
// performs (spec 025 #4): the scores went with the traces, so the hour has to
// stop counting them. Since spec 023 #19 that re-roll is inside the chunk's
// own transaction, so the erase job alone is the whole correction.
func TestErasingAUserCorrectsTheScoreRollup(t *testing.T) {
	s, project := readStore(t)

	// One user's trace in the fixture's hour, with a score on it.
	start := rollupHour*1e9 + 30*int64(1e9)
	seedTrace(t, s, project.ID,
		&model.Trace{ID: hexTrace(50), Name: "run", Environment: "production",
			Release: "2026.8.30", UserID: "erase-me"},
		&model.Observation{TraceID: hexTrace(50), ID: hexSpan(50), Type: model.TypeGeneration,
			Name: "call", Level: model.LevelDefault,
			StartTime: start, EndTime: start + 1e8, Model: "claude-sonnet-5"})
	writeScores(t, s, project.ID, rollupHour*1e9,
		numericScore(60, hexTrace(50), "", nameHallucination, 0.7))
	roll(t, s, project.ID, rollupHour)
	advance(t, s, project.ID, rollupHour)

	if rows := rolledScoreRows(t, s, project.ID, rollupHour); len(rows) != 1 {
		t.Fatalf("rows = %v, want the one score", keysSorted(rows))
	}

	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	erase := &UserDataErase{ProjectID: project.ID, UserID: "erase-me",
		Confirm: "erase-me", Limit: 100}
	if err := writer.Submit(t.Context(), erase); err != nil {
		t.Fatal(err)
	}

	if rows := rolledScoreRows(t, s, project.ID, rollupHour); len(rows) != 0 {
		t.Errorf("the erased user's scores are still counted: %v", keysSorted(rows))
	}
}

// A purged project takes its score rows with it, through the cascade every
// other per-project table hangs off.
func TestPurgingAProjectLeavesNoScoreRows(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	scoreFixture(t, s, project.ID)
	roll(t, s, project.ID, rollupHour)

	if _, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM scores_hourly`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d score rows survived the project, want none", left)
	}
}

// pastTheWindow is a clock far enough past the fixture's hour that a one-day
// retention window has closed over it. `afterTheHour` is two hours on, which
// closes the hour for the aggregator and freezes nothing at all — the mistake
// the first draft of these three tests made.
func pastTheWindow() time.Time { return time.Unix(rollupHour, 0).Add(10 * 24 * time.Hour) }

// The hour a shared freeze gate refused for ever (spec 025 #21).
//
// The window is measured against the client's timestamp while the sweep
// deletes by arrival, so history imported into an install that had already
// rolled it is "past the window" and completely intact. Under one gate for all
// three tables, `stats_hourly` already held rows for such an hour, so the whole
// job returned early and migration 0015's backfill wrote no score rows for it —
// ever, with the traces and the scores sitting right there.
func TestABackfillFillsAFrozenHourWhoseTracesAreIntact(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	scoreFixture(t, s, project.ID)
	stampTraces(t, s, project.ID, rollupHour*1e9)

	// Rolled before the score table existed: the statistics hold the hour and
	// the score rollup does not, which is what an upgrade finds.
	base := afterTheHour()
	passAt(t, s, base)
	if _, err := s.db.Exec(`DELETE FROM scores_hourly WHERE project_id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	if rows := rolledRows(t, s, project.ID, rollupHour); len(rows) == 0 {
		t.Fatal("the statistics were not rolled, so the test proves nothing")
	}

	// A retention window the fixture's own hour is long past — by the client's
	// clock. Nothing has swept: `ingested_at` is a moment ago.
	if _, err := s.db.Exec(
		`UPDATE projects SET retention_days = 1 WHERE id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	var traces int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM traces WHERE project_id = ?`, project.ID).Scan(&traces); err != nil {
		t.Fatal(err)
	}
	if traces == 0 {
		t.Fatal("the traces are gone, which is the other case entirely")
	}

	// The migration's own line, and the pass it buys.
	if _, err := s.db.Exec(`UPDATE stats_rollup SET last_pass = 0`); err != nil {
		t.Fatal(err)
	}
	// Ten days on, so the fixture's hour really is past a one-day window —
	// which is what makes it frozen, and what the first pass was not.
	passAt(t, s, pastTheWindow())

	rows := rolledScoreRows(t, s, project.ID, rollupHour)
	if len(rows) == 0 {
		t.Fatal("the backfill left a frozen hour empty although its traces are intact")
	}
	if row, ok := rows["production|2026.8.30||"+nameHallucination+"|numeric|"]; !ok || row.Count != 2 {
		t.Errorf("the frozen hour's score rows are wrong: %v", keysSorted(rows))
	}
}

// The other half of the same rule: once the score rollup holds an hour, a
// re-roll past the window must not touch it — which is what spec 013 #11
// protects, asked of this table.
func TestAFrozenScoreHourIsNotRecomputed(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	scoreFixture(t, s, project.ID)
	stampTraces(t, s, project.ID, rollupHour*1e9)

	base := afterTheHour()
	passAt(t, s, base)
	before := rolledScoreRows(t, s, project.ID, rollupHour)
	if len(before) == 0 {
		t.Fatal("the pass wrote no score rows")
	}

	// Past the window now, with rows standing — and the raw scores taken, the
	// way retention takes them with their targets.
	if _, err := s.db.Exec(
		`UPDATE projects SET retention_days = 1 WHERE id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM scores WHERE project_id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE stats_rollup SET last_pass = 0`); err != nil {
		t.Fatal(err)
	}
	passAt(t, s, pastTheWindow())

	after := rolledScoreRows(t, s, project.ID, rollupHour)
	if len(after) != len(before) {
		t.Errorf("the frozen hour was demolished: %d rows, was %d", len(after), len(before))
	}
}

// And the case the rule does *not* change: an hour past the window whose
// traces retention really did take writes nothing, because the join finds
// nothing — which is the truth, since the scores went with their targets.
func TestASweptHourGetsNoScoreRows(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	scoreFixture(t, s, project.ID)
	stampTraces(t, s, project.ID, rollupHour*1e9)

	base := afterTheHour()
	passAt(t, s, base)
	if _, err := s.db.Exec(`DELETE FROM scores_hourly WHERE project_id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	// The sweep's work, done here directly: the traces and their scores gone,
	// the statistics standing (spec 013 #6).
	for _, statement := range []string{
		`DELETE FROM scores WHERE project_id = ?`,
		`DELETE FROM observations WHERE project_id = ?`,
		`DELETE FROM traces WHERE project_id = ?`,
	} {
		if _, err := s.db.Exec(statement, project.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(
		`UPDATE projects SET retention_days = 1 WHERE id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE stats_rollup SET last_pass = 0`); err != nil {
		t.Fatal(err)
	}
	passAt(t, s, pastTheWindow())

	if rows := rolledScoreRows(t, s, project.ID, rollupHour); len(rows) != 0 {
		t.Errorf("a swept hour grew score rows out of nothing: %v", keysSorted(rows))
	}
	// And the statistics it stands beside were not demolished either.
	if rows := rolledRows(t, s, project.ID, rollupHour); len(rows) == 0 {
		t.Error("the statistics of a frozen hour were recomputed away")
	}
}
