package store

import (
	"database/sql"
	"fmt"
	"strings"
)

// Scores (spec 003): a quality judgement about a trace, an observation or a
// session, written by an eval loop or a human reviewer. Unlike telemetry,
// scores arrive as plain JSON on an interactive request — but they take the
// same road into the database, so a 201 means the row is on disk (#9).

// Score data types (#5). The set is closed by a CHECK in schema 0003.
const (
	ScoreNumeric     = "numeric"
	ScoreBoolean     = "boolean"
	ScoreCategorical = "categorical"
	ScoreText        = "text"
)

// Score is one judgement. Exactly one of Value and StringValue carries it,
// chosen by DataType (#5); the store takes the pair as already validated.
type Score struct {
	ID            string
	TraceID       string
	ObservationID string
	SessionID     string
	Name          string
	DataType      string
	Value         *float64
	StringValue   *string
	Comment       string
	// Metadata is raw JSON stored inline rather than in `payloads` (#6);
	// nil when the client sent none.
	Metadata []byte
	// Timestamp is event time — when the graded interaction happened —
	// and CreatedAt is receive time. Both Unix nanoseconds (#16).
	Timestamp int64
	CreatedAt int64
}

// ScoreWrite is one POST /api/v1/scores. Its scores commit together or not at
// all: the client here is an application that can fix its batch and retry, and
// a half-applied batch is harder to reason about than a clean failure (#7).
type ScoreWrite struct {
	ProjectID string
	Scores    []*Score

	// Vacated are the hours whose `scores_hourly` rows this write invalidated
	// without dirtying them: the hour a score was counted in *before* a
	// re-POST moved it to another trace, or off one. The caller re-rolls them
	// before it answers, exactly as it does for a deletion (spec 025 #20).
	Vacated []int64
}

// apply upserts every score by (project_id, id). A re-POST with the same id
// replaces the row wholesale — a correction is a re-POST, not a delete and an
// insert (#3), so `created_at` moves to the receive time of the newest
// delivery just like every other column.
//
// A score whose name has a config is checked against it first, here rather
// than in the handler, because a config can be replaced between a
// handler-side read and the commit (spec 014 #15, spec 003 Decision 20). One
// violating item refuses the whole batch with a message naming it (#7).
func (s *ScoreWrite) apply(tx *sql.Tx) error {
	configs := map[string]*ScoreConfig{}
	for i, score := range s.Scores {
		config, known := configs[score.Name]
		if !known {
			var err error
			if config, err = scoreConfigByName(tx, s.ProjectID, score.Name); err != nil {
				return err
			}
			configs[score.Name] = config
		}
		if config == nil {
			continue
		}
		if err := config.check(score); err != nil {
			message := err.Error()
			if len(s.Scores) > 1 {
				message = fmt.Sprintf("score at index %d: %s", i, message)
			}
			return &Rejection{Kind: RejectInvalid, Message: message}
		}
	}
	s.Vacated = nil
	for _, score := range s.Scores {
		// Read before the upsert overwrites it: afterwards nothing names the
		// hour this score is leaving (spec 025 #20).
		hour, moved, err := vacatedScoreHour(tx, s.ProjectID, score.ID, score.TraceID)
		if err != nil {
			return err
		}
		if moved {
			s.Vacated = append(s.Vacated, hour)
		}
		_, err = tx.Exec(
			`INSERT INTO scores (
			   project_id, id, trace_id, observation_id, session_id, name,
			   data_type, value, string_value, comment, metadata, timestamp, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(project_id, id) DO UPDATE SET
			   trace_id       = excluded.trace_id,
			   observation_id = excluded.observation_id,
			   session_id     = excluded.session_id,
			   name           = excluded.name,
			   data_type      = excluded.data_type,
			   value          = excluded.value,
			   string_value   = excluded.string_value,
			   comment        = excluded.comment,
			   metadata       = excluded.metadata,
			   timestamp      = excluded.timestamp,
			   created_at     = excluded.created_at`,
			s.ProjectID, score.ID, nullString(score.TraceID), nullString(score.ObservationID),
			nullString(score.SessionID), score.Name, score.DataType, nullFloat(score.Value),
			nullText(score.StringValue), nullString(score.Comment), nullJSON(score.Metadata),
			score.Timestamp, score.CreatedAt,
		)
		if err != nil {
			return fmt.Errorf("upsert score %s: %w", score.ID, err)
		}
	}
	return nil
}

// ScoreDelete is DELETE /api/v1/scores/{id} (spec 022 #6): one row, gone. It
// wears none of spec 005 #8's ceremony — a score is a single row that a re-POST
// with the same id recreates, so the act is undoable by the same client that
// undid it, and a retraction the store refused would leave a wrong verdict on a
// trace with no way off it but retention.
//
// A write like every other, so it travels the group-commit queue (spec 001)
// rather than opening a second connection to the database.
type ScoreDelete struct {
	ProjectID string
	ID        string

	// Hour is the hour of the trace this score was filed under, and Rolled
	// says whether there was one. The caller re-rolls it before it answers
	// (spec 025 #4): a deleted row is not found by `created_at > last_pass`,
	// because it is gone, so the correction has to be made here — the way a
	// user-data erasure re-rolls the hours it emptied (spec 013 #7).
	Hour   int64
	Rolled bool
}

func (d *ScoreDelete) apply(tx *sql.Tx) error {
	// Read before the delete: afterwards nothing names the hour. Three
	// scores answer nothing here, and each of them is one the rollup never
	// held — a session-only score, one whose trace has not arrived, and a
	// `text` one.
	var hour sql.NullInt64
	err := tx.QueryRow(scoreHourOfQuery, SecondsPerHour, SecondsPerHour, d.ProjectID, d.ID).Scan(&hour)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("find the hour of score %s: %w", d.ID, err)
	}
	d.Hour, d.Rolled = hour.Int64, hour.Valid

	// Scoped by project as well as by id: an id from another project must
	// read as "no such score", never as a row this caller may take.
	result, err := tx.Exec(`DELETE FROM scores WHERE project_id = ? AND id = ?`, d.ProjectID, d.ID)
	if err != nil {
		return fmt.Errorf("delete score %s: %w", d.ID, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return &Rejection{Kind: RejectNotFound, Message: fmt.Sprintf("score %q not found", d.ID)}
	}
	return nil
}

// ScoreFilter narrows a score listing (spec 003, API contract). The zero value
// lists a project's newest scores.
type ScoreFilter struct {
	TraceID       string
	ObservationID string
	SessionID     string
	Name          string
	DataType      string
	// From and To bound `timestamp` in Unix nanoseconds as a half-open
	// range — From inclusive, To exclusive — so that paging a day at a
	// time never counts a score twice. Nil is unbounded; a pointer rather
	// than a zero sentinel because zero is a legal timestamp, and reading
	// `to=1970-01-01T00:00:00Z` as "no bound" would answer a query for
	// nothing with everything.
	From *int64
	To   *int64
	// Limit caps the rows returned; the caller asks for one more than the
	// page size to learn whether another page exists.
	Limit int
	// After continues a previous page. Nil starts at the newest score.
	After *ScoreCursor
}

// ScoreCursor is the keyset of the last row of a page: pagination follows the
// sort key rather than an offset, so a score written concurrently cannot make
// the next page skip or repeat a row (#18).
type ScoreCursor struct {
	Timestamp int64
	ID        string
}

// Scores lists scores newest first.
func (s *Store) Scores(projectID string, filter ScoreFilter) ([]*Score, error) {
	where := []string{"project_id = ?"}
	args := []any{projectID}
	add := func(clause string, values ...any) {
		where = append(where, clause)
		args = append(args, values...)
	}
	if filter.TraceID != "" {
		add("trace_id = ?", filter.TraceID)
	}
	if filter.ObservationID != "" {
		add("observation_id = ?", filter.ObservationID)
	}
	if filter.SessionID != "" {
		add("session_id = ?", filter.SessionID)
	}
	if filter.Name != "" {
		add("name = ?", filter.Name)
	}
	if filter.DataType != "" {
		add("data_type = ?", filter.DataType)
	}
	if filter.From != nil {
		add("timestamp >= ?", *filter.From)
	}
	if filter.To != nil {
		add("timestamp < ?", *filter.To)
	}
	if filter.After != nil {
		// The tie-break on id keeps the order total: scores written in
		// the same commit share a timestamp often enough that ordering
		// by it alone would make a page boundary ambiguous.
		//
		// Written as a row-value comparison rather than the equivalent
		// `timestamp < ? OR (timestamp = ? AND id < ?)`: SQLite seeks
		// straight to the cursor with the former and scans from the
		// newest row with the latter (Decision 25).
		add("(timestamp, id) < (?, ?)", filter.After.Timestamp, filter.After.ID)
	}
	args = append(args, filter.Limit)

	rows, err := s.db.Query(
		`SELECT id, trace_id, observation_id, session_id, name, data_type, value,
		        string_value, comment, metadata, timestamp, created_at
		 FROM scores WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY timestamp DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list scores: %w", err)
	}
	defer rows.Close()

	var out []*Score
	for rows.Next() {
		score, err := scanScore(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, score)
	}
	return out, rows.Err()
}

// scoreColumns is the SELECT list every score read shares, in the order
// scanScore expects.
const scoreColumns = `id, trace_id, observation_id, session_id, name, data_type, value,
	        string_value, comment, metadata, timestamp, created_at`

// Score returns one score, or nil when it does not exist.
func (s *Store) Score(projectID, id string) (*Score, error) {
	rows, err := s.db.Query(
		`SELECT `+scoreColumns+`
		 FROM scores WHERE project_id = ? AND id = ?`, projectID, id)
	if err != nil {
		return nil, fmt.Errorf("read score %s: %w", id, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	return scanScore(rows)
}

func scanScore(rows *sql.Rows) (*Score, error) {
	var (
		score         Score
		traceID       sql.NullString
		observationID sql.NullString
		sessionID     sql.NullString
		value         sql.NullFloat64
		stringValue   sql.NullString
		comment       sql.NullString
		metadata      sql.NullString
	)
	if err := rows.Scan(&score.ID, &traceID, &observationID, &sessionID, &score.Name,
		&score.DataType, &value, &stringValue, &comment, &metadata,
		&score.Timestamp, &score.CreatedAt); err != nil {
		return nil, fmt.Errorf("scan score: %w", err)
	}
	score.TraceID, score.ObservationID = traceID.String, observationID.String
	score.SessionID, score.Comment = sessionID.String, comment.String
	if value.Valid {
		score.Value = &value.Float64
	}
	if stringValue.Valid {
		score.StringValue = &stringValue.String
	}
	if metadata.Valid {
		score.Metadata = []byte(metadata.String)
	}
	return &score, nil
}

func nullFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullText(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// nullJSON binds raw JSON to a TEXT column. The conversion to string is not
// cosmetic: a []byte binds as a BLOB, which a STRICT table refuses to store in
// a TEXT column.
func nullJSON(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}
