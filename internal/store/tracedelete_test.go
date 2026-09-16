package store

import (
	"errors"
	"strings"
	"testing"
)

// Deleting traces (spec 035, Testing — store): everything attached to a trace
// goes with it and nothing attached to another does, the hour is corrected in
// the same commit, and the echo is checked inside the transaction like every
// destructive job's (spec 005 #8).

// TestDeletingATraceTakesWhatHangsOffIt: observations, scores, the queue item,
// the payloads and the search entries of the deleted trace go; the neighbour
// keeps all of its own.
func TestDeletingATraceTakesWhatHangsOffIt(t *testing.T) {
	f := newSweepFixture(t)
	f.arriveWithText(t, hexTrace(1), daysAgo(2), "", "the refund failed")
	f.arrive(t, f.project.ID, hexTrace(2), daysAgo(1))
	f.queue(t, "review", "accuracy")
	f.enqueue(t, "review", hexTrace(1))
	f.enqueue(t, "review", hexTrace(2))
	if err := f.writer.Submit(t.Context(), &ScoreWrite{ProjectID: f.project.ID, Scores: []*Score{
		{ID: scoreID(1), TraceID: hexTrace(1), Name: "accuracy", DataType: ScoreNumeric, Value: money(1), Timestamp: 1},
		{ID: scoreID(2), TraceID: hexTrace(2), Name: "accuracy", DataType: ScoreNumeric, Value: money(0), Timestamp: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	// A run holding the trace about to go: deletion overrides the pin (#6)
	// and the preview names the run.
	live := strings.Repeat("a", 32)
	f.createRun(t, "golden", live)
	if _, err := f.store.db.Exec(`UPDATE traces SET run_id = ? WHERE id = ?`, live, hexTrace(1)); err != nil {
		t.Fatal(err)
	}

	counts, runs, err := f.store.TracePreview(f.project.ID, hexTrace(1))
	if err != nil {
		t.Fatal(err)
	}
	if counts.Traces != 1 || counts.Observations != 1 || counts.Scores != 1 || counts.AnnotationItems != 1 {
		t.Errorf("preview = %+v, want one of each attached to the trace", counts)
	}
	if counts.Oldest != daysAgo(2) {
		t.Errorf("oldest = %d, want the trace's own arrival %d", counts.Oldest, daysAgo(2))
	}
	if len(runs) != 1 || runs[0].ID != live || runs[0].Dataset != "golden" || runs[0].Traces != 1 {
		t.Errorf("affected runs = %+v, want the live run named with its one trace", runs)
	}
	// An id the project does not hold previews as nothing, which is the
	// handler's 404.
	if counts, _, err := f.store.TracePreview(f.project.ID, hexTrace(9)); err != nil || counts.Any() {
		t.Errorf("preview of an unknown id = %+v, %v; want nothing", counts, err)
	}

	// The echo is the trace id and nothing else (#1).
	err = f.writer.Submit(t.Context(), &TraceDelete{ProjectID: f.project.ID,
		IDs: []string{hexTrace(1)}, Confirm: hexTrace(2)})
	var rejection *Rejection
	if !errors.As(err, &rejection) || rejection.Kind != RejectInvalid {
		t.Fatalf("a wrong echo came back %v, want RejectInvalid", err)
	}
	err = f.writer.Submit(t.Context(), &TraceDelete{ProjectID: f.project.ID,
		IDs: []string{hexTrace(9)}, Confirm: hexTrace(9)})
	if !errors.As(err, &rejection) || rejection.Kind != RejectNotFound {
		t.Fatalf("an unknown id came back %v, want RejectNotFound", err)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces`); got != 2 {
		t.Fatalf("a refused deletion took a trace: %d left", got)
	}

	del := &TraceDelete{ProjectID: f.project.ID, IDs: []string{hexTrace(1)}, Confirm: hexTrace(1)}
	if err := f.writer.Submit(t.Context(), del); err != nil {
		t.Fatal(err)
	}
	if del.Counts.Traces != 1 || del.Counts.Observations != 1 || del.Counts.Scores != 1 ||
		del.Counts.AnnotationItems != 1 || del.Counts.Payloads != 1 {
		t.Errorf("deleted = %+v, want the trace, its span, its score, its item and its one payload", del.Counts)
	}
	if len(del.Hours) != 1 || del.Hours[0] != HourOf(daysAgo(2)) {
		t.Errorf("hours = %v, want the one the trace started in", del.Hours)
	}
	for table, want := range map[string]int64{
		"traces": 1, "observations": 1, "scores": 1, "annotation_items": 1,
		// The neighbour's three: its metadata, its span's input and output.
		"payloads": 3,
	} {
		if got := f.count(t, `SELECT COUNT(*) FROM `+table); got != want {
			t.Errorf("%s = %d after the deletion, want the neighbour's %d", table, got, want)
		}
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces WHERE id = ?`, hexTrace(2)); got != 1 {
		t.Errorf("the neighbour went with the deleted trace")
	}
	if got := matchingTraces(t, f.store, f.project.ID, "refund"); len(got) != 0 {
		t.Errorf("deleted text is still findable: %v", got)
	}
	assertNoEntries(t, f.store, f.project.ID, hexTrace(1))
	checkIntegrity(t, f.store)
	// The run stands, one trace poorer.
	if pinned, _ := f.store.PinnedTraces(f.project.ID); pinned != 0 {
		t.Errorf("pinned = %d after the deletion, want the run's trace gone", pinned)
	}
}

// TestBulkDeletionSelectsWhatTheListingShows: the filter preview counts
// exactly what the filter matches with what hangs off it, the bulk echo is
// the project's name, and a chunk that finds nothing left answers with zeros
// rather than refusing (edge cases).
func TestBulkDeletionSelectsWhatTheListingShows(t *testing.T) {
	f := newSweepFixture(t)
	for n := 1; n <= 3; n++ {
		f.arrive(t, f.project.ID, hexTrace(n), daysAgo(n), func(tr *modelTrace) { tr.Environment = "staging" })
	}
	f.arrive(t, f.project.ID, hexTrace(4), daysAgo(1), func(tr *modelTrace) { tr.Environment = "production" })

	filter := TraceFilter{Environment: []string{"staging"}}
	counts, runs, err := f.store.TraceDeletePreview(f.project.ID, filter)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Traces != 3 || counts.Observations != 3 || len(runs) != 0 {
		t.Errorf("preview = %+v, %v; want the three staging traces and their spans", counts, runs)
	}
	if counts.Oldest != daysAgo(3) {
		t.Errorf("oldest = %d, want the oldest staging arrival", counts.Oldest)
	}

	rows, err := f.store.Traces(f.project.ID, TraceFilter{Environment: []string{"staging"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	if len(ids) != 3 || ids[0] != hexTrace(1) {
		t.Fatalf("selected %v, want the three staging traces newest first", ids)
	}

	err = f.writer.Submit(t.Context(), &TraceDelete{ProjectID: f.project.ID, IDs: ids,
		Confirm: "not-the-project", ByFilter: true})
	var rejection *Rejection
	if !errors.As(err, &rejection) || rejection.Kind != RejectInvalid {
		t.Fatalf("a wrong project name came back %v, want RejectInvalid", err)
	}
	del := &TraceDelete{ProjectID: f.project.ID, IDs: ids, Confirm: f.project.Name, ByFilter: true}
	if err := f.writer.Submit(t.Context(), del); err != nil {
		t.Fatal(err)
	}
	if del.Counts.Traces != 3 || len(del.Hours) != 3 {
		t.Errorf("deleted = %+v over %v, want three traces of three hours", del.Counts, del.Hours)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces`); got != 1 {
		t.Errorf("traces = %d, want the production one left", got)
	}

	// A repeat over ids another round already took: zeros, not a refusal.
	again := &TraceDelete{ProjectID: f.project.ID, IDs: ids, Confirm: f.project.Name, ByFilter: true}
	if err := f.writer.Submit(t.Context(), again); err != nil {
		t.Fatalf("a repeat came back %v, want nothing deleted and no error", err)
	}
	if again.Counts.Any() || len(again.Hours) != 0 {
		t.Errorf("the repeat reported %+v over %v, want zeros", again.Counts, again.Hours)
	}

	// The chunk bound is the caller's to keep (#3).
	too := make([]string, TraceDeleteChunk+1)
	for i := range too {
		too[i] = hexTrace(100 + i)
	}
	err = f.writer.Submit(t.Context(), &TraceDelete{ProjectID: f.project.ID, IDs: too,
		Confirm: f.project.Name, ByFilter: true})
	if err == nil || rejected(err) {
		t.Errorf("a chunk over the bound came back %v, want a storage error", err)
	}
}

// TestDeletionCorrectsTheHourAsItCommits (spec 013 #7, spec 023 #19 by way
// of #3): the hours the deleted traces occupied are re-rolled in the same
// transaction, a user whose only trace went leaves the per-user tables, a
// user with others keeps a corrected summary, and the live hour at the
// watermark is left for the pass.
func TestDeletionCorrectsTheHourAsItCommits(t *testing.T) {
	s, project := readStore(t)
	hours := []int64{rollupHour, rollupHour + SecondsPerHour}
	// Hour 0: two of alice's traces and one of bob's. Hour 1, the live one
	// at the watermark: one of alice's.
	seedUserTrace(t, s, project.ID, userSeed{n: 1, user: "alice", session: "s-a",
		environment: "production", model: "claude-sonnet-5", cost: money(0.01), latencyMs: 250,
		hour: hours[0], offsetSeconds: 10})
	seedUserTrace(t, s, project.ID, userSeed{n: 2, user: "alice", session: "s-a",
		environment: "production", model: "claude-sonnet-5", cost: money(0.02), latencyMs: 900,
		hour: hours[0], offsetSeconds: 20})
	seedUserTrace(t, s, project.ID, userSeed{n: 3, user: "bob", session: "s-b",
		environment: "production", model: "claude-sonnet-5", cost: money(0.005), latencyMs: 40,
		hour: hours[0], offsetSeconds: 30})
	seedUserTrace(t, s, project.ID, userSeed{n: 4, user: "alice", session: "s-c",
		environment: "production", model: "claude-sonnet-5", latencyMs: 60,
		hour: hours[1], offsetSeconds: 40})
	for _, hour := range hours {
		roll(t, s, project.ID, hour)
	}
	advance(t, s, project.ID, hours[0])
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
	if got := rolled(hours[0]); got != 3 {
		t.Fatalf("hour 0 rolled %d traces before the deletion, want 3", got)
	}

	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	now := (rollupHour + 2*SecondsPerHour) * int64(1e9)
	// Bob's only trace and one of alice's, both of hour 0; and alice's
	// trace of the live hour.
	del := &TraceDelete{ProjectID: project.ID, IDs: []string{hexTrace(3), hexTrace(1), hexTrace(4)},
		Confirm: project.Name, ByFilter: true, Now: now}
	if err := writer.Submit(t.Context(), del); err != nil {
		t.Fatal(err)
	}
	if del.Counts.Traces != 3 || len(del.Hours) != 2 {
		t.Fatalf("deleted = %+v over %v, want three traces of two hours", del.Counts, del.Hours)
	}
	if got := rolled(hours[0]); got != 1 {
		t.Errorf("hour 0 rolled %d traces after the deletion, want alice's remaining 1", got)
	}
	if got := rolled(hours[1]); got != 1 {
		t.Errorf("the live hour was rolled to %d by the deletion, want it left at 1 for the pass", got)
	}
	if bob, err := s.UserSummaryRow(project.ID, "bob"); err != nil || bob != nil {
		t.Errorf("bob's summary = %v, %v; want him gone with his only trace", bob, err)
	}
	if rows := userRows(t, s, project.ID, "bob", hours[0]); len(rows) != 0 {
		t.Errorf("bob still has %d rolled rows, want none", len(rows))
	}
	alice, err := s.UserSummaryRow(project.ID, "alice")
	if err != nil || alice == nil {
		t.Fatalf("alice's summary = %v, %v; want her kept with her other trace", alice, err)
	}
	// Her summary is the rolled hours' sum: hour 0's one remaining trace,
	// and the live hour's row as the pass left it.
	if alice.Traces != 2 {
		t.Errorf("alice's summary counts %d traces, want 2 (one corrected hour, one live)", alice.Traces)
	}
}

// TestDeletionLeavesAFrozenHourStanding (spec 013 #11): an hour past the
// retention window keeps its totals — the roll obeys the freeze under the
// deletion's own clock — and the same hour inside the window is corrected.
func TestDeletionLeavesAFrozenHourStanding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		now    int64
		rolled int64
	}{
		{"frozen", (rollupHour + 2*24*3600) * int64(1e9), 5},
		{"inside the window", (rollupHour + 3600) * int64(1e9), 4},
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
			del := &TraceDelete{ProjectID: project.ID, IDs: []string{hexTrace(4)},
				Confirm: hexTrace(4), Now: tc.now}
			if err := writer.Submit(t.Context(), del); err != nil {
				t.Fatal(err)
			}
			var count int64
			for _, row := range rolledRows(t, s, project.ID, rollupHour) {
				if row.Model == "" {
					count += row.Count
				}
			}
			if count != tc.rolled {
				t.Errorf("the hour rolled %d traces after the deletion, want %d", count, tc.rolled)
			}
		})
	}
}

// The single form takes one id and the echo is that id; a trace of another
// project is not this project's to delete.
func TestTraceDeletionStaysInItsProject(t *testing.T) {
	f := newSweepFixture(t)
	other, err := f.store.CreateProject("other", KeyPair{PublicKey: "tp-pk-other", Secret: "tp-sk-other"})
	if err != nil {
		t.Fatal(err)
	}
	f.arrive(t, other.ID, hexTrace(1), daysAgo(1))
	f.arrive(t, f.project.ID, hexTrace(2), daysAgo(1))

	err = f.writer.Submit(t.Context(), &TraceDelete{ProjectID: f.project.ID,
		IDs: []string{hexTrace(1)}, Confirm: hexTrace(1)})
	var rejection *Rejection
	if !errors.As(err, &rejection) || rejection.Kind != RejectNotFound {
		t.Fatalf("another project's trace came back %v, want RejectNotFound", err)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces`); got != 2 {
		t.Errorf("traces = %d, want both untouched", got)
	}
}

// TestPayloadDeleteSeeksTheReferenceIndexes (spec 035 #13): deleting a payload
// row is a foreign-key check in every column that references it, and without
// an index each check was a scan of `observations` — six milliseconds per
// payload on a real database, and the reason a deletion of a few hundred
// traces outran the interface's clock. Migration 0020's partial indexes turn
// the four checks into seeks, and this is the plan test that keeps them so.
func TestPayloadDeleteSeeksTheReferenceIndexes(t *testing.T) {
	f := newSweepFixture(t)
	plan, err := f.store.explainQueryPlan(`DELETE FROM payloads WHERE id IN (?, ?)`, int64(1), int64(2))
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	for _, index := range []string{
		"idx_observations_input_payload",
		"idx_observations_output_payload",
		"idx_observations_metadata_payload",
		"idx_traces_metadata_payload",
	} {
		if !strings.Contains(joined, index) {
			t.Errorf("the foreign-key check does not seek %s:\n%s", index, joined)
		}
	}
	if strings.Contains(joined, "SCAN") {
		t.Errorf("a foreign-key check scans rather than seeks:\n%s", joined)
	}
}
