package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// The row types the read API renders and the single-row reads behind them.
// Filtering, paging and aggregation live in query.go; this file is the shape
// of a stored trace and how its payloads are resolved.

// TraceRow is a stored trace with its denormalized aggregates.
type TraceRow struct {
	ProjectID   string
	ID          string
	Name        string
	UserID      string
	SessionID   string
	Environment string
	// Release is the deployment the trace ran in, Version the version of
	// its own logic; both are empty when no delivery said (spec 012 #4).
	Release   string
	Version   string
	Tags      []string
	Metadata  map[string]any
	Timestamp int64
	TotalCost *float64
	LatencyMs *int64
	// TTFTMs is the wait before the first token of the trace's earliest
	// completion, nil when no observation carried a completion start.
	TTFTMs           *int64
	ErrorCount       int
	ObservationCount int
}

// ObservationRow is a stored span. Input, Output and Metadata are resolved
// only when the read asked for them (IOMode): a trace tree that does not
// expand its payloads must not pay for reading them.
type ObservationRow struct {
	ProjectID           string
	TraceID             string
	ID                  string
	ParentObservationID string
	Type                string
	Name                string
	StartTime           int64
	EndTime             int64
	// CompletionStartTime is when the first token came back, zero when the
	// client did not say; TTFTMs is the wait it implies (spec 012 #3).
	CompletionStartTime int64
	Model               string
	ModelParameters     map[string]any
	Level               string
	StatusMessage       string
	Usage               map[string]any
	CostDetails         map[string]any
	ProvidedCost        bool
	// PromptName and PromptVersion are the label the client put on this
	// observation, resolved against no registry (spec 012 #5).
	PromptName    string
	PromptVersion *int64
	// InputBytes and OutputBytes are the uncompressed sizes of the two
	// payloads, read from `payloads` rather than stored a second time
	// (spec 012 #6). Nil when there is no payload — which is not the same
	// as a payload of zero bytes.
	InputBytes  *int64
	OutputBytes *int64
	Input       any
	Output      any
	// Metadata is always an object: mapping builds it from attributes
	// (spec 002), unlike Input and Output, which are whatever the client
	// logged.
	Metadata map[string]any
}

// IOMode selects whether a span's payloads are read along with its row.
type IOMode bool

// The two IO modes, named at the call site because `true` says nothing there.
const (
	SkipIO IOMode = false
	WithIO IOMode = true
)

// RawBatchRow is a stored export body, decompressed.
type RawBatchRow struct {
	ID              int64
	ProjectID       string
	ReceivedAt      int64
	Dialect         string
	ContentEncoding string
	Body            []byte
}

// Trace returns one trace with its metadata, or nil when it does not exist.
// Metadata is a payload and so is absent from a list row (spec 004, API
// contract); a single trace is where it belongs.
func (s *Store) Trace(projectID, id string) (*TraceRow, error) {
	var metadataID sql.NullInt64
	row, err := scanTrace(s.db.QueryRow(
		`SELECT `+traceColumns+`, metadata_id FROM traces WHERE project_id = ? AND id = ?`,
		projectID, id), &metadataID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	metadata, err := s.readPayload(metadataID)
	if err != nil {
		return nil, err
	}
	object, _ := metadata.(map[string]any)
	row.Metadata = object
	return row, nil
}

// scanner is the half of *sql.Row and *sql.Rows that scanTrace needs, so one
// scan serves both the single read and the listing.
type scanner interface{ Scan(dest ...any) error }

// scanTrace reads one row, optionally with extra columns the caller selected
// beyond the shared list-row shape. sql.ErrNoRows passes through unwrapped so
// a caller can tell "no such trace" from a read that failed.
func scanTrace(rows scanner, extra ...any) (*TraceRow, error) {
	var (
		row       TraceRow
		name      sql.NullString
		userID    sql.NullString
		sessionID sql.NullString
		release   sql.NullString
		version   sql.NullString
		tags      sql.NullString
		timestamp sql.NullInt64
		totalCost sql.NullFloat64
		latency   sql.NullInt64
		ttft      sql.NullInt64
	)
	targets := []any{&row.ProjectID, &row.ID, &name, &userID, &sessionID, &row.Environment,
		&release, &version, &tags, &timestamp, &totalCost, &latency, &ttft,
		&row.ErrorCount, &row.ObservationCount}
	if err := rows.Scan(append(targets, extra...)...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan trace: %w", err)
	}
	row.Name, row.UserID, row.SessionID = name.String, userID.String, sessionID.String
	row.Release, row.Version = release.String, version.String
	row.Timestamp = timestamp.Int64
	if totalCost.Valid {
		row.TotalCost = &totalCost.Float64
	}
	if latency.Valid {
		row.LatencyMs = &latency.Int64
	}
	if ttft.Valid {
		row.TTFTMs = &ttft.Int64
	}
	if tags.Valid {
		if err := json.Unmarshal([]byte(tags.String), &row.Tags); err != nil {
			return nil, fmt.Errorf("decode trace tags %s: %w", row.ID, err)
		}
	}
	return &row, nil
}

const observationColumns = `o.project_id, o.trace_id, o.id, o.parent_observation_id, o.type, o.name,
	        o.start_time, o.end_time, o.completion_start_time, o.model, o.model_parameters,
	        o.level, o.status_message, o.usage, o.cost_details, o.provided_cost,
	        o.prompt_name, o.prompt_version, i.size_raw, u.size_raw,
	        o.input_id, o.output_id, o.metadata_id`

// observationFrom joins the payload rows the sizes come from. Two outer joins
// on the primary key of `payloads`, which the row already references: the
// sizes are one select away, and a denormalized copy would be two columns to
// keep in step with a number nothing filters on (spec 012 #6).
const observationFrom = `FROM observations o
	        LEFT JOIN payloads i ON i.id = o.input_id
	        LEFT JOIN payloads u ON u.id = o.output_id`

// Observations returns a trace's spans ordered by start time.
func (s *Store) Observations(projectID, traceID string, io IOMode) ([]*ObservationRow, error) {
	rows, err := s.db.Query(
		`SELECT `+observationColumns+`
		 `+observationFrom+`
		 WHERE o.project_id = ? AND o.trace_id = ?
		 ORDER BY o.start_time, o.id`, projectID, traceID)
	if err != nil {
		return nil, fmt.Errorf("read observations of %s: %w", traceID, err)
	}
	defer rows.Close()

	var out []*ObservationRow
	for rows.Next() {
		row, err := s.scanObservation(rows, io)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) scanObservation(rows *sql.Rows, io IOMode) (*ObservationRow, error) {
	var (
		row             ObservationRow
		parent          sql.NullString
		name            sql.NullString
		completionStart sql.NullInt64
		modelName       sql.NullString
		modelParameters sql.NullString
		statusMessage   sql.NullString
		usage           sql.NullString
		costDetails     sql.NullString
		providedCost    int
		promptName      sql.NullString
		promptVersion   sql.NullInt64
		inputBytes      sql.NullInt64
		outputBytes     sql.NullInt64
		inputID         sql.NullInt64
		outputID        sql.NullInt64
		metadataID      sql.NullInt64
	)
	if err := rows.Scan(&row.ProjectID, &row.TraceID, &row.ID, &parent, &row.Type, &name,
		&row.StartTime, &row.EndTime, &completionStart, &modelName, &modelParameters,
		&row.Level, &statusMessage, &usage, &costDetails, &providedCost,
		&promptName, &promptVersion, &inputBytes, &outputBytes,
		&inputID, &outputID, &metadataID); err != nil {
		return nil, fmt.Errorf("scan observation: %w", err)
	}
	row.ParentObservationID, row.Name, row.Model = parent.String, name.String, modelName.String
	row.StatusMessage, row.ProvidedCost = statusMessage.String, providedCost != 0
	row.CompletionStartTime, row.PromptName = completionStart.Int64, promptName.String
	if promptVersion.Valid {
		row.PromptVersion = &promptVersion.Int64
	}
	if inputBytes.Valid {
		row.InputBytes = &inputBytes.Int64
	}
	if outputBytes.Valid {
		row.OutputBytes = &outputBytes.Int64
	}

	var err error
	if row.ModelParameters, err = decodeObject(modelParameters); err != nil {
		return nil, err
	}
	if row.Usage, err = decodeObject(usage); err != nil {
		return nil, err
	}
	if row.CostDetails, err = decodeObject(costDetails); err != nil {
		return nil, err
	}
	if !io {
		return &row, nil
	}
	if row.Input, err = s.readPayload(inputID); err != nil {
		return nil, err
	}
	if row.Output, err = s.readPayload(outputID); err != nil {
		return nil, err
	}
	metadata, err := s.readPayload(metadataID)
	if err != nil {
		return nil, err
	}
	if metadata != nil {
		object, _ := metadata.(map[string]any)
		row.Metadata = object
	}
	return &row, nil
}

// RawBatches returns the stored export bodies for a project, oldest first.
func (s *Store) RawBatches(projectID string) ([]RawBatchRow, error) {
	rows, err := s.db.Query(
		`SELECT id, project_id, received_at, dialect, content_encoding, body
		 FROM raw_batches WHERE project_id = ? ORDER BY received_at, id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("read raw batches: %w", err)
	}
	defer rows.Close()

	var out []RawBatchRow
	for rows.Next() {
		var (
			row      RawBatchRow
			dialect  sql.NullString
			encoding sql.NullString
			body     []byte
		)
		if err := rows.Scan(&row.ID, &row.ProjectID, &row.ReceivedAt, &dialect, &encoding, &body); err != nil {
			return nil, err
		}
		row.Dialect, row.ContentEncoding = dialect.String, encoding.String
		if row.Body, err = Decompress(CompressionZstd, body); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// FileSize reports the database's footprint on disk: the main file plus the
// write-ahead log, which is where recently committed rows still live.
func (s *Store) FileSize() int64 {
	var total int64
	for _, suffix := range []string{"", "-wal"} {
		if info, err := os.Stat(s.path + suffix); err == nil {
			total += info.Size()
		}
	}
	return total
}

// readPayload resolves a payload reference into the value it holds.
func (s *Store) readPayload(id sql.NullInt64) (any, error) {
	if !id.Valid {
		return nil, nil
	}
	var (
		compression string
		body        []byte
	)
	if err := s.db.QueryRow(`SELECT compression, body FROM payloads WHERE id = ?`, id.Int64).
		Scan(&compression, &body); err != nil {
		return nil, fmt.Errorf("read payload %d: %w", id.Int64, err)
	}
	raw, err := Decompress(compression, body)
	if err != nil {
		return nil, err
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode payload %d: %w", id.Int64, err)
	}
	return out, nil
}

func decodeObject(v sql.NullString) (map[string]any, error) {
	if !v.Valid {
		return nil, nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(v.String), &out); err != nil {
		return nil, fmt.Errorf("decode json column: %w", err)
	}
	return out, nil
}
