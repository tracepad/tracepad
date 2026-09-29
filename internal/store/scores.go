package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
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
	//
	// A zero CreatedAt on a write means "stamp it in the transaction": see
	// ScoreWrite.apply, which is where receive time is actually decided
	// (spec 025 #23).
	Timestamp int64
	CreatedAt int64
	// Author is who wrote the score (spec 048): the server stamps it from
	// the caller on every write. Nil on a score written before schema 0031,
	// which has none and never will (#6).
	Author *ScoreAuthor
}

// The two kinds of credential that write a score (spec 048 #1). The set is
// closed by a CHECK in schema 0031.
const (
	AuthorAccount = "account"
	AuthorKey     = "key"
)

// A key author's standing (spec 048 #4). An account author's is one of the
// roles or StandingRemoved, StandingDisabled, StandingDeleted, as a key's
// minter's is (spec 045 #8).
const (
	// StandingActive is a key that still exists in the score's project.
	StandingActive = "active"
	// StandingRevoked is a key that was revoked: its row is gone, and the
	// name copied onto the score is all that is left of it.
	StandingRevoked = "revoked"
)

// ScoreAuthor is the credential that wrote a score (spec 048 #2): an account's
// id or a key's public key, and how it was named when it wrote. Plain values,
// not a reference: an account and a key are both deleted outright, and the
// copy is what answers "who said this" afterwards.
type ScoreAuthor struct {
	Kind string
	ID   string
	// Name is the account's display name or the key's name. On a read it is
	// the account's name now while the account exists and has one, and the
	// copy otherwise (#4).
	Name string
	// Email is the account's email when it wrote; empty for a key.
	Email string
	// Standing is the credential's relation to the score's project now,
	// computed when the score is read (#4); empty on a write.
	Standing string
}

// AccountAuthor is a score written by a signed-in account.
func AccountAuthor(a *Account) *ScoreAuthor {
	return &ScoreAuthor{Kind: AuthorAccount, ID: a.ID, Name: a.Name, Email: a.Email}
}

// KeyAuthor is a score written with a project key.
func KeyAuthor(k *KeyInfo) *ScoreAuthor {
	return &ScoreAuthor{Kind: AuthorKey, ID: k.PublicKey, Name: k.Name}
}

// ScoreWrite is one POST /api/v1/scores. Its scores commit together or not at
// all: the client here is an application that can fix its batch and retry, and
// a half-applied batch is harder to reason about than a clean failure (#7).
type ScoreWrite struct {
	ProjectID string
	Scores    []*Score
}

// weight is the scores the write carries (spec 043 #35): an array has no
// count cap, and a body at the cap is hundreds of thousands of them.
func (s *ScoreWrite) weight() int { return len(s.Scores) }

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
	// Receive time is stamped here rather than in the handler, for the reason
	// `traces.updated_at` is (`ingest.go`): the rollup's dirty set asks
	// `created_at > last_pass`, and that question is only sound if the stamp
	// and the commit are the same moment to within `commitMargin`. Stamped
	// before the group-commit queue, a write that waited a second in it
	// carried a `created_at` already behind the next pass's cutoff and was
	// missed by every pass afterwards — permanently, since nothing else
	// dirties the hour of a score (spec 025 #23).
	//
	// A caller that stamped its own — a fixture dating rows on its clock — is
	// left alone.
	receivedAt := time.Now().UnixNano()

	var vacated []int64
	for _, score := range s.Scores {
		// Read before the upsert overwrites it: afterwards nothing names the
		// hour this score is leaving (spec 025 #20).
		hour, moved, err := vacatedScoreHour(tx, s.ProjectID, score.ID, score.TraceID)
		if err != nil {
			return err
		}
		if moved {
			vacated = append(vacated, hour)
		}
		kind, id, name, email, err := authorColumns(score.Author)
		if err != nil {
			return err
		}
		// Kept off the caller's Score, so that an application that runs again
		// (a window sent back, spec 043 #38) stamps afresh.
		createdAt := score.CreatedAt
		if createdAt == 0 {
			createdAt = receivedAt
		}
		_, err = tx.Exec(
			`INSERT INTO scores (
			   project_id, id, trace_id, observation_id, session_id, name,
			   data_type, value, string_value, comment, metadata, timestamp, created_at,
			   author_kind, author_id, author_name, author_email)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
			   created_at     = excluded.created_at,
			   author_kind    = excluded.author_kind,
			   author_id      = excluded.author_id,
			   author_name    = excluded.author_name,
			   author_email   = excluded.author_email`,
			s.ProjectID, score.ID, nullString(score.TraceID), nullString(score.ObservationID),
			nullString(score.SessionID), score.Name, score.DataType, nullFloat(score.Value),
			nullText(score.StringValue), nullString(score.Comment), nullJSON(score.Metadata),
			score.Timestamp, createdAt, kind, id, name, email,
		)
		if err != nil {
			return fmt.Errorf("upsert score %s: %w", score.ID, err)
		}
	}
	// The hours the moved scores left, corrected in this transaction so that
	// the correction cannot outlive the write that needed it (spec 025 #22).
	return correctScoreHours(tx, s.ProjectID, receivedAt, vacated...)
}

// authorColumns binds an author to its four columns: all NULL for none, all
// set otherwise, as schema 0031's CHECKs require. The last writer is the author
// (spec 048 #3), so the upsert sets them like every other column.
func authorColumns(author *ScoreAuthor) (kind, id, name, email any, err error) {
	if author == nil {
		return nil, nil, nil, nil, nil
	}
	if (author.Kind != AuthorAccount && author.Kind != AuthorKey) || author.ID == "" {
		return nil, nil, nil, nil, fmt.Errorf("score author %q/%q is neither an account nor a key", author.Kind, author.ID)
	}
	return author.Kind, author.ID, author.Name, author.Email, nil
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
	// The hour this score was counted in, recomputed here rather than by the
	// handler afterwards (spec 025 #22): a deleted row is not found by
	// `created_at > last_pass`, because it is gone, so the correction has to
	// travel with the deletion — the way a user-data erasure re-rolls the
	// hours it emptied (spec 013 #7).
	if !hour.Valid {
		return nil
	}
	return correctScoreHours(tx, d.ProjectID, time.Now().UnixNano(), hour.Int64)
}

// ScoreFilter narrows a score listing (spec 003, API contract). The zero value
// lists a project's newest scores.
type ScoreFilter struct {
	TraceID       string
	ObservationID string
	SessionID     string
	Name          string
	DataType      string
	// AuthorID keeps the scores one account or one key wrote (spec 048 #9).
	AuthorID string
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

// Scores lists scores newest first, each with its author as it stands now.
func (s *Store) Scores(ctx context.Context, projectID string, filter ScoreFilter) ([]*Score, error) {
	// Qualified throughout: the author's joins bring in tables with an `id`,
	// a `name` and a `project_id` of their own.
	where := []string{"s.project_id = ?"}
	args := []any{projectID}
	add := func(clause string, values ...any) {
		where = append(where, clause)
		args = append(args, values...)
	}
	if filter.TraceID != "" {
		add("s.trace_id = ?", filter.TraceID)
	}
	if filter.ObservationID != "" {
		add("s.observation_id = ?", filter.ObservationID)
	}
	if filter.SessionID != "" {
		add("s.session_id = ?", filter.SessionID)
	}
	if filter.Name != "" {
		add("s.name = ?", filter.Name)
	}
	if filter.DataType != "" {
		add("s.data_type = ?", filter.DataType)
	}
	if filter.AuthorID != "" {
		add("s.author_id = ?", filter.AuthorID)
	}
	if filter.From != nil {
		add("s.timestamp >= ?", *filter.From)
	}
	if filter.To != nil {
		add("s.timestamp < ?", *filter.To)
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
		add("(s.timestamp, s.id) < (?, ?)", filter.After.Timestamp, filter.After.ID)
	}
	args = append(args, filter.Limit)

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+authoredScoreColumns+authoredScoreFrom+`
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY s.timestamp DESC, s.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list scores: %w", err)
	}
	defer rows.Close()

	var out []*Score
	for rows.Next() {
		score, err := scanAuthoredScore(rows)
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

// authoredScoreColumns and authoredScoreFrom read a score with its author's
// standing now (spec 048 #4), for a query that names `scores` as `s`: one
// LEFT JOIN each to the account, its membership in the score's project and
// the key, all by primary key. The run screens read scoreColumns alone: their
// attempt scores carry no author (#10).
const (
	authoredScoreColumns = `s.id, s.trace_id, s.observation_id, s.session_id, s.name, s.data_type,
	        s.value, s.string_value, s.comment, s.metadata, s.timestamp, s.created_at,
	        s.author_kind, s.author_id, s.author_name, s.author_email,
	        a.id IS NOT NULL, COALESCE(a.owner, 0), COALESCE(a.disabled, 0),
	        COALESCE(m.role, ''), COALESCE(a.name, ''), k.public_key IS NOT NULL`
	authoredScoreFrom = `
	   FROM scores s
	   LEFT JOIN accounts a ON s.author_kind = 'account' AND a.id = s.author_id
	   LEFT JOIN memberships m ON s.author_kind = 'account'
	         AND m.account_id = s.author_id AND m.project_id = s.project_id
	   LEFT JOIN api_keys k ON s.author_kind = 'key'
	         AND k.public_key = s.author_id AND k.project_id = s.project_id`
)

// Score returns one score with its author, or nil when it does not exist.
func (s *Store) Score(ctx context.Context, projectID, id string) (*Score, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+authoredScoreColumns+authoredScoreFrom+`
		 WHERE s.project_id = ? AND s.id = ?`, projectID, id)
	if err != nil {
		return nil, fmt.Errorf("read score %s: %w", id, err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	return scanAuthoredScore(rows)
}

// ScoresAuthoredBy counts the scores an account wrote, in every project, soft
// deleted ones included (spec 048 #7): what deleting the account leaves its
// name on. One seek into idx_scores_author per project. The kind needs no
// test: an account id is 32 hex characters and a public key starts `tp-pk-`.
func (s *Store) ScoresAuthoredBy(ctx context.Context, accountID string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*)
	   FROM projects p JOIN scores s ON s.project_id = p.id AND s.author_id = ?`, accountID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count the scores an account wrote: %w", err)
	}
	return n, nil
}

// scanScore reads scoreColumns, then whatever else the query selected after
// them into extra.
func scanScore(rows *sql.Rows, extra ...any) (*Score, error) {
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
	if err := rows.Scan(append([]any{&score.ID, &traceID, &observationID, &sessionID, &score.Name,
		&score.DataType, &value, &stringValue, &comment, &metadata,
		&score.Timestamp, &score.CreatedAt}, extra...)...); err != nil {
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

// scanAuthoredScore reads authoredScoreColumns.
func scanAuthoredScore(rows *sql.Rows) (*Score, error) {
	var (
		kind, id, name, email   sql.NullString
		exists, owner, disabled bool
		role, nameNow           string
		keyExists               bool
	)
	score, err := scanScore(rows, &kind, &id, &name, &email,
		&exists, &owner, &disabled, &role, &nameNow, &keyExists)
	if err != nil || !kind.Valid {
		return score, err
	}
	author := &ScoreAuthor{Kind: kind.String, ID: id.String, Name: name.String, Email: email.String}
	switch author.Kind {
	case AuthorAccount:
		author.Standing = standing(exists, owner, disabled, role)
		if exists && nameNow != "" {
			author.Name = nameNow
		}
	case AuthorKey:
		author.Standing = StandingRevoked
		if keyExists {
			author.Standing = StandingActive
		}
	}
	score.Author = author
	return score, nil
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
