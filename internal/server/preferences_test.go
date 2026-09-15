package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/store"
)

// `accounts.preferences` (spec 034 #9, #10): an opaque JSON object the
// interface owns the shape of, carried on `Me.account`, written whole by
// `PATCH /auth/me`, and nobody else's business.

type preferencesBody struct {
	Account struct {
		Name        string          `json:"name"`
		Preferences json.RawMessage `json:"preferences"`
	} `json:"account"`
}

func (h *harness) preferencesOf(t *testing.T, who *signedIn) string {
	t.Helper()
	rec := h.call(t, "GET", "/api/v1/auth/me", nil, asSession(who))
	expectStatus(t, rec, 200)
	return string(decodeJSON[preferencesBody](t, rec).Account.Preferences)
}

func TestPreferencesRoundTrip(t *testing.T) {
	h := newAccountHarness(t)
	who := h.viewer(t)

	// A fresh account has an empty object, not an absent key (Decision 10).
	if got := h.preferencesOf(t, who); got != "{}" {
		t.Errorf("preferences = %s on a fresh account, want {}", got)
	}

	arrangement := map[string]any{"dashboard": map[string]any{
		h.project.ID: map[string]any{"order": []string{"errors", "summary"}, "hidden": []string{"tokens"}},
	}}
	rec := h.call(t, "PATCH", "/api/v1/auth/me",
		mustJSON(t, map[string]any{"preferences": arrangement}), asSession(who))
	expectStatus(t, rec, 200)
	written := decodeJSON[preferencesBody](t, rec).Account.Preferences
	want, _ := json.Marshal(arrangement)
	if string(written) != string(want) {
		t.Errorf("the response carries %s, want %s", written, want)
	}
	if got := h.preferencesOf(t, who); got != string(want) {
		t.Errorf("preferences = %s after the write, want %s", got, want)
	}

	// Replaced whole, never merged: a second write with another key drops
	// the first.
	rec = h.call(t, "PATCH", "/api/v1/auth/me",
		mustJSON(t, map[string]any{"preferences": map[string]any{"theme": "dark"}}), asSession(who))
	expectStatus(t, rec, 200)
	if got := h.preferencesOf(t, who); got != `{"theme":"dark"}` {
		t.Errorf("preferences = %s after the second write, want the second object alone", got)
	}

	// A rename beside it leaves it alone, and one PATCH may carry both.
	rec = h.call(t, "PATCH", "/api/v1/auth/me",
		mustJSON(t, map[string]any{"name": "Ada"}), asSession(who))
	expectStatus(t, rec, 200)
	body := decodeJSON[preferencesBody](t, rec)
	if body.Account.Name != "Ada" || string(body.Account.Preferences) != `{"theme":"dark"}` {
		t.Errorf("account = %+v after a rename, want the preferences untouched", body.Account)
	}
	rec = h.call(t, "PATCH", "/api/v1/auth/me",
		mustJSON(t, map[string]any{"name": "Ada L.", "preferences": map[string]any{}}), asSession(who))
	expectStatus(t, rec, 200)
	body = decodeJSON[preferencesBody](t, rec)
	if body.Account.Name != "Ada L." || string(body.Account.Preferences) != `{}` {
		t.Errorf("account = %+v after a combined write, want both changed", body.Account)
	}
}

func TestPreferencesMustBeAnObjectUnderTheCap(t *testing.T) {
	h := newAccountHarness(t)
	who := h.viewer(t)

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"an array", `{"preferences": []}`, "preferences: must be an object"},
		{"a string", `{"preferences": "dark"}`, "preferences: must be an object"},
		{"null", `{"preferences": null}`, "preferences: must be an object"},
		{"a number", `{"preferences": 1}`, "preferences: must be an object"},
		{"17 KiB", `{"preferences": {"pad": "` + strings.Repeat("x", 17*1024) + `"}}`,
			"preferences: larger than 16 KiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.call(t, "PATCH", "/api/v1/auth/me", []byte(tc.body), asSession(who))
			expectError(t, rec, http.StatusUnprocessableEntity, tc.want)
		})
	}
	// Nothing above was stored.
	if got := h.preferencesOf(t, who); got != "{}" {
		t.Errorf("preferences = %s after refused writes, want {}", got)
	}
	// The cap is on the object, not on its whitespace: a padded body whose
	// object is small is fine.
	padded := `{"preferences": {"a":   1` + strings.Repeat(" ", 17*1024) + `}}`
	rec := h.call(t, "PATCH", "/api/v1/auth/me", []byte(padded), asSession(who))
	expectStatus(t, rec, 200)
	if got := h.preferencesOf(t, who); got != `{"a":1}` {
		t.Errorf("preferences = %s, want the object compacted", got)
	}
}

// An owner's edit of somebody else is about their standing, not their
// screens; the whole row goes when the account does.
func TestPreferencesAreTheAccountsOwn(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.owner(t)
	helper := h.account(t, "helper@example.com", false,
		store.Membership{ProjectID: h.project.ID, Role: store.RoleViewer})

	rec := h.call(t, "PATCH", "/api/v1/auth/me",
		mustJSON(t, map[string]any{"preferences": map[string]any{"theme": "dark"}}), asSession(helper))
	expectStatus(t, rec, 200)

	// The owner's route does not know the field.
	rec = h.call(t, "PATCH", "/api/v1/accounts/"+helper.account.ID,
		mustJSON(t, map[string]any{"preferences": map[string]any{}}), asSession(owner))
	expectError(t, rec, http.StatusBadRequest, "unknown field")
	// And its own edits leave the object alone.
	rec = h.call(t, "PATCH", "/api/v1/accounts/"+helper.account.ID,
		mustJSON(t, map[string]any{"name": "Helper"}), asSession(owner))
	expectStatus(t, rec, 200)
	if got := h.preferencesOf(t, helper); got != `{"theme":"dark"}` {
		t.Errorf("preferences = %s after an owner's rename, want them untouched", got)
	}

	rec = h.call(t, "DELETE",
		"/api/v1/accounts/"+helper.account.ID+"?confirm=helper@example.com", nil, asSession(owner))
	expectStatus(t, rec, http.StatusNoContent)
	gone, err := h.store.AccountByID(helper.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gone != nil {
		t.Errorf("the account row survives the deletion: %+v", gone)
	}
}
