package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
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
	// plus that job — under two slices. A job that weighs more than
	// WindowRows by itself joins no window: it commits alone (#35, and
	// see solitary).
	WindowRows = 1000
	// MaxItemsPerWrite is the most rows the API takes in one array of scores
	// or of dataset items (spec 043 #36): the server refuses a longer one,
	// and the CLI splits a longer file at it. The writer does not check it —
	// a job built any other way is bounded by whoever builds it.
	MaxItemsPerWrite = 10_000
)

// solitary is a job that commits alone, whatever it weighs: a delete whose
// rows nothing counts before it runs (spec 043 #35), and every background
// job (#37). It joins no window and collects none behind it, so it holds no
// other write's acknowledgement in its transaction.
type solitary interface {
	commitsAlone()
}

// classify is a job's weight in its window and whether it commits alone: a
// solitary job does, and so does one heavier than a window by its weight
// (spec 043 #35, #37).
func classify(job WriteJob) (weight int, lone bool) {
	weight = weightOf(job)
	_, marked := job.(solitary)
	return weight, marked || weight > WindowRows
}

// background is embedded by the jobs of the retention sweeper and of the
// maintenance passes that remove or recompute rows in bulk: a chunk of a
// sweep, a merge step, a vacuum, an hour's roll, a batch of user summaries.
// However its chunk is bounded, such a job changes more pages than a
// foreground write, so it commits alone (spec 043 #37).
type background struct{}

func (background) commitsAlone() {}

// errNilJob is a submission of nothing. Refused where it is made, so that no
// method of a job is ever called on a nil one by the writer's loop (spec 043 #42).
var errNilJob = errors.New("store: a nil write job")

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
	// be idempotent: a window is applied again when one of its jobs fails
	// by itself, each job in a savepoint, and one that fails as a whole — at
	// COMMIT, or on a database condition — is retried job by job (see
	// commit and flush), so one submission can be applied more than once.
	// A job that fails by itself is rolled back alone, and committed once
	// are the others.
	apply(tx *sql.Tx) error
}

// weighted is a job that says how many rows it writes, which is what a window
// is bounded by beside its count of jobs (spec 043 #12, #35). A job that does
// not say weighs one row.
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

// routineRefusal is a refusal a job returns as a plain error rather than a
// *Rejection, one the caller compares with errors.Is: a link used twice, a race
// for the first owner, a password typed wrong, a media body collected before
// the write. It is traffic, not an incident, like any rejection, and says so by
// its type rather than by a list someone has to keep (spec 043 #39).
// A pointer, so that each is its own sentinel to errors.Is, as errors.New's
// are, whatever its words.
type routineRefusal struct{ text string }

func (r *routineRefusal) Error() string { return r.text }

// rejected reports an error the caller caused rather than a storage failure.
func rejected(err error) bool {
	var rejection *Rejection
	var routine *routineRefusal
	return errors.As(err, &rejection) || errors.As(err, &routine)
}

// WriterOptions tunes the group-commit writer. Zero fields take defaults.
type WriterOptions struct {
	CommitWindow time.Duration
	MaxBatch     int
	QueueDepth   int
	// Committed, when set, is called on the writer's goroutine after each
	// transaction commits, with the rows the jobs it committed weighed
	// (spec 043 #12): a job refused inside the window is not counted, and
	// a job that commits alone weighs what its weight says — one, for a delete
	// of a whole history. A seam for tests — the one place that sees the
	// transactions an export was cut into — and nil in production.
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
	// closing ends when Close begins, and wakes every SubmitWaiting still
	// waiting for room: a stop does not wait for them.
	closing   chan struct{}
	closeOnce sync.Once

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
		closing:   make(chan struct{}),
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
	if isNilJob(job) {
		return errNilJob
	}
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

// SubmitWaiting is Submit for a job that continues work already admitted —
// an export's slice after its first (spec 043 #31): a full queue is waited out
// rather than refused, since the export's first slice was the one a full queue
// could turn away, and refusing a later one would leave the export half
// written for a retry that meets the same queue. How many wait is bounded by
// the body budget, each holding its body's reservation. A stop wakes them
// with ErrWriterClosed; ctx ends the wait, and once queued, the wait for the
// commit, as it does Submit's.
func (w *Writer) SubmitWaiting(ctx context.Context, job WriteJob) error {
	if isNilJob(job) {
		return errNilJob
	}
	sub := &submission{job: job, done: make(chan error, 1)}

	w.mu.RLock()
	if w.closed {
		w.mu.RUnlock()
		return ErrWriterClosed
	}
	select {
	case w.queue <- sub:
	case <-w.closing:
		w.mu.RUnlock()
		return ErrWriterClosed
	case <-ctx.Done():
		w.mu.RUnlock()
		return ctx.Err()
	}
	w.mu.RUnlock()

	select {
	case err := <-sub.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close stops the writer after the queued submissions have been committed.
func (w *Writer) Close() error {
	// First, and outside the lock: a SubmitWaiting blocked on a full queue
	// holds the read lock, and has to give it up before the lock is taken.
	w.closeOnce.Do(func() { close(w.closing) })
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
	// next is a job that commits alone, carried over from the window it
	// would have joined to start the next one.
	var next *submission
	for {
		first := next
		next = nil
		if first == nil {
			var ok bool
			if first, ok = <-w.queue; !ok {
				return
			}
		}
		if w.runSolo(first) {
			continue
		}
		pending = append(pending[:0], first)
		rows, lone := classify(first.job)

		// A solo step ends the window: what came before it commits first,
		// then it runs alone, in submission order. So does a window whose
		// jobs weigh WindowRows (spec 043 #12), and so does a job that
		// commits alone (#35, #37): behind others it starts the next window,
		// and starting one it collects nothing behind it.
		var solo *submission
		var timer *time.Timer
		var expired <-chan time.Time
		if !lone {
			timer = time.NewTimer(w.window)
			expired = timer.C
		}
		drained := false
	collect:
		for !lone && len(pending) < w.max && rows < WindowRows {
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
				weight, subLone := classify(sub.job)
				if subLone {
					next = sub
					break collect
				}
				pending = append(pending, sub)
				rows += weight
			case <-expired:
				break collect
			}
		}
		if timer != nil {
			timer.Stop()
		}

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
	// A step runs outside any window, so pass does not see it: a step of a
	// job that redacts its failure is wrapped here (spec 047 #36).
	err := failedAt(sub.job, job.runAlone(context.Background(), w.conn))
	if err != nil {
		logFailure(err, slog.LevelError, "writer step failed")
	}
	sub.done <- err
	return true
}

// flush commits one window. A job that fails inside it — a constraint it
// violates, a *Rejection — is rolled back alone and answered with its error,
// and the window commits the others (spec 043 #38). If the window fails as a
// whole — at BEGIN, at COMMIT, on a database condition, after which SQLite may
// have rolled the transaction back itself, or when a savepoint cannot be taken
// or rolled back to — each submission is retried alone: a genuine storage
// failure simply fails them all again, one by one, and a write the retries
// commit is not lost.
func (w *Writer) flush(pending []*submission) {
	refused, err := w.commit(pending)
	if err == nil {
		for i, sub := range pending {
			settle(sub, refused[i])
		}
		return
	}
	if len(pending) == 1 {
		lost(pending[0], err, "write commit failed")
		answer(pending, err)
		return
	}
	// A database condition is left to the retries below: a write they
	// commit is not lost, and one they cannot commit is logged as lost
	// (spec 043 #24). Anything else says why the window came apart — for a
	// window that holds a job whose words the writer does not log, its
	// facts and not its words (spec 047 #33, #36): one that commits alone
	// leaves this the only trace of why the window came apart.
	if _, condition := Condition(err); !condition {
		logFailure(err, slog.LevelWarn, "write window failed, retrying jobs individually",
			"jobs", len(pending))
	}
	for _, sub := range pending {
		one := []*submission{sub}
		refused, err := w.commit(one)
		if err != nil {
			lost(sub, err, "write commit failed")
			answer(one, err)
			continue
		}
		settle(sub, refused[0])
	}
}

// settle answers one job of a transaction that did not fail: nil if the job
// committed, its own error if it was refused — rolled back to its savepoint,
// or, alone, with its transaction.
func settle(sub *submission, refused error) {
	if refused != nil {
		lost(sub, refused, "write refused")
	}
	sub.done <- refused
}

// reportsItsFailure is a job whose caller logs its failure itself, with what
// the writer does not know — the aggregator's hour names its project and its
// hour. The writer leaves it to that line, so a failure is one line (spec 043
// #24).
type reportsItsFailure interface {
	failureReported() bool
}

// redactsItsFailure is a job whose error the writer does not put in its log:
// the error may quote what the job holds — an erasure's user (spec 047 #33) —
// so the writer gives the facts of the failure and not its text. It answers
// the id of what it is a step of, which the writer's line names.
type redactsItsFailure interface {
	failureRedacted() (id string, redacted bool)
}

func redacts(job WriteJob) (string, bool) {
	redacting, ok := job.(redactsItsFailure)
	if !ok {
		return "", false
	}
	return redacting.failureRedacted()
}

// jobFailure is what fails at a job that redacts its failure — its own error,
// or the window's met at it — or what fails a window that holds such jobs when
// no job in it is to blame (spec 047 #36). The writer wraps it in one place,
// pass, and from there it says only its facts, to a log line and to anything
// that prints it: its LogValue is the erasure, the cause, the types and the code
// (failureFacts), and its Error a sentence of the erasure and the cause, or of a
// refusal and its kind. The error itself is reached by errors.Is and errors.As,
// which every classifier of a failure uses, never by its text. A database
// condition says its name alone and not the erasure: the writer's paced line
// counts the condition's failures of every job (spec 043 #2).
//
// It names erasures because the jobs of an erasure's task are the only ones
// that redact their failure (spec 047 #33); a second kind of such job would
// rename it to what the two have in common.
type jobFailure struct {
	erasures []string
	err      error
}

// named is the erasure, or the erasures, the failure is a step of, as a key and
// a value a search for an id finds: a comma, not a space, which a log line
// would quote.
func (f *jobFailure) named() (key, value string) {
	if len(f.erasures) == 1 {
		return "erasure", f.erasures[0]
	}
	return "erasures", strings.Join(f.erasures, ",")
}

func (f *jobFailure) Error() string {
	key, value := f.named()
	if rejected(f.err) {
		// A refusal, and its kind when it has one: the causes are the
		// record's list (spec 047 #32), which has none for one.
		var refusal *Rejection
		if errors.As(f.err, &refusal) {
			return fmt.Sprintf("a write of %s %s was rejected (%s)", key, value, refusal.Kind)
		}
		return fmt.Sprintf("a write of %s %s was rejected", key, value)
	}
	return fmt.Sprintf("a write of %s %s failed: %s", key, value, failureCause(f.err))
}

func (f *jobFailure) Unwrap() error { return f.err }

func (f *jobFailure) LogValue() slog.Value {
	if condition, ok := Condition(f.err); ok {
		return slog.StringValue(condition)
	}
	key, value := f.named()
	return slog.Group("", append([]any{key, value}, failureFacts(f.err)...)...).Value
}

// failedAt is err as the writer answers and logs it when it failed at job: a
// jobFailure for a job that redacts its failure, err itself for any other.
func failedAt(job WriteJob, err error) error {
	if err == nil {
		return nil
	}
	if id, redacted := redacts(job); redacted {
		return &jobFailure{erasures: []string{id}, err: err}
	}
	return err
}

// failedByWindow is err as the writer answers and logs it when it failed a
// window and no job in it — a BEGIN or a COMMIT (spec 047 #36): wrapped for
// every erasure the window holds a job of, since any of them may be the one it
// failed; a window that holds none is its own error. The retries that follow
// say which.
func failedByWindow(pending []*submission, err error) error {
	var erasures []string
	for _, sub := range pending {
		if id, redacted := redacts(sub.job); redacted && !slices.Contains(erasures, id) {
			erasures = append(erasures, id)
		}
	}
	if len(erasures) == 0 {
		return err
	}
	return &jobFailure{erasures: erasures, err: err}
}

// lost logs one write that did not commit: "write commit failed" for one whose
// transaction failed, "write refused" for one that failed by itself (spec 043
// #37). A job that logs its own failures (the aggregator's hour) is left to do
// so. One that redacts its failure says "a write of an erasure did not commit"
// with its facts, so that a failure whose submitter has gone is still one the
// log has; a database condition it met counts in the paced line, which gives
// the condition and not the job, since a full disk is news whoever met it.
func lost(sub *submission, err error, message string) {
	if _, redacted := asJobFailure(err); redacted {
		if _, condition := Condition(err); !condition {
			message = "a write of an erasure did not commit"
		}
	} else if reports(sub.job) {
		return
	}
	logFailure(err, slog.LevelError, message)
}

func reports(job WriteJob) bool {
	reporter, ok := job.(reportsItsFailure)
	return ok && reporter.failureReported()
}

// logFailure is the writer's line for a failure — a write lost or refused, a
// window that came apart, a step, a job that failed a window's first pass and
// was committed by its second — at level, demoting a rejection: a caller asking
// for something the stored state does not allow is routine traffic, not an
// incident, and it is already being told so in the response.
//
// A database condition is logged once a minute per condition, with the number
// of writes it failed since the last line (spec 043 #2): it is the same news
// every time until it passes, and every caller has already been answered with
// a status that says to retry. Only a lost write or a failed step reaches here
// with one — a window's condition is left to its retries, and a replay's first
// failure was the job's own — so the count counts failures.
func logFailure(err error, level slog.Level, message string, args ...any) {
	level = levelFor(err, level)
	if condition, ok := Condition(err); ok {
		failed, now := conditionLog.Allow(condition, time.Now())
		if !now {
			return
		}
		args = append(args, "condition", condition, "failed_since_last_line", failed.SameKey)
	}
	logger().Log(context.Background(), level, message, append([]any{"err", logged(err)}, args...)...)
}

// levelFor is the level of a line about err: Info for a refusal, as a caller
// asking for something the stored state does not allow is routine traffic
// (spec 043 #37, #39), and level for anything else.
func levelFor(err error, level slog.Level) slog.Level {
	if rejected(err) {
		return slog.LevelInfo
	}
	return level
}

// logged is err as a log line gives it: the facts of a jobFailure anywhere in
// its chain — a wrap above one would print the wrap's text, not the failure's
// value (spec 047 #36) — and err itself otherwise. The facts stand for the
// whole of err: the wrap's own words and the errors joined beside it are text
// the writer cannot vouch for, and a line about an erasure's job gives none
// (#33). The writer's own lines hand it a jobFailure unwrapped, so the search
// is a guard, for a line that is handed one a caller wrapped.
func logged(err error) any {
	if redacted, ok := asJobFailure(err); ok {
		return redacted
	}
	return err
}

// asJobFailure is the jobFailure in err's chain, if there is one.
func asJobFailure(err error) (*jobFailure, bool) {
	var redacted *jobFailure
	ok := errors.As(err, &redacted)
	return redacted, ok
}

// commit applies a window in one transaction and commits it. A window of two
// jobs or more is applied plainly first, as it always was: when no job fails,
// it costs what it cost before savepoints, whatever the jobs are. When one
// fails by itself, the transaction is rolled back and the window applied again
// with each job in a savepoint, the failing one rolled back to its own and the
// others committed. That second pass is the last: what fails the window as a
// whole there — a condition, a savepoint that cannot be rolled back to — comes
// back as the window's error, and flush retries its jobs one by one. refused
// holds, at its index, the error of a job that failed by itself, nil for every
// job that committed; a window in which every job is refused commits nothing
// (spec 043 #38).
func (w *Writer) commit(pending []*submission) (refused []error, err error) {
	if w.beforeCommit != nil {
		w.beforeCommit()
	}
	if len(pending) == 1 {
		return w.pass(pending, false)
	}
	first, err := w.pass(pending, false)
	if !errors.Is(err, errJobFailed) {
		return first, err
	}
	refused, err = w.pass(pending, true)
	if err == nil {
		for i, failed := range first {
			if failed != nil && refused[i] == nil {
				explainReplay(pending[i], failed)
			}
		}
	}
	return refused, err
}

// pass applies a window once and gives what failed in it as the writer answers
// and logs it — where the writer wraps a failure, with runSolo for a step that
// runs outside any window (spec 047 #36): each
// refusal by the job it refused, the window's failure by the job it was met at,
// and one that no job met, a BEGIN or a COMMIT, by every erasure the window
// holds a job of.
func (w *Writer) pass(pending []*submission, savepoints bool) ([]error, error) {
	refused, at, err := w.commitWindow(pending, savepoints)
	for i, own := range refused {
		refused[i] = failedAt(pending[i].job, own)
	}
	switch {
	case err == nil || errors.Is(err, errJobFailed):
	case at >= 0:
		err = failedAt(pending[at].job, err)
	default:
		err = failedByWindow(pending, err)
	}
	return refused, err
}

// errJobFailed says a job of a window applied plainly failed by itself: the
// window is to be applied again with each job in a savepoint. The job's own
// error comes with it, in refused, at the job's index.
var errJobFailed = errors.New("a job of the window failed by itself")

// explainReplay says why a window was applied twice when it did not show:
// a job failed the first pass and was committed by the second, which leaves no
// refusal to log. It names the job's type and gives its error — for a job that
// redacts its failure, the facts of it and what the job was a step of (spec 047
// #33) — unless the job reports its failure itself: that line has the type
// alone, since its error may name a user (spec 044 #15). A refusal is Info, as
// every refusal the writer logs is (spec 043 #39).
func explainReplay(sub *submission, failed error) {
	const message = "a job failed its window's first pass and passed the second"
	job := fmt.Sprintf("%T", sub.job)
	if _, redacted := asJobFailure(failed); !redacted && reports(sub.job) {
		logger().Log(context.Background(), levelFor(failed, slog.LevelWarn), message, "job", job)
		return
	}
	logFailure(failed, slog.LevelWarn, message, "job", job)
}

// commitWindow applies the window in one transaction and commits what was not
// refused, each job of a window of many in a savepoint if savepoints is set. A
// window of one job takes none: its failure discards the transaction, which
// holds nothing else. Without savepoints a job that fails by itself stops the
// pass, and errJobFailed comes back with its error in refused. The other
// returned error is what fails the window as a whole: BEGIN, COMMIT, a
// savepoint that cannot be taken or rolled back to, and a database condition
// in any apply; at is the index of the job it was met at, -1 for BEGIN and
// COMMIT, which fail no job in particular.
func (w *Writer) commitWindow(pending []*submission, savepoints bool) (refused []error, at int, err error) {
	tx, err := w.conn.BeginTx(context.Background(), nil)
	if err != nil {
		return nil, -1, fmt.Errorf("begin write transaction: %w", err)
	}
	defer tx.Rollback()

	many := len(pending) > 1
	refused = make([]error, len(pending))
	written := false
	for i, sub := range pending {
		own, err := applyOne(tx, sub.job, many && savepoints)
		if err != nil {
			return nil, i, err
		}
		if own == nil {
			written = true
			continue
		}
		refused[i] = own
		if many && !savepoints {
			return refused, -1, errJobFailed
		}
	}
	if !written {
		return refused, -1, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, -1, fmt.Errorf("commit write transaction: %w", err)
	}
	if w.committed != nil {
		rows := 0
		for i, sub := range pending {
			if refused[i] == nil {
				rows += weightOf(sub.job)
			}
		}
		w.committed(rows)
	}
	return refused, -1, nil
}

// applyOne applies one job, inside a savepoint if it is to be rolled back
// alone. own is the job's own failure, the job rolled back to its savepoint, or
// left to the caller to discard with its transaction; windowErr fails the
// window as a whole. A database condition is the window's: SQLite may already
// have rolled the whole transaction back for one, and there is no savepoint
// left to return to.
func applyOne(tx *sql.Tx, job WriteJob, savepoint bool) (own, windowErr error) {
	if savepoint {
		if _, err := tx.Exec(`SAVEPOINT job`); err != nil {
			return nil, fmt.Errorf("open a job's savepoint: %w", err)
		}
	}
	// A panic in the job is that job failing by itself (spec 043 #42): the
	// window is applied again with savepoints and the job is refused alone.
	err := guardJob("write job", job, func() error { return job.apply(tx) })
	if err != nil {
		if _, condition := Condition(err); condition {
			return nil, err
		}
		if savepoint {
			// The job's own error stays out of the window's: the window's is
			// logged, and a job that reports its failure itself — one whose
			// error names a user (spec 044 #15) — is logged by its caller alone.
			if _, rollback := tx.Exec(`ROLLBACK TO job`); rollback != nil {
				return nil, fmt.Errorf("roll back to a job's savepoint: %w", rollback)
			}
		}
	}
	if savepoint {
		if _, release := tx.Exec(`RELEASE job`); release != nil {
			return nil, fmt.Errorf("release a job's savepoint: %w", release)
		}
	}
	return err, nil
}

func answer(pending []*submission, err error) {
	for _, sub := range pending {
		sub.done <- err
	}
}
