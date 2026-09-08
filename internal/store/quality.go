package store

import (
	"database/sql"
	"fmt"
)

// The score rollup (spec 025): spec 013's hourly table one dimension further
// along, so that "did hallucination drop after 2.5.0" is an indexed range read
// rather than the scan the design forbids at scale (design §5.2).
//
// Everything here rides the aggregator of spec 013 — the same pass, the same
// `(project, hour)` job, the same watermark, the same freeze, the same
// `stats_retention_days` sweep — because two aggregators would be two
// watermarks and two seams (spec 023 #2). What is new is the tuple (`name`,
// `data_type` and `category` in it), the four numbers a row carries (#2), and
// one addition to the dirty set: a score's arrival touches no trace row, so
// the pass asks `scores.created_at` as well (#3).

// ScoreStatsRow is one dimension tuple of one hour: the stored row, and the
// shape the live scan yields so that both halves of the read seam fold the
// same way.
//
// A score is in exactly one row — unlike `stats_hourly`, where trace rows and
// observation rows are two different units (spec 013 #1). `Model` is a
// dimension here rather than a discriminator: summing over every model counts
// each score once, and keeping only the rows with a model is what the model
// grouping means (spec 025 #6).
type ScoreStatsRow struct {
	Hour        int64 // Unix seconds, top of the hour, UTC: the trace's hour
	Environment string
	Release     string
	// Model is the model of the observation the score names, and empty when
	// it names none — or when that observation carries no model.
	Model    string
	Name     string
	DataType string
	// Category is a categorical score's `string_value` and empty for every
	// other type; it is part of the key, so one categorical name is one row
	// per value seen in the hour.
	Category string
	Count    int64
	// Sum is Σ value for a numeric name, the number of `1`s for a boolean
	// one, and zero for a categorical one. The mean and the rate are
	// read-time divisions, which is what makes a day the exact merge of its
	// 24 hours (spec 025 #2).
	Sum float64
	// Min and Max are set on numeric rows only.
	Min *float64
	Max *float64
}

// scoreRollQuery recomputes one `(project, hour)` of `scores_hourly` from the
// raw rows: named so that a test can hand the shipped SQL to EXPLAIN QUERY
// PLAN (spec 025 #14, on the method of spec 003 #25).
//
// It drives off the *traces* of the hour rather than off the scores, and that
// is the whole performance story: `idx_traces_timestamp` bounds the hour, and
// `idx_scores_trace` finds each trace's scores. Driving from `scores` would
// have nothing to bound — a score's own `timestamp` is not what it is filed
// under (Decision 1) — so it would read every score the project holds, once
// per hour of a backfill.
//
// Its `WHERE` is where two kinds of score leave the table: one that names no
// trace has nothing to join to, and a `text` one has nothing to add up.
//
// The aggregate arithmetic is SQLite's, not Go's: there is no histogram here,
// so the whole hour is a `DELETE` plus one `INSERT … SELECT` inside the job's
// transaction — idempotent by construction, as spec 013 #3 requires.
const scoreRollQuery = `INSERT INTO scores_hourly
	   (project_id, hour, environment, release, model, name, data_type, category,
	    count, sum, min, max)
	 SELECT ?, ?,
	        t.environment AS env,
	        COALESCE(t.release, '') AS rel,
	        COALESCE(o.model, '') AS mdl,
	        s.name AS score_name,
	        s.data_type AS dtype,
	        CASE WHEN s.data_type = 'categorical'
	             THEN COALESCE(s.string_value, '') ELSE '' END AS cat,
	        COUNT(*),
	        CASE WHEN s.data_type = 'categorical' THEN 0
	             ELSE COALESCE(SUM(s.value), 0) END,
	        CASE WHEN s.data_type = 'numeric' THEN MIN(s.value) END,
	        CASE WHEN s.data_type = 'numeric' THEN MAX(s.value) END
	   FROM traces t
	   JOIN scores s ON s.project_id = t.project_id AND s.trace_id = t.id
	   LEFT JOIN observations o ON o.project_id = s.project_id
	                           AND o.trace_id = s.trace_id AND o.id = s.observation_id
	  WHERE t.project_id = ? AND t.timestamp >= ? AND t.timestamp < ?
	    AND s.data_type != 'text'
	  GROUP BY env, rel, mdl, score_name, dtype, cat`

// rollScoreHour replaces one `(project, hour)` of `scores_hourly` and reports
// how many tuples the hour produced.
func rollScoreHour(tx *sql.Tx, projectID string, hour int64) (int, error) {
	if _, err := tx.Exec(
		`DELETE FROM scores_hourly WHERE project_id = ? AND hour = ?`,
		projectID, hour); err != nil {
		return 0, fmt.Errorf("clear the scores of hour %d: %w", hour, err)
	}
	result, err := tx.Exec(scoreRollQuery,
		projectID, hour, projectID, hour*1e9, (hour+SecondsPerHour)*1e9)
	if err != nil {
		return 0, fmt.Errorf("write the score rows of hour %d: %w", hour, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(rows), nil
}

// scoreRollupQuery is the rolled half of the read seam: a half-open hour
// range, oldest first (spec 025 #7).
//
// A function rather than a constant because the two optional narrowings change
// which index answers it, and the EXPLAIN test asserts both: without a name it
// seeks the primary key, whose leading columns are exactly `(project, hour)`;
// with one it seeks `idx_scores_hourly_name`, which is why that index exists.
func scoreRollupQuery(projectID string, fromHour, toHour int64, environment, name string) (string, []any) {
	query := `SELECT hour, environment, release, model, name, data_type, category,
	                 count, sum, min, max
	          FROM scores_hourly
	          WHERE project_id = ? AND hour >= ? AND hour < ?`
	args := []any{projectID, fromHour, toHour}
	if name != "" {
		query += ` AND name = ?`
		args = append(args, name)
	}
	if environment != "" {
		// A filter, not a grouping — the same rule the statistics follow:
		// filter first, group after.
		query += ` AND environment = ?`
		args = append(args, environment)
	}
	return query + ` ORDER BY hour`, args
}

// ScoresRollupRows reads the stored rows of a half-open hour range.
func (s *Store) ScoresRollupRows(projectID string, fromHour, toHour int64,
	environment, name string, yield func(ScoreStatsRow)) error {
	query, args := scoreRollupQuery(projectID, fromHour, toHour, environment, name)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return fmt.Errorf("read the score rollup: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			row      ScoreStatsRow
			min, max sql.NullFloat64
		)
		if err := rows.Scan(&row.Hour, &row.Environment, &row.Release, &row.Model,
			&row.Name, &row.DataType, &row.Category,
			&row.Count, &row.Sum, &min, &max); err != nil {
			return fmt.Errorf("scan a score rollup row: %w", err)
		}
		if min.Valid {
			row.Min = &min.Float64
		}
		if max.Valid {
			row.Max = &max.Float64
		}
		yield(row)
	}
	return rows.Err()
}

// scoreLiveQuery is the live half of the seam: the same tuples read straight
// off the raw rows, one row per score, for the tail past the watermark and for
// the partial hours at either edge of the asked range (spec 025 #7).
//
// It drives off the traces of the range for `scoreRollQuery`'s reason, and it
// applies exactly the same two exclusions — no trace, or `text` — so the seam
// cannot show one score on one side of the watermark and not on the other.
//
// The unary `+` before `s.name` is load-bearing, and is spec 023's lesson
// (`sessionStartCondition`) met a third time. Without it the planner reads
// `s.name = ?` as an index term and drives the join off `idx_scores_name`,
// which carries no `trace_id` — so for **every trace of the range** it walks
// **every score of that name in the project** and checks `s.trace_id = t.id` by
// hand. Nothing here ever runs `ANALYZE`, so a shipped database has no
// `sqlite_stat1` and the planner cannot know that the trace is the selective
// side. Measured on the month fixture, one name over 30 days: 1,100 s without
// the `+`, 0.9 s with it. `TestScoreQueriesSeekTheirIndexes` asserts the plan,
// so "simplifying" the `+` away fails a test rather than a deployment.
func scoreLiveQuery(projectID string, from, to int64, environment, name string) (string, []any) {
	query := `SELECT t.timestamp, t.environment, COALESCE(t.release, ''),
	                 COALESCE(o.model, ''), s.name, s.data_type,
	                 COALESCE(s.string_value, ''), s.value
	          FROM traces t
	          JOIN scores s ON s.project_id = t.project_id AND s.trace_id = t.id
	          LEFT JOIN observations o ON o.project_id = s.project_id
	                                  AND o.trace_id = s.trace_id AND o.id = s.observation_id
	          WHERE t.project_id = ? AND t.timestamp >= ? AND t.timestamp < ?
	            AND s.data_type != 'text'`
	args := []any{projectID, from, to}
	if name != "" {
		query += ` AND +s.name = ?`
		args = append(args, name)
	}
	if environment != "" {
		query += ` AND t.environment = ?`
		args = append(args, environment)
	}
	return query, args
}

// ScoreSamples yields one row per score in a half-open range of *trace*
// timestamps, shaped as a one-sample rollup row so that the caller folds both
// halves of the seam with one function.
func (s *Store) ScoreSamples(projectID string, from, to int64,
	environment, name string, yield func(ScoreStatsRow)) error {
	query, args := scoreLiveQuery(projectID, from, to, environment, name)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return fmt.Errorf("read the scores of a range: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			timestamp   int64
			row         ScoreStatsRow
			stringValue string
			value       sql.NullFloat64
		)
		if err := rows.Scan(&timestamp, &row.Environment, &row.Release, &row.Model,
			&row.Name, &row.DataType, &stringValue, &value); err != nil {
			return fmt.Errorf("scan a score of the range: %w", err)
		}
		row.Hour = HourOf(timestamp)
		row.Count = 1
		switch row.DataType {
		case ScoreCategorical:
			row.Category = stringValue
		case ScoreNumeric:
			if value.Valid {
				row.Sum = value.Float64
				row.Min, row.Max = &value.Float64, &value.Float64
			}
		default:
			if value.Valid {
				row.Sum = value.Float64
			}
		}
		yield(row)
	}
	return rows.Err()
}

// ScoresRollupHours reports which hours of a project hold score rows. The
// tests ask; nothing on the read path needs it.
func (s *Store) ScoresRollupHours(projectID string) ([]int64, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT hour FROM scores_hourly WHERE project_id = ? ORDER BY hour`, projectID)
	if err != nil {
		return nil, fmt.Errorf("read the rolled score hours: %w", err)
	}
	return scanHours(rows)
}

// dirtyScoreHoursQuery is spec 025 #3's addition to the dirty set, named so
// that a test can hand the shipped SQL to EXPLAIN QUERY PLAN.
//
// It exists because a score's arrival touches no trace row. A judge grading
// yesterday's traffic today writes only `scores`, and `traces.updated_at` —
// spec 013 #15's whole mechanism — never hears of it, so the hour of the trace
// the score names would keep its old answer for ever. `idx_scores_created` is
// what makes the question an indexed range rather than a scan of every score
// the project holds, on every pass.
//
// The other direction is inherited for free: a trace whose own hour moved
// carries its scores with it, since they are keyed by *its* hour and spec 013
// #16's rule already re-rolls both hours.
//
// `data_type` is deliberately not filtered here, unlike in the roll itself: an
// upsert that turns a numeric score into a text one moves `created_at` (spec
// 003 #3), and the hour has to be re-rolled to *remove* the row that score
// used to have. Filtering text out would leave that row standing for ever.
//
// The unary `+` before `t.timestamp` is the guard `hoursWithTraces` explains:
// this division truncates towards zero where `HourOf` floors, so a pre-epoch
// timestamp — a client's bytes gone wrong rather than a time — would name an
// hour the roll does not use, and such a trace is left to the live scan. The
// `+` also keeps the comparison from reading as an index term, which would
// drive the query off `idx_traces_timestamp` — a scan of every trace in the
// project — instead of off the scores that actually changed (the lesson of
// spec 023's `dirtySessionHoursQuery`).
const dirtyScoreHoursQuery = `SELECT DISTINCT (t.timestamp / 1000000000 / ?) * ? AS hour
	 FROM scores s
	 JOIN traces t ON t.project_id = s.project_id AND t.id = s.trace_id
	 WHERE s.project_id = ? AND s.created_at > ? AND +t.timestamp >= 0
	 ORDER BY hour`

// dirtyScoreHours are the hours whose scores changed since the last pass.
func (s *Store) dirtyScoreHours(projectID string, since int64) ([]int64, error) {
	rows, err := s.db.Query(dirtyScoreHoursQuery,
		SecondsPerHour, SecondsPerHour, projectID, since)
	if err != nil {
		return nil, fmt.Errorf("find the hours a score touched: %w", err)
	}
	return scanHours(rows)
}

// scoreHourOfQuery is what a deletion asks before it deletes (spec 025 #4): a
// deleted row is not found by `created_at > last_pass`, because it is gone, so
// the handler re-rolls its hour in the request that removes it — the way a
// user-data erasure re-rolls the hours it emptied (spec 013 #7).
//
// It answers nothing for the three scores that are not in the table anyway: one
// with no trace, one whose trace has not arrived, and a `text` one.
const scoreHourOfQuery = `SELECT (t.timestamp / 1000000000 / ?) * ?
	 FROM scores s
	 JOIN traces t ON t.project_id = s.project_id AND t.id = s.trace_id
	 WHERE s.project_id = ? AND s.id = ? AND s.data_type != 'text' AND t.timestamp >= 0`

// traceHourQuery is the hour one trace sits in, for the caller that knows a
// trace id and needs the `scores_hourly` row it governs (spec 025 #20).
const traceHourQuery = `SELECT (t.timestamp / 1000000000 / ?) * ?
	 FROM traces t
	 WHERE t.project_id = ? AND t.id = ? AND t.timestamp >= 0`

// vacatedScoreHour is the hour a score is *leaving*, read before an upsert
// replaces it, and nothing when it is leaving none (spec 025 #20).
//
// A write is found by `created_at > last_pass` and dirties the hour of the
// trace it names **now**. When a re-POST re-points a score at another trace —
// or drops the target for a session-only one — the hour it used to be counted
// in is named by nothing at all afterwards, exactly as a deleted score's is
// (#4). So it is read here, while the old row still says it.
//
// It answers only when the target actually changed: a correction that leaves
// `trace_id` alone cannot move the hour, because what decides the hour is the
// trace's timestamp and a score write does not touch it.
func vacatedScoreHour(tx *sql.Tx, projectID, id, target string) (int64, bool, error) {
	var previous sql.NullString
	err := tx.QueryRow(
		`SELECT trace_id FROM scores WHERE project_id = ? AND id = ?`, projectID, id).
		Scan(&previous)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read the target of score %s: %w", id, err)
	}
	if previous.String == "" || previous.String == target {
		return 0, false, nil
	}
	var hour sql.NullInt64
	err = tx.QueryRow(traceHourQuery, SecondsPerHour, SecondsPerHour, projectID, previous.String).
		Scan(&hour)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("find the hour score %s is leaving: %w", id, err)
	}
	return hour.Int64, hour.Valid, nil
}

// correctScoreHours re-rolls the `scores_hourly` rows a write left wrong, in
// the very transaction that wrote it (spec 025 #22).
//
// Two writes leave an hour that no later question finds: a deletion, whose row
// `created_at > last_pass` cannot see because it is gone (#4), and a re-POST
// that moves a score off the trace it was counted under (#20). Both used to be
// corrected by the handler afterwards, submitting the whole `RollHour` job —
// which rewrites `stats_hourly` and `users_hourly` too and then re-aggregates
// the entire history of every user the hour touched, the exact fan-out the
// aggregator defers with `DeferSummary`. It was also a second transaction: one
// that failed left the correction lost and the request answering 500 over a
// write that had committed, and the client's retry could not find it again.
//
// Here it is neither. Only the third table is touched, at a cost of the hour's
// own scores, and the correction commits with the write or not at all.
//
// The two gates are the ones the job itself applies, read from the same
// transaction. An hour at or past the watermark is the live half of the read
// seam's, and the raw rows it scans are already right. A frozen hour is left as
// it stands, because past the retention window a recompute from what the sweep
// left is a demolition rather than a correction (spec 013 #11, asked per table
// by #21).
func correctScoreHours(tx *sql.Tx, projectID string, now int64, hours ...int64) error {
	if len(hours) == 0 {
		return nil
	}
	state, err := rollupState(tx, projectID)
	if err != nil {
		return err
	}
	project, err := projectByID(tx, projectID)
	if err != nil || project == nil {
		return err
	}
	// One recompute per distinct hour: a batch that re-points fifty scores
	// off one trace has one hour to correct, not fifty.
	seen := map[int64]bool{}
	for _, hour := range hours {
		if hour >= state.RolledUntil || seen[hour] {
			continue
		}
		seen[hour] = true
		frozen, err := hourFrozenIn(tx, "scores_hourly", projectID, hour,
			hourPastWindow(project, hour, now))
		if err != nil {
			return err
		}
		if frozen {
			continue
		}
		if _, err := rollScoreHour(tx, projectID, hour); err != nil {
			return err
		}
	}
	return nil
}

// scoresRollupSweep deletes rolled score rows older than the project's stats
// window. One chunk per job, like every other deletion this store does; the
// window is `stats_retention_days` and there is no knob of its own, because
// all three tables are one rollup (spec 025, config additions).
type scoresRollupSweep struct {
	ProjectID string
	Before    int64

	Deleted int64
}

func (s *scoresRollupSweep) apply(tx *sql.Tx) error {
	result, err := tx.Exec(
		`DELETE FROM scores_hourly
		 WHERE rowid IN (SELECT rowid FROM scores_hourly
		                 WHERE project_id = ? AND hour < ? LIMIT ?)`,
		s.ProjectID, s.Before, DefaultSweepChunk)
	if err != nil {
		return fmt.Errorf("sweep the score rollup: %w", err)
	}
	s.Deleted, err = result.RowsAffected()
	return err
}
