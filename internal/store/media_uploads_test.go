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

// uploadOf is the job the PUT submits for a grant a project's key asked for in
// the project's first generation.
func uploadOf(projectID, traceID, key string, body MediaBody, now int64) *MediaUpload {
	return &MediaUpload{Grant: MediaGrant{ProjectID: projectID, TraceID: traceID, SHA256: body.SHA256, Key: key},
		Body: body, Now: now}
}

// upload is the fixture's own key's job, in a given generation.
func (f *sweepFixture) upload(traceID string, body MediaBody, generation int64) *MediaUpload {
	job := uploadOf(f.project.ID, traceID, "tp-pk-test", body, 0)
	job.Grant.Generation = generation
	return job
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
	job := f.upload(hexTrace(1), body, 0)
	job.Grant.Key = second.PublicKey
	if err := f.writer.Submit(t.Context(), job); !rejectedAs(err, RejectForbidden) {
		t.Fatalf("an upload for a revoked key = %v, want a forbidden rejection", err)
	}
	if f.mediaRows(t) != 0 {
		t.Error("the revoked key's upload stored its body")
	}
	// The key that is still the project's uploads.
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(1), body, 0)); err != nil {
		t.Fatalf("an upload for a live key = %v", err)
	}
}

// TestMediaDeletionVoidsEarlierGrants: every trace deletion and erasure starts
// a new generation inside its transaction, a grant from an earlier one is
// refused in the write, and one from the current one uploads. A deletion that
// removed nothing leaves the generation alone (#29). A counter, so no clock
// takes part: a deletion stamped in the past voids as much as one stamped now.
func TestMediaDeletionVoidsEarlierGrants(t *testing.T) {
	f := newSweepFixture(t)
	generation := func() int64 {
		project, err := f.store.ProjectByID(t.Context(), f.project.ID)
		if err != nil {
			t.Fatal(err)
		}
		return project.MediaGeneration
	}
	if generation() != 0 {
		t.Fatal("a fresh project starts past its first generation")
	}

	// An erasure of a user with no traces removes nothing.
	if err := f.writer.Submit(t.Context(), &UserDataErase{ProjectID: f.project.ID, UserID: "nobody",
		Confirm: "nobody", Limit: 500}); err != nil {
		t.Fatal(err)
	}
	if generation() != 0 {
		t.Error("an erasure that removed nothing started a generation")
	}

	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1))
	if err := f.writer.Submit(t.Context(), &TraceDelete{ProjectID: f.project.ID, IDs: []string{hexTrace(1)},
		Confirm: hexTrace(1), Now: daysAgo(30)}); err != nil {
		t.Fatal(err)
	}
	if generation() != 1 {
		t.Fatalf("the generation after a deletion = %d, want 1", generation())
	}
	body := mediaBody(41, 700)
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(1), body, 0)); !rejectedAs(err, RejectForbidden) {
		t.Fatalf("a grant from before the deletion = %v, want a forbidden rejection", err)
	}
	if f.mediaRows(t) != 0 {
		t.Error("a voided grant stored its body")
	}
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(1), body, 1)); err != nil {
		t.Fatalf("a grant from after the deletion = %v", err)
	}

	// An erasure starts one too.
	f.arrive(t, f.project.ID, hexTrace(2), daysAgo(1))
	if err := f.writer.Submit(t.Context(), &UserDataErase{ProjectID: f.project.ID, UserID: "u1",
		Confirm: "u1", Limit: 500}); err != nil {
		t.Fatal(err)
	}
	if generation() != 2 {
		t.Errorf("the generation after an erasure = %d, want 2", generation())
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
	job := f.upload(hexTrace(1), x, 0)
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
// never comes either. Each null answer writes a pending ref, and every one of
// them is as old as the upload, so the sweep a day past the upload collects X
// though the last ask was an hour ago (#30).
func TestMediaNamingAHashDailyDoesNotExtendIt(t *testing.T) {
	f := newSweepFixture(t)
	x := mediaBody(43, 1300)
	t0 := daysAgo(4)
	job := f.upload(hexTrace(1), x, 0)
	job.Now = t0
	if err := f.writer.Submit(t.Context(), job); err != nil {
		t.Fatal(err)
	}
	for i, at := range []int64{daysAgo(3), daysAgo(2), daysAgo(1), sweepNow.Add(-time.Hour).UnixNano()} {
		add := &MediaRefAdd{ProjectID: f.project.ID, SHA256: x.SHA256, TraceID: hexTrace(2 + i), Now: at}
		if err := f.writer.Submit(t.Context(), add); err != nil || !add.Held {
			t.Fatalf("ask %d = %v, %v", i, add.Held, err)
		}
		if got := f.count(t, `SELECT created_at FROM media_refs WHERE sha256 = ? AND trace_id = ? AND pending = 1`,
			x.SHA256, hexTrace(2+i)); got != t0 {
			t.Fatalf("ask %d wrote a pending ref from %d, want the upload's %d", i, got, t0)
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
// A job that names no cap has the constant's.
func TestMediaPendingCapInTheWrite(t *testing.T) {
	f := newSweepFixture(t)
	capped := func(job *MediaUpload) *MediaUpload {
		job.PendingCap = 2
		return job
	}
	first := mediaBody(50, 300)
	for i, body := range []MediaBody{first, mediaBody(51, 300)} {
		if err := f.writer.Submit(t.Context(), capped(f.upload(hexTrace(10+i), body, 0))); err != nil {
			t.Fatal(err)
		}
	}
	if err := pendingRoom(f.store.db, f.project.ID, 2); !rejectedAs(err, RejectFull) {
		t.Fatalf("pendingRoom at the cap = %v", err)
	}
	if err := f.writer.Submit(t.Context(), capped(f.upload(hexTrace(12), mediaBody(52, 300), 0))); !rejectedAs(err, RejectFull) {
		t.Fatalf("a third pending upload = %v, want a full rejection", err)
	}
	add := &MediaRefAdd{ProjectID: f.project.ID, SHA256: first.SHA256, TraceID: hexTrace(12), PendingCap: 2}
	if err := f.writer.Submit(t.Context(), add); !rejectedAs(err, RejectFull) {
		t.Fatalf("a null answer for a third pending ref = %v, want a full rejection", err)
	}

	// The retry of the first upload, whose ref is there.
	if err := f.writer.Submit(t.Context(), capped(f.upload(hexTrace(10), first, 0))); err != nil {
		t.Fatalf("a repeated upload at the cap = %v", err)
	}
	f.arrive(t, f.project.ID, hexTrace(13), daysAgo(1))
	if err := f.writer.Submit(t.Context(), capped(f.upload(hexTrace(13), mediaBody(53, 300), 0))); err != nil {
		t.Fatalf("an upload for a stored trace at the cap = %v", err)
	}
	// No cap named: ten thousand, far off.
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(14), mediaBody(54, 300), 0)); err != nil {
		t.Fatalf("an upload with the default cap = %v", err)
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
