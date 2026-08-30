package store

import (
	"context"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
)

// The aggregator's pass (spec 013 #3–#5): what it rolls, what it leaves to
// the live tail, how it catches up, and the hours it must refuse to touch.

// passAt runs one pass with the clock stopped at the given instant.
func passAt(t *testing.T, s *Store, at time.Time) *Aggregator {
	t.Helper()
	writer, err := s.NewWriter(WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	aggregator := s.NewAggregator(writer, RollupOptions{
		Interval: DefaultRollupInterval,
		Now:      func() time.Time { return at },
	})
	if err := aggregator.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	return aggregator
}

// afterTheHour is a clock far enough past the fixture's hour that the hour
// counts as closed.
func afterTheHour() time.Time {
	return time.Unix(rollupHour+2*SecondsPerHour, 0)
}

// The first pass on a database nobody has rolled is the backfill: it walks
// from the oldest trace forward, and until it has run every query is the live
// scan (spec 013 #5).
func TestTheFirstPassBackfills(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)

	state, err := s.RollupState(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.RolledUntil != 0 {
		t.Fatalf("rolled_until = %d before any pass, want 0 — the tail is everything",
			state.RolledUntil)
	}

	passAt(t, s, afterTheHour())

	hours, err := s.StatsRollupHours(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hours) != 1 || hours[0] != rollupHour {
		t.Fatalf("rolled hours = %v, want just %d", hours, rollupHour)
	}
	// Three trace-unit tuples — production at two releases, and staging —
	// and the two models among them, which is Decision 1's one table
	// carrying both units.
	if rows := rolledRows(t, s, project.ID, rollupHour); len(rows) != 5 {
		t.Errorf("the backfilled hour has %d rows, want the fixture's 5: %v",
			len(rows), keysOf(rows))
	}
	state, err = s.RollupState(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.RolledUntil <= rollupHour {
		t.Errorf("rolled_until = %d, want past the hour it just rolled", state.RolledUntil)
	}
}

// The hour in progress is never rolled: it belongs to the live tail until it
// has been closed for a tick (spec 013 #4, edge cases).
func TestTheOpenHourIsLeftToTheLiveTail(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)

	// A clock inside the fixture's own hour.
	passAt(t, s, time.Unix(rollupHour+30*60, 0))

	hours, err := s.StatsRollupHours(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hours) != 0 {
		t.Errorf("rolled %v while the hour was still open", hours)
	}
	state, err := s.RollupState(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.RolledUntil > rollupHour {
		t.Errorf("rolled_until = %d, want at or before the open hour %d",
			state.RolledUntil, rollupHour)
	}
}

// A span that arrives for an hour already rolled dirties it, and the next
// pass corrects it (spec 013 #4).
func TestAPassCorrectsTheHourALateSpanTouched(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	passAt(t, s, afterTheHour())
	before := rolledRows(t, s, project.ID, rollupHour)["production|2026.8.30|"]

	lateStart := rollupHour*1e9 + 45*1e9
	late := &model.Trace{ID: hexTrace(21), Name: "late",
		Environment: "production", Release: "2026.8.30"}
	seedTrace(t, s, project.ID, late, &model.Observation{
		TraceID: late.ID, ID: hexSpan(21), Type: model.TypeSpan, Name: "step",
		Level: model.LevelDefault, StartTime: lateStart, EndTime: lateStart + 7e6})

	passAt(t, s, afterTheHour().Add(time.Minute))

	after := rolledRows(t, s, project.ID, rollupHour)["production|2026.8.30|"]
	if after.Count != before.Count+1 {
		t.Errorf("count = %d after the correcting pass, want %d",
			after.Count, before.Count+1)
	}
}

// A pass that rolled nothing moves `last_pass` and leaves the watermark
// alone. The watermark is a claim that the rollup can answer for the hours
// behind it, and a pass that wrote no row for an hour has made no such claim
// (spec 013 #12).
func TestAQuietPassMovesTheCutoffAndNotTheWatermark(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)

	passAt(t, s, afterTheHour())
	first, err := s.RollupState(project.ID)
	if err != nil {
		t.Fatal(err)
	}

	later := afterTheHour().Add(2 * time.Hour)
	passAt(t, s, later)
	second, err := s.RollupState(project.ID)
	if err != nil {
		t.Fatal(err)
	}

	if second.RolledUntil != first.RolledUntil {
		t.Errorf("rolled_until moved from %d to %d over a pass that rolled nothing",
			first.RolledUntil, second.RolledUntil)
	}
	if second.LastPass <= first.LastPass {
		t.Errorf("last_pass stayed at %d, so the next pass would re-examine everything",
			second.LastPass)
	}
}

// The defect the live check of this PR found: a pass over an empty database
// used to move the watermark to now, and history imported afterwards then sat
// *behind* a watermark whose rollup knew nothing about it — so the statistics
// answered zero while the listing showed the traces. The watermark must never
// outrun what the pass actually rolled.
func TestHistoryImportedAfterAnEmptyPassIsStillCounted(t *testing.T) {
	s, project := readStore(t)

	// A pass with nothing to do, as happens on every fresh install.
	passAt(t, s, afterTheHour())
	state, err := s.RollupState(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.RolledUntil != 0 {
		t.Fatalf("rolled_until = %d after a pass over an empty project, want 0: "+
			"every hour must still be answered live", state.RolledUntil)
	}

	// Now an import of history, whose hours are behind that pass.
	rollupFixture(t, s, project.ID)
	state, err = s.RollupState(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.RolledUntil > rollupHour {
		t.Fatalf("rolled_until = %d is past the imported hour %d, so the seam would "+
			"ask an empty rollup about it", state.RolledUntil, rollupHour)
	}

	// And once a pass has rolled it, the watermark covers it and no more.
	passAt(t, s, afterTheHour())
	state, err = s.RollupState(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.RolledUntil != rollupHour+SecondsPerHour {
		t.Errorf("rolled_until = %d, want just past the one hour that was rolled (%d)",
			state.RolledUntil, rollupHour+SecondsPerHour)
	}
}

// Spec 013 #11: an hour past the trace-retention window is frozen. The raw
// rows behind it are gone by design, so recomputing it from the fragment that
// arrived late would replace the history with the fragment.
func TestAFrozenHourSurvivesALateFragment(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retention *int
		rerolled  bool
	}{
		{"a project that keeps its traces for a week", intPtr(7), false},
		{"a project that keeps them forever", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, project := readStore(t)
			rollupFixture(t, s, project.ID)

			// Roll the hour, then delete its traces the way the sweep
			// does: the rollup stays, which is Decision 6.
			passAt(t, s, afterTheHour())
			rolled := rolledRows(t, s, project.ID, rollupHour)["production|2026.8.30|"]
			if rolled.Count == 0 {
				t.Fatal("the fixture's hour did not roll")
			}
			if _, err := s.db.Exec(
				`DELETE FROM observations WHERE project_id = ?`, project.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(
				`DELETE FROM traces WHERE project_id = ?`, project.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(
				`UPDATE projects SET retention_days = ? WHERE id = ?`,
				tc.retention, project.ID); err != nil {
				t.Fatal(err)
			}

			// One late fragment for that same hour, which spec 005 #1
			// explicitly allows to exist.
			lateStart := rollupHour*1e9 + 10*1e9
			late := &model.Trace{ID: hexTrace(31), Name: "fragment",
				Environment: "production", Release: "2026.8.30"}
			seedTrace(t, s, project.ID, late, &model.Observation{
				TraceID: late.ID, ID: hexSpan(31), Type: model.TypeSpan, Name: "step",
				Level: model.LevelDefault, StartTime: lateStart, EndTime: lateStart + 1e6})

			// A pass long after the hour, so a week's retention has it
			// well outside the window.
			passAt(t, s, time.Unix(rollupHour, 0).Add(30*24*time.Hour))

			after := rolledRows(t, s, project.ID, rollupHour)["production|2026.8.30|"]
			switch {
			case tc.rerolled && after.Count != 1:
				t.Errorf("count = %d, want 1: with no window nothing is frozen and "+
					"the hour is recomputed from what remains", after.Count)
			case !tc.rerolled && after.Count != rolled.Count:
				t.Errorf("count = %d, want the frozen %d: a fragment must not "+
					"rewrite an hour whose raw rows retention took",
					after.Count, rolled.Count)
			}
		})
	}
}

// The rollup's own window deletes rolled rows and nothing else (spec 013 #6).
func TestStatsRetentionSweepsTheRollupOnly(t *testing.T) {
	s, project := readStore(t)
	rollupFixture(t, s, project.ID)
	passAt(t, s, afterTheHour())

	if _, err := s.db.Exec(
		`UPDATE projects SET stats_retention_days = 1 WHERE id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	passAt(t, s, time.Unix(rollupHour, 0).Add(10*24*time.Hour))

	hours, err := s.StatsRollupHours(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hours) != 0 {
		t.Errorf("rolled hours = %v, want none past the stats window", hours)
	}
	var traces int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM traces WHERE project_id = ?`, project.ID).Scan(&traces); err != nil {
		t.Fatal(err)
	}
	if traces == 0 {
		t.Error("the stats window deleted traces; it owns the rollup and nothing else")
	}
}

func intPtr(n int) *int { return &n }
