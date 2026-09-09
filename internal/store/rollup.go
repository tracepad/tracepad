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
func (s *Store) StatsRollupRows(projectID string, fromHour, toHour int64, environment []string, yield func(StatsRow)) error {
	query := `SELECT hour, environment, release, model, count, error_count, total_cost, latency
	          FROM stats_hourly
	          WHERE project_id = ? AND hour >= ? AND hour < ?`
	args := []any{projectID, fromHour, toHour}
	if clause, bound := matchAny("environment", environment); clause != "" {
		query += ` AND ` + clause
		args = append(args, bound...)
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

// OldestRolledHour is the oldest hour the rollup actually holds for a
// project, and whether it holds anything at all.
//
// The read seam asks this rather than deriving a floor from
// `stats_retention_days`, because the window can be lengthened and the sweep
// cannot be undone: a project swept to one day and then set back to "keep
// forever" would otherwise have the seam trusting the rollup for hours whose
// rows are gone, permanently and silently (spec 013 #17, found in review of
// PR #28). The table itself is the only honest answer to "how far back can
// you speak for".
//
// It is a `MIN` over the primary key's leading columns, so it costs an index
// seek.
func (s *Store) OldestRolledHour(projectID string) (int64, bool, error) {
	var oldest sql.NullInt64
	if err := s.db.QueryRow(
		`SELECT MIN(hour) FROM stats_hourly WHERE project_id = ?`, projectID).
		Scan(&oldest); err != nil {
		return 0, false, fmt.Errorf("read the oldest rolled hour: %w", err)
	}
	return oldest.Int64, oldest.Valid, nil
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
	// Now is the clock the freeze of Decision 11 is measured against
	// (Unix nanoseconds).
	Now int64

	// Rows is how many dimension tuples the hour produced, for the log
	// line the aggregator writes.
	Rows int
	// UserRows is the same count for `users_hourly`, which the same job
	// writes in the same transaction (spec 023 #2).
	UserRows int
	// UsersRolled reports that `users_hourly` was written — its own freeze
	// let the hour through (spec 026 #7) — and with it that the summaries of
	// `Touched` are a sum that has changed.
	UsersRolled bool
	// ScoreRows is the same count for `scores_hourly`, the third table the
	// job writes (spec 025 #3).
	ScoreRows int
	// NameRows is the same count for `names_hourly`, the fourth (spec 027
	// #3).
	NameRows int
	// Touched are the user ids this hour holds or held — the set whose
	// summary has to be recomputed (spec 023 #3).
	Touched []string
	// DeferSummary hands that recompute back to the caller instead of doing
	// it here. The aggregator sets it and recomputes once per *pass*: a user
	// active in five hundred rolled hours would otherwise have their whole
	// history summed five hundred times in one backfill. A caller that
	// corrects a single hour and then answers — the user-data erasure — does
	// not set it, because there is no pass to defer to.
	DeferSummary bool
	// Frozen reports that `stats_hourly` was left alone because retention
	// has taken the raw rows it would have been recomputed from. It is the
	// statistics' own answer since spec 026 #7 gave each table its own: it
	// is what the aggregator counts as an hour rolled, and what its log line
	// has always meant.
	Frozen bool
}

// RollHour is the job that recomputes one `(project, hour)`, for the callers
// outside this package that have to correct an hour before they answer — the
// user-data erasure of spec 005 #7, which spec 013 #7 makes re-roll the hours
// it emptied.
func RollHour(projectID string, hour, now int64) WriteJob {
	return &statsRoll{ProjectID: projectID, Hour: hour, Now: now}
}

func (r *statsRoll) apply(tx *sql.Tx) error {
	// The freeze is read inside the transaction that would act on it,
	// which is the only reading that can gate a write (spec 003 #20). It
	// lives here rather than in the aggregator so that every caller —
	// a pass, an erasure — obeys one rule in one place.
	project, err := projectByID(tx, r.ProjectID)
	if err != nil {
		return err
	}
	if project == nil {
		return nil
	}
	// Past the retention window, the freeze is asked of **each table this job
	// writes**, because spec 013 #14's rule is about the rows being protected
	// and every table has its own (spec 025 #21, spec 026 #7).
	past := hourPastWindow(project, r.Hour, r.Now)
	if err := r.rollTraffic(tx, past); err != nil {
		return err
	}

	// The third table's own freeze (spec 025 #21). An hour whose traces
	// retention has taken writes nothing here either — the join finds no
	// trace, which is the truth, since the scores went with their targets.
	// An hour whose traces are *intact* past the window — history imported
	// into an install that had already rolled it — gets its score rows,
	// which the one shared gate refused it for ever.
	scoresFrozen, err := hourFrozenIn(tx, "scores_hourly", r.ProjectID, r.Hour, past)
	if err != nil {
		return err
	}
	if !scoresFrozen {
		if r.ScoreRows, err = rollScoreHour(tx, r.ProjectID, r.Hour); err != nil {
			return err
		}
	}

	// And the fourth table's own freeze (spec 027 #3), on the same rule: the
	// rows a table holds are the rows that table protects. It is what lets
	// migration 0016's backfill fill an hour whose traces are intact past
	// the window, and what stops it from demolishing one whose name rows
	// already stand.
	namesFrozen, err := hourFrozenIn(tx, "names_hourly", r.ProjectID, r.Hour, past)
	if err != nil {
		return err
	}
	if !namesFrozen {
		if r.NameRows, err = rollNameHour(tx, r.ProjectID, r.Hour); err != nil {
			return err
		}
	}

	// The summary is a sum of the per-user rows, so it is recomputed exactly
	// when they were written (#7): an hour frozen in `users_hourly` left them
	// as they stand, and there is nothing to add up again.
	if !r.UsersRolled || r.DeferSummary {
		return nil
	}
	return recomputeUsers(tx, r.ProjectID, r.Touched)
}

// rollTraffic writes the two tables the traffic freeze protects — the
// statistics of spec 013 and the per-user rows of spec 023 — each on its own
// answer to the freeze (spec 026 #7).
//
// They rode one gate until now, and the gate was `stats_hourly`'s. That is the
// approximation spec 023 #15 wrote down as a known limit: history imported into
// an install that had already rolled those hours is "past the window" and
// completely intact, `stats_hourly` holds the hour, and `/users` was blind to
// the imported traffic for ever. Asked per table it is the same rule — the rows
// a table holds are the rows that table protects — which is what spec 025 #21
// did for the scores and what this does for the third.
func (r *statsRoll) rollTraffic(tx *sql.Tx, past bool) error {
	statsFrozen, err := hourFrozenIn(tx, "stats_hourly", r.ProjectID, r.Hour, past)
	if err != nil {
		return err
	}
	r.Frozen = statsFrozen
	usersFrozen, err := hourFrozenIn(tx, "users_hourly", r.ProjectID, r.Hour, past)
	if err != nil {
		return err
	}
	r.UsersRolled = !usersFrozen

	if !statsFrozen {
		if err := r.rollStats(tx); err != nil {
			return err
		}
	}
	if usersFrozen {
		return nil
	}
	return r.rollUsers(tx)
}

// rollStats recomputes one hour of `stats_hourly` whole (spec 013 #3).
func (r *statsRoll) rollStats(tx *sql.Tx) error {
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

// rollUsers is the same hour one dimension over (spec 023 #2): the same job,
// the same transaction, its own freeze. Two aggregators would be two watermarks
// and two seams, where this is one more `DELETE`/`INSERT` and a bounded
// recompute of the summaries the hour touched.
func (r *statsRoll) rollUsers(tx *sql.Tx) error {
	perUser, err := rollUserHour(tx, r.ProjectID, r.Hour)
	if err != nil {
		return err
	}
	r.Touched, err = writeUserHour(tx, r.ProjectID, r.Hour, perUser)
	if err != nil {
		return err
	}
	r.UserRows = len(perUser)
	return nil
}

// hourPastWindow reports whether an hour lies past the project's
// trace-retention window — the first half of spec 013 #11's rule, and the half
// that is the same for every table the rollup writes.
//
// The epoch hour is never past it. It is not a time: it is where a trace lands
// when no span of it said when it started (spec 004 #26), so its rows are as
// young as any other and retention cannot have taken them. Freezing it instead
// pinned the phantom 1970 bucket that spec 013 #16 exists to remove, for every
// project with a retention window, which is every ordinary one (found in the
// fifth review of PR #28).
func hourPastWindow(project *Project, hour, now int64) bool {
	if hour <= 0 {
		return false
	}
	return hour < frozenBefore(project, now)
}

// hourFrozenIn is spec 013 #14's second half asked of **one** table: past the
// window, an hour is frozen for a table exactly when that table already holds
// rows for it.
//
// Both halves matter, and #14 spells out why. The window is measured against
// the client's timestamp while the sweep deletes by arrival, so a year of
// history imported this morning is "past the window" and completely intact;
// freezing it would leave hours that no half of the read seam can answer.
// Stored rows are the thing #11 protects, and their absence is what says there
// is nothing to protect.
//
// Asked per table it is the same rule; asked once for all of them it was an
// approximation that happened to be exact while there was one table. Spec 023
// inherited the approximation and wrote its cost down as a known limit (#15);
// spec 025 #21 asks the rule of its own table instead — so a migration's
// backfill can fill an hour whose traces are intact, and cannot demolish one
// whose rows already stand.
//
// The table name is this package's own constant, never anything a request
// carries; only the project id and the hour are bound.
func hourFrozenIn(tx *sql.Tx, table, projectID string, hour int64, past bool) (bool, error) {
	if !past {
		return false, nil
	}
	var rows int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM `+table+` WHERE project_id = ? AND hour = ?`,
		projectID, hour).Scan(&rows); err != nil {
		return false, fmt.Errorf("look for hour %d in %s: %w", hour, table, err)
	}
	return rows > 0, nil
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
