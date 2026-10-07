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

	// An open window ends now: the last day passes, with the moment the
	// request took to arrive; the last two do not.
	since := func(ago time.Duration) string {
		return "/api/v1/stats?group_by=minute&from=" + time.Now().Add(-ago).UTC().Format(time.RFC3339Nano)
	}
	expectStatus(t, h.get(t, since(24*time.Hour+5*time.Second)), http.StatusOK)
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
