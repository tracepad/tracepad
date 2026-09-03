package store

import (
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
)

// The pin (spec 014 #13, #14; Testing — sweep): a trace of a live run is not
// swept, deleting the run releases it, a trace naming no run is mortal, the
// dry run agrees with the sweep, erasure outranks the pin, and a purged
// project leaves no eval rows behind.

// modelTrace is the sweep fixture's trace type, named here so the option
// closures read as what they change.
type modelTrace = model.Trace

func (f *sweepFixture) createRun(t *testing.T, dataset, id string) {
	t.Helper()
	f.postItems(t, dataset, itemInput(itemID(1), `1`))
	if err := f.writer.Submit(t.Context(), &RunCreate{
		ProjectID: f.project.ID, Dataset: dataset, ID: id, Now: sweepNow.UnixNano()}); err != nil {
		t.Fatal(err)
	}
}

// TestSweepSparesLiveRunTraces: an expired trace of a live run survives the
// pass; one whose run_id names no run is swept with the rest; deleting the
// run releases the survivor and the next pass takes it.
func TestSweepSparesLiveRunTraces(t *testing.T) {
	f := newSweepFixture(t)
	live := strings.Repeat("a", 32)
	f.createRun(t, "golden", live)
	f.setRetention(t, f.project.ID, days(30), nil)

	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(40), func(tr *modelTrace) { tr.RunID = live })
	f.arrive(t, f.project.ID, hexTrace(2), daysAgo(40), func(tr *modelTrace) { tr.RunID = strings.Repeat("f", 32) })
	f.arrive(t, f.project.ID, hexTrace(3), daysAgo(40))

	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces WHERE id = ?`, hexTrace(1)); got != 1 {
		t.Errorf("the live run's trace was swept")
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces WHERE id = ?`, hexTrace(2)); got != 0 {
		t.Errorf("a trace naming no run survived: it is an orphan and mortal (spec 014 #3)")
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces WHERE id = ?`, hexTrace(3)); got != 0 {
		t.Errorf("an ordinary expired trace survived")
	}
	if pinned, _ := f.store.PinnedTraces(f.project.ID); pinned != 1 {
		t.Errorf("pinned = %d, want the one trace the run holds", pinned)
	}

	deletion := &RunDelete{ProjectID: f.project.ID, ID: live}
	if err := f.writer.Submit(t.Context(), deletion); err != nil {
		t.Fatal(err)
	}
	if deletion.Released != 1 {
		t.Errorf("released = %d, want 1", deletion.Released)
	}
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces`); got != 0 {
		t.Errorf("traces = %d after the run's delete and a pass, want the released trace gone", got)
	}
}

// TestSweepPickStillSeeks: the pin check is a NOT EXISTS beside the window,
// and the plan must still seek idx_traces_ingested rather than scan the
// project (data contract; the method of TestSweepWindowSeeks).
func TestSweepPickStillSeeks(t *testing.T) {
	f := newSweepFixture(t)
	plan, err := f.store.explainQueryPlan(expiredTracesQuery, f.project.ID, daysAgo(30), 1000)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "idx_traces_ingested") {
		t.Errorf("the sweep pick does not use its index:\n%s", joined)
	}
	if !strings.Contains(plan[0], "SEARCH") {
		t.Errorf("the sweep pick scans rather than seeks:\n%s", joined)
	}
	if strings.Contains(joined, "TEMP B-TREE") {
		t.Errorf("the sweep pick sorts through a temporary B-tree:\n%s", joined)
	}
}

// TestRetentionPreviewExcludesPinnedTraces: the dry run counts what the sweep
// would take, so it leaves the pinned trace out (spec 014 #13).
func TestRetentionPreviewExcludesPinnedTraces(t *testing.T) {
	f := newSweepFixture(t)
	live := strings.Repeat("a", 32)
	f.createRun(t, "golden", live)
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(40), func(tr *modelTrace) { tr.RunID = live })
	f.arrive(t, f.project.ID, hexTrace(2), daysAgo(40))

	counts, err := f.store.RetentionPreview(f.project.ID, days(30), nil, nil, sweepNow.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if counts.Traces != 1 || counts.Observations != 1 {
		t.Errorf("preview = %+v, want the one unpinned trace and its observation", counts)
	}
	if counts.Oldest != daysAgo(40) {
		t.Errorf("oldest = %d, want the unpinned trace's arrival", counts.Oldest)
	}
}

// TestUserErasureOverridesThePin is Decision 14: the user's traces go whether
// or not a run holds them, and the preview names the run that loses them.
func TestUserErasureOverridesThePin(t *testing.T) {
	f := newSweepFixture(t)
	live := strings.Repeat("a", 32)
	f.createRun(t, "golden", live)
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1), func(tr *modelTrace) { tr.RunID = live; tr.ItemID = itemID(1) })
	f.arrive(t, f.project.ID, hexTrace(2), daysAgo(1), func(tr *modelTrace) { tr.RunID = live; tr.UserID = "someone-else" })

	counts, runs, err := f.store.UserDataPreview(f.project.ID, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if counts.Traces != 1 {
		t.Errorf("preview counts = %+v, want the one trace filed under u1", counts)
	}
	if len(runs) != 1 || runs[0].ID != live || runs[0].Dataset != "golden" || runs[0].Traces != 1 {
		t.Errorf("affected runs = %+v, want the live run named with its one trace", runs)
	}

	erase := &UserDataErase{ProjectID: f.project.ID, UserID: "u1", Confirm: "u1", Limit: 100}
	if err := f.writer.Submit(t.Context(), erase); err != nil {
		t.Fatal(err)
	}
	if erase.Counts.Traces != 1 {
		t.Errorf("erased = %+v, want the pinned trace taken", erase.Counts)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces WHERE id = ?`, hexTrace(1)); got != 0 {
		t.Errorf("the pin outranked the erasure")
	}
	// The run itself stands; it has one trace fewer.
	if pinned, _ := f.store.PinnedTraces(f.project.ID); pinned != 1 {
		t.Errorf("pinned = %d after the erasure, want the other user's trace still held", pinned)
	}
}

// TestProjectPurgeLeavesNoDatasetRows: the eval tables hang off the project
// through the cascades of schema 0010, so a purged project takes them along.
func TestProjectPurgeLeavesNoDatasetRows(t *testing.T) {
	f := newSweepFixture(t)
	f.createRun(t, "golden", strings.Repeat("a", 32))
	if err := f.writer.Submit(t.Context(), &ScoreConfigPut{ProjectID: f.project.ID, Now: 1,
		Config: &ScoreConfig{Name: "accuracy", DataType: ScoreNumeric, Direction: DirectionHigher}}); err != nil {
		t.Fatal(err)
	}
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1), func(tr *modelTrace) { tr.RunID = strings.Repeat("a", 32) })

	if _, err := f.store.db.Exec(`UPDATE projects SET deleted_at = ? WHERE id = ?`, daysAgo(8), f.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"projects", "traces", "datasets", "dataset_items", "dataset_runs", "score_configs"} {
		if got := f.count(t, `SELECT COUNT(*) FROM `+table); got != 0 {
			t.Errorf("%s = %d after the purge, want nothing left", table, got)
		}
	}
	// `payloads` has no project column and no cascade, so the pass has to
	// have taken them itself. This is the end state and holds either way —
	// the orphan collector at the tail of the same pass would find them if
	// the chunks did not, a few thousand at a time. What the chunk must do
	// is the test below.
	if got := f.count(t, `SELECT COUNT(*) FROM payloads`); got != 0 {
		t.Errorf("payloads = %d after the purge, want the pinned trace's taken with it", got)
	}
}

// TestPurgeTakesPinnedTracesInItsChunks: the chunk itself must contain the
// pinned trace. A purge that honoured the pin would report drained on a chunk
// that came back short only because everything left was pinned, and
// `sweepProject` would then drop the `projects` row with those traces still
// there — they go by cascade, but their payloads have no cascade to go by and
// are left for the orphan pass, which is exactly what the drain exists to
// prevent (spec 005 #4, found in review of PR #30).
func TestPurgeTakesPinnedTracesInItsChunks(t *testing.T) {
	f := newSweepFixture(t)
	live := strings.Repeat("a", 32)
	f.createRun(t, "golden", live)
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1), func(tr *modelTrace) { tr.RunID = live })

	// Deleted past its grace window: everything goes, pin or no pin.
	if _, err := f.store.db.Exec(`UPDATE projects SET deleted_at = ? WHERE id = ?`, daysAgo(8), f.project.ID); err != nil {
		t.Fatal(err)
	}
	chunk := &traceSweep{ProjectID: f.project.ID, Now: sweepNow.UnixNano(), Purge: true, Limit: 1000}
	if err := f.writer.Submit(t.Context(), chunk); err != nil {
		t.Fatal(err)
	}
	if chunk.Traces != 1 {
		t.Errorf("the purge chunk took %d traces, want the pinned one", chunk.Traces)
	}
	if chunk.Payloads == 0 {
		t.Errorf("the purge chunk took no payloads, so the trace's would have been stranded")
	}

	// And an ordinary sweep of a live project still spares it.
	f2 := newSweepFixture(t)
	f2.createRun(t, "golden", live)
	f2.setRetention(t, f2.project.ID, days(30), nil)
	f2.arrive(t, f2.project.ID, hexTrace(1), daysAgo(40), func(tr *modelTrace) { tr.RunID = live })
	ordinary := &traceSweep{ProjectID: f2.project.ID, Now: sweepNow.UnixNano(), Limit: 1000}
	if err := f2.writer.Submit(t.Context(), ordinary); err != nil {
		t.Fatal(err)
	}
	if ordinary.Traces != 0 {
		t.Errorf("an ordinary chunk took %d traces, want the pin to have spared it", ordinary.Traces)
	}
}

// TestIngestRecordsTheLinkAndTheOrphan is Testing — ingest, at the store: a
// trace naming an unknown run is stored with its columns and reported; a
// known run links; a second trace of the same item links beside the first;
// and a trace re-delivered with a different run moves (edge cases).
func TestIngestRecordsTheLinkAndTheOrphan(t *testing.T) {
	f := newSweepFixture(t)
	live := strings.Repeat("a", 32)
	unknown := strings.Repeat("f", 32)
	f.createRun(t, "golden", live)

	link := func(traceID, runID, itemID string) *IngestBatch {
		return &IngestBatch{
			ProjectID:  f.project.ID,
			IngestedAt: sweepNow.UnixNano(),
			Traces:     []*model.Trace{{ID: traceID, RunID: runID, ItemID: itemID}},
			Observations: []*model.Observation{{TraceID: traceID, ID: traceID[:16], Type: model.TypeSpan,
				Level: model.LevelDefault, StartTime: 1, EndTime: 2}},
		}
	}
	orphan := link(hexTrace(1), unknown, itemID(1))
	if err := f.writer.Submit(t.Context(), orphan); err != nil {
		t.Fatal(err)
	}
	if len(orphan.UnknownRuns) != 1 || orphan.UnknownRuns[0] != unknown {
		t.Errorf("unknown runs = %v, want the one id nobody created", orphan.UnknownRuns)
	}
	row, err := f.store.Trace(f.project.ID, hexTrace(1))
	if err != nil {
		t.Fatal(err)
	}
	if row.RunID != unknown || row.ItemID != itemID(1) {
		t.Errorf("orphan trace = run %q item %q, want the columns written as the client said", row.RunID, row.ItemID)
	}

	for _, traceID := range []string{hexTrace(2), hexTrace(3)} {
		batch := link(traceID, live, itemID(1))
		if err := f.writer.Submit(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
		if len(batch.UnknownRuns) != 0 {
			t.Errorf("a known run was reported unknown: %v", batch.UnknownRuns)
		}
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces WHERE run_id = ? AND item_id = ?`, live, itemID(1)); got != 2 {
		t.Errorf("traces of the item = %d, want both attempts kept (spec 014 #2)", got)
	}

	// Re-delivered naming the live run: last delivery wins, the trace
	// moves (spec 002 #6).
	if err := f.writer.Submit(t.Context(), link(hexTrace(1), live, itemID(1))); err != nil {
		t.Fatal(err)
	}
	row, _ = f.store.Trace(f.project.ID, hexTrace(1))
	if row.RunID != live {
		t.Errorf("run_id = %q after re-delivery, want the trace moved to %q", row.RunID, live)
	}
	// And a delivery that says nothing about the run leaves it alone.
	if err := f.writer.Submit(t.Context(), link(hexTrace(1), "", "")); err != nil {
		t.Fatal(err)
	}
	row, _ = f.store.Trace(f.project.ID, hexTrace(1))
	if row.RunID != live || row.ItemID != itemID(1) {
		t.Errorf("a silent delivery cleared the link: run %q item %q", row.RunID, row.ItemID)
	}
}
