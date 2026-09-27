package store

import (
	"context"
	"fmt"
	"reflect"
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
// every observation in exactly one, a trace's full row in exactly one and its
// id alone in every later slice it continues into, and no trace row at the end
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
			if trace.Name != "" {
				full[trace.ID]++
			} else if !reflect.DeepEqual(*trace, model.Trace{ID: trace.ID}) {
				t.Errorf("slice %d repeats trace %s with more than its id: %+v", i, trace.ID, trace)
			}
			if !slices.ContainsFunc(slice.Observations, func(o *model.Observation) bool { return o.TraceID == trace.ID }) {
				t.Errorf("slice %d carries trace %s with none of its observations", i, trace.ID)
			}
		}
		// A slice with another after it weighs a whole slice, so that
		// its window closes at once; the last weighs its rows.
		want := rows
		if i < len(cut)-1 {
			want = SliceRows
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

	first, last := cut[0], cut[len(cut)-1]
	if len(first.Media) != 1 || first.Media[0].SHA256 != "named" || len(first.MediaRefs) != 1 ||
		len(first.Resolved) != 1 || first.Raw != nil {
		t.Errorf("first slice media = %v, refs %v, resolved %v, raw %v; want the named body, its ref and the check",
			first.Media, first.MediaRefs, first.Resolved, first.Raw)
	}
	if len(last.Media) != 1 || last.Media[0].SHA256 != "rawonly" || last.Raw == nil ||
		len(last.RawMedia) != 2 || last.mediaTypes["named"] != "image/png" {
		t.Errorf("last slice media = %v, raw %v, raw media %v, types %v; want the raw body and what only it names",
			last.Media, last.Raw, last.RawMedia, last.mediaTypes)
	}
	for _, middle := range cut[1 : len(cut)-1] {
		if len(middle.Media)+len(middle.MediaRefs)+len(middle.Resolved)+len(middle.RawMedia) > 0 || middle.Raw != nil {
			t.Errorf("a middle slice carries media or raw: %+v", middle)
		}
	}

	small := bulkBatch("p", 10)
	if got := small.Slices(); len(got) != 1 || got[0] != small {
		t.Error("a batch that fits a slice is not its own one slice")
	}
}

// Written slice by slice, an export converges on the rows one transaction
// writes, and the ref the first slice writes for a trace a later one carries
// is pending until then — so an export that stops half-way leaves a ref the
// sweep collects, not one that holds its body for ever (spec 043 #11).
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
	pending := func() int {
		var n int
		if err := s.db.QueryRow(`SELECT pending FROM media_refs WHERE trace_id = ?`, last).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	apply(whole)
	cut := sliced.Slices()
	apply(cut[0])
	if pending() != 1 {
		t.Error("the ref of a trace no slice has written yet is settled")
	}
	for _, slice := range cut[1:] {
		apply(slice)
	}
	if pending() != 0 {
		t.Error("the ref is still pending after its trace arrived")
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
