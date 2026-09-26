package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// The admin surface (spec 005, Testing #5–#8). Three properties are checked
// everywhere: who may ask, that a destructive request changes nothing until it
// is confirmed, and that neither the asking nor the answering crosses into
// another project.

const adminToken = "tp-admin-secret-token"

// newAdminHarness is the ordinary harness with a cross-project token
// configured, which is what the token-less deployment tests deliberately do
// not use.
func newAdminHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, &config.Config{
		Listen:       ":0",
		StoreRaw:     true,
		MaxBodyBytes: config.DefaultMaxBodyBytes,
		AdminToken:   adminToken,
	}, store.WriterOptions{})
}

// asAdmin sends a request with the cross-project token instead of a key.
func asAdmin(r *http.Request) { r.Header.Set("Authorization", "Bearer "+adminToken) }

// asKey sends a request with a particular project secret.
func asKey(secret string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
}

// second creates a neighbouring tenant to check that nothing reaches across.
func (h *harness) second(t *testing.T, name, secret string) *store.Project {
	t.Helper()
	project, err := h.store.CreateProject(name, store.KeyPair{PublicKey: "tp-pk-" + name, Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	return project
}

// TestProjectAdminAuthMatrix is Testing #7: who reaches what. The interesting
// rows are the last two — a project key administers itself but cannot delete
// itself, and a deployment with no token is told that rather than being told
// its perfectly good key is unauthorized.
func TestProjectAdminAuthMatrix(t *testing.T) {
	h := newAdminHarness(t)
	other := h.second(t, "other", "tp-sk-other")

	// Its own project: yes.
	rec := h.get(t, "/api/v1/projects/"+h.project.ID)
	expectStatus(t, rec, 200)

	// Somebody else's: no, and the message says why.
	rec = h.get(t, "/api/v1/projects/"+other.ID)
	expectError(t, rec, http.StatusForbidden, "own project only")

	// The listing is scoped the same way.
	rec = h.get(t, "/api/v1/projects")
	expectStatus(t, rec, 200)
	listing := decodeJSON[struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}](t, rec)
	if len(listing.Projects) != 1 || listing.Projects[0].ID != h.project.ID {
		t.Errorf("projects = %+v, want only the asking project's own", listing.Projects)
	}

	// The admin token sees both.
	rec = h.call(t, "GET", "/api/v1/projects", nil, asAdmin)
	expectStatus(t, rec, 200)
	all := decodeJSON[struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}](t, rec)
	if len(all.Projects) != 2 {
		t.Errorf("projects = %+v, want both tenants", all.Projects)
	}

	// Creating and deleting are the token's alone, even for one's own
	// project: an sk lives in application config and CI (#11).
	rec = h.send(t, "POST", "/api/v1/projects", map[string]any{"name": "third"})
	expectError(t, rec, http.StatusForbidden, "admin token")
	rec = h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"?confirm=test", nil)
	expectError(t, rec, http.StatusForbidden, "admin token")

	// And an unknown credential is still just unauthorized.
	rec = h.call(t, "GET", "/api/v1/projects", nil, asKey("tp-sk-nobody"))
	expectStatus(t, rec, 401)
}

// TestAdminEndpointsSayWhenNoTokenIsConfigured: the default install has no
// admin token, and a 403 that names the missing variable is the difference
// between a five-minute fix and a support thread (#11).
func TestAdminEndpointsSayWhenNoTokenIsConfigured(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"?confirm=test", nil)
	expectError(t, rec, http.StatusForbidden, "TRACEPAD_ADMIN_TOKEN is not configured")

	// The project's own administration still works without one, which is
	// the whole point of Decision 11.
	rec = h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID, map[string]any{"retention_days": 90})
	expectStatus(t, rec, 200)
}

// TestRetentionRoundTrip: the default is "keep forever" and it survives being
// read back, a longer window applies straight away, and a shorter one is a dry
// run until the project's name is echoed (#2, #8).
func TestRetentionRoundTrip(t *testing.T) {
	h := newAdminHarness(t)
	h.seed(t, &model.Trace{ID: traceHex(1)},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})

	rec := h.get(t, "/api/v1/projects/"+h.project.ID)
	expectStatus(t, rec, 200)
	project := decodeJSON[struct {
		Name             string `json:"name"`
		RetentionDays    *int   `json:"retention_days"`
		RawRetentionDays *int   `json:"raw_retention_days"`
	}](t, rec)
	if project.RetentionDays != nil || project.RawRetentionDays != nil {
		t.Fatalf("project = %+v, want both windows unset: forever is the default (#2)", project)
	}

	// Setting a window where there was none is a shrink — from forever to
	// ninety days — so it previews first.
	rec = h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID, map[string]any{"retention_days": 90})
	expectStatus(t, rec, 200)
	preview := decodeJSON[struct {
		DryRun      bool           `json:"dry_run"`
		WouldDelete map[string]int `json:"would_delete"`
		Confirm     string         `json:"confirm"`
	}](t, rec)
	if !preview.DryRun || preview.Confirm != "test" {
		t.Fatalf("preview = %+v, want a dry run asking for the project's name", preview)
	}
	if stored, _ := h.store.ProjectByID(context.Background(), h.project.ID); stored.RetentionDays != nil {
		t.Errorf("the preview changed the window to %d; a dry run changes nothing", *stored.RetentionDays)
	}

	// A wrong echo is a 400 and still changes nothing.
	rec = h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID+"?confirm=Test",
		map[string]any{"retention_days": 90})
	expectError(t, rec, http.StatusBadRequest, `"test"`)
	if stored, _ := h.store.ProjectByID(context.Background(), h.project.ID); stored.RetentionDays != nil {
		t.Errorf("a rejected confirmation changed the window anyway")
	}

	// The right one applies it.
	rec = h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID+"?confirm=test",
		map[string]any{"retention_days": 90})
	expectStatus(t, rec, 200)
	stored, _ := h.store.ProjectByID(context.Background(), h.project.ID)
	if stored.RetentionDays == nil || *stored.RetentionDays != 90 {
		t.Fatalf("retention = %v, want 90", stored.RetentionDays)
	}

	// Growing it destroys nothing, so it needs no ceremony.
	rec = h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID, map[string]any{"retention_days": 365})
	expectStatus(t, rec, 200)
	if stored, _ := h.store.ProjectByID(context.Background(), h.project.ID); *stored.RetentionDays != 365 {
		t.Errorf("retention = %v, want the longer window applied without a confirmation", stored.RetentionDays)
	}

	// And so does clearing it back to forever.
	rec = h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID, map[string]any{"retention_days": nil})
	expectStatus(t, rec, 200)
	if stored, _ := h.store.ProjectByID(context.Background(), h.project.ID); stored.RetentionDays != nil {
		t.Errorf("retention = %v, want null to mean forever", stored.RetentionDays)
	}
}

// TestRetentionPreviewCountsWhatWouldGo: the dry run is the same arithmetic
// the sweeper does, so what it promises is what the next pass keeps.
func TestRetentionPreviewCountsWhatWouldGo(t *testing.T) {
	h := newAdminHarness(t)
	// Two traces that arrived a fortnight ago and one that arrived now.
	old := seedBase
	if err := h.writer.Submit(t.Context(), &store.IngestBatch{
		ProjectID:  h.project.ID,
		IngestedAt: old - 14*24*int64(3600)*1_000_000_000,
		Traces:     []*model.Trace{{ID: traceHex(1)}, {ID: traceHex(2)}},
		Observations: []*model.Observation{
			{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan, Level: model.LevelDefault},
			{TraceID: traceHex(2), ID: spanHex(2), Type: model.TypeSpan, Level: model.LevelDefault},
		},
	}); err != nil {
		t.Fatal(err)
	}
	h.seed(t, &model.Trace{ID: traceHex(3)},
		&model.Observation{TraceID: traceHex(3), ID: spanHex(3), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})

	rec := h.send(t, "PATCH", "/api/v1/projects/"+h.project.ID, map[string]any{"retention_days": 7})
	expectStatus(t, rec, 200)
	preview := decodeJSON[struct {
		WouldDelete map[string]int `json:"would_delete"`
		Oldest      string         `json:"oldest"`
	}](t, rec)
	if preview.WouldDelete["traces"] != 2 {
		t.Errorf("would_delete = %v, want the two that are already past a seven-day window", preview.WouldDelete)
	}
	if preview.Oldest == "" {
		t.Errorf("the preview does not say how far back the deletion reaches")
	}
}

// TestKeyRotation is Decision 12: several active pairs, so the sequence
// create → move the SDKs → revoke never has a window where ingest 401s.
func TestKeyRotation(t *testing.T) {
	h := newAdminHarness(t)

	rec := h.call(t, "POST", "/api/v1/projects/"+h.project.ID+"/keys", nil)
	expectStatus(t, rec, 201)
	minted := decodeJSON[struct {
		PublicKey string `json:"public_key"`
		SecretKey string `json:"secret_key"`
	}](t, rec)
	if minted.SecretKey == "" || minted.PublicKey == "" {
		t.Fatalf("minted = %+v, want a whole pair", minted)
	}

	// Both keys work: that is the point.
	for _, secret := range []string{testSecret, minted.SecretKey} {
		rec := h.call(t, "GET", "/api/v1/traces", nil, asKey(secret))
		expectStatus(t, rec, 200)
	}

	rec = h.get(t, "/api/v1/projects/"+h.project.ID+"/keys")
	expectStatus(t, rec, 200)
	keys := decodeJSON[struct {
		Keys []struct {
			PublicKey string `json:"public_key"`
			CreatedAt string `json:"created_at"`
		} `json:"keys"`
	}](t, rec)
	if len(keys.Keys) != 2 {
		t.Fatalf("keys = %+v, want both pairs listed", keys.Keys)
	}
	for _, key := range keys.Keys {
		if key.CreatedAt == "" {
			t.Errorf("key %s has no creation date to rotate by", key.PublicKey)
		}
	}

	// Revoking one of two needs no ceremony; the other still ingests.
	rec = h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"/keys/"+testPublic, nil,
		asKey(minted.SecretKey))
	expectStatus(t, rec, 200)
	rec = h.call(t, "GET", "/api/v1/traces", nil, asKey(testSecret))
	expectStatus(t, rec, 401)
	rec = h.call(t, "GET", "/api/v1/traces", nil, asKey(minted.SecretKey))
	expectStatus(t, rec, 200)

	// The last one does: a project with no keys cannot ingest (#12).
	path := "/api/v1/projects/" + h.project.ID + "/keys/" + minted.PublicKey
	rec = h.call(t, "DELETE", path, nil, asKey(minted.SecretKey))
	expectStatus(t, rec, 200)
	preview := decodeJSON[struct {
		DryRun  bool   `json:"dry_run"`
		Confirm string `json:"confirm"`
	}](t, rec)
	if !preview.DryRun || preview.Confirm != "test" {
		t.Fatalf("preview = %+v, want the last key to ask for the project's name", preview)
	}
	rec = h.call(t, "GET", "/api/v1/traces", nil, asKey(minted.SecretKey))
	expectStatus(t, rec, 200)

	rec = h.call(t, "DELETE", path+"?confirm=wrong", nil, asKey(minted.SecretKey))
	expectError(t, rec, http.StatusBadRequest, `"test"`)
	rec = h.call(t, "DELETE", path+"?confirm=test", nil, asKey(minted.SecretKey))
	expectStatus(t, rec, 200)
	rec = h.call(t, "GET", "/api/v1/traces", nil, asKey(minted.SecretKey))
	expectStatus(t, rec, 401)
}

// TestRevokingAKeyOfAnotherProject: keys are project-scoped like everything
// else, and a public key that belongs to a neighbour is not a key this project
// has (spec 004 Decision 33).
func TestRevokingAKeyOfAnotherProject(t *testing.T) {
	h := newAdminHarness(t)
	other := h.second(t, "other", "tp-sk-other")

	rec := h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"/keys/tp-pk-other", nil)
	expectError(t, rec, http.StatusNotFound, "tp-pk-other")

	keys, err := h.store.ProjectKeys(other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Errorf("the other project's keys = %+v, want it untouched", keys)
	}
}

// TestSoftDeleteAndRestore is Testing #5 end to end: the keys die immediately
// but can still undo the deletion, the data survives the grace window, restore
// brings everything back, and the name stays reserved throughout.
func TestSoftDeleteAndRestore(t *testing.T) {
	h := newAdminHarness(t)
	h.seed(t, &model.Trace{ID: traceHex(1)},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})

	// A review queue with an item in it, because the cascade takes both of
	// spec 024's tables and the preview is what says so before the fact
	// (spec 005 #8; found in review of PR #43).
	h.putConfigs(t, "accuracy")
	expectStatus(t, h.putQueue(t, "review", "accuracy"), 201)
	h.addTarget(t, "review", map[string]any{"trace_id": traceHex(1)})

	// The dry run first: it names what would go and changes nothing.
	rec := h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID, nil, asAdmin)
	expectStatus(t, rec, 200)
	preview := decodeJSON[struct {
		DryRun      bool           `json:"dry_run"`
		WouldDelete map[string]int `json:"would_delete"`
		Confirm     string         `json:"confirm"`
	}](t, rec)
	if !preview.DryRun || preview.Confirm != "test" || preview.WouldDelete["traces"] != 1 {
		t.Fatalf("preview = %+v, want a dry run naming the trace and the echo", preview)
	}
	if preview.WouldDelete["annotation_queues"] != 1 || preview.WouldDelete["annotation_items"] != 1 {
		t.Errorf("would_delete = %v, want the queue and its item named too", preview.WouldDelete)
	}
	if stored, _ := h.store.ProjectByID(context.Background(), h.project.ID); stored.Deleted() {
		t.Fatal("the dry run deleted the project")
	}

	// A wrong echo does nothing either.
	rec = h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"?confirm=tests", nil, asAdmin)
	expectError(t, rec, http.StatusBadRequest, `"test"`)
	if stored, _ := h.store.ProjectByID(context.Background(), h.project.ID); stored.Deleted() {
		t.Fatal("a rejected confirmation deleted the project")
	}

	// The right one answers 202 with the date the data goes.
	rec = h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"?confirm=test", nil, asAdmin)
	expectStatus(t, rec, 202)
	deleted := decodeJSON[struct {
		DeletedAt string `json:"deleted_at"`
		PurgeAt   string `json:"purge_at"`
	}](t, rec)
	if deleted.DeletedAt == "" || deleted.PurgeAt == "" {
		t.Fatalf("deleted = %+v, want both stamps", deleted)
	}

	// The keys are dead everywhere — ingest, reads, its own administration.
	for _, request := range []struct{ method, path string }{
		{"GET", "/api/v1/traces"},
		{"GET", "/api/v1/system"},
		{"GET", "/api/v1/projects/" + h.project.ID + "/keys"},
		{"POST", "/api/v1/scores"},
	} {
		rec := h.call(t, request.method, request.path, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d for a deleted project's key, want 401",
				request.method, request.path, rec.Code)
		}
	}

	// Except the two that can undo it (#10).
	rec = h.get(t, "/api/v1/projects/"+h.project.ID)
	expectStatus(t, rec, 200)

	// The name stays reserved while it is restorable, and the 409 says so.
	rec = h.send(t, "POST", "/api/v1/projects", map[string]any{"name": "test"})
	// Sent with the project key, this is a 403 before it is a 409; the
	// admin token is what reaches the check.
	expectStatus(t, rec, 403)
	rec = h.call(t, "POST", "/api/v1/projects", mustJSON(t, map[string]any{"name": "test"}), asAdmin)
	expectError(t, rec, http.StatusConflict, "restored")

	// The data is untouched throughout.
	if got := h.countTraces(t); got != 1 {
		t.Errorf("traces = %d during the grace window, want the data still there", got)
	}

	// Restoring is an owner's, not a key's (spec 028 #3). Spec 005 #10 let
	// the project's own key do it because a token-less deployment had no
	// other credential; a deployment with accounts always has an owner, and
	// this key is exactly the leaked-application-credential case that must
	// not be able to move a project in or out of existence.
	rec = h.call(t, "POST", "/api/v1/projects/"+h.project.ID+"/restore", nil)
	expectError(t, rec, http.StatusForbidden, "owner account")

	rec = h.call(t, "POST", "/api/v1/projects/"+h.project.ID+"/restore", nil, asAdmin)
	expectStatus(t, rec, 200)
	restored := decodeJSON[struct {
		DeletedAt *string `json:"deleted_at"`
	}](t, rec)
	if restored.DeletedAt != nil {
		t.Errorf("deleted_at = %v after a restore, want it gone", *restored.DeletedAt)
	}

	// And everything works again.
	rec = h.get(t, "/api/v1/traces")
	expectStatus(t, rec, 200)
	if got := h.countTraces(t); got != 1 {
		t.Errorf("traces = %d after the restore, want the data back", got)
	}

	// Restoring what is not deleted is a mistake worth naming.
	rec = h.call(t, "POST", "/api/v1/projects/"+h.project.ID+"/restore", nil, asAdmin)
	expectError(t, rec, http.StatusBadRequest, "not deleted")
}

// TestDeletedProjectsAreHiddenUnlessAsked: a deleted project vanishes from
// listings, and an administrator can still find it and its purge date (#9).
func TestDeletedProjectsAreHiddenUnlessAsked(t *testing.T) {
	h := newAdminHarness(t)
	other := h.second(t, "other", "tp-sk-other")

	rec := h.call(t, "DELETE", "/api/v1/projects/"+other.ID+"?confirm=other", nil, asAdmin)
	expectStatus(t, rec, 202)

	rec = h.call(t, "GET", "/api/v1/projects", nil, asAdmin)
	expectStatus(t, rec, 200)
	visible := decodeJSON[struct {
		Projects []struct {
			ID string `json:"id"`
		} `json:"projects"`
	}](t, rec)
	if len(visible.Projects) != 1 || visible.Projects[0].ID != h.project.ID {
		t.Errorf("projects = %+v, want the deleted one hidden", visible.Projects)
	}

	rec = h.call(t, "GET", "/api/v1/projects?include=deleted", nil, asAdmin)
	expectStatus(t, rec, 200)
	all := decodeJSON[struct {
		Projects []struct {
			ID      string `json:"id"`
			PurgeAt string `json:"purge_at"`
		} `json:"projects"`
	}](t, rec)
	if len(all.Projects) != 2 {
		t.Fatalf("projects = %+v, want the deleted one shown when asked for", all.Projects)
	}
	found := false
	for _, project := range all.Projects {
		if project.ID == other.ID {
			found = project.PurgeAt != ""
		}
	}
	if !found {
		t.Errorf("the deleted project is listed without the date its data goes")
	}

	// A project key asking for other people's deleted projects is told no.
	rec = h.get(t, "/api/v1/projects?include=deleted")
	expectError(t, rec, http.StatusForbidden, "admin token")
	// And an unknown value for it is a 400, not a silent listing.
	rec = h.call(t, "GET", "/api/v1/projects?include=everything", nil, asAdmin)
	expectError(t, rec, http.StatusBadRequest, "deleted")
}

// TestEraseUserData is Testing #8: the queryable stores are emptied for one
// user, the raw bodies are deliberately not, and nobody else's data moves.
func TestEraseUserData(t *testing.T) {
	h := newAdminHarness(t)
	other := h.second(t, "other", "tp-sk-other")

	h.seed(t, &model.Trace{ID: traceHex(1), UserID: "erase-me"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})
	h.seed(t, &model.Trace{ID: traceHex(2), UserID: "keep-me"},
		&model.Observation{TraceID: traceHex(2), ID: spanHex(2), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})
	// A raw body, which erasure does not touch (#7).
	if err := h.writer.Submit(t.Context(), &store.IngestBatch{
		ProjectID: h.project.ID,
		Raw:       &store.RawBatch{ReceivedAt: seedBase, Dialect: "otel", Body: []byte("body")},
	}); err != nil {
		t.Fatal(err)
	}
	// The same user id in the neighbouring project, which is a different
	// person as far as this project is concerned.
	if err := h.writer.Submit(t.Context(), &store.IngestBatch{
		ProjectID: other.ID,
		Traces:    []*model.Trace{{ID: traceHex(3), UserID: "erase-me"}},
		Observations: []*model.Observation{{TraceID: traceHex(3), ID: spanHex(3),
			Type: model.TypeSpan, Level: model.LevelDefault}},
	}); err != nil {
		t.Fatal(err)
	}

	// A review queue holding the trace about to be erased (spec 024 #3): the
	// item goes with the trace, and the preview and the answer both say so —
	// they are what an operator shows for "everything about this person is
	// gone" (found in review of PR #43).
	h.putConfigs(t, "accuracy")
	expectStatus(t, h.putQueue(t, "review", "accuracy"), 201)
	h.addTarget(t, "review", map[string]any{"trace_id": traceHex(1)})
	h.addTarget(t, "review", map[string]any{"trace_id": traceHex(2)})

	path := "/api/v1/projects/" + h.project.ID + "/users/erase-me/data"

	rec := h.call(t, "DELETE", path, nil)
	expectStatus(t, rec, 200)
	preview := decodeJSON[struct {
		DryRun      bool           `json:"dry_run"`
		WouldDelete map[string]int `json:"would_delete"`
		Confirm     string         `json:"confirm"`
	}](t, rec)
	// The echo is the user id here, because the user is what is destroyed.
	if !preview.DryRun || preview.Confirm != "erase-me" {
		t.Fatalf("preview = %+v, want a dry run asking for the user id", preview)
	}
	if preview.WouldDelete["traces"] != 1 || preview.WouldDelete["observations"] != 1 {
		t.Errorf("would_delete = %v, want the one trace and its span", preview.WouldDelete)
	}
	// One of the two queued items: the other trace's stays.
	if preview.WouldDelete["annotation_items"] != 1 {
		t.Errorf("would_delete = %v, want the erased trace's queue item counted", preview.WouldDelete)
	}
	if got := h.countTraces(t); got != 2 {
		t.Fatalf("traces = %d after a dry run, want both still there", got)
	}

	rec = h.call(t, "DELETE", path+"?confirm=keep-me", nil)
	expectError(t, rec, http.StatusBadRequest, "erase-me")
	if got := h.countTraces(t); got != 2 {
		t.Fatalf("a rejected confirmation erased data anyway")
	}

	rec = h.call(t, "DELETE", path+"?confirm=erase-me", nil)
	expectStatus(t, rec, 200)
	erased := decodeJSON[struct {
		DryRun  bool           `json:"dry_run"`
		Deleted map[string]int `json:"deleted"`
	}](t, rec)
	if erased.DryRun || erased.Deleted["traces"] != 1 {
		t.Fatalf("erased = %+v, want the one trace reported as gone", erased)
	}
	if erased.Deleted["annotation_items"] != 1 {
		t.Errorf("erased = %+v, want the queue item reported gone with its trace", erased)
	}
	// And the queue itself, with the other trace's item, stands.
	if counts := h.queueCounts(t, "review"); counts.Pending != 1 {
		t.Errorf("queue counts = %+v, want the other trace's item to survive", counts)
	}

	// The other user in this project is untouched, and so is the same id in
	// the neighbouring one.
	rec = h.get(t, "/api/v1/traces?user_id=erase-me")
	expectStatus(t, rec, 200)
	if listing := decodeJSON[struct {
		Traces []struct{} `json:"traces"`
	}](t, rec); len(listing.Traces) != 0 {
		t.Errorf("the erased user still has %d traces", len(listing.Traces))
	}
	rec = h.get(t, "/api/v1/traces?user_id=keep-me")
	expectStatus(t, rec, 200)
	if listing := decodeJSON[struct {
		Traces []struct{} `json:"traces"`
	}](t, rec); len(listing.Traces) != 1 {
		t.Errorf("the other user lost data: traces = %d", len(listing.Traces))
	}
	rec = h.call(t, "GET", "/api/v1/traces?user_id=erase-me", nil, asKey("tp-sk-other"))
	expectStatus(t, rec, 200)
	if listing := decodeJSON[struct {
		Traces []struct{} `json:"traces"`
	}](t, rec); len(listing.Traces) != 1 {
		t.Errorf("erasing in one project reached into another: traces = %d", len(listing.Traces))
	}

	// Raw is an archive with its own schedule; erasure leaves it alone and
	// the documentation says so rather than pretending otherwise.
	if raw := h.countRows(t, h.project.ID, "raw_batches"); raw != 1 {
		t.Errorf("raw batches = %d after an erasure, want the archive untouched", raw)
	}
}

// TestPreviewsAndConfirmationsDoNotCrossProjects: every count a preview shows
// and every row a confirmation removes belongs to the project in the path
// (spec 004 Decision 33).
func TestPreviewsAndConfirmationsDoNotCrossProjects(t *testing.T) {
	h := newAdminHarness(t)
	other := h.second(t, "other", "tp-sk-other")

	for i := 1; i <= 3; i++ {
		if err := h.writer.Submit(t.Context(), &store.IngestBatch{
			ProjectID: other.ID,
			Traces:    []*model.Trace{{ID: traceHex(100 + i), UserID: "u1"}},
			Observations: []*model.Observation{{TraceID: traceHex(100 + i), ID: spanHex(i),
				Type: model.TypeSpan, Level: model.LevelDefault}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	h.seed(t, &model.Trace{ID: traceHex(1), UserID: "u1"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(9), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})

	rec := h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID, nil, asAdmin)
	expectStatus(t, rec, 200)
	preview := decodeJSON[struct {
		WouldDelete map[string]int `json:"would_delete"`
	}](t, rec)
	if preview.WouldDelete["traces"] != 1 {
		t.Errorf("would_delete = %v, want only this project's one trace", preview.WouldDelete)
	}
	if preview.WouldDelete["api_keys"] != 1 {
		t.Errorf("would_delete = %v, want only this project's one key", preview.WouldDelete)
	}

	rec = h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"/users/u1/data", nil)
	expectStatus(t, rec, 200)
	erasure := decodeJSON[struct {
		WouldDelete map[string]int `json:"would_delete"`
	}](t, rec)
	if erasure.WouldDelete["traces"] != 1 {
		t.Errorf("would_delete = %v, want only this project's trace for that user", erasure.WouldDelete)
	}
}

// TestProjectCreateRejectsABadName: a project name is one URL path segment and
// a bootstrap entry, so it obeys the same grammar as everything else nameable.
func TestProjectCreateRejectsABadName(t *testing.T) {
	h := newAdminHarness(t)

	for _, name := range []string{"", "has space", "-leading", "with:colon"} {
		rec := h.call(t, "POST", "/api/v1/projects", mustJSON(t, map[string]any{"name": name}), asAdmin)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("creating %q = %d, want 400", name, rec.Code)
		}
	}

	rec := h.call(t, "POST", "/api/v1/projects", mustJSON(t, map[string]any{"name": "test"}), asAdmin)
	expectError(t, rec, http.StatusConflict, "already exists")

	rec = h.call(t, "POST", "/api/v1/projects", mustJSON(t, map[string]any{"name": "fresh"}), asAdmin)
	expectStatus(t, rec, 201)
	created := decodeJSON[struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		SecretKey     string `json:"secret_key"`
		RetentionDays *int   `json:"retention_days"`
	}](t, rec)
	if created.Name != "fresh" || created.SecretKey == "" {
		t.Fatalf("created = %+v, want the project and its one-time secret", created)
	}
	if created.RetentionDays != nil {
		t.Errorf("retention_days = %d on a new project, want forever (#2)", *created.RetentionDays)
	}
	// The minted key works immediately.
	rec = h.call(t, "GET", "/api/v1/traces", nil, asKey(created.SecretKey))
	expectStatus(t, rec, 200)
}

// TestPatchRejectsNonsense keeps the write surface as strict as the read one
// (spec 003 #17, #21): a body that says nothing, an unknown field, a window
// that is not a number of days.
func TestPatchRejectsNonsense(t *testing.T) {
	h := newAdminHarness(t)
	path := "/api/v1/projects/" + h.project.ID

	rec := h.send(t, "PATCH", path, map[string]any{})
	expectError(t, rec, http.StatusBadRequest, "nothing to change")

	rec = h.send(t, "PATCH", path, map[string]any{"retention": 30})
	expectError(t, rec, http.StatusBadRequest, "unknown field")

	for _, value := range []any{0, -1, "30"} {
		rec := h.send(t, "PATCH", path, map[string]any{"retention_days": value})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("retention_days = %v gave %d, want 400", value, rec.Code)
		}
	}

	rec = h.call(t, "PATCH", path+"?days=30", mustJSON(t, map[string]any{"retention_days": 30}))
	expectError(t, rec, http.StatusBadRequest, "unknown query parameter")
}

// countRows reads a project's row count from the store rather than through the
// read API: a deleted project's key cannot call the API, and what is being
// asserted is exactly that its data is still on disk.
func (h *harness) countRows(t *testing.T, projectID, table string) int64 {
	t.Helper()
	counts, err := h.store.TableCounts(projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range counts {
		if count.Table == table {
			return count.Rows
		}
	}
	t.Fatalf("%s is not a counted table", table)
	return 0
}

func (h *harness) countTraces(t *testing.T) int64 {
	t.Helper()
	return h.countRows(t, h.project.ID, "traces")
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestRetentionWindowHasACeiling: a window becomes a nanosecond cutoff, and
// past ~106751 days that multiplication overflows int64 and wraps the cutoff
// into the future, where it matches every row. Worse, "30 days becomes a
// million" is not a shrink, so it would have gone through with no preview and
// no echo — `tracepad retention set --days 999999`, meaning "keep it forever",
// emptying the project on the next sweep.
func TestRetentionWindowHasACeiling(t *testing.T) {
	h := newAdminHarness(t)
	path := "/api/v1/projects/" + h.project.ID

	rec := h.send(t, "PATCH", path+"?confirm=test", map[string]any{"retention_days": 30})
	expectStatus(t, rec, 200)

	for _, field := range []string{"retention_days", "raw_retention_days"} {
		rec := h.send(t, "PATCH", path, map[string]any{field: store.MaxRetentionDays + 1})
		expectError(t, rec, http.StatusBadRequest, "between 1 and")
	}
	stored, _ := h.store.ProjectByID(context.Background(), h.project.ID)
	if *stored.RetentionDays != 30 {
		t.Fatalf("retention = %v, want the refused window to have changed nothing", stored.RetentionDays)
	}

	// The ceiling itself is a legal window, and "forever" has a spelling.
	rec = h.send(t, "PATCH", path, map[string]any{"retention_days": store.MaxRetentionDays})
	expectStatus(t, rec, 200)
	rec = h.send(t, "PATCH", path, map[string]any{"retention_days": nil})
	expectStatus(t, rec, 200)
	if stored, _ := h.store.ProjectByID(context.Background(), h.project.ID); stored.RetentionDays != nil {
		t.Errorf("retention = %v, want null to be how forever is said", stored.RetentionDays)
	}
}
