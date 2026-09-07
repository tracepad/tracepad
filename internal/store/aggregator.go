package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// The aggregator (spec 013 #3): a goroutine beside the sweeper that rolls
// closed hours into `stats_hourly` and re-rolls the hours late spans touched.
//
// It is deliberately not in the ingest transaction. There the rollup would
// have to be delta-maintained, and a delta is what spec 002 #22 refused for
// the trace aggregates: re-delivery is routine, and a delta double-counts
// every retried span. Recomputing a whole `(project, hour)` is idempotent by
// construction, and it keeps the write path's measured cost exactly where it
// was.

const (
	// DefaultRollupInterval is the cadence of a pass, and with it the lag
	// the docs publish: a closed hour is in the rollup within one of these.
	DefaultRollupInterval = 5 * time.Minute
	// maxHoursPerPass bounds one project's work so that a first pass over
	// a year of history cannot monopolise the writer. What is left is
	// picked up by the next pass, which is what makes the backfill
	// incremental rather than a stall (spec 013 #5).
	maxHoursPerPass = 500
)

// RollupOptions tunes the aggregator. Zero fields take defaults.
type RollupOptions struct {
	Interval time.Duration
	// Now is the clock a pass measures closed hours against; time.Now when
	// unset. Tests move it instead of waiting an hour.
	Now func() time.Time
}

// Aggregator rolls raw rows into the hourly statistics.
type Aggregator struct {
	store    *Store
	writer   jobSubmitter
	interval time.Duration
	now      func() time.Time

	stop   context.CancelFunc
	done   chan struct{}
	closed sync.Once

	mu      sync.Mutex
	lastRun time.Time
}

// NewAggregator builds the aggregator without starting it, on the same terms
// as the sweeper: whoever owns the writer's lifetime owns this one, and the
// tests drive passes by hand.
func (s *Store) NewAggregator(writer jobSubmitter, opts RollupOptions) *Aggregator {
	if opts.Interval <= 0 {
		opts.Interval = DefaultRollupInterval
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Aggregator{store: s, writer: writer, interval: opts.Interval, now: opts.Now}
}

// Start runs passes on the tick until Close. Like the sweeper, the first pass
// waits out one interval: a process that has just started has better things
// to do than a backfill, and the statistics answer live meanwhile.
func (a *Aggregator) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	a.stop = cancel
	a.done = make(chan struct{})
	go func() {
		defer close(a.done)
		ticker := time.NewTicker(a.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := a.Pass(ctx); err != nil && !errors.Is(err, context.Canceled) {
					logger().Error("stats rollup failed", "err", err)
				}
			}
		}
	}()
}

// Close stops the aggregator and waits for a pass in flight.
func (a *Aggregator) Close() error {
	a.closed.Do(func() {
		if a.stop != nil {
			a.stop()
			<-a.done
		}
	})
	return nil
}

// LastRun is when the last pass started, zero before the first one.
func (a *Aggregator) LastRun() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastRun
}

// Pass rolls every project once. A failure on one project is logged and the
// pass continues, for the reason the sweeper does the same: one project's
// broken state must not stop everyone else's statistics.
func (a *Aggregator) Pass(ctx context.Context) error {
	start := a.now()
	projects, err := a.store.ListProjects(false)
	if err != nil {
		return fmt.Errorf("rollup: list projects: %w", err)
	}

	var failures []error
	var rolled int
	for _, project := range projects {
		hours, err := a.rollProject(ctx, project, start)
		rolled += hours
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, ErrWriterClosed) {
				return err
			}
			failures = append(failures, fmt.Errorf("project %s: %w", project.Name, err))
		}
	}

	a.mu.Lock()
	a.lastRun = start
	a.mu.Unlock()

	// One line per pass, which with the row count on `/api/v1/system` is
	// what an operator needs to watch freshness and size (spec 013 #8).
	logger().Info("rolled up statistics",
		"projects", len(projects), "hours", rolled,
		"took", a.now().Sub(start).Round(time.Millisecond))
	return errors.Join(failures...)
}

// rollProject is one project's pass: correct what changed, roll forward what
// closed, then move the watermark. It reports how many hours it wrote.
func (a *Aggregator) rollProject(ctx context.Context, project *Project, at time.Time) (int, error) {
	state, err := a.store.RollupState(project.ID)
	if err != nil {
		return 0, err
	}

	// An hour is closed once it ended more than one tick ago, so the tail
	// the live scan owns is never rolled early (spec 013 #4).
	closed := HourOf(at.Add(-a.interval).UnixNano())

	var rolled int
	// Every user any hour of this pass touched, summarized once at the end
	// (spec 023 #3). Held here rather than recomputed by each hour's job
	// because a user active in n rolled hours would otherwise have their
	// whole history summed n times in one pass, which made a backfill
	// quadratic in the history it was walking.
	touched := map[string]bool{}
	// (1) and (2): the hours that changed under an already-rolled part of
	// the timeline. A pass that has never run has nothing behind it.
	if state.RolledUntil > 0 {
		dirty, err := a.store.dirtyHours(project.ID, state.LastPass, state.RolledUntil)
		if err != nil {
			return rolled, err
		}
		// Not bounded, unlike the forward roll. The bound there exists
		// because a first pass walks the whole history once and the
		// walk has no other end; here the set is exactly the hours that
		// changed since the last pass, and cutting it would lose those
		// corrections rather than defer them — `last_pass` moves on
		// whatever this loop does, so an hour dropped here never comes
		// back (found in review of PR #28, where the code did cut it and
		// this comment claimed the opposite). A long pass costs
		// background time; a dropped correction costs a wrong number for
		// ever, and the group commit keeps ingest from waiting on either.
		for _, hour := range dirty {
			// Whether a dirty hour is frozen is settled by the job,
			// inside its transaction (spec 013 #11); a pass that
			// pre-filtered here would be a second opinion about the
			// same rule.
			job, err := a.rollOne(ctx, project.ID, hour, at)
			if err != nil {
				return rolled, err
			}
			if !job.Frozen {
				rolled++
			}
			for _, id := range job.Touched {
				touched[id] = true
			}
		}
	}

	// (3): every closed hour that holds traces, from the watermark
	// forward. On a database nobody has rolled this starts at the oldest
	// trace, which is what makes the first pass the backfill (#5).
	from := state.RolledUntil
	if from == 0 {
		oldest, ok, err := a.store.oldestTraceHour(project.ID)
		if err != nil {
			return rolled, err
		}
		if !ok {
			// No traces at all. The watermark stays where it is —
			// see below for why moving it would be a lie — but the
			// rollup's own window still applies: a project whose
			// traces retention has taken is exactly one whose
			// history is left to age on its own (found in review of
			// PR #28, by a test written for the chunking).
			if err := a.advance(ctx, project.ID, state.RolledUntil, at); err != nil {
				return rolled, err
			}
			return rolled, a.sweepRollup(ctx, project, at)
		}
		from = oldest
	}

	hours, err := a.store.hoursWithTraces(project.ID, from, closed, maxHoursPerPass)
	if err != nil {
		return rolled, err
	}
	// The newest hour this pass examined, and whether it examined any.
	//
	// A frozen hour counts here even though it wrote nothing: freezing
	// requires stored rows (spec 013 #14), so the rollup does answer for
	// it and the watermark may stand past it. Skipping it would wedge the
	// watermark for ever — the next pass sees the same list, freezes the
	// same hour, and never moves (found in review of PR #28). The flag
	// exists because hour zero is a real hour: a trace no span of which
	// said when it started is stamped there deliberately (spec 004 #26),
	// and `newest > 0` would have read that as "nothing was rolled".
	var (
		newest   int64
		examined bool
	)
	for _, hour := range hours {
		job, err := a.rollOne(ctx, project.ID, hour, at)
		if err != nil {
			return rolled, err
		}
		if !job.Frozen {
			rolled++
		}
		for _, id := range job.Touched {
			touched[id] = true
		}
		newest, examined = hour, true
	}

	// The per-user summaries of everyone this pass touched, before the
	// watermark moves: a crash between the two leaves `last_pass` where it
	// was, so the next pass finds the same dirty hours and summarizes them
	// again (spec 023 #3).
	if err := a.summarize(ctx, project.ID, touched); err != nil {
		return rolled, err
	}

	// (4): the watermark, which moves to just past the newest hour this
	// pass actually rolled and no further.
	//
	// Advancing it to `closed` instead — over hours the pass never wrote a
	// row for — is a claim that the rollup can answer for them, and it is
	// false for any hour whose data arrives afterwards: the read seam would
	// ask the rollup, find nothing, and report zero where the live scan
	// would have reported the truth. That is not the interval's lag the
	// docs promise, it is a wrong answer, and it is what a first pass over
	// an empty database followed by an import of history does (found in the
	// live check of this PR — spec 013 #12).
	//
	// A pass that rolled nothing leaves the watermark alone; `last_pass`
	// still moves, so the dirty-hour window stays bounded.
	until := state.RolledUntil
	if examined {
		until = min(closed, newest+SecondsPerHour)
	}
	if err := a.advance(ctx, project.ID, until, at); err != nil {
		return rolled, err
	}

	// The rollup's own window, when the operator set one (spec 013 #6).
	if err := a.sweepRollup(ctx, project, at); err != nil {
		return rolled, err
	}
	return rolled, nil
}

// rollOne submits one hour and hands back the job, which is the only place
// that knows whether the hour was frozen rather than rolled — and which users
// it touched, for the summary the pass writes once at the end.
func (a *Aggregator) rollOne(ctx context.Context, projectID string, hour int64, at time.Time) (*statsRoll, error) {
	job := &statsRoll{ProjectID: projectID, Hour: hour, Now: at.UnixNano(), DeferSummary: true}
	return job, a.writer.Submit(ctx, job)
}

// summarizeChunk bounds one summary job, so that a backfill touching every
// user of a large project is many bounded transactions rather than one long
// one — the same rule every deletion in this store follows.
const summarizeChunk = 2000

// summarize recomputes the summaries of everyone the pass touched.
func (a *Aggregator) summarize(ctx context.Context, projectID string, touched map[string]bool) error {
	if len(touched) == 0 {
		return nil
	}
	ids := make([]string, 0, len(touched))
	for id := range touched {
		ids = append(ids, id)
	}
	// A stable order makes a failure reproducible and the chunks the same
	// chunks on a retry.
	slices.Sort(ids)
	for chunk := range slices.Chunk(ids, summarizeChunk) {
		if err := a.writer.Submit(ctx, &usersSummary{ProjectID: projectID, UserIDs: chunk}); err != nil {
			return err
		}
	}
	return nil
}

// frozenBefore is the hour at which an already-rolled hour stops being
// re-rollable (spec 013 #11). Past a project's trace-retention window the raw
// rows are incomplete by design — the sweep took them and deliberately left
// the rollup standing (#6) — so recomputing such an hour from what remains is
// not a correction but a demolition: one late fragment arriving for a swept
// hour would rewrite five thousand traces down to one. A project that keeps
// its traces forever has no such hours and re-rolls everything.
func frozenBefore(project *Project, nowNanos int64) int64 {
	if project.RetentionDays == nil {
		return 0
	}
	days := *project.RetentionDays
	if days < 1 || days > MaxRetentionDays {
		return 0
	}
	return HourOf(nowNanos - int64(days)*24*int64(time.Hour))
}

// commitMargin is how far back the pass dates its own cutoff. `updated_at` is
// stamped inside the write transaction, and the group commit lands after it —
// so a batch stamped just before this pass began can commit just after its
// dirty query has read, and would be invisible to this pass and to every one
// after it, since the cutoff had already moved past its stamp. Re-rolling an
// hour costs nothing and is idempotent; missing one is permanent, so the
// cutoff is deliberately conservative (found in review of PR #28).
const commitMargin = time.Second

func (a *Aggregator) advance(ctx context.Context, projectID string, until int64, at time.Time) error {
	return a.writer.Submit(ctx, &statsRollupAdvance{
		ProjectID:   projectID,
		RolledUntil: until,
		LastPass:    at.Add(-commitMargin).UnixNano(),
	})
}

// sweepRollup applies `stats_retention_days` when it is set. NULL is keep
// forever, which is the default and what every existing project takes: the
// aggregates are the cheap thing, and deleting them alongside the expensive
// thing they summarize would re-break the charts this spec exists to keep
// (spec 013 #6).
func (a *Aggregator) sweepRollup(ctx context.Context, project *Project, at time.Time) error {
	if project.StatsRetentionDays == nil {
		return nil
	}
	days := *project.StatsRetentionDays
	if days < 1 || days > MaxRetentionDays {
		return nil
	}
	cutoff := at.Add(-time.Duration(days) * 24 * time.Hour).Unix()
	// Chunk by chunk until a chunk comes back short, bounded per pass like
	// the trace sweeper's own loop. One chunk per pass would have cleared a
	// large backlog at a thousand rows every five minutes — days of it —
	// while the operator believed the window they set was in force (found
	// in review of PR #28).
	for range maxChunksPerProject {
		chunk := &statsRollupSweep{ProjectID: project.ID, Before: cutoff}
		if err := a.writer.Submit(ctx, chunk); err != nil {
			return err
		}
		if chunk.Deleted < int64(DefaultSweepChunk) {
			break
		}
	}
	// The per-user rows live under the same window (spec 023: no config of
	// their own), swept the same way. Their own loop rather than the same
	// chunk, because the two tables are of different sizes and one running
	// short says nothing about the other.
	for range maxChunksPerProject {
		chunk := &usersRollupSweep{ProjectID: project.ID, Before: cutoff}
		if err := a.writer.Submit(ctx, chunk); err != nil {
			return err
		}
		if chunk.Deleted < int64(DefaultSweepChunk) {
			return nil
		}
	}
	return nil
}

// dirtyHours are the already-rolled hours that changed since the last pass.
//
// The question is asked of `updated_at`, not of `ingested_at`: arrival is set
// once and never moves (spec 005 #1), so a span joining an existing trace —
// which is how the spans of one trace ordinarily arrive — would never make
// its hour dirty, and the hour would keep its first answer for ever. That is
// the defect this column exists to close (spec 013 #15, found in review of
// PR #28).
// A changed trace dirties **every hour one of its observations starts in**,
// not just the hour the trace sits in now.
//
// A trace's `timestamp` is derived — `COALESCE(MIN(start > 0), MIN(start))`,
// recomputed on every delivery — so it is always equal to the start of one of
// its own observations, and it moves as observations arrive. Backwards, when
// a late span starts earlier than any seen so far; and *forwards*, from the
// epoch, when the first delivery carried no usable start at all and a later
// one does (spec 004 #26). Either way the hour the trace left goes on
// counting it until that hour is rolled again.
//
// So the set to mark is the hours of the observations themselves, and it is
// bounded by the trace's own observation count rather than by the distance
// between them — which matters because a start time is unvalidated client
// input: walking an *hour range* between two of them let one span with a
// far-future start mark every hour of a year's history (spec 013 #16,
// corrected in review of PR #28).
//
// It is not quite complete, and the limit is named rather than implied: a
// re-delivered span overwrites `start_time` (spec 002 #5, the latest delivery
// is the truth), so if the same span id comes back with a start in a
// different hour, the hour it used to put the trace in is no longer derivable
// from anything stored, and that hour keeps counting the trace until
// something else dirties it. Recording each trace's rolled hour would close
// it, at the price of a write per trace on the ingest path for a shape that
// needs one span id re-sent with a moved start.
func (s *Store) dirtyHours(projectID string, since, before int64) ([]int64, error) {
	rows, err := s.db.Query(
		`SELECT DISTINCT (o.start_time / 1000000000 / ?) * ? AS hour
		 FROM observations o
		 JOIN traces t ON t.project_id = o.project_id AND t.id = o.trace_id
		 WHERE t.project_id = ? AND t.updated_at > ? AND o.start_time >= 0
		 ORDER BY hour`,
		SecondsPerHour, SecondsPerHour, projectID, since)
	if err != nil {
		return nil, fmt.Errorf("find the hours late spans touched: %w", err)
	}
	all, err := scanHours(rows)
	if err != nil {
		return nil, err
	}
	sessions, err := s.dirtySessionHours(projectID, since)
	if err != nil {
		return nil, err
	}
	all = append(all, sessions...)
	slices.Sort(all)
	all = slices.Compact(all)

	// Only the rolled part of the timeline: an hour at or past the
	// watermark is the live tail's, and the forward roll will take it when
	// it closes. The filter is on the hour rather than on the trace,
	// because a trace that moved into the tail still left a row behind.
	hours := make([]int64, 0, len(all))
	for _, hour := range all {
		if hour < before {
			hours = append(hours, hour)
		}
	}
	return hours, nil
}

// dirtySessionHours is the addition spec 023 #2 makes to the set above: a
// changed trace that carries a session id also dirties the hours the *other*
// traces of that session sit in.
//
// It exists because `sessions_started` in hour H depends on rows outside H. A
// session is counted in the hour its earliest trace of that user starts in, so
// a late trace starting before the earliest one seen so far moves the count out
// of H — and H, which nothing else touched, would go on reporting it. The set
// is bounded by the session's trace count, the way spec 013 #16's is by the
// observation count.
//
// *Known limit*, the same shape as #16's: a session whose start moved because
// one of its traces was re-delivered with an earlier start keeps its
// `sessions_started` in the old hour until that hour is dirtied by something
// else. The hour it used to be counted in is no longer derivable from anything
// stored.
func (s *Store) dirtySessionHours(projectID string, since int64) ([]int64, error) {
	rows, err := s.db.Query(dirtySessionHoursQuery,
		SecondsPerHour, SecondsPerHour, projectID, projectID, since)
	if err != nil {
		return nil, fmt.Errorf("find the hours a changed session touched: %w", err)
	}
	return scanHours(rows)
}

// dirtySessionHoursQuery is that statement, named so that a test can hand the
// shipped SQL to EXPLAIN QUERY PLAN (the method of spec 003 #25).
//
// The `+` before `other.timestamp` is load-bearing, and the reason is the one
// `sessionStartCondition` gives: without it the planner reads `timestamp >= 0`
// as an index term and drives the whole query off `idx_traces_timestamp`, which
// is a scan of every trace the project holds — on **every** pass, five minutes
// apart, however little changed. With it the only usable term is
// `session_id IN (…)`, which is the seek the design intends: one per changed
// session (found in review of PR #42).
//
// The guard itself stays because it is about correctness, not speed: this
// division truncates towards zero where `HourOf` floors, so a pre-epoch
// timestamp — a client's bytes gone wrong rather than a time — would name an
// hour the roll does not use. Such a trace is left to the live scan, exactly as
// `hoursWithTraces` leaves it.
const dirtySessionHoursQuery = `SELECT DISTINCT (other.timestamp / 1000000000 / ?) * ? AS hour
	 FROM traces other
	 WHERE other.project_id = ? AND +other.timestamp >= 0
	   AND other.session_id IN (
	         SELECT t.session_id FROM traces t
	          WHERE t.project_id = ? AND t.updated_at > ?
	            AND t.session_id IS NOT NULL)
	 ORDER BY hour`

// hoursWithTraces lists the hours of a half-open range that hold anything to
// roll, oldest first, at most limit of them.
func (s *Store) hoursWithTraces(projectID string, from, to int64, limit int) ([]int64, error) {
	if from >= to {
		return nil, nil
	}
	rows, err := s.db.Query(
		// The floor here is not only the range's: a timestamp before the
		// epoch would be truncated toward zero by this division while
		// `HourOf` floors, so the two would disagree about which hour a
		// row belongs to. Such a timestamp is a client's bytes gone
		// wrong rather than a time, and leaving it out of the rollup
		// leaves it to the live scan, which reads it correctly (found in
		// review of PR #28).
		`SELECT DISTINCT (timestamp / 1000000000 / ?) * ? AS hour
		 FROM traces
		 WHERE project_id = ? AND timestamp >= ? AND timestamp < ? AND timestamp >= 0
		 ORDER BY hour LIMIT ?`,
		SecondsPerHour, SecondsPerHour, projectID, from*1e9, to*1e9, limit)
	if err != nil {
		return nil, fmt.Errorf("find the hours to roll: %w", err)
	}
	return scanHours(rows)
}

// oldestTraceHour is where a backfill starts.
func (s *Store) oldestTraceHour(projectID string) (int64, bool, error) {
	var oldest sql.NullInt64
	if err := s.db.QueryRow(
		`SELECT MIN(timestamp) FROM traces WHERE project_id = ?`, projectID).
		Scan(&oldest); err != nil {
		return 0, false, fmt.Errorf("find the oldest trace: %w", err)
	}
	if !oldest.Valid {
		return 0, false, nil
	}
	return HourOf(oldest.Int64), true, nil
}

func scanHours(rows *sql.Rows) ([]int64, error) {
	defer rows.Close()
	var hours []int64
	for rows.Next() {
		var hour int64
		if err := rows.Scan(&hour); err != nil {
			return nil, err
		}
		hours = append(hours, hour)
	}
	return hours, rows.Err()
}

// statsRollupSweep deletes rolled rows older than the project's stats window.
// Bounded like every other deletion this store does, so a long-disabled
// window turned on does not hold the writer for one transaction.
type statsRollupSweep struct {
	ProjectID string
	Before    int64

	Deleted int64
}

func (s *statsRollupSweep) apply(tx *sql.Tx) error {
	result, err := tx.Exec(
		`DELETE FROM stats_hourly
		 WHERE rowid IN (SELECT rowid FROM stats_hourly
		                 WHERE project_id = ? AND hour < ? LIMIT ?)`,
		s.ProjectID, s.Before, DefaultSweepChunk)
	if err != nil {
		return fmt.Errorf("sweep the rollup: %w", err)
	}
	s.Deleted, err = result.RowsAffected()
	return err
}
