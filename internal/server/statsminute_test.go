package server

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// `group_by=minute` (spec 034 #15): the timeline of a short window, read from
// the traces themselves because the rollup holds no minutes, and refused for
// a window longer than a day so that it stays a scan of one day.

// seedAt puts one trace, with one priced generation, at an offset in seconds
// from statsHour.
func (h *harness) seedAt(t *testing.T, n int, offset int64, environment string) {
	t.Helper()
	start := (statsHour + offset) * int64(time.Second)
	trace := &model.Trace{ID: traceHex(n), Environment: environment}
	h.seed(t, trace, &model.Observation{
		TraceID: trace.ID, ID: spanHex(n), Type: model.TypeGeneration,
		Level: model.LevelDefault, Model: "claude-sonnet-5",
		StartTime: start, EndTime: start + int64(100+n*10)*ms,
		CostDetails: map[string]any{"total": 0.001},
	})
}

// minuteURL is the stats query over [statsHour+from, statsHour+to) in seconds.
func minuteURL(from, to int64, extra string) string {
	at := func(offset int64) string { return time.Unix(statsHour+offset, 0).UTC().Format(time.RFC3339) }
	return fmt.Sprintf("/api/v1/stats?group_by=minute&from=%s&to=%s%s", at(from), at(to), extra)
}

func TestStatsMinuteReadsTheTracesBehindTheRollup(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedAt(t, 1, 5*60+1, "production")
	h.seedAt(t, 2, 5*60+40, "production")
	h.seedAt(t, 3, 17*60, "production")
	h.seedAt(t, 4, 17*60+59, "staging")
	// The hour is rolled: a minute answer that came from the rollup would
	// carry the hour's key, not two minutes'.
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	buckets := h.statsBuckets(t, minuteURL(0, 3600, ""))
	got := map[string]int{}
	for _, b := range buckets {
		got[b.Key] = b.Count
		if b.TotalCost == nil || b.LatencyMs.P95 == nil {
			t.Errorf("bucket %s = %+v, want a cost and a p95", b.Key, b)
		}
	}
	want := map[string]int{"2026-08-26T10:05:00Z": 2, "2026-08-26T10:17:00Z": 2}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("minutes = %v, want %v and no zero rows between them", got, want)
	}
	if len(buckets) != 2 || buckets[0].Key > buckets[1].Key {
		t.Errorf("buckets = %+v, want two in ascending order", buckets)
	}

	// The filters are the hourly timeline's.
	production := h.statsBuckets(t, minuteURL(0, 3600, "&environment=production"))
	if len(production) != 2 || production[1].Count != 1 {
		t.Errorf("production = %+v, want 10:17 to hold one trace", production)
	}
	// And the bounds are the trace timestamp's, half-open.
	if late := h.statsBuckets(t, minuteURL(17*60+59, 3600, "")); len(late) != 1 || late[0].Count != 1 {
		t.Errorf("from 10:17:59 = %+v, want the one trace at that instant", late)
	}
}

func TestStatsMinuteTakesADayAtMost(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedAt(t, 1, 60, "production")

	expectError(t, h.get(t, "/api/v1/stats?group_by=minute"), http.StatusBadRequest, "needs from")
	expectError(t, h.get(t, minuteURL(0, 25*3600, "")), http.StatusBadRequest, "at most 24 hours")
	expectStatus(t, h.get(t, minuteURL(0, 24*3600, "")), http.StatusOK)
	// Bounds near either end of what a timestamp can be: their difference
	// does not fit in an int64, and must not wrap into a short window.
	expectError(t, h.get(t, "/api/v1/stats?group_by=minute&from=1700-01-01T00:00:00Z&to=2200-01-01T00:00:00Z"),
		http.StatusBadRequest, "at most 24 hours")

	// An open window ends now: the last day passes, with a few minutes for
	// the request in flight and a client clock behind this one; the last
	// two days do not.
	since := func(ago time.Duration) string {
		return "/api/v1/stats?group_by=minute&from=" + time.Now().Add(-ago).UTC().Format(time.RFC3339Nano)
	}
	expectStatus(t, h.get(t, since(24*time.Hour+4*time.Minute)), http.StatusOK)
	expectError(t, h.get(t, since(24*time.Hour+6*time.Minute)), http.StatusBadRequest, "at most 24 hours")
	expectError(t, h.get(t, since(48*time.Hour)), http.StatusBadRequest, "group by hour or day")

	// The limit is the minute grouping's alone.
	expectStatus(t, h.get(t, "/api/v1/stats?group_by=hour"), http.StatusOK)
}

// With `user_id` a minute carries `sessions`, as every timed grouping does,
// each session counted in the minute its first trace fell in.
func TestStatsMinuteByUserCarriesSessions(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	userCorpus(t, h)
	h.seedUserHour(t, 7, "alice", "s-late", statsHour, 125, "production", false, nil)

	body := h.userStats(t, minuteURL(0, 3600, "&user_id=alice"))
	got := map[string]string{}
	for _, b := range body.Buckets {
		got[b.Key] = fmt.Sprintf("%d/%d", b.Count, deref(b.Sessions))
	}
	want := map[string]string{"2026-08-26T10:00:00Z": "3/2", "2026-08-26T10:02:00Z": "1/1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("alice by minute (traces/sessions) = %v, want %v", got, want)
	}
}

// An open window is scanned to its bound, not to the end of time: a trace a
// skewed clock stamped an hour ahead is counted, one a week ahead is not.
func TestStatsMinuteScansAnOpenWindowToItsBound(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	now := time.Now().Unix()
	h.seedAt(t, 1, now-statsHour-30*60, "production")
	h.seedAt(t, 2, now-statsHour+3600, "production")
	h.seedAt(t, 3, now-statsHour+7*24*3600, "production")

	from := time.Unix(now-3600, 0).UTC().Format(time.RFC3339)
	total := 0
	for _, b := range h.statsBuckets(t, "/api/v1/stats?group_by=minute&from="+from) {
		total += b.Count
	}
	if total != 2 {
		t.Errorf("an open minute window counted %d traces, want 2: the week-ahead one is past its bound", total)
	}
}
