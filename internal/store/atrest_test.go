package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
)

// Data at rest (spec 044 #10–#13): what a deletion leaves in the file, and who
// can read the file.

// request records a compaction request the way a deletion does, in a
// transaction of its own, on the clock given (zero is the wall clock), and
// returns the stamp stored.
func request(t *testing.T, s *Store, now int64) int64 {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := requestCompaction(tx, now)
	if err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return stamp
}

// marker is a random lowercase word nothing else in a test database contains.
func marker(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		raw[i] = 'a' + raw[i]%26
	}
	return "zq" + string(raw)
}

// filesHolding names which of the database's files contain needle anywhere in
// their bytes: the database, its write-ahead log and its shared-memory index.
func filesHolding(t *testing.T, path, needle string) []string {
	t.Helper()
	var holding []string
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		body, err := os.ReadFile(file)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(body, []byte(needle)) {
			holding = append(holding, filepath.Base(file))
		}
	}
	return holding
}

// checkpoint runs the writer's truncating checkpoint and fails the test if a
// reader kept it from finishing.
func checkpoint(t *testing.T, writer *Writer) {
	t.Helper()
	step := &walCheckpoint{BusyWait: time.Second}
	if err := writer.Submit(t.Context(), step); err != nil {
		t.Fatal(err)
	}
	if step.Busy {
		t.Fatal("the checkpoint was busy with nothing else open")
	}
}

// secure_delete alone (#10), with no vacuum and no compaction: a deleted row's
// bytes are gone from the file once the log is checkpointed — the small row's
// cell inside a live page, and the large row's overflow pages on the freelist,
// which FAST would leave as they were.
func TestSecureDeleteZeroesWhatADeleteFrees(t *testing.T) {
	f := newSweepFixture(t)
	word := marker(t)
	large := strings.Repeat(word+" ", 16*1024/len(word))

	submit := func(job func(*sql.Tx) error) {
		t.Helper()
		if err := f.writer.Submit(t.Context(), writeJob(job)); err != nil {
			t.Fatal(err)
		}
	}
	submit(func(tx *sql.Tx) error {
		_, err := tx.Exec(`CREATE TABLE probe (id INTEGER PRIMARY KEY, body BLOB)`)
		return err
	})
	submit(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO probe (id, body) VALUES (1, ?), (2, ?), (3, 'kept')`,
			[]byte(large), []byte("small "+word))
		return err
	})
	checkpoint(t, f.writer)
	if held := filesHolding(t, f.store.path, word); len(held) == 0 {
		t.Fatal("control: the rows are not in the file before the delete, so the scan proves nothing")
	}

	submit(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM probe WHERE id IN (1, 2)`)
		return err
	})
	checkpoint(t, f.writer)
	if held := filesHolding(t, f.store.path, word); len(held) != 0 {
		t.Errorf("a deleted row's bytes are still in %v after the checkpoint", held)
	}
}

// The compaction an explicit deletion asks for (#11): after one pass the
// deleted text is in neither the search index's segments nor the write-ahead
// log, and the request is stamped done.
func TestCompactionTakesADeletedTraceOutOfTheFile(t *testing.T) {
	f := newSweepFixture(t)
	// The migration asks for one compaction of whatever came before it;
	// that one is not this test's.
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if state, err := f.store.Compaction(t.Context()); err != nil || state.RequestedAt != 0 {
		t.Fatalf("after the first pass: %+v, %v; want nothing pending", state, err)
	}

	word := marker(t)
	traceID := strings.Repeat("ab", 16)
	batch := &IngestBatch{
		ProjectID:  f.project.ID,
		IngestedAt: daysAgo(1),
		Traces:     []*model.Trace{{ID: traceID, Name: "chat " + word}},
		Observations: []*model.Observation{{
			TraceID: traceID, ID: traceID[:16], Type: model.TypeSpan, Level: model.LevelDefault,
			StartTime: daysAgo(1), EndTime: daysAgo(1) + 1_000_000,
			Input: map[string]any{"prompt": "tell " + word},
		}},
	}
	if err := f.writer.Submit(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	// FTS5 stores a term after the prefix it shares with the one before
	// it, so the scan looks for the part no neighbour can share.
	tail := word[4:]
	if held := filesHolding(t, f.store.path, tail); len(held) == 0 {
		t.Fatal("control: the trace's text is not in the files, so the scan proves nothing")
	}

	del := &TraceDelete{ProjectID: f.project.ID, IDs: []string{traceID}, Confirm: traceID}
	if err := f.writer.Submit(t.Context(), del); err != nil {
		t.Fatal(err)
	}
	state, err := f.store.Compaction(t.Context())
	if err != nil || state.RequestedAt == 0 {
		t.Fatalf("after a deletion: %+v, %v; want a pending request", state, err)
	}

	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if held := filesHolding(t, f.store.path, tail); len(held) != 0 {
		t.Errorf("the deleted trace's text is still in %v after the compaction", held)
	}
	state, err = f.store.Compaction(t.Context())
	if err != nil || state.RequestedAt != 0 || state.CompletedAt != sweepNow.UnixNano() {
		t.Errorf("after the pass: %+v, %v; want the request cleared and completed at the pass", state, err)
	}
}

// The retention sweep deletes on its own schedule and asks for nothing: a
// compaction after every hourly pass would rewrite the index every hour.
func TestTheRetentionSweepDoesNotAskForACompaction(t *testing.T) {
	f := newSweepFixture(t)
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.arrive(t, f.project.ID, strings.Repeat("cd", 16), daysAgo(40))
	f.setRetention(t, f.project.ID, intPtr(30), nil)
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.store.db.QueryRow(`SELECT count(*) FROM traces`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the sweep left %d traces (%v)", n, err)
	}
	if state, _ := f.store.Compaction(t.Context()); state.RequestedAt != 0 {
		t.Errorf("the retention sweep requested a compaction: %+v", state)
	}
}

// A reader holding a snapshot keeps the truncating checkpoint from finishing;
// the pass gives up after its short wait and leaves the request for the next.
func TestABusyCheckpointLeavesTheRequestPending(t *testing.T) {
	f := newSweepFixture(t)
	request(t, f.store, 0)
	// An open statement holds a read snapshot. Not a transaction: every
	// BEGIN here takes the write lock (`_txlock=immediate`).
	reader, err := f.store.db.Query(`SELECT id FROM projects`)
	if err != nil {
		t.Fatal(err)
	}
	if !reader.Next() {
		t.Fatal("no project to read")
	}
	// Something for the log to hold that the reader's snapshot predates.
	f.arrive(t, f.project.ID, strings.Repeat("ef", 16), daysAgo(1))

	done, _, err := f.sweeper.compact(t.Context())
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("the compaction reported done while a reader held the log")
	}
	state, _ := f.store.Compaction(t.Context())
	if state.RequestedAt == 0 {
		t.Fatal("the request was cleared although the checkpoint did not finish")
	}
	if state.PreparedFor != state.RequestedAt {
		t.Fatalf("state = %+v: the merge and drain that did finish are not recorded", state)
	}

	// The next pass runs the checkpoint alone: the index is not merged,
	// nor the freelist drained, a second time for the same request.
	merges := 0
	f.sweeper.afterMergeStep = func() { merges++ }
	done, drained, err := f.sweeper.compact(t.Context())
	if err != nil || !done || drained || merges != 0 {
		t.Fatalf("the next attempt: done = %v, drained = %v, merges = %d, err = %v; want only the checkpoint",
			done, drained, merges, err)
	}
}

// Under live ingest every commit adds a segment, so "a merge found nothing to
// do" may never come. The phase is bounded by the index as it stood when it
// began: it ends, and the deleted text is gone all the same.
func TestTheIndexMergeEndsUnderLiveIngest(t *testing.T) {
	f := newSweepFixture(t)
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	word := marker(t)
	doomed := strings.Repeat("ab", 16)
	if err := f.writer.Submit(t.Context(), &IngestBatch{
		ProjectID: f.project.ID, IngestedAt: daysAgo(1),
		Traces: []*model.Trace{{ID: doomed, Name: "chat " + word}},
		Observations: []*model.Observation{{TraceID: doomed, ID: doomed[:16], Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: daysAgo(1), EndTime: daysAgo(1) + 1_000_000}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.writer.Submit(t.Context(), &TraceDelete{ProjectID: f.project.ID, IDs: []string{doomed}, Confirm: doomed}); err != nil {
		t.Fatal(err)
	}

	steps := 0
	f.sweeper.afterMergeStep = func() {
		steps++
		if steps > 50 {
			t.Fatalf("the merge is still going after %d steps under ingest", steps)
		}
		// A new trace, and with it a new segment, after every step.
		id := fmt.Sprintf("%032x", 0xfeed0000+steps)
		f.arrive(t, f.project.ID, id, daysAgo(1), func(tr *model.Trace) { tr.Name = "live " + id })
	}
	done, _, err := f.sweeper.compact(t.Context())
	if err != nil || !done {
		t.Fatalf("done = %v, err = %v", done, err)
	}
	if held := filesHolding(t, f.store.path, word[4:]); len(held) != 0 {
		t.Errorf("the deleted trace's text is still in %v", held)
	}
}

// On a file that is not in incremental auto-vacuum mode the pragma frees
// nothing; the drain must see that and stop, not submit job after job.
func TestTheDrainStopsWhenNothingCanBeFreed(t *testing.T) {
	s := openFresh(t)
	conn, err := s.db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{`PRAGMA auto_vacuum = NONE`, `VACUUM`,
		`CREATE TABLE probe (body BLOB)`, `INSERT INTO probe VALUES (zeroblob(200000))`, `DELETE FROM probe`} {
		if _, err := conn.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	conn.Close()
	var free int64
	if err := s.db.QueryRow(`PRAGMA freelist_count`).Scan(&free); err != nil || free == 0 {
		t.Fatalf("control: freelist_count = %d (%v), want free pages to drain", free, err)
	}

	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	counted := &countingSubmitter{writer: writer}
	sweeper := s.NewSweeper(counted, SweepOptions{Now: func() time.Time { return sweepNow }})
	request(t, s, 0)
	done, _, err := sweeper.compact(t.Context())
	if err != nil || !done {
		t.Fatalf("done = %v, err = %v", done, err)
	}
	if counted.vacuums != 0 {
		t.Errorf("%d incremental-vacuum jobs on a file where they free nothing", counted.vacuums)
	}
}

// countingSubmitter counts the free-page jobs a sweeper submits.
type countingSubmitter struct {
	writer  *Writer
	vacuums int
}

func (c *countingSubmitter) Submit(ctx context.Context, job WriteJob) error {
	if _, ok := job.(*incrementalVacuum); ok {
		c.vacuums++
	}
	return c.writer.Submit(ctx, job)
}

// An erasure that finds only the user's per-user rows — their traces already
// swept — deletes rows that name them, and asks for the compaction every
// erasure that deleted something asks for (spec 044 #1).
func TestAnErasureOfOnlyTheRollupAsksForACompaction(t *testing.T) {
	f := newSweepFixture(t)
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec(`INSERT INTO users
		(project_id, user_id, traces, error_count, sessions, first_seen, last_seen)
		VALUES (?, 'gone', 3, 0, 1, 1, 2)`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	erase := &UserDataErase{ProjectID: f.project.ID, UserID: "gone", Confirm: "gone", Limit: 10,
		Now: sweepNow.UnixNano()}
	if err := f.writer.Submit(t.Context(), erase); err != nil {
		t.Fatal(err)
	}
	state, _ := f.store.Compaction(t.Context())
	// Stamped on the erasure's own clock, the one it measures everything
	// else by, not the wall clock beside it.
	if erase.CompactionRequested != erase.Now || state.RequestedAt != erase.CompactionRequested {
		t.Errorf("CompactionRequested = %d, state = %+v; want the erasure's own request, at its Now %d",
			erase.CompactionRequested, state, erase.Now)
	}

	// And one that finds nothing at all asks for nothing.
	nobody := &UserDataErase{ProjectID: f.project.ID, UserID: "nobody", Confirm: "nobody", Limit: 10}
	if err := f.writer.Submit(t.Context(), nobody); err != nil {
		t.Fatal(err)
	}
	if nobody.CompactionRequested != 0 {
		t.Errorf("an erasure that deleted nothing requested a compaction at %d", nobody.CompactionRequested)
	}
}

// A request made while a pass is under way may be one the pass has already
// read past, so it is due at the pass after; and never earlier than now.
func TestACompactionIsExpectedByTheNextPassNotThePast(t *testing.T) {
	f := newSweepFixture(t)
	now := sweepNow
	f.sweeper.now = func() time.Time { return now }
	f.sweeper.mu.Lock()
	f.sweeper.nextRun = now.Add(-time.Minute) // a late tick
	f.sweeper.mu.Unlock()
	if got := f.sweeper.ExpectedBy(); got != now.UnixNano() {
		t.Errorf("with the tick late: ExpectedBy = %v, want now", time.Unix(0, got).UTC())
	}

	f.sweeper.mu.Lock()
	f.sweeper.running, f.sweeper.runStart = true, now.Add(-time.Second)
	f.sweeper.mu.Unlock()
	if got, want := f.sweeper.ExpectedBy(), now.Add(-time.Second).Add(f.sweeper.interval).UnixNano(); got != want {
		t.Errorf("during a pass: ExpectedBy = %v, want the pass after it at %v",
			time.Unix(0, got).UTC(), time.Unix(0, want).UTC())
	}
}

// A pass that ends early — cancelled here, failed elsewhere — still moves the
// next run on, so an answer given after it names the pass the ticker will run,
// not the moment of the answer (#19).
func TestAPassThatEndsEarlyStillMovesTheNextRun(t *testing.T) {
	f := newSweepFixture(t)
	f.arrive(t, f.project.ID, strings.Repeat("ab", 16), daysAgo(400))
	f.sweeper.mu.Lock()
	f.sweeper.nextRun = sweepNow.Add(-time.Hour) // the pass before this one's
	f.sweeper.mu.Unlock()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := f.sweeper.Pass(cancelled); err == nil {
		t.Fatal("control: a pass on a cancelled context finished")
	}
	if got, want := f.sweeper.ExpectedBy(), sweepNow.Add(f.sweeper.interval).UnixNano(); got != want {
		t.Errorf("ExpectedBy = %v, want the next tick at %v", time.Unix(0, got).UTC(), time.Unix(0, want).UTC())
	}
}

// A store whose compaction row went missing still lets a deletion commit, and
// the deletion's request puts the row back (#19).
func TestADeletionWithoutTheCompactionRowStillCommits(t *testing.T) {
	f := newSweepFixture(t)
	id := strings.Repeat("cd", 16)
	f.arrive(t, f.project.ID, id, daysAgo(1))
	if _, err := f.store.db.Exec(`DELETE FROM compaction`); err != nil {
		t.Fatal(err)
	}
	job := &TraceDelete{ProjectID: f.project.ID, IDs: []string{id}, Confirm: id}
	if err := f.writer.Submit(t.Context(), job); err != nil {
		t.Fatalf("the deletion failed without the row: %v", err)
	}
	if state, _ := f.store.Compaction(t.Context()); job.CompactionRequested == 0 || state.RequestedAt != job.CompactionRequested {
		t.Errorf("CompactionRequested = %d, state = %+v; want the row back with the request", job.CompactionRequested, state)
	}
}

// A backup that failed to write leaves no file: an empty one would be the
// newest, the one the recovery hint names and an operator swaps in.
func TestAFailedBackupLeavesNoFile(t *testing.T) {
	for _, older := range []bool{true, false} {
		s, path := openTemp(t)
		previous := path + ".pre-0001_init.bak"
		if older {
			if err := os.WriteFile(previous, []byte("an older upgrade's copy"), 0o600); err != nil {
				t.Fatal(err)
			}
			at := time.Now().Add(-time.Hour)
			os.Chtimes(previous, at, at)
		}
		pendingAgain(t, s, false)
		s.Close()

		failing := vacuumInto
		vacuumInto = func(*sql.DB, string) error { return errors.New("disk full") }
		_, err := Open(path)
		vacuumInto = failing
		if err == nil {
			t.Fatal("Open succeeded although the backup failed")
		}
		if _, statErr := os.Stat(path + ".pre-0024_compaction.bak"); !os.IsNotExist(statErr) {
			t.Errorf("the failed backup left its file behind: %v", statErr)
		}
		want := "latest backup, if any: none"
		if older {
			want = "latest backup, if any: " + previous
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want the hint %q", err, want)
		}
	}
}

// A compaction clears the request it started from and no later one: a
// deletion committed while it ran is still pending afterwards.
func TestACompactionClearsOnlyTheRequestItStartedFrom(t *testing.T) {
	f := newSweepFixture(t)
	set := func(requested int64) {
		t.Helper()
		if _, err := f.store.db.Exec(`UPDATE compaction SET requested_at = ? WHERE id = 1`, requested); err != nil {
			t.Fatal(err)
		}
	}
	set(200)
	if err := f.writer.Submit(t.Context(), &compactionDone{Started: 100, At: 300}); err != nil {
		t.Fatal(err)
	}
	if state, _ := f.store.Compaction(t.Context()); state.RequestedAt != 200 || state.CompletedAt != 300 {
		t.Errorf("a request newer than the compaction: %+v, want it pending and the completion stamped", state)
	}
	if err := f.writer.Submit(t.Context(), &compactionDone{Started: 200, At: 400}); err != nil {
		t.Fatal(err)
	}
	if state, _ := f.store.Compaction(t.Context()); state.RequestedAt != 0 || state.CompletedAt != 400 {
		t.Errorf("the request the compaction started from: %+v, want it cleared", state)
	}
}

// A deletion whose clock reads earlier than the pending request — a time sync
// stepped it back, or it is injected — still makes a request of its own: the
// compaction that started from the earlier one does not clear it.
func TestARequestSurvivesAClockThatStepsBack(t *testing.T) {
	f := newSweepFixture(t)
	started := request(t, f.store, 1_000_000)
	late := request(t, f.store, 500_000) // committed while the compaction runs
	if late <= started {
		t.Fatalf("the later request was stamped %d, not after %d", late, started)
	}
	if state, _ := f.store.Compaction(t.Context()); state.RequestedAt != late {
		t.Errorf("stored %d, the request answered %d: they must be the same stamp", state.RequestedAt, late)
	}
	if err := f.writer.Submit(t.Context(), &compactionDone{Started: started, At: 2_000_000}); err != nil {
		t.Fatal(err)
	}
	if state, _ := f.store.Compaction(t.Context()); state.RequestedAt != late {
		t.Errorf("after the compaction: %+v, want the later request still pending", state)
	}
}

// The migration's request is for what an upgraded store's earlier deletions
// left; a new store has deleted nothing and starts with none (#18).
func TestOnlyAnUpgradedStoreStartsWithACompactionPending(t *testing.T) {
	s, path := openTemp(t)
	if state, _ := s.Compaction(t.Context()); state.RequestedAt != 0 {
		t.Errorf("a new store: %+v, want nothing pending", state)
	}
	pendingAgain(t, s, false)
	s.Close()

	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if state, _ := upgraded.Compaction(t.Context()); state.RequestedAt == 0 {
		t.Errorf("an upgraded store: %+v, want the migration's request pending", state)
	}
}

// A backup is found by its name in the database's own directory, never by a
// pattern built from the path: a data directory named with `[` or `*` names
// its own backups and no other directory's, and a link named like a backup is
// not one.
func TestBackupsAreTheDirectorysOwnWhateverItIsCalled(t *testing.T) {
	for _, name := range []string{"tp[12]*", "tp["} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dir, neighbour := filepath.Join(root, name), filepath.Join(root, "tp1")
			for _, d := range []string{dir, neighbour} {
				if err := os.Mkdir(d, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(dir, "tracepad.db")
			mine := path + ".pre-0001_init.bak"
			theirs := filepath.Join(neighbour, "tracepad.db.pre-0001_init.bak")
			old := time.Now().Add(-2 * BackupLifetime)
			for _, file := range []string{mine, theirs} {
				if err := os.WriteFile(file, []byte("copy"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(file, old, old); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(theirs, path+".pre-0002_traces.bak"); err != nil {
				t.Fatal(err)
			}

			if got := backupFiles(path); len(got) != 1 || got[0] != mine {
				t.Errorf("backupFiles = %q, want only %q", got, mine)
			}
			(&Store{path: path}).expireBackups(time.Now())
			if _, err := os.Stat(mine); !os.IsNotExist(err) {
				t.Errorf("the directory's own expired backup survived: %v", err)
			}
			if _, err := os.Stat(theirs); err != nil {
				t.Errorf("the neighbouring directory's backup was touched: %v", err)
			}
		})
	}
}

// Backups older than seven days go with the sweeper's pass; a younger one
// stays, and the store names it with the date it goes (#12).
func TestTheSweeperRemovesABackupAfterSevenDays(t *testing.T) {
	f := newSweepFixture(t)
	old := f.store.path + ".pre-0022_media_holders.bak"
	young := f.store.path + ".pre-0024_compaction.bak"
	for file, age := range map[string]time.Duration{old: 8 * 24 * time.Hour, young: 6 * 24 * time.Hour} {
		if err := os.WriteFile(file, []byte("backup"), 0o600); err != nil {
			t.Fatal(err)
		}
		at := sweepNow.Add(-age)
		if err := os.Chtimes(file, at, at); err != nil {
			t.Fatal(err)
		}
	}

	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the eight-day-old backup is still there: %v", err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Errorf("the six-day-old backup went early: %v", err)
	}
	backup := f.store.PreMigrationBackup()
	written := sweepNow.Add(-6 * 24 * time.Hour).UnixNano()
	if backup == nil || backup.Path != young || backup.CreatedAt != written ||
		backup.RemoveAfter != written+int64(BackupLifetime) {
		t.Errorf("PreMigrationBackup = %+v, want %s written %d", backup, young, written)
	}
}

// pendingAgain makes the newest migration pending on a database that already
// has it, the way an upgrade finds a database: its table dropped and its
// record gone. keepTable leaves the table, so that re-applying it fails.
func pendingAgain(t *testing.T, s *Store, keepTable bool) {
	t.Helper()
	if !keepTable {
		// A store from before 0024 is from before 0034 too, which adds a
		// column to the table and two to the erasures.
		for _, stmt := range []string{
			`DROP TABLE compaction`,
			`DROP INDEX idx_erasures_awaiting_compaction`,
			`ALTER TABLE erasures DROP COLUMN compacted_at`,
			`ALTER TABLE erasures DROP COLUMN compaction_request`,
			`DELETE FROM schema_migrations WHERE filename = '0034_erasure_compacted.sql'`,
		} {
			if _, err := s.db.Exec(stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
	}
	if _, err := s.db.Exec(`DELETE FROM schema_migrations WHERE filename = '0024_compaction.sql'`); err != nil {
		t.Fatal(err)
	}
}

// Once the migrations a run's backup guards have committed, the backups older
// than it go, each by name; the run's own stays.
func TestAnUpgradeRemovesTheBackupsItSupersedes(t *testing.T) {
	s, path := openTemp(t)
	older := path + ".pre-0001_init.bak"
	if err := os.WriteFile(older, []byte("an older upgrade's copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	pendingAgain(t, s, false)
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := os.Stat(older); !os.IsNotExist(err) {
		t.Errorf("the superseded backup survived a successful upgrade: %v", err)
	}
	if _, err := os.Stat(path + ".pre-0024_compaction.bak"); err != nil {
		t.Errorf("the upgrade's own backup is missing: %v", err)
	}
}

// A copy the operator took by hand is not the server's to delete, whatever it
// is called — `tracepad.db.pre-upgrade-<sha>.bak` begins and ends the way a
// pre-migration backup does, and a start, the sweeper and the erasure's answer
// all leave it alone (#23).
func TestAnOperatorsOwnBackupSurvivesAStartAndTheSweeper(t *testing.T) {
	s, path := openTemp(t)
	ours := path + ".pre-0001_init.bak"
	manual := []string{
		path + ".pre-upgrade-3f9c2ab.bak",
		path + ".pre-0035.bak",
		path + ".pre-0035_.bak",
		path + ".pre-0035_Raw.bak",
		path + ".pre-35_raw.bak",
		path + ".pre-.bak",
	}
	old := time.Now().Add(-2 * BackupLifetime)
	for _, file := range append([]string{ours}, manual...) {
		if err := os.WriteFile(file, []byte("a copy"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(file, old, old); err != nil {
			t.Fatal(err)
		}
	}
	pendingAgain(t, s, false)
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	s2.expireBackups(time.Now())
	if _, err := os.Stat(ours); !os.IsNotExist(err) {
		t.Errorf("the server's own superseded backup survived: %v", err)
	}
	for _, file := range manual {
		if _, err := os.Stat(file); err != nil {
			t.Errorf("a backup the operator took was removed: %s: %v", filepath.Base(file), err)
		}
	}
	if got := s2.PreMigrationBackup(); got == nil || got.Path != path+".pre-0024_compaction.bak" {
		t.Errorf("PreMigrationBackup = %+v, want the upgrade's own", got)
	}
}

// A migration that fails deletes nothing: every backup is still the way back.
func TestAFailedUpgradeKeepsEveryBackup(t *testing.T) {
	s, path := openTemp(t)
	older := path + ".pre-0001_init.bak"
	if err := os.WriteFile(older, []byte("an older upgrade's copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	pendingAgain(t, s, true)
	s.Close()

	if s2, err := Open(path); err == nil {
		s2.Close()
		t.Fatal("re-applying a migration over its own table succeeded")
	}
	for _, file := range []string{older, path + ".pre-0024_compaction.bak"} {
		if _, err := os.Stat(file); err != nil {
			t.Errorf("a failed upgrade removed %s: %v", filepath.Base(file), err)
		}
	}
}
