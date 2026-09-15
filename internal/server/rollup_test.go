package server

import (
	"context"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// The read seam (spec 013 #5): the statistics answer from the rollup for the
// hours behind the watermark and from the live scan for the tail, and a
// reader cannot tell which half answered.

const statsHour = int64(1787738400) // 2026-08-26T10:00:00Z

// rollTheCorpus runs one aggregator pass with the clock stopped well past the
// seeded hours, so everything the fixture holds is closed and rolled.
func (h *harness) rollTheCorpus(t *testing.T, at time.Time) {
	t.Helper()
	aggregator := h.store.NewAggregator(h.writer, store.RollupOptions{
		Interval: time.Minute,
		Now:      func() time.Time { return at },
	})
	if err := aggregator.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// seedHour puts n traces in one hour, each with one costed generation.
func (h *harness) seedHour(t *testing.T, hour int64, first, n int, environment string) {
	t.Helper()
	for i := range n {
		start := hour*int64(time.Second) + int64(i)*int64(time.Second)
		trace := &model.Trace{ID: traceHex(first + i), Environment: environment}
		h.seed(t, trace, &model.Observation{
			TraceID: trace.ID, ID: spanHex(first + i), Type: model.TypeGeneration,
			Level: model.LevelDefault, Model: "claude-sonnet-5",
			StartTime: start, EndTime: start + int64(100+i*10)*ms,
			CostDetails: map[string]any{"total": 0.001},
		})
	}
}

// The whole point, checked the way the spec asks: a range entirely behind the
// watermark answers with the raw tables emptied.
func TestARangeBehindTheWatermarkOutlivesTheRawRows(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedHour(t, statsHour, 1, 4, "production")
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	before := h.statsBuckets(t, "/api/v1/stats?group_by=hour")
	if len(before) != 1 || before[0].Count != 4 {
		t.Fatalf("buckets = %+v, want one hour of four traces", before)
	}

	// Retention takes the raw rows through its own path, which is the one
	// that must leave the rollup standing (spec 013 #6).
	if err := h.setRetention(h.project.ID, 1); err != nil {
		t.Fatal(err)
	}
	sweeper := h.store.NewSweeper(h.writer, store.SweepOptions{
		Now: func() time.Time { return time.Unix(statsHour, 0).Add(30 * 24 * time.Hour) },
	})
	if err := sweeper.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if remaining := h.countTraces(t); remaining != 0 {
		t.Fatalf("%d traces survived the sweep; the test proves nothing", remaining)
	}

	after := h.statsBuckets(t, "/api/v1/stats?group_by=hour")
	if len(after) != 1 || after[0].Count != 4 {
		t.Fatalf("buckets = %+v after the raw rows went, want the same four", after)
	}
	if after[0].Key != before[0].Key {
		t.Errorf("bucket key %q became %q", before[0].Key, after[0].Key)
	}
}

// A range that straddles the watermark is the two halves added, each row
// counted once. Double-counting at the seam would be a chart that doubles.
func TestARangeStraddlingTheWatermarkCountsEachRowOnce(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedHour(t, statsHour, 1, 3, "production")         // rolled
	h.seedHour(t, statsHour+3600, 10, 2, "production")   // rolled
	h.seedHour(t, statsHour+2*3600, 20, 5, "production") // the live tail

	// A pass whose clock leaves the third hour open.
	h.rollTheCorpus(t, time.Unix(statsHour+2*3600+30*60, 0))

	live := h.statsBuckets(t, "/api/v1/stats?group_by=environment")
	if len(live) != 1 || live[0].Count != 10 {
		t.Fatalf("buckets = %+v, want the ten traces of all three hours", live)
	}

	byHour := h.statsBuckets(t, "/api/v1/stats?group_by=hour")
	if len(byHour) != 3 {
		t.Fatalf("hours = %+v, want three", byHour)
	}
	for i, want := range []int{3, 2, 5} {
		if byHour[i].Count != want {
			t.Errorf("hour %s counts %d, want %d", byHour[i].Key, byHour[i].Count, want)
		}
	}
}

// A day bucket is its hours merged — the property that lets the rollup have
// no day table at all (spec 013, data contract).
func TestADayIsItsHoursMerged(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedHour(t, statsHour, 1, 3, "production")
	h.seedHour(t, statsHour+3600, 10, 2, "production")
	h.rollTheCorpus(t, time.Unix(statsHour+5*3600, 0))

	hours := h.statsBuckets(t, "/api/v1/stats?group_by=hour")
	days := h.statsBuckets(t, "/api/v1/stats?group_by=day")
	if len(days) != 1 {
		t.Fatalf("days = %+v, want one", days)
	}
	var total int
	for _, hour := range hours {
		total += hour.Count
	}
	if days[0].Count != total {
		t.Errorf("the day counts %d and its hours count %d", days[0].Count, total)
	}
}

// The seam must not change the answer: what the rollup reports for a hour is
// what the live scan reported for it before the pass ran.
func TestTheRolledAnswerEqualsTheLiveOne(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedHour(t, statsHour, 1, 6, "production")
	h.seedHour(t, statsHour, 30, 2, "staging")

	for _, grouping := range []string{"hour", "day", "environment", "model", "total"} {
		t.Run(grouping, func(t *testing.T) {
			path := "/api/v1/stats?group_by=" + grouping
			before := h.statsBuckets(t, path)

			// A pass on a copy of the same data: the same question, the
			// other side of the seam.
			h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))
			after := h.statsBuckets(t, path)

			if len(after) != len(before) {
				t.Fatalf("%d buckets rolled, %d live: %+v vs %+v",
					len(after), len(before), after, before)
			}
			for i := range before {
				if after[i].Key != before[i].Key || after[i].Count != before[i].Count ||
					after[i].ErrorCount != before[i].ErrorCount {
					t.Errorf("bucket %d: %+v rolled, %+v live", i, after[i], before[i])
				}
				switch {
				case (after[i].TotalCost == nil) != (before[i].TotalCost == nil):
					t.Errorf("bucket %s: cost %v rolled, %v live",
						after[i].Key, after[i].TotalCost, before[i].TotalCost)
				case after[i].TotalCost != nil &&
					(*after[i].TotalCost-*before[i].TotalCost > 1e-9 ||
						*before[i].TotalCost-*after[i].TotalCost > 1e-9):
					t.Errorf("bucket %s: cost %f rolled, %f live",
						after[i].Key, *after[i].TotalCost, *before[i].TotalCost)
				}
			}
		})
	}
}

// A window that starts inside an hour cannot be answered by that hour's row,
// so the partial edge stays live — and is counted once.
func TestAPartialHourAtTheEdgeStaysLive(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedHour(t, statsHour, 1, 4, "production")
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	// From half past the hour: the fixture puts its four traces in the
	// first four seconds, so a window opening later holds none of them.
	from := time.Unix(statsHour+1800, 0).UTC().Format(time.RFC3339)
	if buckets := h.statsBuckets(t, "/api/v1/stats?group_by=hour&from="+from); len(buckets) != 0 {
		t.Errorf("buckets = %+v, want none: the rolled hour must not answer a window "+
			"it only partly covers", buckets)
	}

	// And from the top of the hour it is the whole hour again.
	whole := time.Unix(statsHour, 0).UTC().Format(time.RFC3339)
	buckets := h.statsBuckets(t, "/api/v1/stats?group_by=hour&from="+whole)
	if len(buckets) != 1 || buckets[0].Count != 4 {
		t.Errorf("buckets = %+v, want the four traces of the whole hour", buckets)
	}
}

func (h *harness) statsBuckets(t *testing.T, path string) []struct {
	Key        string   `json:"key"`
	Count      int      `json:"count"`
	ErrorCount int      `json:"error_count"`
	TotalCost  *float64 `json:"total_cost"`
	LatencyMs  struct {
		P50 *int64 `json:"p50"`
		P95 *int64 `json:"p95"`
	} `json:"latency_ms"`
} {
	t.Helper()
	rec := h.get(t, path)
	expectStatus(t, rec, 200)
	return decodeJSON[statsBody](t, rec).Buckets
}
