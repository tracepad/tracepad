package server

import (
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// Statistics (spec 004 #8, spec 013): answered from the hourly rollup for the
// hours behind a project's watermark and from the live scan for the tail. The
// seam is invisible because both halves produce the same thing — dimension
// tuples with counts and a latency histogram — and because percentiles are
// histogram-based on both sides (spec 013 #2).
//
// The exact sort this file used to do, over every latency of a bucket
// buffered in Go, is gone. Two paths would have answered the same question
// with two numbers, and the number *changing* when the raw rows expire is
// exactly the surprise a chart must not spring.

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

// bucket accumulates one group, from either side of the seam.
type bucket struct {
	key        string
	count      int64
	errorCount int64
	totalCost  float64
	// costed reports whether anything in this bucket carried a cost:
	// summing over rows that provided none would report zero where the
	// truth is "nobody said" (spec 002 #14).
	costed  bool
	latency store.Histogram
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
	at := func(key string) *bucket {
		b := buckets[key]
		if b == nil {
			b = &bucket{key: key}
			buckets[key] = b
		}
		return b
	}
	if err := s.readStats(project, filter, at); err != nil {
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
			put("p50", percentile(b.latency, 50)).
			put("p95", percentile(b.latency, 95))))
	}

	writeJSON(w, http.StatusOK, object{}.
		put("group_by", filter.GroupBy).
		// What a bucket counts, said out loud: grouping by model counts
		// observations because a trace has no model, and two counts
		// that are not comparable must not look alike (Decision 23).
		put("unit", store.StatsUnit(filter.GroupBy)).
		put("buckets", rows))
}

// readStats fills the buckets from both sides of the watermark: the rolled
// hours fully inside the asked range, and the live scan for everything else —
// the tail past the watermark, and the partial hours at either edge that no
// hourly row can answer (spec 013 #5).
//
// A project nobody has rolled has a watermark of zero, so every query is the
// live scan and the first pass is the backfill.
func (s *Server) readStats(project *store.Project, filter store.StatsFilter, at func(string) *bucket) error {
	projectID := project.ID
	state, err := s.store.RollupState(projectID)
	if err != nil {
		return err
	}

	from, to := unbounded, int64(math.MaxInt64)
	if filter.From != nil {
		from = *filter.From
	}
	if filter.To != nil {
		to = *filter.To
	}

	// Only whole hours can come from the rollup, only hours the aggregator
	// has closed, and only if it has ever run.
	rolledFrom, rolledTo := int64(0), int64(0)
	if state.RolledUntil > 0 {
		rolledFrom = max(hourCeiling(max(from, 0)), statsFloor(project, time.Now()))
		rolledTo = min(state.RolledUntil, store.HourOf(to))
	}
	if rolledTo <= rolledFrom {
		return s.liveStats(projectID, filter, from, to, at)
	}

	if err := s.rolledStats(projectID, filter, rolledFrom, rolledTo, at); err != nil {
		return err
	}
	// The partial hour at the head, and everything from the watermark on.
	// The two live segments and the rolled range are disjoint by
	// construction: a row counted twice would be a chart that doubles at
	// the seam.
	head := rolledFrom * int64(time.Second)
	if from < head {
		if err := s.liveStats(projectID, filter, from, head, at); err != nil {
			return err
		}
	}
	if tail := rolledTo * int64(time.Second); tail < to {
		return s.liveStats(projectID, filter, tail, to, at)
	}
	return nil
}

// unbounded is what an absent `from` means to the seam: before every trace
// there is. It is not `math.MinInt64`, which would overflow the moment it was
// turned into an hour.
const unbounded = int64(0)

// statsFloor is the oldest hour the rollup may be asked about: past the
// project's `stats_retention_days` the aggregator has deleted its rows, and
// asking a swept table is how a chart reports nothing about data the store
// still holds (spec 013 #13). Those hours go to the live scan instead — the
// pre-rollup answer at the pre-rollup cost. When the traces are gone too, the
// live scan finds nothing and the emptiness is the truth.
func statsFloor(project *store.Project, now time.Time) int64 {
	if project == nil || project.StatsRetentionDays == nil {
		return 0
	}
	days := *project.StatsRetentionDays
	if days < 1 || days > store.MaxRetentionDays {
		return 0
	}
	return store.HourOf(now.Add(-time.Duration(days) * 24 * time.Hour).UnixNano())
}

// rolledStats folds the stored rows of a range into the buckets. Which rows
// count is the unit: the model grouping reads observation rows, everything
// else reads trace rows, which is the one table carrying both (spec 013 #1).
func (s *Server) rolledStats(projectID string, filter store.StatsFilter, fromHour, toHour int64, at func(string) *bucket) error {
	wantModel := filter.GroupBy == store.GroupByModel
	return s.store.StatsRollupRows(projectID, fromHour, toHour, filter.Environment,
		func(row store.StatsRow) {
			if (row.Model != "") != wantModel {
				return
			}
			b := at(rollupKey(filter.GroupBy, row))
			b.count += row.Count
			b.errorCount += row.ErrorCount
			if row.TotalCost != nil {
				b.totalCost += *row.TotalCost
				b.costed = true
			}
			b.latency.Merge(row.Latency)
		})
}

// liveStats folds a half-open range of raw rows into the same buckets.
func (s *Server) liveStats(projectID string, filter store.StatsFilter, from, to int64, at func(string) *bucket) error {
	window := filter
	window.From, window.To = &from, &to
	if from == unbounded {
		window.From = nil
	}
	if to == math.MaxInt64 {
		window.To = nil
	}
	return s.store.StatsSamples(projectID, window, func(sample store.StatsSample) {
		b := at(sample.Key)
		b.count++
		if sample.Errored {
			b.errorCount++
		}
		if sample.Cost != nil {
			b.totalCost += *sample.Cost
			b.costed = true
		}
		if sample.LatencyMs != nil {
			b.latency.Add(*sample.LatencyMs)
		}
	})
}

// rollupKey is the bucket key a stored row belongs to, spelled exactly as the
// live scan spells it: the two halves of one answer must not disagree about
// what a bucket is called.
func rollupKey(groupBy string, row store.StatsRow) string {
	hour := time.Unix(row.Hour, 0).UTC()
	switch groupBy {
	case store.GroupByHour:
		return hour.Format("2006-01-02T15:00:00Z")
	case store.GroupByDay:
		return hour.Format("2006-01-02")
	case store.GroupByModel:
		return row.Model
	case store.GroupByRelease:
		return row.Release
	default:
		return row.Environment
	}
}

// hourCeiling is the first whole hour at or after an instant: the rollup can
// only answer hours it holds entirely.
func hourCeiling(nanos int64) int64 {
	hour := store.HourOf(nanos)
	if hour*int64(time.Second) < nanos {
		hour += store.SecondsPerHour
	}
	return hour
}

// percentile reads a percentile out of a bucket's histogram, or nothing when
// the bucket has no latency to report. Histogram-based on both sides of the
// seam, by decision: two paths would answer the same question with two
// numbers (spec 013 #2).
func percentile(h store.Histogram, p int) any {
	value, ok := h.Percentile(p)
	if !ok {
		return nil
	}
	return value
}
