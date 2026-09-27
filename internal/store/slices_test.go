package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
)

// bulkBatch is a batch of traces of the given sizes, each observation a plain
// span a millisecond after the one before it.
func bulkBatch(projectID string, sizes ...int) *IngestBatch {
	batch := &IngestBatch{ProjectID: projectID}
	for t, size := range sizes {
		traceID := fmt.Sprintf("%032x", 0x5a1ce000+t)
		batch.Traces = append(batch.Traces, &model.Trace{ID: traceID, Name: "sliced", UserID: "u1",
			Tags: []string{"a"}, Metadata: map[string]any{"k": "v"}})
		for o := range size {
			start := int64(1_000_000_000 + o*1_000_000)
			batch.Observations = append(batch.Observations, &model.Observation{
				TraceID: traceID, ID: fmt.Sprintf("%08x%08x", t, o), Type: model.TypeSpan, Name: "step",
				Level: model.LevelDefault, StartTime: start, EndTime: start + 500_000,
			})
		}
	}
	return batch
}

// An export larger than a slice is cut into slices of at most SliceRows rows,
// every observation in exactly one, a trace written in exactly one and carried
// on as continued in every later slice it goes into, and no trace row at the end
// of a slice without one of its observations (spec 043 #11). The media go with
// the first slice, the raw body and what only it names with the last.
func TestSlicesCutAnExport(t *testing.T) {
	// 999 rows, then a trace that has no room for an observation, then one
	// larger than a slice, then small ones.
	batch := bulkBatch("p", 998, 5, 2500, 3, 3, 3)
	batch.Media = []MediaBody{{SHA256: "named", MimeType: "image/png"}, {SHA256: "rawonly", MimeType: "image/gif"}}
	batch.MediaRefs = []MediaRef{{SHA256: "named", TraceID: batch.Traces[2].ID}}
	batch.Resolved = []string{"named"}
	batch.Raw = &RawBatch{Body: []byte("raw")}
	batch.RawMedia = []string{"named", "rawonly"}

	cut := batch.Slices()
	if len(cut) < 4 {
		t.Fatalf("%d slices, want the export cut", len(cut))
	}
	var observations []*model.Observation
	full := map[string]int{}
	for i, slice := range cut {
		rows := len(slice.Traces) + len(slice.Observations)
		if i == 0 && rows != 999 {
			t.Errorf("the first slice holds %d rows, want 999: the next trace has no room in it", rows)
		}
		if rows > SliceRows {
			t.Errorf("slice %d holds %d rows, want at most %d", i, rows, SliceRows)
		}
		observations = append(observations, slice.Observations...)
		for _, trace := range slice.Traces {
			if !slice.continued[trace.ID] {
				full[trace.ID]++
			}
			if !slices.ContainsFunc(slice.Observations, func(o *model.Observation) bool { return o.TraceID == trace.ID }) {
				t.Errorf("slice %d carries trace %s with none of its observations", i, trace.ID)
			}
		}
		// A slice with another after it weighs a whole window, so that
		// its window closes at once; the last weighs its rows.
		want := rows
		if i < len(cut)-1 {
			want = WindowRows
		}
		if slice.weight() != want {
			t.Errorf("slice %d of %d weighs %d with %d rows, want %d", i, len(cut), slice.weight(), rows, want)
		}
	}
	if !slices.Equal(observations, batch.Observations) {
		t.Error("the slices do not hold every observation once, in order")
	}
	for _, trace := range batch.Traces {
		if full[trace.ID] != 1 {
			t.Errorf("trace %s carried whole %d times, want once", trace.ID, full[trace.ID])
		}
	}

	// Media travel with the trace that names them, in every slice that
	// carries it; the raw body's go with the last slice, beside the raw body;
	// every slice checks the resolved ids.
	named := batch.Traces[2].ID
	last := cut[len(cut)-1]
	for i, slice := range cut {
		carries := slices.ContainsFunc(slice.Traces, func(t *model.Trace) bool { return t.ID == named })
		var shas []string
		for _, body := range slice.Media {
			shas = append(shas, body.SHA256)
		}
		want := []string(nil)
		if carries {
			want = append(want, "named")
		}
		if slice == last {
			if !carries {
				want = append(want, "named")
			}
			want = append(want, "rawonly")
		}
		if !slices.Equal(shas, want) {
			t.Errorf("slice %d carries bodies %v, want %v", i, shas, want)
		}
		if refs := len(slice.MediaRefs); (refs == 1) != carries {
			t.Errorf("slice %d carries %d refs; carries the trace that names the body: %t", i, refs, carries)
		}
		if len(slice.Resolved) != 1 {
			t.Errorf("slice %d checks %d resolved ids, want the export's one", i, len(slice.Resolved))
		}
		if (slice.Raw != nil) != (slice == last) || (len(slice.RawMedia) > 0) != (slice == last) {
			t.Errorf("slice %d of %d: raw %v, raw media %v; want them on the last slice only", i, len(cut), slice.Raw, slice.RawMedia)
		}
	}

	small := bulkBatch("p", 10)
	if got := small.Slices(); len(got) != 1 || got[0] != small {
		t.Error("a batch that fits a slice is not its own one slice")
	}
}

// Written slice by slice, an export converges on the rows one transaction
// writes, and no slice leaves a media ref pending (spec 043 #11, #32).
func TestSlicesWriteWhatOneTransactionWrites(t *testing.T) {
	s, p := openIngestStore(t)
	whole, sliced := bulkBatch(p.ID, 600, 1500, 7), bulkBatch(p.ID, 600, 1500, 7)
	other, err := s.CreateProject("other", KeyPair{PublicKey: "tp-pk-other", Secret: "tp-sk-other"})
	if err != nil {
		t.Fatal(err)
	}
	whole.ProjectID = other.ID
	last := sliced.Traces[2].ID
	sliced.Media = []MediaBody{{SHA256: "ab", MimeType: "image/png", Body: []byte("png")}}
	sliced.MediaRefs = []MediaRef{{SHA256: "ab", TraceID: last}}

	apply := func(job *IngestBatch) {
		t.Helper()
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := job.apply(tx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	refs := func() (pending, settled int) {
		t.Helper()
		if err := s.db.QueryRow(`SELECT COALESCE(SUM(pending), 0), COALESCE(SUM(1 - pending), 0) FROM media_refs
		                          WHERE project_id = ?`, p.ID).Scan(&pending, &settled); err != nil {
			t.Fatal(err)
		}
		return pending, settled
	}

	apply(whole)
	cut := sliced.Slices()
	for i, slice := range cut {
		apply(slice)
		// No slice leaves a ref pending: a trace's refs are written with
		// the trace, so an export never takes room under the Langfuse
		// channel's cap of pending refs (spec 043 #32).
		if pending, _ := refs(); pending != 0 {
			t.Errorf("%d refs pending after slice %d", pending, i)
		}
	}
	if _, settled := refs(); settled != 1 {
		t.Errorf("%d refs settled, want the one", settled)
	}

	rows := func(projectID string) []string {
		t.Helper()
		var out []string
		q, err := s.db.Query(`SELECT id, name, user_id, tags, observation_count, timestamp, latency_ms
		                        FROM traces WHERE project_id = ? ORDER BY id`, projectID)
		if err != nil {
			t.Fatal(err)
		}
		defer q.Close()
		for q.Next() {
			var id, name, user, tags string
			var count, ts, latency int64
			if err := q.Scan(&id, &name, &user, &tags, &count, &ts, &latency); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprint(id, name, user, tags, count, ts, latency))
		}
		return out
	}
	if got, want := rows(p.ID), rows(other.ID); !slices.Equal(got, want) {
		t.Errorf("sliced traces\n%v\nwant\n%v", got, want)
	}
}

// A window closes at WindowRows as well as at MaxBatch jobs (spec 043 #12):
// sixteen big jobs queued behind a held writer commit two to a transaction, not
// all sixteen in one.
func TestWriterWindowClosesAtItsWeight(t *testing.T) {
	s, p := openIngestStore(t)
	var mu sync.Mutex
	var windows []int
	w, err := s.NewWriter(WriterOptions{CommitWindow: 100 * time.Millisecond, Committed: func(rows int) {
		mu.Lock()
		defer mu.Unlock()
		windows = append(windows, rows)
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	parked, release := make(chan struct{}, 1), make(chan struct{})
	w.beforeCommit = func() {
		select {
		case parked <- struct{}{}:
			<-release
		default:
		}
	}

	submit := func(jobs []*IngestBatch) {
		var wg sync.WaitGroup
		for _, job := range jobs {
			wg.Go(func() {
				if err := w.Submit(context.Background(), job); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
	}
	hold := batchFor(p.ID, fmt.Sprintf("%032x", 1), spanHex(1))
	go submit([]*IngestBatch{hold})
	<-parked

	var big []*IngestBatch
	for i := range 16 {
		job := bulkBatch(p.ID, 599)
		job.Traces[0].ID = fmt.Sprintf("%032x", 0xb16000+i)
		for _, o := range job.Observations {
			o.TraceID = job.Traces[0].ID
		}
		big = append(big, job)
	}
	done := make(chan struct{})
	go func() { submit(big); close(done) }()
	waitFor(t, func() bool { return len(w.queue) == 16 })
	close(release)
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(windows) < 2 {
		t.Fatalf("windows = %v", windows)
	}
	for _, rows := range windows[1:] {
		if rows >= 2*WindowRows {
			t.Errorf("a window of %d rows, want under %d: %v", rows, 2*WindowRows, windows)
		}
	}
}

// A trace deleted between two slices of its export is written whole by the
// slice that carries it on, as a late export would write it — not brought back
// as a row with its id alone (spec 043 #31). One still there is only stamped.
func TestAContinuedTraceDeletedBetweenSlicesIsWrittenWhole(t *testing.T) {
	s, p := openIngestStore(t)
	batch := bulkBatch(p.ID, 1500)
	cut := batch.Slices()
	if len(cut) != 2 || !cut[1].continued[batch.Traces[0].ID] {
		t.Fatalf("%d slices; want the one trace carried on into a second", len(cut))
	}
	apply := func(job *IngestBatch) {
		t.Helper()
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := job.apply(tx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	apply(cut[0])
	if _, err := s.db.Exec(`DELETE FROM traces WHERE project_id = ?`, p.ID); err != nil {
		t.Fatal(err)
	}
	apply(cut[1])
	var name, user, env string
	var count int
	if err := s.db.QueryRow(`SELECT name, user_id, environment, observation_count FROM traces WHERE project_id = ?`,
		p.ID).Scan(&name, &user, &env, &count); err != nil {
		t.Fatal(err)
	}
	if name != "sliced" || user != "u1" {
		t.Errorf("the trace came back as name %q, user %q; want it written whole", name, user)
	}
}

// SubmitWaiting waits out a full queue instead of refusing, and a stop wakes
// the ones still waiting with ErrWriterClosed rather than leaving them hung,
// even while the writer is still busy with its commit (spec 043 #31).
func TestSubmitWaitingWaitsForRoomAndAStopWakesIt(t *testing.T) {
	s, p := openIngestStore(t)
	w, err := s.NewWriter(WriterOptions{QueueDepth: 1, MaxBatch: 1})
	if err != nil {
		t.Fatal(err)
	}
	parked, release := make(chan struct{}, 8), make(chan struct{})
	w.beforeCommit = func() {
		parked <- struct{}{}
		<-release
	}
	job := func(i int) *IngestBatch { return batchFor(p.ID, fmt.Sprintf("%032x", i), spanHex(i)) }
	submit := func(waiting bool, i int) <-chan error {
		out := make(chan error, 1)
		go func() {
			if waiting {
				out <- w.SubmitWaiting(context.Background(), job(i))
			} else {
				out <- w.Submit(context.Background(), job(i))
			}
		}()
		return out
	}

	first := submit(false, 1)
	<-parked
	second := submit(false, 2)
	waitFor(t, func() bool { return len(w.queue) == 1 })
	if err := <-submit(false, 3); !errors.Is(err, ErrWriterBusy) {
		t.Fatalf("Submit into a full queue = %v, want ErrWriterBusy", err)
	}
	waiting := submit(true, 4)
	select {
	case err := <-waiting:
		t.Fatalf("SubmitWaiting returned %v with the queue full, want it to wait", err)
	case <-time.After(50 * time.Millisecond):
	}

	// Room comes when the writer moves on: the waiting job is taken.
	release <- struct{}{}
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	<-parked // the second is committing, the waiting one queued behind it
	waitFor(t, func() bool { return len(w.queue) == 1 })
	stranded := submit(true, 5)
	select {
	case err := <-stranded:
		t.Fatalf("SubmitWaiting returned %v with the queue full, want it to wait", err)
	case <-time.After(50 * time.Millisecond):
	}

	// A stop wakes the one still waiting at once, with the writer still
	// holding its commit.
	closed := make(chan error, 1)
	go func() { closed <- w.Close() }()
	select {
	case err := <-stranded:
		if !errors.Is(err, ErrWriterClosed) {
			t.Errorf("a waiting submission woken by a stop = %v, want ErrWriterClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a stop left a waiting submission hung")
	}
	close(release)
	for name, done := range map[string]<-chan error{"second": second, "waiting": waiting} {
		if err := <-done; err != nil {
			t.Errorf("%s submission: %v, want it committed", name, err)
		}
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}

// The raw batch of a sliced export lies inside the arrival window of every
// trace the export carried, as it does for one written in one transaction:
// an erasure finds a trace's batches by that window (spec 044 #3, spec 043
// #31).
func TestASlicedExportsRawBatchIsInsideEveryTracesWindow(t *testing.T) {
	s, p := openIngestStore(t)
	batch := bulkBatch(p.ID, 900, 400, 3, 1500, 2)
	batch.Raw = &RawBatch{Body: []byte("raw")}
	cut := batch.Slices()
	if len(cut) < 3 {
		t.Fatalf("%d slices, want several", len(cut))
	}
	for _, slice := range cut {
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := slice.apply(tx); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // each slice its own reading
	}
	var received int64
	if err := s.db.QueryRow(`SELECT received_at FROM raw_batches WHERE project_id = ?`, p.ID).Scan(&received); err != nil {
		t.Fatal(err)
	}
	rows, err := s.db.Query(`SELECT id, ingested_at, updated_at FROM traces WHERE project_id = ?`, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id string
		var ingested, updated int64
		if err := rows.Scan(&id, &ingested, &updated); err != nil {
			t.Fatal(err)
		}
		n++
		if received < ingested || received > updated {
			t.Errorf("trace %s's window [%d, %d] does not hold its raw batch, received at %d", id, ingested, updated, received)
		}
	}
	if n != len(batch.Traces) {
		t.Errorf("%d traces, want %d", n, len(batch.Traces))
	}
}

// A body the raw body names is written again by the last slice, beside the raw
// ref to it: a deletion between two slices that collected it — its one trace
// ref gone — does not leave the archived batch pointing at nothing (spec 043
// #32).
func TestARawBodysMediaSurviveADeletionBetweenSlices(t *testing.T) {
	s, p := openIngestStore(t)
	batch := bulkBatch(p.ID, 900, 400, 900)
	shared := batch.Traces[0].ID
	batch.Media = []MediaBody{{SHA256: "cd", MimeType: "image/png", Body: []byte("png")}}
	batch.MediaRefs = []MediaRef{{SHA256: "cd", TraceID: shared}}
	batch.Raw = &RawBatch{Body: []byte("raw")}
	batch.RawMedia = []string{"cd"}
	cut := batch.Slices()
	if len(cut) < 2 || slices.ContainsFunc(cut[len(cut)-1].Traces, func(t *model.Trace) bool { return t.ID == shared }) {
		t.Fatal("want the trace that names the body in an earlier slice than the raw body")
	}
	apply := func(job *IngestBatch) {
		t.Helper()
		tx, err := s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := job.apply(tx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	apply(cut[0])
	// What a deletion of the trace does to its media: the ref and the hold
	// go, and the body with nothing left naming it is collected.
	for _, statement := range []string{
		`DELETE FROM media_refs WHERE project_id = '` + p.ID + `'`,
		`DELETE FROM media_holders WHERE project_id = '` + p.ID + `'`,
		`DELETE FROM media WHERE sha256 = 'cd'`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, slice := range cut[1:] {
		apply(slice)
	}
	var bodies, rawRefs int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM media WHERE sha256 = 'cd'),
	                                (SELECT COUNT(*) FROM media_raw_refs WHERE sha256 = 'cd')`).Scan(&bodies, &rawRefs); err != nil {
		t.Fatal(err)
	}
	if bodies != 1 || rawRefs != 1 {
		t.Errorf("after the deletion the body is stored %d times and the raw batch names it %d times, want both once",
			bodies, rawRefs)
	}
}

// Observations whose trace the batch does not carry — which the mapper never
// produces, but a batch may hold — are sliced like the rest, with no trace
// row, rather than dropped.
func TestSlicesCarryObservationsWithoutTheirTrace(t *testing.T) {
	batch := bulkBatch("p", 3, 2400)
	strays := batch.Observations[3:]
	batch.Traces = batch.Traces[:1]
	cut := batch.Slices()
	var got []*model.Observation
	for i, slice := range cut {
		if rows := len(slice.Traces) + len(slice.Observations); rows > SliceRows {
			t.Errorf("slice %d holds %d rows", i, rows)
		}
		got = append(got, slice.Observations...)
	}
	if !slices.Equal(got[3:], strays) || len(got) != len(batch.Observations) {
		t.Errorf("%d observations sliced of %d, want every one, the strays after the trace's own", len(got), len(batch.Observations))
	}
}
