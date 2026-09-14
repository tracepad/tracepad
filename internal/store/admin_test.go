package store

import (
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
	if project, _ := f.store.ProjectByID(f.project.ID); project.RetentionDays != nil {
		t.Errorf("retention = %v after a refused update, want it untouched", project.RetentionDays)
	}

	update.Confirm = "test"
	if err := f.writer.Submit(t.Context(), update); err != nil {
		t.Fatalf("the confirmed update failed: %v", err)
	}
	if project, _ := f.store.ProjectByID(f.project.ID); *project.RetentionDays != 30 {
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

	project, err := f.store.ProjectByID(f.project.ID)
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
	project, _ := f.store.ProjectByID(f.project.ID)
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
	project, _ := f.store.ProjectByID(f.project.ID)
	if *project.DeletedAt != sweepNow.UnixNano() {
		t.Errorf("deleted_at = %d, want the first deletion's %d", *project.DeletedAt, sweepNow.UnixNano())
	}
}

// TestErasureCorrectsTheHoursOfEachChunkAsItCommits (spec 023 #19): the
// re-roll of spec 013 #7 rides inside the chunk's own transaction, so a
// request cut off between chunks leaves no hour counting traces that are
// gone — and a repeat, which only sees the traces that remain, has nothing
// to miss. The chunk here straddles an hour, and the user is not written
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
			Confirm: "forget-me", Limit: 3, Now: (rollupHour + 3*SecondsPerHour) * 1e9}
		if err := writer.Submit(t.Context(), chunk); err != nil {
			t.Fatal(err)
		}
		return chunk
	}
	gone := func(when string) {
		t.Helper()
		if summary, err := s.UserSummaryRow(project.ID, "forget-me"); err != nil || summary != nil {
			t.Errorf("the user's summary %s = %v, %v; want none", when, summary, err)
		}
		for _, hour := range hours {
			if rows := userRows(t, s, project.ID, "forget-me", hour); len(rows) != 0 {
				t.Errorf("the user has %d rolled rows in hour %d %s, want none", len(rows), hour, when)
			}
		}
	}

	// One chunk, as a request that hung up after it would leave things:
	// the first hour's two traces and one of the second's.
	first := erase()
	if first.Counts.Traces != 3 || len(first.Hours) != 2 || !first.More {
		t.Fatalf("first chunk = %d traces over hours %v, more %v; want 3 over two hours with more to come",
			first.Counts.Traces, first.Hours, first.More)
	}
	if got := rolled(hours[0]); got != 1 {
		t.Errorf("hour %d rolled %d traces after its chunk committed, want the bystander's 1", hours[0], got)
	}
	if got := rolled(hours[1]); got != 2 {
		t.Errorf("hour %d rolled %d traces with one of the user's erased, want 2", hours[1], got)
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

// TestAnEraseChunkIsBoundedInHours (spec 023 #19): the rolls are what a
// chunk now costs, so a chunk is capped in distinct hours as well as in
// traces, and a chunk cut short by the cap says there is more. The traces
// arrive interleaved across hours, and the cap skips the hours beyond it
// rather than stopping at the first of them (second review of PR #65): a
// chunk holds everything the window has for its hours, so each hour is
// rolled by one chunk and not by every chunk that reaches into it.
func TestAnEraseChunkIsBoundedInHours(t *testing.T) {
	s, project := readStore(t)
	// Three hours, arriving round-robin: hour 0, 1, 2, 0, 1, 2, 0.
	for i := range 7 {
		seedUserTrace(t, s, project.ID, userSeed{n: i + 1, user: "sparse", session: "s",
			environment: "production", model: "claude-sonnet-5", latencyMs: 50,
			hour: rollupHour + int64(i%3)*SecondsPerHour, offsetSeconds: int64(i)})
	}

	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	var chunks []*UserDataErase
	for {
		chunk := &UserDataErase{ProjectID: project.ID, UserID: "sparse", Confirm: "sparse",
			Limit: 100, HourLimit: 2}
		if err := writer.Submit(t.Context(), chunk); err != nil {
			t.Fatal(err)
		}
		chunks = append(chunks, chunk)
		if !chunk.More {
			break
		}
	}
	var got []int64
	for _, chunk := range chunks {
		got = append(got, chunk.Counts.Traces)
	}
	// The first chunk: every trace of hours 0 and 1 (five of them), the
	// third hour's two skipped; the second: those two, and nothing more.
	if want := []int64{5, 2}; !slices.Equal(got, want) {
		t.Fatalf("chunks took %v traces, want %v: two hours at a time, whole", got, want)
	}
	if !chunks[0].More || chunks[1].More {
		t.Errorf("more = %v %v, want the capped chunk to say so and the last not to",
			chunks[0].More, chunks[1].More)
	}
	if len(chunks[0].Hours) != 2 || len(chunks[1].Hours) != 1 {
		t.Errorf("hours = %v and %v, want two then one", chunks[0].Hours, chunks[1].Hours)
	}

	// And a chunk of no size is a mistake, not a size: it would say
	// `More` for ever.
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
			if alice, err := s.UserSummaryRow(project.ID, "alice"); err != nil || alice != nil {
				t.Errorf("alice's summary after erasure = %v, %v; want it gone, frozen hour or not", alice, err)
			}
		})
	}
}
