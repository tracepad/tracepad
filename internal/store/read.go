package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
)

// The row types the read API renders and the single-row reads behind them.
// Filtering, paging and aggregation live in query.go; this file is the shape
// of a stored trace and how its payloads are resolved.

// TraceRow is a stored trace with its denormalized aggregates.
type TraceRow struct {
	ProjectID        string
	ID               string
	Name             string
	UserID           string
	SessionID        string
	Environment      string
	Tags             []string
	Metadata         map[string]any
	Timestamp        int64
	TotalCost        *float64
	LatencyMs        *int64
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
	Model               string
	ModelParameters     map[string]any
	Level               string
	StatusMessage       string
	Usage               map[string]any
	CostDetails         map[string]any
	ProvidedCost        bool
	Input               any
	Output              any
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
	if err == errNoRow {
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

// errNoRow marks the absence scanTrace saw, so callers can tell "no such
// trace" from a real read failure.
var errNoRow = sql.ErrNoRows

func scanTrace(rows scanner, extra ...any) (*TraceRow, error) {
	var (
		row       TraceRow
		name      sql.NullString
		userID    sql.NullString
		sessionID sql.NullString
		tags      sql.NullString
		timestamp sql.NullInt64
		totalCost sql.NullFloat64
		latency   sql.NullInt64
	)
	targets := []any{&row.ProjectID, &row.ID, &name, &userID, &sessionID, &row.Environment,
		&tags, &timestamp, &totalCost, &latency, &row.ErrorCount, &row.ObservationCount}
	if err := rows.Scan(append(targets, extra...)...); err != nil {
		if err == errNoRow {
			return nil, err
		}
		return nil, fmt.Errorf("scan trace: %w", err)
	}
	row.Name, row.UserID, row.SessionID = name.String, userID.String, sessionID.String
	row.Timestamp = timestamp.Int64
	if totalCost.Valid {
		row.TotalCost = &totalCost.Float64
	}
	if latency.Valid {
		row.LatencyMs = &latency.Int64
	}
	if tags.Valid {
		if err := json.Unmarshal([]byte(tags.String), &row.Tags); err != nil {
			return nil, fmt.Errorf("decode trace tags %s: %w", row.ID, err)
		}
	}
	return &row, nil
}

const observationColumns = `project_id, trace_id, id, parent_observation_id, type, name,
	        start_time, end_time, model, model_parameters, level, status_message,
	        usage, cost_details, provided_cost, input_id, output_id, metadata_id`

// Observations returns a trace's spans ordered by start time.
func (s *Store) Observations(projectID, traceID string, io IOMode) ([]*ObservationRow, error) {
	rows, err := s.db.Query(
		`SELECT `+observationColumns+`
		 FROM observations WHERE project_id = ? AND trace_id = ?
		 ORDER BY start_time, id`, projectID, traceID)
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
		modelName       sql.NullString
		modelParameters sql.NullString
		statusMessage   sql.NullString
		usage           sql.NullString
		costDetails     sql.NullString
		providedCost    int
		inputID         sql.NullInt64
		outputID        sql.NullInt64
		metadataID      sql.NullInt64
	)
	if err := rows.Scan(&row.ProjectID, &row.TraceID, &row.ID, &parent, &row.Type, &name,
		&row.StartTime, &row.EndTime, &modelName, &modelParameters, &row.Level, &statusMessage,
		&usage, &costDetails, &providedCost, &inputID, &outputID, &metadataID); err != nil {
		return nil, fmt.Errorf("scan observation: %w", err)
	}
	row.ParentObservationID, row.Name, row.Model = parent.String, name.String, modelName.String
	row.StatusMessage, row.ProvidedCost = statusMessage.String, providedCost != 0

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
