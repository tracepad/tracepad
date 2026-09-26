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

// maxDrainSteps bounds the free-page drain of one pass, a backstop behind the
// rule that stops it when a step frees nothing.
const maxDrainSteps = 10_000

// CompactionState is what `GET /api/v1/system` reports: the latest pending
// request and the last completed compaction, Unix nanoseconds, zero for none.
// PreparedFor is the request whose merge and drain are done, so that only the
// checkpoint is left of it.
type CompactionState struct {
	RequestedAt int64
	CompletedAt int64
	PreparedFor int64
}

// Compaction reads the deployment's compaction state.
func (s *Store) Compaction() (CompactionState, error) {
	var requested, completed, prepared sql.NullInt64
	err := s.db.QueryRow(`SELECT requested_at, completed_at, prepared_for FROM compaction WHERE id = 1`).
		Scan(&requested, &completed, &prepared)
	if errors.Is(err, sql.ErrNoRows) {
		return CompactionState{}, nil
	}
	if err != nil {
		return CompactionState{}, fmt.Errorf("read compaction state: %w", err)
	}
	return CompactionState{RequestedAt: requested.Int64, CompletedAt: completed.Int64, PreparedFor: prepared.Int64}, nil
}

// requestCompaction records that a deletion wants the next pass to compact,
// inside the deletion's own transaction, and returns the stamp it wrote. The
// stored stamp only moves forward: a compaction clears the request it started
// from and no later one, which is how a deletion committed while a compaction
// runs is not forgotten.
func requestCompaction(tx *sql.Tx) (int64, error) {
	now := time.Now().UnixNano()
	if _, err := tx.Exec(
		`UPDATE compaction SET requested_at = MAX(COALESCE(requested_at, 0), ?) WHERE id = 1`, now); err != nil {
		return 0, fmt.Errorf("request a compaction: %w", err)
	}
	return now, nil
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

func (c *walCheckpoint) runAlone(ctx context.Context, conn *sql.Conn) (err error) {
	c.Busy = false
	// The writer's own wait, whatever the DSN set it to, is put back after:
	// every commit after this one runs on this connection.
	var wait int64
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&wait); err != nil {
		return fmt.Errorf("checkpoint: read busy wait: %w", err)
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, c.BusyWait.Milliseconds())); err != nil {
		return fmt.Errorf("checkpoint: set busy wait: %w", err)
	}
	defer func() {
		if _, restore := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, wait)); restore != nil {
			err = errors.Join(err, fmt.Errorf("checkpoint: restore busy wait: %w", restore))
		}
	}()
	var busy, logFrames, checkpointed int64
	if err := conn.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).
		Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint the write-ahead log: %w", err)
	}
	c.Busy = busy != 0
	return nil
}

// compactionPrepared records that a request's merge and drain are done, so a
// pass that finds only its checkpoint left — a reader held the log last time —
// runs that alone rather than rewriting the index again.
type compactionPrepared struct{ For int64 }

func (p *compactionPrepared) apply(tx *sql.Tx) error {
	_, err := tx.Exec(`UPDATE compaction SET prepared_for = ? WHERE id = 1 AND requested_at = ?`, p.For, p.For)
	return err
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
	        requested_at = CASE WHEN requested_at = ? THEN NULL ELSE requested_at END,
	        prepared_for = NULL
	  WHERE id = 1`, d.At, d.Started)
	return err
}

// compact runs a pending compaction: the index merged, the freelist drained,
// the log checkpointed and truncated. It reports whether it completed and
// whether it drained the freelist; a checkpoint that stays busy leaves the
// request for the next pass, which then runs the checkpoint alone.
func (sw *Sweeper) compact(ctx context.Context) (done, drained bool, err error) {
	state, err := sw.store.Compaction()
	if err != nil || state.RequestedAt == 0 {
		return false, false, err
	}
	if state.PreparedFor != state.RequestedAt {
		if err := sw.mergeIndex(ctx); err != nil {
			return false, false, err
		}
		if err := sw.drainFreelist(ctx, true); err != nil {
			return false, false, err
		}
		drained = true
		if err := sw.writer.Submit(ctx, &compactionPrepared{For: state.RequestedAt}); err != nil {
			return false, drained, err
		}
	}
	checkpoint := &walCheckpoint{BusyWait: checkpointBusyWait}
	if err := sw.writer.Submit(ctx, checkpoint); err != nil {
		return false, drained, err
	}
	if checkpoint.Busy {
		logger().Info("compaction waits for the next pass: a reader held the write-ahead log")
		return false, drained, nil
	}
	if err := sw.writer.Submit(ctx, &compactionDone{Started: state.RequestedAt, At: sw.now().UnixNano()}); err != nil {
		return false, drained, err
	}
	return true, drained, nil
}

// mergeIndex merges the search index in bounded steps until a merge finds
// nothing to do — or until it has written twice the index's size as it stood
// when the phase began. Under live ingest every commit adds a segment, so
// "nothing to do" may never come; the budget is what makes the phase end
// once the segments that held the deleted text have been rewritten, rather
// than rewriting the whole index over and over.
func (sw *Sweeper) mergeIndex(ctx context.Context) error {
	var pages int
	if err := sw.store.db.QueryRowContext(ctx, `SELECT count(*) FROM search_fts_data`).Scan(&pages); err != nil {
		return fmt.Errorf("measure the search index: %w", err)
	}
	budget := 2*pages + ftsMergePages
	for written := 0; written < budget; written += ftsMergePages {
		merge := &ftsMerge{Pages: ftsMergePages}
		if err := sw.writer.Submit(ctx, merge); err != nil {
			return err
		}
		if sw.afterMergeStep != nil {
			sw.afterMergeStep()
		}
		if !merge.Worked {
			return nil
		}
	}
	return nil
}

// drainFreelist hands free pages back to the filesystem: until none are left
// when untilEmpty, one bounded job otherwise — the retention sweep's own
// reclaim. It stops when a job frees nothing, and does nothing on a file that
// is not in incremental auto-vacuum mode, where the pragma is a no-op and a
// loop waiting for the freelist to empty would never end.
func (sw *Sweeper) drainFreelist(ctx context.Context, untilEmpty bool) error {
	var mode int
	if err := sw.store.db.QueryRowContext(ctx, `PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		return fmt.Errorf("read auto_vacuum mode: %w", err)
	}
	if mode != incrementalVacuumMode {
		return nil
	}
	freelist := func() (int64, error) {
		var free int64
		err := sw.store.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free)
		return free, err
	}
	free, err := freelist()
	if err != nil {
		return fmt.Errorf("read the freelist: %w", err)
	}
	for range maxDrainSteps {
		if free == 0 {
			return nil
		}
		if err := sw.writer.Submit(ctx, &incrementalVacuum{Pages: vacuumPages}); err != nil {
			return err
		}
		if !untilEmpty {
			return nil
		}
		left, err := freelist()
		if err != nil {
			return fmt.Errorf("read the freelist: %w", err)
		}
		if left >= free {
			return nil
		}
		free = left
	}
	return nil
}
