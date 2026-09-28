package store

import (
	"context"
	"database/sql"
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
// of a slice without one of its observations (spec 043 #11). A body goes with
// every slice whose refs name it, the raw body and what it names with the last.
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

	// A trace's refs travel in every slice that carries it, with the body
	// they name; the raw body's bodies go with the last slice, beside the
	// raw body; a slice checks the resolved ids its refs name, and the last
	// every one the raw body names.
	named := batch.Traces[2].ID
	last := cut[len(cut)-1]
	firstNaming := -1
	for i, slice := range cut {
		carries := slices.ContainsFunc(slice.Traces, func(t *model.Trace) bool { return t.ID == named })
		if carries && firstNaming < 0 {
			firstNaming = i
		}
		var shas []string
		for _, body := range slice.Media {
			shas = append(shas, body.SHA256)
		}
		want := []string(nil)
		if carries || slice == last {
			want = append(want, "named")
		}
		if slice == last {
			want = append(want, "rawonly")
		}
		if !slices.Equal(shas, want) {
			t.Errorf("slice %d carries bodies %v, want %v", i, shas, want)
		}
		if refs := len(slice.MediaRefs); (refs == 1) != carries {
			t.Errorf("slice %d carries %d refs; carries the trace that names the body: %t", i, refs, carries)
		}
		if checks := len(slice.Resolved); (checks == 1) != (carries || slice == last) {
			t.Errorf("slice %d checks %d resolved ids; carries the trace or the raw body that names it: %t",
				i, checks, carries || slice == last)
		}
		if (slice.Raw != nil) != (slice == last) || (len(slice.RawMedia) > 0) != (slice == last) {
			t.Errorf("slice %d of %d: raw %v, raw media %v; want them on the last slice only", i, len(cut), slice.Raw, slice.RawMedia)
		}
	}
	if firstNaming < 0 || firstNaming == len(cut)-1 {
		t.Fatalf("the trace that names the body starts in slice %d of %d; want it before the last", firstNaming, len(cut))
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

// heldWriter is a writer whose first commit waits for release, so that the
// jobs submitted meanwhile queue up and the windows they form can be read, in
// transactions and in the rows Committed reports for each (spec 043 #12).
type heldWriter struct {
	*Writer
	mu      sync.Mutex
	windows []int
	release chan struct{}
}

func newHeldWriter(t *testing.T, s *Store, p *Project) *heldWriter {
	t.Helper()
	h := &heldWriter{release: make(chan struct{})}
	w, err := s.NewWriter(WriterOptions{CommitWindow: 100 * time.Millisecond, Committed: func(rows int) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.windows = append(h.windows, rows)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	h.Writer = w
	parked := make(chan struct{}, 1)
	w.beforeCommit = func() {
		select {
		case parked <- struct{}{}:
			<-h.release
		default:
		}
	}
	go func() {
		if err := w.Submit(context.Background(), batchFor(p.ID, fmt.Sprintf("%032x", 1), spanHex(1))); err != nil {
			t.Error(err)
		}
	}()
	<-parked
	return h
}

// submitAll submits the jobs at once, releases the writer when all of them are
// queued, and returns each one's answer and the windows after the held one.
func (h *heldWriter) submitAll(t *testing.T, jobs ...WriteJob) ([]error, []int) {
	t.Helper()
	errs := make([]error, len(jobs))
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Go(func() { errs[i] = h.Submit(context.Background(), job) })
		// One at a time into the queue, so the windows follow this order.
		waitFor(t, func() bool { return len(h.queue) == i+1 })
	}
	close(h.release)
	wg.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	return errs, slices.Clone(h.windows[1:])
}

// A window closes at WindowRows as well as at MaxBatch jobs (spec 043 #12):
// sixteen big jobs queued behind a held writer commit two to a transaction, not
// all sixteen in one.
func TestWriterWindowClosesAtItsWeight(t *testing.T) {
	s, p := openIngestStore(t)
	h := newHeldWriter(t, s, p)
	var big []WriteJob
	for i := range 16 {
		job := bulkBatch(p.ID, 599)
		job.Traces[0].ID = fmt.Sprintf("%032x", 0xb16000+i)
		for _, o := range job.Observations {
			o.TraceID = job.Traces[0].ID
		}
		big = append(big, job)
	}
	errs, windows := h.submitAll(t, big...)
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if len(windows) == 0 {
		t.Fatal("no windows after the held one")
	}
	for _, rows := range windows {
		if rows >= 2*WindowRows {
			t.Errorf("a window of %d rows, want under %d: %v", rows, 2*WindowRows, windows)
		}
	}
}

// A write that carries rows weighs them, not one (spec 043 #35): an array of
// scores or dataset items has no count cap, and weighing one it let a window
// take 64 of them whole. A delete whose cascade nothing counts commits alone;
// a job of a row or a few still weighs one.
func TestAJobWeighsTheRowsItCarries(t *testing.T) {
	for _, c := range []struct {
		job  WriteJob
		want int
	}{
		{&ScoreWrite{Scores: make([]*Score, 700)}, 700},
		{&DatasetItemsWrite{Items: make([]*DatasetItemInput, 1200)}, 1200},
		{&QueueItemsAdd{Targets: make([]QueueTarget, 300)}, 300},
		{&QueueItemsFromTraces{Limit: 1000, Matched: 2}, 2},
		{&QueueItemsFromTraces{Limit: 500, Matched: 40000}, 500},
		{&QueueItemsFromTraces{Limit: 1000}, 1},
		{&TraceDelete{IDs: []string{"a", "b", "c"}}, 3},
		{&TraceDelete{ByFilter: true}, 1},
		{&UserDataErase{Limit: 500}, 500},
		{&DatasetDelete{Name: "d"}, WindowRows + 1},
		{&QueueDelete{Name: "q"}, WindowRows + 1},
		{&PromptDelete{Name: "p"}, WindowRows + 1},
		{&ScoreDelete{ID: "a"}, 1},
		{&QueueNext{}, 1},
		{&PromptVersionWrite{}, 1},
	} {
		if got := weightOf(c.job); got != c.want {
			t.Errorf("%T%+v weighs %d, want %d", c.job, c.job, got, c.want)
		}
	}
}

// Big writes that are not ingest close their windows too (spec 043 #35):
// sixteen score arrays of 600 queued behind a held writer commit at most two to
// a transaction, where weighing one each they committed all sixteen in one.
func TestAWindowOfScoreWritesClosesAtItsWeight(t *testing.T) {
	s, p := openIngestStore(t)
	h := newHeldWriter(t, s, p)
	one := 1.0
	var jobs []WriteJob
	for i := range 16 {
		job := &ScoreWrite{ProjectID: p.ID, Scores: make([]*Score, 600)}
		for j := range job.Scores {
			job.Scores[j] = &Score{ID: fmt.Sprintf("s-%d-%d", i, j), SessionID: "sess", Name: "q",
				DataType: "numeric", Value: &one, Timestamp: 1}
		}
		jobs = append(jobs, job)
	}
	errs, windows := h.submitAll(t, jobs...)
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	// Counted in transactions, not in the weights Committed reports: those
	// are what is under test, and weighing one each they summed to 16.
	if len(windows) < 8 {
		t.Errorf("sixteen writes of 600 committed in %d transactions, want at least 8: %v", len(windows), windows)
	}
	var stored int
	if err := s.db.QueryRow(`SELECT count(*) FROM scores WHERE project_id = ?`, p.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 16*600 {
		t.Errorf("%d scores stored, want %d", stored, 16*600)
	}
}

// weighedJob is a job of a given weight that runs do.
type weighedJob struct {
	rows int
	do   func(tx *sql.Tx) error
}

func (j *weighedJob) weight() int            { return j.rows }
func (j *weighedJob) apply(tx *sql.Tx) error { return j.do(tx) }

// A job heavier than a window commits alone (spec 043 #35). A window that
// fails is retried job by job, and the one that failed it is applied again: a
// score array the size of a body cap, refused by a score config at its last
// item, would run twice over, and a small job ahead of it would wait for both.
// Alone, it is applied once and refused once.
func TestAJobHeavierThanAWindowCommitsAlone(t *testing.T) {
	s, p := openIngestStore(t)
	h := newHeldWriter(t, s, p)
	applied := 0
	small := &weighedJob{rows: 1, do: func(*sql.Tx) error { return nil }}
	heavy := &weighedJob{rows: 5 * WindowRows, do: func(*sql.Tx) error {
		applied++
		return &Rejection{Kind: RejectInvalid, Message: "score 499999 does not fit its config"}
	}}
	errs, windows := h.submitAll(t, small, heavy)
	if errs[0] != nil || errs[1] == nil {
		t.Fatalf("answers %v, want the small job committed and the heavy one refused", errs)
	}
	if applied != 1 {
		t.Errorf("the heavy job was applied %d times, want once", applied)
	}
	if want := []int{1}; !slices.Equal(windows, want) {
		t.Errorf("committed windows %v, want %v: the small job by itself", windows, want)
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

// A trace cut into three slices takes its ref and its body in all three, and
// the body is stored once: the slices after the first find it stored (spec 043
// #34).
func TestATraceInThreeSlicesStoresItsBodyOnce(t *testing.T) {
	s, p := openIngestStore(t)
	batch := bulkBatch(p.ID, 500, 2200)
	long := batch.Traces[1].ID
	batch.Media = []MediaBody{{SHA256: "ef", MimeType: "image/png", Body: []byte("png")}}
	batch.MediaRefs = []MediaRef{{SHA256: "ef", TraceID: long}}
	cut := batch.Slices()
	carrying := 0
	for i, slice := range cut {
		if !slices.ContainsFunc(slice.Traces, func(t *model.Trace) bool { return t.ID == long }) {
			continue
		}
		carrying++
		if len(slice.Media) != 1 {
			t.Errorf("slice %d, the trace's slice number %d, carries %d bodies, want the one its ref names",
				i, carrying, len(slice.Media))
		}
		if len(slice.MediaRefs) != 1 {
			t.Errorf("slice %d carries %d refs, want the trace's one", i, len(slice.MediaRefs))
		}
	}
	if carrying != 3 {
		t.Fatalf("the trace spans %d slices, want three", carrying)
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
	}
	var bodies, refs int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM media WHERE sha256 = 'ef'),
	                                (SELECT COUNT(*) FROM media_refs WHERE sha256 = 'ef' AND pending = 0)`).Scan(&bodies, &refs); err != nil {
		t.Fatal(err)
	}
	if bodies != 1 || refs != 1 {
		t.Errorf("body stored %d times, %d settled refs; want one of each", bodies, refs)
	}
}

// A continued trace written whole again after a deletion between slices does
// not look its run up a second time: the export counts its unknown run once
// (spec 043 #33).
func TestAContinuedTraceCountsItsUnknownRunOnce(t *testing.T) {
	s, p := openIngestStore(t)
	batch := bulkBatch(p.ID, 1500)
	batch.Traces[0].RunID = fmt.Sprintf("%032x", 0xdead)
	cut := batch.Slices()
	runs := 0
	for i, slice := range cut {
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
		runs += len(slice.UnknownRuns)
		if i == 0 {
			if _, err := s.db.Exec(`DELETE FROM traces WHERE project_id = ?`, p.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if runs != 1 {
		t.Errorf("the export named an unknown run %d times, want once", runs)
	}
}

// applySlice commits one slice in a transaction of its own, as the writer
// would.
func applySlice(t *testing.T, s *Store, job *IngestBatch) error {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := job.apply(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// collectBetweenSlices does to a body what a deletion of the one trace that
// names it does: the ref and the project's hold go, and the body with nothing
// left naming it is collected.
func collectBetweenSlices(t *testing.T, s *Store, projectID, sha string) {
	t.Helper()
	for _, statement := range []string{
		`DELETE FROM media_refs WHERE project_id = ? AND sha256 = ?`,
		`DELETE FROM media_holders WHERE project_id = ? AND sha256 = ?`,
	} {
		if _, err := s.db.Exec(statement, projectID, sha); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`DELETE FROM media WHERE sha256 = ?`, sha); err != nil {
		t.Fatal(err)
	}
}

// Two traces in different slices point at one body. A deletion between the
// slices of the trace in the first collects it; the second slice writes it
// again beside its own ref, rather than a ref that finds no body and is not
// written (spec 043 #34).
func TestABodyTwoSlicesNameSurvivesADeletionBetweenThem(t *testing.T) {
	s, p := openIngestStore(t)
	batch := bulkBatch(p.ID, 900, 900)
	early, late := batch.Traces[0].ID, batch.Traces[1].ID
	batch.Media = []MediaBody{{SHA256: "gh", MimeType: "image/png", Body: []byte("png")}}
	batch.MediaRefs = []MediaRef{{SHA256: "gh", TraceID: early}, {SHA256: "gh", TraceID: late}}
	cut := batch.Slices()
	if len(cut) != 2 {
		t.Fatalf("%d slices, want the two traces in two", len(cut))
	}
	if err := applySlice(t, s, cut[0]); err != nil {
		t.Fatal(err)
	}
	collectBetweenSlices(t, s, p.ID, "gh")
	if err := applySlice(t, s, cut[1]); err != nil {
		t.Fatal(err)
	}
	var bodies, refs int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM media WHERE sha256 = 'gh'),
	                                (SELECT COUNT(*) FROM media_refs WHERE sha256 = 'gh' AND trace_id = ?)`,
		late).Scan(&bodies, &refs); err != nil {
		t.Fatal(err)
	}
	if bodies != 1 || refs != 1 {
		t.Errorf("the later trace's body is stored %d times and its ref %d times, want both once", bodies, refs)
	}
}

// A resolved Langfuse id that only the raw body names — a span the mapper
// skipped carried it — is checked by the last slice, where the raw refs are
// written: the archived body points at the body the id was resolved to, and
// one collected between the slices refuses the export with ErrMediaGone, to be
// taken again unresolved, as an export in one slice is (spec 041 #9, spec 043
// #34).
func TestAResolvedIdOnlyTheRawBodyNamesIsCheckedByTheLastSlice(t *testing.T) {
	s, p := openIngestStore(t)
	if _, err := s.db.Exec(`INSERT INTO media (sha256, mime_type, size, body, created_at) VALUES ('ij', 'image/png', 3, x'706e67', 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO media_holders (sha256, project_id, mime_type, first_at) VALUES ('ij', ?, 'image/png', 1)`, p.ID); err != nil {
		t.Fatal(err)
	}
	batch := bulkBatch(p.ID, 900, 900)
	batch.Resolved = []string{"ij"}
	batch.Raw = &RawBatch{Body: []byte("raw")}
	batch.RawMedia = []string{"ij"}
	cut := batch.Slices()
	if len(cut) != 2 {
		t.Fatalf("%d slices, want two", len(cut))
	}
	if err := applySlice(t, s, cut[0]); err != nil {
		t.Fatal(err)
	}
	collectBetweenSlices(t, s, p.ID, "ij")
	if err := applySlice(t, s, cut[1]); !errors.Is(err, ErrMediaGone) {
		t.Errorf("the last slice answered %v, want ErrMediaGone", err)
	}
}
