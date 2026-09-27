package store

import (
	"context"
	"database/sql"
	"fmt"
)

// MaxTreeObservations is the most observations one trace's tree holds
// (spec 043 #18): the tree is read from the longest prefix of the trace's
// observations in (start_time, id) order, and the store reads no more rows
// than this. A constant, not a setting: it shapes the answer a client parses.
const MaxTreeObservations = 10_000

// TreeObservations reads a trace's observations for its tree: in (start_time,
// id) order, at most limit of them, without their payloads, each handed to
// keep as it is scanned. The read stops at the first row keep refuses, so a
// caller that measures what it keeps holds no more than it keeps. Each row
// carries the references its payloads are read by (PayloadReader).
func (s *Store) TreeObservations(ctx context.Context, projectID, traceID string, limit int,
	keep func(*ObservationRow) bool) error {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+observationColumns+`
		 `+observationFrom+`
		 WHERE o.project_id = ? AND o.trace_id = ?
		 ORDER BY o.start_time, o.id
		 LIMIT ?`, projectID, traceID, limit)
	if err != nil {
		return fmt.Errorf("read observations of %s: %w", traceID, err)
	}
	defer rows.Close()
	for rows.Next() {
		row, err := s.scanObservation(ctx, rows, SkipIO, nil)
		if err != nil {
			return err
		}
		if !keep(row) {
			return nil
		}
	}
	return rows.Err()
}

// PayloadKind names one of an observation's three payloads.
type PayloadKind int

// The three payloads, in the order a tree renders them.
const (
	PayloadInput PayloadKind = iota
	PayloadOutput
	PayloadMetadata
)

// PayloadKinds is every kind, in rendering order.
var PayloadKinds = []PayloadKind{PayloadInput, PayloadOutput, PayloadMetadata}

// Key is the field a payload renders under.
func (k PayloadKind) Key() string {
	switch k {
	case PayloadInput:
		return "input"
	case PayloadOutput:
		return "output"
	}
	return "metadata"
}

// ref is the payload row one kind of this observation's payload lives in.
func (o *ObservationRow) ref(kind PayloadKind) sql.NullInt64 {
	switch kind {
	case PayloadInput:
		return o.inputID
	case PayloadOutput:
		return o.outputID
	}
	return o.metadataID
}

// HasPayload reports whether the observation carries this payload, without
// reading it.
func (o *ObservationRow) HasPayload(kind PayloadKind) bool {
	return o.ref(kind).Valid
}

// PayloadReader reads the payloads of one trace's observations one at a time
// (spec 043 #18), resolving Langfuse references as a read WithIO does
// (spec 041 #21): the bodies a trace's refs hold are asked for once however
// many of its payloads carry a reference.
type PayloadReader struct {
	store *Store
	reads mediaReads
}

// PayloadReader starts one read's payloads.
func (s *Store) PayloadReader() *PayloadReader {
	return &PayloadReader{store: s}
}

// Read decodes one payload of an observation, nil when it carries none. A
// metadata payload is always an object; anything else in its place reads as
// none, as a read WithIO has it.
func (p *PayloadReader) Read(ctx context.Context, row *ObservationRow, kind PayloadKind) (any, error) {
	value, err := p.store.readPayload(ctx, row.ref(kind), row.ProjectID, row.TraceID, &p.reads)
	if err != nil || kind != PayloadMetadata || value == nil {
		return value, err
	}
	if object, ok := value.(map[string]any); ok {
		return object, nil
	}
	return nil, nil
}
