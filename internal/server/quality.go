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

// Score trends (spec 025): the quality curve beside the traffic one, answered
// through spec 013's read seam — hours behind the watermark from
// `scores_hourly`, the tail and the partial hours at either edge from a live
// scan of the raw rows.
//
// It is an endpoint of its own rather than a `metric=` on `/stats` because the
// answer is a different shape: a series per score name, and a distribution per
// bucket of a categorical one. `targets` is here for the reason spec 004 #23
// put `unit` on the statistics — a trace-level score has no model, so a model
// grouping cannot count it, and two counts that are not comparable must not
// look alike.

// scoreGroupings is every value `group_by` accepts, in the order the error
// message and `openapi.json` list them (spec 025 #6). One list, so a grouping
// this endpoint knows and rejects cannot happen.
var scoreGroupings = []string{
	store.GroupByHour, store.GroupByDay, store.GroupByEnvironment,
	store.GroupByRelease, store.GroupByModel,
}

// defaultScoreGroupBy is what the endpoint groups by when the caller says
// nothing — the same bucket `/api/v1/stats` reaches for first.
const defaultScoreGroupBy = store.GroupByDay

// The two answers to "what did these scores grade" (Decision 6).
const (
	targetsAny         = "any"
	targetsObservation = "observation"
)

// scoreTrendFilter is what the endpoint was asked for.
type scoreTrendFilter struct {
	From        *int64
	To          *int64
	Environment string
	// Name narrows the answer to one score name — what the detail view
	// asks. Without it every name in the range is a series, which is what
	// the overview needs in one request.
	Name    string
	GroupBy string
}

// scoreBucket accumulates one group of one series, from either side of the
// seam. Nothing here is a mean or a rate yet: those are read-time divisions of
// the four numbers, which is what makes a day the exact merge of its 24 hours
// (spec 025 #2).
type scoreBucket struct {
	key   string
	count int64
	sum   float64
	min   *float64
	max   *float64
	// categories is the distribution of a categorical series in this
	// bucket, and nil for every other type.
	categories map[string]int64
}

// scoreSeries is one `(name, data_type)`. A name graded two ways — one without
// a config, scored numerically here and categorically there — is two series
// with the same name, which the type tells apart (edge cases).
type scoreSeries struct {
	name     string
	dataType string
	buckets  map[string]*scoreBucket
}

// handleScoreTrends serves the series and the breakdowns.
func (s *Server) handleScoreTrends(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "from", "to", "environment", "name", "group_by")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	filter := scoreTrendFilter{
		Environment: values.Get("environment"),
		Name:        values.Get("name"),
		GroupBy:     values.Get("group_by"),
	}
	if filter.GroupBy == "" {
		filter.GroupBy = defaultScoreGroupBy
	}
	if !slices.Contains(scoreGroupings, filter.GroupBy) {
		writeError(w, http.StatusBadRequest, fmt.Sprintf(
			"group_by must be one of %s, got %q",
			strings.Join(scoreGroupings, ", "), filter.GroupBy))
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

	// Grouping by model counts observation-level scores and nothing else: a
	// trace-level score has no model to sit under, and putting it in an
	// empty-keyed bucket would make the breakdown's rows add up to something
	// no reader asked for.
	byModel := filter.GroupBy == store.GroupByModel
	series := map[string]*scoreSeries{}
	fold := func(row store.ScoreStatsRow) {
		if byModel && row.Model == "" {
			return
		}
		key := row.Name + "\x00" + row.DataType
		one := series[key]
		if one == nil {
			one = &scoreSeries{name: row.Name, dataType: row.DataType,
				buckets: map[string]*scoreBucket{}}
			series[key] = one
		}
		bucketKey := scoreBucketKey(filter.GroupBy, row)
		bucket := one.buckets[bucketKey]
		if bucket == nil {
			bucket = &scoreBucket{key: bucketKey}
			one.buckets[bucketKey] = bucket
		}
		bucket.add(row)
	}

	if err := s.readScoreTrends(project, filter, fold); err != nil {
		slog.Error("read score trends failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to compute the score trends")
		return
	}

	targets := targetsAny
	if byModel {
		targets = targetsObservation
	}
	writeJSON(w, http.StatusOK, object{}.
		put("group_by", filter.GroupBy).
		put("targets", targets).
		put("series", renderScoreSeries(series)))
}

// add folds one row — a stored hour or one live score — into a bucket.
func (b *scoreBucket) add(row store.ScoreStatsRow) {
	b.count += row.Count
	b.sum += row.Sum
	if row.Min != nil && (b.min == nil || *row.Min < *b.min) {
		b.min = row.Min
	}
	if row.Max != nil && (b.max == nil || *row.Max > *b.max) {
		b.max = row.Max
	}
	if row.DataType != store.ScoreCategorical {
		return
	}
	if b.categories == nil {
		b.categories = map[string]int64{}
	}
	b.categories[row.Category] += row.Count
}

// renderScoreSeries writes the answer: series ordered by name and then by type,
// buckets ascending by key — a timeline reads as a timeline and a breakdown as
// a list, and both are the same rule.
func renderScoreSeries(series map[string]*scoreSeries) []object {
	ordered := make([]*scoreSeries, 0, len(series))
	for _, one := range series {
		ordered = append(ordered, one)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].name != ordered[j].name {
			return ordered[i].name < ordered[j].name
		}
		return ordered[i].dataType < ordered[j].dataType
	})

	out := make([]object, 0, len(ordered))
	for _, one := range ordered {
		buckets := make([]*scoreBucket, 0, len(one.buckets))
		for _, bucket := range one.buckets {
			buckets = append(buckets, bucket)
		}
		sort.Slice(buckets, func(i, j int) bool { return buckets[i].key < buckets[j].key })

		rows := make([]object, 0, len(buckets))
		for _, bucket := range buckets {
			rows = append(rows, bucket.render(one.dataType))
		}
		out = append(out, object{}.
			put("name", one.name).
			put("data_type", one.dataType).
			put("buckets", rows))
	}
	return out
}

// render is one bucket in the shape its data type reads in: a mean and the
// extremes for a numeric name, a rate for a boolean one, the distribution for a
// categorical one. The count is on all three, because "the mean of what" is a
// question every one of them raises.
func (b *scoreBucket) render(dataType string) object {
	row := object{}.put("key", b.key).put("count", b.count)
	switch dataType {
	case store.ScoreNumeric:
		return row.
			put("mean", b.mean()).
			put("min", b.min).
			put("max", b.max)
	case store.ScoreBoolean:
		// The share of `1`s, which is what a boolean score's curve is:
		// `sum` counts them and `count` is how many were graded.
		return row.put("rate", b.mean())
	default:
		categories := object{}
		names := make([]string, 0, len(b.categories))
		for name := range b.categories {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			categories = categories.put(name, b.categories[name])
		}
		return row.put("categories", categories)
	}
}

// mean is the read-time division. A bucket with no rows in it does not exist,
// so the guard is against a stored count of zero rather than against the
// ordinary case.
func (b *scoreBucket) mean() any {
	if b.count == 0 {
		return nil
	}
	value := b.sum / float64(b.count)
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	return value
}

// scoreBucketKey is the bucket a row belongs to, spelled exactly as the
// statistics spell it: two screens reading "2026-09-01" from two endpoints must
// mean the same day.
func scoreBucketKey(groupBy string, row store.ScoreStatsRow) string {
	switch groupBy {
	case store.GroupByModel:
		return row.Model
	case store.GroupByEnvironment:
		return row.Environment
	case store.GroupByRelease:
		return row.Release
	default:
		return rollupKey(groupBy, store.StatsRow{Hour: row.Hour})
	}
}

// readScoreTrends splits the asked range at the project's watermark, exactly as
// `readStats` does and for the same reasons: one seam, one set of rules, one
// place for the lag to be stated (spec 025 #7).
//
// A project nobody has rolled has a watermark of zero, so every query is the
// live scan and the first pass is the backfill.
func (s *Server) readScoreTrends(project *store.Project, filter scoreTrendFilter,
	fold func(store.ScoreStatsRow)) error {
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

	rolledFrom, rolledTo := int64(0), int64(0)
	if state.RolledUntil > 0 {
		// How far back the rollup can speak for is what it holds, not
		// what a retention window says it should (spec 013 #17). The
		// floor is `stats_hourly`'s own oldest row, which is the same
		// floor for all three tables: one pass writes them together and
		// one sweep takes them together.
		oldest, held, err := s.store.OldestRolledHour(projectID)
		if err != nil {
			return err
		}
		if held {
			rolledFrom = max(hourCeiling(max(from, 0)), oldest)
			rolledTo = min(state.RolledUntil, store.HourOf(to))
		}
	}
	if rolledTo <= rolledFrom {
		return s.liveScores(projectID, filter, from, to, fold)
	}

	if err := s.store.ScoresRollupRows(projectID, rolledFrom, rolledTo,
		filter.Environment, filter.Name, fold); err != nil {
		return err
	}
	// The partial hour at the head, and everything from the watermark on.
	// The two live segments and the rolled range are disjoint by
	// construction: a score counted twice would be a curve that jumps at the
	// seam.
	head := rolledFrom * int64(time.Second)
	if from < head {
		if err := s.liveScores(projectID, filter, from, head, fold); err != nil {
			return err
		}
	}
	if tail := rolledTo * int64(time.Second); tail < to {
		return s.liveScores(projectID, filter, tail, to, fold)
	}
	return nil
}

// liveScores folds a half-open range of raw rows into the same buckets.
func (s *Server) liveScores(projectID string, filter scoreTrendFilter,
	from, to int64, fold func(store.ScoreStatsRow)) error {
	return s.store.ScoreSamples(projectID, from, to, filter.Environment, filter.Name, fold)
}
