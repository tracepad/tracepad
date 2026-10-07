package server

import (
	"context"
	"errors"
	"fmt"
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
// knows and the API rejects cannot happen (spec 012 #4 added `release`,
// spec 034 #3 `total`, spec 034 #15 `minute`).
var statsGroupings = []string{
	store.GroupByMinute, store.GroupByHour, store.GroupByDay, store.GroupByModel,
	store.GroupByEnvironment, store.GroupByRelease, store.GroupByTotal,
}

// carriesSessions reports whether an answer to this filter carries
// `sessions` per bucket: a count of sessions has no meaning in `stats_hourly`
// and no bucket to live in outside a timeline, so it rides exactly the one
// question it answers — this user's activity over time (spec 023 #6) — and
// the whole window as one bucket, which is that timeline summed (spec 034 #3).
func carriesSessions(filter store.StatsFilter) bool {
	if filter.UserID == "" {
		return false
	}
	switch filter.GroupBy {
	case store.GroupByMinute, store.GroupByHour, store.GroupByDay, store.GroupByTotal:
		return true
	}
	return false
}

// bucket accumulates one group, from either side of the seam.
type bucket struct {
	key        string
	count      int64
	errorCount int64
	// totalCost holds nothing while nothing in this bucket carried a cost:
	// summing over rows that provided none would report zero where the
	// truth is "nobody said" (spec 002 #14).
	totalCost store.CostSum
	latency   store.Histogram
	// tokens are the three sums of spec 031, added the way cost is: a half
	// that carried none contributes nothing and does not make the sum zero.
	tokens store.Tokens
	// sessions is how many of the user's sessions began in this bucket. It
	// rides only a `user_id` timeline (spec 023 #6): `stats_hourly` has no
	// such number, so without the filter the key is absent rather than zero.
	sessions int64
}

// handleStats serves count, errors, cost and latency percentiles per bucket.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "from", "to", "environment", "user_id", "group_by")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	environment, err := filterList(values, "environment")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	filter := store.StatsFilter{
		Environment: environment,
		UserID:      lookupLabel(values.Get("user_id")),
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
	if filter.GroupBy == store.GroupByMinute {
		if err := minuteWindow(filter, time.Now()); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
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
	sessions := carriesSessions(filter)
	if err := s.readStats(r.Context(), project, filter, at); err != nil {
		readFailed(w, r, "failed to compute the statistics", err)
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
		if cost, ok := b.totalCost.Value(); ok {
			row = row.put("total_cost", cost)
		}
		if tokens := tokensObject(b.tokens); tokens != nil {
			row = row.put("tokens", tokens)
		}
		if sessions {
			row = row.put("sessions", b.sessions)
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

// minuteWindowLimit is the longest window `group_by=minute` answers (spec 034
// #15): 1,440 points, and a scan of a day of the traces table rather than of
// all of it — minutes are read from the raw rows, never from the rollup.
const minuteWindowLimit = 24 * time.Hour

// minuteWindowGrace is how far past the limit a window may reach: a client
// that resolved "the last 24 hours" a moment ago sends a window that is, by
// the time it is read here, 24 hours and the time the request took.
const minuteWindowGrace = time.Minute

// minuteWindow refuses a minute timeline whose window has no start or is
// longer than a day, with what to ask instead.
func minuteWindow(filter store.StatsFilter, now time.Time) error {
	if filter.From == nil {
		return errors.New("group_by=minute needs from: it answers a window of at most 24 hours")
	}
	to := now.UnixNano()
	if filter.To != nil {
		to = *filter.To
	}
	if span := time.Duration(to - *filter.From); span > minuteWindowLimit+minuteWindowGrace {
		return fmt.Errorf("group_by=minute answers a window of at most 24 hours, this one is %s; group by hour or day",
			span.Round(time.Minute))
	}
	return nil
}

// readStats fills the buckets from both sides of the watermark: the rolled
// hours fully inside the asked range, and the live scan for everything else —
// the tail past the watermark, and the partial hours at either edge that no
// hourly row can answer (spec 013 #5).
//
// A project nobody has rolled has a watermark of zero, so every query is the
// live scan and the first pass is the backfill.
func (s *Server) readStats(ctx context.Context, project *store.Project, filter store.StatsFilter, at func(string) *bucket) error {
	projectID := project.ID
	from, to := unbounded, int64(math.MaxInt64)
	if filter.From != nil {
		from = *filter.From
	}
	if filter.To != nil {
		to = *filter.To
	}
	// The rollup has no minutes: a minute timeline is the live scan of its
	// window, which the handler has bounded to a day.
	if filter.GroupBy == store.GroupByMinute {
		return s.liveStats(ctx, projectID, filter, from, to, at)
	}

	state, err := s.store.RollupState(ctx, projectID)
	if err != nil {
		return err
	}

	// Only whole hours can come from the rollup, only hours the aggregator
	// has closed, and only if it has ever run.
	rolledFrom, rolledTo := int64(0), int64(0)
	if state.RolledUntil > 0 {
		// How far back the rollup can speak for is what it holds, not
		// what the retention window says it should hold: the window can
		// be lengthened and a sweep cannot be undone (spec 013 #17).
		oldest, held, err := s.store.OldestRolledHour(ctx, projectID)
		if err != nil {
			return err
		}
		if held {
			rolledFrom = max(hourCeiling(max(from, 0)), oldest)
			rolledTo = min(state.RolledUntil, store.HourOf(to))
		}
	}
	if rolledTo <= rolledFrom {
		return s.liveStats(ctx, projectID, filter, from, to, at)
	}

	if err := s.rolledStats(ctx, projectID, filter, rolledFrom, rolledTo, at); err != nil {
		return err
	}
	// The partial hour at the head, and everything from the watermark on.
	// The two live segments and the rolled range are disjoint by
	// construction: a row counted twice would be a chart that doubles at
	// the seam.
	head := rolledFrom * int64(time.Second)
	if from < head {
		if err := s.liveStats(ctx, projectID, filter, from, head, at); err != nil {
			return err
		}
	}
	if tail := rolledTo * int64(time.Second); tail < to {
		return s.liveStats(ctx, projectID, filter, tail, to, at)
	}
	return nil
}

// unbounded is what an absent `from` means to the seam: before every trace
// there is. It is not `math.MinInt64`, which would overflow the moment it was
// turned into an hour.
const unbounded = int64(0)

// rolledStats folds the stored rows of a range into the buckets. Which rows
// count is the unit: the model grouping reads observation rows, everything
// else reads trace rows, which is the one table carrying both (spec 013 #1).
//
// With a `user_id` the rows come from `users_hourly` instead — the same tuple
// with the user in it, so the fold below is the same fold (spec 023 #6).
func (s *Server) rolledStats(ctx context.Context, projectID string, filter store.StatsFilter, fromHour, toHour int64, at func(string) *bucket) error {
	wantModel := filter.GroupBy == store.GroupByModel
	fold := func(row store.StatsRow) *bucket {
		if (row.Model != "") != wantModel {
			return nil
		}
		b := at(rollupKey(filter.GroupBy, row))
		b.count += row.Count
		b.errorCount += row.ErrorCount
		if row.TotalCost != nil {
			b.totalCost.Add(*row.TotalCost)
		}
		b.latency.Merge(row.Latency)
		b.tokens.Add(row.Tokens)
		return b
	}
	if filter.UserID != "" {
		return s.store.UsersRollupRows(ctx, projectID, filter.UserID, fromHour, toHour,
			filter.Environment, func(row store.UserStatsRow) {
				if b := fold(row.StatsRow); b != nil {
					// Only trace-unit rows carry it, and only those
					// reach here when the grouping is not by model.
					b.sessions += row.SessionsStarted
				}
			})
	}
	return s.store.StatsRollupRows(ctx, projectID, fromHour, toHour, filter.Environment,
		func(row store.StatsRow) { fold(row) })
}

// liveStats folds a half-open range of raw rows into the same buckets.
func (s *Server) liveStats(ctx context.Context, projectID string, filter store.StatsFilter, from, to int64, at func(string) *bucket) error {
	window := filter
	window.From, window.To = &from, &to
	if from == unbounded {
		window.From = nil
	}
	if to == math.MaxInt64 {
		window.To = nil
	}
	if err := s.store.StatsSamples(ctx, projectID, window, func(sample store.StatsSample) {
		b := at(sample.Key)
		b.count++
		if sample.Errored {
			b.errorCount++
		}
		if sample.Cost != nil {
			b.totalCost.Add(*sample.Cost)
		}
		if sample.LatencyMs != nil {
			b.latency.Add(*sample.LatencyMs)
		}
		b.tokens.Add(sample.Tokens)
	}); err != nil {
		return err
	}
	// The trace unit's tokens are one aggregate over the same window,
	// merged by key (spec 031 #5). It yields nothing for the model
	// grouping, whose samples carried their own above — and nothing for a
	// key whose traces had no observation with a count, so a bucket is
	// never created here that the scan did not.
	if err := s.store.StatsTokens(ctx, projectID, window, func(sum store.StatsTokenSum) {
		at(sum.Key).tokens.Add(sum.Tokens)
	}); err != nil {
		return err
	}
	return s.liveSessions(ctx, projectID, filter, from, to, at)
}

// tokensObject is the `tokens` of a bucket, a trace, a session or a user:
// each class present only when something carried that count, and no object at
// all when none did (spec 031 #4, spec 049 #6). Absent rather than zero is the
// rule for cost and the rows behind it. Since spec 049 #5 a `user_id` answer
// carries it too: `users_hourly` rolls the classes as `stats_hourly` does.
func tokensObject(t store.Tokens) object {
	var out object
	for _, count := range []struct {
		key   string
		value *int64
	}{
		{"input", t.Input},
		{"output", t.Output},
		{"cache_read", t.CacheRead},
		{"reasoning", t.Reasoning},
		{"cache_write", t.CacheWrite},
	} {
		if count.value == nil {
			continue
		}
		if out == nil {
			out = object{}
		}
		out = out.put(count.key, *count.value)
	}
	return out
}

// liveSessions is the live half of `sessions` per bucket. It counts a session
// in the bucket its earliest trace of this user falls in — the same predicate
// the rollup counts by, so a session straddling the seam is counted once and
// on the same side both halves would put it (spec 023 #6).
//
// It takes the environment filter for the same reason: the rolled half reads
// `sessions_started` off the environment cell of the starting trace, so a live
// half that ignored it would count a different set of sessions past the
// watermark than before it — and, because a bucket exists as soon as anything
// is put in it, would invent a `count: 0, sessions: 1` bucket for a session
// whose traces the filter removed (found in review of PR #42).
func (s *Server) liveSessions(ctx context.Context, projectID string, filter store.StatsFilter, from, to int64, at func(string) *bucket) error {
	if !carriesSessions(filter) {
		return nil
	}
	return s.store.UserSessionStarts(ctx, projectID, filter.UserID, filter.Environment, from, to,
		func(start int64) {
			key := rollupKey(filter.GroupBy, store.StatsRow{Hour: store.HourOf(start)})
			if filter.GroupBy == store.GroupByMinute {
				// The one grouping finer than a rolled row, spelled as
				// the scan spells it.
				key = time.Unix(0, start).UTC().Format("2006-01-02T15:04:00Z")
			}
			at(key).sessions++
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
	case store.GroupByTotal:
		return ""
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
