package server

import (
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/tracepad/tracepad/internal/store"
)

// Statistics (spec 004 #8): computed on the fly, no rollup table. At MVP scale
// SQLite scans the rows in tens of milliseconds, and a rollup is premature
// state to keep consistent. Percentiles are exact — computed in Go over the
// grouped scan — because an approximation is not worth its explanation.

// defaultGroupBy is what `GET /api/v1/stats` groups by when the caller says
// nothing. A day is the bucket a human and an agent both reach for first.
const defaultGroupBy = store.GroupByDay

// statsGroupings is every value `group_by` accepts, in the order the error
// message and `openapi.json` list them. One list, so a grouping the store
// knows and the API rejects cannot happen (spec 012 #4 added `release`).
var statsGroupings = []string{
	store.GroupByHour, store.GroupByDay, store.GroupByModel,
	store.GroupByEnvironment, store.GroupByRelease,
}

// bucket accumulates one group of the scan.
type bucket struct {
	key        string
	count      int
	errorCount int
	totalCost  float64
	// costed reports whether anything in this bucket carried a cost:
	// summing over rows that provided none would report zero where the
	// truth is "nobody said" (spec 002 #14).
	costed    bool
	latencies []int64
}

// handleStats serves count, errors, cost and latency percentiles per bucket.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "from", "to", "environment", "group_by")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	filter := store.StatsFilter{
		Environment: values.Get("environment"),
		GroupBy:     values.Get("group_by"),
	}
	if filter.GroupBy == "" {
		filter.GroupBy = defaultGroupBy
	}
	if !slices.Contains(statsGroupings, filter.GroupBy) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"group_by must be one of %s, got %q",
			strings.Join(statsGroupings, ", "), filter.GroupBy))
		return
	}
	for _, bound := range []struct {
		name   string
		target **int64
	}{
		{"from", &filter.From},
		{"to", &filter.To},
	} {
		raw := values.Get(bound.name)
		if raw == "" {
			continue
		}
		instant, err := parseTime(bound.name, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		*bound.target = &instant
	}

	buckets := map[string]*bucket{}
	err = s.store.StatsSamples(project.ID, filter, func(sample store.StatsSample) {
		b := buckets[sample.Key]
		if b == nil {
			b = &bucket{key: sample.Key}
			buckets[sample.Key] = b
		}
		b.count++
		if sample.Errored {
			b.errorCount++
		}
		if sample.Cost != nil {
			b.totalCost += *sample.Cost
			b.costed = true
		}
		if sample.LatencyMs != nil {
			b.latencies = append(b.latencies, *sample.LatencyMs)
		}
	})
	if err != nil {
		slog.Error("read stats failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to compute the statistics")
		return
	}

	// An empty range answers with no buckets rather than with fabricated
	// zero rows (edge cases): a gap in the data is information.
	ordered := make([]*bucket, 0, len(buckets))
	for _, b := range buckets {
		ordered = append(ordered, b)
	}
	// Time buckets read as a timeline and the others as a list, and both
	// are the same rule: ascending by key.
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].key < ordered[j].key })

	rows := make([]object, 0, len(ordered))
	for _, b := range ordered {
		row := object{}.
			put("key", b.key).
			put("count", b.count).
			put("error_count", b.errorCount)
		if b.costed {
			row = row.put("total_cost", b.totalCost)
		}
		rows = append(rows, row.put("latency_ms", object{}.
			put("p50", percentile(b.latencies, 50)).
			put("p95", percentile(b.latencies, 95))))
	}

	writeJSON(w, http.StatusOK, object{}.
		put("group_by", filter.GroupBy).
		// What a bucket counts, said out loud: grouping by model counts
		// observations because a trace has no model, and two counts
		// that are not comparable must not look alike (Decision 23).
		put("unit", store.StatsUnit(filter.GroupBy)).
		put("buckets", rows))
}

// percentile returns the nearest-rank percentile of the samples, or nothing
// when the bucket has no latency to report.
func percentile(samples []int64, p int) any {
	if len(samples) == 0 {
		return nil
	}
	sorted := append([]int64(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	// Nearest rank: the smallest value at or above which p% of the samples
	// lie. Exact, no interpolation, and it always returns a value that was
	// actually measured.
	rank := int(math.Ceil(float64(p) / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}
