package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/tracepad/tracepad/internal/mapping"
)

// An erasure reaches every copy the store holds (spec 044): the parsed rows as
// before, and the erased traces' spans inside the raw batches — each batch
// that held one rewritten without it, or deleted when nothing else was in it
// (#2). No index leads from a trace to the batches that carried it, so the
// batches are found by the traces' arrival windows (#3), in an order that
// lets a request cut off anywhere be finished by repeating it (#4).

// stampMargin is how far an arrival stamp written under an older rule may sit
// before the one its trace carries (#3): the handler read its clock, then the
// batch waited for the writer. A submission waits behind at most the queue's
// capacity, since a full queue answers 429 instead of growing, and sixty
// seconds is generous for that. The tail of an erasure (#4) opens its window
// by the same margin, for a batch the writer stamped before the request's
// first read and committed after it.
const stampMargin = int64(60 * time.Second)

// scrubAttempts is how often one batch is re-read and recomputed when another
// rewrite landed between the read and the job (#4).
const scrubAttempts = 3

// The migrations whose stamps an arrival window has to correct for (#3).
const (
	// Before 0005 a trace's `ingested_at` was backfilled from the client's
	// timestamp: the window opens at the project's oldest batch.
	migrationIngestedAt = "0005_retention_admin.sql"
	// Before 0009 a trace's `updated_at` was backfilled from `ingested_at`:
	// the window closes when 0009 was applied, not at `updated_at`.
	migrationUpdatedAt = "0009_stats_rollup.sql"
	// Before the erasure migration a batch's `received_at` was the
	// handler's reading, earlier than its traces' stamps: the window opens
	// stampMargin earlier. Matched by its suffix, so the number it takes
	// when it lands is not written here twice.
	migrationErasureSuffix = "_erasure.sql"
)

// erasedTrace is what step 1 reads of a trace: its id and the stamps its
// window is derived from.
type erasedTrace struct {
	id                   string
	ingestedAt, updateAt int64
}

// legacyStamps are the moments the stamps changed meaning, from
// `schema_migrations.applied_at`; zero when the migration is not recorded.
type legacyStamps struct {
	ingestedAt, updatedAt, erasure int64
}

func (s *Store) legacyStamps() (legacyStamps, error) {
	var out legacyStamps
	rows, err := s.db.Query(`SELECT filename, applied_at FROM schema_migrations
		WHERE filename IN (?, ?) OR filename LIKE ? ESCAPE '\'`,
		migrationIngestedAt, migrationUpdatedAt, `%\`+migrationErasureSuffix)
	if err != nil {
		return out, fmt.Errorf("read when the stamps changed: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, applied string
		if err := rows.Scan(&name, &applied); err != nil {
			return out, err
		}
		at, err := time.Parse(time.RFC3339Nano, applied)
		if err != nil {
			return out, fmt.Errorf("read when %s was applied: %w", name, err)
		}
		switch name {
		case migrationIngestedAt:
			out.ingestedAt = at.UnixNano()
		case migrationUpdatedAt:
			out.updatedAt = at.UnixNano()
		default:
			out.erasure = at.UnixNano()
		}
	}
	return out, rows.Err()
}

// arrivalWindow is one trace's arrival window, both ends inclusive.
type arrivalWindow struct{ from, to int64 }

// windowOf is #3 for one trace: its spans arrived in batches whose
// `received_at` lies within `[ingested_at, updated_at]`, with the side each
// legacy stamp cannot vouch for opened.
func (l legacyStamps) windowOf(t erasedTrace) arrivalWindow {
	w := arrivalWindow{from: t.ingestedAt, to: max(t.updateAt, t.ingestedAt)}
	if t.ingestedAt < l.erasure {
		w.from -= stampMargin
	}
	if t.ingestedAt < l.updatedAt {
		w.to = max(w.to, l.updatedAt)
	}
	if t.ingestedAt < l.ingestedAt {
		w.from = math.MinInt64
	}
	return w
}

// mergeWindows answers the union of the windows as disjoint windows in order.
func mergeWindows(windows []arrivalWindow) []arrivalWindow {
	slices.SortFunc(windows, func(a, b arrivalWindow) int {
		switch {
		case a.from < b.from:
			return -1
		case a.from > b.from:
			return 1
		}
		return 0
	})
	var out []arrivalWindow
	for _, w := range windows {
		if n := len(out); n > 0 && w.from <= out[n-1].to {
			out[n-1].to = max(out[n-1].to, w.to)
			continue
		}
		out = append(out, w)
	}
	return out
}

// userTraces is step 1: the user's traces with their stamps, through
// `idx_traces_user`.
func (s *Store) userTraces(projectID, userID string) ([]erasedTrace, error) {
	rows, err := s.db.Query(
		`SELECT id, ingested_at, updated_at FROM traces WHERE project_id = ? AND user_id = ?`,
		projectID, userID)
	if err != nil {
		return nil, fmt.Errorf("read the user's traces: %w", err)
	}
	defer rows.Close()
	var out []erasedTrace
	for rows.Next() {
		var (
			t       erasedTrace
			updated sql.NullInt64
		)
		if err := rows.Scan(&t.id, &t.ingestedAt, &updated); err != nil {
			return nil, err
		}
		t.updateAt = updated.Int64
		out = append(out, t)
	}
	return out, rows.Err()
}

// userWindows is the union of the arrival windows of a user's traces.
func (s *Store) userWindows(traces []erasedTrace) ([]arrivalWindow, error) {
	if len(traces) == 0 {
		return nil, nil
	}
	legacy, err := s.legacyStamps()
	if err != nil {
		return nil, err
	}
	windows := make([]arrivalWindow, 0, len(traces))
	for _, t := range traces {
		windows = append(windows, legacy.windowOf(t))
	}
	return mergeWindows(windows), nil
}

// candidateBatches lists the project's batches received inside the windows,
// oldest first, through `idx_raw_batches_received`.
func (s *Store) candidateBatches(projectID string, windows []arrivalWindow) ([]int64, error) {
	var ids []int64
	for _, w := range windows {
		rows, err := s.db.Query(
			`SELECT id FROM raw_batches WHERE project_id = ? AND received_at >= ? AND received_at <= ?
			  ORDER BY received_at, id`, projectID, w.from, w.to)
		if err != nil {
			return nil, fmt.Errorf("find the batches to scan: %w", err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// countBatches is candidateBatches as a count, for the dry run: a count and
// not a decode (spec 044, API contract).
func (s *Store) countBatches(projectID string, windows []arrivalWindow) (int64, error) {
	var total int64
	for _, w := range windows {
		var n int64
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM raw_batches WHERE project_id = ? AND received_at >= ? AND received_at <= ?`,
			projectID, w.from, w.to).Scan(&n); err != nil {
			return 0, fmt.Errorf("count the batches to scan: %w", err)
		}
		total += n
	}
	return total, nil
}

// unattributableBatches is #5 (a): the batches older than the trace window,
// which hold spans of traces the sweep already took — nothing names their
// user any more. Zero for a project that keeps its traces for ever.
func (s *Store) unattributableBatches(projectID string, now int64) (int64, error) {
	var retention sql.NullInt64
	if err := s.db.QueryRow(`SELECT retention_days FROM projects WHERE id = ?`, projectID).
		Scan(&retention); err != nil {
		return 0, fmt.Errorf("read the trace window: %w", err)
	}
	cutoff, windowed := windowCutoff(retention, now)
	if !windowed {
		return 0, nil
	}
	var n int64
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM raw_batches WHERE project_id = ? AND received_at < ?`,
		projectID, cutoff).Scan(&n); err != nil {
		return 0, fmt.Errorf("count the batches older than the trace window: %w", err)
	}
	return n, nil
}

// RawScrub rewrites one raw batch without the erased spans, or deletes it
// (spec 044 #2, #4). The body was computed on the read side, so the job is a
// byte swap: it refuses — a conflict — when the batch's `scrubbed_at` is no
// longer the one the body was computed from, because another rewrite landed
// in between and writing this body would bring back what that one removed. A
// batch that is gone (swept since it was read) is left gone.
type RawScrub struct {
	ProjectID string
	BatchID   int64
	// Expect is the `scrubbed_at` the body was computed from; nil is a
	// batch as received.
	Expect *int64
	// Body is the new body, zstd-compressed; Media the bodies it references,
	// which become the batch's refs.
	Body  []byte
	Media []string
	// Delete drops the batch instead: nothing is left in it, or its rewrite
	// failed.
	Delete bool
	// Now is the stamp's clock; zero is the wall clock.
	Now int64

	// Gone reports a batch that was no longer there.
	Gone bool
	// Released and ReleasedBytes are the media bodies the project stopped
	// holding with the refs the batch dropped.
	Released, ReleasedBytes int64
	// CompactionRequested is the compaction this rewrite asked for: the old
	// body's pages hold the erased spans until they are overwritten (#11).
	CompactionRequested int64
}

func (j *RawScrub) apply(tx *sql.Tx) error {
	j.Gone, j.Released, j.ReleasedBytes, j.CompactionRequested = false, 0, 0, 0
	var scrubbed sql.NullInt64
	err := tx.QueryRow(`SELECT scrubbed_at FROM raw_batches WHERE project_id = ? AND id = ?`,
		j.ProjectID, j.BatchID).Scan(&scrubbed)
	if err == sql.ErrNoRows {
		j.Gone = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("read raw batch %d: %w", j.BatchID, err)
	}
	if scrubbed.Valid != (j.Expect != nil) || (j.Expect != nil && *j.Expect != scrubbed.Int64) {
		return &Rejection{Kind: RejectConflict, Message: fmt.Sprintf(
			"raw batch %d was rewritten since it was read", j.BatchID)}
	}
	now := nowOr(j.Now)
	var drop mediaDrop
	if j.Delete {
		if drop, err = dropRawMedia(tx, j.ProjectID, []any{j.BatchID}); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM raw_batches WHERE id = ?`, j.BatchID); err != nil {
			return fmt.Errorf("delete raw batch %d: %w", j.BatchID, err)
		}
	} else {
		// Later than the stamp read, whatever the clock says, so that a
		// body computed from this state is refused once this one lands.
		stamp := now
		if scrubbed.Valid && stamp <= scrubbed.Int64 {
			stamp = scrubbed.Int64 + 1
		}
		if _, err := tx.Exec(`UPDATE raw_batches SET body = ?, scrubbed_at = ? WHERE id = ?`,
			j.Body, stamp, j.BatchID); err != nil {
			return fmt.Errorf("rewrite raw batch %d: %w", j.BatchID, err)
		}
		if drop, err = j.replaceRefs(tx); err != nil {
			return err
		}
	}
	j.Released, j.ReleasedBytes = drop.Released, drop.ReleasedBytes
	j.CompactionRequested, err = requestCompaction(tx, now)
	return err
}

// replaceRefs makes the batch's media refs the ones its new body carries:
// the refs of the bodies only the removed spans pointed at go, and a body no
// ref names any more is collected in the same transaction (spec 041
// Decision 12).
func (j *RawScrub) replaceRefs(tx *sql.Tx) (mediaDrop, error) {
	old, err := queryColumn[string](tx,
		`SELECT sha256 FROM media_raw_refs WHERE raw_batch_id = ?`, j.BatchID)
	if err != nil {
		return mediaDrop{}, fmt.Errorf("read raw batch %d's media: %w", j.BatchID, err)
	}
	keep := make(map[string]bool, len(j.Media))
	for _, sha := range j.Media {
		keep[sha] = true
	}
	var gone []any
	for _, sha := range old {
		if !keep[sha.(string)] {
			gone = append(gone, sha)
		}
	}
	if len(gone) == 0 {
		return mediaDrop{}, nil
	}
	if _, err := deleteIn(tx, `DELETE FROM media_raw_refs WHERE raw_batch_id = ? AND sha256 IN`,
		[]any{j.BatchID}, gone); err != nil {
		return mediaDrop{}, fmt.Errorf("delete raw batch %d's media refs: %w", j.BatchID, err)
	}
	return releaseAndCollect(tx, j.ProjectID, gone)
}

// scrubRewrite is the rewrite a scrub applies, a variable so that a test can
// make it fail and see the batch deleted instead (#2).
var scrubRewrite = func(body *mapping.ExportBody, traces map[string]bool) ([]byte, int, error) {
	return body.Without(traces)
}

// scrubPlan is what the read side decided for one batch.
type scrubPlan struct {
	job *RawScrub
	// spans is how many erased spans the batch held.
	spans int
	// fallback reports a batch deleted because its rewrite failed.
	fallback bool
}

// planScrub reads one batch and computes its scrub: nil when the batch is
// gone or holds none of the traces, which leaves it untouched and unmarked.
func (s *Store) planScrub(projectID string, id int64, traces map[string]bool, now int64) (*scrubPlan, error) {
	var (
		stored      []byte
		contentType sql.NullString
		scrubbed    sql.NullInt64
	)
	err := s.db.QueryRow(
		`SELECT body, content_type, scrubbed_at FROM raw_batches WHERE project_id = ? AND id = ?`,
		projectID, id).Scan(&stored, &contentType, &scrubbed)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read raw batch %d: %w", id, err)
	}
	job := &RawScrub{ProjectID: projectID, BatchID: id, Now: now}
	if scrubbed.Valid {
		expect := scrubbed.Int64
		job.Expect = &expect
	}
	fail := func(spans int, reason error) (*scrubPlan, error) {
		// The spans must not stay for want of an encoder: the batch goes
		// whole, and the answer counts it (#2). The log line names the
		// batch and never the user (#15).
		logger().Warn("a raw batch could not be rewritten without the erased spans; deleting it whole",
			"project", projectID, "batch", id, "err", reason)
		job.Delete = true
		return &scrubPlan{job: job, spans: spans, fallback: true}, nil
	}
	body, err := Decompress(CompressionZstd, stored)
	if err != nil {
		return fail(0, err)
	}
	asJSON := rawContentType(contentType) == mapping.ContentTypeJSON
	decoded, err := mapping.DecodeExportBody(body, asJSON)
	if err != nil {
		return fail(0, err)
	}
	if !mapping.HoldsTrace(decoded.ResourceSpans, traces) {
		return nil, nil
	}
	held := mapping.SpanCount(decoded.ResourceSpans)
	rewritten, removed, err := scrubRewrite(decoded, traces)
	if err != nil {
		return fail(held-mapping.SpanCount(decoded.ResourceSpans), err)
	}
	// The new body is read back before it is trusted: it must decode, and
	// it must hold none of the traces.
	again, err := mapping.DecodeExportBody(rewritten, asJSON)
	if err != nil {
		return fail(removed, err)
	}
	if mapping.HoldsTrace(again.ResourceSpans, traces) {
		return fail(removed, errors.New("the rewritten body still holds an erased span"))
	}
	plan := &scrubPlan{job: job, spans: removed}
	if mapping.SpanCount(again.ResourceSpans) == 0 && again.Unreadable == 0 {
		// Nothing is left in it: a batch with no span is deleted.
		job.Delete = true
		return plan, nil
	}
	job.Body = zstdEncoder.EncodeAll(rewritten, nil)
	job.Media = mapping.MediaReferences(again.ResourceSpans)
	return plan, nil
}

// scrubTally is what a scrub phase did.
type scrubTally struct {
	spans, rewritten, deleted int64
	released, releasedBytes   int64
	compaction                int64
}

func (t *scrubTally) add(o scrubTally) {
	t.spans += o.spans
	t.rewritten += o.rewritten
	t.deleted += o.deleted
	t.released += o.released
	t.releasedBytes += o.releasedBytes
	t.compaction = max(t.compaction, o.compaction)
}

// A scrub submits the jobs of a group of batches at once, so that they share
// the writer's commit windows instead of each waiting out one of its own: one
// at a time, a user whose spans sit in two hundred batches paid ten seconds
// of windows for a few milliseconds of writing. The group is bounded by count
// and by the bodies it holds in memory.
const (
	scrubGroup      = 32
	scrubGroupBytes = 16 << 20
)

// scrubBatches runs the scrub over a list of candidate batches: each is read
// and decoded here, one writer job is submitted per batch that changes, and a
// job refused because another rewrite landed first is re-read and recomputed
// (#4).
func (s *Store) scrubBatches(ctx context.Context, writer jobSubmitter, projectID string, ids []int64,
	traces map[string]bool, now int64) (scrubTally, error) {
	var tally scrubTally
	if len(traces) == 0 {
		return tally, nil
	}
	for len(ids) > 0 {
		if err := ctx.Err(); err != nil {
			return tally, err
		}
		var (
			group []int64
			plans []*scrubPlan
			held  int
		)
		for len(ids) > 0 && len(plans) < scrubGroup && held < scrubGroupBytes {
			id := ids[0]
			ids = ids[1:]
			plan, err := s.planScrub(projectID, id, traces, now)
			if err != nil {
				return tally, err
			}
			if plan != nil {
				group, plans = append(group, id), append(plans, plan)
				held += len(plan.job.Body)
			}
		}
		errs := make([]error, len(plans))
		var wg sync.WaitGroup
		for i, plan := range plans {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[i] = writer.Submit(ctx, plan.job)
			}()
		}
		wg.Wait()
		for i, plan := range plans {
			err := errs[i]
			for attempt := 1; conflict(err) && attempt < scrubAttempts; attempt++ {
				if plan, err = s.planScrub(projectID, group[i], traces, now); err != nil {
					return tally, err
				}
				if plan == nil {
					break
				}
				err = writer.Submit(ctx, plan.job)
			}
			if err != nil {
				return tally, err
			}
			tally.record(plan)
		}
	}
	return tally, nil
}

// conflict reports a scrub refused because the batch changed since it was read.
func conflict(err error) bool {
	var rejection *Rejection
	return errors.As(err, &rejection) && rejection.Kind == RejectConflict
}

// record counts what one scrub job did; nothing for a batch that was gone or
// held nothing any more.
func (t *scrubTally) record(plan *scrubPlan) {
	if plan == nil || plan.job.Gone {
		return
	}
	t.spans += int64(plan.spans)
	if plan.job.Delete {
		t.deleted++
	} else {
		t.rewritten++
	}
	t.released += plan.job.Released
	t.releasedBytes += plan.job.ReleasedBytes
	t.compaction = max(t.compaction, plan.job.CompactionRequested)
}

// UserErasure is one erasure request (spec 005 #7, spec 044 #1, #4).
type UserErasure struct {
	ProjectID string
	UserID    string
	Confirm   string
	// Chunk and ChunkHours bound one transaction of the parsed phase
	// (spec 023 #19); see UserDataErase.
	Chunk      int
	ChunkHours int
	// Now is the clock the freeze is measured against (spec 013 #11) and
	// the scrub's stamp; zero is the wall clock.
	Now int64

	// after is a test seam, told when each step has finished; an error
	// ends the request there, the way a client hanging up does.
	after func(step int) error
}

// ErasureResult is what an erasure did.
type ErasureResult struct {
	Counts DeleteCounts
	// Compaction is the latest compaction the request asked for, zero when
	// it deleted nothing (spec 044 #17).
	Compaction int64
}

// EraseUserData runs an erasure in the order of #4, and runs it to completion
// (spec 035 #14):
//
//  1. read the user's traces — ids and stamps;
//  2. scrub the batches of their arrival windows;
//  3. delete the parsed rows chunk by chunk, as before, with the session
//     scores (#7) and the dataset items (#9);
//  4. scrub the batches received since step 1 for every trace step 3
//     deleted.
//
// Raw goes first because once the parsed rows are gone nothing names the
// batches: a request cut off in step 2 or 3 is finished by repeating it, which
// finds the remaining traces and derives their windows again. The raw phase
// runs on what the archive holds, whatever `TRACEPAD_STORE_RAW` says now: a
// store that stopped archiving still holds the batches it kept before.
func (s *Store) EraseUserData(ctx context.Context, writer jobSubmitter, e UserErasure) (ErasureResult, error) {
	var result ErasureResult
	// The echo first (spec 005 #8): the raw phase destroys too, and must not
	// run for a request its last step would refuse.
	if e.Confirm != e.UserID {
		return result, &Rejection{Kind: RejectInvalid, Message: fmt.Sprintf(
			"confirm must be the user id being erased, %q, to erase their data", e.UserID)}
	}
	now := nowOr(e.Now)
	after := func(step int) error {
		if e.after == nil {
			return nil
		}
		return e.after(step)
	}

	// 1. The traces and their windows; the moment is kept for step 4.
	since := time.Now().UnixNano()
	traces, err := s.userTraces(e.ProjectID, e.UserID)
	if err != nil {
		return result, err
	}
	if err := after(1); err != nil {
		return result, err
	}

	// 2. Their batches.
	windows, err := s.userWindows(traces)
	if err != nil {
		return result, err
	}
	candidates, err := s.candidateBatches(e.ProjectID, windows)
	if err != nil {
		return result, err
	}
	erased := make(map[string]bool, len(traces))
	for _, t := range traces {
		erased[t.id] = true
	}
	raw, err := s.scrubBatches(ctx, writer, e.ProjectID, candidates, erased, now)
	if err != nil {
		return result, err
	}
	if err := after(2); err != nil {
		return result, err
	}

	// 3. The parsed rows.
	deleted := map[string]bool{}
	for {
		chunk := &UserDataErase{ProjectID: e.ProjectID, UserID: e.UserID, Confirm: e.Confirm,
			Limit: e.Chunk, HourLimit: e.ChunkHours, Now: e.Now}
		if err := writer.Submit(ctx, chunk); err != nil {
			return result, err
		}
		result.Counts.add(chunk.Counts)
		result.Compaction = max(result.Compaction, chunk.CompactionRequested)
		for _, id := range chunk.IDs {
			deleted[id] = true
		}
		if !chunk.More {
			break
		}
	}
	if err := after(3); err != nil {
		return result, err
	}

	// 4. The tail: what arrived while the request ran.
	recent, err := s.candidateBatches(e.ProjectID, []arrivalWindow{{from: since - stampMargin, to: math.MaxInt64}})
	if err != nil {
		return result, err
	}
	tail, err := s.scrubBatches(ctx, writer, e.ProjectID, recent, deleted, now)
	if err != nil {
		return result, err
	}
	raw.add(tail)

	result.Counts.RawSpans = raw.spans
	result.Counts.RawBatchesRewritten = raw.rewritten
	result.Counts.RawBatchesDeleted = raw.deleted
	result.Counts.Media += raw.released
	result.Counts.MediaBytes += raw.releasedBytes
	result.Compaction = max(result.Compaction, raw.compaction)
	// Counts, never the id (#15).
	if raw.spans > 0 {
		logger().Info("erasure removed spans from the raw archive", "project", e.ProjectID,
			"spans", raw.spans, "batches_rewritten", raw.rewritten, "batches_deleted", raw.deleted)
	}
	return result, nil
}

// AffectedDataset is a dataset an erasure takes items from (#9), the way
// AffectedRun is a run it takes traces from.
type AffectedDataset struct {
	Dataset string
	Items   int64
}

// RawErasure is the dry run's raw block: the candidates of #3, a count and not
// a decode, and the batches #5 (a) cannot attribute.
type RawErasure struct {
	BatchesToScan         int64
	UnattributableBatches int64
}

// UserPreview is what erasing one user would take.
type UserPreview struct {
	Counts   DeleteCounts
	Runs     []AffectedRun
	Datasets []AffectedDataset
	Raw      RawErasure
}

// UserDataPreview counts what erasing one user would take: exactly what the
// confirmed request deletes, and the raw batches it would read.
func (s *Store) UserDataPreview(ctx context.Context, projectID, userID string, now int64) (UserPreview, error) {
	var out UserPreview
	const owned = `SELECT id FROM traces WHERE project_id = ? AND user_id = ?`
	var err error
	if out.Counts, out.Runs, err = s.tracesPreview(ctx, projectID, owned, projectID, userID); err != nil {
		return out, err
	}
	// The session-only scores of the sessions the traces carried (#7).
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM scores WHERE project_id = ? AND trace_id IS NULL AND session_id IN
		   (SELECT session_id FROM traces WHERE project_id = ? AND user_id = ? AND session_id IS NOT NULL)`,
		projectID, projectID, userID).Scan(&out.Counts.SessionScores); err != nil {
		return out, fmt.Errorf("count the session scores: %w", err)
	}
	// The items any row of which names one of the traces (#9).
	rows, err := s.db.Query(
		`SELECT dataset, COUNT(DISTINCT item_id) FROM dataset_items
		  WHERE project_id = ? AND source_trace_id IN (`+owned+`)
		  GROUP BY dataset ORDER BY dataset`, projectID, projectID, userID)
	if err != nil {
		return out, fmt.Errorf("find the dataset items cut from the traces: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var d AffectedDataset
		if err := rows.Scan(&d.Dataset, &d.Items); err != nil {
			return out, err
		}
		out.Datasets = append(out.Datasets, d)
		out.Counts.DatasetItems += d.Items
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	traces, err := s.userTraces(projectID, userID)
	if err != nil {
		return out, err
	}
	windows, err := s.userWindows(traces)
	if err != nil {
		return out, err
	}
	if out.Raw.BatchesToScan, err = s.countBatches(projectID, windows); err != nil {
		return out, err
	}
	out.Raw.UnattributableBatches, err = s.unattributableBatches(projectID, nowOr(now))
	return out, err
}

// deleteSessionScores deletes the session-only scores of the sessions a chunk
// of traces carried (#7), through `idx_scores_session`. A session shared with
// another user's traces loses them too: the verdict is about a conversation
// the erased person was part of.
func deleteSessionScores(tx *sql.Tx, projectID string, traceIDs []any) (int64, error) {
	var sessions []any
	err := eachIn(traceIDs, func(batch []any) error {
		found, err := queryColumn[string](tx,
			`SELECT DISTINCT session_id FROM traces WHERE project_id = ? AND session_id IS NOT NULL AND id IN (`+
				placeholders(len(batch))+`)`, append([]any{projectID}, batch...)...)
		sessions = append(sessions, found...)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("find the traces' sessions: %w", err)
	}
	n, err := deleteIn(tx, `DELETE FROM scores WHERE project_id = ? AND trace_id IS NULL AND session_id IN`,
		[]any{projectID}, sessions)
	if err != nil {
		return 0, fmt.Errorf("delete the session scores: %w", err)
	}
	return n, nil
}

// deleteSourcedItems deletes every row of every dataset item any row of which
// names one of the traces as its source (#9), and ticks each affected
// dataset's version once. It answers how many items went.
func deleteSourcedItems(tx *sql.Tx, projectID string, traceIDs []any, now int64) (int64, error) {
	type item struct{ dataset, id string }
	var items []item
	err := eachIn(traceIDs, func(batch []any) error {
		rows, err := tx.Query(
			`SELECT DISTINCT dataset, item_id FROM dataset_items
			  WHERE project_id = ? AND source_trace_id IN (`+placeholders(len(batch))+`)`,
			append([]any{projectID}, batch...)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it item
			if err := rows.Scan(&it.dataset, &it.id); err != nil {
				return err
			}
			items = append(items, it)
		}
		return rows.Err()
	})
	if err != nil {
		return 0, fmt.Errorf("find the dataset items cut from the traces: %w", err)
	}
	seen := map[item]bool{}
	var datasets []string
	for _, it := range items {
		if seen[it] {
			continue
		}
		seen[it] = true
		if _, err := tx.Exec(`DELETE FROM dataset_items WHERE project_id = ? AND dataset = ? AND item_id = ?`,
			projectID, it.dataset, it.id); err != nil {
			return 0, fmt.Errorf("delete dataset item %s: %w", it.id, err)
		}
		if !slices.Contains(datasets, it.dataset) {
			datasets = append(datasets, it.dataset)
		}
	}
	for _, dataset := range datasets {
		// One tick per chunk: a harness that caches by version sees that
		// the set changed (#9).
		if _, err := tx.Exec(
			`UPDATE datasets SET version = version + 1, updated_at = ? WHERE project_id = ? AND name = ?`,
			nowOr(now), projectID, dataset); err != nil {
			return 0, fmt.Errorf("advance dataset %s's version: %w", dataset, err)
		}
	}
	return int64(len(seen)), nil
}
