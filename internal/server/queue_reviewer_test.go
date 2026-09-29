package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/store"
)

// A signed-in reviewer is the account (spec 048 #15): the claim and the
// completion are held by its id, and the name is rendered when the item is
// read, by the rule the score's author follows (#5).

// asMember is a session's request in the harness project.
func (h *harness) asMember(who *signedIn) []func(*http.Request) {
	return []func(*http.Request){asSession(who), inProject(h.project.ID)}
}

func (h *harness) nextAs(t *testing.T, queue string, who *signedIn) nextResponse {
	t.Helper()
	rec := h.call(t, "GET", "/api/v1/queues/"+queue+"/next", nil, h.asMember(who)...)
	expectStatus(t, rec, http.StatusOK)
	return decodeJSON[nextResponse](t, rec)
}

func (h *harness) rename(t *testing.T, who *signedIn, name string) {
	t.Helper()
	if err := h.writer.Submit(t.Context(), &store.AccountUpdate{AccountID: who.account.ID, Name: &name}); err != nil {
		t.Fatal(err)
	}
}

// reviewQueue declares a queue of two traces over one config.
func (h *harness) reviewQueue(t *testing.T) (first, second string) {
	t.Helper()
	h.putConfigs(t, "accuracy")
	expectStatus(t, h.putQueue(t, "review", "accuracy"), http.StatusCreated)
	return h.addTarget(t, "review", map[string]any{"trace_id": traceHex(1)}),
		h.addTarget(t, "review", map[string]any{"trace_id": traceHex(2)})
}

// TestTwoAccountsOfOneNameDoNotShareAClaim: the claim is the account's, so
// two people called the same are two reviewers.
func TestTwoAccountsOfOneNameDoNotShareAClaim(t *testing.T) {
	h := newAccountHarness(t)
	first, second := h.reviewQueue(t)
	a := h.account(t, "alex.a@example.com", false, store.Membership{ProjectID: h.project.ID, Role: store.RoleEditor})
	b := h.account(t, "alex.b@example.com", false, store.Membership{ProjectID: h.project.ID, Role: store.RoleEditor})
	h.rename(t, a, "Alex")
	h.rename(t, b, "Alex")

	if got := h.nextAs(t, "review", a).Item; got == nil || got.ID != first {
		t.Fatalf("a takes %+v, want the first item", got)
	}
	got := h.nextAs(t, "review", b).Item
	if got == nil || got.ID != second {
		t.Fatalf("b takes %+v, want the second: the first is a's", got)
	}
	if got.ClaimedByAccount != b.account.ID || got.ClaimedBy != "Alex" {
		t.Errorf("b's claim reads %q/%q, want b's id and its name", got.ClaimedByAccount, got.ClaimedBy)
	}
	if again := h.nextAs(t, "review", a).Item; again == nil || again.ID != first {
		t.Errorf("a asks again and gets %+v, want the first back", again)
	}
}

// TestARenameKeepsTheClaim: the display name is not what holds the item.
func TestARenameKeepsTheClaim(t *testing.T) {
	h := newAccountHarness(t)
	first, _ := h.reviewQueue(t)
	ada := h.account(t, "ada@example.com", false, store.Membership{ProjectID: h.project.ID, Role: store.RoleEditor})
	h.rename(t, ada, "Ada")

	if got := h.nextAs(t, "review", ada).Item; got == nil || got.ID != first {
		t.Fatalf("ada takes %+v, want the first item", got)
	}
	h.rename(t, ada, "Ada L.")
	got := h.nextAs(t, "review", ada).Item
	if got == nil || got.ID != first || got.ClaimedBy != "Ada L." {
		t.Errorf("after the rename ada gets %+v, want the first item, held under the new name", got)
	}
}

// TestANamelessAccountsEmailIsNotInTheQueue: an account with no display name
// finishes items, and a viewer reads none of the queue's fields — nor a
// refusal — with its email in them; an editor does, by #5's rule.
func TestANamelessAccountsEmailIsNotInTheQueue(t *testing.T) {
	h := newAccountHarness(t)
	first, second := h.reviewQueue(t)
	nameless := h.account(t, "quiet@example.com", false, store.Membership{ProjectID: h.project.ID, Role: store.RoleEditor})
	viewer := h.viewer(t)
	editor := h.editor(t)

	h.nextAs(t, "review", nameless)
	h.postScore(t, map[string]any{"trace_id": traceHex(1), "name": "accuracy", "value": 1})
	expectStatus(t, h.call(t, "POST", "/api/v1/queues/review/items/"+first+"/complete",
		[]byte(`{}`), h.asMember(nameless)...), http.StatusOK)
	h.nextAs(t, "review", nameless) // holds the second

	for _, path := range []string{
		"/api/v1/queues/review/items",
		"/api/v1/queues/review/items/" + first,
		"/api/v1/queues/review/items/" + second,
	} {
		rec := h.call(t, "GET", path, nil, h.asMember(viewer)...)
		expectStatus(t, rec, http.StatusOK)
		if strings.Contains(rec.Body.String(), "quiet@example.com") {
			t.Errorf("a viewer reads the email at %s: %s", path, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"a member"`) {
			t.Errorf("a viewer is not told who at %s: %s", path, rec.Body.String())
		}
	}
	// The refusal a second completion gets names the finisher as the listing
	// does, for whoever is refused: a member to a viewer, the email to an
	// editor.
	rec := h.call(t, "POST", "/api/v1/queues/review/items/"+first+"/complete",
		[]byte(`{}`), h.asMember(viewer)...)
	expectError(t, rec, http.StatusConflict, "completed by a member")
	if strings.Contains(rec.Body.String(), "quiet@example.com") {
		t.Errorf("the refusal names the email to a viewer: %s", rec.Body.String())
	}
	rec = h.call(t, "POST", "/api/v1/queues/review/items/"+first+"/skip",
		[]byte(`{"reason":"x"}`), h.asMember(editor)...)
	expectError(t, rec, http.StatusConflict, "completed by quiet@example.com")

	rec = h.call(t, "GET", "/api/v1/queues/review/items/"+first, nil, h.asMember(editor)...)
	if item := decodeJSON[queueItemResponse](t, rec); item.CompletedBy != "quiet@example.com" ||
		item.CompletedByAccount != nameless.account.ID {
		t.Errorf("an editor reads %q/%q, want the email and the id", item.CompletedBy, item.CompletedByAccount)
	}
}

// TestASessionSendsNoAnnotator: a signed-in reviewer is the account, and a
// string it sends is refused rather than dropped; a key still names itself.
func TestASessionSendsNoAnnotator(t *testing.T) {
	h := newAccountHarness(t)
	first, _ := h.reviewQueue(t)
	editor := h.editor(t)

	expectError(t, h.call(t, "GET", "/api/v1/queues/review/next?annotator=ada", nil, h.asMember(editor)...),
		http.StatusBadRequest, "signed-in reviewer is the account")
	h.nextAs(t, "review", editor)
	expectError(t, h.call(t, "POST", "/api/v1/queues/review/items/"+first+"/reopen",
		[]byte(`{"annotator":"ada"}`), h.asMember(editor)...), http.StatusBadRequest, "send none")
	expectError(t, h.call(t, "POST", "/api/v1/queues/review/items/"+first+"/skip",
		[]byte(`{"annotator":"ada","reason":"x"}`), h.asMember(editor)...), http.StatusBadRequest, "send none")

	// A key without a name is refused as before (spec 024 #6).
	expectError(t, h.get(t, "/api/v1/queues/review/next"), http.StatusBadRequest, "annotator is required")
}

// TestTheAccountFilter: `account=me` is the caller's items — held or finished
// — and a key, which has no account, is told to ask by annotator.
func TestTheAccountFilter(t *testing.T) {
	h := newAccountHarness(t)
	first, second := h.reviewQueue(t)
	ada := h.account(t, "ada@example.com", false, store.Membership{ProjectID: h.project.ID, Role: store.RoleEditor})
	h.nextAs(t, "review", ada)
	h.next(t, "review", "judge") // the key holds the second

	rec := h.call(t, "GET", "/api/v1/queues/review/items?account=me", nil, h.asMember(ada)...)
	expectStatus(t, rec, http.StatusOK)
	items := decodeJSON[queueItemListResponse](t, rec).Items
	if len(items) != 1 || items[0].ID != first {
		t.Errorf("ada's = %+v, want the first", items)
	}
	rec = h.call(t, "GET", "/api/v1/queues/review/items?account="+ada.account.ID, nil)
	if items := decodeJSON[queueItemListResponse](t, rec).Items; len(items) != 1 || items[0].ID != first {
		t.Errorf("by ada's id = %+v, want the first", items)
	}
	if items := h.items(t, "review", "?annotator=judge"); len(items) != 1 || items[0].ID != second {
		t.Errorf("the key's = %+v, want the second", items)
	}
	expectError(t, h.get(t, "/api/v1/queues/review/items?account=me"), http.StatusBadRequest, "annotator=")
}

// TestADeletedReviewerIsSaidSo: the account is gone, and so is its name.
func TestADeletedReviewerIsSaidSo(t *testing.T) {
	h := newAccountHarness(t)
	first, _ := h.reviewQueue(t)
	owner := h.owner(t)
	gone := h.account(t, "gone@example.com", false, store.Membership{ProjectID: h.project.ID, Role: store.RoleEditor})
	h.postScore(t, map[string]any{"trace_id": traceHex(1), "name": "accuracy", "value": 1})
	expectStatus(t, h.call(t, "POST", "/api/v1/queues/review/items/"+first+"/complete",
		[]byte(`{}`), h.asMember(gone)...), http.StatusOK)
	expectStatus(t, h.call(t, "DELETE", "/api/v1/accounts/"+gone.account.ID+"?confirm=gone@example.com",
		nil, asSession(owner)), http.StatusNoContent)

	rec := h.call(t, "GET", "/api/v1/queues/review/items/"+first, nil, h.asMember(owner)...)
	if item := decodeJSON[queueItemResponse](t, rec); item.CompletedBy != "a deleted account" ||
		item.CompletedByStanding != store.StandingDeleted {
		t.Errorf("completed_by = %q (%q), want a deleted account", item.CompletedBy, item.CompletedByStanding)
	}
}

// TestAReviewerWhoIsGoneSaysWhy: the queue gives a reviewer's standing as the
// scores block gives an author's (spec 048 #4): removed and disabled are said,
// and a reviewer still in the project carries their role.
func TestAReviewerWhoIsGoneSaysWhy(t *testing.T) {
	h := newAccountHarness(t)
	first, second := h.reviewQueue(t)
	owner := h.owner(t)
	bob := h.account(t, "bob@example.com", false, store.Membership{ProjectID: h.project.ID, Role: store.RoleEditor})
	cleo := h.account(t, "cleo@example.com", false, store.Membership{ProjectID: h.project.ID, Role: store.RoleEditor})
	h.postScore(t, map[string]any{"trace_id": traceHex(1), "name": "accuracy", "value": 1})
	h.postScore(t, map[string]any{"trace_id": traceHex(2), "name": "accuracy", "value": 1})
	for who, item := range map[*signedIn]string{bob: first, cleo: second} {
		expectStatus(t, h.call(t, "POST", "/api/v1/queues/review/items/"+item+"/complete",
			[]byte(`{}`), h.asMember(who)...), http.StatusOK)
	}
	standings := func() map[string]string {
		rec := h.call(t, "GET", "/api/v1/queues/review/items", nil, h.asMember(owner)...)
		out := map[string]string{}
		for _, item := range decodeJSON[queueItemListResponse](t, rec).Items {
			out[item.ID] = item.CompletedByStanding
		}
		return out
	}
	if got := standings(); got[first] != store.RoleEditor || got[second] != store.RoleEditor {
		t.Errorf("standings = %v, want both editors", got)
	}
	expectStatus(t, h.call(t, "DELETE", "/api/v1/accounts/"+bob.account.ID+"/projects/"+h.project.ID,
		nil, asSession(owner)), http.StatusNoContent)
	yes := true
	if err := h.writer.Submit(t.Context(), &store.AccountUpdate{AccountID: cleo.account.ID, Disabled: &yes}); err != nil {
		t.Fatal(err)
	}
	if got := standings(); got[first] != store.StandingRemoved || got[second] != store.StandingDisabled {
		t.Errorf("standings = %v, want removed and disabled", got)
	}
}
