package store

import (
	"context"
	"fmt"
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
// pass corrects it (spec 013 #4). Both shapes are checked, because only one
// of them used to work: a *new* trace moves `ingested_at`, while a span
// joining an *existing* trace does not — and the second is how the spans of
// one trace ordinarily arrive (spec 013 #15, found in review of PR #28).
func TestAPassCorrectsTheHourALateSpanTouched(t *testing.T) {
	t.Run("a new trace in a rolled hour", func(t *testing.T) {
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
	})

	// This one runs on the real clock, deliberately. `ingested_at` is
	// stamped by the writer with `time.Now()`, so a pass driven by a
	// simulated clock years earlier finds every trace "arrived since the
	// last pass" and the test passes whatever the code does — which is
	// what the first draft of it did (found in review of PR #28).
	t.Run("another span of a trace that was already rolled", func(t *testing.T) {
		s, project := readStore(t)
		hour := HourOf(time.Now().Add(-2 * time.Hour).UnixNano())
		start := hour * int64(time.Second)

		seedTrace(t, s, project.ID,
			&model.Trace{ID: hexTrace(1), Environment: "production", Release: "r1"},
			&model.Observation{
				TraceID: hexTrace(1), ID: hexSpan(1), Type: model.TypeGeneration,
				Name: "answer", Level: model.LevelDefault, Model: "claude-sonnet-5",
				StartTime: start, EndTime: start + 200*1e6,
			})
		// The pass clock is read *after* the seed, so `last_pass` is
		// genuinely later than the trace's arrival. Reading it before
		// makes every trace look newly arrived and the test vacuous,
		// which is how the first two drafts of this passed without the
		// fix they exist to check.
		passAt(t, s, time.Now())

		before := rolledRows(t, s, project.ID, hour)["production|r1|claude-sonnet-5"]
		if before.Count != 1 {
			t.Fatalf("the generation rolled as %d rows, want 1", before.Count)
		}

		// The same trace, one more generation: no new trace row, and so
		// no new arrival time anywhere.
		seedTrace(t, s, project.ID,
			&model.Trace{ID: hexTrace(1), Environment: "production", Release: "r1"},
			&model.Observation{
				TraceID: hexTrace(1), ID: hexSpan(2), Type: model.TypeGeneration,
				Name: "retry", Level: model.LevelDefault, Model: "claude-sonnet-5",
				StartTime: start + 60*int64(time.Second), EndTime: start + 60*int64(time.Second) + 300*1e6,
			})
		passAt(t, s, time.Now())

		after := rolledRows(t, s, project.ID, hour)["production|r1|claude-sonnet-5"]
		if after.Count != 2 {
			t.Errorf("the model row counts %d after the correcting pass, want 2: "+
				"a span joining an existing trace must dirty its hour", after.Count)
		}
	})
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

// Freezing protects a stored summary from being recomputed out of rows
// retention has taken. An hour with no stored summary has nothing to protect,
// and must be rolled — otherwise a year of history imported today, with a
// thirty-day window, is frozen while completely intact, and the watermark
// walks over hours that no half of the read seam can answer (spec 013 #14,
// found in review of PR #28).
func TestBackdatedHistoryIsRolledRatherThanFrozen(t *testing.T) {
	s, project := readStore(t)
	// Imported now, but stamped a year ago by the client — which is what
	// an import is. Retention sweeps by arrival, so nothing is missing.
	rollupFixture(t, s, project.ID)
	if _, err := s.db.Exec(
		`UPDATE projects SET retention_days = 30 WHERE id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}

	passAt(t, s, time.Unix(rollupHour, 0).Add(365*24*time.Hour))

	rows := rolledRows(t, s, project.ID, rollupHour)
	if len(rows) == 0 {
		t.Fatal("the imported hour was frozen although the rollup had nothing to protect")
	}
	state, err := s.RollupState(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.RolledUntil != rollupHour+SecondsPerHour {
		t.Errorf("rolled_until = %d, want just past the hour that was rolled (%d)",
			state.RolledUntil, rollupHour+SecondsPerHour)
	}
}

// A trace's timestamp is the minimum start of its observations, so a late
// span with an earlier start moves the trace to an earlier hour — and the
// hour it left keeps counting it unless that one is re-rolled too. Both hours
// must come back, and the trace must be counted once (spec 013 #16, found in
// review of PR #28).
func TestATraceThatMovesHoursIsCountedOnce(t *testing.T) {
	s, project := readStore(t)
	// The real clock, so the pass and the writer agree about what
	// "since the last pass" means.
	second := HourOf(time.Now().Add(-2 * time.Hour).UnixNano())
	first := second - SecondsPerHour

	// The children arrive first and put the trace in the later hour.
	seedTrace(t, s, project.ID,
		&model.Trace{ID: hexTrace(1), Environment: "production", Release: "r1"},
		&model.Observation{
			TraceID: hexTrace(1), ID: hexSpan(1), Type: model.TypeGeneration,
			Name: "answer", Level: model.LevelDefault, Model: "claude-sonnet-5",
			StartTime: second*int64(time.Second) + 2*int64(time.Second),
			EndTime:   second*int64(time.Second) + 3*int64(time.Second),
		})
	passAt(t, s, time.Now())
	if rows := rolledRows(t, s, project.ID, second); len(rows) == 0 {
		t.Fatal("the later hour did not roll")
	}

	// The root arrives late, starting in the hour before: the trace moves.
	seedTrace(t, s, project.ID,
		&model.Trace{ID: hexTrace(1), Environment: "production", Release: "r1"},
		&model.Observation{
			TraceID: hexTrace(1), ID: hexSpan(2), Type: model.TypeSpan,
			Name: "root", Level: model.LevelDefault,
			StartTime: first*int64(time.Second) + 30*int64(time.Second),
			EndTime:   second*int64(time.Second) + 3*int64(time.Second),
		})
	passAt(t, s, time.Now())

	var counted int64
	for _, hour := range []int64{first, second} {
		for _, row := range rolledRows(t, s, project.ID, hour) {
			if row.Model == "" {
				counted += row.Count
			}
		}
	}
	if counted != 1 {
		t.Errorf("the trace is counted %d times across the two hours, want once: "+
			"the hour it left must be re-rolled with it", counted)
	}
}

// The other direction, which the first version of the fix above missed and
// its comment denied: a trace whose first delivery carried no usable start is
// stamped at the epoch (spec 004 #26), and a later span with a real start
// moves it *forwards*. The epoch hour then keeps counting it — a phantom
// 1970 bucket on every chart — unless it is re-rolled too (spec 013 #16,
// found in review of PR #28).
// Both retention shapes, because the first version of this test used only
// the one where nothing freezes: hour 0 is below *every* retention boundary,
// so with a window set the correction was scheduled and then discarded by the
// freeze, and the phantom survived in exactly the configuration an ordinary
// deployment runs (found in the fifth review of PR #28).
func TestATraceThatMovesForwardFromTheEpochIsCountedOnce(t *testing.T) {
	for _, retention := range []*int{nil, intPtr(7)} {
		name := "traces kept forever"
		if retention != nil {
			name = "a retention window set"
		}
		t.Run(name, func(t *testing.T) { epochMoveIsCountedOnce(t, retention) })
	}
}

func epochMoveIsCountedOnce(t *testing.T, retention *int) {
	s, project := readStore(t)
	if retention != nil {
		if _, err := s.db.Exec(`UPDATE projects SET retention_days = ? WHERE id = ?`,
			*retention, project.ID); err != nil {
			t.Fatal(err)
		}
	}
	hour := HourOf(time.Now().Add(-2 * time.Hour).UnixNano())

	// A first delivery that says nothing about when anything started.
	seedTrace(t, s, project.ID,
		&model.Trace{ID: hexTrace(1), Environment: "production"},
		&model.Observation{
			TraceID: hexTrace(1), ID: hexSpan(1), Type: model.TypeSpan,
			Name: "root", Level: model.LevelDefault,
		})
	passAt(t, s, time.Now())
	if rows := rolledRows(t, s, project.ID, 0); len(rows) == 0 {
		t.Fatal("the epoch hour did not roll, so this test proves nothing")
	}

	// And a later one that does, moving the trace out of the epoch.
	seedTrace(t, s, project.ID,
		&model.Trace{ID: hexTrace(1), Environment: "production"},
		&model.Observation{
			TraceID: hexTrace(1), ID: hexSpan(2), Type: model.TypeGeneration,
			Name: "answer", Level: model.LevelDefault,
			StartTime: hour*int64(time.Second) + int64(time.Second),
			EndTime:   hour*int64(time.Second) + 2*int64(time.Second),
		})
	passAt(t, s, time.Now())

	var counted int64
	for _, at := range []int64{0, hour} {
		for _, row := range rolledRows(t, s, project.ID, at) {
			if row.Model == "" {
				counted += row.Count
			}
		}
	}
	if counted != 1 {
		t.Errorf("the trace is counted %d times, want once: the epoch hour it left "+
			"must be re-rolled with it", counted)
	}
}

// That an hour *with* a summary stays frozen past the window is
// TestAFrozenHourSurvivesALateFragment above; the refinement narrows what is
// frozen, it does not loosen what is protected. The watermark cannot walk
// over an unanswerable hour any more, because a frozen hour now always has
// rows behind it.

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

// The stats window clears its backlog chunk by chunk within one pass, the way
// the trace sweeper does. One chunk a pass would have taken days over a large
// rollup while the operator believed the window was in force (found in review
// of PR #28).
func TestTheStatsWindowClearsMoreThanOneChunkPerPass(t *testing.T) {
	s, project := readStore(t)
	if _, err := s.db.Exec(
		`UPDATE projects SET stats_retention_days = 1 WHERE id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	// More rows than one chunk holds, spread over hours old enough to be
	// swept. Written directly: the point is the deletion loop, not how they
	// came to be there.
	rows := DefaultSweepChunk + 500
	for i := range rows {
		if _, err := s.db.Exec(
			`INSERT INTO stats_hourly
			   (project_id, hour, environment, release, model, count, error_count, latency)
			 VALUES (?, ?, ?, '', '', 1, 0, '[1]')`,
			project.ID, rollupHour+int64(i/50)*SecondsPerHour,
			fmt.Sprintf("env-%d", i%50)); err != nil {
			t.Fatal(err)
		}
	}

	passAt(t, s, time.Unix(rollupHour, 0).Add(400*24*time.Hour))

	var left int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM stats_hourly WHERE project_id = ?`, project.ID).
		Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d rolled rows survived one pass of %d, want the backlog cleared",
			left, rows)
	}
}

func intPtr(n int) *int { return &n }
