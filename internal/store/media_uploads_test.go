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
	was := f.store.maxPendingMediaRefs
	f.store.maxPendingMediaRefs = n
	t.Cleanup(func() { f.store.maxPendingMediaRefs = was })
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
	// The null answer for a trace chunk 1 took writes no ref either.
	add := &MediaRefAdd{ProjectID: f.project.ID, SHA256: kept.SHA256, TraceID: hexTrace(20)}
	if err := f.writer.Submit(t.Context(), add); err != nil || !add.Held {
		t.Fatalf("the null answer for a deleted trace = %v, %v", add.Held, err)
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

	// The removed traces are remembered for the URL's lifetime, and not
	// beyond it.
	if err := f.sweeper.sweepVoidedUploads(t.Context(), sweepNow.Add(MediaUploadWindow-time.Minute).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM media_voided`); n != 2 {
		t.Fatalf("voided traces within the hour = %d, want 2", n)
	}
	if err := f.sweeper.sweepVoidedUploads(t.Context(), sweepNow.Add(MediaUploadWindow+time.Minute).UnixNano()); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM media_voided`); n != 0 {
		t.Errorf("voided traces an hour on = %d, want none", n)
	}
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(20), taken)); err != nil {
		t.Errorf("an upload for the trace an hour after its deletion = %v", err)
	}
}
