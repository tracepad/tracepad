package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// The write pipeline (spec 002 #15). SQLite has one writer; handlers decode,
// map and validate concurrently, then hand the result to this single
// goroutine, which folds a short window of submissions into one transaction.
// A handler is released only once that transaction has committed and been
// fsynced, so a 200 to an exporter means the spans are on disk.
//
// Every durable write goes through here, not just ingest (spec 003 #9): a
// 201 on a score has to mean the same thing a 200 on an export does, and
// serializing through one goroutine is also what makes prompt version
// numbering atomic without a lock of its own.

// Writer defaults. The window is short enough to stay invisible next to an
// exporter's own batching and long enough to amortize fsync across a burst.
const (
	DefaultCommitWindow = 50 * time.Millisecond
	DefaultMaxBatch     = 64
	DefaultQueueDepth   = 256
	// WindowRows is the other bound of a window (spec 043 #12): it stops
	// collecting once the rows of its jobs reach this many, so 64 big
	// ingest slices cannot become one transaction of 64,000 rows. A window
	// holds at most what it held before the job that crossed the line,
	// plus that job — under two slices.
	WindowRows = 1000
)

// ErrWriterBusy means the submission queue is full. Callers turn it into a
// 429 with Retry-After, which OTLP exporters retry natively (spec 002 #15).
var ErrWriterBusy = errors.New("writer is saturated")

// ErrWriterClosed means the server is shutting down.
var ErrWriterClosed = errors.New("writer is closed")

// WriteJob is one durable unit of work. An ingest batch (spec 002), a set of
// scores and a prompt write (spec 003) are all jobs, which is what lets one
// goroutine, one connection and one acknowledgement rule cover them all.
//
// Whether there is anything worth writing is settled before a job is
// submitted — an export with no spans answers 200 without one, an empty score
// array is a 400 — so the writer commits whatever it is handed.
type WriteJob interface {
	// apply issues the job's statements inside the writer's transaction.
	// It is unexported so that SQL stays inside this package, and it must
	// be idempotent: a window that fails is retried job by job (see
	// flush), so one submission can be applied more than once.
	apply(tx *sql.Tx) error
}

// weighted is a job that says how many rows it writes, which is what a window
// is bounded by beside its count of jobs (spec 043 #12). A job that does not
// say weighs one row.
type weighted interface {
	weight() int
}

// weightOf is a job's weight in its window.
func weightOf(job WriteJob) int {
	if w, ok := job.(weighted); ok {
		return max(1, w.weight())
	}
	return 1
}

// soloJob is a step the writer runs by itself, between commit windows and
// outside any transaction: a truncating WAL checkpoint (spec 044 #11), which
// cannot run inside a transaction and blocks every other writer while it
// does. Through the writer and not on a second connection, because a second
// connection doing it is the contention the one-writer rule exists to
// prevent (spec 003 #24). Its apply is never called.
type soloJob interface {
	WriteJob
	runAlone(ctx context.Context, conn *sql.Conn) error
}

// Rejection is a write refused for a reason the caller can fix — a prompt
// version whose type contradicts its name's earlier versions, a label moved
// onto a version that does not exist. Such checks read stored state, so they
// belong inside the write transaction rather than in a handler, where they
// would race (spec 003 Decision 20, 2026-08-27).
//
// The distinction pays for itself twice: the handler renders a rejection as
// 400 or 404 instead of 500, and the writer logs it as routine instead of as
// a storage failure.
type Rejection struct {
	Kind    string
	Message string
	// Details are extra fields the refusal's body carries beside `error`,
	// for a caller that has to *act* on the refusal rather than only show
	// it. The optimistic append of spec 021 #14 is the one so far: a `409`
	// says which version the name is actually at, so the editor can offer
	// to open it rather than making the reader go and look.
	//
	// Rendered in key order, so one refusal is one body however the map was
	// built.
	Details map[string]any
}

// Rejection kinds. The store does not know about HTTP; the handler maps these
// onto statuses.
const (
	// RejectInvalid is a malformed or contradictory request.
	RejectInvalid = "invalid"
	// RejectNotFound is a reference to something that does not exist.
	RejectNotFound = "not_found"
	// RejectConflict is a request the stored state already answers
	// differently: a project name that is taken, a deletion of something
	// already deleted (spec 005).
	RejectConflict = "conflict"
	// RejectForbidden is a credential the stored state no longer honours:
	// an upload URL whose key was revoked, or one an erasure voided
	// (spec 041 #28, #29).
	RejectForbidden = "forbidden"
	// RejectFull is a write refused for a bound the project has reached,
	// which a retry after a while may find room under (spec 041 #31).
	RejectFull = "full"
)

func (r *Rejection) Error() string { return r.Message }

// rejected reports an error the caller caused rather than a storage failure.
func rejected(err error) bool {
	var rejection *Rejection
	return errors.As(err, &rejection)
}

// WriterOptions tunes the group-commit writer. Zero fields take defaults.
type WriterOptions struct {
	CommitWindow time.Duration
	MaxBatch     int
	QueueDepth   int
	// Committed, when set, is called on the writer's goroutine after each
	// transaction commits, with the rows its jobs weighed (spec 043 #12).
	// A seam for tests — the one place that sees the transactions an
	// export was cut into — and nil in production.
	Committed func(rows int)
}

type submission struct {
	job  WriteJob
	done chan error
}

// Writer is the single writer goroutine.
type Writer struct {
	store  *Store
	conn   *sql.Conn
	queue  chan *submission
	window time.Duration
	max    int
	// committed is WriterOptions.Committed.
	committed func(rows int)

	mu     sync.RWMutex
	closed bool
	wg     sync.WaitGroup

	// beforeCommit is a test seam: the only way to hold the writer still
	// long enough to observe a full queue, since the writer otherwise
	// drains it faster than a test can fill it. Nil in production.
	beforeCommit func()
}

// NewWriter starts the writer goroutine. It holds one connection for its
// lifetime, which is what makes the fsync guarantee cheap to state: the
// connection runs with `synchronous=FULL` while readers keep the NORMAL of
// spec 001, so every committed write is durable without slowing anything else
// (spec 002 Decision 23, 2026-08-26).
func (s *Store) NewWriter(opts WriterOptions) (*Writer, error) {
	if opts.CommitWindow <= 0 {
		opts.CommitWindow = DefaultCommitWindow
	}
	if opts.MaxBatch <= 0 {
		opts.MaxBatch = DefaultMaxBatch
	}
	if opts.QueueDepth <= 0 {
		opts.QueueDepth = DefaultQueueDepth
	}

	conn, err := s.db.Conn(context.Background())
	if err != nil {
		return nil, fmt.Errorf("writer: reserve connection: %w", err)
	}
	if _, err := conn.ExecContext(context.Background(), `PRAGMA synchronous=FULL`); err != nil {
		conn.Close()
		return nil, fmt.Errorf("writer: set synchronous: %w", err)
	}

	w := &Writer{
		store:     s,
		conn:      conn,
		queue:     make(chan *submission, opts.QueueDepth),
		window:    opts.CommitWindow,
		max:       opts.MaxBatch,
		committed: opts.Committed,
	}
	w.wg.Add(1)
	go w.run()
	return w, nil
}

// QueueDepth reports how many submissions are waiting for the writer and how
// many it can hold. It is the one number that says whether writes are keeping
// up, which is why `GET /api/v1/system` publishes it (spec 004 #10).
func (w *Writer) QueueDepth() (waiting, capacity int) {
	return len(w.queue), cap(w.queue)
}

// Submit queues a job and blocks until it is committed. A full queue is
// reported immediately as ErrWriterBusy rather than waited on: backpressure
// an exporter can see beats a request that silently stalls.
//
// A job that reaches its transaction and is refused there comes back as a
// *Rejection; the handler renders it rather than retrying it.
func (w *Writer) Submit(ctx context.Context, job WriteJob) error {
	sub := &submission{job: job, done: make(chan error, 1)}

	w.mu.RLock()
	if w.closed {
		w.mu.RUnlock()
		return ErrWriterClosed
	}
	select {
	case w.queue <- sub:
	default:
		w.mu.RUnlock()
		return ErrWriterBusy
	}
	w.mu.RUnlock()

	select {
	case err := <-sub.done:
		return err
	case <-ctx.Done():
		// The writer still commits and still answers; done is buffered,
		// so abandoning it here leaks nothing.
		return ctx.Err()
	}
}

// Close stops the writer after the queued submissions have been committed.
func (w *Writer) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	// Safe under the write lock: Submit sends while holding the read
	// lock, so no send can be in flight when the channel closes.
	close(w.queue)
	w.mu.Unlock()

	w.wg.Wait()
	return w.conn.Close()
}

func (w *Writer) run() {
	defer w.wg.Done()

	pending := make([]*submission, 0, w.max)
	for {
		first, ok := <-w.queue
		if !ok {
			return
		}
		if w.runSolo(first) {
			continue
		}
		pending = append(pending[:0], first)
		rows := weightOf(first.job)

		// A solo step ends the window: what came before it commits first,
		// then it runs alone, in submission order. So does a window whose
		// jobs weigh WindowRows (spec 043 #12).
		var solo *submission
		timer := time.NewTimer(w.window)
		drained := false
	collect:
		for len(pending) < w.max && rows < WindowRows {
			select {
			case sub, ok := <-w.queue:
				if !ok {
					drained = true
					break collect
				}
				if _, isSolo := sub.job.(soloJob); isSolo {
					solo = sub
					break collect
				}
				pending = append(pending, sub)
				rows += weightOf(sub.job)
			case <-timer.C:
				break collect
			}
		}
		timer.Stop()

		w.flush(pending)
		if solo != nil {
			w.runSolo(solo)
		}
		if drained {
			return
		}
	}
}

// runSolo runs a solo step and answers it, reporting whether sub was one.
func (w *Writer) runSolo(sub *submission) bool {
	job, ok := sub.job.(soloJob)
	if !ok {
		return false
	}
	err := job.runAlone(context.Background(), w.conn)
	if err != nil {
		logFailure(err, slog.LevelError, "writer step failed")
	}
	sub.done <- err
	return true
}

// flush commits one window. If the window fails as a whole, each submission
// is retried alone: a job that violates a constraint — or that the write
// transaction refuses (a *Rejection) — must not take its neighbours down with
// it, and a genuine storage failure simply fails them all again, one by one.
func (w *Writer) flush(pending []*submission) {
	err := w.commit(pending)
	if err == nil {
		answer(pending, nil)
		return
	}
	if len(pending) == 1 {
		lost(pending[0], err)
		answer(pending, err)
		return
	}
	// A database condition is left to the retries below: a write they
	// commit is not lost, and one they cannot commit is logged as lost
	// (spec 043 #24). Anything else says why the window came apart.
	if _, condition := Condition(err); !condition {
		logFailure(err, slog.LevelWarn, "write window failed, retrying jobs individually",
			"jobs", len(pending))
	}
	for _, sub := range pending {
		one := []*submission{sub}
		if err := w.commit(one); err != nil {
			lost(sub, err)
			answer(one, err)
			continue
		}
		answer(one, nil)
	}
}

// reportsItsFailure is a job whose caller logs its failure itself, with what
// the writer does not know — the aggregator's hour names its project and its
// hour. The writer leaves it to that line, so a failure is one line (spec 043
// #24).
type reportsItsFailure interface {
	failureReported() bool
}

// lost logs one write that did not commit.
func lost(sub *submission, err error) {
	if job, ok := sub.job.(reportsItsFailure); ok && job.failureReported() {
		return
	}
	logFailure(err, slog.LevelError, "write commit failed")
}

// logFailure reports a failed commit, demoting a rejection: a caller asking
// for something the stored state does not allow is routine traffic, not an
// incident, and it is already being told so in the response.
//
// A database condition is logged once a minute per condition, with the number
// of writes it failed since the last line (spec 043 #2): it is the same news
// every time until it passes, and every caller has already been answered with
// a status that says to retry. It is only ever called for a write that was
// lost, so the line is an error and the count counts failures.
func logFailure(err error, level slog.Level, message string, args ...any) {
	if rejected(err) {
		level = slog.LevelInfo
	}
	if condition, ok := Condition(err); ok {
		failed, now := conditionLog.Allow(condition, time.Now())
		if !now {
			return
		}
		args = append(args, "condition", condition, "failed_since_last_line", failed.SameKey)
	}
	logger().Log(context.Background(), level, message, append([]any{"err", err}, args...)...)
}

func (w *Writer) commit(pending []*submission) error {
	if w.beforeCommit != nil {
		w.beforeCommit()
	}
	ctx := context.Background()
	tx, err := w.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin write transaction: %w", err)
	}
	defer tx.Rollback()

	for _, sub := range pending {
		if err := sub.job.apply(tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit write transaction: %w", err)
	}
	if w.committed != nil {
		rows := 0
		for _, sub := range pending {
			rows += weightOf(sub.job)
		}
		w.committed(rows)
	}
	return nil
}

func answer(pending []*submission, err error) {
	for _, sub := range pending {
		sub.done <- err
	}
}
