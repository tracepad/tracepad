package store

import (
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// Deleting traces (spec 035): one by id, or every trace a listing filter
// matches. Both walk the path user-data erasure already walks (spec 005 #7),
// because that path was measured and reviewed and every promise
// `docs/retention.md` makes about what outlives what was made for it. One
// body, `traceRemoval`, is what "deleted" means on every door; the jobs
// around it differ only in how they choose the ids.

// TraceDeleteChunk is the most traces one `TraceDelete` job takes: the bound
// erasure already has (spec 023 #19), and the reason is the same — each chunk
// re-rolls the hours it emptied inside its own transaction, and a transaction
// of several dense hours would hold the one writer while ingest queued behind
// it. The caller groups a round's ids by hour and hands them over a chunk at
// a time (#3).
const TraceDeleteChunk = 500

// traceRemoval is the body user-data erasure and trace deletion share (#3):
// the observations, scores and annotation items attached to the traces, the
// traces, the payloads nothing references any more, the search entries — and
// then, in the same transaction, a whole-hour `statsRoll` for every hour the
// chunk emptied that is below the rollup watermark (spec 013 #7, spec 023
// #19), the per-user summaries recomputed once for the users those rolls
// touched. Raw OTLP bodies are not touched: they are stored per batch, and a
// batch holds many traces of many kinds, so a trace cannot be cut out of one.
type traceRemoval struct {
	projectID string
	// ids are the traces to remove, and hours the distinct hours they
	// start in — the caller already read both when it chose them.
	ids   []any
	hours []int64
	// now is the clock the freeze is measured against (spec 013 #11):
	// an hour past the project's retention window is left as it stands.
	now int64
	// skipUser is a user whose summary the rolls must not recompute:
	// erasure deletes that user's rows outright after the rolls (spec
	// 023 #10), and a summary rebuilt here would be a user listed again
	// between a hang-up and the repeat. Empty means nobody is skipped.
	skipUser string
}

func (r *traceRemoval) apply(tx *sql.Tx) (DeleteCounts, error) {
	var counts DeleteCounts
	if len(r.ids) == 0 {
		return counts, nil
	}
	payloads, err := referencedPayloads(tx, r.projectID, r.ids)
	if err != nil {
		return counts, err
	}
	if counts.Observations, err = deleteIn(tx,
		`DELETE FROM observations WHERE project_id = ? AND trace_id IN`,
		[]any{r.projectID}, r.ids); err != nil {
		return counts, fmt.Errorf("delete observations: %w", err)
	}
	if counts.Scores, err = deleteIn(tx,
		`DELETE FROM scores WHERE project_id = ? AND trace_id IN`,
		[]any{r.projectID}, r.ids); err != nil {
		return counts, fmt.Errorf("delete scores: %w", err)
	}
	// The queues keep their shape; what pointed at the removed traces goes
	// with them (spec 024 #3), for the same reason the scores do. Counted,
	// because the response is what an operator shows for "it is gone".
	if counts.AnnotationItems, err = deleteIn(tx,
		`DELETE FROM annotation_items WHERE project_id = ? AND trace_id IN`,
		[]any{r.projectID}, r.ids); err != nil {
		return counts, fmt.Errorf("delete annotation items: %w", err)
	}
	if counts.Traces, err = deleteIn(tx,
		`DELETE FROM traces WHERE project_id = ? AND id IN`,
		[]any{r.projectID}, r.ids); err != nil {
		return counts, fmt.Errorf("delete traces: %w", err)
	}
	if counts.Payloads, err = deleteIn(tx,
		`DELETE FROM payloads WHERE id IN`, nil, payloads); err != nil {
		return counts, fmt.Errorf("delete payloads: %w", err)
	}
	// The traces' media refs, and the bodies nothing points at any more
	// (spec 041 #3). A body a raw batch still names stays: the batch
	// outlives the trace here, as it always has (#3 above). The answer
	// counts what the project stopped holding (spec 041 #27), whether or
	// not another project keeps the bytes.
	drop, err := dropTraceMedia(tx, r.projectID, r.ids)
	if err != nil {
		return counts, err
	}
	counts.Media, counts.MediaBytes = drop.Released, drop.ReleasedBytes
	// Text that is gone must not remain findable (spec 011 #7).
	if err := deleteTraceSearchEntries(tx, r.projectID, r.ids); err != nil {
		return counts, err
	}

	// The statistics are corrected here, in the transaction that made them
	// wrong, one whole `RollHour` per hour this chunk touched. An hour whose
	// raw rows retention already took is frozen and the roll leaves it
	// alone (spec 013 #11) — the aggregates carry no user id, and
	// `docs/retention.md` states that position rather than hiding it.
	//
	// Two gates, both read from this transaction, the ones the score
	// correction of spec 025 #22 applies. An hour at or past the watermark
	// is the live half of the read seam: its raw rows are already right,
	// and rows written for it would be ignored until the pass rewrote
	// them. The summaries are deferred and recomputed once per chunk, the
	// way the aggregator does once per pass.
	now := r.now
	if now == 0 {
		now = time.Now().UnixNano()
	}
	state, err := rollupState(tx, r.projectID)
	if err != nil {
		return counts, err
	}
	touched := map[string]bool{}
	for _, hour := range r.hours {
		if hour >= state.RolledUntil {
			continue
		}
		roll := &statsRoll{ProjectID: r.projectID, Hour: hour, Now: now, DeferSummary: true}
		if err := roll.apply(tx); err != nil {
			return counts, fmt.Errorf("re-roll hour %d after deleting from it: %w", hour, err)
		}
		for _, id := range roll.Touched {
			if id != r.skipUser {
				touched[id] = true
			}
		}
	}
	return counts, recomputeUsers(tx, r.projectID, slices.Sorted(maps.Keys(touched)))
}

// TraceDelete removes a set of traces and everything attached to them (#3).
// It takes exactly the ids it is given — the caller chose them, by id or by
// the listing's own query — and re-reads their hours inside the transaction,
// so the rolls follow what is actually there and not what a preview saw.
//
// The echo is checked here, inside the transaction, as every destructive
// job's is (spec 005 #8): the trace id for the single form, because the id is
// the trace's only identity (#1), and the project's name for the bulk form,
// because the thing being destroyed is a slice of the project (#2).
type TraceDelete struct {
	ProjectID string
	IDs       []string
	Confirm   string
	// ByFilter says which echo is being asked for: the project's name
	// rather than the one trace's id.
	ByFilter bool
	// Now is the clock the freeze is measured against (spec 013 #11); zero
	// is the wall clock.
	Now int64

	Counts DeleteCounts
	// Hours are the hours the deleted traces started in; the ones below
	// the watermark were re-rolled.
	Hours []int64
}

func (d *TraceDelete) apply(tx *sql.Tx) error {
	d.Counts, d.Hours = DeleteCounts{}, nil
	// The bulk form may hand over nothing: a filter that matches nothing is
	// still a confirmed request, and its echo is checked here like every
	// other's (review of PR #74). The single form always names one id.
	if len(d.IDs) == 0 && !d.ByFilter {
		return fmt.Errorf("delete traces: no ids were given")
	}
	// The bound is the caller's to keep (#3); crossing it is a programmer's
	// mistake, so it fails the job rather than being read as a size.
	if len(d.IDs) > TraceDeleteChunk {
		return fmt.Errorf("delete traces: %d ids in one chunk, the most is %d", len(d.IDs), TraceDeleteChunk)
	}
	if d.ByFilter {
		if _, err := confirmProjectName(tx, d.ProjectID, d.Confirm); err != nil {
			return err
		}
	} else {
		if len(d.IDs) != 1 {
			return fmt.Errorf("delete traces: the single form takes one id, got %d", len(d.IDs))
		}
		if d.Confirm != d.IDs[0] {
			return &Rejection{Kind: RejectInvalid, Message: fmt.Sprintf(
				"confirm must be the id of the trace being deleted, %q, to delete it", d.IDs[0])}
		}
		project, err := projectByID(tx, d.ProjectID)
		if err != nil {
			return err
		}
		if project == nil {
			return &Rejection{Kind: RejectNotFound, Message: "no such project"}
		}
	}

	// What is actually still here: a trace another round or another
	// operator already took is not an error, it is zero (spec 035, edge
	// cases). The single form is the exception — one id that is not there
	// is a 404, dry run and confirmed alike.
	wanted := make([]any, 0, len(d.IDs))
	for _, id := range d.IDs {
		wanted = append(wanted, id)
	}
	var ids []any
	seen := map[int64]bool{}
	err := eachIn(wanted, func(batch []any) error {
		args := append([]any{d.ProjectID}, batch...)
		rows, err := tx.Query(`SELECT id, timestamp FROM traces WHERE project_id = ? AND id IN (`+
			placeholders(len(batch))+`)`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id        string
				timestamp sql.NullInt64
			)
			if err := rows.Scan(&id, &timestamp); err != nil {
				return err
			}
			ids = append(ids, id)
			hour := HourOf(timestamp.Int64)
			if !seen[hour] {
				seen[hour] = true
				d.Hours = append(d.Hours, hour)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return fmt.Errorf("select the traces to delete: %w", err)
	}
	if len(ids) == 0 {
		if !d.ByFilter {
			return &Rejection{Kind: RejectNotFound, Message: fmt.Sprintf("trace %q not found", d.IDs[0])}
		}
		return nil
	}
	removal := &traceRemoval{projectID: d.ProjectID, ids: ids, hours: d.Hours, now: d.Now}
	d.Counts, err = removal.apply(tx)
	return err
}

// TracePreview counts what deleting one trace would take (#1): the trace
// itself and what hangs off it, and the run holding it, if one does. A trace
// the project does not hold counts nothing, which is how the handler knows to
// answer 404.
func (s *Store) TracePreview(projectID, id string) (DeleteCounts, []AffectedRun, error) {
	return s.tracesPreview(projectID,
		`SELECT id FROM traces WHERE project_id = ? AND id = ?`, projectID, id)
}

// TraceDeletePreview counts what deleting every trace a listing filter
// matches would take (#2): exactly, not at the listing's cap, because this is
// one deliberate act and "1000+" would leave the operator unable to tell a
// filter they can finish from one they cannot.
func (s *Store) TraceDeletePreview(projectID string, filter TraceFilter) (DeleteCounts, []AffectedRun, error) {
	where, args := traceConditions(projectID, filter)
	return s.tracesPreview(projectID,
		`SELECT id FROM traces WHERE `+strings.Join(where, " AND "), args...)
}

// tracesPreview is the one preview behind erasure and both deletions: given
// a subquery selecting the ids that would go, what hangs off them. Raw bodies
// are not counted because they are not touched — `docs/retention.md` states
// that position rather than hiding it. The runs holding any of the traces
// come back beside the counts: a deletion overrides the pin (spec 014 #14,
// spec 035 #6), and the preview is where that is said.
func (s *Store) tracesPreview(projectID, owned string, args ...any) (DeleteCounts, []AffectedRun, error) {
	var (
		counts DeleteCounts
		oldest sql.NullInt64
	)
	err := s.db.QueryRow(
		`SELECT COUNT(*), MIN(ingested_at) FROM traces WHERE project_id = ? AND id IN (`+owned+`)`,
		append([]any{projectID}, args...)...).Scan(&counts.Traces, &oldest)
	if err != nil {
		return counts, nil, fmt.Errorf("count the traces: %w", err)
	}
	if oldest.Valid {
		counts.Oldest = oldest.Int64
	}
	for _, table := range []struct {
		name  string
		count *int64
	}{
		{"observations", &counts.Observations},
		{"scores", &counts.Scores},
		{"annotation_items", &counts.AnnotationItems},
	} {
		// The table names are this package's own constants; only the
		// project id and the filter's values are bound.
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM `+table.name+` WHERE project_id = ? AND trace_id IN (`+owned+`)`,
			append([]any{projectID}, args...)...).Scan(table.count); err != nil {
			return counts, nil, fmt.Errorf("count the %s: %w", table.name, err)
		}
	}
	// The bodies only these traces point at (spec 041 #11). Raw batches
	// are not touched here, so a body one still names is not counted.
	if counts.Media, counts.MediaBytes, err = s.mediaFreed(projectID, owned, args, "", nil); err != nil {
		return counts, nil, err
	}
	rows, err := s.db.Query(
		`SELECT r.id, r.dataset, COUNT(*) FROM traces t
		   JOIN dataset_runs r ON r.project_id = t.project_id AND r.id = t.run_id
		  WHERE t.project_id = ? AND t.id IN (`+owned+`)
		  GROUP BY r.id, r.dataset ORDER BY r.dataset, r.id`,
		append([]any{projectID}, args...)...)
	if err != nil {
		return counts, nil, fmt.Errorf("find the runs holding them: %w", err)
	}
	defer rows.Close()
	var runs []AffectedRun
	for rows.Next() {
		var run AffectedRun
		if err := rows.Scan(&run.ID, &run.Dataset, &run.Traces); err != nil {
			return counts, nil, err
		}
		runs = append(runs, run)
	}
	return counts, runs, rows.Err()
}
