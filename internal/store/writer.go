package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// The write pipeline (spec 002 #15). SQLite has one writer; handlers decode
// and map concurrently, then hand the result to this single goroutine, which
// folds a short window of submissions into one transaction. A handler is
// released only once that transaction has committed and been fsynced, so a
// 200 to an exporter means the spans are on disk.

// Writer defaults. The window is short enough to stay invisible next to an
// exporter's own batching and long enough to amortize fsync across a burst.
const (
	DefaultCommitWindow = 50 * time.Millisecond
	DefaultMaxBatch     = 64
	DefaultQueueDepth   = 256
)

// ErrWriterBusy means the submission queue is full. Callers turn it into a
// 429 with Retry-After, which OTLP exporters retry natively (spec 002 #15).
var ErrWriterBusy = errors.New("ingest writer is saturated")

// ErrWriterClosed means the server is shutting down.
var ErrWriterClosed = errors.New("ingest writer is closed")

// WriterOptions tunes the group-commit writer. Zero fields take defaults.
type WriterOptions struct {
	CommitWindow time.Duration
	MaxBatch     int
	QueueDepth   int
}

type submission struct {
	batch *IngestBatch
	done  chan error
}

// Writer is the single ingest writer goroutine.
type Writer struct {
	store  *Store
	conn   *sql.Conn
	queue  chan *submission
	window time.Duration
	max    int

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
// spec 001, so every ingest commit is durable without slowing anything else
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
		return nil, fmt.Errorf("ingest writer: reserve connection: %w", err)
	}
	if _, err := conn.ExecContext(context.Background(), `PRAGMA synchronous=FULL`); err != nil {
		conn.Close()
		return nil, fmt.Errorf("ingest writer: set synchronous: %w", err)
	}

	w := &Writer{
		store:  s,
		conn:   conn,
		queue:  make(chan *submission, opts.QueueDepth),
		window: opts.CommitWindow,
		max:    opts.MaxBatch,
	}
	w.wg.Add(1)
	go w.run()
	return w, nil
}

// Submit queues a batch and blocks until it is committed. A full queue is
// reported immediately as ErrWriterBusy rather than waited on: backpressure
// an exporter can see beats a request that silently stalls.
func (w *Writer) Submit(ctx context.Context, batch *IngestBatch) error {
	if batch.Empty() {
		return nil
	}
	sub := &submission{batch: batch, done: make(chan error, 1)}

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
		pending = append(pending[:0], first)

		timer := time.NewTimer(w.window)
		drained := false
	collect:
		for len(pending) < w.max {
			select {
			case sub, ok := <-w.queue:
				if !ok {
					drained = true
					break collect
				}
				pending = append(pending, sub)
			case <-timer.C:
				break collect
			}
		}
		timer.Stop()

		w.flush(pending)
		if drained {
			return
		}
	}
}

// flush commits one window. If the window fails as a whole, each submission
// is retried alone: a batch that violates a constraint must not take its
// neighbours down with it, and a genuine storage failure simply fails them
// all again, one by one.
func (w *Writer) flush(pending []*submission) {
	err := w.commit(pending)
	if err == nil {
		answer(pending, nil)
		return
	}
	if len(pending) == 1 {
		logger().Error("ingest commit failed", "err", err, "submissions", 1)
		answer(pending, err)
		return
	}
	logger().Warn("ingest window failed, retrying submissions individually",
		"err", err, "submissions", len(pending))
	for _, sub := range pending {
		one := []*submission{sub}
		if err := w.commit(one); err != nil {
			logger().Error("ingest commit failed", "err", err, "submissions", 1)
			answer(one, err)
			continue
		}
		answer(one, nil)
	}
}

func (w *Writer) commit(pending []*submission) error {
	if w.beforeCommit != nil {
		w.beforeCommit()
	}
	ctx := context.Background()
	tx, err := w.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin ingest transaction: %w", err)
	}
	defer tx.Rollback()

	for _, sub := range pending {
		if err := sub.batch.apply(tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit ingest transaction: %w", err)
	}
	return nil
}

func answer(pending []*submission, err error) {
	for _, sub := range pending {
		sub.done <- err
	}
}
