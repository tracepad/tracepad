package server

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// The raw archive is an editor's read (spec 044 #6): the bulk way out of a
// project, every body whole. A viewer session is refused; a key, an editor and
// an owner are not.
func TestTheRawArchiveIsAnEditorRead(t *testing.T) {
	h := newAccountHarness(t)
	if err := h.writer.Submit(t.Context(), &store.IngestBatch{
		ProjectID: h.project.ID,
		Raw:       &store.RawBatch{ReceivedAt: seedBase, Dialect: "otel", Body: []byte("body")},
	}); err != nil {
		t.Fatal(err)
	}
	viewer, editor, owner := h.viewer(t), h.editor(t), h.owner(t)
	for _, path := range []string{"/api/v1/raw", "/api/v1/raw/1"} {
		expectError(t, h.call(t, "GET", path, nil, asSession(viewer), inProject(h.project.ID)),
			http.StatusForbidden, "")
		expectStatus(t, h.call(t, "GET", path, nil), http.StatusOK) // the project key
		expectStatus(t, h.call(t, "GET", path, nil, asSession(editor), inProject(h.project.ID)), http.StatusOK)
		expectStatus(t, h.call(t, "GET", path, nil, asSession(owner), inProject(h.project.ID)), http.StatusOK)
	}
}

// A confirmed erasure says when the pass that overwrites what it unlinked is
// due, and names the one copy of the database it does not reach — the newest
// pre-migration backup — with the day it goes (spec 044 #11, #12).
func TestAnErasureAnswersWithItsCompactionAndTheBackup(t *testing.T) {
	h := newAdminHarness(t)
	h.seed(t, &model.Trace{ID: traceHex(1), UserID: "erase-me"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})
	backup := h.store.Path() + ".pre-0024_compaction.bak"
	written := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	if err := os.WriteFile(backup, []byte("copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(backup, written, written); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/projects/" + h.project.ID + "/users/erase-me/data"

	type backupBlock struct {
		CreatedAt time.Time `json:"created_at"`
		RemovedAt time.Time `json:"removed_at"`
	}
	rec := h.call(t, "DELETE", path, nil)
	expectStatus(t, rec, http.StatusOK)
	preview := decodeJSON[struct {
		Backup *backupBlock `json:"pre_migration_backup"`
	}](t, rec)
	if preview.Backup == nil || !preview.Backup.CreatedAt.Equal(written) ||
		!preview.Backup.RemovedAt.Equal(written.Add(store.BackupLifetime)) {
		t.Errorf("the preview's backup = %+v, want written %v and removed a week later", preview.Backup, written)
	}

	rec = h.call(t, "DELETE", path+"?confirm=erase-me", nil)
	expectStatus(t, rec, http.StatusOK)
	answer := decodeJSON[struct {
		Deleted    map[string]int `json:"deleted"`
		Compaction struct {
			RequestedAt *time.Time `json:"requested_at"`
			ExpectedBy  *time.Time `json:"expected_by"`
		} `json:"compaction"`
		Backup *backupBlock `json:"pre_migration_backup"`
	}](t, rec)
	if answer.Deleted["traces"] != 1 {
		t.Fatalf("deleted = %v, want the one trace", answer.Deleted)
	}
	next := time.Unix(0, h.sweeper.Status("").NextRun)
	if answer.Compaction.RequestedAt == nil || answer.Compaction.ExpectedBy == nil ||
		!answer.Compaction.ExpectedBy.Equal(next) {
		t.Errorf("compaction = %+v, want a request expected by the next pass at %v", answer.Compaction, next)
	}
	if answer.Backup == nil || !answer.Backup.CreatedAt.Equal(written) {
		t.Errorf("the answer's backup = %+v, want the one written %v", answer.Backup, written)
	}

	// Once the pass has run, `/system` says so, and nothing is pending.
	if err := h.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	rec = h.get(t, "/api/v1/system")
	expectStatus(t, rec, http.StatusOK)
	system := decodeJSON[struct {
		Compaction struct {
			RequestedAt *time.Time `json:"requested_at"`
			CompletedAt *time.Time `json:"completed_at"`
		} `json:"compaction"`
	}](t, rec)
	if system.Compaction.RequestedAt != nil || system.Compaction.CompletedAt == nil {
		t.Errorf("/system compaction = %+v, want nothing pending and a completion", system.Compaction)
	}
}

// Deleting traces answers with the compaction it asked for, as an erasure
// does; and with no backup beside the database, the erasure names none.
func TestATraceDeletionAnswersWithItsCompaction(t *testing.T) {
	h := newAdminHarness(t)
	h.seed(t, &model.Trace{ID: traceHex(1)})
	rec := h.call(t, "DELETE", "/api/v1/traces/"+traceHex(1)+"?confirm="+traceHex(1), nil)
	expectStatus(t, rec, http.StatusOK)
	answer := decodeJSON[struct {
		Compaction struct {
			RequestedAt *time.Time `json:"requested_at"`
			ExpectedBy  *time.Time `json:"expected_by"`
		} `json:"compaction"`
	}](t, rec)
	if answer.Compaction.RequestedAt == nil || answer.Compaction.ExpectedBy == nil {
		t.Errorf("compaction = %+v, want the request and the pass it waits for", answer.Compaction)
	}

	rec = h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"/users/nobody/data", nil)
	expectStatus(t, rec, http.StatusOK)
	if body := decodeJSON[map[string]any](t, rec); body["pre_migration_backup"] != nil {
		t.Errorf("pre_migration_backup = %v with no backup on disk, want the field absent", body["pre_migration_backup"])
	}
}
