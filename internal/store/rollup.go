package store

import (
	"database/sql"
	"fmt"
	"sort"
)

// The hourly rollup (spec 013): a few thousand rows that answer what a scan of
// millions used to, and that stay when the raw rows go.
//
// One table carries both units. A row with an empty model aggregates traces —
// the hour, day, environment and release groupings — and a row with a model
// aggregates observations, which is the unit the API already tells apart
// (spec 013 #1, spec 004 #23). The dimension tuple is the primary key, so a
// range read is one indexed scan and a re-roll is a delete and an insert of
// one hour of one project.

// SecondsPerHour is the rollup's grain.
const SecondsPerHour = 3600

// StatsRow is one dimension tuple of one hour, as stored and as read back.
type StatsRow struct {
	Hour        int64 // Unix seconds, top of the hour, UTC
	Environment string
	Release     string
	// Model is empty on a trace-unit row and set on an observation-unit
	// row; nothing else distinguishes the two (spec 013 #1).
	Model      string
	Count      int64
	ErrorCount int64
	// TotalCost is nil when nothing in this cell carried a cost, which is
	// not the same claim as zero (spec 002 #14).
	TotalCost *float64
	Latency   Histogram
}

// HourOf is the top of the hour a client timestamp falls in. The rollup
// buckets by the same clock the live scan does — `traces.timestamp` — so the
// two halves of an answer cannot disagree about which hour a trace is in.
func HourOf(timestampNanos int64) int64 {
	seconds := timestampNanos / 1e9
	if timestampNanos < 0 && timestampNanos%1e9 != 0 {
		seconds--
	}
	hour := seconds - seconds%SecondsPerHour
	if seconds < 0 && seconds%SecondsPerHour != 0 {
		hour -= SecondsPerHour
	}
	return hour
}

// RollupState is how far a project is rolled and when its last pass ran.
type RollupState struct {
	// RolledUntil is the watermark: hours strictly before it are answered
	// from the rollup, hours at or past it from the live scan (spec 013 #5).
	// Zero — the state of a project nobody has rolled yet — means every
	// query is live, which is what makes the first pass the backfill.
	RolledUntil int64
	// LastPass is the server clock (Unix ns) at the previous pass, the
	// cutoff that finds the hours late spans touched (spec 013 #4).
	LastPass int64
}

// RollupState reads a project's bookkeeping. A project nobody has rolled
// answers a zero state rather than an error: it has no history yet, and every
// caller treats that as "all live".
func (s *Store) RollupState(projectID string) (RollupState, error) {
	return rollupState(s.db, projectID)
}

type querier interface {
	QueryRow(string, ...any) *sql.Row
}

func rollupState(q querier, projectID string) (RollupState, error) {
	var state RollupState
	err := q.QueryRow(
		`SELECT rolled_until, last_pass FROM stats_rollup WHERE project_id = ?`,
		projectID).Scan(&state.RolledUntil, &state.LastPass)
	if err == sql.ErrNoRows {
		return RollupState{}, nil
	}
	if err != nil {
		return RollupState{}, fmt.Errorf("read the rollup state: %w", err)
	}
	return state, nil
}

// StatsRollupRows reads the rolled rows of a half-open hour range, oldest
// first. The read is by the primary key's leading columns, which is the whole
// reason the key is what it is.
//
// `environment` filters when set, for the same reason the live scan filters
// before it groups: a filter is not a grouping.
func (s *Store) StatsRollupRows(projectID string, fromHour, toHour int64, environment string, yield func(StatsRow)) error {
	query := `SELECT hour, environment, release, model, count, error_count, total_cost, latency
	          FROM stats_hourly
	          WHERE project_id = ? AND hour >= ? AND hour < ?`
	args := []any{projectID, fromHour, toHour}
	if environment != "" {
		query += ` AND environment = ?`
		args = append(args, environment)
	}
	query += ` ORDER BY hour`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return fmt.Errorf("read the rollup: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			row     StatsRow
			cost    sql.NullFloat64
			latency string
		)
		if err := rows.Scan(&row.Hour, &row.Environment, &row.Release, &row.Model,
			&row.Count, &row.ErrorCount, &cost, &latency); err != nil {
			return fmt.Errorf("scan a rollup row: %w", err)
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

// StatsRollupHours reports which hours of a project hold rolled rows. The
// tests and the system endpoint ask; the read path never needs to.
func (s *Store) StatsRollupHours(projectID string) ([]int64, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT hour FROM stats_hourly WHERE project_id = ? ORDER BY hour`, projectID)
	if err != nil {
		return nil, fmt.Errorf("read the rolled hours: %w", err)
	}
	defer rows.Close()
	var hours []int64
	for rows.Next() {
		var hour int64
		if err := rows.Scan(&hour); err != nil {
			return nil, err
		}
		hours = append(hours, hour)
	}
	return hours, rows.Err()
}

// statsRoll recomputes one `(project, hour)` from the raw rows: a delete and
// an insert, which is idempotent by construction. A delta would not be — spec
// 002 #22 refused deltas for the trace aggregates for the same reason, that
// re-delivery is routine and a delta double-counts every retried span
// (spec 013 #3).
type statsRoll struct {
	ProjectID string
	Hour      int64

	// Rows is how many dimension tuples the hour produced, for the log
	// line the aggregator writes.
	Rows int
}

func (r *statsRoll) apply(tx *sql.Tx) error {
	rows, err := rollHour(tx, r.ProjectID, r.Hour)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(
		`DELETE FROM stats_hourly WHERE project_id = ? AND hour = ?`,
		r.ProjectID, r.Hour); err != nil {
		return fmt.Errorf("clear hour %d: %w", r.Hour, err)
	}
	for _, row := range rows {
		latency, err := encodeHistogram(row.Latency)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO stats_hourly
			   (project_id, hour, environment, release, model,
			    count, error_count, total_cost, latency)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ProjectID, r.Hour, row.Environment, row.Release, row.Model,
			row.Count, row.ErrorCount, row.TotalCost, latency); err != nil {
			return fmt.Errorf("write a rollup row for hour %d: %w", r.Hour, err)
		}
	}
	r.Rows = len(rows)
	return nil
}

// rollHour aggregates one hour out of the raw tables. Both halves read the
// same rows the live scan reads and bucket them the same way, because the
// read seam is only invisible if they do (spec 013 #5).
func rollHour(tx *sql.Tx, projectID string, hour int64) ([]StatsRow, error) {
	from := hour * 1e9
	to := (hour + SecondsPerHour) * 1e9

	cells := map[string]*StatsRow{}
	cell := func(environment, release, model string) *StatsRow {
		key := environment + "\x00" + release + "\x00" + model
		row := cells[key]
		if row == nil {
			row = &StatsRow{Hour: hour, Environment: environment,
				Release: release, Model: model}
			cells[key] = row
		}
		return row
	}

	// The trace-unit rows: what the hour, day, environment and release
	// groupings count.
	traces, err := tx.Query(
		`SELECT environment, COALESCE(release, ''), error_count > 0, total_cost, latency_ms
		 FROM traces
		 WHERE project_id = ? AND timestamp >= ? AND timestamp < ?`,
		projectID, from, to)
	if err != nil {
		return nil, fmt.Errorf("read the hour's traces: %w", err)
	}
	if err := accumulate(traces, func(environment, release string) *StatsRow {
		return cell(environment, release, "")
	}); err != nil {
		return nil, err
	}

	// The observation-unit rows: what the model grouping counts. The join
	// is the live scan's — the filters are trace-level, the numbers are the
	// observation's.
	observations, err := tx.Query(
		`SELECT t.environment, COALESCE(t.release, ''), o.model,
		        o.level = 'ERROR',
		        CASE WHEN o.provided_cost = 1
		             THEN json_extract(o.cost_details, '$.total') END,
		        CASE WHEN o.start_time > 0 AND o.end_time >= o.start_time
		             THEN (o.end_time - o.start_time) / 1000000 END
		 FROM observations o
		 JOIN traces t ON t.project_id = o.project_id AND t.id = o.trace_id
		 WHERE o.project_id = ? AND t.timestamp >= ? AND t.timestamp < ?
		   AND o.model IS NOT NULL AND o.model != ''`,
		projectID, from, to)
	if err != nil {
		return nil, fmt.Errorf("read the hour's observations: %w", err)
	}
	defer observations.Close()
	for observations.Next() {
		var (
			environment, release, model string
			errored                     int
			cost                        sql.NullFloat64
			latency                     sql.NullInt64
		)
		if err := observations.Scan(&environment, &release, &model,
			&errored, &cost, &latency); err != nil {
			return nil, fmt.Errorf("scan an observation of the hour: %w", err)
		}
		add(cell(environment, release, model), errored != 0, cost, latency)
	}
	if err := observations.Err(); err != nil {
		return nil, err
	}

	out := make([]StatsRow, 0, len(cells))
	for _, row := range cells {
		out = append(out, *row)
	}
	// Insertion order decides nothing, but a stable one makes a failure
	// reproducible and the rows readable in a dump.
	sort.Slice(out, func(i, j int) bool {
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

// accumulate folds the trace rows of an hour into their cells.
func accumulate(rows *sql.Rows, cell func(environment, release string) *StatsRow) error {
	defer rows.Close()
	for rows.Next() {
		var (
			environment, release string
			errored              int
			cost                 sql.NullFloat64
			latency              sql.NullInt64
		)
		if err := rows.Scan(&environment, &release, &errored, &cost, &latency); err != nil {
			return fmt.Errorf("scan a trace of the hour: %w", err)
		}
		add(cell(environment, release), errored != 0, cost, latency)
	}
	return rows.Err()
}

// add is one sample into one cell, in the one place both units share it.
func add(row *StatsRow, errored bool, cost sql.NullFloat64, latency sql.NullInt64) {
	row.Count++
	if errored {
		row.ErrorCount++
	}
	if cost.Valid {
		total := cost.Float64
		if row.TotalCost != nil {
			total += *row.TotalCost
		}
		row.TotalCost = &total
	}
	if latency.Valid {
		row.Latency.Add(latency.Int64)
	}
}

// statsRollupAdvance moves a project's watermark and records the pass. The
// watermark never moves backwards: a pass that rolled less than the last one
// leaves it where it was (spec 013 #4).
type statsRollupAdvance struct {
	ProjectID   string
	RolledUntil int64
	LastPass    int64
}

func (a *statsRollupAdvance) apply(tx *sql.Tx) error {
	_, err := tx.Exec(
		`INSERT INTO stats_rollup (project_id, rolled_until, last_pass)
		 VALUES (?, ?, ?)
		 ON CONFLICT(project_id) DO UPDATE SET
		   rolled_until = MAX(rolled_until, excluded.rolled_until),
		   last_pass    = excluded.last_pass`,
		a.ProjectID, a.RolledUntil, a.LastPass)
	if err != nil {
		return fmt.Errorf("advance the rollup watermark: %w", err)
	}
	return nil
}
