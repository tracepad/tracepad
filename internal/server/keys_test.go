package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// Keys stop managing keys, and every key says who minted it and when it was
// last used (spec 045, PR 1; Testing #4, #6–#8 and #10).

// keyListing is `GET /api/v1/projects/{id}/keys` as a test reads it.
type keyListing struct {
	Keys []listedKey `json:"keys"`
}

type listedKey struct {
	PublicKey string   `json:"public_key"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	CreatedAt string   `json:"created_at"`
	CreatedBy struct {
		Kind      string `json:"kind"`
		AccountID string `json:"account_id"`
		Email     string `json:"email"`
		Standing  string `json:"standing"`
	} `json:"created_by"`
	LastUsedAt *string `json:"last_used_at"`
}

// keysOf lists a project's keys with the admin token, by public key.
func (h *harness) keysOf(t *testing.T, projectID string) map[string]listedKey {
	t.Helper()
	rec := h.call(t, "GET", "/api/v1/projects/"+projectID+"/keys", nil, asAdmin)
	expectStatus(t, rec, 200)
	byKey := map[string]listedKey{}
	for _, key := range decodeJSON[keyListing](t, rec).Keys {
		byKey[key.PublicKey] = key
	}
	return byKey
}

// minted is the answer to a mint.
type minted struct {
	PublicKey string   `json:"public_key"`
	SecretKey string   `json:"secret_key"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	CreatedBy struct {
		Kind     string `json:"kind"`
		Email    string `json:"email"`
		Standing string `json:"standing"`
	} `json:"created_by"`
}

// TestAKeyCannotMintAKey is red without the fix: the harness's key lists,
// mints and revokes keys on `main`, and a key minted with a key outlives the
// revocation of the first (Testing #4).
func TestAKeyCannotMintAKey(t *testing.T) {
	h := newAccountHarness(t)
	keys := "/api/v1/projects/" + h.project.ID + "/keys"
	before := h.keysOf(t, h.project.ID)

	for _, request := range []struct{ method, path string }{
		{"POST", keys},
		{"GET", keys},
		{"DELETE", keys + "/" + testPublic},
		{"DELETE", keys + "/" + testPublic + "?confirm=test"},
	} {
		rec := h.call(t, request.method, request.path, nil)
		expectError(t, rec, http.StatusForbidden,
			"a project key cannot list, mint or revoke keys; that needs an owner or editor signed in, or the admin token")
	}
	// A body changes nothing: the refusal is the guard's, before any.
	rec := h.call(t, "POST", keys, mustJSON(t, map[string]any{"name": "mine now"}))
	expectStatus(t, rec, http.StatusForbidden)

	if after := h.keysOf(t, h.project.ID); len(after) != len(before) || after[testPublic].PublicKey == "" {
		t.Errorf("keys after a key's attempts = %+v, want exactly the %d there were", after, len(before))
	}
	// The key itself goes on doing everything else it did.
	expectStatus(t, h.get(t, "/api/v1/traces"), 200)
	expectStatus(t, h.get(t, "/api/v1/projects/"+h.project.ID), 200)
}

// TestPeopleAndTheTokenManageKeys: an owner and an editor signed in, and the
// admin token, each list, mint and revoke — and each mint records who did it
// (Testing #6).
func TestPeopleAndTheTokenManageKeys(t *testing.T) {
	h := newAccountHarness(t)
	owner, editor := h.owner(t), h.editor(t)
	keys := "/api/v1/projects/" + h.project.ID + "/keys"

	for _, c := range []struct {
		who      string
		as       func(*http.Request)
		kind     string
		email    string
		standing string
	}{
		{"an owner", asSession(owner), store.MintedByAccount, "owner@example.com", store.RoleOwner},
		{"an editor", asSession(editor), store.MintedByAccount, "editor@example.com", store.RoleEditor},
		{"the admin token", asAdmin, store.MintedByAdminToken, "", ""},
	} {
		rec := h.call(t, "POST", keys, mustJSON(t, map[string]any{"name": "  by " + c.who + "  "}),
			c.as, inProject(h.project.ID))
		expectStatus(t, rec, http.StatusCreated)
		got := decodeJSON[minted](t, rec)
		if got.SecretKey == "" || got.Name != "by "+c.who || got.CreatedBy.Kind != c.kind ||
			got.CreatedBy.Email != c.email || got.CreatedBy.Standing != c.standing {
			t.Errorf("%s minted %+v", c.who, got)
		}

		rec = h.call(t, "GET", keys, nil, c.as, inProject(h.project.ID))
		expectStatus(t, rec, 200)
		var listed *listedKey
		for _, key := range decodeJSON[keyListing](t, rec).Keys {
			if key.PublicKey == got.PublicKey {
				listed = &key
			}
		}
		if listed == nil {
			t.Fatalf("%s's listing does not have the key it minted", c.who)
		}
		if listed.CreatedBy.Kind != c.kind || listed.CreatedBy.Email != c.email ||
			listed.CreatedBy.Standing != c.standing || strings.Join(listed.Scopes, " ") != store.AllScopes ||
			listed.LastUsedAt != nil || listed.Name != "by "+c.who {
			t.Errorf("%s's key is listed as %+v", c.who, *listed)
		}
		if c.kind == store.MintedByAccount && listed.CreatedBy.AccountID == "" {
			t.Errorf("%s's key does not name the account", c.who)
		}

		rec = h.call(t, "DELETE", keys+"/"+got.PublicKey, nil, c.as, inProject(h.project.ID))
		expectStatus(t, rec, 200)
		if _, still := h.keysOf(t, h.project.ID)[got.PublicKey]; still {
			t.Errorf("%s could not revoke the key it minted", c.who)
		}
	}

	// The harness's own key predates any of this: the server made it.
	if key := h.keysOf(t, h.project.ID)[testPublic]; key.CreatedBy.Kind != store.MintedAtStartup {
		t.Errorf("the harness's key = %+v, want the server's own", key)
	}

	// A project an owner creates has that owner's first key.
	rec := h.call(t, "POST", "/api/v1/projects", mustJSON(t, map[string]any{"name": "fresh"}), asSession(owner))
	expectStatus(t, rec, http.StatusCreated)
	created := decodeJSON[struct {
		ID        string `json:"id"`
		PublicKey string `json:"public_key"`
	}](t, rec)
	first := h.keysOf(t, created.ID)[created.PublicKey]
	if first.CreatedBy.Kind != store.MintedByAccount || first.CreatedBy.AccountID != owner.account.ID ||
		first.CreatedBy.Standing != store.RoleOwner {
		t.Errorf("a new project's first key = %+v, want its creator's", first)
	}
}

// TestStandingFollowsTheMinter: the listing computes the standing when it is
// read, so it moves with the account, and a deleted account's email is still
// answered (Testing #6).
func TestStandingFollowsTheMinter(t *testing.T) {
	h := newAccountHarness(t)
	editor := h.editor(t)
	rec := h.call(t, "POST", "/api/v1/projects/"+h.project.ID+"/keys", nil, asSession(editor),
		inProject(h.project.ID))
	expectStatus(t, rec, http.StatusCreated)
	publicKey := decodeJSON[minted](t, rec).PublicKey

	expect := func(step, want string) {
		t.Helper()
		key := h.keysOf(t, h.project.ID)[publicKey]
		if key.CreatedBy.Standing != want || key.CreatedBy.Email != "editor@example.com" {
			t.Errorf("%s: the editor's key = %+v, want standing %q and the email", step, key.CreatedBy, want)
		}
	}
	expect("minted", store.RoleEditor)

	accountPath := "/api/v1/accounts/" + editor.account.ID
	membership := accountPath + "/projects/" + h.project.ID
	expectStatus(t, h.call(t, "PUT", membership, mustJSON(t, map[string]any{"role": "viewer"}), asAdmin), 200)
	expect("demoted", store.RoleViewer)

	expectStatus(t, h.call(t, "DELETE", membership, nil, asAdmin), 204)
	expect("removed", store.StandingRemoved)

	expectStatus(t, h.call(t, "PATCH", accountPath, mustJSON(t, map[string]any{"disabled": true}), asAdmin), 200)
	expect("disabled", store.StandingDisabled)

	expectStatus(t, h.call(t, "DELETE", accountPath+"?confirm=editor@example.com", nil, asAdmin), 204)
	expect("deleted", store.StandingDeleted)
	if key := h.keysOf(t, h.project.ID)[publicKey]; key.CreatedBy.AccountID != "" {
		t.Errorf("a deleted minter is still named by id: %+v", key.CreatedBy)
	}
}

// TestMintingValidatesTheName: the body is optional, a name is trimmed and at
// most 64 characters, and nothing else is taken yet (Testing #8).
func TestMintingValidatesTheName(t *testing.T) {
	h := newAccountHarness(t)
	keys := "/api/v1/projects/" + h.project.ID + "/keys"

	rec := h.call(t, "POST", keys, nil, asAdmin)
	expectStatus(t, rec, http.StatusCreated)
	if got := decodeJSON[minted](t, rec); got.Name != "" || strings.Join(got.Scopes, " ") != store.AllScopes {
		t.Errorf("a mint with no body = %+v, want no name and every scope", got)
	}

	sixtyFour := strings.Repeat("é", 64)
	rec = h.call(t, "POST", keys, mustJSON(t, map[string]any{"name": sixtyFour}), asAdmin)
	expectStatus(t, rec, http.StatusCreated)
	if got := decodeJSON[minted](t, rec); got.Name != sixtyFour {
		t.Errorf("a 64-character name came back as %q", got.Name)
	}

	rec = h.call(t, "POST", keys, mustJSON(t, map[string]any{"name": sixtyFour + "x"}), asAdmin)
	expectError(t, rec, http.StatusBadRequest, "at most 64 characters")

	rec = h.call(t, "POST", keys, mustJSON(t, map[string]any{"scopes": []string{"read"}}), asAdmin)
	expectStatus(t, rec, http.StatusBadRequest)
}

// countingWriter counts the key-use jobs that reach the writer.
type countingWriter struct {
	JobWriter
	mu   sync.Mutex
	uses []map[string]int64
	fail bool
}

func (w *countingWriter) Submit(ctx context.Context, job store.WriteJob) error {
	if use, ok := job.(*store.KeyUse); ok {
		w.mu.Lock()
		copied := map[string]int64{}
		for k, v := range use.Seen {
			copied[k] = v
		}
		w.uses = append(w.uses, copied)
		fail := w.fail
		w.mu.Unlock()
		if fail {
			return store.ErrWriterBusy
		}
	}
	return w.JobWriter.Submit(ctx, job)
}

// TestLastUseIsWrittenOnceAMinute: a use is recorded in memory and written by
// the flush, as one job for every key seen; the listing shows a use nobody has
// written yet; a refused request counts; a failed flush is retried; a revoked
// key's use is dropped; and `Shutdown` writes what is left (Testing #7).
func TestLastUseIsWrittenOnceAMinute(t *testing.T) {
	h := newAccountHarness(t)
	counting := &countingWriter{JobWriter: h.writer}
	h.server.writer = counting
	other := h.second(t, "other", "tp-sk-other")

	stored := func(projectID, publicKey string) *int64 {
		t.Helper()
		keys, err := h.store.ProjectKeys(projectID)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range keys {
			if key.PublicKey == publicKey {
				return key.LastUsedAt
			}
		}
		t.Fatalf("no key %s", publicKey)
		return nil
	}

	start := time.Now().UnixNano()
	expectStatus(t, h.get(t, "/api/v1/traces"), 200)
	// Refused — a key on a route only a session reaches — and still a use.
	expectStatus(t, h.call(t, "GET", "/api/v1/auth/me", nil, asKey("tp-sk-other")), 400)

	if at := stored(h.project.ID, testPublic); at != nil {
		t.Fatalf("a request wrote the use before any flush: %d", *at)
	}
	if listed := h.keysOf(t, h.project.ID)[testPublic]; listed.LastUsedAt == nil {
		t.Errorf("the listing does not show a use that is waiting for the flush")
	}

	h.server.flushKeyUses(t.Context())
	if len(counting.uses) != 1 || len(counting.uses[0]) != 2 {
		t.Fatalf("the flush submitted %v, want one job with both keys", counting.uses)
	}
	for _, c := range []struct{ project, key string }{{h.project.ID, testPublic}, {other.ID, "tp-pk-other"}} {
		if at := stored(c.project, c.key); at == nil || *at < start {
			t.Errorf("%s after the flush was last used at %v, want the request's time", c.key, at)
		}
	}
	// Nothing seen since: nothing to write.
	h.server.flushKeyUses(t.Context())
	if len(counting.uses) != 1 {
		t.Errorf("an idle flush submitted a job: %v", counting.uses)
	}

	// A flush that fails keeps its values for the next one.
	first := *stored(h.project.ID, testPublic)
	expectStatus(t, h.get(t, "/api/v1/traces"), 200)
	counting.fail = true
	h.server.flushKeyUses(t.Context())
	counting.fail = false
	if at := stored(h.project.ID, testPublic); *at != first {
		t.Errorf("a failed flush moved the stored use")
	}
	if listed := h.keysOf(t, h.project.ID)[testPublic]; listed.LastUsedAt == nil ||
		*listed.LastUsedAt == formatTime(first) {
		t.Errorf("after a failed flush the listing shows %v, want the newer, unwritten use", listed.LastUsedAt)
	}
	h.server.flushKeyUses(t.Context())
	if at := stored(h.project.ID, testPublic); *at <= first {
		t.Errorf("the retry did not write the newer use")
	}

	// A key revoked with a use pending: the flush writes nothing for it and
	// does not fail for the others.
	expectStatus(t, h.call(t, "GET", "/api/v1/traces", nil, asKey("tp-sk-other")), 200)
	expectStatus(t, h.get(t, "/api/v1/traces"), 200)
	expectStatus(t, h.call(t, "DELETE", "/api/v1/projects/"+other.ID+"/keys/tp-pk-other?confirm=other",
		nil, asAdmin), 200)
	before := *stored(h.project.ID, testPublic)

	// Shutdown writes what is left; the row is read after the server is
	// closed.
	if err := h.server.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if at := stored(h.project.ID, testPublic); *at <= before {
		t.Errorf("Shutdown did not write the last use")
	}
	if last := counting.uses[len(counting.uses)-1]; len(last) != 2 {
		t.Errorf("the shutdown flush = %v, want both keys including the revoked one", last)
	}
}

// TestAccountDeletionListsTheKeysItMinted: the dry run names every key the
// account minted, in every project, outside `would_delete` — and deleting the
// account leaves them working (Testing #10).
func TestAccountDeletionListsTheKeysItMinted(t *testing.T) {
	h := newAccountHarness(t)
	other := h.second(t, "other", "tp-sk-other")
	editor := h.account(t, "editor@example.com", false,
		store.Membership{ProjectID: h.project.ID, Role: store.RoleEditor},
		store.Membership{ProjectID: other.ID, Role: store.RoleEditor})

	secrets := map[string]string{}
	for _, project := range []*store.Project{h.project, other} {
		rec := h.call(t, "POST", "/api/v1/projects/"+project.ID+"/keys",
			mustJSON(t, map[string]any{"name": "app in " + project.Name}),
			asSession(editor), inProject(project.ID))
		expectStatus(t, rec, http.StatusCreated)
		secrets[project.ID] = decodeJSON[minted](t, rec).SecretKey
	}

	rec := h.call(t, "DELETE", "/api/v1/accounts/"+editor.account.ID, nil, asAdmin)
	expectStatus(t, rec, 200)
	preview := decodeJSON[struct {
		DryRun      bool           `json:"dry_run"`
		WouldDelete map[string]int `json:"would_delete"`
		Keys        []struct {
			ProjectID   string   `json:"project_id"`
			ProjectName string   `json:"project_name"`
			PublicKey   string   `json:"public_key"`
			Name        string   `json:"name"`
			Scopes      []string `json:"scopes"`
			LastUsedAt  *string  `json:"last_used_at"`
		} `json:"keys"`
		Note string `json:"note"`
	}](t, rec)
	if !preview.DryRun || len(preview.Keys) != 2 {
		t.Fatalf("preview = %+v, want both keys listed", preview)
	}
	for _, key := range preview.Keys {
		if key.Name != "app in "+key.ProjectName || key.PublicKey == "" ||
			strings.Join(key.Scopes, " ") != store.AllScopes || key.LastUsedAt != nil {
			t.Errorf("a listed key = %+v", key)
		}
	}
	if _, listed := preview.WouldDelete["api_keys"]; listed {
		t.Errorf("would_delete = %v, but the deletion takes no key", preview.WouldDelete)
	}
	if !strings.Contains(preview.Note, "keep working") {
		t.Errorf("note = %q, want it to say the keys stay", preview.Note)
	}

	expectStatus(t, h.call(t, "DELETE", "/api/v1/accounts/"+editor.account.ID+"?confirm=editor@example.com",
		nil, asAdmin), 204)
	for _, secret := range secrets {
		expectStatus(t, h.call(t, "GET", "/api/v1/traces", nil, asKey(secret)), 200)
	}
}
