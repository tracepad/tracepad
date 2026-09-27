package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// The write jobs behind the admin API (spec 005). The HTTP tests cover what an
// operator sees; these cover the property an operator cannot see, which is that
// every destructive job decides for itself, from the row inside its own
// transaction, whether it is allowed to proceed (spec 003 Decision 20).

func TestShrinkingAWindowDemandsTheEchoFromInsideTheTransaction(t *testing.T) {
	f := newSweepFixture(t)

	// A handler that believed the window was 90 days would not ask for a
	// confirmation to set 30. The job asks anyway, because it reads the
	// stored row itself.
	update := &ProjectUpdate{
		ProjectID: f.project.ID,
		Retention: OptionalDays{Set: true, Value: days(30)},
	}
	err := f.writer.Submit(t.Context(), update)
	if err == nil {
		t.Fatal("a window shortened from forever to 30 days went through without an echo")
	}
	if !strings.Contains(err.Error(), `"test"`) {
		t.Errorf("err = %v, want it to name the project to echo", err)
	}
	if project, _ := f.store.ProjectByID(context.Background(), f.project.ID); project.RetentionDays != nil {
		t.Errorf("retention = %v after a refused update, want it untouched", project.RetentionDays)
	}

	update.Confirm = "test"
	if err := f.writer.Submit(t.Context(), update); err != nil {
		t.Fatalf("the confirmed update failed: %v", err)
	}
	if project, _ := f.store.ProjectByID(context.Background(), f.project.ID); *project.RetentionDays != 30 {
		t.Errorf("retention = %v, want 30", project.RetentionDays)
	}

	// Growing it back needs nothing: it destroys nothing.
	grow := &ProjectUpdate{
		ProjectID: f.project.ID,
		Retention: OptionalDays{Set: true, Value: days(60)},
	}
	if err := f.writer.Submit(t.Context(), grow); err != nil {
		t.Fatalf("growing the window asked for a confirmation: %v", err)
	}
}

// TestRawWindowShrinkIsMeasuredAgainstTheEffectiveOne: raw follows the trace
// window when it has none of its own (#6), so setting a raw window of 30 days
// under a 90-day trace window is a shrink even though the raw column was NULL.
func TestRawWindowShrinkIsMeasuredAgainstTheEffectiveOne(t *testing.T) {
	f := newSweepFixture(t)
	f.setRetention(t, f.project.ID, days(90), nil)

	project, err := f.store.ProjectByID(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	shorter := &ProjectUpdate{
		ProjectID: f.project.ID,
		RawWindow: OptionalDays{Set: true, Value: days(30)},
	}
	if !shorter.Shrinks(project) {
		t.Errorf("a 30-day raw window under a 90-day trace window is not seen as a shrink")
	}
	longer := &ProjectUpdate{
		ProjectID: f.project.ID,
		RawWindow: OptionalDays{Set: true, Value: days(180)},
	}
	if longer.Shrinks(project) {
		t.Errorf("a raw window longer than the trace window is seen as a shrink")
	}
	// Clearing raw back to "follow the traces" is not a shrink either.
	follow := &ProjectUpdate{
		ProjectID: f.project.ID,
		RawWindow: OptionalDays{Set: true},
	}
	if follow.Shrinks(project) {
		t.Errorf("clearing the raw window is seen as a shrink")
	}
}

// TestCreatingADeletedProjectsNameIsRefused: the name stays reserved through
// the grace window, so that restore always has its name to come back to (#9).
func TestCreatingADeletedProjectsNameIsRefused(t *testing.T) {
	f := newSweepFixture(t)

	deletion := &ProjectDelete{ProjectID: f.project.ID, Confirm: "test", Now: sweepNow.UnixNano()}
	if err := f.writer.Submit(t.Context(), deletion); err != nil {
		t.Fatal(err)
	}

	create := &ProjectCreate{Name: "test", Keys: KeyPair{PublicKey: "tp-pk-2", Secret: "tp-sk-2"}}
	err := f.writer.Submit(t.Context(), create)
	if err == nil {
		t.Fatal("a deleted project's name was reused while it was still restorable")
	}
	var rejection *Rejection
	if !errors.As(err, &rejection) || rejection.Kind != RejectConflict {
		t.Fatalf("err = %v, want a conflict", err)
	}
	if !strings.Contains(rejection.Message, "restored") {
		t.Errorf("message = %q, want it to point at the restore", rejection.Message)
	}

	// Once it is restored, so is everything about it.
	if err := f.writer.Submit(t.Context(), &ProjectRestore{ProjectID: f.project.ID}); err != nil {
		t.Fatal(err)
	}
	project, _ := f.store.ProjectByID(context.Background(), f.project.ID)
	if project.Deleted() {
		t.Errorf("the project is still deleted after a restore")
	}
}

// TestDeletingATwiceDeletedProject: the second delete is a conflict naming the
// purge date, not a silent no-op that resets the grace window.
func TestDeletingATwiceDeletedProject(t *testing.T) {
	f := newSweepFixture(t)
	first := &ProjectDelete{ProjectID: f.project.ID, Confirm: "test", Now: sweepNow.UnixNano()}
	if err := f.writer.Submit(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	second := &ProjectDelete{ProjectID: f.project.ID, Confirm: "test",
		Now: sweepNow.Add(time.Hour).UnixNano()}
	if err := f.writer.Submit(t.Context(), second); err == nil {
		t.Fatal("deleting a deleted project moved its purge date")
	}
	project, _ := f.store.ProjectByID(context.Background(), f.project.ID)
	if *project.DeletedAt != sweepNow.UnixNano() {
		t.Errorf("deleted_at = %d, want the first deletion's %d", *project.DeletedAt, sweepNow.UnixNano())
	}
}

// TestErasureCorrectsTheHoursOfEachChunkAsItCommits (spec 023 #19): the
// re-roll of spec 013 #7 rides inside the chunk's own transaction, so a
// request cut off between chunks leaves no hour counting traces that are
// gone — and a repeat, which only sees the traces that remain, has nothing
// to miss. The chunk here cuts an hour, and the user is not written
// back into the per-user tables by the roll of that hour (review of PR
// #65): between a hang-up and the repeat they are gone from the listing,
// as `docs/admin.md` promises. The last hour sits at the watermark, and
// the erasure leaves it to the live scan.
func TestErasureCorrectsTheHoursOfEachChunkAsItCommits(t *testing.T) {
	s, project := readStore(t)

	// Three hours; in each, two traces of the user being erased and one of
	// a bystander. Seeded in hour order, which is the order the chunks
	// take them in. The watermark stands after the second hour: the third
	// is rolled here as a pass that crashed before advancing would leave
	// it, and stays as it is — the seam answers it live.
	hours := []int64{rollupHour, rollupHour + SecondsPerHour, rollupHour + 2*SecondsPerHour}
	n := 0
	for _, hour := range hours {
		for _, user := range []string{"forget-me", "forget-me", "keep"} {
			n++
			seedUserTrace(t, s, project.ID, userSeed{n: n, user: user, session: "s",
				environment: "production", model: "claude-sonnet-5", latencyMs: 50,
				hour: hour, offsetSeconds: int64(n)})
		}
		roll(t, s, project.ID, hour)
	}
	advance(t, s, project.ID, hours[1])
	rolled := func(hour int64) int64 {
		t.Helper()
		var count int64
		for _, row := range rolledRows(t, s, project.ID, hour) {
			if row.Model == "" {
				count += row.Count
			}
		}
		return count
	}
	for _, hour := range hours {
		if got := rolled(hour); got != 3 {
			t.Fatalf("hour %d rolled %d traces, want 3", hour, got)
		}
	}

	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	erase := func() *UserDataErase {
		t.Helper()
		chunk := &UserDataErase{ProjectID: project.ID, UserID: "forget-me",
			Confirm: "forget-me", Limit: 1, Now: (rollupHour + 3*SecondsPerHour) * 1e9}
		if err := writer.Submit(t.Context(), chunk); err != nil {
			t.Fatal(err)
		}
		return chunk
	}
	gone := func(when string) {
		t.Helper()
		if summary, err := s.UserSummaryRow(t.Context(), project.ID, "forget-me"); err != nil || summary != nil {
			t.Errorf("the user's summary %s = %v, %v; want none", when, summary, err)
		}
		for _, hour := range hours {
			if rows := userRows(t, s, project.ID, "forget-me", hour); len(rows) != 0 {
				t.Errorf("the user has %d rolled rows in hour %d %s, want none", len(rows), hour, when)
			}
		}
	}

	// One chunk, as a request that hung up after it would leave things:
	// one of the first hour's two traces — a chunk of one cuts the hour
	// (spec 047 #1), so the hour is left holding the user's other trace.
	first := erase()
	if first.Counts.Traces != 1 || len(first.Hours) != 1 || !first.More {
		t.Fatalf("first chunk = %d traces over hours %v, more %v; want 1 over one hour with more to come",
			first.Counts.Traces, first.Hours, first.More)
	}
	if got := rolled(hours[0]); got != 2 {
		t.Errorf("hour %d rolled %d traces with one of the user's erased, want 2", hours[0], got)
	}
	if got := rolled(hours[1]); got != 3 {
		t.Errorf("hour %d rolled %d traces before any of its own were erased, want 3", hours[1], got)
	}
	if got := rolled(hours[2]); got != 3 {
		t.Errorf("hour %d rolled %d traces before any of its own were erased, want 3", hours[2], got)
	}
	gone("after the first chunk")

	// The repeat: chunks while there is more, the way the handler loops.
	// Every rolled hour ends at the bystander's one; the live hour keeps
	// the rows it had, because nothing reads them.
	for erase().More {
	}
	for _, hour := range hours[:2] {
		if got := rolled(hour); got != 1 {
			t.Errorf("hour %d rolled %d traces after the erasure, want 1", hour, got)
		}
	}
	if got := rolled(hours[2]); got != 3 {
		t.Errorf("the live hour %d was rolled to %d by the erasure, want it left at 3 for the pass", hours[2], got)
	}
	gone("after the erasure")
}

// TestAnEraseChunkTakesWholeHoursInStartOrder (spec 047 #1, #2): the traces
// arrive round-robin across three hours, and a chunk still takes them hour by
// hour in the order they started — every trace of an hour in one chunk, so
// each hour is rolled once. A chunk is bounded by its traces and, past its
// first hour, by what its rolls recompute; an hour of more traces than the
// chunk holds is cut, and a chunk cut short by either bound says there is
// more.
func TestAnEraseChunkTakesWholeHoursInStartOrder(t *testing.T) {
	hour := func(i int) int64 { return rollupHour + int64(i)*SecondsPerHour }
	seed := func(t *testing.T) (*Store, *Project) {
		t.Helper()
		s, project := readStore(t)
		// Hour 0, 1, 2, 0, 1, 2, 0: three traces in hour 0, two in each
		// of the others, none arriving in the order it started.
		for i := range 7 {
			seedUserTrace(t, s, project.ID, userSeed{n: i + 1, user: "sparse", session: "s",
				environment: "production", model: "claude-sonnet-5", latencyMs: 50,
				hour: hour(i % 3), offsetSeconds: int64(i)})
		}
		// Rolled and behind the watermark, so each hour costs its rows.
		for i := range 3 {
			roll(t, s, project.ID, hour(i))
		}
		advance(t, s, project.ID, hour(2))
		return s, project
	}
	erase := func(t *testing.T, s *Store, project *Project, limit int, budget int64) []*UserDataErase {
		t.Helper()
		writer, err := s.NewWriter(quickWrites)
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Close()
		var chunks []*UserDataErase
		for {
			chunk := &UserDataErase{ProjectID: project.ID, UserID: "sparse", Confirm: "sparse",
				Limit: limit, RollBudget: budget}
			if err := writer.Submit(t.Context(), chunk); err != nil {
				t.Fatal(err)
			}
			chunks = append(chunks, chunk)
			if !chunk.More {
				return chunks
			}
		}
	}
	shape := func(chunks []*UserDataErase) (traces []int64, hours [][]int64, more []bool) {
		for _, chunk := range chunks {
			traces = append(traces, chunk.Counts.Traces)
			hours = append(hours, chunk.Hours)
			more = append(more, chunk.More)
		}
		return traces, hours, more
	}
	for _, tc := range []struct {
		name   string
		limit  int
		budget int64
		traces []int64
		hours  [][]int64
	}{
		{"one chunk holds them all", 100, 0, []int64{7}, [][]int64{{hour(0), hour(1), hour(2)}}},
		{"the traces bound, at an hour", 4, 0, []int64{3, 4},
			[][]int64{{hour(0)}, {hour(1), hour(2)}}},
		{"the roll budget bound, past the first hour", 100, 1, []int64{3, 2, 2},
			[][]int64{{hour(0)}, {hour(1)}, {hour(2)}}},
		{"an hour larger than a chunk is cut", 2, 0, []int64{2, 1, 2, 2},
			[][]int64{{hour(0)}, {hour(0)}, {hour(1)}, {hour(2)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, project := seed(t)
			traces, hours, more := shape(erase(t, s, project, tc.limit, tc.budget))
			if !slices.Equal(traces, tc.traces) || !slices.EqualFunc(hours, tc.hours, slices.Equal) {
				t.Errorf("chunks took %v traces in hours %v, want %v in %v", traces, hours, tc.traces, tc.hours)
			}
			for i, m := range more {
				if m != (i < len(more)-1) {
					t.Errorf("more = %v, want every chunk but the last to say so", more)
					break
				}
			}
		})
	}

	// And a chunk of no size is a mistake, not a size: it would say
	// `More` for ever.
	s, project := seed(t)
	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	err = writer.Submit(t.Context(), &UserDataErase{ProjectID: project.ID, UserID: "sparse",
		Confirm: "sparse", Limit: 0})
	if err == nil || rejected(err) {
		t.Errorf("a zero limit came back %v, want a storage error", err)
	}
}

// TestErasureLeavesAFrozenHourStanding (spec 013 #11): the roll inside the
// chunk obeys the freeze, and it is the erasure's clock that measures it —
// an hour past the retention window keeps its totals, the same hour inside
// it is corrected, and the per-user rows go outright either way (spec 023
// #10).
func TestErasureLeavesAFrozenHourStanding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		now    int64
		rolled int64
	}{
		// Two days past the hour: outside a one-day window, so the hour is
		// frozen — its traces are still here only because the sweep has
		// not run.
		{"frozen", (rollupHour + 2*24*3600) * int64(1e9), 5},
		// An hour later: inside the window, and the correction happens.
		{"inside the window", (rollupHour + 3600) * int64(1e9), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, project := readStore(t)
			usersFixture(t, s, project.ID)
			roll(t, s, project.ID, rollupHour)
			advance(t, s, project.ID, rollupHour)
			if _, err := s.db.Exec(
				`UPDATE projects SET retention_days = 1 WHERE id = ?`, project.ID); err != nil {
				t.Fatal(err)
			}

			writer, err := s.NewWriter(quickWrites)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			erase := &UserDataErase{ProjectID: project.ID, UserID: "alice", Confirm: "alice",
				Limit: 100, Now: tc.now}
			if err := writer.Submit(t.Context(), erase); err != nil {
				t.Fatal(err)
			}
			if erase.Counts.Traces != 3 {
				t.Fatalf("erased %d traces, want alice's 3", erase.Counts.Traces)
			}

			var count int64
			for _, row := range rolledRows(t, s, project.ID, rollupHour) {
				if row.Model == "" {
					count += row.Count
				}
			}
			if count != tc.rolled {
				t.Errorf("the hour rolled %d traces after the erasure, want %d", count, tc.rolled)
			}
			if alice, err := s.UserSummaryRow(t.Context(), project.ID, "alice"); err != nil || alice != nil {
				t.Errorf("alice's summary after erasure = %v, %v; want it gone, frozen hour or not", alice, err)
			}
		})
	}
}
