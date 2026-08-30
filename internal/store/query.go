package store

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// The read-API queries (spec 004): the filtered trace listing, the session
// roll-up, the on-the-fly statistics scan and the span-id lookup behind
// `/observations/{id}/io`. Everything here is a read; nothing goes through the
// writer.

// TraceFilter narrows the trace listing (spec 004, API contract). The zero
// value lists a project's newest traces.
type TraceFilter struct {
	// From and To bound `timestamp` in Unix nanoseconds as a half-open
	// range — From inclusive, To exclusive — so walking a timeline a day
	// at a time never reports a trace twice. Nil is unbounded.
	From        *int64
	To          *int64
	Environment string
	UserID      string
	SessionID   string
	Name        string
	// Tags are ANDed: a trace matches only if it carries all of them.
	Tags []string
	// Status is "", TraceStatusError or TraceStatusOK.
	Status string
	// MinCost keeps traces whose total cost is at least this much. A trace
	// with no provided cost has no total cost and never matches (spec 002
	// #14: absent cost stays absent, it is not zero).
	MinCost *float64
	// Search keeps traces one of whose observations has a field matching
	// every word of the query — or whose own name does (spec 011 #5). It is
	// one more condition, not an ordering: rows stay newest first and the
	// cursors mean what they meant.
	Search *SearchQuery
	// Limit caps the rows returned; the caller asks for one more than the
	// page size to learn whether another page exists.
	Limit int
	// After continues a previous page. Nil starts at either end of the
	// listing, whichever end Backward names.
	After *TraceCursor
	// Backward pages towards the *newer* end of the listing: the keyset
	// comparison flips and the scan runs the same index the other way, so
	// the previous page costs what the next one does (spec 009 #2). With
	// no cursor it is the oldest page — which is how "jump to the end" is
	// a direction rather than an offset.
	//
	// Rows still come back newest first either way: the direction is how
	// the page was found, not how it is read.
	Backward bool
}

// Trace status filter values.
const (
	TraceStatusError = "error"
	TraceStatusOK    = "ok"
)

// TraceCursor is the keyset of the last row of a page. Pagination follows the
// sort key rather than an offset, so a trace ingested concurrently cannot make
// the next page skip or repeat a row (spec 004 #4).
type TraceCursor struct {
	Timestamp int64
	ID        string
}

// traceColumns is the row shape both the listing and the single-trace read
// scan. Payload columns are absent on purpose: a list row never carries a
// payload (spec 004, API contract).
const traceColumns = `project_id, id, name, user_id, session_id, environment, tags,
	        timestamp, total_cost, latency_ms, error_count, observation_count`

// traceConditions builds everything the filter says about *which* traces
// match, cursor excluded. Two callers need exactly this and disagree only
// about what follows it: the listing adds a keyset and a page, and the count
// adds neither (spec 009 #4).
func traceConditions(projectID string, filter TraceFilter) ([]string, []any) {
	where := []string{"project_id = ?"}
	args := []any{projectID}
	add := func(clause string, values ...any) {
		where = append(where, clause)
		args = append(args, values...)
	}
	if filter.From != nil {
		add("timestamp >= ?", *filter.From)
	}
	if filter.To != nil {
		add("timestamp < ?", *filter.To)
	}
	if filter.Environment != "" {
		add("environment = ?", filter.Environment)
	}
	if filter.UserID != "" {
		add("user_id = ?", filter.UserID)
	}
	if filter.SessionID != "" {
		add("session_id = ?", filter.SessionID)
	}
	if filter.Name != "" {
		add("name = ?", filter.Name)
	}
	for _, tag := range filter.Tags {
		// Tags are stored as a JSON array in one TEXT column (schema
		// 0002); json_each turns it back into rows for the membership
		// test. One EXISTS per tag is what makes repeated `?tag=` an
		// AND rather than an OR.
		add(`EXISTS (SELECT 1 FROM json_each(traces.tags) WHERE json_each.value = ?)`, tag)
	}
	switch filter.Status {
	case TraceStatusError:
		add("error_count > 0")
	case TraceStatusOK:
		add("error_count = 0")
	}
	if filter.MinCost != nil {
		add("total_cost >= ?", *filter.MinCost)
	}
	if filter.Search != nil {
		// The index is one table across every project, and the side
		// table's `project_id` is the only scope there is: a hit in
		// another project is discarded here and never returned. The
		// subquery materializes once per statement, so the outer scan
		// still seeks `idx_traces_timestamp` with the keyset (spec 011,
		// data contract).
		add(`id IN (SELECT trace_id FROM search_entries
		             WHERE project_id = ? AND id IN (
		               SELECT rowid FROM search_fts WHERE search_fts MATCH ?))`,
			projectID, filter.Search.Match)
	}
	return where, args
}

// traceQuery builds the listing statement and its arguments. It is a function
// of its own so that a test can hand the exact shipped SQL to EXPLAIN QUERY
// PLAN and assert the keyset seek (spec 004 #4, method of spec 003 #25) —
// in both directions, since a backward page is the same index read the other
// way and would be a sort if it were not (spec 009 #2).
func traceQuery(projectID string, filter TraceFilter) (string, []any) {
	where, args := traceConditions(projectID, filter)
	// The comparison and the order flip together: they are the same
	// statement about which way the page is being read.
	comparison, order := "<", "DESC"
	if filter.Backward {
		comparison, order = ">", "ASC"
	}
	if filter.After != nil {
		// A row-value comparison rather than the equivalent
		// `timestamp < ? OR (timestamp = ? AND id < ?)`: SQLite seeks
		// straight to the cursor with the former and scans from the
		// newest row with the latter (spec 003 #25).
		where = append(where, "(timestamp, id) "+comparison+" (?, ?)")
		args = append(args, filter.After.Timestamp, filter.After.ID)
	}
	args = append(args, filter.Limit)
	return `SELECT ` + traceColumns + `
	 FROM traces WHERE ` + strings.Join(where, " AND ") + `
	 ORDER BY timestamp ` + order + `, id ` + order + ` LIMIT ?`, args
}

// traceCountQuery counts the matches, stopping at `cap` of them. The subquery
// bounds the *answer*, and the scan with it only where matches are plentiful:
// `LIMIT` ends a scan once that many rows have matched, so a selective filter
// over an unindexed column still reads to the end (spec 009 #12). The listing
// beside it reads the same rows for the same reason.
func traceCountQuery(projectID string, filter TraceFilter, cap int) (string, []any) {
	where, args := traceConditions(projectID, filter)
	args = append(args, cap)
	return `SELECT COUNT(*) FROM (SELECT 1 FROM traces WHERE ` +
		strings.Join(where, " AND ") + ` LIMIT ?)`, args
}

// Traces lists a project's traces newest first.
func (s *Store) Traces(projectID string, filter TraceFilter) ([]*TraceRow, error) {
	query, args := traceQuery(projectID, filter)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list traces: %w", err)
	}
	defer rows.Close()

	var out []*TraceRow
	for rows.Next() {
		row, err := scanTrace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A backward page arrives oldest first, because that is the order the
	// index was read in. Every caller reads a listing newest first.
	if filter.Backward {
		slices.Reverse(out)
	}
	return out, nil
}

// CountTraces answers "how many match", stopping at `cap`: the count is `cap`
// exactly when there are at least that many, and the caller says "cap+" rather
// than pretending to know (spec 009 #4).
func (s *Store) CountTraces(projectID string, filter TraceFilter, cap int) (int, error) {
	query, args := traceCountQuery(projectID, filter, cap)
	var count int
	if err := s.db.QueryRow(query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count traces: %w", err)
	}
	return count, nil
}

// SessionRow is the roll-up over the traces of one session. Every aggregate is
// counted in traces, which is what a session is a collection of: `ErrorCount`
// is the number of traces that carry at least one failed observation, not the
// number of failed observations.
type SessionRow struct {
	ID         string
	TraceCount int
	TotalCost  *float64
	ErrorCount int
	FirstSeen  int64
	LastSeen   int64
}

// Session returns the roll-up, or nil when the project has no trace filed
// under that session id.
func (s *Store) Session(projectID, id string) (*SessionRow, error) {
	// Every aggregate but COUNT is NULL over an empty set, which is the
	// answer for a session id no trace ever named.
	var (
		row        = SessionRow{ID: id}
		totalCost  sql.NullFloat64
		errorCount sql.NullInt64
		firstSeen  sql.NullInt64
		lastSeen   sql.NullInt64
	)
	err := s.db.QueryRow(
		`SELECT COUNT(*), SUM(total_cost), SUM(error_count > 0), MIN(timestamp), MAX(timestamp)
		 FROM traces WHERE project_id = ? AND session_id = ?`, projectID, id).
		Scan(&row.TraceCount, &totalCost, &errorCount, &firstSeen, &lastSeen)
	if err != nil {
		return nil, fmt.Errorf("read session %s: %w", id, err)
	}
	if row.TraceCount == 0 {
		return nil, nil
	}
	if totalCost.Valid {
		row.TotalCost = &totalCost.Float64
	}
	row.ErrorCount = int(errorCount.Int64)
	row.FirstSeen, row.LastSeen = firstSeen.Int64, lastSeen.Int64
	return &row, nil
}

// SessionFilter narrows the session listing (spec 007 #2). The zero value
// lists every session of a project, most recently active first.
type SessionFilter struct {
	// From and To bound the *trace* timestamp, half-open like the trace
	// listing: a session appears when at least one of its traces falls in
	// the window, and its aggregates then describe those traces (spec 007
	// #10).
	From        *int64
	To          *int64
	Environment string
	UserID      string
	// Limit caps the rows returned; the caller asks for one more than the
	// page size to learn whether another page exists.
	Limit int
	// After continues a previous page. Nil starts at whichever end
	// Backward names.
	After *SessionCursor
	// Backward pages towards the more recently active end, exactly as it
	// does for traces (spec 009 #2). Rows still come back newest first.
	Backward bool
}

// SessionCursor is the keyset of the last row of a page — the pair the listing
// sorts by, exactly as TraceCursor is for traces (spec 004 #4).
type SessionCursor struct {
	LastSeen int64
	ID       string
}

// sessionQuery builds the listing statement and its arguments. A function of
// its own so that a test can hand the exact shipped SQL to EXPLAIN QUERY PLAN
// and assert that the grouping rides `idx_traces_session` rather than sorting
// the project's whole trace table (spec 003 #25's method).
//
// Sessions are aggregated from traces on every read rather than maintained at
// ingest (spec 007 #2): a session is a grouping of traces, not a stored
// entity, and a second source of truth is what a rollup table would be.
func sessionConditions(projectID string, filter SessionFilter) ([]string, []any) {
	// A trace that named no session is not a session of one (spec 007 #2).
	where := []string{"project_id = ?", "session_id IS NOT NULL"}
	args := []any{projectID}
	add := func(clause string, values ...any) {
		where = append(where, clause)
		args = append(args, values...)
	}
	if filter.From != nil {
		add("timestamp >= ?", *filter.From)
	}
	if filter.To != nil {
		add("timestamp < ?", *filter.To)
	}
	if filter.Environment != "" {
		add("environment = ?", filter.Environment)
	}
	if filter.UserID != "" {
		add("user_id = ?", filter.UserID)
	}
	return where, args
}

func sessionQuery(projectID string, filter SessionFilter) (string, []any) {
	where, args := sessionConditions(projectID, filter)
	comparison, order := "<", "DESC"
	if filter.Backward {
		comparison, order = ">", "ASC"
	}

	// The keyset is a HAVING rather than a WHERE because half of it is an
	// aggregate: `last_seen` exists only once the group is formed. The row
	// value keeps it one comparison, as in the trace listing.
	having := ""
	if filter.After != nil {
		having = " HAVING (MAX(timestamp), session_id) " + comparison + " (?, ?)"
		args = append(args, filter.After.LastSeen, filter.After.ID)
	}
	args = append(args, filter.Limit)
	return `SELECT session_id, COUNT(*), SUM(total_cost), SUM(error_count > 0),
	               MIN(timestamp), MAX(timestamp)
	 FROM traces WHERE ` + strings.Join(where, " AND ") + `
	 GROUP BY session_id` + having + `
	 ORDER BY MAX(timestamp) ` + order + `, session_id ` + order + ` LIMIT ?`, args
}

// sessionCountQuery counts the *sessions* a filter matches, capped. The
// grouping happens inside the subquery, so the cap bounds groups rather than
// traces — which is what the number on screen means.
//
// The cap bounds the answer, not the work, and here it bounds it least: the
// groups have to be formed before they can be counted, so a window narrow
// enough to make most sessions irrelevant is still walked in full (measured
// on 500k rows: 23 ms unfiltered against 719 ms inside a 1 % window). Spec
// 009 #12 is where that trade is written down rather than wished away.
func sessionCountQuery(projectID string, filter SessionFilter, cap int) (string, []any) {
	where, args := sessionConditions(projectID, filter)
	args = append(args, cap)
	return `SELECT COUNT(*) FROM (SELECT session_id FROM traces WHERE ` +
		strings.Join(where, " AND ") + ` GROUP BY session_id LIMIT ?)`, args
}

// Sessions lists a project's sessions, most recently active first.
func (s *Store) Sessions(projectID string, filter SessionFilter) ([]*SessionRow, error) {
	query, args := sessionQuery(projectID, filter)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var out []*SessionRow
	for rows.Next() {
		var (
			row        SessionRow
			totalCost  sql.NullFloat64
			errorCount sql.NullInt64
			firstSeen  sql.NullInt64
			lastSeen   sql.NullInt64
		)
		if err := rows.Scan(&row.ID, &row.TraceCount, &totalCost, &errorCount,
			&firstSeen, &lastSeen); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		// A cost nobody reported is absent, not zero (spec 002 #14).
		if totalCost.Valid {
			row.TotalCost = &totalCost.Float64
		}
		row.ErrorCount = int(errorCount.Int64)
		row.FirstSeen, row.LastSeen = firstSeen.Int64, lastSeen.Int64
		out = append(out, &row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if filter.Backward {
		slices.Reverse(out)
	}
	return out, nil
}

// CountSessions answers "how many sessions match", stopping at `cap`.
func (s *Store) CountSessions(projectID string, filter SessionFilter, cap int) (int, error) {
	query, args := sessionCountQuery(projectID, filter, cap)
	var count int
	if err := s.db.QueryRow(query, args...).Scan(&count); err != nil {
		return 0, fmt.Errorf("count sessions: %w", err)
	}
	return count, nil
}

// ObservationTraces returns the ids of the traces in which a span id appears,
// alphabetically. A span id is only unique within its trace (schema 0002), so
// `/observations/{id}/io` needs this to tell "one obvious answer" from "the
// caller has to say which trace" (spec 004, API contract).
func (s *Store) ObservationTraces(projectID, observationID string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT trace_id FROM observations
		 WHERE project_id = ? AND id = ? ORDER BY trace_id`, projectID, observationID)
	if err != nil {
		return nil, fmt.Errorf("resolve observation %s: %w", observationID, err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var traceID string
		if err := rows.Scan(&traceID); err != nil {
			return nil, err
		}
		out = append(out, traceID)
	}
	return out, rows.Err()
}

// Observation returns one span with its payloads resolved, or nil when the
// (trace, span) pair does not exist.
func (s *Store) Observation(projectID, traceID, id string) (*ObservationRow, error) {
	rows, err := s.db.Query(
		`SELECT `+observationColumns+`
		 FROM observations WHERE project_id = ? AND trace_id = ? AND id = ?`,
		projectID, traceID, id)
	if err != nil {
		return nil, fmt.Errorf("read observation %s: %w", id, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	return s.scanObservation(rows, WithIO)
}

// Statistics grouping (spec 004 #8). The first two group traces by when they
// happened, `environment` groups traces by where they ran, and `model` groups
// *observations*: a trace has no model, so that one dimension counts a
// different unit and the response says so (spec 004 Decision 23).
const (
	GroupByHour        = "hour"
	GroupByDay         = "day"
	GroupByModel       = "model"
	GroupByEnvironment = "environment"
)

// StatsFilter bounds the statistics scan. From/To always bound the *trace*
// timestamp, whatever the grouping — one rule for the time window is worth
// more than a per-grouping clock.
type StatsFilter struct {
	From        *int64
	To          *int64
	Environment string
	GroupBy     string
}

// StatsSample is one row of the scan: the bucket it falls in and the three
// things a bucket summarizes.
type StatsSample struct {
	Key       string
	Errored   bool
	Cost      *float64
	LatencyMs *int64
}

// StatsUnit reports what a bucket of this grouping counts.
func StatsUnit(groupBy string) string {
	if groupBy == GroupByModel {
		return "observation"
	}
	return "trace"
}

// StatsSamples scans the rows behind a statistics query and hands each to
// yield. Percentiles are computed exactly by the caller over what arrives
// here, rather than approximated in SQL (spec 004 #8).
func (s *Store) StatsSamples(projectID string, filter StatsFilter, yield func(StatsSample)) error {
	query, args := statsQuery(projectID, filter)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return fmt.Errorf("read stats: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			key     sql.NullString
			errored int
			cost    sql.NullFloat64
			latency sql.NullInt64
		)
		if err := rows.Scan(&key, &errored, &cost, &latency); err != nil {
			return fmt.Errorf("scan stats row: %w", err)
		}
		sample := StatsSample{Key: key.String, Errored: errored != 0}
		if cost.Valid {
			sample.Cost = &cost.Float64
		}
		if latency.Valid {
			sample.LatencyMs = &latency.Int64
		}
		yield(sample)
	}
	return rows.Err()
}

// statsQuery builds the scan. `timestamp` is Unix nanoseconds, so the bucket
// expressions divide before handing it to strftime, which speaks seconds.
func statsQuery(projectID string, filter StatsFilter) (string, []any) {
	where := []string{"t.project_id = ?"}
	args := []any{projectID}
	add := func(clause string, values ...any) {
		where = append(where, clause)
		args = append(args, values...)
	}
	if filter.From != nil {
		add("t.timestamp >= ?", *filter.From)
	}
	if filter.To != nil {
		add("t.timestamp < ?", *filter.To)
	}
	if filter.Environment != "" {
		add("t.environment = ?", filter.Environment)
	}

	if filter.GroupBy == GroupByModel {
		// The join exists for the filters, which are all trace-level;
		// the numbers come from the observation.
		add("o.model IS NOT NULL")
		add("o.model != ''")
		return `SELECT o.model, o.level = 'ERROR',
		               CASE WHEN o.provided_cost = 1
		                    THEN json_extract(o.cost_details, '$.total') END,
		               CASE WHEN o.start_time > 0 AND o.end_time >= o.start_time
		                    THEN (o.end_time - o.start_time) / 1000000 END
		        FROM observations o
		        JOIN traces t ON t.project_id = o.project_id AND t.id = o.trace_id
		        WHERE ` + strings.Join(where, " AND "), args
	}

	var key string
	switch filter.GroupBy {
	case GroupByHour:
		key = `strftime('%Y-%m-%dT%H:00:00Z', t.timestamp / 1000000000, 'unixepoch')`
	case GroupByDay:
		key = `strftime('%Y-%m-%d', t.timestamp / 1000000000, 'unixepoch')`
	default:
		key = `t.environment`
	}
	return `SELECT ` + key + `, t.error_count > 0, t.total_cost, t.latency_ms
	        FROM traces t WHERE ` + strings.Join(where, " AND "), args
}

// countedTables are the tables `GET /api/v1/system` reports row counts for, in
// the order they are reported (spec 004 #10). Every one of them is counted
// within the asking project.
//
// `payloads` is absent because it cannot be: the table has no project_id, and
// reporting it whole would tell one project how much data the others hold
// (spec 004 Decision 33). `projects` is reported as a plain count — how many
// tenants share this process is an operator fact, and it names none of them.
var countedTables = []string{
	"api_keys", "traces", "observations",
	"raw_batches", "scores", "prompts", "prompt_labels",
}

// TableCount is one table's row count.
type TableCount struct {
	Table string
	Rows  int64
}

// TableCounts counts one project's rows, in a fixed order so the answer is a
// stable diff between two calls.
func (s *Store) TableCounts(projectID string) ([]TableCount, error) {
	out := make([]TableCount, 0, len(countedTables)+1)
	for _, table := range countedTables {
		var rows int64
		// The table names are the package's own constants, never
		// anything a request carries; only the project id is bound.
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE project_id = ?`, projectID).
			Scan(&rows); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		out = append(out, TableCount{Table: table, Rows: rows})
	}
	// Live projects only: a soft-deleted one has vanished from every
	// listing, and a count that still included it would be the one place
	// the deletion did not take (spec 005 #9).
	var projects int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM projects WHERE deleted_at IS NULL`).
		Scan(&projects); err != nil {
		return nil, fmt.Errorf("count projects: %w", err)
	}
	return append(out, TableCount{Table: "projects", Rows: projects}), nil
}

// explainQueryPlan returns SQLite's plan for a statement, one line per step.
// Used by tests to assert that a keyset page seeks rather than scans.
func (s *Store) explainQueryPlan(query string, args ...any) ([]string, error) {
	rows, err := s.db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var (
			id, parent, notUsed int
			detail              string
		)
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			return nil, err
		}
		out = append(out, detail)
	}
	return out, rows.Err()
}
