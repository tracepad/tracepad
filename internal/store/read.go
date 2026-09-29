package store

import (
	"bytes"
	"context"
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
	Release string
	Version string
	// RunID and ItemID are the link to a dataset run and the item the
	// trace answered, as the client stamped them (spec 014 #2); empty when
	// it stamped none. No foreign key stands behind either.
	RunID     string
	ItemID    string
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
	// Tokens are the five classes summed over the trace's observations that
	// name a model, maintained on write like the cost (spec 049 #2).
	Tokens Tokens
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
	// inputID, outputID and metadataID are the payload rows the three
	// payloads live in, which a PayloadReader reads one at a time.
	inputID, outputID, metadataID sql.NullInt64
}

// IOMode selects whether a span's payloads are read along with its row.
type IOMode bool

// The two IO modes, named at the call site because `true` says nothing there.
const (
	SkipIO IOMode = false
	WithIO IOMode = true
)

// Trace returns one trace with its metadata, or nil when it does not exist.
// Metadata is a payload and so is absent from a list row (spec 004, API
// contract); a single trace is where it belongs.
func (s *Store) Trace(ctx context.Context, projectID, id string) (*TraceRow, error) {
	var metadataID sql.NullInt64
	row, err := scanTrace(s.db.QueryRowContext(ctx,
		`SELECT `+traceColumns+`, metadata_id FROM traces WHERE project_id = ? AND id = ?`,
		projectID, id), &metadataID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	metadata, err := s.readPayload(ctx, metadataID, projectID, id, nil)
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
		runID     sql.NullString
		itemID    sql.NullString
		tags      sql.NullString
		timestamp sql.NullInt64
		totalCost sql.NullFloat64
		latency   sql.NullInt64
		ttft      sql.NullInt64
		tokens    tokenScan
	)
	targets := append([]any{&row.ProjectID, &row.ID, &name, &userID, &sessionID, &row.Environment,
		&release, &version, &runID, &itemID, &tags, &timestamp, &totalCost, &latency, &ttft,
		&row.ErrorCount, &row.ObservationCount}, tokens.targets()...)
	if err := rows.Scan(append(targets, extra...)...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("scan trace: %w", err)
	}
	row.Name, row.UserID, row.SessionID = name.String, userID.String, sessionID.String
	row.Release, row.Version = release.String, version.String
	row.RunID, row.ItemID = runID.String, itemID.String
	row.Timestamp = timestamp.Int64
	row.Tokens = tokens.tokens()
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

// Observations returns a trace's spans ordered by start time, with their
// payloads when io asks. The tree reads through TreeObservations and a
// PayloadReader; this is the whole read, for checks and tests of what a write
// stored. Payloads are read once the rows are, so a read never holds its
// cursor open while it asks for another statement.
func (s *Store) Observations(ctx context.Context, projectID, traceID string, io IOMode) ([]*ObservationRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+observationColumns+`
		 `+observationFrom+`
		 WHERE o.project_id = ? AND o.trace_id = ?
		 ORDER BY o.start_time, o.id`, projectID, traceID)
	if err != nil {
		return nil, fmt.Errorf("read observations of %s: %w", traceID, err)
	}
	var out []*ObservationRow
	for rows.Next() {
		row, err := scanObservation(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil || !io {
		return out, err
	}
	reads := &mediaReads{}
	for _, row := range out {
		if err := s.resolvePayloads(ctx, row, reads); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scanObservation reads one row, its payloads left as the references a
// PayloadReader or resolvePayloads reads them by.
func scanObservation(rows *sql.Rows) (*ObservationRow, error) {
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
	row.inputID, row.outputID, row.metadataID = inputID, outputID, metadataID
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
	return &row, nil
}

// resolvePayloads reads an observation's three payloads into it; reads
// memoises the Langfuse resolution of Decision 21 across the rows of one read,
// and may be nil.
func (s *Store) resolvePayloads(ctx context.Context, row *ObservationRow, reads *mediaReads) error {
	var err error
	if row.Input, err = s.readPayload(ctx, row.inputID, row.ProjectID, row.TraceID, reads); err != nil {
		return err
	}
	if row.Output, err = s.readPayload(ctx, row.outputID, row.ProjectID, row.TraceID, reads); err != nil {
		return err
	}
	metadata, err := s.readPayload(ctx, row.metadataID, row.ProjectID, row.TraceID, reads)
	if err != nil {
		return err
	}
	if metadata != nil {
		object, _ := metadata.(map[string]any)
		row.Metadata = object
	}
	return nil
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

// readPayload resolves a payload reference into the value it holds, for one
// trace of a project: a Langfuse reference string the trace's upload has
// since caught up with reads as the reference (spec 041, Decision 21).
func (s *Store) readPayload(ctx context.Context, id sql.NullInt64, projectID, traceID string, reads *mediaReads) (any, error) {
	if !id.Valid {
		return nil, nil
	}
	var (
		compression string
		body        []byte
	)
	if err := s.db.QueryRowContext(ctx, `SELECT compression, body FROM payloads WHERE id = ?`, id.Int64).
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
	if bytes.Contains(raw, langfuseMarker) {
		if reads == nil {
			reads = &mediaReads{}
		}
		return reads.resolve(ctx, s, out, projectID, traceID)
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
