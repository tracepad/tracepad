package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
)

// Media follows its traces (spec 041, Testing — deletion): every path that
// deletes traces or raw batches deletes their refs in the same transaction and
// collects a body nothing points at any more — and only such a body.

func mediaBody(seed byte, n int) MediaBody {
	body := make([]byte, n)
	for i := range body {
		body[i] = byte(i) ^ seed
	}
	sum := sha256.Sum256(body)
	return MediaBody{SHA256: hex.EncodeToString(sum[:]), MimeType: "image/png", Body: body}
}

// arriveWithMedia writes one trace pointing at a body, the way ingest does,
// and a raw batch pointing at it too when raw is set.
func (f *sweepFixture) arriveWithMedia(t *testing.T, projectID, traceID string, at int64, body MediaBody, raw bool) {
	t.Helper()
	ref := map[string]any{"tracepad_media": body.SHA256, "mime_type": body.MimeType, "size": len(body.Body)}
	batch := &IngestBatch{
		ProjectID:  projectID,
		IngestedAt: at,
		Traces:     []*model.Trace{{ID: traceID, UserID: "u1"}},
		Observations: []*model.Observation{{
			TraceID: traceID, ID: traceID[:16], Type: model.TypeGeneration, Level: model.LevelDefault,
			StartTime: at, EndTime: at + 1_000_000, Input: []any{ref},
		}},
		Media:     []MediaBody{body},
		MediaRefs: []MediaRef{{SHA256: body.SHA256, TraceID: traceID}},
	}
	if raw {
		batch.Raw = &RawBatch{ReceivedAt: at, Body: []byte("factored body")}
		batch.RawMedia = []string{body.SHA256}
	}
	if err := f.writer.Submit(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
}

func (f *sweepFixture) mediaRows(t *testing.T) int64 {
	return f.count(t, `SELECT COUNT(*) FROM media`)
}

func (f *sweepFixture) secondProject(t *testing.T, name string) *Project {
	t.Helper()
	p, err := f.store.CreateProject(name, KeyPair{PublicKey: "tp-pk-" + name, Secret: "tp-sk-" + name})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// One body, two traces: the body survives the first deletion and goes with
// the second, and the previews say which of the two would free it.
func TestMediaFollowsItsTraces(t *testing.T) {
	f := newSweepFixture(t)
	body := mediaBody(1, 5000)
	f.arriveWithMedia(t, f.project.ID, hexTrace(1), daysAgo(2), body, false)
	f.arriveWithMedia(t, f.project.ID, hexTrace(2), daysAgo(1), body, false)
	if got := f.mediaRows(t); got != 1 {
		t.Fatalf("%d media rows, want one for the shared body", got)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM media_refs`); got != 2 {
		t.Fatalf("%d refs, want one per trace", got)
	}

	counts, _, err := f.store.TracePreview(f.project.ID, hexTrace(1))
	if err != nil {
		t.Fatal(err)
	}
	if counts.Media != 0 {
		t.Errorf("preview of the first trace frees %d bodies; the second still points at it", counts.Media)
	}
	del := &TraceDelete{ProjectID: f.project.ID, IDs: []string{hexTrace(1)}, Confirm: hexTrace(1)}
	if err := f.writer.Submit(t.Context(), del); err != nil {
		t.Fatal(err)
	}
	if del.Counts.Media != 0 || f.mediaRows(t) != 1 {
		t.Fatalf("deleting one of two traces collected the body: %+v", del.Counts)
	}

	counts, _, err = f.store.TracePreview(f.project.ID, hexTrace(2))
	if err != nil {
		t.Fatal(err)
	}
	if counts.Media != 1 || counts.MediaBytes != 5000 {
		t.Errorf("preview of the last trace = %d bodies, %d bytes; want 1 and 5000", counts.Media, counts.MediaBytes)
	}
	del = &TraceDelete{ProjectID: f.project.ID, IDs: []string{hexTrace(2)}, Confirm: hexTrace(2)}
	if err := f.writer.Submit(t.Context(), del); err != nil {
		t.Fatal(err)
	}
	if del.Counts.Media != 1 || del.Counts.MediaBytes != 5000 {
		t.Errorf("deleted = %+v, want the body and its bytes", del.Counts)
	}
	if got := f.mediaRows(t) + f.count(t, `SELECT COUNT(*) FROM media_refs`); got != 0 {
		t.Errorf("%d media rows and refs left after the last trace went", got)
	}
}

// The retention sweep takes the refs with the traces; a raw batch with a
// longer window keeps the body until it goes too (Decision 12).
func TestMediaRetentionAndRawWindow(t *testing.T) {
	f := newSweepFixture(t)
	body := mediaBody(2, 6000)
	f.arriveWithMedia(t, f.project.ID, hexTrace(1), daysAgo(40), body, true)
	thirty, sixty := 30, 60
	f.setRetention(t, f.project.ID, &thirty, &sixty)

	preview, err := f.store.RetentionPreview(f.project.ID, &thirty, &sixty, nil, sweepNow.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if preview.Traces != 1 || preview.Media != 0 {
		t.Errorf("preview = %+v; the raw batch still holds the body, so none is freed", preview)
	}
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `SELECT COUNT(*) FROM traces`) != 0 || f.count(t, `SELECT COUNT(*) FROM media_refs`) != 0 {
		t.Fatal("the expired trace or its ref survived the sweep")
	}
	if f.mediaRows(t) != 1 {
		t.Fatal("the body went while the raw batch that points at it is inside its window")
	}
	// The project can still read it: the raw batch is its ref.
	if file, err := f.store.MediaFor(f.project.ID, body.SHA256); err != nil || file == nil {
		t.Fatalf("a body held by a raw batch is not readable: %v", err)
	}

	ten := 10
	f.setRetention(t, f.project.ID, &thirty, &ten)
	preview, err = f.store.RetentionPreview(f.project.ID, &thirty, &ten, nil, sweepNow.UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if preview.RawBatches != 1 || preview.Media != 1 {
		t.Errorf("preview = %+v, want the raw batch and the body it frees", preview)
	}
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := f.mediaRows(t) + f.count(t, `SELECT COUNT(*) FROM media_raw_refs`); got != 0 {
		t.Errorf("%d media rows and raw refs left after the raw batch went", got)
	}
}

// Erasure deletes the user's refs and collects what only they pointed at.
func TestMediaErasure(t *testing.T) {
	f := newSweepFixture(t)
	body := mediaBody(3, 4096)
	f.arriveWithMedia(t, f.project.ID, hexTrace(1), daysAgo(1), body, false)
	counts, _, err := f.store.UserDataPreview(f.project.ID, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if counts.Media != 1 {
		t.Errorf("erasure preview = %+v, want the body named", counts)
	}
	erase := &UserDataErase{ProjectID: f.project.ID, UserID: "u1", Confirm: "u1", Limit: 500}
	if err := f.writer.Submit(t.Context(), erase); err != nil {
		t.Fatal(err)
	}
	if erase.Counts.Media != 1 || f.mediaRows(t) != 0 {
		t.Errorf("erased = %+v, %d media rows left", erase.Counts, f.mediaRows(t))
	}
}

// A body two projects point at survives one project's purge, and the other
// project can still read it; the purged project's leftover refs — one naming
// a trace that never arrived — go with it.
func TestMediaSharedAcrossAPurge(t *testing.T) {
	f := newSweepFixture(t)
	other := f.secondProject(t, "other")
	shared := mediaBody(4, 8000)
	own := mediaBody(5, 4500)
	f.arriveWithMedia(t, f.project.ID, hexTrace(1), daysAgo(3), shared, true)
	f.arriveWithMedia(t, other.ID, hexTrace(2), daysAgo(3), shared, false)
	if err := f.writer.Submit(t.Context(),
		uploadOf(f.project.ID, hexTrace(9), "tp-pk-test", own, sweepNow.UnixNano())); err != nil {
		t.Fatal(err)
	}

	preview, err := f.store.ProjectPreview(f.project.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Both bodies: the preview counts what the project stops holding, the
	// shared one included, not what leaves the disk (Decision 27).
	if preview.Media != 2 || preview.MediaBytes != 12500 {
		t.Errorf("project preview = %d bodies, %d bytes; want both it holds", preview.Media, preview.MediaBytes)
	}

	if _, err := f.store.db.Exec(`UPDATE projects SET deleted_at = ? WHERE id = ?`, daysAgo(10), f.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.store.ProjectByID(context.Background(), f.project.ID); p != nil {
		t.Fatal("the project was not purged")
	}
	if got := f.mediaRows(t); got != 1 {
		t.Fatalf("%d media rows after the purge, want the shared one", got)
	}
	if file, err := f.store.MediaFor(other.ID, shared.SHA256); err != nil || file == nil {
		t.Fatalf("the other project lost the shared body: %v", err)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM media_refs WHERE project_id = ?`, f.project.ID); got != 0 {
		t.Errorf("%d refs of the purged project survive", got)
	}
}

// A hash is not a capability (#7): a project reads a body only through a ref
// of its own, and a Langfuse id resolves the same way.
func TestMediaScopedByRef(t *testing.T) {
	f := newSweepFixture(t)
	other := f.secondProject(t, "other")
	body := mediaBody(6, 5000)
	f.arriveWithMedia(t, f.project.ID, hexTrace(1), daysAgo(1), body, false)

	if file, err := f.store.MediaFor(f.project.ID, body.SHA256); err != nil || file == nil ||
		file.MimeType != "image/png" || len(file.Body) != 5000 {
		t.Fatalf("own body = %+v, %v", file, err)
	}
	if file, err := f.store.MediaFor(other.ID, body.SHA256); err != nil || file != nil {
		t.Fatalf("another project read the body: %+v, %v", file, err)
	}
	id := MediaIDFor(body.SHA256)
	if len(id) != 22 {
		t.Fatalf("media id %q is not 22 characters", id)
	}
	if info, err := f.store.MediaByLangfuseID(f.project.ID, id); err != nil || info == nil || info.SHA256 != body.SHA256 {
		t.Fatalf("own Langfuse id resolved to %+v, %v", info, err)
	}
	if info, err := f.store.MediaByLangfuseID(other.ID, id); err != nil || info != nil {
		t.Fatalf("another project resolved the Langfuse id: %+v, %v", info, err)
	}
	summary, err := f.store.MediaSummary(f.project.ID)
	if err != nil || summary.Count != 1 || summary.Bytes != 5000 {
		t.Errorf("summary = %+v, %v", summary, err)
	}
}

// A ref the Langfuse channel wrote for a trace that never arrived is taken
// after the grace, with its body; one inside the grace stays; one whose trace
// did arrive is settled and kept (Decision 13). The hourly look reads only
// pending refs, through their own index, and counts only what it deleted.
func TestMediaOrphanRefs(t *testing.T) {
	f := newSweepFixture(t)
	late := mediaBody(7, 4200)
	fresh := mediaBody(8, 4300)
	settled := mediaBody(11, 4400)
	resolved := mediaBody(12, 4500)
	for _, upload := range []*MediaUpload{
		uploadOf(f.project.ID, hexTrace(1), "tp-pk-test", late, daysAgo(3)),
		uploadOf(f.project.ID, hexTrace(2), "tp-pk-test", fresh, sweepNow.UnixNano()),
		uploadOf(f.project.ID, hexTrace(3), "tp-pk-test", settled, daysAgo(3)),
		uploadOf(f.project.ID, hexTrace(4), "tp-pk-test", resolved, daysAgo(3)),
	} {
		if err := f.writer.Submit(t.Context(), upload); err != nil {
			t.Fatal(err)
		}
	}
	// Trace 3 arrives carrying something else (its span overtook the
	// upload); trace 4 arrives with the upload resolved, which settles the
	// ref at ingest.
	f.arriveWithMedia(t, f.project.ID, hexTrace(3), daysAgo(2), mediaBody(13, 4096), false)
	f.arriveWithMedia(t, f.project.ID, hexTrace(4), daysAgo(2), resolved, false)
	if got := f.count(t, `SELECT COUNT(*) FROM media_refs WHERE pending = 1`); got != 3 {
		t.Fatalf("%d pending refs before the sweep, want 3", got)
	}

	var plan string
	var id, parent, notused int
	if err := f.store.db.QueryRow(`EXPLAIN QUERY PLAN SELECT sha256, project_id, trace_id FROM media_refs
		  WHERE pending = 1 AND created_at < ? LIMIT ?`, 0, 1).Scan(&id, &parent, &notused, &plan); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "idx_media_refs_pending") {
		t.Errorf("the orphan look is %q, want a seek on idx_media_refs_pending", plan)
	}

	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if file, _ := f.store.MediaFor(f.project.ID, late.SHA256); file != nil {
		t.Error("the ref whose trace never came outlived the grace")
	}
	for name, body := range map[string]MediaBody{"inside the grace": fresh, "settled": settled, "resolved": resolved} {
		if file, _ := f.store.MediaFor(f.project.ID, body.SHA256); file == nil {
			t.Errorf("the ref %s was taken", name)
		}
	}
	if got := f.count(t, `SELECT COUNT(*) FROM media_refs WHERE pending = 1`); got != 1 {
		t.Errorf("%d pending refs after the sweep, want only the fresh one", got)
	}
	if got := f.mediaRows(t); got != 4 {
		t.Errorf("%d media rows, want all but the late one", got)
	}
	if freed, err := f.sweeper.sweepOrphanMedia(t.Context(), sweepNow.UnixNano()); err != nil || freed != 0 {
		t.Errorf("a second look freed %d (%v), want nothing", freed, err)
	}
}

// A second identical upload for another trace asks for no bytes when the body
// is there, and for the bytes when it is not. The ref is written settled for a
// trace the project has, and pending for a trace not here yet — as old as the
// bytes are in the project, not as old as the ask (#30).
func TestMediaRefAdd(t *testing.T) {
	f := newSweepFixture(t)
	body := mediaBody(9, 4096)
	add := &MediaRefAdd{ProjectID: f.project.ID, SHA256: body.SHA256, TraceID: hexTrace(1)}
	if err := f.writer.Submit(t.Context(), add); err != nil {
		t.Fatal(err)
	}
	if add.Held {
		t.Fatal("a body that does not exist is held")
	}
	f.arriveWithMedia(t, f.project.ID, hexTrace(2), daysAgo(1), body, false)
	delivered := f.count(t, `SELECT MAX(created_at) FROM media_refs WHERE sha256 = ?`, body.SHA256)

	// Half a day after the delivery: well inside the grace, so the date is
	// the delivery's.
	add = &MediaRefAdd{ProjectID: f.project.ID, SHA256: body.SHA256, TraceID: hexTrace(1),
		Now: daysAgo(1) + int64(12*time.Hour)}
	if err := f.writer.Submit(t.Context(), add); err != nil || !add.Held {
		t.Fatalf("the null answer for a trace not here = %v, %v", add.Held, err)
	}
	if got := f.count(t, `SELECT created_at FROM media_refs WHERE sha256 = ? AND trace_id = ? AND pending = 1`,
		body.SHA256, hexTrace(1)); got != delivered {
		t.Errorf("the pending ref for a trace not here is from %d, want the delivery's %d", got, delivered)
	}

	f.arriveWithMedia(t, f.project.ID, hexTrace(3), daysAgo(1), mediaBody(10, 100), false)
	add = &MediaRefAdd{ProjectID: f.project.ID, SHA256: body.SHA256, TraceID: hexTrace(3)}
	if err := f.writer.Submit(t.Context(), add); err != nil || !add.Held {
		t.Fatalf("the null answer for a stored trace = %v, %v", add.Held, err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM media_refs WHERE sha256 = '`+body.SHA256+`'
	                    AND trace_id = '`+hexTrace(3)+`' AND pending = 0`); n != 1 {
		t.Errorf("settled refs for the stored trace = %d, want 1", n)
	}
	// Idempotent: the SDK retries.
	if err := f.writer.Submit(t.Context(), add); err != nil || !add.Held {
		t.Fatalf("a repeated ref = %v, %v", add.Held, err)
	}
}

// The first stored type wins (spec 041, edge cases).
func TestMediaFirstTypeWins(t *testing.T) {
	f := newSweepFixture(t)
	body := mediaBody(10, 4096)
	f.arriveWithMedia(t, f.project.ID, hexTrace(1), daysAgo(1), body, false)
	body.MimeType = "application/octet-stream"
	f.arriveWithMedia(t, f.project.ID, hexTrace(2), daysAgo(1), body, false)
	file, err := f.store.MediaFor(f.project.ID, body.SHA256)
	if err != nil || file == nil || file.MimeType != "image/png" {
		t.Fatalf("stored type = %+v, %v; want the first", file, err)
	}
}

// A batch whose Langfuse strings were resolved to a body that a deletion
// collected before the write is refused whole, so the caller can take it
// again with the strings kept (spec 041 #9).
func TestMediaResolvedBodyGone(t *testing.T) {
	f := newSweepFixture(t)
	gone := mediaBody(14, 4096)
	batch := &IngestBatch{
		ProjectID: f.project.ID,
		Traces:    []*model.Trace{{ID: hexTrace(1)}},
		MediaRefs: []MediaRef{{SHA256: gone.SHA256, TraceID: hexTrace(1)}},
		Resolved:  []string{gone.SHA256},
	}
	if err := f.writer.Submit(t.Context(), batch); !errors.Is(err, ErrMediaGone) {
		t.Fatalf("submit = %v, want ErrMediaGone", err)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces WHERE project_id = ?`, f.project.ID); got != 0 {
		t.Errorf("%d traces written by a refused batch", got)
	}
}

// The look for bodies no ref names reads one page per pass and goes on from
// where it stopped, wrapping at the end: its cost is a page, not the table.
func TestMediaOrphanBodiesPaged(t *testing.T) {
	f := newSweepFixture(t)
	held := mediaBody(15, 4096)
	f.arriveWithMedia(t, f.project.ID, hexTrace(1), daysAgo(1), held, false)
	var loose []string
	for seed := byte(16); seed < 19; seed++ {
		body := mediaBody(seed, 4096)
		if _, err := f.store.db.Exec(`INSERT INTO media (sha256, mime_type, size, body, created_at) VALUES (?, ?, ?, ?, 0)`,
			body.SHA256, body.MimeType, len(body.Body), body.Body); err != nil {
			t.Fatal(err)
		}
		loose = append(loose, body.SHA256)
	}
	found := map[string]bool{}
	cursor, passes := "", 0
	for {
		orphans, next, err := f.store.orphanMedia(cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, sha := range orphans {
			found[sha.(string)] = true
		}
		passes++
		if cursor = next; cursor == "" {
			break
		}
	}
	// Two full pages, then an empty one that wraps the cursor.
	if passes != 3 {
		t.Errorf("four bodies in pages of two took %d passes, want 3", passes)
	}
	if len(found) != len(loose) || found[held.SHA256] {
		t.Errorf("orphans = %v, want the three loose bodies and not the held one", found)
	}
}
