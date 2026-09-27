package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

// The retention sweeper (spec 005 #3). One goroutine on a tick, deleting per
// project in chunks, each chunk a WriteJob through the group-commit writer.
//
// Through the writer and not beside it: a second write connection would
// reintroduce the SQLITE_BUSY_SNAPSHOT class spec 003 Decision 24 killed, and
// as a job the sweeper serializes with ingest for milliseconds at a time and
// inherits the fsync semantics for free. Retention is therefore not a
// background thread racing the foreground — it is a writer of the same kind as
// an export, just one that removes rows.

// Sweeper defaults.
const (
	// DefaultSweepInterval is the cadence of a pass. Retention changes take
	// effect on the next one, which is also the safety margin after a
	// mistaken change (spec 005 #14).
	DefaultSweepInterval = time.Hour
	// DefaultSweepChunk is how many traces one transaction removes. Small
	// enough to stay invisible to a concurrent export, large enough that a
	// day of the reference workload is a handful of them.
	DefaultSweepChunk = 1000
	// maxChunksPerProject bounds one pass so that a project with a huge
	// backlog cannot monopolise the writer; the rest goes next pass.
	maxChunksPerProject = 100
	// orphanScanLimit bounds how many orphaned payload rows one pass
	// collects (#4).
	orphanScanLimit = 5000
	// vacuumPages is how much free space one pass hands back to the
	// filesystem. Bounded for the same reason the chunks are.
	vacuumPages = 2000
)

// SweepOptions tunes the sweeper. Zero fields take defaults.
type SweepOptions struct {
	Interval time.Duration
	Chunk    int
	// Now is the clock the windows are measured against; time.Now when
	// unset. Tests move it instead of waiting a day.
	Now func() time.Time
}

// SweepStatus is what `GET /api/v1/system` publishes about the sweeper
// (spec 005 #14). The counters are since this process started, like every
// other counter the endpoint reports, and the deletion counts are one
// project's own (spec 004 Decision 33).
type SweepStatus struct {
	Interval          time.Duration
	LastRun           int64 // Unix nanoseconds; zero means "has not run yet"
	NextRun           int64
	Since             int64
	TracesDeleted     int64
	RawBatchesDeleted int64
}

// jobSubmitter is the writer as the sweeper needs it.
type jobSubmitter interface {
	Submit(ctx context.Context, job WriteJob) error
}

// Sweeper deletes what the retention windows say is expired.
type Sweeper struct {
	store     *Store
	writer    jobSubmitter
	interval  time.Duration
	chunk     int
	maxChunks int
	now       func() time.Time

	stop   context.CancelFunc
	done   chan struct{}
	closed sync.Once

	mu      sync.Mutex
	started time.Time
	lastRun time.Time
	nextRun time.Time
	deleted map[string]*sweepCounters

	// mediaCursor is where the next pass's look for bodies no ref names
	// starts (spec 041): one page per pass, not the whole table.
	mediaCursor string

	// running and runStart say a pass is under way, so that a deletion
	// committed now is told the pass after it (spec 044 #11).
	running  bool
	runStart time.Time

	// afterMergeStep is a test seam: it runs after every merge step of a
	// compaction, which is how a test lands ingest in the middle of one.
	// Nil in production.
	afterMergeStep func()
}

type sweepCounters struct {
	traces int64
	raw    int64
}

// NewSweeper builds the sweeper. It does not start it: whoever owns the
// writer's lifetime owns this one too, and the tests drive passes by hand.
func (s *Store) NewSweeper(writer jobSubmitter, opts SweepOptions) *Sweeper {
	if opts.Interval <= 0 {
		opts.Interval = DefaultSweepInterval
	}
	if opts.Chunk <= 0 {
		opts.Chunk = DefaultSweepChunk
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	now := opts.Now()
	return &Sweeper{
		store:     s,
		writer:    writer,
		interval:  opts.Interval,
		chunk:     opts.Chunk,
		maxChunks: maxChunksPerProject,
		now:       opts.Now,
		started:   now,
		nextRun:   now.Add(opts.Interval),
		deleted:   map[string]*sweepCounters{},
	}
}

// Start runs passes on the tick until Close. The first pass waits out one
// interval: a process that has just started has better things to do, and a
// restart loop must not turn into a delete loop.
func (sw *Sweeper) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	sw.stop = cancel
	sw.done = make(chan struct{})
	go func() {
		defer close(sw.done)
		ticker := time.NewTicker(sw.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := sw.Pass(ctx); err != nil && !errors.Is(err, context.Canceled) {
					logger().Error("retention sweep failed", "err", err)
				}
			}
		}
	}()
}

// Close stops the sweeper and waits for a pass in flight.
func (sw *Sweeper) Close() error {
	sw.closed.Do(func() {
		if sw.stop != nil {
			sw.stop()
			<-sw.done
		}
	})
	return nil
}

// ExpectedBy is when a compaction requested now will have run: the next pass,
// or — while a pass is under way, which may have read the request state
// already — the one after it. Never earlier than now: a tick that is late is
// still due.
func (sw *Sweeper) ExpectedBy() int64 {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	due := sw.nextRun
	if sw.running {
		due = sw.runStart.Add(sw.interval)
	}
	if now := sw.now(); due.Before(now) {
		due = now
	}
	return due.UnixNano()
}

// Status reports the sweeper's state for one project.
func (sw *Sweeper) Status(projectID string) SweepStatus {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	status := SweepStatus{
		Interval: sw.interval,
		NextRun:  sw.nextRun.UnixNano(),
		Since:    sw.started.UnixNano(),
	}
	if !sw.lastRun.IsZero() {
		status.LastRun = sw.lastRun.UnixNano()
	}
	if counters := sw.deleted[projectID]; counters != nil {
		status.TracesDeleted = counters.traces
		status.RawBatchesDeleted = counters.raw
	}
	return status
}

// Pass runs one full sweep: every project's expired traces and raw batches,
// the projects whose grace window has run out, the orphaned payloads, and
// finally the incremental vacuum that hands the freed pages back.
//
// A failure on one project is logged and the pass continues: one project's
// broken state must not stop everyone else's retention.
func (sw *Sweeper) Pass(ctx context.Context) error {
	start := sw.now()
	sw.mu.Lock()
	sw.running, sw.runStart = true, start
	sw.mu.Unlock()
	// Whatever way the pass ends — finished, failed, cancelled — the ticker
	// fires again an interval after it began, and that is the pass a
	// compaction requested now is expected by (spec 044 #19).
	defer func() {
		sw.mu.Lock()
		sw.running = false
		sw.nextRun = start.Add(sw.interval)
		sw.mu.Unlock()
	}()
	projects, err := sw.store.ListProjects(ctx, true)
	if err != nil {
		return fmt.Errorf("sweep: list projects: %w", err)
	}

	var freed bool
	var failures []error
	for _, project := range projects {
		removed, err := sw.sweepProject(ctx, project, start)
		if removed {
			freed = true
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, ErrWriterClosed) {
				return err
			}
			failures = append(failures, fmt.Errorf("project %s: %w", project.Name, err))
		}
	}

	orphans, err := sw.sweepOrphanPayloads(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrWriterClosed) {
			return err
		}
		failures = append(failures, fmt.Errorf("orphaned payloads: %w", err))
	}
	if orphans > 0 {
		freed = true
	}

	media, err := sw.sweepOrphanMedia(ctx, start.UnixNano())
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrWriterClosed) {
			return err
		}
		failures = append(failures, fmt.Errorf("orphaned media: %w", err))
	}
	if media > 0 {
		freed = true
	}

	// Removed traces whose uploads no URL can still carry (spec 041 #29).
	// Not counted as freed: a row per trace, gone within the hour.
	// On the wall clock, as the rows are stamped and read (#29), not on the
	// pass's own clock.
	if err := sw.sweepVoidedUploads(ctx, time.Now().UnixNano()); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrWriterClosed) {
			return err
		}
		failures = append(failures, fmt.Errorf("voided uploads: %w", err))
	}

	entries, err := sw.sweepOrphanSearchEntries(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrWriterClosed) {
			return err
		}
		failures = append(failures, fmt.Errorf("orphaned search entries: %w", err))
	}
	if entries > 0 {
		freed = true
	}

	// Expired browser sessions and spent-by-time invitations (spec 028 #4,
	// #10). Once per pass rather than once per project: an account belongs
	// to the deployment, not to a project. A lookup already ignores an
	// expired row, so this is about the disk and not about access.
	accounts, err := sw.sweepAccounts(ctx, start.UnixNano())
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrWriterClosed) {
			return err
		}
		failures = append(failures, fmt.Errorf("expired sessions and invitations: %w", err))
	}
	if accounts > 0 {
		freed = true
	}

	// What an explicit deletion asked for, after this pass's own deletions
	// so that a purge finished above is compacted in the same pass
	// (spec 044 #11). A compaction drains the whole freelist; a pass without
	// one hands back one bounded job's worth of what its own deletions freed.
	_, drained, err := sw.compact(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, ErrWriterClosed) {
			return err
		}
		failures = append(failures, fmt.Errorf("compaction: %w", err))
	}
	if freed && !drained {
		if err := sw.drainFreelist(ctx, false); err != nil {
			failures = append(failures, fmt.Errorf("incremental vacuum: %w", err))
		}
	}

	// The newest pre-migration backup, seven days after it was written
	// (spec 044 #12). Files, not rows: nothing here goes through the writer.
	sw.store.expireBackups(start)

	sw.mu.Lock()
	sw.lastRun = start
	sw.mu.Unlock()

	return errors.Join(failures...)
}

// sweepProject applies one project's windows. It reports whether anything was
// deleted, so the pass knows whether a vacuum has anything to reclaim.
func (sw *Sweeper) sweepProject(ctx context.Context, project *Project, at time.Time) (bool, error) {
	now := at.UnixNano()
	purging := project.Deleted() && now >= project.PurgeAt()

	var removed bool
	traces, tracesDrained, err := sw.sweepTraces(ctx, project.ID, now, purging)
	if traces > 0 {
		removed = true
		sw.count(project.ID, traces, 0)
	}
	if err != nil {
		return removed, err
	}

	raw, rawDrained, err := sw.sweepRawBatches(ctx, project.ID, now, purging)
	if raw > 0 {
		removed = true
		sw.count(project.ID, 0, raw)
	}
	if err != nil {
		return removed, err
	}

	if purging && tracesDrained && rawDrained {
		// Only once the chunks have actually run out. The project row
		// takes the small remainder (keys, scores, prompts, labels) with
		// it through the cascades of 0001–0003 — but `payloads` has no
		// project column and so no cascade (spec 002 #8), so dropping
		// the row while traces remain would strand every payload they
		// own for the orphan pass to find a few thousand at a time.
		// A backlog larger than one pass simply finishes next pass.
		purge := &projectPurge{ProjectID: project.ID, Now: now}
		if err := sw.writer.Submit(ctx, purge); err != nil {
			return removed, err
		}
		if purge.Purged {
			removed = true
			logger().Info("purged deleted project after its grace window",
				"project", project.Name, "deleted_at", project.DeletedAt)
		}
	} else if purging {
		logger().Info("purge of a deleted project continues next pass",
			"project", project.Name, "traces_removed", traces, "raw_batches_removed", raw)
	}
	return removed, nil
}

// sweepTraces removes expired traces chunk by chunk until a chunk comes back
// short, which is what says the window is clear. It reports whether it got
// that far: the per-pass chunk bound stops a huge backlog from monopolising
// the writer, and a caller that has more to do than delete rows — the purge —
// has to know the difference between "done" and "out of chunks".
func (sw *Sweeper) sweepTraces(ctx context.Context, projectID string, now int64, purge bool) (int64, bool, error) {
	var total int64
	for range sw.maxChunks {
		chunk := &traceSweep{ProjectID: projectID, Now: now, Purge: purge, Limit: sw.chunk}
		if err := sw.writer.Submit(ctx, chunk); err != nil {
			return total, false, err
		}
		total += chunk.Traces
		if chunk.Traces < int64(sw.chunk) {
			return total, true, nil
		}
	}
	return total, false, nil
}

func (sw *Sweeper) sweepRawBatches(ctx context.Context, projectID string, now int64, purge bool) (int64, bool, error) {
	var total int64
	for range sw.maxChunks {
		chunk := &rawSweep{ProjectID: projectID, Now: now, Purge: purge, Limit: sw.chunk}
		if err := sw.writer.Submit(ctx, chunk); err != nil {
			return total, false, err
		}
		total += chunk.Deleted
		if chunk.Deleted < int64(sw.chunk) {
			return total, true, nil
		}
	}
	return total, false, nil
}

// sweepOrphanPayloads collects payload rows nothing references any more:
// what an overwriting upsert leaves behind (spec 005 #4, and the promise
// migration 0002's own comment made).
//
// Finding them is a read outside the write transaction, and that is safe
// rather than sloppy: a payload row is written by the same transaction as the
// row that points at it and is never adopted afterwards, so a payload that is
// unreferenced now cannot become referenced later. Only the deletion needs the
// writer, and it gets one short transaction instead of a table scan inside it.
func (sw *Sweeper) sweepOrphanPayloads(ctx context.Context) (int64, error) {
	ids, err := sw.store.orphanPayloads(ctx, orphanScanLimit)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	job := &payloadSweep{IDs: ids}
	if err := sw.writer.Submit(ctx, job); err != nil {
		return 0, err
	}
	if job.Deleted > 0 {
		logger().Info("collected orphaned payloads", "payloads", job.Deleted)
	}
	return job.Deleted, nil
}

// sweepOrphanMedia collects the pending refs whose trace never arrived within
// the grace (spec 041, Decision 13) and any body no ref names at all, then the
// bodies those refs leave. Found by a read outside the writer, like the
// orphaned payloads; the job re-checks each predicate inside its transaction.
func (sw *Sweeper) sweepOrphanMedia(ctx context.Context, now int64) (int64, error) {
	before := now - int64(MediaOrphanGrace)
	refs, err := sw.store.orphanMediaRefs(ctx, before, orphanScanLimit)
	if err != nil {
		return 0, err
	}
	bodies, next, err := sw.store.orphanMedia(ctx, sw.mediaCursor, orphanScanLimit)
	if err != nil {
		return 0, err
	}
	// The holds of the same page out of step with the refs (spec 041 #26).
	stale, missing, err := sw.store.holdDrift(ctx, sw.mediaCursor, next)
	if err != nil {
		return 0, err
	}
	sw.mediaCursor = next
	if len(refs) == 0 && len(bodies) == 0 && len(stale) == 0 && len(missing) == 0 {
		return 0, nil
	}
	job := &mediaSweep{Refs: refs, Bodies: bodies, Stale: stale, Missing: missing, Now: now, Before: before}
	if err := sw.writer.Submit(ctx, job); err != nil {
		return 0, err
	}
	if job.Deleted > 0 || job.Dropped > 0 || job.Released > 0 || job.Restored > 0 {
		logger().Info("collected orphaned media", "refs", job.Dropped, "holds_released", job.Released,
			"holds_restored", job.Restored, "bodies", job.Deleted)
	}
	// Bodies only: a dropped or settled ref frees no page worth a vacuum.
	return job.Deleted, nil
}

// sweepOrphanSearchEntries collects index entries whose observation — or whose
// trace — is gone (spec 011, data contract). The three deletion paths keep the
// index in step inside their own transactions; this is the belt to those
// braces, and the only thing that would ever find an entry a hand-edited
// database left behind.
func (sw *Sweeper) sweepOrphanSearchEntries(ctx context.Context) (int64, error) {
	ids, err := sw.store.orphanSearchEntries(ctx, orphanScanLimit)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	job := &searchEntrySweep{IDs: ids}
	if err := sw.writer.Submit(ctx, job); err != nil {
		return 0, err
	}
	if job.Deleted > 0 {
		logger().Info("collected orphaned search entries", "entries", job.Deleted)
	}
	return job.Deleted, nil
}

// sweepVoidedUploads forgets the traces removed longer ago than an upload
// URL lives, and the slack, in bounded chunks (spec 041 #29).
func (sw *Sweeper) sweepVoidedUploads(ctx context.Context, now int64) error {
	for range sw.maxChunks {
		chunk := &mediaVoidedSweep{Before: now - int64(MediaUploadWindow+mediaVoidedSlack), Limit: sw.chunk}
		if err := sw.writer.Submit(ctx, chunk); err != nil {
			return err
		}
		if chunk.Removed < int64(sw.chunk) {
			return nil
		}
	}
	return nil
}

// sweepAccounts removes the browser sessions and invitations that have run
// out (spec 028 #4). Chunked like everything else here, so a deployment that
// has been away for a month does not hold the writer for one enormous DELETE;
// what a pass does not reach, the next one does.
func (sw *Sweeper) sweepAccounts(ctx context.Context, now int64) (int64, error) {
	var total int64
	for range sw.maxChunks {
		chunk := &AccountSweep{Now: now, Limit: sw.chunk}
		if err := sw.writer.Submit(ctx, chunk); err != nil {
			return total, err
		}
		removed := chunk.Sessions + chunk.Tokens
		total += removed
		if chunk.Sessions < int64(sw.chunk) && chunk.Tokens < int64(sw.chunk) {
			break
		}
	}
	if total > 0 {
		logger().Info("removed expired sessions and invitations", "rows", total)
	}
	return total, nil
}

func (sw *Sweeper) count(projectID string, traces, raw int64) {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	entry := sw.deleted[projectID]
	if entry == nil {
		entry = &sweepCounters{}
		sw.deleted[projectID] = entry
	}
	entry.traces += traces
	entry.raw += raw
}

// orphanPayloads lists payload rows no owner column points at.
//
// It is a scan of `payloads` minus three scans of `observations`, deliberately
// un-indexed: indexing the three payload columns would tax every ingest write
// to speed up an hourly maintenance query, and at this product's scale (design
// §5.6 names the ceiling) the scan is milliseconds. The LIMIT is what keeps
// the cost bounded if that ever stops being true.
func (s *Store) orphanPayloads(ctx context.Context, limit int) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id FROM payloads
		 EXCEPT SELECT metadata_id FROM traces WHERE metadata_id IS NOT NULL
		 EXCEPT SELECT input_id FROM observations WHERE input_id IS NOT NULL
		 EXCEPT SELECT output_id FROM observations WHERE output_id IS NOT NULL
		 EXCEPT SELECT metadata_id FROM observations WHERE metadata_id IS NOT NULL
		 LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("find orphaned payloads: %w", err)
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

// traceSweep deletes one chunk of a project's expired traces with everything
// hanging off them.
//
// The window is recomputed from the stored project row inside the transaction
// rather than passed in: retention can change and a project can be restored
// between the pass reading the list and the writer reaching this job, and a
// deletion decided on stale state is the one mistake retention cannot take
// back (spec 003 Decision 20).
type traceSweep struct {
	ProjectID string
	Now       int64
	// Purge ignores the retention window and takes everything, for a
	// project whose grace window has run out (spec 005 #9).
	Purge bool
	Limit int

	// Filled by apply. Assigned rather than accumulated: a window that
	// fails is retried job by job, so apply can run more than once.
	Traces       int64
	Observations int64
	Scores       int64
	Payloads     int64
	// Items are the annotation queue items pointing at the swept traces
	// (spec 024 #3): an item is a pointer, and a pointer to a deleted trace
	// is a desk showing an empty page.
	Items int64
	// Media are the bodies the chunk collected (spec 041 #3).
	Media int64
}

func (t *traceSweep) apply(tx *sql.Tx) error {
	t.Traces, t.Observations, t.Scores, t.Payloads, t.Items, t.Media = 0, 0, 0, 0, 0, 0

	cutoff, sweeping, err := sweepCutoff(tx, t.ProjectID, t.Now, t.Purge)
	if err != nil || !sweeping {
		return err
	}

	ids, err := expiredTraceIDs(tx, t.ProjectID, cutoff, t.Limit, t.Purge)
	if err != nil || len(ids) == 0 {
		return err
	}
	// The payload ids have to be read before their owners go: `payloads`
	// has no owner column by design (spec 002 #8), and the foreign keys
	// run the other way, so a payload cannot be deleted while the row
	// pointing at it still exists (Decision 17).
	payloads, err := referencedPayloads(tx, t.ProjectID, ids)
	if err != nil {
		return err
	}

	if t.Observations, err = deleteIn(tx,
		`DELETE FROM observations WHERE project_id = ? AND trace_id IN`,
		[]any{t.ProjectID}, ids); err != nil {
		return fmt.Errorf("sweep observations: %w", err)
	}
	if t.Scores, err = deleteIn(tx,
		`DELETE FROM scores WHERE project_id = ? AND trace_id IN`,
		[]any{t.ProjectID}, ids); err != nil {
		return fmt.Errorf("sweep scores: %w", err)
	}
	// In the same job as the traces themselves (spec 024 #3), through
	// `idx_annotation_items_trace`: items follow their trace, and a queue
	// that outlived its evidence would hand a reviewer a not-found page.
	if t.Items, err = deleteIn(tx,
		`DELETE FROM annotation_items WHERE project_id = ? AND trace_id IN`,
		[]any{t.ProjectID}, ids); err != nil {
		return fmt.Errorf("sweep annotation items: %w", err)
	}
	if t.Traces, err = deleteIn(tx,
		`DELETE FROM traces WHERE project_id = ? AND id IN`,
		[]any{t.ProjectID}, ids); err != nil {
		return fmt.Errorf("sweep traces: %w", err)
	}
	if t.Payloads, err = deleteIn(tx,
		`DELETE FROM payloads WHERE id IN`, nil, payloads); err != nil {
		return fmt.Errorf("sweep payloads: %w", err)
	}
	// Media follows its traces (spec 041 #3): no window of its own.
	drop, err := dropTraceMedia(tx, t.ProjectID, ids)
	if err != nil {
		return err
	}
	t.Media = drop.Collected
	// In the same transaction as the rows themselves: an index that outlives
	// a deletion is a retention promise broken (spec 011 #7).
	return deleteTraceSearchEntries(tx, t.ProjectID, ids)
}

// rawSweep deletes one chunk of a project's expired raw bodies. Raw has its
// own window (spec 005 #6) and a batch is swept only by its own age: one batch
// feeds many traces with different fates, so the traces it fed being gone says
// nothing about it.
type rawSweep struct {
	ProjectID string
	Now       int64
	Purge     bool
	Limit     int

	Deleted int64
	// Media are the bodies the chunk collected.
	Media int64
}

func (r *rawSweep) apply(tx *sql.Tx) error {
	r.Deleted, r.Media = 0, 0

	cutoff, sweeping, err := rawSweepCutoff(tx, r.ProjectID, r.Now, r.Purge)
	if err != nil || !sweeping {
		return err
	}
	// The ids first: the batches' media refs go with them, and the bodies
	// only they pointed at are collected in the same transaction (spec 041,
	// Decision 12).
	ids, err := queryColumn[int64](tx,
		`SELECT id FROM raw_batches WHERE project_id = ? AND received_at < ?
		  ORDER BY received_at LIMIT ?`, r.ProjectID, cutoff, r.Limit)
	if err != nil {
		return fmt.Errorf("sweep raw batches: %w", err)
	}
	if len(ids) == 0 {
		return nil
	}
	drop, err := dropRawMedia(tx, r.ProjectID, ids)
	if err != nil {
		return err
	}
	r.Media = drop.Collected
	r.Deleted, err = deleteIn(tx, `DELETE FROM raw_batches WHERE id IN`, nil, ids)
	if err != nil {
		return fmt.Errorf("sweep raw batches: %w", err)
	}
	return nil
}

// payloadSweep removes the orphans the read pass found.
type payloadSweep struct {
	IDs     []int64
	Deleted int64
}

func (p *payloadSweep) apply(tx *sql.Tx) error {
	p.Deleted = 0
	ids := make([]any, 0, len(p.IDs))
	for _, id := range p.IDs {
		ids = append(ids, id)
	}
	var err error
	p.Deleted, err = deleteIn(tx, `DELETE FROM payloads WHERE id IN`, nil, ids)
	if err != nil {
		return fmt.Errorf("sweep orphaned payloads: %w", err)
	}
	return nil
}

// projectPurge removes a soft-deleted project once its grace window has run
// out. The window is checked against the stored row inside the transaction:
// the restore that would make this wrong is one HTTP request away.
type projectPurge struct {
	ProjectID string
	Now       int64
	Purged    bool
}

func (p *projectPurge) apply(tx *sql.Tx) error {
	p.Purged = false
	result, err := tx.Exec(
		`DELETE FROM projects
		  WHERE id = ? AND deleted_at IS NOT NULL AND deleted_at <= ?`,
		p.ProjectID, p.Now-int64(GraceWindow))
	if err != nil {
		return fmt.Errorf("purge project: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	p.Purged = rows > 0
	if !p.Purged {
		return nil
	}
	// The project's data went in this pass's chunks and goes here; the
	// same pass compacts after it (spec 044 #11).
	if _, err := requestCompaction(tx, p.Now); err != nil {
		return err
	}
	// Nor have `media_refs` and `media_holders` (schemas 0021, 0022): what
	// is left of them — a ref the Langfuse channel wrote for a trace that
	// never arrived — goes here, with the bodies only this project pointed
	// at (spec 041 #3).
	if err := dropProjectMedia(tx, p.ProjectID); err != nil {
		return err
	}
	// `search_entries` has no foreign key to cascade from (schema 0006), so
	// the remainder the drained chunks left behind goes here (spec 011 #7).
	return deleteProjectSearchEntries(tx, p.ProjectID)
}

// incrementalVacuum hands freed pages back to the filesystem. It runs as a
// job like everything else: the pragma takes the write lock, and taking it on
// a second connection is exactly what the one-writer rule exists to prevent.
type incrementalVacuum struct{ Pages int }

func (v *incrementalVacuum) apply(tx *sql.Tx) error {
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA incremental_vacuum(%d)`, v.Pages)); err != nil {
		return fmt.Errorf("incremental vacuum: %w", err)
	}
	return nil
}

// sweepCutoff reads the project's trace window from the stored row. It reports
// whether there is anything to sweep at all: a NULL retention means keep
// forever (spec 005 #2), and a project that is no longer purgable — restored
// while the pass was running — is left alone.
func sweepCutoff(tx *sql.Tx, projectID string, now int64, purge bool) (int64, bool, error) {
	var (
		retention sql.NullInt64
		deletedAt sql.NullInt64
	)
	err := tx.QueryRow(`SELECT retention_days, deleted_at FROM projects WHERE id = ?`, projectID).
		Scan(&retention, &deletedAt)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read retention window: %w", err)
	}
	if purge {
		if !deletedAt.Valid || now < deletedAt.Int64+int64(GraceWindow) {
			return 0, false, nil
		}
		return math.MaxInt64, true, nil
	}
	cutoff, sweeping := windowCutoff(retention, now)
	return cutoff, sweeping, nil
}

// rawSweepCutoff is the same for raw bodies, whose window falls back to the
// project's when it has none of its own (spec 005 #6).
func rawSweepCutoff(tx *sql.Tx, projectID string, now int64, purge bool) (int64, bool, error) {
	var (
		retention sql.NullInt64
		raw       sql.NullInt64
		deletedAt sql.NullInt64
	)
	err := tx.QueryRow(
		`SELECT retention_days, raw_retention_days, deleted_at FROM projects WHERE id = ?`, projectID).
		Scan(&retention, &raw, &deletedAt)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read raw retention window: %w", err)
	}
	if purge {
		if !deletedAt.Valid || now < deletedAt.Int64+int64(GraceWindow) {
			return 0, false, nil
		}
		return math.MaxInt64, true, nil
	}
	window := raw
	if !window.Valid {
		window = retention
	}
	cutoff, sweeping := windowCutoff(window, now)
	return cutoff, sweeping, nil
}

// windowCutoff resolves a stored window column into the arrival time before
// which rows expire. NULL means keep forever (spec 005 #2), and so does a
// window past the ceiling cutoffFor guards — the sweeper and the dry run share
// that reading, so a preview can never promise something the sweep would do
// differently.
func windowCutoff(days sql.NullInt64, now int64) (int64, bool) {
	if !days.Valid {
		return 0, false
	}
	window := int(days.Int64)
	return cutoffFor(&window, now)
}

// notPinned is the predicate that keeps a trace of a live run out of the sweep
// (spec 014 #13): a trace whose `run_id` names an existing run of its project
// is evidence somebody chose to keep, and only deleting the run releases it.
// The sweep and the retention dry run share the predicate, so the preview
// cannot promise something the pass would do differently (spec 005 #8). The
// table is aliased `t` wherever it appears.
const notPinned = `(t.run_id IS NULL OR NOT EXISTS
	(SELECT 1 FROM dataset_runs r WHERE r.project_id = t.project_id AND r.id = t.run_id))`

// expiredTraceIDs picks the chunk, oldest arrival first, minus the pinned
// ones. Served by idx_traces_ingested; the plan is asserted in the tests,
// because a sweep that scans the project every hour is a sweep that will be
// turned off.
//
// A purge takes them all. The pin is a choice somebody made inside a project
// (spec 014 #13), and a project whose grace window has run out is a project
// whose runs are going too — leaving its pinned traces behind would stall the
// drain `sweepProject` waits for, and dropping the `projects` row with them
// still there would strand their payloads, which is the whole reason it waits
// (spec 005 #4, found in review of PR #30).
func expiredTraceIDs(tx *sql.Tx, projectID string, cutoff int64, limit int, purge bool) ([]any, error) {
	query := expiredTracesQuery
	if purge {
		query = purgedTracesQuery
	}
	rows, err := tx.Query(query, projectID, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("select expired traces: %w", err)
	}
	defer rows.Close()

	ids := make([]any, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// expiredTracesQuery is the sweep's pick as shipped, so the plan test asserts
// the exact statement. Binds the project, the cutoff and the chunk size.
const expiredTracesQuery = `SELECT id FROM traces t
	  WHERE t.project_id = ? AND t.ingested_at < ? AND ` + notPinned + `
	  ORDER BY t.ingested_at LIMIT ?`

// purgedTracesQuery is the same pick without the pin, for a project that is
// being destroyed rather than swept.
const purgedTracesQuery = `SELECT id FROM traces t
	  WHERE t.project_id = ? AND t.ingested_at < ?
	  ORDER BY t.ingested_at LIMIT ?`

// referencedPayloads gathers the payload rows the chunk's traces and their
// observations own, so they go with them rather than becoming orphans the next
// pass has to find (spec 005 #4).
func referencedPayloads(tx *sql.Tx, projectID string, traceIDs []any) ([]any, error) {
	var ids []any
	collect := func(query string) error {
		return eachIn(traceIDs, func(batch []any) error {
			args := append([]any{projectID}, batch...)
			rows, err := tx.Query(query+" ("+placeholders(len(batch))+")", args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					return err
				}
				ids = append(ids, id)
			}
			return rows.Err()
		})
	}
	if err := collect(`SELECT metadata_id FROM traces
	                    WHERE metadata_id IS NOT NULL AND project_id = ? AND id IN`); err != nil {
		return nil, fmt.Errorf("collect trace payloads: %w", err)
	}
	for _, column := range []string{"input_id", "output_id", "metadata_id"} {
		if err := collect(`SELECT ` + column + ` FROM observations
		                    WHERE ` + column + ` IS NOT NULL AND project_id = ? AND trace_id IN`); err != nil {
			return nil, fmt.Errorf("collect observation payloads: %w", err)
		}
	}
	return ids, nil
}

// inBatch is how many ids one statement binds. SQLite's parameter limit is far
// higher, but a bounded statement keeps the plan cache from being churned by a
// different-shaped query on every chunk.
const inBatch = 500

// deleteIn runs a DELETE whose predicate ends in `IN (…)`, in statements small
// enough to stay under the bound-parameter limit.
func deleteIn(tx *sql.Tx, prefix string, lead []any, ids []any) (int64, error) {
	var total int64
	err := eachIn(ids, func(batch []any) error {
		args := append(append([]any{}, lead...), batch...)
		result, err := tx.Exec(prefix+" ("+placeholders(len(batch))+")", args...)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		total += affected
		return nil
	})
	return total, err
}

// rowsQuerier is what a column read needs of a transaction or of the
// database.
type rowsQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

// queryColumn runs a query whose rows are one column of type T and answers
// the values, as the []any an IN list takes.
func queryColumn[T any](q rowsQuerier, query string, args ...any) ([]any, error) {
	rows, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []any
	for rows.Next() {
		var v T
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// eachIn walks an id list in statement-sized batches. An empty list runs
// nothing at all: `IN ()` is not SQL, and "delete none of them" is a no-op
// rather than a statement.
func eachIn(ids []any, do func([]any) error) error {
	for start := 0; start < len(ids); start += inBatch {
		if err := do(ids[start:min(start+inBatch, len(ids))]); err != nil {
			return err
		}
	}
	return nil
}

func placeholders(n int) string {
	out := make([]byte, 0, 2*n)
	for i := range n {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, '?')
	}
	return string(out)
}
