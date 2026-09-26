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

// upload is the job the PUT submits for the fixture's own key.
func (f *sweepFixture) upload(traceID string, body MediaBody, issued int64) *MediaUpload {
	return &MediaUpload{ProjectID: f.project.ID, TraceID: traceID, Body: body,
		Key: "tp-pk-test", Issued: issued, PendingCap: MaxPendingMediaRefs}
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
	job := f.upload(hexTrace(1), body, time.Now().UnixNano())
	job.Key = second.PublicKey
	if err := f.writer.Submit(t.Context(), job); !rejectedAs(err, RejectForbidden) {
		t.Fatalf("an upload for a revoked key = %v, want a forbidden rejection", err)
	}
	if f.mediaRows(t) != 0 {
		t.Error("the revoked key's upload stored its body")
	}
	// The key that is still the project's uploads.
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(1), body, time.Now().UnixNano())); err != nil {
		t.Fatalf("an upload for a live key = %v", err)
	}
}

// TestMediaDeletionVoidsEarlierGrants: every trace deletion and erasure moves
// the project's watermark inside its transaction, a grant issued at or before
// it is refused in the write, and one issued after uploads. A deletion that
// removed nothing leaves the watermark alone (#29).
func TestMediaDeletionVoidsEarlierGrants(t *testing.T) {
	f := newSweepFixture(t)
	watermark := func() int64 {
		project, err := f.store.ProjectByID(t.Context(), f.project.ID)
		if err != nil {
			t.Fatal(err)
		}
		return project.MediaGrantsAfter
	}
	if watermark() != 0 {
		t.Fatal("a fresh project voids grants")
	}

	// An erasure of a user with no traces removes nothing.
	if err := f.writer.Submit(t.Context(), &UserDataErase{ProjectID: f.project.ID, UserID: "nobody",
		Confirm: "nobody", Limit: 500}); err != nil {
		t.Fatal(err)
	}
	if watermark() != 0 {
		t.Error("an erasure that removed nothing moved the watermark")
	}

	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1))
	before := time.Now().UnixNano()
	if err := f.writer.Submit(t.Context(), &TraceDelete{ProjectID: f.project.ID, IDs: []string{hexTrace(1)},
		Confirm: hexTrace(1)}); err != nil {
		t.Fatal(err)
	}
	moved := watermark()
	if moved < before {
		t.Fatalf("the watermark after a deletion = %d, want at least %d", moved, before)
	}
	body := mediaBody(41, 700)
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(1), body, moved)); !rejectedAs(err, RejectForbidden) {
		t.Fatalf("a grant issued at the deletion = %v, want a forbidden rejection", err)
	}
	if f.mediaRows(t) != 0 {
		t.Error("a voided grant stored its body")
	}
	if err := f.writer.Submit(t.Context(), f.upload(hexTrace(1), body, moved+1)); err != nil {
		t.Fatalf("a grant issued after the deletion = %v", err)
	}

	// An erasure moves it too.
	f.arrive(t, f.project.ID, hexTrace(2), daysAgo(1))
	if err := f.writer.Submit(t.Context(), &UserDataErase{ProjectID: f.project.ID, UserID: "u1",
		Confirm: "u1", Limit: 500}); err != nil {
		t.Fatal(err)
	}
	if watermark() <= moved {
		t.Error("an erasure did not move the watermark")
	}
}

// TestMediaNamingAHashDoesNotExtendIt: X is uploaded for a trace that never
// comes; a day later, less a minute, the project asks for X for another trace
// that is not here and is answered without an upload — which writes nothing.
// The sweep past the upload's grace collects X. On the old rule the second
// ask wrote a fresh pending ref and kept X for another day (#30).
func TestMediaNamingAHashDoesNotExtendIt(t *testing.T) {
	f := newSweepFixture(t)
	x := mediaBody(42, 1200)
	t0 := sweepNow.Add(-MediaOrphanGrace - time.Minute).UnixNano()
	job := f.upload(hexTrace(1), x, 1)
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

// TestMediaPendingCapInTheWrite: at the cap, an upload whose ref would be
// pending is refused inside the write; one for a trace the project has is
// not, because its ref is settled (#31).
func TestMediaPendingCapInTheWrite(t *testing.T) {
	f := newSweepFixture(t)
	for i := range 2 {
		job := f.upload(hexTrace(10+i), mediaBody(byte(50+i), 300), 1)
		job.PendingCap = 2
		if err := f.writer.Submit(t.Context(), job); err != nil {
			t.Fatal(err)
		}
	}
	full, err := pendingFull(f.store.db, f.project.ID, 2)
	if err != nil || !full {
		t.Fatalf("pendingFull = %v, %v", full, err)
	}
	job := f.upload(hexTrace(12), mediaBody(52, 300), 1)
	job.PendingCap = 2
	if err := f.writer.Submit(t.Context(), job); !rejectedAs(err, RejectFull) {
		t.Fatalf("a third pending upload = %v, want a full rejection", err)
	}

	f.arrive(t, f.project.ID, hexTrace(13), daysAgo(1))
	job = f.upload(hexTrace(13), mediaBody(53, 300), 1)
	job.PendingCap = 2
	if err := f.writer.Submit(t.Context(), job); err != nil {
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
