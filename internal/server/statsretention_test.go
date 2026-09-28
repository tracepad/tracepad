package server

import (
	"context"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// The rollup's own window and the deletion paths around it (spec 013 #6, #7).
// Each of the three ways data leaves this store meets the rollup differently,
// and the difference is the whole point of the spec.

type projectView struct {
	RetentionDays      *int `json:"retention_days"`
	RawRetentionDays   *int `json:"raw_retention_days"`
	StatsRetentionDays *int `json:"stats_retention_days"`
}

// The field rides the paths spec 005 built for its sibling (#9): it is in the
// project response, it is settable, and null means forever.
func TestStatsRetentionTravelsWithTheOtherWindows(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.get(t, "/api/v1/projects/"+h.project.ID)
	expectStatus(t, rec, 200)
	if got := decodeJSON[projectView](t, rec).StatsRetentionDays; got != nil {
		t.Errorf("stats_retention_days = %d on a new project, want null — forever is the default", *got)
	}

	// Setting it from forever to a window is a shrink, so it previews
	// first and applies on the echo (spec 005 #8).
	rec = h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID,
		map[string]any{"stats_retention_days": 90})
	expectStatus(t, rec, 200)
	if dry := decodeJSON[struct {
		DryRun  bool   `json:"dry_run"`
		Confirm string `json:"confirm"`
	}](t, rec); !dry.DryRun {
		t.Fatal("shortening the statistics window applied without a preview")
	}

	rec = h.send(t, "PATCH",
		"/api/v1/projects/"+h.project.ID+"?confirm="+h.project.Name,
		map[string]any{"stats_retention_days": 90})
	expectStatus(t, rec, 200)
	if got := decodeJSON[projectView](t, rec).StatsRetentionDays; got == nil || *got != 90 {
		t.Fatalf("stats_retention_days = %v, want 90", got)
	}

	// And back to forever, which grows the window and needs no echo.
	rec = h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID,
		map[string]any{"stats_retention_days": nil})
	expectStatus(t, rec, 200)
	if got := decodeJSON[projectView](t, rec).StatsRetentionDays; got != nil {
		t.Errorf("stats_retention_days = %d, want null again", *got)
	}
}

// The preview names the rolled hours, because they are the one thing in it
// that the trace sweep would have spared.
func TestTheStatsWindowPreviewNamesTheRolledHours(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedHour(t, statsHour, 1, 3, "production")
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	rec := h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID,
		map[string]any{"stats_retention_days": 1})
	expectStatus(t, rec, 200)
	dry := decodeJSON[struct {
		DryRun      bool `json:"dry_run"`
		WouldDelete struct {
			StatsHours int64 `json:"stats_hours"`
		} `json:"would_delete"`
	}](t, rec)
	if !dry.DryRun {
		t.Fatal("the shrink applied without a preview")
	}
	if dry.WouldDelete.StatsHours != 1 {
		t.Errorf("would_delete.stats_hours = %d, want the one rolled hour",
			dry.WouldDelete.StatsHours)
	}
}

// A stats window shorter than the trace window must not hide data the store
// still holds (spec 013 #13): those hours go back to the live scan, which is
// what answered them before this spec existed.
func TestAShortStatsWindowFallsBackToTheLiveScan(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedHour(t, statsHour, 1, 3, "production")
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	truth := h.statsBuckets(t, "/api/v1/stats?group_by=day")
	if len(truth) != 1 || truth[0].Count != 3 {
		t.Fatalf("buckets = %+v, want the three seeded traces", truth)
	}

	// One day of statistics, traces kept forever — and a fixture hour that
	// is far older than a day, so the aggregator sweeps its rolled rows.
	rec := h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID+"?confirm="+h.project.Name,
		map[string]any{"stats_retention_days": 1})
	expectStatus(t, rec, 200)
	h.rollTheCorpus(t, time.Now())

	if hours, err := h.store.StatsRollupHours(t.Context(), h.project.ID); err != nil {
		t.Fatal(err)
	} else if len(hours) != 0 {
		t.Fatalf("rolled hours = %v, want the window to have swept them", hours)
	}

	after := h.statsBuckets(t, "/api/v1/stats?group_by=day")
	if len(after) != 1 || after[0].Count != truth[0].Count {
		t.Errorf("buckets = %+v, want the live truth %+v: deleting the summary of an "+
			"hour must not hide the rows behind it", after, truth)
	}
}

// The hour the stats cutoff falls inside is swept — the aggregator deletes
// `hour < cutoff` and the cutoff is not hour-aligned — so the read must not
// ask the rollup about it. Rounding the floor down left exactly that hour
// answered by neither half (found in review of PR #28, spec 013 #13).
func TestTheHourOnTheStatsWindowEdgeIsStillAnswered(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	// The hour a one-day window cuts through, on the real clock, so the
	// sweep and the read see the same edge.
	edge := store.HourOf(time.Now().Add(-24 * time.Hour).UnixNano())
	h.seedHour(t, edge, 1, 3, "production")
	h.rollTheCorpus(t, time.Now())

	truth := h.statsBuckets(t, "/api/v1/stats?group_by=hour")
	if len(truth) != 1 || truth[0].Count != 3 {
		t.Fatalf("buckets = %+v, want the three seeded traces", truth)
	}

	rec := h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID+"?confirm="+h.project.Name,
		map[string]any{"stats_retention_days": 1})
	expectStatus(t, rec, 200)
	h.rollTheCorpus(t, time.Now())

	after := h.statsBuckets(t, "/api/v1/stats?group_by=hour")
	if len(after) != 1 || after[0].Count != truth[0].Count {
		t.Errorf("buckets = %+v, want the live truth %+v: the hour the window cuts "+
			"through is swept from the rollup and must fall to the live scan",
			after, truth)
	}
}

// Lengthening the window again must not leave a hole. The sweep cannot be
// undone, so a floor derived from the *current* window would send the seam
// back to a table whose rows are gone — permanently, since nothing re-rolls
// an hour nobody has touched (spec 013 #17, found in review of PR #28).
func TestWideningTheStatsWindowDoesNotLeaveAHole(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedHour(t, statsHour, 1, 3, "production")
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))
	truth := h.statsBuckets(t, "/api/v1/stats?group_by=day")
	if len(truth) != 1 || truth[0].Count != 3 {
		t.Fatalf("buckets = %+v, want the three seeded traces", truth)
	}

	// One day of statistics: the fixture's hour is swept from the rollup.
	rec := h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID+"?confirm="+h.project.Name,
		map[string]any{"stats_retention_days": 1})
	expectStatus(t, rec, 200)
	h.rollTheCorpus(t, time.Now())
	if hours, err := h.store.StatsRollupHours(t.Context(), h.project.ID); err != nil {
		t.Fatal(err)
	} else if len(hours) != 0 {
		t.Fatalf("rolled hours = %v, want the window to have swept them", hours)
	}

	// And back to keeping them forever, which deletes nothing and looks
	// harmless — the traces are all still here.
	rec = h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID,
		map[string]any{"stats_retention_days": nil})
	expectStatus(t, rec, 200)

	after := h.statsBuckets(t, "/api/v1/stats?group_by=day")
	if len(after) != 1 || after[0].Count != truth[0].Count {
		t.Errorf("buckets = %+v, want the live truth %+v: the rollup can only speak "+
			"for the hours it still holds", after, truth)
	}
}

// And when both windows have passed, the emptiness is real: nothing is
// stored, nothing is recomputed, and the honest answer is no buckets.
func TestWhenBothWindowsHavePassedTheAnswerIsEmpty(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedHour(t, statsHour, 1, 3, "production")
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	rec := h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID+"?confirm="+h.project.Name,
		map[string]any{"stats_retention_days": 1, "retention_days": 1})
	expectStatus(t, rec, 200)

	// The traces go through retention's own path, the rolled rows through
	// the aggregator's. Retention counts from arrival and these arrived a
	// moment ago (spec 005 #1), so the sweep runs on a clock two days on.
	sweeper := h.store.NewSweeper(h.writer, store.SweepOptions{
		Now: func() time.Time { return time.Now().Add(2 * 24 * time.Hour) },
	})
	if err := sweeper.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	h.rollTheCorpus(t, time.Now())

	if buckets := h.statsBuckets(t, "/api/v1/stats?group_by=day"); len(buckets) != 0 {
		t.Errorf("buckets = %+v, want none: both windows have passed", buckets)
	}
}

// Erasing a user's data corrects the hours their traces occupied, before the
// request answers (spec 013 #7).
func TestErasingAUserCorrectsTheRolledHours(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	for i := range 4 {
		start := statsHour*int64(time.Second) + int64(i)*int64(time.Second)
		user := "keep"
		if i < 2 {
			user = "forget-me"
		}
		trace := &model.Trace{ID: traceHex(i + 1), Environment: "production", UserID: user}
		h.seed(t, trace, &model.Observation{
			TraceID: trace.ID, ID: spanHex(i + 1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: start, EndTime: start + 50*ms})
	}
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	if before := h.statsBuckets(t, "/api/v1/stats?group_by=hour"); before[0].Count != 4 {
		t.Fatalf("the hour rolled %d traces, want 4", before[0].Count)
	}

	rec := h.send(t, "DELETE",
		"/api/v1/projects/"+h.project.ID+"/users/forget-me/data?confirm=forget-me&wait=30", nil)
	expectStatus(t, rec, 200)

	// The rollup is corrected in the same request that promised the data is
	// gone — a per-hour count is data derived from what was erased.
	after := h.statsBuckets(t, "/api/v1/stats?group_by=hour")
	if len(after) != 1 || after[0].Count != 2 {
		t.Errorf("buckets = %+v, want the two traces that remain", after)
	}
}

// stopAfterTheFirstChunk is a stop of the server as the erasure worker sees
// it: the first erase chunk commits — the writer never abandons a job whose
// caller has — and the worker is told the writer is closing, so the erasure
// stays running for the next start (spec 047 #17). stopped says so.
type stopAfterTheFirstChunk struct {
	inner   JobWriter
	chunks  int
	stopped chan struct{}
}

func (w *stopAfterTheFirstChunk) Submit(ctx context.Context, job store.WriteJob) error {
	if _, ok := job.(*store.UserDataErase); !ok {
		return w.inner.Submit(ctx, job)
	}
	w.chunks++
	if w.chunks > 1 {
		return store.ErrWriterClosed
	}
	if err := w.inner.Submit(context.Background(), job); err != nil {
		return err
	}
	close(w.stopped)
	return store.ErrWriterClosed
}

// A stop between chunks leaves every hour the committed chunks emptied already
// corrected, and the erasure resumed on the next start finishes the rest with
// no hour left counting traces that are gone (spec 023 #19, spec 047 #12;
// found in review of PR #61, when a client hanging up was the way an erasure
// was cut off).
func TestAnErasureCutOffBetweenChunksLeavesNoHourDirty(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	// More traces than one chunk takes, over two hours, a bystander in
	// each. The chunks follow the user index, which is start order (spec
	// 047 #1), so the first chunk is exactly the first hour's five
	// hundred.
	first, second := statsHour, statsHour+3600
	batch := &store.IngestBatch{ProjectID: h.project.ID}
	add := func(n int, hour int64, user string) {
		start := hour*int64(time.Second) + int64(n)*int64(time.Millisecond)
		batch.Traces = append(batch.Traces,
			&model.Trace{ID: traceHex(n), Environment: "production", UserID: user})
		batch.Observations = append(batch.Observations, &model.Observation{
			TraceID: traceHex(n), ID: spanHex(n), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: start, EndTime: start + 50*ms})
	}
	n := 0
	for range store.DefaultEraseChunk {
		n++
		add(n, first, "forget-me")
	}
	n++
	add(n, first, "keep")
	for range 50 {
		n++
		add(n, second, "forget-me")
	}
	n++
	add(n, second, "keep")
	if err := h.writer.Submit(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	h.rollTheCorpus(t, time.Unix(second+3*3600, 0))

	before := h.statsBuckets(t, "/api/v1/stats?group_by=hour")
	if len(before) != 2 || before[0].Count != store.DefaultEraseChunk+1 || before[1].Count != 51 {
		t.Fatalf("buckets = %+v, want %d and 51", before, store.DefaultEraseChunk+1)
	}

	// The same store, with a worker the stop comes to after one chunk.
	h.eraser.Close()
	writer := &stopAfterTheFirstChunk{inner: h.writer, stopped: make(chan struct{})}
	stopping := h.store.NewEraser(writer, store.EraserOptions{})
	stopping.Start()
	path := "/api/v1/projects/" + h.project.ID + "/users/forget-me/data?confirm=forget-me"
	rec := h.call(t, "DELETE", path, nil)
	expectStatus(t, rec, 202)
	select {
	case <-writer.stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("the erasure's first chunk never came")
	}
	stopping.Close()
	if writer.chunks != 1 {
		t.Fatalf("the worker submitted %d chunks after the stop, want 1", writer.chunks)
	}

	// The first hour's traces are gone, and so is their count: the chunk
	// that took them corrected the hour in the same commit. The second
	// hour, untouched, still says what it said.
	preview := decodeJSON[struct {
		WouldDelete map[string]int `json:"would_delete"`
	}](t, h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"/users/forget-me/data", nil))
	if preview.WouldDelete["traces"] != 50 {
		t.Fatalf("%d traces left after one chunk, want the second hour's 50", preview.WouldDelete["traces"])
	}
	cut := h.statsBuckets(t, "/api/v1/stats?group_by=hour")
	if len(cut) != 2 || cut[0].Count != 1 || cut[1].Count != 51 {
		t.Errorf("buckets = %+v after the hang-up, want 1 and 51", cut)
	}

	// The next start resumes it, and finds only the traces that remain;
	// that is enough. The request of the stopped one answers the same
	// erasure (#11).
	resumed := h.store.NewEraser(h.writer, store.EraserOptions{})
	resumed.Start()
	defer resumed.Close()
	rec = h.call(t, "DELETE", path+"&wait=30", nil)
	expectStatus(t, rec, 200)
	erased := decodeJSON[struct {
		State   string         `json:"state"`
		Deleted map[string]int `json:"deleted"`
	}](t, rec)
	if erased.State != store.ErasureDone || erased.Deleted["traces"] != store.DefaultEraseChunk+50 {
		t.Errorf("the resumed erasure is %s with %d traces, want done with all %d",
			erased.State, erased.Deleted["traces"], store.DefaultEraseChunk+50)
	}
	after := h.statsBuckets(t, "/api/v1/stats?group_by=hour")
	if len(after) != 2 || after[0].Count != 1 || after[1].Count != 1 {
		t.Errorf("buckets = %+v after the repeat, want the bystanders' 1 and 1", after)
	}
}

// Purging a project takes its rollup with everything else: the rows cascade
// from the project row (spec 013 #6).
func TestPurgingAProjectTakesItsRollup(t *testing.T) {
	// Deleting a project is cross-project work, so it needs the admin
	// token (spec 005 #11).
	h := newAdminHarness(t)
	h.seedHour(t, statsHour, 1, 2, "production")
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	hours, err := h.store.StatsRollupHours(t.Context(), h.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hours) == 0 {
		t.Fatal("nothing rolled, so the purge would prove nothing")
	}

	// A soft delete is accepted, not done: the grace window is the point
	// (spec 005 #9).
	rec := h.call(t, "DELETE",
		"/api/v1/projects/"+h.project.ID+"?confirm="+h.project.Name, nil, asAdmin)
	expectStatus(t, rec, 202)

	// Past the grace window, the sweeper purges what is left.
	sweeper := h.store.NewSweeper(h.writer, store.SweepOptions{
		Now: func() time.Time { return time.Now().Add(store.GraceWindow + time.Hour) },
	})
	if err := sweeper.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}

	hours, err = h.store.StatsRollupHours(t.Context(), h.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hours) != 0 {
		t.Errorf("rolled hours = %v survived the purge", hours)
	}
}
