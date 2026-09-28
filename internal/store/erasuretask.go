package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// An erasure is a task (spec 047 #6): the confirmed request records one and
// answers, and one worker beside the sweeper and the aggregator runs it (#10).
// Its progress is written in the transactions that make it (#12) — a scrub's
// tally with the scrub, a chunk's counts and tail with the chunk — so that a
// restart resumes it from its phase, and nothing it reports was not committed.

// The states and phases of an erasure (#8).
const (
	ErasureQueued  = "queued"
	ErasureRunning = "running"
	ErasureDone    = "done"
	ErasureFailed  = "failed"

	phaseRaw    = "raw"
	phaseParsed = "parsed"
	phaseTail   = "tail"
)

// DefaultEraseChunk is the most of a user's traces one transaction of the
// parsed phase removes (spec 047 #1): each chunk re-rolls the hours it
// empties in that same transaction (spec 023 #19), so it is bounded by the
// roll budget as well.
const DefaultEraseChunk = 500

// ErasureKept is how long a finished erasure's record stays (#15).
const ErasureKept = 30 * 24 * time.Hour

// ErasureListed is the most erasures a listing returns (#14).
const ErasureListed = 100

// erasureAttempts is how many starts an erasure gets: the one after the third
// that a restart interrupted ends it failed (#12).
const erasureAttempts = 3

// erasurePoll is how often an idle worker looks for an erasure it was not
// woken for; a wake is what normally starts one.
const erasurePoll = time.Minute

// Erasure is one erasure task as its row holds it.
type Erasure struct {
	ID        string
	ProjectID string
	// UserID is empty once the erasure has ended (#9).
	UserID string
	State  string
	// Phase is raw, parsed or tail while running, and empty otherwise.
	Phase                            string
	CreatedAt, StartedAt, FinishedAt int64
	// TracesAtStart is step 1's count, nil before step 1 has read it.
	TracesAtStart *int64
	// Counts is what has been committed so far.
	Counts DeleteCounts
	// Compaction is the latest compaction the erasure asked for, zero
	// while it has deleted nothing (spec 044 #17).
	Compaction int64
	Error      string

	since, now int64
	attempts   int
}

// Ended reports an erasure that is done or failed.
func (e *Erasure) Ended() bool { return e.State == ErasureDone || e.State == ErasureFailed }

const erasureColumns = `id, project_id, user_id, state, phase, created_at, started_at, finished_at,
	since, now, attempts, traces_at_start, counts, compaction, error`

func scanErasure(row interface{ Scan(...any) error }) (*Erasure, error) {
	var (
		e                           Erasure
		user, phase, failure        sql.NullString
		started, finished, since, n sql.NullInt64
		counts                      string
	)
	if err := row.Scan(&e.ID, &e.ProjectID, &user, &e.State, &phase, &e.CreatedAt, &started, &finished,
		&since, &e.now, &e.attempts, &n, &counts, &e.Compaction, &failure); err != nil {
		return nil, err
	}
	e.UserID, e.Phase, e.Error = user.String, phase.String, failure.String
	e.StartedAt, e.FinishedAt, e.since = started.Int64, finished.Int64, since.Int64
	if n.Valid {
		e.TracesAtStart = &n.Int64
	}
	var stored storedCounts
	if err := json.Unmarshal([]byte(counts), &stored); err != nil {
		return nil, fmt.Errorf("read erasure %s's counts: %w", e.ID, err)
	}
	e.Counts = stored.counts()
	return &e, nil
}

// storedCounts is the `counts` column: the resource's `deleted` keys.
type storedCounts struct {
	Traces              int64 `json:"traces,omitempty"`
	Observations        int64 `json:"observations,omitempty"`
	Scores              int64 `json:"scores,omitempty"`
	SessionScores       int64 `json:"session_scores,omitempty"`
	Payloads            int64 `json:"payloads,omitempty"`
	AnnotationItems     int64 `json:"annotation_items,omitempty"`
	DatasetItems        int64 `json:"dataset_items,omitempty"`
	Media               int64 `json:"media,omitempty"`
	MediaBytes          int64 `json:"media_bytes,omitempty"`
	RawSpans            int64 `json:"raw_spans,omitempty"`
	RawBatchesRewritten int64 `json:"raw_batches_rewritten,omitempty"`
	RawBatchesDeleted   int64 `json:"raw_batches_deleted,omitempty"`
}

func (s storedCounts) counts() DeleteCounts {
	return DeleteCounts{Traces: s.Traces, Observations: s.Observations, Scores: s.Scores,
		SessionScores: s.SessionScores, Payloads: s.Payloads, AnnotationItems: s.AnnotationItems,
		DatasetItems: s.DatasetItems, Media: s.Media, MediaBytes: s.MediaBytes, RawSpans: s.RawSpans,
		RawBatchesRewritten: s.RawBatchesRewritten, RawBatchesDeleted: s.RawBatchesDeleted}
}

func storeCounts(c DeleteCounts) storedCounts {
	return storedCounts{Traces: c.Traces, Observations: c.Observations, Scores: c.Scores,
		SessionScores: c.SessionScores, Payloads: c.Payloads, AnnotationItems: c.AnnotationItems,
		DatasetItems: c.DatasetItems, Media: c.Media, MediaBytes: c.MediaBytes, RawSpans: c.RawSpans,
		RawBatchesRewritten: c.RawBatchesRewritten, RawBatchesDeleted: c.RawBatchesDeleted}
}

func txErasure(tx *sql.Tx, id string) (*Erasure, error) {
	e, err := scanErasure(tx.QueryRow(`SELECT `+erasureColumns+` FROM erasures WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return e, err
}

// recordProgress adds what a job committed to its erasure's row, in the job's
// own transaction (#12), and moves the phase when the job ends one. An
// erasure that is gone — its project purged — or over records nothing.
func recordProgress(tx *sql.Tx, id string, counts DeleteCounts, compaction int64, phase string) (bool, error) {
	var stored string
	err := tx.QueryRow(`SELECT counts FROM erasures WHERE id = ? AND state = ?`, id, ErasureRunning).Scan(&stored)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read erasure %s: %w", id, err)
	}
	var sum storedCounts
	if err := json.Unmarshal([]byte(stored), &sum); err != nil {
		return false, fmt.Errorf("read erasure %s's counts: %w", id, err)
	}
	total := sum.counts()
	total.add(counts)
	encoded, err := json.Marshal(storeCounts(total))
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE erasures SET counts = ?, compaction = MAX(compaction, ?),
		phase = COALESCE(NULLIF(?, ''), phase) WHERE id = ?`, string(encoded), compaction, phase, id); err != nil {
		return false, fmt.Errorf("record erasure %s's progress: %w", id, err)
	}
	return true, nil
}

// chunkErasure is what a chunk of the parsed phase needs to record itself on
// its erasure's row: which traces step 1 read — nil after a resume — and
// when, to tell which of the traces it deletes received a batch step 2 did
// not read.
type chunkErasure struct {
	ID     string
	Since  int64
	Known  map[string]bool
	Legacy legacyStamps
}

// record adds the chunk's counts to its erasure, stores the tail of each
// trace it deleted that has one, and moves the erasure to its tail with the
// last chunk — all in the chunk's own transaction (#12).
func (c *chunkErasure) record(tx *sql.Tx, e *UserDataErase) error {
	phase := phaseParsed
	if !e.More {
		phase = phaseTail
	}
	if running, err := recordProgress(tx, c.ID, e.Counts, e.CompactionRequested, phase); err != nil || !running {
		return err
	}
	for i, id := range e.IDs {
		updated := e.Updated[i]
		var w arrivalWindow
		switch {
		case c.Known != nil && !c.Known[id], c.Known == nil && updated >= c.Since-stampMargin:
			// Not the user's when step 1 read them — its first spans
			// came without the id and a later batch brought it; a
			// root span ends, and is exported, last — so step 2 read
			// none of its batches: its whole window. After a resume
			// the chunk cannot tell, and reads the whole window of
			// every trace that changed since step 1.
			w = c.Legacy.windowOf(erasedTrace{id: id, ingestedAt: e.Ingested[i], updateAt: updated})
		case updated >= c.Since-stampMargin:
			// Updated after step 1 read it — minus the margin a
			// commit can trail its stamp by — it received a batch
			// step 2 did not read.
			w = arrivalWindow{from: c.Since - stampMargin, to: updated}
		default:
			continue
		}
		// A trace a late span brought back after a chunk deleted it is
		// deleted again, and keeps one window that covers both.
		if _, err := tx.Exec(`INSERT INTO erasure_tail (erasure_id, trace_id, arrived_from, arrived_to)
			VALUES (?, ?, ?, ?) ON CONFLICT (erasure_id, trace_id) DO UPDATE SET
			arrived_from = MIN(arrived_from, excluded.arrived_from), arrived_to = MAX(arrived_to, excluded.arrived_to)`,
			c.ID, id, w.from, w.to); err != nil {
			return fmt.Errorf("record erasure %s's tail: %w", c.ID, err)
		}
	}
	return nil
}

// erasureStart records an erasure, or finds the one of the same user that is
// queued or running (#11).
type erasureStart struct {
	ProjectID, UserID string
	Now               int64

	Erasure *Erasure
	Created bool
}

func (j *erasureStart) apply(tx *sql.Tx) error {
	j.Erasure, j.Created = nil, false
	project, err := projectByID(tx, j.ProjectID)
	if err != nil {
		return err
	}
	if project == nil {
		return &Rejection{Kind: RejectNotFound, Message: "no such project"}
	}
	running, err := scanErasure(tx.QueryRow(`SELECT `+erasureColumns+` FROM erasures
		WHERE project_id = ? AND user_id = ?`, j.ProjectID, j.UserID))
	if err == nil {
		j.Erasure = running
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("find a running erasure: %w", err)
	}
	// Random, never derived from the user id: a hash of an id is a record
	// of the person, since an id space is small enough to walk (#8).
	id, err := randomHex(16)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO erasures (id, project_id, user_id, state, created_at, now)
		VALUES (?, ?, ?, ?, ?, ?)`, id, j.ProjectID, j.UserID, ErasureQueued, time.Now().UnixNano(),
		nowOr(j.Now)); err != nil {
		return fmt.Errorf("record an erasure: %w", err)
	}
	if j.Erasure, err = txErasure(tx, id); err != nil {
		return err
	}
	j.Created = true
	return nil
}

// erasureBegin is a start of an erasure by the worker, the first or a
// resume: it counts the attempt, and fixes step 1's moment the first time
// (#12). An erasure started three times already ends failed instead.
type erasureBegin struct {
	ID string

	Erasure *Erasure
	// Gone reports an erasure that is not there or is over; GaveUp one
	// this start ended.
	Gone, GaveUp bool
}

func (j *erasureBegin) apply(tx *sql.Tx) error {
	j.Erasure, j.Gone, j.GaveUp = nil, false, false
	e, err := txErasure(tx, j.ID)
	if err != nil {
		return err
	}
	if e == nil || e.Ended() {
		j.Gone = true
		return nil
	}
	now := time.Now().UnixNano()
	if e.attempts >= erasureAttempts {
		j.GaveUp = true
		j.Erasure, err = finishErasure(tx, j.ID, fmt.Sprintf("interrupted by %d restarts", erasureAttempts), now)
		return err
	}
	if _, err := tx.Exec(`UPDATE erasures SET state = ?, attempts = attempts + 1,
		started_at = COALESCE(started_at, ?), since = COALESCE(since, ?), phase = COALESCE(phase, ?)
		WHERE id = ?`, ErasureRunning, now, now, phaseRaw, j.ID); err != nil {
		return fmt.Errorf("start erasure %s: %w", j.ID, err)
	}
	j.Erasure, err = txErasure(tx, j.ID)
	return err
}

// erasureStep records what a phase learned outside a job of its own: step 1's
// count, which a resume does not overwrite, and the move to the tail after a
// chunk failed, with the failure the erasure will end with (#16).
type erasureStep struct {
	ID            string
	TracesAtStart *int64
	Phase, Error  string
}

func (j *erasureStep) apply(tx *sql.Tx) error {
	var counted any
	if j.TracesAtStart != nil {
		counted = *j.TracesAtStart
	}
	_, err := tx.Exec(`UPDATE erasures SET traces_at_start = COALESCE(traces_at_start, ?),
		phase = COALESCE(NULLIF(?, ''), phase), error = COALESCE(error, NULLIF(?, ''))
		WHERE id = ? AND state = ?`, counted, j.Phase, j.Error, j.ID, ErasureRunning)
	return err
}

// erasureEnd ends an erasure: done, or failed when it carries a failure.
type erasureEnd struct {
	ID, Error string

	Erasure *Erasure
}

func (j *erasureEnd) apply(tx *sql.Tx) error {
	var err error
	j.Erasure, err = finishErasure(tx, j.ID, j.Error, time.Now().UnixNano())
	return err
}

// finishErasure ends an erasure in one statement that also forgets whom it
// erased (#9), and drops the tail it no longer needs. Nil for an erasure that
// is gone or already over.
func finishErasure(tx *sql.Tx, id, failure string, now int64) (*Erasure, error) {
	result, err := tx.Exec(`UPDATE erasures SET
		error = COALESCE(error, NULLIF(?, '')),
		state = CASE WHEN COALESCE(error, NULLIF(?, '')) IS NULL THEN ? ELSE ? END,
		phase = NULL, user_id = NULL, finished_at = ?
		WHERE id = ? AND state IN (?, ?)`,
		failure, failure, ErasureDone, ErasureFailed, now, id, ErasureQueued, ErasureRunning)
	if err != nil {
		return nil, fmt.Errorf("end erasure %s: %w", id, err)
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM erasure_tail WHERE erasure_id = ?`, id); err != nil {
		return nil, fmt.Errorf("drop erasure %s's tail: %w", id, err)
	}
	return txErasure(tx, id)
}

// erasureSweep removes the finished erasures past ErasureKept (#15).
type erasureSweep struct {
	Before int64

	Removed int64
}

func (j *erasureSweep) apply(tx *sql.Tx) error {
	result, err := tx.Exec(`DELETE FROM erasures WHERE state IN (?, ?) AND finished_at < ?`,
		ErasureDone, ErasureFailed, j.Before)
	if err != nil {
		return fmt.Errorf("remove finished erasures: %w", err)
	}
	j.Removed, err = result.RowsAffected()
	return err
}

// StartErasure records a confirmed erasure and wakes the worker, or answers
// the erasure of the same user that is queued or running (#11). Refusals are
// answered before anything is recorded: a wrong echo, a project that is gone.
func (s *Store) StartErasure(ctx context.Context, writer jobSubmitter, e UserErasure) (*Erasure, error) {
	// The echo (spec 005 #8): the user id, since the user is what goes.
	if e.Confirm != e.UserID {
		return nil, &Rejection{Kind: RejectInvalid, Message: fmt.Sprintf(
			"confirm must be the user id being erased, %q, to erase their data", e.UserID)}
	}
	job := &erasureStart{ProjectID: e.ProjectID, UserID: e.UserID, Now: e.Now}
	if err := writer.Submit(ctx, job); err != nil {
		return nil, err
	}
	if job.Created {
		s.erasures.wakeWorker()
	}
	return job.Erasure, nil
}

// Erasure reads one erasure of a project; nil when there is none of that id —
// removed after its 30 days, or another project's.
func (s *Store) Erasure(ctx context.Context, projectID, id string) (*Erasure, error) {
	e, err := scanErasure(s.db.QueryRowContext(ctx, `SELECT `+erasureColumns+` FROM erasures
		WHERE id = ? AND project_id = ?`, id, projectID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return e, err
}

// Erasures lists a project's erasures, newest first, at most ErasureListed
// (#14).
func (s *Store) Erasures(ctx context.Context, projectID string) ([]*Erasure, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+erasureColumns+` FROM erasures
		WHERE project_id = ? ORDER BY created_at DESC, id LIMIT ?`, projectID, ErasureListed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Erasure
	for rows.Next() {
		e, err := scanErasure(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RunningErasure is the erasure of one user that is queued or running, or nil
// (#13).
func (s *Store) RunningErasure(ctx context.Context, projectID, userID string) (*Erasure, error) {
	e, err := scanErasure(s.db.QueryRowContext(ctx, `SELECT `+erasureColumns+` FROM erasures
		WHERE project_id = ? AND user_id = ?`, projectID, userID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return e, err
}

// AwaitErasure reads an erasure once it has ended, or when wait is up, the
// caller leaves, or the worker stops — whichever is first (#7). Nil when the
// erasure is gone.
func (s *Store) AwaitErasure(ctx context.Context, projectID, id string, wait time.Duration) (*Erasure, error) {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		// Before the read, so that an end committed after it is not missed.
		ended, stopped := s.erasures.watch()
		e, err := s.Erasure(ctx, projectID, id)
		if err != nil || e == nil || e.Ended() {
			return e, err
		}
		select {
		case <-ended:
		case <-deadline.C:
			return e, nil
		case <-ctx.Done():
			return e, nil
		case <-stopped:
			return e, nil
		}
	}
}

// erasureSignals is how the store's callers and its worker find each other:
// a request wakes the worker, and a waiting request hears an erasure end, or
// the worker stop.
type erasureSignals struct {
	wake chan struct{}

	mu      sync.Mutex
	ended   chan struct{}
	stopped chan struct{}
}

func newErasureSignals() *erasureSignals {
	return &erasureSignals{wake: make(chan struct{}, 1), ended: make(chan struct{}),
		stopped: make(chan struct{})}
}

// started is the channel the worker that starts now closes when it stops.
func (e *erasureSignals) started() chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stopped = make(chan struct{})
	return e.stopped
}

func (e *erasureSignals) wakeWorker() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// watch is a channel closed by the next end of an erasure, and one closed
// when the worker stops.
func (e *erasureSignals) watch() (ended, stopped <-chan struct{}) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ended, e.stopped
}

func (e *erasureSignals) announceEnd() {
	e.mu.Lock()
	defer e.mu.Unlock()
	close(e.ended)
	e.ended = make(chan struct{})
}

// EraserOptions tunes the erasure worker. Zero fields take defaults.
type EraserOptions struct {
	// Chunk bounds the traces of one transaction of the parsed phase.
	Chunk int

	// after is a test seam, told when each step has finished; an error
	// stops the run there, the way a stop of the server does.
	after func(step int) error
}

// Eraser is the erasure worker (#10): one erasure at a time, a resumed one
// before any queued, oldest first across projects.
type Eraser struct {
	store  *Store
	writer jobSubmitter
	opts   EraserOptions

	stop    context.CancelFunc
	done    chan struct{}
	stopped chan struct{}
	closed  sync.Once
}

// NewEraser builds the worker. It does not start it: whoever owns the
// writer's lifetime owns this one too.
func (s *Store) NewEraser(writer jobSubmitter, opts EraserOptions) *Eraser {
	if opts.Chunk <= 0 {
		opts.Chunk = DefaultEraseChunk
	}
	return &Eraser{store: s, writer: writer, opts: opts}
}

// Start runs erasures until Close: at once any a stop interrupted (#12), then
// each as it is recorded.
func (er *Eraser) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	er.stop = cancel
	er.done = make(chan struct{})
	er.stopped = er.store.erasures.started()
	go func() {
		defer close(er.done)
		for {
			id, err := er.store.nextErasure(ctx)
			if err == nil && id != "" {
				if _, err = er.store.runErasure(ctx, er.writer, id, er.opts); err == nil {
					continue
				}
			}
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				// Left running: the next start takes it again, and
				// the third gives up (#12).
				logger().Error("an erasure stopped before it ended", "erasure", id, "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-er.store.erasures.wake:
			case <-time.After(erasurePoll):
			}
		}
	}()
}

// Close stops the worker and waits for it (#17): the job in flight commits or
// does not, and the erasure stays running for the next start to resume. A
// request waiting on an erasure is answered at once.
func (er *Eraser) Close() error {
	er.closed.Do(func() {
		if er.stop != nil {
			close(er.stopped)
			er.stop()
			<-er.done
		}
	})
	return nil
}

// nextErasure is the erasure the worker takes next: a running one — a stop
// interrupted it — before any queued, oldest first (#10, #12).
func (s *Store) nextErasure(ctx context.Context) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM erasures WHERE state IN (?, ?)
		ORDER BY state = ? DESC, created_at LIMIT 1`, ErasureQueued, ErasureRunning, ErasureRunning).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// submitErasureJob submits one of an erasure's jobs, waiting out a full queue
// (spec 044 #20 h) and retrying a job that failed with a database condition
// (spec 043 #2) with backoff, for as long (#16): a full disk or a busy
// database passes, and nobody watches a background task to retry it by hand.
func submitErasureJob(ctx context.Context, writer jobSubmitter, job WriteJob) error {
	delay := 100 * time.Millisecond
	deadline := time.Now().Add(busyWait)
	for {
		err := submitPatiently(ctx, writer, job)
		if _, condition := Condition(err); !condition || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay = min(2*delay, 10*time.Second)
	}
}

// stopped reports a run that ended because the worker is stopping, not
// because the erasure failed.
func stopped(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrWriterClosed) ||
		errors.Is(err, errRunStopped)
}

// errRunStopped is a run the test seam stopped, as a stop of the server would.
var errRunStopped = errors.New("the erasure's run was stopped")

// failureSentence is the error an erasure ends with, without the id of the
// user it erased (#9) — a refusal quotes the echo it was given.
func failureSentence(err error, userID string) string {
	sentence := err.Error()
	if userID != "" {
		sentence = strings.ReplaceAll(sentence, userID, "the user")
	}
	return sentence
}
