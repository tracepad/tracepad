package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// The per-user rollup (spec 023): spec 013's hourly table one dimension over,
// and a summary row per user for the listing to sort and page.
//
// Everything here rides the aggregator of spec 013 — the same pass, the same
// watermark, the same freeze — because two aggregators would be two watermarks
// and two seams (#2). What is new is the tuple (`user_id` in it), the one
// number that is not a plain sum of the hour's rows (`sessions_started`, #1),
// and the summary table the listing needs to page without a scan (#3).

// UserStatsRow is one dimension tuple of one hour for one user: spec 013's row
// with the user on it, and — on trace-unit rows only — how many of that user's
// sessions began in the hour.
type UserStatsRow struct {
	StatsRow
	UserID string
	// SessionsStarted counts the sessions whose earliest trace of this user
	// falls in this hour. Counted at the start because a start sums exactly
	// over a range and a distinct count does not (spec 023 #1).
	SessionsStarted int64
}

// UserRow is the summary of one user, as the listing renders it and as the
// user page starts from. The two instants are *hours* — Unix seconds at the
// top of the hour — because that is the grain the rollup holds.
type UserRow struct {
	UserID     string
	Traces     int64
	ErrorCount int64
	// TotalCost is nil when nothing this user ran carried a cost, which is
	// not the claim that it was free (spec 002 #14).
	TotalCost *float64
	Sessions  int64
	FirstSeen int64
	LastSeen  int64
}

// UserSummary is a user with the latency the page reports beside the counts.
// The histogram is not in the summary table: it is merged out of the user's
// hourly rows, which is one indexed scan of one user (spec 023 #5).
type UserSummary struct {
	UserRow
	Latency Histogram
}

// The sorts the listing offers (spec 023 #5). Four, because they are the four
// questions a listing of users is opened with — recent, heavy, costly,
// failing — and ascending order of any of them is nobody's question.
const (
	UsersByLastSeen = "last_seen"
	UsersByTraces   = "traces"
	UsersByCost     = "cost"
	UsersByErrors   = "errors"
)

// UserSorts is every accepted value of `sort`, in the order the error message
// and `openapi.json` list them. The first is the default.
var UserSorts = []string{UsersByLastSeen, UsersByTraces, UsersByCost, UsersByErrors}

// userSortColumn is the column each sort reads. `cost` is the only nullable
// one, which is the whole reason the keyset below has two shapes.
var userSortColumn = map[string]string{
	UsersByLastSeen: "last_seen",
	UsersByTraces:   "traces",
	UsersByCost:     "total_cost",
	UsersByErrors:   "error_count",
}

// UserFilter narrows the user listing. The zero value lists a project's users
// by most recent activity.
type UserFilter struct {
	// Sort is one of UserSorts; empty means the default.
	Sort string
	// Prefix keeps ids starting with it, case-sensitively. A prefix rather
	// than a substring because an index answers a prefix and a substring is
	// a scan; the listing is not search (spec 023 #5).
	Prefix string
	Limit  int
	After  *UserCursor
	// Backward pages towards the head of the order, as every listing does
	// (spec 009 #2). Rows still come back in the listing's own order.
	Backward bool
}

// UserCursor is the keyset of the last row of a page: the sort key as text,
// and the id that breaks its ties.
//
// The key is text rather than a number because one of the four is nullable —
// a user with no costed trace has no cost — and the empty string is how that
// row names itself. Nothing else in the API needs a cursor that can carry an
// absence.
type UserCursor struct {
	Key    string
	UserID string
}

// UserCursorKey renders a row's sort key for the cursor that continues from
// it. The empty string means the row had no cost.
func UserCursorKey(sortBy string, row *UserRow) string {
	switch sortBy {
	case UsersByTraces:
		return strconv.FormatInt(row.Traces, 10)
	case UsersByErrors:
		return strconv.FormatInt(row.ErrorCount, 10)
	case UsersByCost:
		if row.TotalCost == nil {
			return ""
		}
		return strconv.FormatFloat(*row.TotalCost, 'g', -1, 64)
	default:
		return strconv.FormatInt(row.LastSeen, 10)
	}
}

// ParseUserCursorKey turns that text back into the value the query binds, or
// nil for the absent cost. It is where a hand-written cursor is refused.
func ParseUserCursorKey(sortBy, key string) (any, error) {
	if key == "" {
		if sortBy != UsersByCost {
			return nil, fmt.Errorf("invalid cursor")
		}
		return nil, nil
	}
	if sortBy == UsersByCost {
		value, err := strconv.ParseFloat(key, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid cursor")
		}
		return value, nil
	}
	value, err := strconv.ParseInt(key, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor")
	}
	return value, nil
}

const userColumns = `user_id, traces, error_count, total_cost, sessions, first_seen, last_seen`

// userConditions is everything the filter says about *which* users match,
// cursor excluded — the split spec 009 #4 made so that the listing and the
// count cannot disagree about what they are about.
func userConditions(projectID string, filter UserFilter) ([]string, []any) {
	where := []string{"project_id = ?"}
	args := []any{projectID}
	if filter.Prefix != "" {
		// A range rather than LIKE: SQLite's LIKE folds ASCII case by
		// default, and the spec asks for a case-sensitive prefix — and a
		// range is what an index on `user_id` can seek.
		where = append(where, "user_id >= ?")
		args = append(args, filter.Prefix)
		if upper, ok := prefixUpperBound(filter.Prefix); ok {
			where = append(where, "user_id < ?")
			args = append(args, upper)
		}
	}
	return where, args
}

// prefixUpperBound is the least string greater than every string carrying this
// prefix, and whether there is one at all (there is not when every byte is
// 0xFF, in which case the lower bound alone is the range).
func prefixUpperBound(prefix string) (string, bool) {
	raw := []byte(prefix)
	for i := len(raw) - 1; i >= 0; i-- {
		if raw[i] < 0xFF {
			raw[i]++
			return string(raw[:i+1]), true
		}
	}
	return "", false
}

// userSort is the sort in force, defaulted.
func userSort(filter UserFilter) string {
	if column, ok := userSortColumn[filter.Sort]; ok && column != "" {
		return filter.Sort
	}
	return UsersByLastSeen
}

// userQuery builds the listing statement. A function of its own so that a test
// can hand the exact shipped SQL to EXPLAIN QUERY PLAN and assert that each
// sort rides its own index (spec 003 #25's method), in both directions.
//
// The order is the key descending with the id ascending after it — which is
// the shape of the indexes, and the reason the keyset below is written out
// rather than expressed as a row value: `(key, id) < (?, ?)` would order the
// tie-break the other way and sort.
func userQuery(projectID string, filter UserFilter) (string, []any) {
	sortBy := userSort(filter)
	column := userSortColumn[sortBy]
	where, args := userConditions(projectID, filter)

	keyOrder, idOrder := "DESC", "ASC"
	if filter.Backward {
		keyOrder, idOrder = "ASC", "DESC"
	}
	if filter.After != nil {
		clause, values := userKeyset(column, sortBy, filter)
		where = append(where, clause)
		args = append(args, values...)
	}
	args = append(args, filter.Limit)
	return `SELECT ` + userColumns + `
	 FROM users WHERE ` + strings.Join(where, " AND ") + `
	 ORDER BY ` + column + ` ` + keyOrder + `, user_id ` + idOrder + ` LIMIT ?`, args
}

// userKeyset is "the rows after this one", in whichever direction the page is
// being read.
//
// A NULL key — only `cost` has one — sorts last in the descending order the
// listing reads, and SQLite agrees: NULL is smaller than every number, so
// `DESC` puts it at the end. So a page continuing past a real cost has to
// admit the costless tail explicitly, and a page continuing from *inside* that
// tail compares ids alone.
func userKeyset(column, sortBy string, filter UserFilter) (string, []any) {
	value, err := ParseUserCursorKey(sortBy, filter.After.Key)
	if err != nil {
		// The server refuses a malformed cursor before it gets here; a
		// clause that matches nothing is the safe reading if one ever does.
		return "0 = 1", nil
	}
	id := filter.After.UserID
	if value == nil {
		if filter.Backward {
			return "(" + column + " IS NOT NULL OR user_id < ?)", []any{id}
		}
		return "(" + column + " IS NULL AND user_id > ?)", []any{id}
	}
	if filter.Backward {
		// NULLs are behind this page, and `column > ?` already excludes
		// them: a comparison with NULL is not true.
		return "(" + column + " > ? OR (" + column + " = ? AND user_id < ?))",
			[]any{value, value, id}
	}
	forward := "(" + column + " < ? OR (" + column + " = ? AND user_id > ?)"
	if sortBy == UsersByCost {
		forward += " OR " + column + " IS NULL"
	}
	return forward + ")", []any{value, value, id}
}

// userCountQuery counts the matches, stopping at `cap`, exactly as the other
// listings do.
func userCountQuery(projectID string, filter UserFilter, cap int) (string, []any) {
	where, args := userConditions(projectID, filter)
	args = append(args, cap)
	return `SELECT COUNT(*) FROM (SELECT 1 FROM users WHERE ` +
		strings.Join(where, " AND ") + ` LIMIT ?)`, args
}

// Users lists a project's users in the order the filter asks for.
func (s *Store) Users(ctx context.Context, projectID string, filter UserFilter) ([]*UserRow, error) {
	query, args := userQuery(projectID, filter)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var out []*UserRow
	for rows.Next() {
		row, err := scanUserRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if filter.Backward {
		// A backward page arrives in the reverse of the listing's order,
		// because that is the order the index was read in.
		slices.Reverse(out)
	}
	return out, nil
}

// CountUsers answers "how many users match", stopping at `cap`.
func (s *Store) CountUsers(ctx context.Context, projectID string, filter UserFilter, cap int) (int, error) {
	query, args := userCountQuery(projectID, filter, cap)
	var count int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return count, nil
}

func scanUserRow(row scanner) (*UserRow, error) {
	var (
		out  UserRow
		cost sql.NullFloat64
	)
	if err := row.Scan(&out.UserID, &out.Traces, &out.ErrorCount, &cost,
		&out.Sessions, &out.FirstSeen, &out.LastSeen); err != nil {
		return nil, fmt.Errorf("scan a user: %w", err)
	}
	if cost.Valid {
		out.TotalCost = &cost.Float64
	}
	return &out, nil
}

// UserRollup is what the rollup holds about one user in the hours *before*
// `beforeHour`, or nil when it holds nothing there.
//
// The bound is the watermark, and it is the same bound `/api/v1/stats` puts on
// its rolled half (spec 013 #5). It matters because the rollup can hold a row
// for an hour the watermark has not reached: a user-data erasure re-rolls the
// hours it emptied *in the same request* (spec 013 #7), and one of those can be
// the hour in progress. Summing the table whole and then adding the live tail
// on top of it would count that hour twice, in the one place that adds the two
// halves together.
//
// It reads the hourly rows rather than the `users` summary for the same
// reason: the summary is the listing's keyset — one row per user with the sort
// key on it (spec 023 #3) — and it has no notion of a watermark. One user's
// hours are an index seek, which is what the page can afford and the listing
// cannot.
func (s *Store) UserRollup(ctx context.Context, projectID, userID string, beforeHour int64) (*UserSummary, error) {
	rows, err := s.db.QueryContext(ctx,
		// Trace-unit rows only: an observation row is one model of one of
		// those traces, and summing both would count every trace twice.
		`SELECT hour, count, error_count, total_cost, latency, sessions_started
		 FROM users_hourly
		 WHERE project_id = ? AND user_id = ? AND hour < ? AND model = ''
		 ORDER BY hour`, projectID, userID, beforeHour)
	if err != nil {
		return nil, fmt.Errorf("read a user's rolled hours: %w", err)
	}
	defer rows.Close()

	summary := &UserSummary{UserRow: UserRow{UserID: userID}}
	var (
		held      bool
		totalCost CostSum
	)
	for rows.Next() {
		var (
			hour, count, errored, sessions int64
			cost                           sql.NullFloat64
			latency                        string
		)
		if err := rows.Scan(&hour, &count, &errored, &cost, &latency, &sessions); err != nil {
			return nil, fmt.Errorf("scan a user's rolled hour: %w", err)
		}
		if !held {
			summary.FirstSeen, held = hour, true
		}
		summary.LastSeen = hour
		summary.Traces += count
		summary.ErrorCount += errored
		summary.Sessions += sessions
		if cost.Valid {
			totalCost.Add(cost.Float64)
		}
		hist, err := decodeHistogram(latency)
		if err != nil {
			return nil, err
		}
		summary.Latency.Merge(hist)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !held {
		return nil, nil
	}
	summary.TotalCost = totalCost.Pointer()
	return summary, nil
}

// UserSummaryRow is the stored summary of one user, or nil when there is none.
// The listing's own row, read by id — what the tests check the recompute
// against, and nothing on the read path uses.
func (s *Store) UserSummaryRow(ctx context.Context, projectID, userID string) (*UserRow, error) {
	row, err := scanUserRow(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE project_id = ? AND user_id = ?`,
		projectID, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return row, err
}

// UserTail is what the raw rows say about one user from an instant onwards:
// the live half of the user page (spec 023 #4). `Traces` is zero when the tail
// holds nothing of theirs, which is how the caller tells "not there" from
// "there, quietly".
//
// The instants here are exact rather than hourly, which is the point: a user
// first seen minutes ago is not in the listing yet, and this page still says
// when.
func (s *Store) UserTail(ctx context.Context, projectID, userID string, fromNanos int64) (*UserSummary, error) {
	summary := &UserSummary{UserRow: UserRow{UserID: userID}}
	rows, err := s.db.QueryContext(ctx,
		`SELECT error_count > 0, total_cost, latency_ms, timestamp
		 FROM traces
		 WHERE project_id = ? AND user_id = ? AND timestamp >= ?`,
		projectID, userID, fromNanos)
	if err != nil {
		return nil, fmt.Errorf("read a user's live tail: %w", err)
	}
	defer rows.Close()
	var totalCost CostSum
	for rows.Next() {
		var (
			errored   int
			cost      sql.NullFloat64
			latency   sql.NullInt64
			timestamp int64
		)
		if err := rows.Scan(&errored, &cost, &latency, &timestamp); err != nil {
			return nil, fmt.Errorf("scan a tail trace: %w", err)
		}
		summary.Traces++
		if errored != 0 {
			summary.ErrorCount++
		}
		if cost.Valid {
			totalCost.Add(cost.Float64)
		}
		if latency.Valid {
			summary.Latency.Add(latency.Int64)
		}
		if summary.FirstSeen == 0 || timestamp < summary.FirstSeen {
			summary.FirstSeen = timestamp
		}
		if timestamp > summary.LastSeen {
			summary.LastSeen = timestamp
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	summary.TotalCost = totalCost.Pointer()
	if summary.Traces == 0 {
		return summary, nil
	}
	// The same predicate the rollup counts by, so that the two halves cannot
	// count one session twice: a session belongs to the hour its earliest
	// trace of this user is in, wherever the rest of it falls.
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM traces t
		 WHERE `+sessionStartCondition+` AND t.user_id = ? AND t.timestamp >= ?`,
		projectID, userID, fromNanos).Scan(&summary.Sessions); err != nil {
		return nil, fmt.Errorf("count a user's tail sessions: %w", err)
	}
	return summary, nil
}

// sessionStartCondition keeps exactly the traces that *begin* one of a user's
// sessions: the earliest trace that user filed under that session id, ties
// broken by trace id so that two traces sharing an instant cannot both count.
//
// It takes the project id as its one placeholder.
//
// The unary `+` in front of the comparison is load-bearing and is SQLite's own
// mechanism for it: it makes the expression unusable as an index key, which is
// the point. Without it the planner takes the `OR` apart into a MULTI-INDEX OR
// over `idx_traces_timestamp` and walks **every older trace in the project**
// for each candidate — because nothing here ever runs `ANALYZE`, so a shipped
// database has no `sqlite_stat1` and the planner has no way to know that
// `session_id = ?` is the selective term and `timestamp < ?` is half the table.
// Measured on the real schema at 20k traces, one hour of this query: 13.35 s
// without the `+`, 0.01 s with it. `TestSessionStartSeeksTheSessionIndex`
// asserts the plan so that "simplifying" it away fails a test rather than a
// deployment (found in review of PR #42).
//
// The `+` on `x.user_id` settles the other choice the subquery offers:
// `idx_traces_user` and `idx_traces_session` both answer its two equalities,
// and with no statistics SQLite breaks the tie by the order the indexes were
// created in. Migration 0029 recreated the first after the second, which
// flipped it (spec 047 #1); the session is the narrower of the two.
const sessionStartCondition = `t.project_id = ?
	   AND t.user_id IS NOT NULL AND t.user_id != '' AND t.session_id IS NOT NULL
	   AND NOT EXISTS (
	         SELECT 1 FROM traces x
	          WHERE x.project_id = t.project_id AND x.session_id = t.session_id
	            AND +x.user_id = t.user_id
	            AND +(x.timestamp < t.timestamp
	                  OR (x.timestamp = t.timestamp AND x.id < t.id)))`

// UserSessionStarts yields the instant each of a user's sessions began, within
// a half-open range. It is what puts `sessions` on the live half of a
// `/stats?user_id=` answer (spec 023 #6), and it is the same predicate the
// rollup counts by, so the seam does not double-count a session.
//
// `environment` filters the *starting* trace, which is where the rollup files
// the count: `sessions_started` sits on the `(user, environment, release)` cell
// of the trace that began the session. Filtering it anywhere else — or, as this
// first shipped, not at all — makes the two halves of one answer count
// different things, and the step is at the watermark (found in review of
// PR #42). Whether a session *started* is still decided over the whole of it:
// a session that began in staging and continued in production began in
// staging, on both sides of the seam.
func (s *Store) UserSessionStarts(ctx context.Context, projectID, userID string, environment []string, from, to int64, yield func(int64)) error {
	query := `SELECT t.timestamp FROM traces t
	          WHERE ` + sessionStartCondition + `
	            AND t.user_id = ? AND t.timestamp >= ? AND t.timestamp < ?`
	args := []any{projectID, userID, from, to}
	if clause, bound := matchAny("t.environment", environment); clause != "" {
		query += ` AND ` + clause
		args = append(args, bound...)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read a user's session starts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var at int64
		if err := rows.Scan(&at); err != nil {
			return err
		}
		yield(at)
	}
	return rows.Err()
}

// UsersRollupRows reads one user's rolled rows over a half-open hour range,
// oldest first — the rolled half of `/stats?user_id=` (spec 023 #6). It is
// `StatsRollupRows` with the user in the seek, riding `idx_users_hourly_user`.
func (s *Store) UsersRollupRows(ctx context.Context, projectID, userID string, fromHour, toHour int64,
	environment []string, yield func(UserStatsRow)) error {
	query := `SELECT hour, environment, release, model, count, error_count,
	                 total_cost, latency, sessions_started
	          FROM users_hourly
	          WHERE project_id = ? AND user_id = ? AND hour >= ? AND hour < ?`
	args := []any{projectID, userID, fromHour, toHour}
	if clause, bound := matchAny("environment", environment); clause != "" {
		query += ` AND ` + clause
		args = append(args, bound...)
	}
	query += ` ORDER BY hour`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read the user rollup: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			row     = UserStatsRow{UserID: userID}
			cost    sql.NullFloat64
			latency string
		)
		if err := rows.Scan(&row.Hour, &row.Environment, &row.Release, &row.Model,
			&row.Count, &row.ErrorCount, &cost, &latency, &row.SessionsStarted); err != nil {
			return fmt.Errorf("scan a user rollup row: %w", err)
		}
		if cost.Valid {
			row.TotalCost = &cost.Float64
		}
		if row.Latency, err = decodeHistogram(latency); err != nil {
			return err
		}
		yield(row)
	}
	return rows.Err()
}

// UsersRollupHours reports which hours of a project hold per-user rows. The
// tests ask; nothing on the read path needs it.
func (s *Store) UsersRollupHours(ctx context.Context, projectID string) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT hour FROM users_hourly WHERE project_id = ? ORDER BY hour`, projectID)
	if err != nil {
		return nil, fmt.Errorf("read the rolled user hours: %w", err)
	}
	return scanHours(rows)
}

// --- the aggregation ------------------------------------------------------

// rollUserHour aggregates one hour into the per-user tuples. It reads the same
// rows `rollHour` does, with the traces that named no user left out: the
// listing is of users, and a trace with no user id belongs to no row here
// (spec 023 #1).
func rollUserHour(tx *sql.Tx, projectID string, hour int64) ([]UserStatsRow, error) {
	from := hour * 1e9
	to := (hour + SecondsPerHour) * 1e9

	cells := map[string]*UserStatsRow{}
	cell := func(user, environment, release, model string) *UserStatsRow {
		key := user + "\x00" + environment + "\x00" + release + "\x00" + model
		row := cells[key]
		if row == nil {
			row = &UserStatsRow{
				StatsRow: StatsRow{Hour: hour, Environment: environment,
					Release: release, Model: model},
				UserID: user,
			}
			cells[key] = row
		}
		return row
	}

	// The trace-unit rows.
	traces, err := tx.Query(
		`SELECT user_id, environment, COALESCE(release, ''), error_count > 0,
		        total_cost, latency_ms
		 FROM traces
		 WHERE project_id = ? AND timestamp >= ? AND timestamp < ?
		   AND user_id IS NOT NULL AND user_id != ''`,
		projectID, from, to)
	if err != nil {
		return nil, fmt.Errorf("read the hour's traces by user: %w", err)
	}
	if err := accumulateUsers(traces, func(user, environment, release string) *UserStatsRow {
		return cell(user, environment, release, "")
	}); err != nil {
		return nil, err
	}

	// The observation-unit rows: the same join the statistics rollup makes,
	// with the user carried down from the trace.
	observations, err := tx.Query(
		`SELECT t.user_id, t.environment, COALESCE(t.release, ''), o.model,
		        o.level = 'ERROR',
		        CASE WHEN o.provided_cost = 1
		             THEN `+costExpr("o.cost_details")+` END,
		        CASE WHEN o.start_time > 0 AND o.end_time >= o.start_time
		             THEN (o.end_time - o.start_time) / 1000000 END
		 FROM observations o
		 JOIN traces t ON t.project_id = o.project_id AND t.id = o.trace_id
		 WHERE o.project_id = ? AND t.timestamp >= ? AND t.timestamp < ?
		   AND t.user_id IS NOT NULL AND t.user_id != ''
		   AND o.model IS NOT NULL AND o.model != ''`,
		projectID, from, to)
	if err != nil {
		return nil, fmt.Errorf("read the hour's observations by user: %w", err)
	}
	defer observations.Close()
	for observations.Next() {
		var (
			user, environment, release, model string
			errored                           int
			cost                              sql.NullFloat64
			latency                           sql.NullInt64
		)
		if err := observations.Scan(&user, &environment, &release, &model,
			&errored, &cost, &latency); err != nil {
			return nil, fmt.Errorf("scan an observation of the hour by user: %w", err)
		}
		add(&cell(user, environment, release, model).StatsRow, errored != 0, cost, latency)
	}
	if err := observations.Err(); err != nil {
		return nil, err
	}

	// `sessions_started`, on the trace-unit rows only: it is a count of
	// sessions, and an observation row would be counting the same session
	// once per model (spec 023 #1).
	starts, err := tx.Query(
		`SELECT t.user_id, t.environment, COALESCE(t.release, ''), COUNT(*)
		 FROM traces t
		 WHERE `+sessionStartCondition+`
		   AND t.timestamp >= ? AND t.timestamp < ?
		 GROUP BY t.user_id, t.environment, COALESCE(t.release, '')`,
		projectID, from, to)
	if err != nil {
		return nil, fmt.Errorf("count the hour's session starts: %w", err)
	}
	defer starts.Close()
	for starts.Next() {
		var (
			user, environment, release string
			started                    int64
		)
		if err := starts.Scan(&user, &environment, &release, &started); err != nil {
			return nil, fmt.Errorf("scan a session start: %w", err)
		}
		// The cell exists already — a session that started here has a
		// trace here — but going through `cell` keeps that a fact rather
		// than an assumption.
		cell(user, environment, release, "").SessionsStarted = started
	}
	if err := starts.Err(); err != nil {
		return nil, err
	}

	out := make([]UserStatsRow, 0, len(cells))
	for _, row := range cells {
		out = append(out, *row)
	}
	// A stable order makes a failure reproducible and a dump readable.
	sort.Slice(out, func(i, j int) bool {
		if out[i].UserID != out[j].UserID {
			return out[i].UserID < out[j].UserID
		}
		if out[i].Environment != out[j].Environment {
			return out[i].Environment < out[j].Environment
		}
		if out[i].Release != out[j].Release {
			return out[i].Release < out[j].Release
		}
		return out[i].Model < out[j].Model
	})
	return out, nil
}

func accumulateUsers(rows *sql.Rows, cell func(user, environment, release string) *UserStatsRow) error {
	defer rows.Close()
	for rows.Next() {
		var (
			user, environment, release string
			errored                    int
			cost                       sql.NullFloat64
			latency                    sql.NullInt64
		)
		if err := rows.Scan(&user, &environment, &release, &errored, &cost, &latency); err != nil {
			return fmt.Errorf("scan a trace of the hour by user: %w", err)
		}
		add(&cell(user, environment, release).StatsRow, errored != 0, cost, latency)
	}
	return rows.Err()
}

// writeUserHour replaces one `(project, hour)` of `users_hourly` and hands
// back every user id the hour holds or held — which is exactly the set whose
// summary the pass has to recompute (spec 023 #3).
func writeUserHour(tx *sql.Tx, projectID string, hour int64, rows []UserStatsRow) ([]string, error) {
	touched := map[string]bool{}
	before, err := tx.Query(
		`SELECT DISTINCT user_id FROM users_hourly WHERE project_id = ? AND hour = ?`,
		projectID, hour)
	if err != nil {
		return nil, fmt.Errorf("read the hour's users: %w", err)
	}
	for before.Next() {
		var id string
		if err := before.Scan(&id); err != nil {
			before.Close()
			return nil, err
		}
		touched[id] = true
	}
	before.Close()
	if err := before.Err(); err != nil {
		return nil, err
	}

	if _, err := tx.Exec(
		`DELETE FROM users_hourly WHERE project_id = ? AND hour = ?`,
		projectID, hour); err != nil {
		return nil, fmt.Errorf("clear the users of hour %d: %w", hour, err)
	}
	// Prepared once for the hour, not once per row. This is the one place in
	// the rollup where the row count is multiplied by the *user* count — an
	// hour of fifty users is fifty times the rows `stats_hourly` writes for
	// it — so the per-statement overhead is fifty times as visible, and it
	// was most of what one pass cost (measured on the month fixture).
	insert, err := tx.Prepare(
		`INSERT INTO users_hourly
		   (project_id, hour, user_id, environment, release, model,
		    count, error_count, total_cost, latency, sessions_started)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return nil, fmt.Errorf("prepare the user rollup insert: %w", err)
	}
	defer insert.Close()
	for _, row := range rows {
		latency, err := encodeHistogram(row.Latency)
		if err != nil {
			return nil, err
		}
		if _, err := insert.Exec(
			projectID, hour, row.UserID, row.Environment, row.Release, row.Model,
			row.Count, row.ErrorCount, row.TotalCost, latency, row.SessionsStarted); err != nil {
			return nil, fmt.Errorf("write a user rollup row for hour %d: %w", hour, err)
		}
		touched[row.UserID] = true
	}

	ids := make([]string, 0, len(touched))
	for id := range touched {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// summaryChunk bounds one recompute statement: how many user ids are named in
// one `IN (…)`, and with it how much work one transaction does. SQLite's own
// parameter limit is far higher; this is about the transaction, not the limit.
const summaryChunk = 400

// recomputeUsers rebuilds the summary rows of the named users out of their
// hourly rows — a recompute rather than a delta, for spec 013 #3's reason, and
// bounded to the users the caller actually touched (spec 023 #3).
//
// Delete then insert, exactly as one hour of the rollup is written: a user with
// no hourly rows left keeps no summary row, which is what the retention sweep
// taking their last hour has to produce, and a row over nothing would be a user
// the listing shows and the page cannot explain.
//
// One grouped statement per chunk rather than one seek per user: a pass that
// touched a thousand users would otherwise be a thousand round trips over the
// same index.
func recomputeUsers(tx *sql.Tx, projectID string, userIDs []string) error {
	for chunk := range slices.Chunk(userIDs, summaryChunk) {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, projectID)
		for _, id := range chunk {
			args = append(args, id)
		}
		list := "(?" + strings.Repeat(", ?", len(chunk)-1) + ")"

		if _, err := tx.Exec(
			`DELETE FROM users WHERE project_id = ? AND user_id IN `+list, args...); err != nil {
			return fmt.Errorf("clear the summaries of %d users: %w", len(chunk), err)
		}
		// `model = ''` is the trace-unit half: an observation row is one
		// model of one of those traces, and summing both would count every
		// trace twice over.
		if _, err := tx.Exec(
			`INSERT INTO users
			   (project_id, user_id, traces, error_count, total_cost,
			    sessions, first_seen, last_seen)
			 SELECT project_id, user_id, SUM(count), SUM(error_count),
			        SUM(total_cost), SUM(sessions_started), MIN(hour), MAX(hour)
			 FROM users_hourly
			 WHERE project_id = ? AND model = '' AND user_id IN `+list+`
			 GROUP BY project_id, user_id`, args...); err != nil {
			return fmt.Errorf("write the summaries of %d users: %w", len(chunk), err)
		}
	}
	return nil
}

// usersSummary is the recompute as a job of its own, which is how the
// aggregator does it **once per pass** rather than once per hour (spec 023 #3:
// "for every user whose hours it rolled").
//
// Doing it inside each hour's job made a backfill quadratic: a user active in
// five hundred rolled hours had their whole history summed five hundred times
// in one pass. Measured on the month fixture, that was 47 s a pass against
// main's 1.4 s; batched here it is back inside the noise (found by the
// measurement spec 023's Testing asks for).
type usersSummary struct {
	ProjectID string
	UserIDs   []string
}

func (s *usersSummary) apply(tx *sql.Tx) error {
	return recomputeUsers(tx, s.ProjectID, s.UserIDs)
}

// RecomputeUserSummaries is that job, for the aggregator.
func RecomputeUserSummaries(projectID string, userIDs []string) WriteJob {
	return &usersSummary{ProjectID: projectID, UserIDs: userIDs}
}

// deleteUserRollup removes everything the two tables hold about one user. It
// is the erasure's second half (spec 023 #10): the per-user rows are *about*
// the user, so they are deleted outright rather than re-rolled — a re-roll
// would recompute them to nothing from raw rows that are gone, and for a
// frozen hour (spec 013 #11) could not recompute them at all.
func deleteUserRollup(tx *sql.Tx, projectID, userID string) (int64, error) {
	hours, err := tx.Exec(
		`DELETE FROM users_hourly WHERE project_id = ? AND user_id = ?`,
		projectID, userID)
	if err != nil {
		return 0, fmt.Errorf("erase the user's rolled hours: %w", err)
	}
	summary, err := tx.Exec(
		`DELETE FROM users WHERE project_id = ? AND user_id = ?`,
		projectID, userID)
	if err != nil {
		return 0, fmt.Errorf("erase the user's summary: %w", err)
	}
	// The count decides whether the erasure asks for a compaction (spec 044
	// #17), so a count that cannot be read is an error, not a zero.
	removedHours, err := hours.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count the user's erased hours: %w", err)
	}
	removedSummary, err := summary.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count the user's erased summary: %w", err)
	}
	return removedHours + removedSummary, nil
}

// usersRollupSweep deletes rolled per-user rows older than the project's stats
// window and recomputes the summaries of the users it touched, so that
// `first_seen` moves forward with the history and a user whose last row went
// leaves the listing (spec 023, edge cases).
//
// One chunk per job, like every other deletion this store does.
type usersRollupSweep struct {
	ProjectID string
	Before    int64

	Deleted int64
}

func (s *usersRollupSweep) apply(tx *sql.Tx) error {
	// Which users the chunk is about has to be read before it is deleted:
	// afterwards the rows that named them are gone.
	rows, err := tx.Query(
		`SELECT DISTINCT user_id FROM users_hourly
		 WHERE rowid IN (SELECT rowid FROM users_hourly
		                 WHERE project_id = ? AND hour < ? LIMIT ?)`,
		s.ProjectID, s.Before, DefaultSweepChunk)
	if err != nil {
		return fmt.Errorf("find the users of a swept chunk: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	result, err := tx.Exec(
		`DELETE FROM users_hourly
		 WHERE rowid IN (SELECT rowid FROM users_hourly
		                 WHERE project_id = ? AND hour < ? LIMIT ?)`,
		s.ProjectID, s.Before, DefaultSweepChunk)
	if err != nil {
		return fmt.Errorf("sweep the user rollup: %w", err)
	}
	if s.Deleted, err = result.RowsAffected(); err != nil {
		return err
	}
	return recomputeUsers(tx, s.ProjectID, ids)
}
