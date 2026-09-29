package server

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/store"
)

// The author of a score through the API (spec 048, Testing): who is stamped,
// who may read the email, the filter, and what deleting an account and erasing
// an end user do to it.

// authorView is a score as a test reads it back: the author and nothing else.
type authorView struct {
	ID     string `json:"id"`
	Author *struct {
		Kind     string  `json:"kind"`
		ID       string  `json:"id"`
		Name     string  `json:"name"`
		Email    *string `json:"email"`
		Standing string  `json:"standing"`
	} `json:"author"`
}

// scoreAs writes one numeric score on a trace with the caller mutate names and
// returns its id.
func (h *harness) scoreAs(t *testing.T, traceID string, mutate ...func(*http.Request)) string {
	t.Helper()
	rec := h.call(t, "POST", "/api/v1/scores",
		mustJSON(t, map[string]any{"trace_id": traceID, "name": "quality", "value": 1}), mutate...)
	expectStatus(t, rec, http.StatusCreated)
	return decodeJSON[scoreIDsResponse](t, rec).IDs[0]
}

// readAuthor reads one score back with the caller mutate names.
func (h *harness) readAuthor(t *testing.T, id string, mutate ...func(*http.Request)) authorView {
	t.Helper()
	rec := h.call(t, "GET", "/api/v1/scores/"+id, nil, mutate...)
	expectStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"author":`) {
		t.Errorf("the score has no author field: %s", rec.Body.String())
	}
	return decodeJSON[authorView](t, rec)
}

// TestAScoreIsStampedWithItsCaller: a session writes as its account, a key as
// the key, and the body cannot say otherwise (#1).
func TestAScoreIsStampedWithItsCaller(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.owner(t)
	editor := h.editor(t)
	name := "Eddie"
	if err := h.writer.Submit(t.Context(), &store.AccountUpdate{AccountID: editor.account.ID, Name: &name}); err != nil {
		t.Fatal(err)
	}

	byKey := h.scoreAs(t, scoreTraceID)
	got := h.readAuthor(t, byKey, asSession(owner), inProject(h.project.ID))
	if got.Author == nil || got.Author.Kind != store.AuthorKey || got.Author.ID != testPublic ||
		got.Author.Standing != store.StandingActive || got.Author.Email != nil {
		t.Errorf("a key's score reads author %+v, want the key, active, without an email", got.Author)
	}

	bySession := h.scoreAs(t, scoreTraceID, asSession(editor), inProject(h.project.ID))
	got = h.readAuthor(t, bySession, asSession(owner), inProject(h.project.ID))
	if got.Author == nil || got.Author.Kind != store.AuthorAccount || got.Author.ID != editor.account.ID ||
		got.Author.Name != "Eddie" || got.Author.Email == nil || *got.Author.Email != "editor@example.com" ||
		got.Author.Standing != store.RoleEditor {
		t.Errorf("a session's score reads author %+v, want the editor with its name and email", got.Author)
	}

	// The body has no say, and a refused body writes nothing.
	before := h.scoreCount(t)
	rec := h.call(t, "POST", "/api/v1/scores", mustJSON(t, map[string]any{
		"trace_id": scoreTraceID, "name": "quality", "value": 1,
		"author": map[string]any{"kind": "account", "id": owner.account.ID}}))
	expectError(t, rec, http.StatusBadRequest, "author")
	if after := h.scoreCount(t); after != before {
		t.Errorf("%d scores after a refused write, want %d", after, before)
	}

	// A rewrite of the same id by somebody else makes them the author (#3).
	rec = h.call(t, "POST", "/api/v1/scores", mustJSON(t, map[string]any{
		"id": byKey, "trace_id": scoreTraceID, "name": "quality", "value": 0}),
		asSession(editor), inProject(h.project.ID))
	expectStatus(t, rec, http.StatusCreated)
	if got := h.readAuthor(t, byKey); got.Author == nil || got.Author.ID != editor.account.ID {
		t.Errorf("a rewrite by the editor reads author %+v, want the editor", got.Author)
	}
}

func (h *harness) scoreCount(t *testing.T) int {
	t.Helper()
	rec := h.call(t, "GET", "/api/v1/scores?limit=500", nil)
	expectStatus(t, rec, http.StatusOK)
	return len(decodeJSON[scoreListResponse](t, rec).Scores)
}

// TestTheAuthorsEmailIsForEditorsAndOwners is the visibility matrix of #5: the
// name for every member, the email for an editor and an owner, never for a
// viewer or a key, whatever the key's scopes.
func TestTheAuthorsEmailIsForEditorsAndOwners(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.owner(t)
	editor := h.editor(t)
	viewer := h.viewer(t)
	reader := h.mint(t, "read")
	everything := h.mint(t, allScopes...)
	id := h.scoreAs(t, scoreTraceID, asSession(editor), inProject(h.project.ID))

	for _, c := range []struct {
		who       string
		mutate    []func(*http.Request)
		wantEmail bool
	}{
		{"owner", []func(*http.Request){asSession(owner), inProject(h.project.ID)}, true},
		{"editor", []func(*http.Request){asSession(editor), inProject(h.project.ID)}, true},
		{"viewer", []func(*http.Request){asSession(viewer), inProject(h.project.ID)}, false},
		{"a read key", []func(*http.Request){asKey(reader.SecretKey)}, false},
		{"a key with every scope", []func(*http.Request){asKey(everything.SecretKey)}, false},
	} {
		t.Run(c.who, func(t *testing.T) {
			for _, path := range []string{"/api/v1/scores/" + id, "/api/v1/scores?trace_id=" + scoreTraceID} {
				rec := h.call(t, "GET", path, nil, c.mutate...)
				expectStatus(t, rec, http.StatusOK)
				body := rec.Body.String()
				if !strings.Contains(body, `"id":"`+editor.account.ID+`"`) {
					t.Errorf("%s: the author is not named: %s", path, body)
				}
				if got := strings.Contains(body, "editor@example.com"); got != c.wantEmail {
					t.Errorf("%s: the email is shown %v, want %v: %s", path, got, c.wantEmail, body)
				}
			}
		})
	}
}

// TestTheAuthorFilter: `me` is the caller itself, account or key; an id is
// taken as sent, and one nobody has is an empty page (#9).
func TestTheAuthorFilter(t *testing.T) {
	h := newAccountHarness(t)
	editor := h.editor(t)
	byEditor := h.scoreAs(t, scoreTraceID, asSession(editor), inProject(h.project.ID))
	byKey := h.scoreAs(t, scoreTraceID)

	list := func(query string, mutate ...func(*http.Request)) []string {
		t.Helper()
		rec := h.call(t, "GET", "/api/v1/scores?"+query, nil, mutate...)
		expectStatus(t, rec, http.StatusOK)
		var ids []string
		for _, score := range decodeJSON[scoreListResponse](t, rec).Scores {
			ids = append(ids, score.ID)
		}
		return ids
	}
	if got := list("author=me", asSession(editor), inProject(h.project.ID)); len(got) != 1 || got[0] != byEditor {
		t.Errorf("the editor's own = %v, want %s", got, byEditor)
	}
	if got := list("author=me"); len(got) != 1 || got[0] != byKey {
		t.Errorf("the key's own = %v, want %s", got, byKey)
	}
	if got := list("author=" + editor.account.ID); len(got) != 1 || got[0] != byEditor {
		t.Errorf("by the editor's id = %v, want %s", got, byEditor)
	}
	if got := list("author=nobody"); len(got) != 0 {
		t.Errorf("an unknown author = %v, want nothing", got)
	}
}

// TestDeletingAnAccountKeepsItsScores: the dry run counts them, the deletion
// leaves them, and they read as the deleted account's with its name and email
// as they were (#7).
func TestDeletingAnAccountKeepsItsScores(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.owner(t)
	helper := h.account(t, "helper@example.com", false,
		store.Membership{ProjectID: h.project.ID, Role: store.RoleEditor})
	id := h.scoreAs(t, scoreTraceID, asSession(helper), inProject(h.project.ID))
	h.scoreAs(t, scoreTraceID, asSession(helper), inProject(h.project.ID))

	rec := h.call(t, "DELETE", "/api/v1/accounts/"+helper.account.ID, nil, asSession(owner))
	expectStatus(t, rec, http.StatusOK)
	preview := decodeJSON[struct {
		ScoresAuthored *int64 `json:"scores_authored"`
		Note           string `json:"note"`
	}](t, rec)
	if preview.ScoresAuthored == nil || *preview.ScoresAuthored != 2 {
		t.Errorf("scores_authored = %v, want 2", preview.ScoresAuthored)
	}
	if !strings.Contains(preview.Note, "scores") {
		t.Errorf("the note does not say the scores stay: %q", preview.Note)
	}

	rec = h.call(t, "DELETE", "/api/v1/accounts/"+helper.account.ID+"?confirm=helper@example.com",
		nil, asSession(owner))
	expectStatus(t, rec, http.StatusNoContent)
	got := h.readAuthor(t, id, asSession(owner), inProject(h.project.ID))
	if got.Author == nil || got.Author.ID != helper.account.ID || got.Author.Standing != store.StandingDeleted ||
		got.Author.Email == nil || *got.Author.Email != "helper@example.com" {
		t.Errorf("after the deletion the author reads %+v, want the deleted account with its email", got.Author)
	}
	if n := h.scoreCount(t); n != 2 {
		t.Errorf("%d scores after the deletion, want both", n)
	}
}

// TestErasingAnEndUserIgnoresAuthors: an erasure takes the scores on the
// erased user's traces whoever wrote them, leaves the same author's scores on
// other traces, and says nothing about any author (#8).
func TestErasingAnEndUserIgnoresAuthors(t *testing.T) {
	h := newAdminHarness(t)
	editor := h.editor(t)
	h.postExport(t, false,
		markedSpan(t, 1, 1, "user-a", "session-a", "a", nil),
		markedSpan(t, 2, 2, "user-b", "session-b", "b", nil))
	h.scoreAs(t, traceHex(1), asSession(editor), inProject(h.project.ID))
	h.scoreAs(t, traceHex(1))
	kept := h.scoreAs(t, traceHex(2), asSession(editor), inProject(h.project.ID))

	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	rec := h.call(t, "DELETE", "/api/v1/projects/"+h.project.ID+"/users/user-a/data?wait=30&confirm=user-a", nil)
	slog.SetDefault(previous)
	expectStatus(t, rec, http.StatusOK)

	deleted := decodeJSON[struct {
		Deleted map[string]int64 `json:"deleted"`
	}](t, rec).Deleted
	if deleted["scores"] != 2 {
		t.Errorf("deleted = %v, want both scores on the erased trace", deleted)
	}
	for _, trace := range []string{rec.Body.String(), logged.String()} {
		if strings.Contains(trace, editor.account.ID) || strings.Contains(trace, "editor@example.com") ||
			strings.Contains(trace, testPublic) {
			t.Errorf("the erasure names an author: %s", trace)
		}
	}
	if got := h.readAuthor(t, kept); got.Author == nil || got.Author.ID != editor.account.ID {
		t.Errorf("the editor's score on another user's trace reads %+v", got.Author)
	}
}
