package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
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
	// lastFailure is what the latest tail that failed said (#29).
	lastFailure string
}

// Ended reports an erasure that is done or failed.
func (e *Erasure) Ended() bool { return e.State == ErasureDone || e.State == ErasureFailed }

const erasureColumns = `id, project_id, user_id, state, phase, created_at, started_at, finished_at,
	since, now, attempts, traces_at_start, counts, compaction, error, last_failure`

func scanErasure(row interface{ Scan(...any) error }) (*Erasure, error) {
	var (
		e                           Erasure
		user, phase, failure, last  sql.NullString
		started, finished, since, n sql.NullInt64
		counts                      string
	)
	if err := row.Scan(&e.ID, &e.ProjectID, &user, &e.State, &phase, &e.CreatedAt, &started, &finished,
		&since, &e.now, &e.attempts, &n, &counts, &e.Compaction, &failure, &last); err != nil {
		return nil, err
	}
	e.UserID, e.Phase, e.Error, e.lastFailure = user.String, phase.String, failure.String, last.String
	e.StartedAt, e.FinishedAt, e.since = started.Int64, finished.Int64, since.Int64
	if n.Valid {
		e.TracesAtStart = &n.Int64
	}
	if err := json.Unmarshal([]byte(counts), &e.Counts); err != nil {
		return nil, fmt.Errorf("read erasure %s's counts: %w", e.ID, err)
	}
	return &e, nil
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
	var total DeleteCounts
	if err := json.Unmarshal([]byte(stored), &total); err != nil {
		return false, fmt.Errorf("read erasure %s's counts: %w", id, err)
	}
	total.add(counts)
	encoded, err := json.Marshal(total)
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
// (#12). A start is counted until the run ends or stops cleanly
// (erasurePause), so what the count holds is the starts a crash cut off.
//
// After erasureAttempts of those, the next start is the erasure's last and
// does only its tail, with the failure recorded (#27): the chunks that
// committed deleted traces whose late batches no repeat can find again, so
// they are scrubbed before the erasure gives up. A tail that is cut off too
// is dropped, and the erasure ends failed at its next start. The sentence it
// ends with carries what its tail last failed with, if one did (#29).
//
// Run names this start on the row, so that the pause of a stop takes back
// this start and no other (#29).
type erasureBegin struct {
	ID, Run string

	Erasure *Erasure
	// Gone reports an erasure that is not there or is over; GaveUp one
	// this start ended, and Dropped the tail windows it ended without.
	Gone, GaveUp bool
	Dropped      int64
}

func (j *erasureBegin) apply(tx *sql.Tx) error {
	j.Erasure, j.Gone, j.GaveUp, j.Dropped = nil, false, false, 0
	e, err := txErasure(tx, j.ID)
	if err != nil {
		return err
	}
	if e == nil || e.Ended() {
		j.Gone = true
		return nil
	}
	now := time.Now().UnixNano()
	interrupted := capSentence
	if e.lastFailure != "" {
		interrupted += "; its tail last failed with: " + e.lastFailure
	}
	if e.attempts > erasureAttempts {
		// The last start wrote the cap's sentence before its own tail
		// ran; what that tail failed with is newer (#30). A chunk's
		// failure stays what the erasure ends with.
		if strings.HasPrefix(e.Error, capSentence) {
			if _, err := tx.Exec(`UPDATE erasures SET error = NULL WHERE id = ?`, j.ID); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM erasure_tail WHERE erasure_id = ?`, j.ID).Scan(&j.Dropped); err != nil {
			return err
		}
		j.GaveUp = true
		j.Erasure, err = finishErasure(tx, j.ID, interrupted, now)
		return err
	}
	phase, failure := phaseRaw, ""
	if e.attempts == erasureAttempts {
		phase, failure = phaseTail, interrupted
	}
	if _, err := tx.Exec(`UPDATE erasures SET state = ?, attempts = attempts + 1, run = ?,
		started_at = COALESCE(started_at, ?), since = COALESCE(since, ?),
		phase = CASE WHEN ? != '' THEN ? ELSE COALESCE(phase, ?) END,
		error = COALESCE(error, NULLIF(?, ''))
		WHERE id = ?`, ErasureRunning, j.Run, now, now, failure, phase, phase, failure, j.ID); err != nil {
		return fmt.Errorf("start erasure %s: %w", j.ID, err)
	}
	j.Erasure, err = txErasure(tx, j.ID)
	return err
}

// erasurePause gives back the start of a run the worker's stop ended (#17,
// #27): a clean stop is not a crash, and an erasure longer than a few
// deploys must not run out of starts for them. Only the start its run
// recorded: a stop that came while the start still waited for a full queue
// ended a run that counted nothing, and the count on the row is the crashes
// before it (#29).
type erasurePause struct{ ID, Run string }

func (j *erasurePause) apply(tx *sql.Tx) error {
	_, err := tx.Exec(`UPDATE erasures SET attempts = MAX(attempts - 1, 0), run = NULL
		WHERE id = ? AND state = ? AND run = ?`, j.ID, ErasureRunning, j.Run)
	return err
}

// capSentence is the error of an erasure that ran out of starts (#27, #28).
var capSentence = fmt.Sprintf("%d starts ended before the erasure did", erasureAttempts)

// pauseTime bounds the one write a clean stop makes for its erasure: the
// stop's own budget is ten seconds (spec 001 #16), and a pause that does not
// land leaves the start counted, which is the safe side.
const pauseTime = time.Second

// erasureStep records what a phase learned outside a job of its own: step 1's
// count, which a resume does not overwrite, the move to the tail after a
// chunk failed, with the failure the erasure will end with (#16), and what a
// tail that failed said, which the erasure ends with only if it gives up
// (#29). TailDone is a tail that ran to its end: its windows are scrubbed and
// go, and so does what an earlier one failed with, so an end that is not
// written leaves a give-up nothing to call dropped and nothing stale to say
// (#31).
type erasureStep struct {
	ID            string
	TracesAtStart *int64
	Phase, Error  string
	TailFailure   string
	TailDone      bool
}

func (j *erasureStep) apply(tx *sql.Tx) error {
	var counted any
	if j.TracesAtStart != nil {
		counted = *j.TracesAtStart
	}
	result, err := tx.Exec(`UPDATE erasures SET traces_at_start = COALESCE(traces_at_start, ?),
		phase = COALESCE(NULLIF(?, ''), phase), error = COALESCE(error, NULLIF(?, '')),
		last_failure = CASE WHEN ? THEN NULL ELSE COALESCE(NULLIF(?, ''), last_failure) END
		WHERE id = ? AND state = ?`, counted, j.Phase, j.Error, j.TailDone, j.TailFailure, j.ID, ErasureRunning)
	if err != nil || !j.TailDone {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n == 0 {
		return err
	}
	_, err = tx.Exec(`DELETE FROM erasure_tail WHERE erasure_id = ?`, j.ID)
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
		phase = NULL, user_id = NULL, finished_at = ?, run = NULL, last_failure = NULL
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

// Erasures lists a project's erasures, at most ErasureListed: those queued or
// running first, then the rest newest first (#14, #28). An erasure under way
// is what a screen looks for in the listing, and it is never past the cut.
func (s *Store) Erasures(ctx context.Context, projectID string) ([]*Erasure, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+erasureColumns+` FROM erasures
		WHERE project_id = ? ORDER BY state IN (?, ?) DESC, created_at DESC, id LIMIT ?`,
		projectID, ErasureQueued, ErasureRunning, ErasureListed)
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
	// Poll is how long an idle worker waits before it looks for work it
	// was not woken for, and how long one waits after a run that failed;
	// zero is a minute.
	Poll time.Duration

	// after is a test seam, told when each step has finished; an error
	// stops the run there, the way a stop of the server does.
	after func(step int) error
}

func (o EraserOptions) poll() time.Duration {
	if o.Poll <= 0 {
		return erasurePoll
	}
	return o.Poll
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
		// The erasures whose run failed here, and when each may be
		// taken again: every other erasure goes before them, and each
		// waits its poll interval (#28, #30).
		resting := map[string]time.Time{}
		for {
			wake := er.store.erasures.wake
			delay := er.opts.poll()
			id, err := er.store.nextErasure(ctx, slices.Collect(maps.Keys(resting)))
			if err == nil && id == "" {
				id, delay = due(resting, delay)
			}
			switch {
			case ctx.Err() != nil:
				return
			case err != nil:
				// Nothing was started: a request that wakes the
				// worker may find the database answering again (#29).
				logger().Error("could not read the next erasure", "err", err)
			case id != "":
				_, err := er.store.runErasure(ctx, er.writer, id, er.opts)
				if ctx.Err() != nil {
					return
				}
				if err == nil {
					delete(resting, id)
					continue
				}
				// Left running: a later start takes it again, and the
				// last one gives up (#12, #27). Not before the others,
				// nor at once: taken first each time, it would spend a
				// start on the same failure and hold every erasure
				// behind it (#28, #30).
				logger().Error("an erasure stopped before it ended", "erasure", id, "err", err)
				resting[id] = time.Now().Add(er.opts.poll())
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-time.After(delay):
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

// due is the resting erasure to take now, the one due first, or else how long
// until it is due, within the poll interval.
func due(resting map[string]time.Time, poll time.Duration) (string, time.Duration) {
	first := ""
	for id, at := range resting {
		if first == "" || at.Before(resting[first]) {
			first = id
		}
	}
	if first == "" {
		return "", poll
	}
	if wait := time.Until(resting[first]); wait > 0 {
		return "", min(wait, poll)
	}
	return first, poll
}

// nextErasure is the erasure the worker takes next: a running one — a stop
// interrupted it — before any queued, oldest first (#10, #12); none of those
// resting after a failed run (#30). The states are written out: the partial
// index is used only for a query that names its condition's literals.
func (s *Store) nextErasure(ctx context.Context, resting []string) (string, error) {
	args := make([]any, 0, len(resting))
	for _, id := range resting {
		args = append(args, id)
	}
	var id string
	err := s.db.QueryRowContext(ctx, nextErasureQuery(len(resting)), args...).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// nextErasureQuery is nextErasure's query with n resting erasures to pass.
func nextErasureQuery(n int) string {
	query := `SELECT id FROM erasures WHERE state IN ('queued', 'running')`
	if n > 0 {
		query += ` AND id NOT IN (?` + strings.Repeat(`, ?`, n-1) + `)`
	}
	return query + ` ORDER BY state = 'running' DESC, created_at LIMIT 1`
}

// submitErasureJob submits one of an erasure's jobs, waiting out a full queue
// (spec 044 #20 h) and retrying a job that failed with a database condition
// (spec 043 #2) with backoff, the two within one bound of two minutes (#16,
// #29): a full disk or a busy database passes, and nobody watches a
// background task to retry it by hand. A full queue is tried again sooner
// than a condition, which takes longer to clear.
func submitErasureJob(ctx context.Context, writer jobSubmitter, job WriteJob) error {
	busy, condition := 10*time.Millisecond, 100*time.Millisecond
	deadline := time.Now().Add(busyWait)
	for {
		err := writer.Submit(ctx, job)
		var delay time.Duration
		if errors.Is(err, ErrWriterBusy) {
			delay, busy = busy, min(2*busy, time.Second)
		} else if _, ok := Condition(err); ok {
			delay, condition = condition, min(2*condition, 10*time.Second)
		} else {
			return err
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
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

// The causes an erasure's record gives for a failure, a fixed list (#32): the
// record keeps what an operator acts on and nothing an error's text happened
// to carry, whatever form a user id took in it. The error itself goes to the
// server's log.
const (
	causeStopped  = "the server stopped"
	causeQueue    = "the write queue stayed full"
	causeFull     = "the disk is full"
	causeBusy     = "the database is busy"
	causeIO       = "the disk could not be read or written"
	causeMemory   = "the server ran out of memory"
	causeOpen     = "the database file could not be opened"
	causeRawBatch = "a raw batch could not be rewritten"
	causeTimeout  = "an operation took too long"
	causeOther    = "an unexpected error, which the server's log has"
)

// conditionCauses are the database's conditions (spec 043 #2) as causes.
var conditionCauses = map[string]string{
	"SQLITE_FULL":     causeFull,
	"SQLITE_BUSY":     causeBusy,
	"SQLITE_LOCKED":   causeBusy,
	"SQLITE_IOERR":    causeIO,
	"SQLITE_NOMEM":    causeMemory,
	"SQLITE_CANTOPEN": causeOpen,
}

// errRawBatch marks a scrub of a raw batch that failed after its retries.
var errRawBatch = errors.New("a raw batch could not be rewritten")

// failureCause is the cause the record gives for err, from the list above.
func failureCause(err error) string {
	name, condition := Condition(err)
	switch {
	case condition && conditionCauses[name] != "":
		return conditionCauses[name]
	case errors.Is(err, context.Canceled) || errors.Is(err, ErrWriterClosed) || errors.Is(err, errRunStopped):
		return causeStopped
	case errors.Is(err, ErrWriterBusy):
		return causeQueue
	case errors.Is(err, errRawBatch) || conflict(err):
		return causeRawBatch
	case errors.Is(err, context.DeadlineExceeded):
		return causeTimeout
	}
	return causeOther
}

// failureSentence is the error an erasure ends with: the phase that failed and
// its cause, never the error's own text (#9, #32).
func failureSentence(err error, phase string) string {
	return fmt.Sprintf("the %s phase failed: %s", phase, failureCause(err))
}

// loggable is an erasure's error as the server's log line gives it: whole,
// unless it names the user — as it is, or quoted the way a refusal quotes the
// echo it was given — which no log line does (spec 044 #15, #27 c).
func loggable(err error, userID string) string {
	text := err.Error()
	quoted := strconv.Quote(userID)
	if userID != "" && (strings.Contains(text, userID) || strings.Contains(text, quoted[1:len(quoted)-1])) {
		return "an error that named the user, which is not logged"
	}
	return text
}

// erasureError is a run's error on its way to the worker's log line, which
// does not name the user either.
type erasureError struct {
	err    error
	userID string
}

func (e *erasureError) Error() string { return loggable(e.err, e.userID) }
func (e *erasureError) Unwrap() error { return e.err }
