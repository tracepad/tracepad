package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// The switcher's count (spec 029 #8): `activity=24h` puts each project's
// traces of the last day on its row, counted the way `GET /api/v1/stats`
// counts them — across the watermark — and absent unless asked for.

type activityRow struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Traces24h *int   `json:"traces_24h"`
}

func (h *harness) projectRows(t *testing.T, path string, who *signedIn) []activityRow {
	t.Helper()
	rec := h.call(t, "GET", path, nil, asSession(who))
	expectStatus(t, rec, 200)
	return decodeJSON[struct {
		Projects []activityRow `json:"projects"`
	}](t, rec).Projects
}

func TestProjectListingCountsTheDaysTraces(t *testing.T) {
	h := newAccountHarness(t)
	other := h.second(t, "other", "tp-sk-other")
	owner := h.owner(t)

	now := time.Now()
	hourAgo := func(hours int) int64 { return store.HourOf(now.Add(-time.Duration(hours) * time.Hour).UnixNano()) }
	// Three traces the aggregator will have rolled, two from the day before
	// yesterday that the window must leave out, and one in the open hour
	// that only the live scan can see.
	h.seedHour(t, hourAgo(3), 1, 3, "production")
	h.seedHour(t, hourAgo(26), 10, 2, "production")
	live := now.Add(-time.Second).UnixNano()
	trace := &model.Trace{ID: traceHex(20), Environment: "production"}
	h.seed(t, trace, &model.Observation{
		TraceID: trace.ID, ID: spanHex(20), Type: model.TypeGeneration,
		Level: model.LevelDefault, StartTime: live, EndTime: live + 100*ms,
	})
	// And one either side of the window's first instant, in an hour the
	// aggregator rolls whole: only the later one is the day's.
	for i, at := range []int64{now.Add(-24*time.Hour - time.Minute).UnixNano(), now.Add(-24*time.Hour + time.Minute).UnixNano()} {
		edge := &model.Trace{ID: traceHex(30 + i), Environment: "production"}
		h.seed(t, edge, &model.Observation{
			TraceID: edge.ID, ID: spanHex(30 + i), Type: model.TypeGeneration,
			Level: model.LevelDefault, StartTime: at, EndTime: at + 100*ms,
		})
	}
	h.rollTheCorpus(t, now)
	if state, err := h.store.RollupState(t.Context(), h.project.ID); err != nil || state.RolledUntil <= hourAgo(3) {
		t.Fatalf("watermark = %d (%v); the rolled hour is not behind it and the test proves nothing", state.RolledUntil, err)
	}

	rows := h.projectRows(t, "/api/v1/projects?activity=24h", owner)
	if len(rows) != 2 {
		t.Fatalf("projects = %+v, want both", rows)
	}
	counts := map[string]*int{}
	for _, row := range rows {
		counts[row.ID] = row.Traces24h
	}
	if got := counts[h.project.ID]; got == nil || *got != 5 {
		t.Errorf("traces_24h = %v, want the three rolled, the one live and the one just inside the window, "+
			"not the two of the day before or the one just outside", deref(got))
	}
	if got := counts[other.ID]; got == nil || *got != 0 {
		t.Errorf("an idle project's traces_24h = %v, want 0 rather than absent", deref(got))
	}

	// The same number the statistics screen would show for the same day.
	from := now.Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)
	to := now.UTC().Format(time.RFC3339Nano)
	var total int
	for _, bucket := range h.statsBuckets(t, "/api/v1/stats?group_by=hour&from="+from+"&to="+to) {
		total += bucket.Count
	}
	if total != 5 {
		t.Errorf("stats over the same day count %d, want the listing's 5", total)
	}

	// Without the parameter the listing is exactly what it was.
	rec := h.call(t, "GET", "/api/v1/projects", nil, asSession(owner))
	expectStatus(t, rec, 200)
	if strings.Contains(rec.Body.String(), "traces_24h") {
		t.Errorf("the listing carries a count nobody asked for: %s", rec.Body.String())
	}
	for _, row := range h.projectRows(t, "/api/v1/projects", owner) {
		if row.Traces24h != nil {
			t.Errorf("%s carries traces_24h without activity=", row.Name)
		}
	}

	// One value today; anything else is a 400 rather than a silent listing.
	expectError(t, h.call(t, "GET", "/api/v1/projects?activity=7d", nil, asSession(owner)),
		http.StatusBadRequest, "activity: only 24h")

	// One statement for the listing counts what the stats seam counts, the
	// rolled hours and the live tail alike (spec 043 #29).
	since, until := now.Add(-24*time.Hour).UnixNano(), time.Now().UnixNano()
	grouped, err := h.store.TracesBetween(t.Context(), []string{h.project.ID, other.ID}, since, until)
	if err != nil {
		t.Fatal(err)
	}
	for _, project := range []*store.Project{h.project, other} {
		var tally bucket
		if err := h.server.readStats(t.Context(), project,
			store.StatsFilter{From: &since, To: &until, GroupBy: defaultGroupBy},
			func(string) *bucket { return &tally }); err != nil {
			t.Fatal(err)
		}
		if grouped[project.ID] != tally.count {
			t.Errorf("%s: one statement counts %d, the stats seam %d", project.Name, grouped[project.ID], tally.count)
		}
	}
}

// A soft-deleted project is not there as far as a person is concerned, and
// its count says so rather than counting what the sweeper is about to purge.
func TestADeletedProjectCountsNoTraces(t *testing.T) {
	h := newAccountHarness(t)
	other := h.second(t, "other", "tp-sk-other")
	owner := h.owner(t)
	h.seedHour(t, store.HourOf(time.Now().Add(-time.Hour).UnixNano()), 1, 2, "production")

	rec := h.call(t, "DELETE", "/api/v1/projects/"+other.ID+"?confirm=other", nil, asAdmin)
	expectStatus(t, rec, 202)

	for _, row := range h.projectRows(t, "/api/v1/projects?include=deleted&activity=24h", owner) {
		switch row.ID {
		case h.project.ID:
			if row.Traces24h == nil || *row.Traces24h != 2 {
				t.Errorf("the live project's traces_24h = %v, want 2", deref(row.Traces24h))
			}
		case other.ID:
			if row.Traces24h == nil || *row.Traces24h != 0 {
				t.Errorf("the deleted project's traces_24h = %v, want 0", deref(row.Traces24h))
			}
		}
	}
}
