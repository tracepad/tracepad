package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The upload channel's lifecycle in the store (spec 041 #28–#31): the write
// re-checks what the handler checked before the body, because a revocation,
// a deletion or a burst of uploads can land in between.

// uploadOf is the job the PUT submits for a grant a project's key asked for.
func uploadOf(projectID, traceID, key string, body MediaBody, now int64) *MediaUpload {
	return &MediaUpload{Grant: MediaGrant{ProjectID: projectID, TraceID: traceID, SHA256: body.SHA256, Key: key},
		Body: body, Now: now}
}

// upload is the fixture's own key's job.
func (f *sweepFixture) upload(traceID string, body MediaBody) *MediaUpload {
	return uploadOf(f.project.ID, traceID, "tp-pk-test", body, 0)
}

// capPending lowers the pending-ref cap for one test.
func (f *sweepFixture) capPending(t *testing.T, n int) {
	t.Helper()
	was := maxPendingMediaRefs
	maxPendingMediaRefs = n
	t.Cleanup(func() { maxPendingMediaRefs = was })
}

// MediaRefStates counts one trace's media refs by state: pending, until its
// spans arrive, and settled.
func (s *Store) MediaRefStates(projectID, traceID string) (pending, settled int, err error) {
	err = s.db.QueryRow(`SELECT COALESCE(SUM(pending), 0), COALESCE(SUM(1 - pending), 0)
	   FROM media_refs WHERE project_id = ? AND trace_id = ?`, projectID, traceID).Scan(&pending, &settled)
	return pending, settled, err
}

func rejectedAs(err error, kind string) bool {
	var rejection *Rejection
	return errors.As(err, &rejection) && rejection.Kind == kind
}

// TestMediaUploadDiesWithItsKey: a key revoked between the handler's check and
// the write is refused inside it, and nothing is stored (#28).
func TestMediaUploadDiesWithItsKey(t *testing.T) {
	f := newSweepFixture(t)
	second, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.writer.Submit(t.Context(), &KeyCreate{ProjectID: f.project.ID, Keys: second,
		Origin: KeyOrigin{Via: MintedByAdminToken}}); err != nil {
		t.Fatal(err)
	}
	if err := f.writer.Submit(t.Context(), &KeyRevoke{ProjectID: f.project.ID, PublicKey: second.PublicKey}); err != nil {
		t.Fatal(err)
	}

	body := mediaBody(40, 900)
	job := f.upload(hexTrace(1), body)
	job.Grant.Key = second.PublicKey
	if err := f.writer.Submit(t.Context(), job); !rejectedAs(err, RejectForbidden) {
		t.Fatalf("an upload for a revoked key = %v, want a forbidden rejection", err)
	}
	if f.mediaRows(t) != 0 {
		t.Error("the revoked key's upload stored its body")
	}
	// The key that is still the project's uploads.
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(1), body)); err != nil {
		t.Fatalf("an upload for a live key = %v", err)
	}
}

// TestMediaDeletionVoidsItsTraces: a trace deletion and an erasure void the
// uploads of the traces they remove, in their own transactions — an upload
// for one of them is refused in the write and nothing is stored — and no
// other trace's. A removal of nothing voids nothing (#29).
func TestMediaDeletionVoidsItsTraces(t *testing.T) {
	f := newSweepFixture(t)
	voided := func() int64 {
		return f.count(t, `SELECT COUNT(*) FROM media_voided WHERE project_id = ?`, f.project.ID)
	}
	if err := f.writer.Submit(t.Context(), &UserDataErase{ProjectID: f.project.ID, UserID: "nobody",
		Confirm: "nobody", Limit: 500}); err != nil {
		t.Fatal(err)
	}
	if voided() != 0 {
		t.Error("an erasure that removed nothing voided uploads")
	}

	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1))
	if err := f.writer.Submit(t.Context(), &TraceDelete{ProjectID: f.project.ID, IDs: []string{hexTrace(1)},
		Confirm: hexTrace(1)}); err != nil {
		t.Fatal(err)
	}
	body := mediaBody(41, 700)
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(1), body)); !rejectedAs(err, RejectForbidden) {
		t.Fatalf("an upload for the deleted trace = %v, want a forbidden rejection", err)
	}
	if f.mediaRows(t) != 0 {
		t.Error("a voided upload stored its body")
	}
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(2), body)); err != nil {
		t.Fatalf("an upload for another trace = %v", err)
	}

	f.arrive(t, f.project.ID, hexTrace(3), daysAgo(1))
	if err := f.writer.Submit(t.Context(), &UserDataErase{ProjectID: f.project.ID, UserID: "u1",
		Confirm: "u1", Limit: 500}); err != nil {
		t.Fatal(err)
	}
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(3), mediaBody(42, 700))); !rejectedAs(err, RejectForbidden) {
		t.Fatalf("an upload for the erased trace = %v, want a forbidden rejection", err)
	}
	if voided() != 2 {
		t.Errorf("voided traces = %d, want the deleted one and the erased one", voided())
	}
}

// TestMediaNamingAHashDoesNotExtendIt: X is uploaded for a trace that never
// comes; a day later, less a minute, the project asks for X for another trace
// that is not here and is answered without an upload — which writes a ref as
// old as the bytes, not a fresh one. The sweep past the upload's grace
// collects X. On the old rule the second ask wrote a fresh pending ref and
// kept X for another day (#30).
func TestMediaNamingAHashDoesNotExtendIt(t *testing.T) {
	f := newSweepFixture(t)
	x := mediaBody(42, 1200)
	t0 := sweepNow.Add(-MediaOrphanGrace - time.Minute).UnixNano()
	job := f.upload(hexTrace(1), x)
	job.Now = t0
	if err := f.writer.Submit(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	add := &MediaRefAdd{ProjectID: f.project.ID, SHA256: x.SHA256, TraceID: hexTrace(2),
		Now: t0 + int64(23*time.Hour)}
	if err := f.writer.Submit(t.Context(), add); err != nil || !add.Held {
		t.Fatalf("the null answer = %v, %v", add.Held, err)
	}
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if file, _ := f.store.MediaFor(f.project.ID, x.SHA256); file != nil || f.mediaRows(t) != 0 {
		t.Error("naming the hash again kept a body no trace claims past its grace")
	}
}

// TestMediaNamingAHashDailyDoesNotExtendIt: X is uploaded once, for a trace
// that never comes; its hash is named every day after for a new trace that
// never comes either. Each null answer writes a pending ref dated as the
// upload, or an hour inside the grace when that is later — never as the ask —
// so the sweep a day past the upload collects X though the last ask was two
// hours ago (#30).
func TestMediaNamingAHashDailyDoesNotExtendIt(t *testing.T) {
	f := newSweepFixture(t)
	x := mediaBody(43, 1300)
	t0 := daysAgo(4)
	job := f.upload(hexTrace(1), x)
	job.Now = t0
	if err := f.writer.Submit(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	for i, at := range []int64{daysAgo(3), daysAgo(2), daysAgo(1), sweepNow.Add(-2 * time.Hour).UnixNano()} {
		add := &MediaRefAdd{ProjectID: f.project.ID, SHA256: x.SHA256, TraceID: hexTrace(2 + i), Now: at}
		if err := f.writer.Submit(t.Context(), add); err != nil || !add.Held {
			t.Fatalf("ask %d = %v, %v", i, add.Held, err)
		}
		if got := f.count(t, `SELECT created_at FROM media_refs WHERE sha256 = ? AND trace_id = ? AND pending = 1`,
			x.SHA256, hexTrace(2+i)); got != max(t0, at-int64(MediaOrphanGrace-time.Hour)) {
			t.Fatalf("ask %d wrote a pending ref from %d, want the upload's %d or an hour inside the grace",
				i, got, t0)
		}
	}
	if _, err := f.sweeper.sweepOrphanMedia(t.Context(), sweepNow.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if f.mediaRows(t) != 0 {
		t.Error("naming the hash every day kept a body no trace claims")
	}
}

// TestMediaNullAnswerKeepsTheBody: trace A holds X; the project asks for X for
// trace B, not here yet, and is answered without an upload, so the SDK sends
// no bytes. A is deleted before B's spans come. The null answer's pending ref
// keeps X — for as long as the bytes are younger than the grace — and B's
// spans settle it (#30).
func TestMediaNullAnswerKeepsTheBody(t *testing.T) {
	f := newSweepFixture(t)
	x := mediaBody(44, 1400)
	at := sweepNow.Add(-time.Hour).UnixNano()
	f.arriveWithMedia(t, f.project.ID, hexTrace(1), at, x, false)
	add := &MediaRefAdd{ProjectID: f.project.ID, SHA256: x.SHA256, TraceID: hexTrace(2), Now: sweepNow.UnixNano()}
	if err := f.writer.Submit(t.Context(), add); err != nil || !add.Held {
		t.Fatalf("the null answer = %v, %v", add.Held, err)
	}
	if err := f.writer.Submit(t.Context(), &TraceDelete{ProjectID: f.project.ID, IDs: []string{hexTrace(1)},
		Confirm: hexTrace(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sweeper.sweepOrphanMedia(t.Context(), sweepNow.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if file, _ := f.store.MediaFor(f.project.ID, x.SHA256); file == nil {
		t.Fatal("deleting the trace that held X lost it before B's spans came")
	}

	// B's spans come and name X: the ref settles.
	f.arriveWithMedia(t, f.project.ID, hexTrace(2), sweepNow.UnixNano(), x, false)
	if n := f.count(t, `SELECT COUNT(*) FROM media_refs WHERE trace_id = ? AND pending = 0`, hexTrace(2)); n != 1 {
		t.Errorf("B's settled refs = %d, want 1", n)
	}

	// Had B never come, X goes once its bytes are past the grace.
	g := newSweepFixture(t)
	g.arriveWithMedia(t, g.project.ID, hexTrace(1), at, x, false)
	add = &MediaRefAdd{ProjectID: g.project.ID, SHA256: x.SHA256, TraceID: hexTrace(2), Now: sweepNow.UnixNano()}
	if err := g.writer.Submit(t.Context(), add); err != nil || !add.Held {
		t.Fatalf("the null answer = %v, %v", add.Held, err)
	}
	if err := g.writer.Submit(t.Context(), &TraceDelete{ProjectID: g.project.ID, IDs: []string{hexTrace(1)},
		Confirm: hexTrace(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.sweeper.sweepOrphanMedia(t.Context(), at+int64(MediaOrphanGrace+time.Minute)); err != nil {
		t.Fatal(err)
	}
	if g.mediaRows(t) != 0 {
		t.Error("a null answer's ref kept X past the grace of its bytes")
	}
}

// TestMediaPendingCapInTheWrite: at the cap, an upload or a null answer whose
// ref would be a new pending one is refused inside the write; one for a trace
// the project has is not, because its ref is settled, and neither is one whose
// ref is already there — the SDK's retry of an upload that was stored (#31).
func TestMediaPendingCapInTheWrite(t *testing.T) {
	f := newSweepFixture(t)
	f.capPending(t, 2)
	first := mediaBody(50, 300)
	for i, body := range []MediaBody{first, mediaBody(51, 300)} {
		if err := f.writer.Submit(t.Context(), f.upload(hexTrace(10+i), body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := pendingRoom(f.store.db, f.project.ID, 2); !rejectedAs(err, RejectFull) {
		t.Fatalf("pendingRoom at the cap = %v", err)
	}
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(12), mediaBody(52, 300))); !rejectedAs(err, RejectFull) {
		t.Fatalf("a third pending upload = %v, want a full rejection", err)
	}
	add := &MediaRefAdd{ProjectID: f.project.ID, SHA256: first.SHA256, TraceID: hexTrace(12)}
	if err := f.writer.Submit(t.Context(), add); !rejectedAs(err, RejectFull) {
		t.Fatalf("a null answer for a third pending ref = %v, want a full rejection", err)
	}

	// The retry of the first upload, whose ref is there.
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(10), first)); err != nil {
		t.Fatalf("a repeated upload at the cap = %v", err)
	}
	f.arrive(t, f.project.ID, hexTrace(13), daysAgo(1))
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(13), mediaBody(53, 300))); err != nil {
		t.Fatalf("an upload for a stored trace at the cap = %v", err)
	}
	var plan string
	var id, parent, notused int
	if err := f.store.db.QueryRow(`EXPLAIN QUERY PLAN SELECT 1 FROM media_refs
		  WHERE project_id = ? AND pending = 1 LIMIT ?`, f.project.ID, 2).Scan(&id, &parent, &notused, &plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "idx_media_refs_project_pending") {
		t.Errorf("the cap's count is %q, want a seek on idx_media_refs_project_pending", plan)
	}
}

// TestMediaNullAnswerHoldsAnHour: the project has held X for three days, far
// past the grace, when it asks for X for trace B, not here yet, and is
// answered without an upload. Dated as the bytes, B's ref would be past the
// grace already and the next sweep would drop it — with X, once the trace
// that held it is gone — before B's spans came. It is dated an hour inside
// the grace instead: X outlives a sweep half an hour on and goes with one an
// hour on (#30).
func TestMediaNullAnswerHoldsAnHour(t *testing.T) {
	f := newSweepFixture(t)
	x := mediaBody(45, 1500)
	f.arriveWithMedia(t, f.project.ID, hexTrace(1), daysAgo(3), x, false)
	add := &MediaRefAdd{ProjectID: f.project.ID, SHA256: x.SHA256, TraceID: hexTrace(2), Now: sweepNow.UnixNano()}
	if err := f.writer.Submit(t.Context(), add); err != nil || !add.Held {
		t.Fatalf("the null answer = %v, %v", add.Held, err)
	}
	if err := f.writer.Submit(t.Context(), &TraceDelete{ProjectID: f.project.ID, IDs: []string{hexTrace(1)},
		Confirm: hexTrace(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sweeper.sweepOrphanMedia(t.Context(), sweepNow.Add(30*time.Minute).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if f.mediaRows(t) != 1 {
		t.Fatal("a sweep half an hour after the null answer took the body B's spans were to claim")
	}
	if _, err := f.sweeper.sweepOrphanMedia(t.Context(), sweepNow.Add(61*time.Minute).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if f.mediaRows(t) != 0 {
		t.Error("the null answer kept a body no trace claims past its hour")
	}
}

// TestMediaDeletionInChunks: a deletion in chunks. An upload for a trace
// chunk 1 took, landing before chunk 2, is refused, and nothing is stored; an
// upload for a trace chunk 2 will take, landing before it, is stored and then
// dropped with its trace; an upload for a trace no chunk takes is stored and
// kept. An hour on, the sweep forgets the removed traces, and their uploads
// are taken again (#29).
func TestMediaDeletionInChunks(t *testing.T) {
	f := newSweepFixture(t)
	for i := range 2 {
		f.arrive(t, f.project.ID, hexTrace(20+i), daysAgo(1))
	}
	chunk := func(id string) {
		t.Helper()
		job := &TraceDelete{ProjectID: f.project.ID, IDs: []string{id}, Confirm: f.project.Name,
			ByFilter: true, Now: sweepNow.UnixNano()}
		if err := f.writer.Submit(t.Context(), job); err != nil || job.Counts.Traces != 1 {
			t.Fatalf("chunk for %s = %d traces, %v", id, job.Counts.Traces, err)
		}
	}
	taken, later, kept := mediaBody(60, 500), mediaBody(61, 500), mediaBody(62, 500)
	chunk(hexTrace(20))
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(20), taken)); !rejectedAs(err, RejectForbidden) {
		t.Fatalf("an upload for a trace chunk 1 took = %v, want a forbidden rejection", err)
	}
	for _, job := range []*MediaUpload{f.upload(hexTrace(21), later), f.upload(hexTrace(30), kept)} {
		if err := f.writer.Submit(t.Context(), job); err != nil {
			t.Fatal(err)
		}
	}
	// The null answer for a trace chunk 1 took is refused in the write, as
	// the handler's check would have refused it, and writes no ref.
	add := &MediaRefAdd{ProjectID: f.project.ID, SHA256: kept.SHA256, TraceID: hexTrace(20)}
	if err := f.writer.Submit(t.Context(), add); !errors.Is(err, ErrTraceRemoved) {
		t.Fatalf("the null answer for a deleted trace = %v, %v; want ErrTraceRemoved", add.Held, err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM media_refs WHERE trace_id = ?`, hexTrace(20)); n != 0 {
		t.Errorf("the null answer wrote %d refs for a deleted trace", n)
	}
	chunk(hexTrace(21))
	for name, body := range map[string]MediaBody{"taken": taken, "later": later} {
		if file, _ := f.store.MediaFor(f.project.ID, body.SHA256); file != nil {
			t.Errorf("the %s trace's picture outlived the deletion", name)
		}
	}
	if file, _ := f.store.MediaFor(f.project.ID, kept.SHA256); file == nil {
		t.Error("the picture of a trace no chunk took was lost")
	}

	// The removed traces are remembered for the URL's lifetime and the
	// slack from the chunk that removed them — not from the request's clock,
	// which these chunks set a month back — and not beyond it.
	if err := f.sweeper.sweepVoidedUploads(t.Context(),
		time.Now().Add(MediaUploadWindow+mediaVoidedSlack-10*time.Second).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM media_voided`); n != 2 {
		t.Fatalf("voided traces within the hour and the slack = %d, want 2", n)
	}
	if err := f.sweeper.sweepVoidedUploads(t.Context(),
		time.Now().Add(MediaUploadWindow+mediaVoidedSlack+time.Minute).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM media_voided`); n != 0 {
		t.Errorf("voided traces an hour on = %d, want none", n)
	}
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(20), taken)); err != nil {
		t.Errorf("an upload for the trace an hour after its deletion = %v", err)
	}
}

// TestMediaResentTraceTakesUploads: a trace deleted and sent again under the
// same id within the hour is a trace like any other — seen, and deletable —
// so its upload is stored and the null answer writes its ref settled, as for
// a trace never deleted: the uploads refused are a removed trace's that is
// not here (#29).
func TestMediaResentTraceTakesUploads(t *testing.T) {
	f := newSweepFixture(t)
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1))
	if err := f.writer.Submit(t.Context(), &TraceDelete{ProjectID: f.project.ID, IDs: []string{hexTrace(1)},
		Confirm: hexTrace(1)}); err != nil {
		t.Fatal(err)
	}
	body := mediaBody(70, 800)
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(1), body)); !rejectedAs(err, RejectForbidden) {
		t.Fatalf("an upload for the deleted trace = %v, want a forbidden rejection", err)
	}
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1))
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(1), body)); err != nil {
		t.Fatalf("an upload for the trace sent again = %v", err)
	}
	held := mediaBody(71, 800)
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(2), held)); err != nil {
		t.Fatal(err)
	}
	add := &MediaRefAdd{ProjectID: f.project.ID, SHA256: held.SHA256, TraceID: hexTrace(1)}
	if err := f.writer.Submit(t.Context(), add); err != nil || !add.Held {
		t.Fatalf("the null answer for the trace sent again = %v, %v", add.Held, err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM media_refs WHERE trace_id = ? AND pending = 0`, hexTrace(1)); n != 2 {
		t.Errorf("settled refs of the trace sent again = %d, want its upload's and the null answer's", n)
	}
}

// TestMediaAskForARemovedTrace: the ask for a trace a deletion removed, and
// that is not here, is refused as its upload is, with a refusal that says so
// — a URL issued then would live an hour from the ask, past the removal's
// record. The record outlives the window by the slack, for a URL the ask's
// check let through just before the removal, and voids nothing past it
// whether or not a sweep has run (#29).
func TestMediaAskForARemovedTrace(t *testing.T) {
	f := newSweepFixture(t)
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1))
	if err := f.writer.Submit(t.Context(), &TraceDelete{ProjectID: f.project.ID, IDs: []string{hexTrace(1)},
		Confirm: hexTrace(1)}); err != nil {
		t.Fatal(err)
	}
	body := mediaBody(80, 400)
	ask := func() error { return f.store.MediaUploadRoom(t.Context(), f.project.ID, body.SHA256, hexTrace(1)) }
	if err := ask(); !errors.Is(err, ErrTraceRemoved) {
		t.Fatalf("the ask for a deleted trace = %v, want ErrTraceRemoved", err)
	}
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(1), body)); !errors.Is(err, ErrTraceRemoved) {
		t.Fatalf("the upload for a deleted trace = %v, want ErrTraceRemoved", err)
	}
	if err := f.store.MediaUploadRoom(t.Context(), f.project.ID, body.SHA256, hexTrace(2)); err != nil {
		t.Fatalf("the ask for another trace = %v", err)
	}
	// Half a minute past the window, a URL the check let through before
	// the removal may still be good: the record stays.
	if err := f.sweeper.sweepVoidedUploads(t.Context(),
		time.Now().Add(MediaUploadWindow+30*time.Second).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if err := ask(); !errors.Is(err, ErrTraceRemoved) {
		t.Fatalf("the ask half a minute past the window = %v, want ErrTraceRemoved", err)
	}
	// Past the window and the slack, and no sweep since: the row is still
	// there, and voids nothing.
	if _, err := f.store.db.Exec(`UPDATE media_voided SET at = at - ?`,
		int64(MediaUploadWindow+mediaVoidedSlack+time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := ask(); err != nil {
		t.Errorf("the ask an hour and a minute after the removal, before any sweep = %v", err)
	}
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(1), body)); err != nil {
		t.Errorf("the upload an hour and a minute after the removal, before any sweep = %v", err)
	}
}

// TestMediaTraceSettlesItsRefs: an upload for a trace not here is pending;
// the trace arrives with spans that name no picture, and its ref is settled
// by the arrival, not by the day's sweep — so it leaves the cap, and the next
// trace's upload has room (#31, Decision 13).
func TestMediaTraceSettlesItsRefs(t *testing.T) {
	f := newSweepFixture(t)
	f.capPending(t, 1)
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(1), mediaBody(81, 400))); err != nil {
		t.Fatal(err)
	}
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(2), mediaBody(82, 400))); !rejectedAs(err, RejectFull) {
		t.Fatalf("a second pending upload under a cap of one = %v, want a full rejection", err)
	}
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1))
	if pending, settled, err := f.store.MediaRefStates(f.project.ID, hexTrace(1)); err != nil ||
		pending != 0 || settled != 1 {
		t.Fatalf("the arrived trace's refs = %d pending, %d settled (%v), want its one settled",
			pending, settled, err)
	}
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(2), mediaBody(82, 400))); err != nil {
		t.Fatalf("the next trace's upload once the first arrived = %v", err)
	}
	var plan string
	var id, parent, notused int
	if err := f.store.db.QueryRow(`EXPLAIN QUERY PLAN UPDATE media_refs SET pending = 0
		  WHERE project_id = ? AND trace_id = ? AND pending = 1`, f.project.ID, hexTrace(1)).
		Scan(&id, &parent, &notused, &plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "trace_id=?") {
		t.Errorf("the arrival's settle is %q, want a seek on the project and the trace", plan)
	}
}

// TestMediaNullAnswerHoldsItsOwnRef: an upload for trace T, whose spans are
// late, wrote a pending ref a day ago less a minute; the SDK names the same
// picture for T again and is answered without an upload. The ref takes the
// hour a new one would, so a sweep half an hour on keeps the body the spans
// are to claim, and one an hour on takes it (#30).
func TestMediaNullAnswerHoldsItsOwnRef(t *testing.T) {
	f := newSweepFixture(t)
	x := mediaBody(83, 1500)
	upload := uploadOf(f.project.ID, hexTrace(1), "tp-pk-test", x,
		sweepNow.Add(-MediaOrphanGrace+time.Minute).UnixNano())
	if err := f.writer.Submit(t.Context(), upload); err != nil {
		t.Fatal(err)
	}
	add := &MediaRefAdd{ProjectID: f.project.ID, SHA256: x.SHA256, TraceID: hexTrace(1), Now: sweepNow.UnixNano()}
	if err := f.writer.Submit(t.Context(), add); err != nil || !add.Held {
		t.Fatalf("the null answer = %v, %v", add.Held, err)
	}
	if _, err := f.sweeper.sweepOrphanMedia(t.Context(), sweepNow.Add(30*time.Minute).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if f.mediaRows(t) != 1 {
		t.Fatal("a sweep half an hour after the null answer took the body T's spans were to claim")
	}
	if _, err := f.sweeper.sweepOrphanMedia(t.Context(), sweepNow.Add(61*time.Minute).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if f.mediaRows(t) != 0 {
		t.Error("naming the picture again kept a body no trace claims past its hour")
	}
}

// TestMigration0026SettlesStoredTracesRefs: before 0026 a ref whose trace
// arrived without naming its body stayed pending until the day's sweep. The
// migration settles it, so it does not count toward the cap, and leaves a
// ref whose trace has not come pending (#31).
func TestMigration0026SettlesStoredTracesRefs(t *testing.T) {
	path := freshDB(t)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	f := &sweepFixture{store: s}
	if f.project, err = s.CreateProject("test", KeyPair{PublicKey: "tp-pk-test", Secret: "tp-sk-test"}); err != nil {
		t.Fatal(err)
	}
	if f.writer, err = s.NewWriter(quickWrites); err != nil {
		t.Fatal(err)
	}
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1))
	for i, trace := range []string{hexTrace(1), hexTrace(2)} {
		if err := f.writer.Submit(t.Context(), f.upload(trace, mediaBody(byte(84+i), 600))); err != nil {
			t.Fatal(err)
		}
	}
	f.writer.Close()
	// As a store before 0026 left them: the arrived trace's ref still
	// pending, and the schema without 0026.
	for _, statement := range []string{
		`UPDATE media_refs SET pending = 1`,
		`DROP INDEX idx_media_refs_project_pending`,
		`DROP TABLE media_voided`,
		`DELETE FROM schema_migrations WHERE filename = '0026_media_uploads.sql'`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer s.Close()
	for trace, want := range map[string][2]int{hexTrace(1): {0, 1}, hexTrace(2): {1, 0}} {
		pending, settled, err := s.MediaRefStates(f.project.ID, trace)
		if err != nil || pending != want[0] || settled != want[1] {
			t.Errorf("trace %s after 0026: %d pending, %d settled (%v), want %v", trace, pending, settled, err, want)
		}
	}
}

// TestMediaSweepSparesARefTheNullAnswerHeld: the sweep's read lists a pending
// ref past the grace; before its job runs, the SDK names the same picture for
// the same trace and is told the bytes are held, which gives the ref an hour.
// The job re-checks the age in its transaction and keeps the ref — still
// pending, its trace not here — and the body (#30).
func TestMediaSweepSparesARefTheNullAnswerHeld(t *testing.T) {
	f := newSweepFixture(t)
	x := mediaBody(86, 1500)
	upload := uploadOf(f.project.ID, hexTrace(1), "tp-pk-test", x,
		sweepNow.Add(-MediaOrphanGrace-time.Hour).UnixNano())
	if err := f.writer.Submit(t.Context(), upload); err != nil {
		t.Fatal(err)
	}
	before := sweepNow.Add(-MediaOrphanGrace).UnixNano()
	refs, err := f.store.orphanMediaRefs(before, orphanScanLimit)
	if err != nil || len(refs) != 1 {
		t.Fatalf("the sweep's read = %v, %v; want the one ref", refs, err)
	}
	add := &MediaRefAdd{ProjectID: f.project.ID, SHA256: x.SHA256, TraceID: hexTrace(1), Now: sweepNow.UnixNano()}
	if err := f.writer.Submit(t.Context(), add); err != nil || !add.Held {
		t.Fatalf("the null answer = %v, %v", add.Held, err)
	}
	job := &mediaSweep{Refs: refs, Now: sweepNow.UnixNano(), Before: before}
	if err := f.writer.Submit(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	if job.Dropped != 0 || f.mediaRows(t) != 1 {
		t.Fatalf("the sweep dropped %d refs and left %d bodies after the null answer held the ref",
			job.Dropped, f.mediaRows(t))
	}
	if pending, settled, err := f.store.MediaRefStates(f.project.ID, hexTrace(1)); err != nil ||
		pending != 1 || settled != 0 {
		t.Errorf("the held ref = %d pending, %d settled (%v); want it still waiting for its trace",
			pending, settled, err)
	}
}

// TestMediaVoidedSweepOnTheWallClock: the removal is stamped and read on the
// wall clock, and the pass forgets rows by it too, not by its own clock — a
// pass whose clock runs two hours ahead keeps a row that still voids (#29).
func TestMediaVoidedSweepOnTheWallClock(t *testing.T) {
	f := newSweepFixture(t)
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1))
	if err := f.writer.Submit(t.Context(), &TraceDelete{ProjectID: f.project.ID, IDs: []string{hexTrace(1)},
		Confirm: hexTrace(1)}); err != nil {
		t.Fatal(err)
	}
	ahead := f.store.NewSweeper(f.writer, SweepOptions{Now: func() time.Time { return time.Now().Add(2 * time.Hour) }})
	if err := ahead.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.MediaUploadRoom(t.Context(), f.project.ID, mediaBody(87, 400).SHA256, hexTrace(1)); !errors.Is(err, ErrTraceRemoved) {
		t.Errorf("the ask after a pass on a clock two hours ahead = %v, want ErrTraceRemoved", err)
	}
}
