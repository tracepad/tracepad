package store

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
)

// Data at rest (spec 044 #10–#13): what a deletion leaves in the file, and who
// can read the file.

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
	if state, err := f.store.Compaction(); err != nil || state.RequestedAt != 0 {
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
	state, err := f.store.Compaction()
	if err != nil || state.RequestedAt == 0 {
		t.Fatalf("after a deletion: %+v, %v; want a pending request", state, err)
	}

	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if held := filesHolding(t, f.store.path, tail); len(held) != 0 {
		t.Errorf("the deleted trace's text is still in %v after the compaction", held)
	}
	state, err = f.store.Compaction()
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
	if state, _ := f.store.Compaction(); state.RequestedAt != 0 {
		t.Errorf("the retention sweep requested a compaction: %+v", state)
	}
}

// A reader holding a snapshot keeps the truncating checkpoint from finishing;
// the pass gives up after its short wait and leaves the request for the next.
func TestABusyCheckpointLeavesTheRequestPending(t *testing.T) {
	f := newSweepFixture(t)
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

	done, err := f.sweeper.compact(t.Context())
	reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("the compaction reported done while a reader held the log")
	}
	if state, _ := f.store.Compaction(); state.RequestedAt == 0 {
		t.Fatal("the request was cleared although the checkpoint did not finish")
	}
	if done, err := f.sweeper.compact(t.Context()); err != nil || !done {
		t.Fatalf("the next attempt: done = %v, err = %v", done, err)
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
	if state, _ := f.store.Compaction(); state.RequestedAt != 200 || state.CompletedAt != 300 {
		t.Errorf("a request newer than the compaction: %+v, want it pending and the completion stamped", state)
	}
	if err := f.writer.Submit(t.Context(), &compactionDone{Started: 200, At: 400}); err != nil {
		t.Fatal(err)
	}
	if state, _ := f.store.Compaction(); state.RequestedAt != 0 || state.CompletedAt != 400 {
		t.Errorf("the request the compaction started from: %+v, want it cleared", state)
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
		backup.RemovedAt != written+int64(BackupLifetime) {
		t.Errorf("PreMigrationBackup = %+v, want %s written %d", backup, young, written)
	}
}

// pendingAgain makes the newest migration pending on a database that already
// has it, the way an upgrade finds a database: its table dropped and its
// record gone. keepTable leaves the table, so that re-applying it fails.
func pendingAgain(t *testing.T, s *Store, keepTable bool) {
	t.Helper()
	if !keepTable {
		if _, err := s.db.Exec(`DROP TABLE compaction`); err != nil {
			t.Fatal(err)
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
