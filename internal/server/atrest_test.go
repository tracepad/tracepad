package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
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
	for _, path := range []string{"/api/v1/raw", "/api/v1/raw/n1"} {
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
	backup := h.dbPath + ".pre-0024_compaction.bak"
	written := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	if err := os.WriteFile(backup, []byte("copy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(backup, written, written); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/projects/" + h.project.ID + "/users/erase-me/data"

	type backupBlock struct {
		CreatedAt   time.Time `json:"created_at"`
		RemoveAfter time.Time `json:"remove_after"`
	}
	rec := h.call(t, "DELETE", path, nil)
	expectStatus(t, rec, http.StatusOK)
	preview := decodeJSON[struct {
		Backup *backupBlock `json:"pre_migration_backup"`
	}](t, rec)
	if preview.Backup == nil || !preview.Backup.CreatedAt.Equal(written) ||
		!preview.Backup.RemoveAfter.Equal(written.Add(store.BackupLifetime)) {
		t.Errorf("the preview's backup = %+v, want written %v and removed a week later", preview.Backup, written)
	}

	rec = h.call(t, "DELETE", path+"?wait=30&confirm=erase-me", nil)
	expectStatus(t, rec, http.StatusOK)
	expectUniqueKeys(t, rec)
	type compaction struct {
		RequestedAt *time.Time `json:"requested_at"`
		ExpectedBy  *time.Time `json:"expected_by"`
		CompletedAt *time.Time `json:"completed_at"`
	}
	answer := decodeJSON[struct {
		ID         string         `json:"id"`
		Deleted    map[string]int `json:"deleted"`
		Compaction compaction     `json:"compaction"`
		Backup     *backupBlock   `json:"pre_migration_backup"`
	}](t, rec)
	if answer.Compaction.CompletedAt != nil {
		t.Errorf("compaction = %+v, want no completion before a pass has run", answer.Compaction)
	}
	if answer.Deleted["traces"] != 1 {
		t.Fatalf("deleted = %v, want the one trace", answer.Deleted)
	}
	next := time.Unix(0, h.sweeper.ExpectedBy())
	if answer.Compaction.RequestedAt == nil || answer.Compaction.ExpectedBy == nil ||
		!answer.Compaction.ExpectedBy.Equal(next) {
		t.Errorf("compaction = %+v, want a request expected by the next pass at %v", answer.Compaction, next)
	}
	if answer.Backup == nil || !answer.Backup.CreatedAt.Equal(written) {
		t.Errorf("the answer's backup = %+v, want the one written %v", answer.Backup, written)
	}

	// Once the pass has run, `/system` says so to the deployment's
	// credential, and nothing is pending (spec 044 #22).
	if err := h.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	rec = h.call(t, "GET", "/api/v1/system", nil, asAdmin)
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

	// The erasure itself says it was compacted, to the project's own key,
	// which `/system` does not tell (spec 044 #22).
	status := func() compaction {
		t.Helper()
		rec := h.get(t, "/api/v1/projects/"+h.project.ID+"/erasures/"+answer.ID)
		expectStatus(t, rec, http.StatusOK)
		return decodeJSON[struct {
			Compaction compaction `json:"compaction"`
		}](t, rec).Compaction
	}
	done := status()
	if done.CompletedAt == nil || done.ExpectedBy != nil || !done.CompletedAt.Equal(*system.Compaction.CompletedAt) {
		t.Errorf("the erasure's compaction = %+v, want completed with the pass at %v and nothing due",
			done, system.Compaction.CompletedAt)
	}
	// Another project's deletion and the pass it asks for leave the stamp
	// where it was: a later pass is not news this erasure may carry.
	other := h.second(t, "other", "tp-sk-other")
	if err := h.writer.Submit(t.Context(), &store.IngestBatch{ProjectID: other.ID,
		Traces: []*model.Trace{{ID: traceHex(9)}}}); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, h.call(t, "DELETE", "/api/v1/traces/"+traceHex(9)+"?confirm="+traceHex(9), nil,
		asKey("tp-sk-other")), http.StatusOK)
	time.Sleep(2 * time.Millisecond)
	if err := h.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The positive control: that pass did complete a compaction, later.
	later := decodeJSON[struct {
		Compaction struct {
			CompletedAt *time.Time `json:"completed_at"`
		} `json:"compaction"`
	}](t, h.call(t, "GET", "/api/v1/system", nil, asAdmin)).Compaction.CompletedAt
	if later == nil || !later.After(*done.CompletedAt) {
		t.Fatalf("/system completed_at = %v after the second pass, want later than %v", later, done.CompletedAt)
	}
	if again := status(); again.CompletedAt == nil || !again.CompletedAt.Equal(*done.CompletedAt) {
		t.Errorf("after another project's compaction the erasure's = %+v, want it unmoved from %v", again, done.CompletedAt)
	}
}

// Deleting traces answers with the compaction it asked for, as an erasure
// does; and with no backup beside the database, the erasure names none.
func TestATraceDeletionAnswersWithItsCompaction(t *testing.T) {
	h := newAdminHarness(t)
	h.seed(t, &model.Trace{ID: traceHex(1)})
	rec := h.call(t, "DELETE", "/api/v1/traces/"+traceHex(1)+"?confirm="+traceHex(1), nil)
	expectStatus(t, rec, http.StatusOK)
	expectUniqueKeys(t, rec)
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

	// A confirmed erasure that found nothing asked for nothing, and says so —
	// although a request is pending (this deletion's above, and the
	// migration's), it is not this answer's to report.
	rec = h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"/users/nobody/data?confirm=nobody&wait=30", nil)
	expectStatus(t, rec, http.StatusOK)
	expectUniqueKeys(t, rec)
	empty := decodeJSON[struct {
		Compaction struct {
			RequestedAt *time.Time `json:"requested_at"`
			ExpectedBy  *time.Time `json:"expected_by"`
		} `json:"compaction"`
	}](t, rec)
	if empty.Compaction.RequestedAt != nil || empty.Compaction.ExpectedBy != nil {
		t.Errorf("an erasure of nothing answered compaction = %+v, want both null", empty.Compaction)
	}
}

// expectUniqueKeys fails when any object in the answer names a key twice.
// `encoding/json` keeps the last of two and says nothing, so a decoded answer
// cannot show it; a client in another language may keep the first, and the
// answer means something else to it.
func expectUniqueKeys(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	var walk func(path string) error
	walk = func(path string) error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name := key.(string)
				if seen[name] {
					t.Errorf("%s names %q twice: %s", path, name, rec.Body)
				}
				seen[name] = true
				if err := walk(path + "." + name); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case json.Delim('['):
			for i := 0; decoder.More(); i++ {
				if err := walk(path + "[]"); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		}
		return nil
	}
	if err := walk("$"); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("the answer is not JSON: %v", err)
	}
}

// A running erasure never says it was compacted, even when a pass has stamped
// its row: a later chunk may delete more and ask again, which takes the stamp
// away, and a status that said "compacted" and then did not would be read as
// done the first time (spec 044 #22).
func TestARunningErasureSaysNothingOfItsCompaction(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	for state, wantDone := range map[string]bool{store.ErasureRunning: false, store.ErasureDone: true} {
		answer := mustJSONBytes(h.server.erasureCompaction(&store.Erasure{
			State: state, Compaction: 1_000, CompactedAt: 2_000}))
		var got struct {
			CompletedAt *time.Time `json:"completed_at"`
		}
		if err := json.Unmarshal(answer, &got); err != nil {
			t.Fatal(err)
		}
		if (got.CompletedAt != nil) != wantDone {
			t.Errorf("%s: %s, want completed_at only once it has ended", state, answer)
		}
	}
}
