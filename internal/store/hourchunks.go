package store

import (
	"context"
	"fmt"
)

// DeleteRollBudget bounds what one deletion chunk costs the writer (spec 047
// #2, #22): the rolls of its hours — their traces, a trace weighing
// traceRollWeight, and every observation they hold, the rows a whole-hour roll
// reads — and the deletion of its own traces (TraceDeleteCost). A roll
// recomputes the project's whole hour, every user in it, so its cost follows
// the hour's size and not the deleted traces' share of it. A unit cost about
// 4 µs where it was measured, and this kept a chunk's transaction under 250 ms
// at p99 on light traces and on agent runs of 200 spans alike (spec 047 #22).
const DeleteRollBudget = 30_000

// traceRollWeight is what a trace costs a roll against one observation:
// measured, about 94 µs against 4 (spec 047 #22). A trace is summed into the
// users', the sessions' and the names' cells as well as the hour's own, and an
// observation without a model is read and passed over.
const traceRollWeight = 25

// observationDeleteWeight is what deleting one of the chunk's own observations
// costs against a unit of the roll: about 13 µs, its row, its indexes and its
// payloads (spec 047 #22).
const observationDeleteWeight = 3

// TraceDeleteCost is what deleting one trace of a chunk costs the budget,
// beside the rolls of its hour: the trace's rows and each of its
// observations.
func TraceDeleteCost(observations int) int64 {
	return traceRollWeight + observationDeleteWeight*int64(observations)
}

// HourChunks cuts a run of traces, ordered by the hour each starts in (either
// direction), into chunks of whole hours (spec 047 #1, #2, #4, #22). hours
// holds each trace's hour and deletes what deleting it costs (TraceDeleteCost;
// nil is nothing), and cost answers what rolling an hour recomputes; it is
// asked only of the hours a chunk considers. A chunk always takes its first
// hour, cut at limit when that hour alone holds more traces; it takes each
// next hour only while the chunk stays within limit traces and its total
// cost — its hours' rolls and its traces' deletion, the first hour included —
// within budget. A budget of zero or less is DeleteRollBudget. It answers
// where each chunk ends, for at most chunks of them (all when zero or less):
// a caller that submits one chunk, or a round of fifty, has no use for the
// cost of the hours past them.
func HourChunks(hours, deletes []int64, cost func(hour int64) (int64, error), limit int, budget int64, chunks int) ([]int, error) {
	limit = max(limit, 1)
	if budget <= 0 {
		budget = DeleteRollBudget
	}
	var ends []int
	for start := 0; start < len(hours) && (chunks <= 0 || len(ends) < chunks); {
		end, taken, spent := start, 0, int64(0)
		for end < len(hours) {
			next := end
			for next < len(hours) && hours[next] == hours[end] {
				next++
			}
			size := next - end
			if taken == 0 && size > limit {
				// A first hour of more than a chunk is cut, whatever
				// it costs: the chunk ends in it.
				end = start + limit
				break
			}
			if taken > 0 && taken+size > limit {
				break
			}
			hourCost, err := cost(hours[end])
			if err != nil {
				return nil, err
			}
			if deletes != nil {
				for _, d := range deletes[end:next] {
					hourCost += d
				}
			}
			if taken > 0 && spent+hourCost > budget {
				break
			}
			end, taken, spent = next, taken+size, spent+hourCost
		}
		ends = append(ends, end)
		start = end
	}
	return ends, nil
}

// rollCosts answers what rolling an hour of a project would recompute, for
// HourChunks: its traces, weighed by traceRollWeight, and their observations,
// counted from the rows themselves, so an hour the aggregator has not seen yet costs what it holds.
// An hour at or past the watermark is not rolled (spec 023 #19) and costs
// nothing. The watermark is read once; each hour is counted when first asked.
func rollCosts(ctx context.Context, q ctxQuerier, projectID string) (func(hour int64) (int64, error), error) {
	state, err := rollupState(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	known := map[int64]int64{}
	return func(hour int64) (int64, error) {
		if hour >= state.RolledUntil {
			return 0, nil
		}
		if cost, ok := known[hour]; ok {
			return cost, nil
		}
		var cost int64
		if err := q.QueryRowContext(ctx, rollCostQuery, projectID,
			hour*1_000_000_000, (hour+SecondsPerHour)*1_000_000_000).Scan(&cost); err != nil {
			return 0, fmt.Errorf("count what rolling hour %d recomputes: %w", hour, err)
		}
		known[hour] = cost
		return cost, nil
	}, nil
}

// rollCostQuery weighs an hour's traces and their observations through
// `idx_traces_timestamp`, the range the roll itself reads.
var rollCostQuery = fmt.Sprintf(`SELECT %d * COUNT(*) + COALESCE(SUM(observation_count), 0) FROM traces
	WHERE project_id = ? AND timestamp >= ? AND timestamp < ?`, traceRollWeight)

// DeletionChunks is HourChunks for a caller outside a transaction: the rounds
// of a bulk trace deletion, which cut their chunks before submitting them.
func (s *Store) DeletionChunks(ctx context.Context, projectID string, hours, deletes []int64, limit int, budget int64, chunks int) ([]int, error) {
	cost, err := rollCosts(ctx, s.db, projectID)
	if err != nil {
		return nil, err
	}
	return HourChunks(hours, deletes, cost, limit, budget, chunks)
}
