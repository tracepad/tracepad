package store

import (
	"context"
	"database/sql"
	"fmt"
)

// DeleteRollBudget bounds what the rolls of one deletion chunk recompute (spec 047
// #2): the rows `stats_hourly` holds for the hours the chunk rolls, the
// traces and the observations with a model in them. A roll recomputes the
// project's whole hour, every user in it, so its cost follows the hour's
// density and not the deleted traces' share of it. A unit cost about 40 µs to
// recompute where it was measured, so this is some 200 ms of rolls a chunk
// (spec 047 #21).
const DeleteRollBudget = 5_000

// HourChunks cuts a run of traces, ordered by the hour each starts in (either
// direction), into chunks of whole hours (spec 047 #1, #2, #4). hours holds
// each trace's hour, and costs what rolling an hour recomputes, zero for an
// hour no roll touches. A chunk is at most limit traces and, past its first
// hour, at most budget of cost; it always takes its first hour, cut at limit
// when that hour alone holds more. It answers where each chunk ends.
func HourChunks(hours []int64, costs map[int64]int64, limit int, budget int64) []int {
	limit = max(limit, 1)
	var ends []int
	for start := 0; start < len(hours); {
		end, taken, cost := start, 0, int64(0)
		for end < len(hours) {
			next := end
			for next < len(hours) && hours[next] == hours[end] {
				next++
			}
			size, hourCost := next-end, costs[hours[end]]
			if taken == 0 {
				if size > limit {
					end = start + limit
					break
				}
			} else if taken+size > limit || cost+hourCost > budget {
				break
			}
			end, taken, cost = next, taken+size, cost+hourCost
		}
		ends = append(ends, end)
		start = end
	}
	return ends
}

// txOrDB is a database or a transaction, for a read either may run.
type txOrDB interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// rollCosts reads what rolling each of hours would recompute: the counts
// `stats_hourly` holds for it, for an hour before the watermark. An hour at or
// past it is not rolled and costs nothing (spec 023 #19). A frozen hour is
// counted like any other: the budget may overestimate a chunk, never
// underestimate it.
func rollCosts(ctx context.Context, q txOrDB, projectID string, hours []int64) (map[int64]int64, error) {
	state, err := rollupState(ctx, q, projectID)
	if err != nil {
		return nil, err
	}
	var rolled []any
	seen := map[int64]bool{}
	for _, hour := range hours {
		if hour < state.RolledUntil && !seen[hour] {
			seen[hour] = true
			rolled = append(rolled, hour)
		}
	}
	costs := map[int64]int64{}
	err = eachIn(rolled, func(batch []any) error {
		rows, err := q.QueryContext(ctx, rollCostsQuery(len(batch)), append([]any{projectID}, batch...)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var hour, cost int64
			if err := rows.Scan(&hour, &cost); err != nil {
				return err
			}
			costs[hour] = cost
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("read what rolling the hours costs: %w", err)
	}
	return costs, nil
}

// rollCostsQuery reads the counts of n hours by `stats_hourly`'s key.
func rollCostsQuery(n int) string {
	return `SELECT hour, SUM(count) FROM stats_hourly
		WHERE project_id = ? AND hour IN (` + placeholders(n) + `) GROUP BY hour`
}

// RollCosts is rollCosts for a caller outside a transaction: the rounds of a
// bulk trace deletion, which cut their chunks before submitting them.
func (s *Store) RollCosts(ctx context.Context, projectID string, hours []int64) (map[int64]int64, error) {
	return rollCosts(ctx, s.db, projectID, hours)
}
