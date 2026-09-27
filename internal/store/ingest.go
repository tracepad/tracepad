package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/tracepad/tracepad/internal/model"
)

// IngestBatch is one mapped export handed to the writer.
type IngestBatch struct {
	ProjectID    string
	Traces       []*model.Trace
	Observations []*model.Observation
	// Raw is the body to keep for a later remap; nil when raw storage is
	// off (spec 002 #9).
	Raw *RawBatch
	// Media are the bodies ingest took out of the payloads (spec 041 #2),
	// MediaRefs the traces pointing at them (#3), and RawMedia the bodies
	// the raw body points at, which become the raw batch's own refs
	// (Decision 12).
	Media     []MediaBody
	MediaRefs []MediaRef
	RawMedia  []string
	// Resolved are the bodies a Langfuse reference string was rewritten
	// to, read before this write: if one is gone the batch is refused with
	// ErrMediaGone rather than stored pointing at nothing (spec 041 #9).
	Resolved []string
	// IngestedAt is the server clock at arrival, which is the clock
	// retention counts from (spec 005 #1). Zero means "now", resolved in
	// apply so that every path into the writer has an arrival time even
	// when the caller did not think to set one.
	IngestedAt int64
	// skipSearchIndex writes the batch without indexing it, which is the
	// "without it" half of the pair of numbers spec 011, Testing #5 asks
	// for. Unexported on purpose: it is reachable only from inside this
	// package, so no caller can produce a store whose index silently
	// disagrees with its rows.
	skipSearchIndex bool

	// full says this is a slice with another after it (Slices): it weighs a
	// whole window whatever its rows (weight).
	full bool
	// continued are the traces this slice carries on from an earlier slice
	// of its export (Slices), which apply touches rather than upserts.
	continued map[string]bool
	// stamped, on the slice that carries the raw body, are the export's
	// traces the earlier slices wrote: apply stamps their `updated_at` with
	// the reading the raw batch gets, so that each trace's window still
	// holds the batch its spans arrived in (spec 043 #31, spec 044 #3).
	stamped []string

	// UnknownRuns is filled by apply: the run id of every trace in the
	// batch that named a run this project does not have, one entry per
	// such trace (spec 014 #3). The trace is stored with its columns all
	// the same — refusing it would drop the one artifact that shows what
	// went wrong — and the handler counts and logs what it finds here.
	UnknownRuns []string
}

// RawBatch is the request body as received, kept verbatim for replay.
type RawBatch struct {
	// ReceivedAt is zero on the way in from ingest: the writer stamps the
	// batch with the reading it gives the batch's traces (`IngestedAt`), so
	// a trace's spans arrive in batches inside its own `[ingested_at,
	// updated_at]` — the window an erasure reads the archive by (spec 044
	// #3). A caller seeding an archive sets it.
	ReceivedAt int64
	Dialect    string
	// ContentType is the encoding the body is in — the media type the
	// client sent it under (spec 019 #8). Empty means the protobuf
	// encoding, which is what every row written before schema 0012 holds.
	ContentType string
	// ContentEncoding records how the client sent it, for provenance; the
	// stored bytes are always the decoded body, so a remap does not have to
	// unwrap two layers.
	ContentEncoding string
	Body            []byte
}

// SliceRows is the most rows — traces and observations — one ingest job
// carries (spec 043 #11): the length of time an export holds the only writer
// is the length of its largest slice, not of the export.
const SliceRows = 1000

// Slices cuts a batch into jobs of at most SliceRows rows, to be submitted one
// after another, each once the one before it has committed (spec 043 #11). A
// batch that fits is its own one slice, unchanged.
//
// A trace travels with its observations. One larger than what is left of a
// slice goes on in the next, which carries it as continued: apply stamps its
// `updated_at`, so the rollup sees the hours the slice changed, and
// recomputes its aggregates over what is stored so far (spec 002 #22), without
// writing its fields and metadata again or looking its run up again. Only
// were it deleted since the slice before is it written whole, as a late export
// would write it (spec 043 #31). A trace is not started at the end of a slice
// with no room for one of its observations: a trace row with none would be
// listed without a time.
//
// Each slice carries the refs of the traces it writes, each body in the first
// slice that names it, and the check that the resolved Langfuse ids its refs
// name still name bodies the project holds (spec 041 #9); the last carries the
// raw body, its refs, and every body the raw body names (spec 043 #32, #33). The raw batch
// is stamped with the last slice's reading (spec 044 #3), so that slice stamps
// the traces the earlier ones wrote with it too: an erasure finds a trace's
// batches inside its `[ingested_at, updated_at]`.
func (b *IngestBatch) Slices() []*IngestBatch {
	if len(b.Traces)+len(b.Observations) <= SliceRows {
		return []*IngestBatch{b}
	}
	var (
		slices []*IngestBatch
		cur    *IngestBatch
		rows   int
	)
	open := func() {
		cur = &IngestBatch{ProjectID: b.ProjectID, IngestedAt: b.IngestedAt, skipSearchIndex: b.skipSearchIndex}
		slices = append(slices, cur)
		rows = 0
	}
	open()

	// Each trace's observations, in the order the batch holds them; one
	// whose trace the batch does not carry goes last, with no trace row.
	byTrace := make(map[string][]*model.Observation, len(b.Traces))
	carried := make(map[string]bool, len(b.Traces))
	for _, t := range b.Traces {
		carried[t.ID] = true
	}
	var strays []*model.Observation
	for _, o := range b.Observations {
		if carried[o.TraceID] {
			byTrace[o.TraceID] = append(byTrace[o.TraceID], o)
		} else {
			strays = append(strays, o)
		}
	}
	for _, t := range b.Traces {
		observations := byTrace[t.ID]
		if rows > 0 && rows+min(len(observations), 1)+1 > SliceRows {
			open()
		}
		for {
			cur.Traces = append(cur.Traces, t)
			rows++
			n := min(len(observations), SliceRows-rows)
			cur.Observations = append(cur.Observations, observations[:n]...)
			rows += n
			observations = observations[n:]
			if len(observations) == 0 {
				break
			}
			open()
			if cur.continued == nil {
				cur.continued = map[string]bool{}
			}
			cur.continued[t.ID] = true
		}
	}
	for len(strays) > 0 {
		if rows == SliceRows {
			open()
		}
		n := min(len(strays), SliceRows-rows)
		cur.Observations = append(cur.Observations, strays[:n]...)
		rows += n
		strays = strays[n:]
	}

	// Each slice carries the refs of the traces it writes, settled in the
	// same transaction as the trace, and each body in the first slice that
	// names it — the slices commit in order, so a ref in a later one finds it
	// stored. A slice checks the resolved Langfuse ids its own refs name
	// (spec 041 #9). The last also carries every body the raw body names,
	// beside the raw refs to them, so a deletion between the slices cannot
	// leave the archived batch pointing at nothing.
	bodies := make(map[string]MediaBody, len(b.Media))
	for _, body := range b.Media {
		if _, seen := bodies[body.SHA256]; !seen {
			bodies[body.SHA256] = body
		}
	}
	refsOf := make(map[string][]MediaRef, len(b.MediaRefs))
	for _, ref := range b.MediaRefs {
		refsOf[ref.TraceID] = append(refsOf[ref.TraceID], ref)
	}
	resolved := make(map[string]bool, len(b.Resolved))
	for _, sha := range b.Resolved {
		resolved[sha] = true
	}
	stored := map[string]bool{}
	for i, slice := range slices {
		named := map[string]bool{}
		carry := func(sha string, again bool) {
			if named[sha] || (stored[sha] && !again) {
				return
			}
			if body, ok := bodies[sha]; ok {
				named[sha] = true
				stored[sha] = true
				slice.Media = append(slice.Media, body)
			}
		}
		checked := map[string]bool{}
		for _, t := range slice.Traces {
			for _, ref := range refsOf[t.ID] {
				slice.MediaRefs = append(slice.MediaRefs, ref)
				carry(ref.SHA256, false)
				if resolved[ref.SHA256] && !checked[ref.SHA256] {
					checked[ref.SHA256] = true
					slice.Resolved = append(slice.Resolved, ref.SHA256)
				}
			}
		}
		if i == len(slices)-1 {
			for _, sha := range b.RawMedia {
				carry(sha, true)
			}
		}
	}
	last := slices[len(slices)-1]
	last.Raw, last.RawMedia = b.Raw, b.RawMedia
	if last.Raw != nil {
		inLast := make(map[string]bool, len(last.Traces))
		for _, t := range last.Traces {
			inLast[t.ID] = true
		}
		for _, t := range b.Traces {
			if !inLast[t.ID] {
				last.stamped = append(last.stamped, t.ID)
			}
		}
	}
	for _, slice := range slices[:len(slices)-1] {
		slice.full = true
	}
	return slices
}

// weight is a slice's rows (spec 043 #12). One with another after it weighs a
// whole window, so that it closes its window at once whatever the two bounds
// are: its export's next slice cannot arrive until it commits, and waiting out
// the window for it would add the window to every slice.
func (b *IngestBatch) weight() int {
	if b.full {
		return WindowRows
	}
	return len(b.Traces) + len(b.Observations)
}

// Empty reports a batch with nothing to write.
func (b *IngestBatch) Empty() bool {
	return len(b.Traces) == 0 && len(b.Observations) == 0 && b.Raw == nil
}

// apply writes one batch inside the caller's transaction. Traces come first
// so the aggregate pass at the end always finds its row.
//
// The search index (spec 011 #7) is written here too, in this same
// transaction: an index that lags the data answers with traces that are not
// there.
func (b *IngestBatch) apply(tx *sql.Tx) error {
	arrived := b.IngestedAt
	if arrived == 0 {
		arrived = time.Now().UnixNano()
	}
	if err := mediaStillThere(tx, b.ProjectID, b.Resolved); err != nil {
		return err
	}
	// The bodies first: every ref below, the raw batch's included, names
	// a row that has to exist (spec 041 #2).
	types := declaredTypes(b.Media)
	held, err := writeMedia(tx, b.ProjectID, b.Media, types, b.MediaRefs, arrived)
	if err != nil {
		return err
	}
	if b.Raw != nil {
		// Always zstd, unlike payloads: the table has no compression
		// column, so one encoding keeps replay unambiguous, and an OTLP
		// body small enough for the threshold to matter is not a body
		// worth a special case.
		received := b.Raw.ReceivedAt
		if received == 0 {
			received = arrived
		}
		var rawID int64
		if err := tx.QueryRow(
			`INSERT INTO raw_batches (project_id, received_at, dialect, content_type, content_encoding, body)
			 VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
			b.ProjectID, received, b.Raw.Dialect,
			nullString(b.Raw.ContentType), nullString(b.Raw.ContentEncoding),
			zstdEncoder.EncodeAll(b.Raw.Body, nil),
		).Scan(&rawID); err != nil {
			return fmt.Errorf("store raw batch: %w", err)
		}
		if err := writeRawMediaRefs(tx, b.ProjectID, rawID, b.RawMedia, types, held, arrived); err != nil {
			return err
		}
		stamped := make([]any, len(b.stamped))
		for i, id := range b.stamped {
			stamped[i] = id
		}
		if _, err := deleteIn(tx, `UPDATE traces SET updated_at = ? WHERE project_id = ? AND id IN`,
			[]any{arrived, b.ProjectID}, stamped); err != nil {
			return fmt.Errorf("stamp the traces of a sliced export: %w", err)
		}
	}

	indexing := !b.skipSearchIndex
	b.UnknownRuns = b.UnknownRuns[:0]
	for _, t := range b.Traces {
		if b.continued[t.ID] {
			touched, err := touchTrace(tx, b.ProjectID, t.ID, arrived)
			if err != nil {
				return err
			}
			if touched {
				continue
			}
		}
		if err := upsertTrace(tx, b.ProjectID, t, arrived, indexing); err != nil {
			return err
		}
		// One primary-key lookup per trace that carries the attribute,
		// paid only by eval traffic (spec 014 #3) — once per trace of an
		// export: a continued trace written whole again, after a deletion
		// between two slices, was looked up by the slice before (spec 043
		// #33).
		if t.RunID != "" && !b.continued[t.ID] {
			known, err := runExists(tx, b.ProjectID, t.RunID)
			if err != nil {
				return err
			}
			if !known {
				b.UnknownRuns = append(b.UnknownRuns, t.RunID)
			}
		}
	}
	for _, o := range b.Observations {
		if err := upsertObservation(tx, b.ProjectID, o, indexing); err != nil {
			return err
		}
	}
	for _, t := range b.Traces {
		if err := refreshAggregates(tx, b.ProjectID, t.ID); err != nil {
			return err
		}
	}
	return nil
}

// upsertTrace merges the trace row field by field: a value this delivery did
// not carry leaves the stored one alone, a value it did carry overwrites
// (spec 002 #6). NULL is the wire for "this delivery said nothing", which is
// why every column is bound as a nullable.
//
// `ingested_at` is the exception: it is set when the row is created and never
// touched again, so a trace's retention lease starts once no matter how many
// late spans join it (spec 005 #1). `updated_at` is the opposite stamp and
// exists because of it: the rollup has to know which hours changed since its
// last pass, and a span joining an existing trace changes one without moving
// its arrival (spec 013 #15).
func upsertTrace(tx *sql.Tx, projectID string, t *model.Trace, ingestedAt int64, indexing bool) error {
	metadataID, _, err := writePayload(tx, t.Metadata)
	if err != nil {
		return err
	}
	var tags any
	if len(t.Tags) > 0 {
		encoded, err := json.Marshal(t.Tags)
		if err != nil {
			return fmt.Errorf("encode trace tags: %w", err)
		}
		tags = string(encoded)
	}

	// RETURNING the merged name rather than binding t.Name into the index:
	// a delivery that carried no name leaves the stored one alone, and the
	// index has to hold what the row actually says (spec 011 #2).
	//
	// `run_id` and `item_id` are per-field like the rest (spec 014 #2): a
	// re-delivery naming a different run moves the trace, and one naming
	// none leaves it where it was.
	//
	// The pair moves together, though. Merged independently, a delivery that
	// named a new run and no item would keep the old run's item, and the row
	// would claim that run B answered an item of run A — a made-up fact,
	// which is worse than the missing one it replaces. So a delivery that
	// changes the run says what the item is, including that there is none.
	var stored sql.NullString
	var firstIngested int64
	err = tx.QueryRow(
		`INSERT INTO traces (project_id, id, name, user_id, session_id, environment,
		                     release, version, run_id, item_id, tags, metadata_id,
		                     ingested_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, COALESCE(?, 'default'), ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, id) DO UPDATE SET
		   updated_at  = excluded.updated_at,
		   name        = COALESCE(excluded.name, traces.name),
		   user_id     = COALESCE(excluded.user_id, traces.user_id),
		   session_id  = COALESCE(excluded.session_id, traces.session_id),
		   environment = COALESCE(?, traces.environment),
		   release     = COALESCE(excluded.release, traces.release),
		   version     = COALESCE(excluded.version, traces.version),
		   run_id      = COALESCE(excluded.run_id, traces.run_id),
		   item_id     = CASE
		                   WHEN excluded.run_id IS NOT NULL
		                    AND excluded.run_id IS NOT traces.run_id THEN excluded.item_id
		                   ELSE COALESCE(excluded.item_id, traces.item_id)
		                 END,
		   tags        = COALESCE(excluded.tags, traces.tags),
		   metadata_id = COALESCE(excluded.metadata_id, traces.metadata_id)
		 RETURNING name, ingested_at`,
		projectID, t.ID, nullString(t.Name), nullString(t.UserID), nullString(t.SessionID),
		nullString(t.Environment), nullString(t.Release), nullString(t.Version),
		nullString(t.RunID), nullString(t.ItemID),
		tags, metadataID, ingestedAt, ingestedAt,
		nullString(t.Environment),
	).Scan(&stored, &firstIngested)
	if err != nil {
		return fmt.Errorf("upsert trace %s: %w", t.ID, err)
	}
	// A trace's arrival settles the Langfuse channel's refs to it (spec 041
	// Decision 13), the ones its spans name and the ones they do not: a
	// pending ref is an upload whose trace has not come, and only those
	// count toward the cap (#31). Only a trace this upsert inserted — its
	// arrival keeps its first stamp — can have one; a seek, usually on
	// nothing.
	if firstIngested == ingestedAt {
		if _, err := tx.Exec(`UPDATE media_refs SET pending = 0
		                       WHERE project_id = ? AND trace_id = ? AND pending = 1`, projectID, t.ID); err != nil {
			return fmt.Errorf("settle the media refs of trace %s: %w", t.ID, err)
		}
	}
	if !indexing {
		return nil
	}
	return indexTraceName(tx, projectID, t.ID, stored.String)
}

// touchTrace stamps a stored trace's `updated_at` for a slice that carries it
// on (Slices), and reports whether it was there to stamp.
func touchTrace(tx *sql.Tx, projectID, traceID string, now int64) (bool, error) {
	result, err := tx.Exec(`UPDATE traces SET updated_at = ? WHERE project_id = ? AND id = ?`, now, projectID, traceID)
	if err != nil {
		return false, fmt.Errorf("touch trace %s: %w", traceID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("touch trace %s: %w", traceID, err)
	}
	return n > 0, nil
}

// runExists is the run-existence lookup of spec 014 #3: a seek on the primary
// key of `dataset_runs`, inside the ingest transaction.
func runExists(tx *sql.Tx, projectID, runID string) (bool, error) {
	var one int
	err := tx.QueryRow(`SELECT 1 FROM dataset_runs WHERE project_id = ? AND id = ?`, projectID, runID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("look up run %s: %w", runID, err)
	}
	return true, nil
}

// upsertObservation replaces the row wholesale: a re-delivered span is the
// same span, and the latest delivery is the truth (spec 002 #5).
func upsertObservation(tx *sql.Tx, projectID string, o *model.Observation, indexing bool) error {
	inputID, input, err := writePayload(tx, o.Input)
	if err != nil {
		return err
	}
	outputID, output, err := writePayload(tx, o.Output)
	if err != nil {
		return err
	}
	metadataID, metadata, err := writePayload(tx, o.Metadata)
	if err != nil {
		return err
	}
	modelParameters, err := encodeJSON(o.ModelParameters)
	if err != nil {
		return err
	}
	usage, err := encodeJSON(o.Usage)
	if err != nil {
		return err
	}
	costDetails, err := encodeJSON(o.CostDetails)
	if err != nil {
		return err
	}

	_, err = tx.Exec(
		`INSERT INTO observations (
		   project_id, trace_id, id, parent_observation_id, type, name,
		   start_time, end_time, completion_start_time, model, model_parameters,
		   level, status_message, usage, cost_details, provided_cost,
		   prompt_name, prompt_version, input_id, output_id, metadata_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, trace_id, id) DO UPDATE SET
		   parent_observation_id = excluded.parent_observation_id,
		   type                  = excluded.type,
		   name                  = excluded.name,
		   start_time            = excluded.start_time,
		   end_time              = excluded.end_time,
		   completion_start_time = excluded.completion_start_time,
		   model                 = excluded.model,
		   model_parameters      = excluded.model_parameters,
		   level                 = excluded.level,
		   status_message        = excluded.status_message,
		   usage                 = excluded.usage,
		   cost_details          = excluded.cost_details,
		   provided_cost         = excluded.provided_cost,
		   prompt_name           = excluded.prompt_name,
		   prompt_version        = excluded.prompt_version,
		   input_id              = excluded.input_id,
		   output_id             = excluded.output_id,
		   metadata_id           = excluded.metadata_id`,
		projectID, o.TraceID, o.ID, nullString(o.ParentObservationID), o.Type, nullString(o.Name),
		o.StartTime, o.EndTime, nullInstant(o.CompletionStartTime), nullString(o.Model), modelParameters,
		o.Level, nullString(o.StatusMessage), usage, costDetails, boolToInt(o.ProvidedCost()),
		nullString(o.PromptName), nullNumber(o.PromptVersion), inputID, outputID, metadataID,
	)
	if err != nil {
		return fmt.Errorf("upsert observation %s: %w", o.ID, err)
	}
	if !indexing {
		return nil
	}
	return indexObservation(tx, projectID, o.TraceID, o.ID, observationText{
		Name:          o.Name,
		StatusMessage: o.StatusMessage,
		Input:         string(input),
		Output:        string(output),
		Metadata:      string(metadata),
	})
}

// refreshAggregates recomputes the trace row's denormalized columns from the
// trace's own observations, inside the ingest transaction (spec 002 #7).
//
// Recomputed rather than accumulated as deltas (spec 002 Decision 22,
// 2026-08-26): with upserts (#5) a re-delivered span would double-count every
// delta, and the counts are what the trace list shows. The scan is bounded by
// one trace's observations and served by idx_observations_trace.
//
// `timestamp` prefers the smallest *positive* start time — a span that never
// started carries no information about when the trace did — but falls back to
// the smallest start time of any kind rather than to NULL, because the trace
// list pages on (timestamp, id) and a NULL sort key would hide the row from
// every page after the first (spec 004 Decision 26, 2026-08-27).
//
// `ttft_ms` is the earliest completion start minus the moment the user's wait
// began, which is the trace's own start (spec 012 #3). Both halves come from
// one pass over the same rows: MIN ignores NULLs, so a trace no observation of
// which carried a completion start gets NULL rather than a number derived from
// nothing. The subtrahend is the positive minimum, which is what `timestamp`
// resolves to whenever any span said when it started; a trace where none did
// has no wait to measure and gets NULL here too, where `timestamp` falls back
// to zero so the trace still has a place in a listing ordered by time. A
// duration has no such fallback — measured from the epoch it would be some
// 10^12 ms — and `latency_ms` above is NULL on those same rows (spec 012 #14).
// The minuend is a positive minimum too: a completion start before 1970 is
// the same fact as none, and the smallest int64 minus a positive start
// overflowed into a real that the STRICT column refused, failing the batch
// (spec 043 #5).
//
// `total_cost` sums what the counting rule counts (spec 043 #4), so no string
// and no value near the largest double reaches the sum.
//
// Like every aggregate it is recomputed on each delivery, so a trace whose
// generations arrive in several batches converges on the earliest completion
// start seen so far (spec 002 #22).
func refreshAggregates(tx *sql.Tx, projectID, traceID string) error {
	_, err := tx.Exec(
		`UPDATE traces SET
		   observation_count = (SELECT COUNT(*) FROM observations o
		                        WHERE o.project_id = traces.project_id AND o.trace_id = traces.id),
		   error_count       = (SELECT COUNT(*) FROM observations o
		                        WHERE o.project_id = traces.project_id AND o.trace_id = traces.id
		                          AND o.level = 'ERROR'),
		   total_cost        = (SELECT CAST(SUM(`+costExpr("o.cost_details")+`) AS REAL)
		                        FROM observations o
		                        WHERE o.project_id = traces.project_id AND o.trace_id = traces.id
		                          AND o.provided_cost = 1),
		   timestamp         = COALESCE(
		                        (SELECT MIN(o.start_time) FROM observations o
		                         WHERE o.project_id = traces.project_id AND o.trace_id = traces.id
		                           AND o.start_time > 0),
		                        (SELECT MIN(o.start_time) FROM observations o
		                         WHERE o.project_id = traces.project_id AND o.trace_id = traces.id)),
		   latency_ms        = (SELECT (MAX(o.end_time) - MIN(o.start_time)) / 1000000
		                        FROM observations o
		                        WHERE o.project_id = traces.project_id AND o.trace_id = traces.id
		                          AND o.start_time > 0 AND o.end_time >= o.start_time),
		   ttft_ms           = (SELECT (MIN(CASE WHEN o.completion_start_time > 0 THEN o.completion_start_time END)
		                                - MIN(CASE WHEN o.start_time > 0 THEN o.start_time END)) / 1000000
		                        FROM observations o
		                        WHERE o.project_id = traces.project_id AND o.trace_id = traces.id)
		 WHERE project_id = ? AND id = ?`,
		projectID, traceID,
	)
	if err != nil {
		return fmt.Errorf("refresh trace aggregates %s: %w", traceID, err)
	}
	return nil
}

// writePayload stores a value in `payloads` and returns its id, or NULL for
// an absent value. Payload rows are never updated: an overwriting upsert
// leaves the previous row orphaned for the retention stage to collect.
//
// The encoded bytes come back beside the id because they are also what the
// search index holds (spec 011 #2): re-encoding them for the index would risk
// producing a text the payload does not carry.
func writePayload(tx *sql.Tx, value any) (any, []byte, error) {
	if isBlank(value) {
		return nil, nil, nil
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, nil, fmt.Errorf("encode payload: %w", err)
	}
	compression, body, sizeRaw := compress(raw)
	var id int64
	if err := tx.QueryRow(
		`INSERT INTO payloads (compression, size_raw, body) VALUES (?, ?, ?) RETURNING id`,
		compression, sizeRaw, body,
	).Scan(&id); err != nil {
		return nil, nil, fmt.Errorf("store payload: %w", err)
	}
	return id, raw, nil
}

func encodeJSON(v map[string]any) (any, error) {
	if len(v) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode json column: %w", err)
	}
	return string(raw), nil
}

// isBlank reports a value that carries nothing worth a payload row.
func isBlank(v any) bool {
	switch value := v.(type) {
	case nil:
		return true
	case string:
		return value == ""
	case []any:
		return len(value) == 0
	case map[string]any:
		return len(value) == 0
	}
	return false
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullInstant binds an instant the client did not send as NULL rather than as
// the epoch: a column that says 1970 is a stored lie a reader would act on
// (spec 003 #23), and NULL is what "no completion start" has to be for the
// trace's ttft_ms to come out NULL too.
func nullInstant(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

// nullNumber binds an absent optional integer — a prompt whose version the
// client did not send, or sent unusably (spec 012 #5).
func nullNumber(n *int64) any {
	if n == nil {
		return nil
	}
	return *n
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
