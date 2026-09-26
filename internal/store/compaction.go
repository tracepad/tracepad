package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Compaction (spec 044 #11). secure_delete zeroes what a delete frees, and
// two residues survive it: the search index keeps a deleted document's terms
// in its segments until they are merged, and the write-ahead log keeps the
// page images from before the delete until a checkpoint copies the zeroed
// pages back and truncates it. An explicit deletion — an erasure, a trace
// deletion, a project's purge — records a request; the next sweeper pass runs
// it in bounded steps, the way every background cost runs (spec 005 #14).
//
// The retention sweep does not ask for one: compacting after every hourly
// sweep would rewrite the whole index every hour, and the terms of swept
// traces leave with the index's ordinary merges.

// ftsMergePages is how much of the search index one merge step rewrites. A
// step is one writer job, so this is what bounds how long ingest waits behind
// it.
const ftsMergePages = 500

// checkpointBusyWait is how long the truncating checkpoint waits for readers
// before it gives up for this pass. It holds the one writer while it waits.
const checkpointBusyWait = 250 * time.Millisecond

// maxCompactionSteps bounds each phase of one pass, so that a compaction that
// somehow never finishes a phase cannot hold the pass for ever; the request
// stays pending and the next pass continues.
const maxCompactionSteps = 100_000

// CompactionState is what `GET /api/v1/system` reports: the latest pending
// request and the last completed compaction, Unix nanoseconds, zero for none.
type CompactionState struct {
	RequestedAt int64
	CompletedAt int64
}

// Compaction reads the deployment's compaction state.
func (s *Store) Compaction() (CompactionState, error) {
	var requested, completed sql.NullInt64
	err := s.db.QueryRow(`SELECT requested_at, completed_at FROM compaction WHERE id = 1`).
		Scan(&requested, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return CompactionState{}, nil
	}
	if err != nil {
		return CompactionState{}, fmt.Errorf("read compaction state: %w", err)
	}
	return CompactionState{RequestedAt: requested.Int64, CompletedAt: completed.Int64}, nil
}

// requestCompaction records that a deletion wants the next pass to compact,
// inside the deletion's own transaction. The stamp only moves forward: a
// compaction clears the request it started from and no later one, which is
// how a deletion committed while a compaction runs is not forgotten.
func requestCompaction(tx *sql.Tx) error {
	if _, err := tx.Exec(
		`UPDATE compaction SET requested_at = MAX(COALESCE(requested_at, 0), ?) WHERE id = 1`,
		time.Now().UnixNano()); err != nil {
		return fmt.Errorf("request a compaction: %w", err)
	}
	return nil
}

// ftsMerge is one bounded step of merging the search index. FTS5 says whether
// a merge did anything through the connection's change count: a delta of two
// or more is work done, less is a merge with nothing left to merge.
type ftsMerge struct {
	Pages  int
	Worked bool
}

func (m *ftsMerge) apply(tx *sql.Tx) error {
	m.Worked = false
	var before, after int64
	if err := tx.QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
		return err
	}
	// Negative: segments on any level are eligible, down to a single one
	// still carrying tombstones, which is what takes a deleted
	// document's terms out of the index.
	if _, err := tx.Exec(`INSERT INTO search_fts(search_fts, rank) VALUES ('merge', ?)`, -m.Pages); err != nil {
		return fmt.Errorf("merge the search index: %w", err)
	}
	if err := tx.QueryRow(`SELECT total_changes()`).Scan(&after); err != nil {
		return err
	}
	m.Worked = after-before >= 2
	return nil
}

// walCheckpoint copies the write-ahead log into the database and truncates it,
// run by the writer alone (see soloJob). Busy means a reader held a snapshot
// the checkpoint could not wait out: the log was not truncated, and the
// request stays pending for the next pass.
type walCheckpoint struct {
	BusyWait time.Duration
	Busy     bool
}

func (c *walCheckpoint) apply(*sql.Tx) error {
	return errors.New("a checkpoint runs alone, never inside a transaction")
}

func (c *walCheckpoint) runAlone(ctx context.Context, conn *sql.Conn) error {
	c.Busy = false
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, c.BusyWait.Milliseconds())); err != nil {
		return fmt.Errorf("checkpoint: set busy wait: %w", err)
	}
	// Back to the DSN's wait for every commit after this one.
	defer conn.ExecContext(ctx, `PRAGMA busy_timeout = 5000`)
	var busy, logFrames, checkpointed int64
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).
		Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint the write-ahead log: %w", err)
	}
	c.Busy = busy != 0
	return nil
}

// compactionDone stamps a finished compaction and clears the request it
// started from — only that one: a request made while it ran has a later
// stamp and stays pending for the next pass.
type compactionDone struct {
	Started int64 // the requested_at the compaction read before its first step
	At      int64
}

func (d *compactionDone) apply(tx *sql.Tx) error {
	_, err := tx.Exec(`UPDATE compaction
	    SET completed_at = ?,
	        requested_at = CASE WHEN requested_at = ? THEN NULL ELSE requested_at END
	  WHERE id = 1`, d.At, d.Started)
	return err
}

// compact runs a pending compaction: the index merged to what a merge can
// make of it, the freelist drained, the log checkpointed and truncated. It
// reports whether it completed; a checkpoint that stays busy leaves the
// request for the next pass.
func (sw *Sweeper) compact(ctx context.Context) (bool, error) {
	state, err := sw.store.Compaction()
	if err != nil || state.RequestedAt == 0 {
		return false, err
	}
	for range maxCompactionSteps {
		merge := &ftsMerge{Pages: ftsMergePages}
		if err := sw.writer.Submit(ctx, merge); err != nil {
			return false, err
		}
		if !merge.Worked {
			break
		}
	}
	for range maxCompactionSteps {
		var free int64
		if err := sw.store.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
			return false, fmt.Errorf("read the freelist: %w", err)
		}
		if free == 0 {
			break
		}
		if err := sw.writer.Submit(ctx, &incrementalVacuum{Pages: vacuumPages}); err != nil {
			return false, err
		}
	}
	checkpoint := &walCheckpoint{BusyWait: checkpointBusyWait}
	if err := sw.writer.Submit(ctx, checkpoint); err != nil {
		return false, err
	}
	if checkpoint.Busy {
		logger().Info("compaction waits for the next pass: a reader held the write-ahead log")
		return false, nil
	}
	done := &compactionDone{Started: state.RequestedAt, At: sw.now().UnixNano()}
	if err := sw.writer.Submit(ctx, done); err != nil {
		return false, err
	}
	return true, nil
}
