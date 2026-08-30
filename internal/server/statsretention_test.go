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

	if hours, err := h.store.StatsRollupHours(h.project.ID); err != nil {
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
		"/api/v1/projects/"+h.project.ID+"/users/forget-me/data?confirm=forget-me", nil)
	expectStatus(t, rec, 200)

	// The rollup is corrected in the same request that promised the data is
	// gone — a per-hour count is data derived from what was erased.
	after := h.statsBuckets(t, "/api/v1/stats?group_by=hour")
	if len(after) != 1 || after[0].Count != 2 {
		t.Errorf("buckets = %+v, want the two traces that remain", after)
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

	hours, err := h.store.StatsRollupHours(h.project.ID)
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

	hours, err = h.store.StatsRollupHours(h.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hours) != 0 {
		t.Errorf("rolled hours = %v survived the purge", hours)
	}
}
