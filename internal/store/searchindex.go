package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The search index's lifetime (spec 011 #3, #7, #8): written in the ingest
// transaction, deleted by every path that deletes observations, backfilled once
// for a database that predates it, and read back to say where a trace matched.
//
// The index is one table across all projects; `search_entries.project_id` is
// the only scope there is, and a hit in another project is discarded by the
// join before it can be returned.

// searchDB is the half of *sql.Tx and *sql.DB this file uses, so that the
// ingest path (a transaction) and the backfill (its own transactions) share one
// set of statements.
type searchDB interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// observationText is what one observation contributes to the index: five
// fields, each capped, each skipped when it carries nothing.
type observationText struct {
	Name          string
	StatusMessage string
	Input         string
	Output        string
	Metadata      string
}

// fields returns the entries this observation owns, in the order they are
// written — which is also the order `bm25` breaks a tie in.
func (t observationText) fields() []searchEntry {
	return []searchEntry{
		{FieldInput, t.Input},
		{FieldOutput, t.Output},
		{FieldMetadata, t.Metadata},
		{FieldName, t.Name},
		{FieldStatusMessage, t.StatusMessage},
	}
}

// searchIndexEnabled is what the ingest benchmark turns off to measure what the
// index costs the write path (spec 011, Testing #5) — the "without it" half of
// the pair of numbers that decision asks for. It is never false in a running
// server: nothing but the benchmark assigns it, and a store opened with it off
// would answer searches with a lie.
var searchIndexEnabled = true

// indexObservation replaces an observation's entries. Deleted first and then
// re-inserted, because a re-delivered span is the same span and the latest
// delivery is the truth (spec 002 #5): merging would leave the text of a
// previous delivery findable.
func indexObservation(db searchDB, projectID, traceID, observationID string, text observationText) error {
	if !searchIndexEnabled {
		return nil
	}
	if err := dropEntries(db,
		`WHERE project_id = ? AND trace_id = ? AND observation_id = ?`,
		projectID, traceID, observationID); err != nil {
		return err
	}
	return insertEntries(db, projectID, traceID, observationID, text.fields())
}

// indexTraceName replaces the trace's own entry. Rewritten on every delivery
// that touches the trace rather than only when the name changed: knowing it
// changed would cost a read of the row we have just written, and one contentless
// delete plus one insert is cheaper than that read.
func indexTraceName(db searchDB, projectID, traceID, name string) error {
	if !searchIndexEnabled {
		return nil
	}
	if err := dropEntries(db,
		`WHERE project_id = ? AND trace_id = ? AND observation_id IS NULL`,
		projectID, traceID); err != nil {
		return err
	}
	return insertEntries(db, projectID, traceID, nil,
		[]searchEntry{{field: FieldTraceName, body: name}})
}

// searchEntry is one field's text on its way into the index.
type searchEntry struct{ field, body string }

// insertEntries writes an observation's entries in two statements rather than
// two per field: the side rows in one INSERT, then their bodies in one more.
// Ingest pays this on every span it stores, and on this driver a prepared
// statement costs more than the row it writes — twelve statements per span were
// two thirds of what the index cost the write path (Testing #5).
//
// The ids come back through `RETURNING id, field` and are matched up by field.
// Not by the order they arrive in: SQLite leaves the order of RETURNING rows
// undefined, and an observation has at most one entry per field, so the field
// is a key and the position is not.
//
// A field carrying nothing gets no entry at all: an empty row in a full-text
// index is a row that can never match.
func insertEntries(db searchDB, projectID, traceID string, observationID any, entries []searchEntry) error {
	bodies := make(map[string]string, len(entries))
	values := make([]string, 0, len(entries))
	args := make([]any, 0, 4*len(entries))
	for _, entry := range entries {
		body := searchable(entry.body)
		if body == "" {
			continue
		}
		bodies[entry.field] = body
		values = append(values, "(?, ?, ?, ?)")
		args = append(args, projectID, traceID, observationID, entry.field)
	}
	if len(values) == 0 {
		return nil
	}

	rows, err := db.Query(
		`INSERT INTO search_entries (project_id, trace_id, observation_id, field)
		 VALUES `+strings.Join(values, ", ")+` RETURNING id, field`, args...)
	if err != nil {
		return fmt.Errorf("index trace %s: %w", traceID, err)
	}
	ids := make([]any, 0, 2*len(values))
	slots := make([]string, 0, len(values))
	for rows.Next() {
		var (
			id    int64
			field string
		)
		if err := rows.Scan(&id, &field); err != nil {
			rows.Close()
			return fmt.Errorf("index trace %s: %w", traceID, err)
		}
		ids = append(ids, id, bodies[field])
		slots = append(slots, "(?, ?)")
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("index trace %s: %w", traceID, err)
	}

	if _, err := db.Exec(
		`INSERT INTO search_fts (rowid, body) VALUES `+strings.Join(slots, ", "),
		ids...); err != nil {
		return fmt.Errorf("index trace %s: %w", traceID, err)
	}
	return nil
}

// dropEntries removes the entries a predicate names, from both halves. The FTS
// rows go first: `search_entries` is what says which rowids they are.
func dropEntries(db searchDB, where string, args ...any) error {
	if _, err := db.Exec(
		`DELETE FROM search_fts WHERE rowid IN (SELECT id FROM search_entries `+where+`)`,
		args...); err != nil {
		return fmt.Errorf("drop search index rows: %w", err)
	}
	if _, err := db.Exec(`DELETE FROM search_entries `+where, args...); err != nil {
		return fmt.Errorf("drop search entries: %w", err)
	}
	return nil
}

// deleteTraceSearchEntries removes everything the given traces of one project
// had indexed. Called by every path that deletes observations — the retention
// sweep, user-data erasure and the project purge — inside the same transaction
// as the deletion itself (spec 011 #7): text erased under spec 005 must not
// remain findable.
func deleteTraceSearchEntries(tx *sql.Tx, projectID string, traceIDs []any) error {
	return eachIn(traceIDs, func(batch []any) error {
		args := append([]any{projectID}, batch...)
		return dropEntries(tx,
			`WHERE project_id = ? AND trace_id IN (`+placeholders(len(batch))+`)`, args...)
	})
}

// deleteProjectSearchEntries removes a whole project's entries. The purge drops
// the project row and lets the cascades of 0001–0003 take the rest, but this
// table has no foreign key to cascade from (schema 0006), so it is swept by
// hand — after the traces have already been drained chunk by chunk, so what is
// left here is a remainder rather than the bulk.
func deleteProjectSearchEntries(tx *sql.Tx, projectID string) error {
	return dropEntries(tx, `WHERE project_id = ?`, projectID)
}

// orphanSearchEntries lists entries whose observation — or whose trace, for a
// trace-name entry — is gone. The belt to the deletion paths' braces, collected
// by the sweeper's orphan pass (spec 005 #4, spec 011 data contract), bounded
// the same way the payload pass is.
func (s *Store) orphanSearchEntries(limit int) ([]int64, error) {
	rows, err := s.db.Query(
		`SELECT e.id FROM search_entries e
		  WHERE (e.observation_id IS NOT NULL AND NOT EXISTS (
		           SELECT 1 FROM observations o
		            WHERE o.project_id = e.project_id AND o.trace_id = e.trace_id
		              AND o.id = e.observation_id))
		     OR (e.observation_id IS NULL AND NOT EXISTS (
		           SELECT 1 FROM traces t
		            WHERE t.project_id = e.project_id AND t.id = e.trace_id))
		  LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("find orphaned search entries: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// searchEntrySweep removes the orphans that pass found.
type searchEntrySweep struct {
	IDs     []int64
	Deleted int64
}

func (p *searchEntrySweep) apply(tx *sql.Tx) error {
	p.Deleted = 0
	ids := make([]any, 0, len(p.IDs))
	for _, id := range p.IDs {
		ids = append(ids, id)
	}
	return eachIn(ids, func(batch []any) error {
		list := placeholders(len(batch))
		if _, err := tx.Exec(`DELETE FROM search_fts WHERE rowid IN (`+list+`)`, batch...); err != nil {
			return fmt.Errorf("sweep orphaned search index rows: %w", err)
		}
		result, err := tx.Exec(`DELETE FROM search_entries WHERE id IN (`+list+`)`, batch...)
		if err != nil {
			return fmt.Errorf("sweep orphaned search entries: %w", err)
		}
		affected, err := result.RowsAffected()
		p.Deleted += affected
		return err
	})
}

// TraceMatch is where a trace matched: the observation and field with the best
// `bm25` among its hits, and the window of text around the hit. For a trace
// whose name matched, `ObservationID` is empty.
type TraceMatch struct {
	ObservationID string
	Field         string
	Snippet       string
}

// SearchMatch answers "where in this trace" for one row of a listing taken with
// `q`. It is one indexed lookup and at most one payload read, paid only when
// `q` is set (spec 011 #6).
//
// `bm25` picks the entry; the snippet cannot come from FTS5 — a contentless
// table has no text to cut it from — so the field's own text is read back and
// cut here. Only the indexed prefix is searched, because that is the only part
// the index could have matched.
func (s *Store) SearchMatch(projectID, traceID string, query *SearchQuery) (*TraceMatch, error) {
	var (
		observationID sql.NullString
		field         string
	)
	// The tie-break on `e.id` is what makes the answer stable: bm25 over a
	// contentless index scores two one-hit fields identically, and a row
	// that named a different observation on every read would look like a
	// listing that changes when nothing has.
	err := s.db.QueryRow(
		`SELECT e.observation_id, e.field
		   FROM search_fts JOIN search_entries e ON e.id = search_fts.rowid
		  WHERE search_fts MATCH ? AND e.project_id = ? AND e.trace_id = ?
		  ORDER BY bm25(search_fts), e.id LIMIT 1`,
		query.Match, projectID, traceID).Scan(&observationID, &field)
	if errors.Is(err, sql.ErrNoRows) {
		// The index and the listing disagree only if a delete landed
		// between the two reads, and a row without a match is better
		// than a failed page.
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the match in trace %s: %w", traceID, err)
	}
	text, err := s.matchedText(projectID, traceID, observationID.String, field)
	if err != nil {
		return nil, err
	}
	return &TraceMatch{
		ObservationID: observationID.String,
		Field:         field,
		Snippet:       Snippet(searchable(text), query),
	}, nil
}

// matchedText reads back the one field the match named.
func (s *Store) matchedText(projectID, traceID, observationID, field string) (string, error) {
	if field == FieldTraceName {
		var name sql.NullString
		err := s.db.QueryRow(`SELECT name FROM traces WHERE project_id = ? AND id = ?`,
			projectID, traceID).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return name.String, err
	}
	var (
		name       sql.NullString
		status     sql.NullString
		inputID    sql.NullInt64
		outputID   sql.NullInt64
		metadataID sql.NullInt64
	)
	err := s.db.QueryRow(
		`SELECT name, status_message, input_id, output_id, metadata_id FROM observations
		  WHERE project_id = ? AND trace_id = ? AND id = ?`,
		projectID, traceID, observationID).
		Scan(&name, &status, &inputID, &outputID, &metadataID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the matched observation %s: %w", observationID, err)
	}
	switch field {
	case FieldName:
		return name.String, nil
	case FieldStatusMessage:
		return status.String, nil
	case FieldInput:
		return rawPayload(s.db, inputID)
	case FieldOutput:
		return rawPayload(s.db, outputID)
	case FieldMetadata:
		return rawPayload(s.db, metadataID)
	}
	return "", fmt.Errorf("unknown search field %q", field)
}

// rawPayload reads a payload as the text that was indexed: the stored JSON,
// decompressed and not decoded. The index holds what `json.Marshal` produced,
// so re-encoding a decoded value could differ from it by a space or by a key
// order, and the snippet would then be cut from a text that was never searched.
func rawPayload(db searchDB, id sql.NullInt64) (string, error) {
	if !id.Valid {
		return "", nil
	}
	var (
		compression string
		body        []byte
	)
	err := db.QueryRow(`SELECT compression, body FROM payloads WHERE id = ?`, id.Int64).
		Scan(&compression, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read payload %d: %w", id.Int64, err)
	}
	raw, err := Decompress(compression, body)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// --- the one-off backfill (spec 011 #8) -------------------------------------

// backfillBatch is how many traces one backfill transaction indexes. Small
// enough that a crash loses little work, large enough that the per-transaction
// cost disappears next to the decompression.
const backfillBatch = 200

// backfillSearchIndex indexes everything written before schema 0006, before the
// server listens.
//
// Synchronous, like every migration this store has: a search that answers
// "nothing" because the index is half-built is worse than a start that takes a
// minute. It is Go rather than SQL because it decompresses payloads, so it runs
// after migrate() the way ensureIncrementalVacuum does. Idempotent and
// resumable: a trace already carrying entries is skipped, and the marker is
// written only once the walk has run out of traces.
func (s *Store) backfillSearchIndex() error {
	var doneAt sql.NullInt64
	if err := s.db.QueryRow(`SELECT done_at FROM search_backfill WHERE id = 1`).Scan(&doneAt); err != nil {
		return fmt.Errorf("read the search backfill marker: %w", err)
	}
	if doneAt.Valid {
		return nil
	}

	started := time.Now()
	var (
		traces  int64
		lastKey = [2]string{"", ""}
		logged  bool
	)
	for {
		batch, err := s.unindexedTraces(lastKey, backfillBatch)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			break
		}
		if !logged {
			logger().Info("building the search index for the traces already stored; " +
				"the server starts once it is complete")
			logged = true
		}
		if err := s.indexTraces(batch); err != nil {
			return err
		}
		lastKey = batch[len(batch)-1]
		traces += int64(len(batch))
		logger().Info("search backfill progress", "traces", traces,
			"elapsed", time.Since(started).Round(time.Millisecond))
	}

	if _, err := s.db.Exec(`UPDATE search_backfill SET done_at = ? WHERE id = 1`,
		time.Now().UnixNano()); err != nil {
		return fmt.Errorf("record the search backfill: %w", err)
	}
	if traces > 0 {
		logger().Info("search index built", "traces", traces,
			"elapsed", time.Since(started).Round(time.Millisecond))
	}
	return nil
}

// unindexedTraces returns the next traces to index, past the key the walk has
// reached. The cursor is what makes the walk finite — a trace with no name and
// no observations produces no entries, so "has no entries" alone would return
// it forever — and `NOT EXISTS` is what makes a restart cheap.
func (s *Store) unindexedTraces(after [2]string, limit int) ([][2]string, error) {
	rows, err := s.db.Query(
		`SELECT project_id, id FROM traces t
		  WHERE (project_id, id) > (?, ?)
		    AND NOT EXISTS (SELECT 1 FROM search_entries e
		                     WHERE e.project_id = t.project_id AND e.trace_id = t.id)
		  ORDER BY project_id, id LIMIT ?`, after[0], after[1], limit)
	if err != nil {
		return nil, fmt.Errorf("find traces to index: %w", err)
	}
	defer rows.Close()

	var out [][2]string
	for rows.Next() {
		var key [2]string
		if err := rows.Scan(&key[0], &key[1]); err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

// indexTraces indexes one batch, in one transaction.
func (s *Store) indexTraces(batch [][2]string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, key := range batch {
		if err := backfillTrace(tx, key[0], key[1]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func backfillTrace(tx *sql.Tx, projectID, traceID string) error {
	var name sql.NullString
	err := tx.QueryRow(`SELECT name FROM traces WHERE project_id = ? AND id = ?`,
		projectID, traceID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		// Swept between the read that listed it and this transaction.
		return nil
	}
	if err != nil {
		return fmt.Errorf("read trace %s: %w", traceID, err)
	}
	if err := indexTraceName(tx, projectID, traceID, name.String); err != nil {
		return err
	}

	rows, err := tx.Query(
		`SELECT id, name, status_message, input_id, output_id, metadata_id
		   FROM observations WHERE project_id = ? AND trace_id = ?`, projectID, traceID)
	if err != nil {
		return fmt.Errorf("read the observations of %s: %w", traceID, err)
	}
	type stored struct {
		id                          string
		name, status                sql.NullString
		input, output, metadataOnly sql.NullInt64
	}
	var observations []stored
	for rows.Next() {
		var row stored
		if err := rows.Scan(&row.id, &row.name, &row.status,
			&row.input, &row.output, &row.metadataOnly); err != nil {
			rows.Close()
			return err
		}
		observations = append(observations, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, row := range observations {
		text := observationText{Name: row.name.String, StatusMessage: row.status.String}
		for _, payload := range []struct {
			id     sql.NullInt64
			target *string
		}{
			{row.input, &text.Input},
			{row.output, &text.Output},
			{row.metadataOnly, &text.Metadata},
		} {
			body, err := rawPayload(tx, payload.id)
			if err != nil {
				return err
			}
			*payload.target = body
		}
		if err := indexObservation(tx, projectID, traceID, row.id, text); err != nil {
			return err
		}
	}
	return nil
}
