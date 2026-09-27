package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
)

// The retention sweeper (spec 005, Testing #1–#4): what goes, what stays, and
// that neither the writer nor a neighbouring project notices.

// now is the instant the sweep tests measure their windows from. Fixed rather
// than time.Now so that "31 days ago" is a literal and not a flake.
var sweepNow = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func daysAgo(n int) int64 { return sweepNow.Add(-time.Duration(n) * 24 * time.Hour).UnixNano() }

type sweepFixture struct {
	store   *Store
	writer  *Writer
	sweeper *Sweeper
	project *Project
}

func newSweepFixture(t *testing.T) *sweepFixture {
	t.Helper()
	s := openFresh(t)

	project, err := s.CreateProject("test", KeyPair{PublicKey: "tp-pk-test", Secret: "tp-sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	writer, err := s.NewWriter(quickWrites)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })

	return &sweepFixture{
		store:   s,
		writer:  writer,
		sweeper: s.NewSweeper(writer, SweepOptions{Now: func() time.Time { return sweepNow }}),
		project: project,
	}
}

// arrive writes one trace with one observation carrying payloads, stamped with
// the arrival time retention counts from.
func (f *sweepFixture) arrive(t *testing.T, projectID, traceID string, at int64, opts ...func(*model.Trace)) {
	t.Helper()
	trace := &model.Trace{ID: traceID, UserID: "u1", Metadata: map[string]any{"trace": traceID}}
	for _, opt := range opts {
		opt(trace)
	}
	batch := &IngestBatch{
		ProjectID:  projectID,
		IngestedAt: at,
		Traces:     []*model.Trace{trace},
		Observations: []*model.Observation{{
			TraceID: traceID, ID: traceID[:16], Type: model.TypeSpan, Level: model.LevelDefault,
			StartTime: at, EndTime: at + 1_000_000,
			Input:  map[string]any{"prompt": strings.Repeat("x", 64)},
			Output: map[string]any{"completion": strings.Repeat("y", 64)},
		}},
	}
	if err := f.writer.Submit(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
}

func (f *sweepFixture) setRetention(t *testing.T, projectID string, days, rawDays *int) {
	t.Helper()
	if _, err := f.store.db.Exec(
		`UPDATE projects SET retention_days = ?, raw_retention_days = ? WHERE id = ?`,
		nullInt(days), nullInt(rawDays), projectID); err != nil {
		t.Fatal(err)
	}
}

func nullInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func days(n int) *int { return &n }

func (f *sweepFixture) count(t *testing.T, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.store.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestSweepRemovesOnlyExpiredTraces is Testing #1: the window is applied to
// arrival, everything hanging off an expired trace goes with it, a project
// that never set a window is untouched, and neither is the neighbour.
func TestSweepRemovesOnlyExpiredTraces(t *testing.T) {
	f := newSweepFixture(t)

	// A second tenant with data of exactly the same age, and no window.
	other, err := f.store.CreateProject("other", KeyPair{PublicKey: "tp-pk-other", Secret: "tp-sk-other"})
	if err != nil {
		t.Fatal(err)
	}

	f.setRetention(t, f.project.ID, days(30), nil)
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(31)) // expired
	f.arrive(t, f.project.ID, hexTrace(2), daysAgo(40)) // expired
	f.arrive(t, f.project.ID, hexTrace(3), daysAgo(29)) // inside the window
	f.arrive(t, other.ID, hexTrace(4), daysAgo(90))     // no window at all

	// A score on an expired trace, which goes with it.
	if err := f.writer.Submit(t.Context(), &ScoreWrite{ProjectID: f.project.ID, Scores: []*Score{{
		ID: strings.Repeat("a", 32), TraceID: hexTrace(1), Name: "quality",
		DataType: ScoreNumeric, Value: floatPtr(1), Timestamp: daysAgo(31), CreatedAt: daysAgo(31),
	}}}); err != nil {
		t.Fatal(err)
	}

	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}

	if got := f.count(t, `SELECT COUNT(*) FROM traces WHERE project_id = ?`, f.project.ID); got != 1 {
		t.Errorf("traces = %d, want only the one inside the window", got)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces WHERE project_id = ? AND id = ?`,
		f.project.ID, hexTrace(3)); got != 1 {
		t.Errorf("the trace inside the window was swept")
	}
	if got := f.count(t, `SELECT COUNT(*) FROM observations WHERE project_id = ?`, f.project.ID); got != 1 {
		t.Errorf("observations = %d, want the surviving trace's only", got)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM scores WHERE project_id = ?`, f.project.ID); got != 0 {
		t.Errorf("scores = %d, want the expired trace's judgement gone with it", got)
	}
	// Each trace carries three payloads (its metadata, its span's input and
	// output); the two expired ones took six with them.
	if got := f.count(t, `SELECT COUNT(*) FROM payloads`); got != 6 {
		t.Errorf("payloads = %d, want the surviving trace's three and the other project's three", got)
	}

	// The tenant that set no window keeps everything: NULL means forever,
	// and forever is the default (spec 005 #2).
	if got := f.count(t, `SELECT COUNT(*) FROM traces WHERE project_id = ?`, other.ID); got != 1 {
		t.Errorf("the other project's trace was swept: it has no retention window")
	}

	status := f.sweeper.Status(f.project.ID)
	if status.TracesDeleted != 2 {
		t.Errorf("traces_deleted = %d, want 2", status.TracesDeleted)
	}
	if f.sweeper.Status(other.ID).TracesDeleted != 0 {
		t.Errorf("the other project's counter moved; counters are per project (spec 004 #33)")
	}
	if status.LastRun == 0 || status.NextRun <= status.LastRun {
		t.Errorf("status = %+v, want a last run and a later next one", status)
	}
}

// TestSweepIsChunkedAndFinishes: a backlog larger than one chunk is cleared by
// repeated chunks within the pass, each its own short transaction.
func TestSweepIsChunkedAndFinishes(t *testing.T) {
	f := newSweepFixture(t)
	f.sweeper.chunk = 4
	f.setRetention(t, f.project.ID, days(1), nil)

	for i := 1; i <= 11; i++ {
		f.arrive(t, f.project.ID, hexTrace(i), daysAgo(10))
	}
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces`); got != 0 {
		t.Errorf("traces = %d, want the whole backlog cleared across chunks", got)
	}
}

// TestSweepCollectsOrphanPayloads is Testing #2. An overwriting upsert leaves
// the previous payload row behind (migration 0002 says so in as many words);
// this is the stage that was promised to collect it.
func TestSweepCollectsOrphanPayloads(t *testing.T) {
	f := newSweepFixture(t)

	// Same span delivered twice with different payloads: the second
	// delivery writes new payload rows and orphans the first's.
	for _, body := range []string{"first", "second"} {
		batch := &IngestBatch{
			ProjectID:  f.project.ID,
			IngestedAt: daysAgo(1),
			Traces:     []*model.Trace{{ID: hexTrace(1)}},
			Observations: []*model.Observation{{
				TraceID: hexTrace(1), ID: hexSpan(1), Type: model.TypeSpan,
				Level: model.LevelDefault, StartTime: 1, EndTime: 2,
				Input: map[string]any{"prompt": body},
			}},
		}
		if err := f.writer.Submit(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.count(t, `SELECT COUNT(*) FROM payloads`); got != 2 {
		t.Fatalf("payloads = %d, want the overwritten one still there before the sweep", got)
	}

	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM payloads`); got != 1 {
		t.Errorf("payloads = %d, want the orphan collected and the live one kept", got)
	}
	// And the live one is still readable through its owner.
	var input any
	rows, err := f.store.Observations(t.Context(), f.project.ID, hexTrace(1), WithIO)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("observations = %d, want the one that was upserted", len(rows))
	}
	input = rows[0].Input
	if fmt.Sprint(input) != "map[prompt:second]" {
		t.Errorf("input = %v, want the last delivery's", input)
	}
}

// TestSweepWindowSeeks is the EXPLAIN QUERY PLAN check the spec's data
// contract asks for by name (method of spec 003 #25): the hourly window query
// must seek through idx_traces_ingested, not scan the project.
func TestSweepWindowSeeks(t *testing.T) {
	f := newSweepFixture(t)

	plan, err := f.store.explainQueryPlan(
		`SELECT id FROM traces WHERE project_id = ? AND ingested_at < ? ORDER BY ingested_at LIMIT ?`,
		f.project.ID, daysAgo(30), 1000)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")

	if !strings.Contains(joined, "idx_traces_ingested") {
		t.Errorf("the sweep window does not use its index:\n%s", joined)
	}
	if !strings.Contains(joined, "SEARCH") {
		t.Errorf("the sweep window scans rather than seeks:\n%s", joined)
	}
	if strings.Contains(joined, "TEMP B-TREE") {
		t.Errorf("the sweep window sorts through a temporary B-tree:\n%s", joined)
	}
}

// TestRawBatchesFollowTheirOwnWindow is Decision 6: raw is swept by its own
// age, and by the project's window only when it has none of its own.
func TestRawBatchesFollowTheirOwnWindow(t *testing.T) {
	f := newSweepFixture(t)
	f.setRetention(t, f.project.ID, days(30), days(7))

	for _, age := range []int{3, 10, 40} {
		batch := &IngestBatch{
			ProjectID:  f.project.ID,
			IngestedAt: daysAgo(age),
			Raw:        &RawBatch{ReceivedAt: daysAgo(age), Dialect: "otel", Body: []byte("body")},
		}
		if err := f.writer.Submit(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
	}

	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM raw_batches`); got != 1 {
		t.Errorf("raw batches = %d, want only the one inside the 7-day raw window", got)
	}
	if f.sweeper.Status(f.project.ID).RawBatchesDeleted != 2 {
		t.Errorf("raw counter = %d, want 2", f.sweeper.Status(f.project.ID).RawBatchesDeleted)
	}

	// With no raw window of its own, raw lives exactly as long as the
	// parsed data: the 10-day-old batch would have survived a 30-day one.
	f2 := newSweepFixture(t)
	f2.setRetention(t, f2.project.ID, days(30), nil)
	for _, age := range []int{10, 40} {
		batch := &IngestBatch{
			ProjectID:  f2.project.ID,
			IngestedAt: daysAgo(age),
			Raw:        &RawBatch{ReceivedAt: daysAgo(age), Dialect: "otel", Body: []byte("body")},
		}
		if err := f2.writer.Submit(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
	}
	if err := f2.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f2.count(t, `SELECT COUNT(*) FROM raw_batches`); got != 1 {
		t.Errorf("raw batches = %d, want the 10-day-old one kept under the project's 30-day window", got)
	}
}

// TestSweepPurgesADeletedProjectAfterItsGrace is Testing #5's tail: the data
// of a soft-deleted project survives the grace window and then does not, and a
// restore inside the window cancels the purge — checked against stored state
// inside the transaction, not against what the pass read a moment earlier.
func TestSweepPurgesADeletedProjectAfterItsGrace(t *testing.T) {
	f := newSweepFixture(t)
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1))

	// Deleted yesterday: still inside the seven-day grace.
	if _, err := f.store.db.Exec(`UPDATE projects SET deleted_at = ? WHERE id = ?`,
		daysAgo(1), f.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces`); got != 1 {
		t.Fatalf("traces = %d, want the data still recoverable inside the grace window", got)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM projects`); got != 1 {
		t.Fatalf("the project row went before its grace window ran out")
	}

	// Deleted eight days ago: past it.
	if _, err := f.store.db.Exec(`UPDATE projects SET deleted_at = ? WHERE id = ?`,
		daysAgo(8), f.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"traces", "observations", "payloads", "projects", "api_keys"} {
		if got := f.count(t, `SELECT COUNT(*) FROM `+table); got != 0 {
			t.Errorf("%s = %d after the purge, want nothing left", table, got)
		}
	}
}

// TestPurgeStopsWhenTheProjectIsRestored: the job re-reads the row it is about
// to act on, so a restore that lands between the pass and the transaction wins
// (spec 003 Decision 20 applied to the one write that cannot be undone).
func TestPurgeStopsWhenTheProjectIsRestored(t *testing.T) {
	f := newSweepFixture(t)
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1))
	if _, err := f.store.db.Exec(`UPDATE projects SET deleted_at = ? WHERE id = ?`,
		daysAgo(8), f.project.ID); err != nil {
		t.Fatal(err)
	}

	// The pass has read the project as purgable; the restore lands first.
	project, err := f.store.ProjectByID(context.Background(), f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec(`UPDATE projects SET deleted_at = NULL WHERE id = ?`, f.project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sweeper.sweepProject(t.Context(), project, sweepNow); err != nil {
		t.Fatal(err)
	}

	if got := f.count(t, `SELECT COUNT(*) FROM traces`); got != 1 {
		t.Errorf("traces = %d, want the restored project's data intact", got)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM projects`); got != 1 {
		t.Errorf("the restored project was purged anyway")
	}
}

// TestSweepShrinksTheFile is Testing #4 and the operator-visible half of
// retention (Decision 5): deleting rows without a vacuum returns nothing to
// the filesystem.
func TestSweepShrinksTheFile(t *testing.T) {
	f := newSweepFixture(t)
	f.setRetention(t, f.project.ID, days(1), nil)

	var mode int
	if err := f.store.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != 2 {
		t.Fatalf("auto_vacuum = %d, want 2 (incremental): nothing can be reclaimed otherwise", mode)
	}

	for i := 1; i <= 200; i++ {
		f.arrive(t, f.project.ID, hexTrace(i), daysAgo(10))
	}
	// Checkpoint so the bulk load is in the main file rather than the WAL,
	// which is what the sweep can hand back.
	if _, err := f.store.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	before := fileBytes(t, f.store.path)

	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	after := fileBytes(t, f.store.path)

	if after >= before {
		t.Errorf("database is %d bytes after the sweep and was %d before: nothing was returned to the filesystem",
			after, before)
	}
}

// TestSweepAndIngestShareTheWriter is Testing #3, the harness of spec 003 #24:
// the sweeper is a writer of the same kind as ingest, so the two serialize
// instead of colliding. A second write connection would answer BUSY here.
func TestSweepAndIngestShareTheWriter(t *testing.T) {
	f := newSweepFixture(t)
	f.setRetention(t, f.project.ID, days(1), nil)
	for i := 1; i <= 40; i++ {
		f.arrive(t, f.project.ID, hexTrace(i), daysAgo(10))
	}

	var wg sync.WaitGroup
	errs := make(chan error, 64)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 5 {
			if err := f.sweeper.Pass(context.Background()); err != nil {
				errs <- err
			}
		}
	}()
	for i := 100; i < 140; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			batch := &IngestBatch{
				ProjectID:  f.project.ID,
				IngestedAt: sweepNow.UnixNano(),
				Traces:     []*model.Trace{{ID: hexTrace(i)}},
				Observations: []*model.Observation{{
					TraceID: hexTrace(i), ID: hexSpan(i), Type: model.TypeSpan,
					Level: model.LevelDefault, StartTime: 1, EndTime: 2,
				}},
			}
			if err := f.writer.Submit(context.Background(), batch); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a write failed while the sweeper ran: %v", err)
	}

	// Every fresh trace was acknowledged, so every fresh trace is stored:
	// an ack that the sweep swallowed would be the failure this guards.
	if got := f.count(t, `SELECT COUNT(*) FROM traces`); got != 40 {
		t.Errorf("traces = %d, want the 40 that were acknowledged during the sweep", got)
	}
}

// TestArrivalIsStampedOnceAndNotRefreshed is Decision 1: a late span joining
// an old trace must not restart its lease, or a chatty trace would be
// immortal.
func TestArrivalIsStampedOnceAndNotRefreshed(t *testing.T) {
	f := newSweepFixture(t)
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(40))
	f.arrive(t, f.project.ID, hexTrace(1), sweepNow.UnixNano())

	var arrived int64
	if err := f.store.db.QueryRow(`SELECT ingested_at FROM traces WHERE id = ?`, hexTrace(1)).
		Scan(&arrived); err != nil {
		t.Fatal(err)
	}
	if arrived != daysAgo(40) {
		t.Errorf("ingested_at = %d, want the first delivery's %d", arrived, daysAgo(40))
	}

	f.setRetention(t, f.project.ID, days(30), nil)
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces`); got != 0 {
		t.Errorf("the trace survived: a later span refreshed its lease")
	}
}

// TestRetentionClockIgnoresTheClientClock is the rest of Decision 1: a client
// that sends the future does not buy immortality, and one that sends the past
// does not get deleted for it.
func TestRetentionClockIgnoresTheClientClock(t *testing.T) {
	f := newSweepFixture(t)
	f.setRetention(t, f.project.ID, days(30), nil)

	// Arrived now, but its spans claim 1999.
	batch := &IngestBatch{
		ProjectID:  f.project.ID,
		IngestedAt: sweepNow.UnixNano(),
		Traces:     []*model.Trace{{ID: hexTrace(1)}},
		Observations: []*model.Observation{{
			TraceID: hexTrace(1), ID: hexSpan(1), Type: model.TypeSpan, Level: model.LevelDefault,
			StartTime: time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano(),
			EndTime:   time.Date(1999, 1, 1, 0, 0, 1, 0, time.UTC).UnixNano(),
		}},
	}
	if err := f.writer.Submit(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces`); got != 1 {
		t.Errorf("a trace that arrived today was swept because its client sent 1999")
	}
}

func fileBytes(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func floatPtr(v float64) *float64 { return &v }

// The findings of the code review on PR #6, each as the test that would have
// caught it.

// TestAnAbsurdWindowKeepsEverything: a window is turned into a nanosecond
// cutoff, and past ~106751 days that multiplication overflows int64 and wraps
// the cutoff into the *future*, where it matches every row. "Keep it for a
// million days" must not mean "delete it all on the next pass".
func TestAnAbsurdWindowKeepsEverything(t *testing.T) {
	f := newSweepFixture(t)
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(400))

	// 200000 days is the value that wraps: now − 200000 days lands in 2063.
	f.setRetention(t, f.project.ID, days(200000), nil)
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces`); got != 1 {
		t.Errorf("traces = %d after a 200000-day window, want the trace kept", got)
	}

	// And the dry run agrees with the sweep, rather than confirming the
	// same wrong answer.
	counts, err := f.store.RetentionPreview(t.Context(), f.project.ID, days(200000), nil, nil, sweepNow.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if counts.Any() {
		t.Errorf("the preview of a 200000-day window promises to delete %+v", counts)
	}

	// A window inside the ceiling still works exactly as before.
	f.setRetention(t, f.project.ID, days(30), nil)
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces`); got != 0 {
		t.Errorf("traces = %d under a 30-day window, want the 400-day-old trace swept", got)
	}
}

// TestPurgeWaitsForTheChunksToDrain: dropping the project row cascades the
// traces away, but `payloads` has no project column and so no cascade
// (spec 002 #8). Purging while chunks remain strands their payloads for the
// orphan pass to find a few thousand at a time.
func TestPurgeWaitsForTheChunksToDrain(t *testing.T) {
	f := newSweepFixture(t)
	// More traces than one pass may remove: two chunks of one.
	f.sweeper.chunk = 1
	f.sweeper.maxChunks = 2
	for i := 1; i <= 5; i++ {
		f.arrive(t, f.project.ID, hexTrace(i), daysAgo(1))
	}
	if _, err := f.store.db.Exec(`UPDATE projects SET deleted_at = ? WHERE id = ?`,
		daysAgo(8), f.project.ID); err != nil {
		t.Fatal(err)
	}

	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM projects`); got != 1 {
		t.Fatalf("the project row went while %d traces were still to sweep",
			f.count(t, `SELECT COUNT(*) FROM traces`))
	}

	// Passes until the backlog is clear; then the project goes, and
	// nothing is left behind with it.
	for range 5 {
		if err := f.sweeper.Pass(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"traces", "observations", "payloads", "projects"} {
		if got := f.count(t, `SELECT COUNT(*) FROM `+table); got != 0 {
			t.Errorf("%s = %d after the purge finished, want nothing left", table, got)
		}
	}
}
