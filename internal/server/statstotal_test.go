package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// `group_by=total` (spec 034 #3): the whole window as one bucket, in the
// shape of a day bucket, its percentiles merged over every hour's histogram
// on both sides of the seam. The one number the summary row shows, and the
// one the endpoint could not give before: a p95 over a week is not a
// function of seven daily p95s.

// totalLatencies is what `seedHour` gives the traces of one hour: the i-th
// trace runs for 100 + 10·i milliseconds.
func totalLatencies(n int) []int64 {
	out := make([]int64, 0, n)
	for i := range n {
		out = append(out, int64(100+i*10))
	}
	return out
}

// mergedP95 is the percentile a histogram over every latency reports — which
// is what one rolled hour holds, and what the live half accumulates, so it is
// what `total` must equal whichever side answered.
func mergedP95(latencies ...[]int64) int64 {
	h := store.NewHistogram()
	for _, hour := range latencies {
		for _, ms := range hour {
			h.Add(ms)
		}
	}
	p95, _ := h.Percentile(95)
	return p95
}

func TestStatsTotalIsOneBucketOverTheSeam(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedHour(t, statsHour, 1, 6, "production")         // rolled
	h.seedHour(t, statsHour+3600, 10, 2, "production")   // rolled
	h.seedHour(t, statsHour+2*3600, 20, 5, "production") // the live tail
	// A pass whose clock leaves the third hour open.
	h.rollTheCorpus(t, time.Unix(statsHour+2*3600+30*60, 0))

	rec := h.get(t, "/api/v1/stats?group_by=total")
	expectStatus(t, rec, 200)
	body := decodeJSON[statsBody](t, rec)
	if body.GroupBy != "total" || body.Unit != "trace" {
		t.Errorf("group_by = %q, unit = %q; want total, trace", body.GroupBy, body.Unit)
	}
	if len(body.Buckets) != 1 || body.Buckets[0].Key != "" {
		t.Fatalf("buckets = %+v, want exactly one under the empty key", body.Buckets)
	}
	total := body.Buckets[0]

	// The count and the cost are the hours summed — the same rows, folded
	// under one key rather than thirteen.
	hours := h.statsBuckets(t, "/api/v1/stats?group_by=hour")
	var count int
	var cost float64
	for _, hour := range hours {
		count += hour.Count
		if hour.TotalCost != nil {
			cost += *hour.TotalCost
		}
	}
	if total.Count != count || count != 13 {
		t.Errorf("total counts %d, the hours count %d, want 13", total.Count, count)
	}
	if total.TotalCost == nil || *total.TotalCost-cost > 1e-9 || cost-*total.TotalCost > 1e-9 {
		t.Errorf("total cost = %v, the hours sum to %f", total.TotalCost, cost)
	}

	// The p95 is the merged histogram's, across the rolled hours and the
	// live one alike.
	want := mergedP95(totalLatencies(6), totalLatencies(2), totalLatencies(5))
	if total.LatencyMs.P95 == nil || *total.LatencyMs.P95 != want {
		t.Errorf("p95 = %v over the seam, want %d (the merged histogram's)", total.LatencyMs.P95, want)
	}

	// And over a window the rollup answers alone, the same rule.
	from := time.Unix(statsHour, 0).UTC().Format(time.RFC3339)
	to := time.Unix(statsHour+2*3600, 0).UTC().Format(time.RFC3339)
	rolled := h.statsBuckets(t, fmt.Sprintf("/api/v1/stats?group_by=total&from=%s&to=%s", from, to))
	if len(rolled) != 1 || rolled[0].Count != 8 {
		t.Fatalf("buckets = %+v over the rolled hours, want one of eight traces", rolled)
	}
	if want := mergedP95(totalLatencies(6), totalLatencies(2)); rolled[0].LatencyMs.P95 == nil ||
		*rolled[0].LatencyMs.P95 != want {
		t.Errorf("p95 = %v over the rolled hours, want %d", rolled[0].LatencyMs.P95, want)
	}
}

// An empty window answers with no buckets, not with one of zeros (spec 007
// #7: nothing fabricated).
func TestStatsTotalOfNothingIsNoBucket(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedHour(t, statsHour, 1, 3, "production")

	from := time.Unix(statsHour+24*3600, 0).UTC().Format(time.RFC3339)
	if buckets := h.statsBuckets(t, "/api/v1/stats?group_by=total&from="+from); len(buckets) != 0 {
		t.Errorf("buckets = %+v over an empty window, want none", buckets)
	}
	if buckets := h.statsBuckets(t, "/api/v1/stats?group_by=total&environment=nowhere"); len(buckets) != 0 {
		t.Errorf("buckets = %+v under a filter nothing matches, want none", buckets)
	}
}

// With `user_id` the bucket carries `sessions`, as the timed groupings do:
// the whole window is that timeline summed (spec 034 #3).
func TestStatsTotalByUserCarriesSessions(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	userCorpus(t, h)
	// A live hour, so the count crosses the seam.
	h.seedUserHour(t, 7, "alice", "s-late", statsHour+3*3600, 10, "production", true, price(0.01))

	body := h.userStats(t, "/api/v1/stats?group_by=total&user_id=alice")
	if len(body.Buckets) != 1 || body.Buckets[0].Key != "" {
		t.Fatalf("buckets = %+v, want one under the empty key", body.Buckets)
	}
	total := body.Buckets[0]
	if total.Count != 4 || total.ErrorCount != 2 {
		t.Errorf("alice = %d traces, %d errors; want 4 and 2", total.Count, total.ErrorCount)
	}
	if total.Sessions == nil || *total.Sessions != 3 {
		t.Errorf("sessions = %v, want 3: two rolled and one live, each counted where it began", total.Sessions)
	}

	// Without the user the key is absent, as it is on every other grouping.
	whole := h.userStats(t, "/api/v1/stats?group_by=total")
	if len(whole.Buckets) != 1 || whole.Buckets[0].Sessions != nil {
		t.Errorf("buckets = %+v unfiltered, want one bucket carrying no sessions", whole.Buckets)
	}
}

// The enum is the enum: a value that reads like "everything" is refused.
func TestStatsTotalIsTheOnlyWholeWindowSpelling(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	expectError(t, h.get(t, "/api/v1/stats?group_by=all"), http.StatusBadRequest, "group_by must be one of")
}

// The enum is one list in three places — the handler, `openapi.json` and the
// MCP tool's schema — and the document is the one every client reads, so the
// handler's list is checked against it here and the tool's against the
// document in its own package (spec 034 #3, parity).
func TestStatsGroupingsMatchOpenAPI(t *testing.T) {
	var document struct {
		Paths map[string]struct {
			Get struct {
				Parameters []struct {
					Ref    string `json:"$ref"`
					Name   string `json:"name"`
					Schema struct {
						Enum []string `json:"enum"`
					} `json:"schema"`
				} `json:"parameters"`
				Responses map[string]struct {
					Content map[string]struct {
						Schema struct {
							Properties map[string]struct {
								Enum []string `json:"enum"`
							} `json:"properties"`
						} `json:"schema"`
					} `json:"content"`
				} `json:"responses"`
			} `json:"get"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(openAPIDocument, &document); err != nil {
		t.Fatal(err)
	}
	stats := document.Paths["/api/v1/stats"].Get
	var asked []string
	for _, parameter := range stats.Parameters {
		if parameter.Name == "group_by" {
			asked = parameter.Schema.Enum
		}
	}
	answered := stats.Responses["200"].Content["application/json"].Schema.Properties["group_by"].Enum
	for label, enum := range map[string][]string{"parameter": asked, "response": answered} {
		if !slices.Equal(enum, statsGroupings) {
			t.Errorf("openapi.json's %s group_by enum is %v, the handler accepts %v", label, enum, statsGroupings)
		}
	}
}
