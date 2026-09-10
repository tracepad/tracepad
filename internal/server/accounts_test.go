package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// Account management (spec 028, Testing). The invitation is the interesting
// path: an owner creates an account without a password, is handed a link once,
// and the person on the other end sets a password the owner never saw.

type accountBody struct {
	ID          string  `json:"id"`
	Email       string  `json:"email"`
	Name        string  `json:"name"`
	Owner       bool    `json:"owner"`
	Disabled    bool    `json:"disabled"`
	Pending     bool    `json:"pending"`
	LastLoginAt *string `json:"last_login_at"`
	Projects    []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Role string `json:"role"`
	} `json:"projects"`
}

type invitationBody struct {
	Account         accountBody `json:"account"`
	InviteURL       string      `json:"invite_url"`
	InviteExpiresAt string      `json:"invite_expires_at"`
}

// tokenOf reads the secret out of an invitation link, which is where the
// person clicking it gets it from too.
func tokenOf(t *testing.T, url string) string {
	t.Helper()
	_, token, found := strings.Cut(url, "#token=")
	if !found || token == "" {
		t.Fatalf("invite_url = %q, want the token in the fragment", url)
	}
	return token
}

// TestInvitationEndToEnd is Decision 10: the link is the invitation, it works
// once, and the password never passes through the owner's hands.
func TestInvitationEndToEnd(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.owner(t)

	rec := h.call(t, "POST", "/api/v1/accounts", mustJSON(t, map[string]any{
		"email": "helper@example.com",
		"name":  "The Helper",
		"memberships": []map[string]any{
			{"project_id": h.project.ID, "role": store.RoleViewer},
		},
	}), asSession(owner))
	expectStatus(t, rec, http.StatusCreated)
	invitation := decodeJSON[invitationBody](t, rec)

	if !invitation.Account.Pending || invitation.Account.LastLoginAt != nil {
		t.Errorf("account = %+v, want it pending and never signed in", invitation.Account)
	}
	if len(invitation.Account.Projects) != 1 || invitation.Account.Projects[0].Role != store.RoleViewer {
		t.Errorf("projects = %+v", invitation.Account.Projects)
	}
	// The link points at the host the owner is on, and carries the token in
	// the fragment so a link pasted into a chat keeps no secret in history.
	if !strings.HasPrefix(invitation.InviteURL, "http://example.com/invite#token=") {
		t.Errorf("invite_url = %q", invitation.InviteURL)
	}
	token := tokenOf(t, invitation.InviteURL)

	// The link is in that response and in no other: reading the account
	// back does not hand it out again.
	rec = h.call(t, "GET", "/api/v1/accounts/"+invitation.Account.ID, nil, asSession(owner))
	expectStatus(t, rec, 200)
	if strings.Contains(rec.Body.String(), token) {
		t.Error("the invitation token came back out of a read")
	}

	// A password that is too short is a 422 and does not spend the token.
	rec = h.call(t, "POST", "/api/v1/auth/accept-invite",
		mustJSON(t, map[string]any{"token": token, "password": "short"}), anonymous,
		func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) })
	expectStatus(t, rec, http.StatusUnprocessableEntity)

	rec = h.call(t, "POST", "/api/v1/auth/accept-invite",
		mustJSON(t, map[string]any{"token": token, "password": testAccountPassword}), anonymous,
		func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) })
	expectStatus(t, rec, 200)
	cookie := sessionCookieOf(rec)
	if cookie == nil {
		t.Fatal("accepting an invitation must sign the person in")
	}

	// Once: the same link again is refused, with a sentence a person can
	// act on.
	rec = h.call(t, "POST", "/api/v1/auth/accept-invite",
		mustJSON(t, map[string]any{"token": token, "password": testAccountPassword}), anonymous,
		func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) })
	expectError(t, rec, http.StatusForbidden, "not valid")

	// The account is live: it can read its project and is not an owner.
	helper := &signedIn{account: &store.Account{Email: "helper@example.com"}, cookie: cookie.Value}
	rec = h.call(t, "GET", "/api/v1/traces", nil, asSession(helper), inProject(h.project.ID))
	expectStatus(t, rec, 200)
	rec = h.call(t, "GET", "/api/v1/accounts", nil, asSession(helper))
	expectError(t, rec, http.StatusForbidden, "owner account")

	// And the owner sees it as active, with a login stamped.
	rec = h.call(t, "GET", "/api/v1/accounts/"+invitation.Account.ID, nil, asSession(owner))
	expectStatus(t, rec, 200)
	live := decodeJSON[struct {
		Account accountBody `json:"account"`
	}](t, rec).Account
	if live.Pending || live.LastLoginAt == nil {
		t.Errorf("account = %+v, want it active with a login stamped", live)
	}
}

// TestReInviteIsTheReset: it ends the sessions and leaves the old password
// working until the link is used, because the failure mode of a reset is
// somebody locked out on a Friday (Decision 10).
func TestReInviteIsTheReset(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.owner(t)
	helper := h.account(t, "helper@example.com", false,
		store.Membership{ProjectID: h.project.ID, Role: store.RoleEditor})

	rec := h.call(t, "POST", "/api/v1/accounts/"+helper.account.ID+"/invite", nil, asSession(owner))
	expectStatus(t, rec, http.StatusCreated)
	link := decodeJSON[struct {
		InviteURL string `json:"invite_url"`
	}](t, rec).InviteURL

	// The sessions are gone at once.
	expectStatus(t, h.call(t, "GET", "/api/v1/auth/me", nil, asSession(helper)),
		http.StatusUnauthorized)
	// The old password still works, so nobody is locked out while the link
	// is in transit.
	expectStatus(t, h.login(t, "helper@example.com", testAccountPassword), 200)

	rec = h.call(t, "POST", "/api/v1/auth/accept-invite", mustJSON(t, map[string]any{
		"token": tokenOf(t, link), "password": "a brand new password",
	}), anonymous, func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) })
	expectStatus(t, rec, 200)

	// And now it is the new one.
	expectError(t, h.login(t, "helper@example.com", testAccountPassword),
		http.StatusUnauthorized, wrongCredentials)
	expectStatus(t, h.login(t, "helper@example.com", "a brand new password"), 200)
}

// TestAccountStanding covers Decision 12's rules about who may stop being an
// owner and what a promotion does to the memberships.
func TestAccountStanding(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.owner(t)
	helper := h.account(t, "helper@example.com", false,
		store.Membership{ProjectID: h.project.ID, Role: store.RoleViewer})

	patch := func(id string, body map[string]any) *httptest.ResponseRecorder {
		return h.call(t, "PATCH", "/api/v1/accounts/"+id, mustJSON(t, body), asSession(owner))
	}

	// The last owner cannot stand down, by either door.
	expectError(t, patch(owner.account.ID, map[string]any{"owner": false}),
		http.StatusConflict, "last owner")
	expectError(t, patch(owner.account.ID, map[string]any{"disabled": true}),
		http.StatusConflict, "last owner")
	rec := h.call(t, "DELETE",
		"/api/v1/accounts/"+owner.account.ID+"?confirm=owner@example.com", nil, asSession(owner))
	expectError(t, rec, http.StatusConflict, "last owner")

	// Promoting the helper drops its memberships: an owner has every
	// project, and a stale row would come back as a demotion's surprise.
	rec = patch(helper.account.ID, map[string]any{"owner": true})
	expectStatus(t, rec, 200)
	promoted := decodeJSON[struct {
		Account accountBody `json:"account"`
	}](t, rec).Account
	if !promoted.Owner || len(promoted.Projects) != 0 {
		t.Errorf("account = %+v, want an owner with no membership rows", promoted)
	}

	// With two owners, the first can stand down.
	expectStatus(t, patch(owner.account.ID, map[string]any{"owner": false}), 200)

	// A membership for an owner is refused rather than silently ignored.
	rec = h.call(t, "PUT",
		"/api/v1/accounts/"+helper.account.ID+"/projects/"+h.project.ID,
		mustJSON(t, map[string]any{"role": store.RoleEditor}), asSession(helper))
	expectError(t, rec, http.StatusConflict, "an owner has every project")

	// And so is one at creation time.
	rec = h.call(t, "POST", "/api/v1/accounts", mustJSON(t, map[string]any{
		"email": "partner@example.com", "owner": true,
		"memberships": []map[string]any{{"project_id": h.project.ID, "role": store.RoleViewer}},
	}), asSession(helper))
	expectError(t, rec, http.StatusUnprocessableEntity, "an owner has every project")

	// A role that is not one is a 422 naming the two that are.
	rec = h.call(t, "PUT",
		"/api/v1/accounts/"+owner.account.ID+"/projects/"+h.project.ID,
		mustJSON(t, map[string]any{"role": "admin"}), asSession(helper))
	expectError(t, rec, http.StatusUnprocessableEntity, store.RoleEditor)
}

// TestDisablingEndsAccessAtOnce: disabling is the reversible way to take
// access away today (Decision 12), and it does not wait for a session to
// expire.
func TestDisablingEndsAccessAtOnce(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.owner(t)
	helper := h.account(t, "helper@example.com", false,
		store.Membership{ProjectID: h.project.ID, Role: store.RoleViewer})

	expectStatus(t, h.call(t, "GET", "/api/v1/traces", nil,
		asSession(helper), inProject(h.project.ID)), 200)

	rec := h.call(t, "PATCH", "/api/v1/accounts/"+helper.account.ID,
		mustJSON(t, map[string]any{"disabled": true}), asSession(owner))
	expectStatus(t, rec, 200)

	// The next navigation lands on the login screen, which is what a 401
	// means to the interface.
	expectStatus(t, h.call(t, "GET", "/api/v1/traces", nil,
		asSession(helper), inProject(h.project.ID)), http.StatusUnauthorized)
	expectError(t, h.login(t, "helper@example.com", testAccountPassword),
		http.StatusUnauthorized, wrongCredentials)

	// Enabling puts everything back, memberships included: that is the
	// difference between disabling and deleting.
	rec = h.call(t, "PATCH", "/api/v1/accounts/"+helper.account.ID,
		mustJSON(t, map[string]any{"disabled": false}), asSession(owner))
	expectStatus(t, rec, 200)
	back := decodeJSON[struct {
		Account accountBody `json:"account"`
	}](t, rec).Account
	if back.Disabled || len(back.Projects) != 1 {
		t.Errorf("account = %+v, want it back with its roles", back)
	}
	expectStatus(t, h.login(t, "helper@example.com", testAccountPassword), 200)
}

// TestDeleteAccountIsADryRunUntilTheEmailEchoes is spec 005 #8 applied to an
// account: the echo is the email, the one thing about an account a person
// means.
func TestDeleteAccountIsADryRunUntilTheEmailEchoes(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.owner(t)
	helper := h.account(t, "helper@example.com", false,
		store.Membership{ProjectID: h.project.ID, Role: store.RoleEditor})

	rec := h.call(t, "DELETE", "/api/v1/accounts/"+helper.account.ID, nil, asSession(owner))
	expectStatus(t, rec, 200)
	preview := decodeJSON[struct {
		DryRun      bool   `json:"dry_run"`
		Confirm     string `json:"confirm"`
		WouldDelete struct {
			Memberships int `json:"memberships"`
			Sessions    int `json:"sessions"`
		} `json:"would_delete"`
	}](t, rec)
	if !preview.DryRun || preview.Confirm != "helper@example.com" {
		t.Fatalf("preview = %+v", preview)
	}
	if preview.WouldDelete.Memberships != 1 || preview.WouldDelete.Sessions != 1 {
		t.Errorf("would_delete = %+v, want the membership and the live session", preview.WouldDelete)
	}
	// Nothing happened.
	expectStatus(t, h.call(t, "GET", "/api/v1/auth/me", nil, asSession(helper)), 200)

	// A wrong echo changes nothing either.
	rec = h.call(t, "DELETE",
		"/api/v1/accounts/"+helper.account.ID+"?confirm=helper", nil, asSession(owner))
	expectError(t, rec, http.StatusBadRequest, "helper@example.com")
	expectStatus(t, h.call(t, "GET", "/api/v1/auth/me", nil, asSession(helper)), 200)

	rec = h.call(t, "DELETE",
		"/api/v1/accounts/"+helper.account.ID+"?confirm=helper@example.com", nil, asSession(owner))
	expectStatus(t, rec, http.StatusNoContent)

	// The session went with it, and so did the membership (spec 028, edge
	// cases: the cookie answers 401 and the interface goes to /login).
	expectStatus(t, h.call(t, "GET", "/api/v1/auth/me", nil, asSession(helper)), http.StatusUnauthorized)
	rec = h.call(t, "GET", "/api/v1/projects/"+h.project.ID+"/members", nil, asSession(owner))
	expectStatus(t, rec, 200)
	if members := decodeJSON[struct {
		Members []struct {
			Email string `json:"email"`
		} `json:"members"`
	}](t, rec).Members; len(members) != 0 {
		t.Errorf("members = %+v after the deletion", members)
	}
}

// TestProjectMembers is the project side of the question, and it leaves the
// owners out because they are not membership rows (Decision 12).
func TestProjectMembers(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.owner(t)
	h.editor(t)
	h.viewer(t)

	rec := h.call(t, "GET", "/api/v1/projects/"+h.project.ID+"/members", nil, asSession(owner))
	expectStatus(t, rec, 200)
	members := decodeJSON[struct {
		Members []struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		} `json:"members"`
	}](t, rec).Members
	if len(members) != 2 {
		t.Fatalf("members = %+v, want the editor and the viewer and no owner", members)
	}
	if members[0].Email != "editor@example.com" || members[0].Role != store.RoleEditor {
		t.Errorf("members[0] = %+v", members[0])
	}
	if members[1].Email != "viewer@example.com" || members[1].Role != store.RoleViewer {
		t.Errorf("members[1] = %+v", members[1])
	}
}

// TestMembershipChangesWhatAProjectAllows: the role is the whole of the
// difference between looking and changing, and it takes effect on the next
// request.
func TestMembershipChangesWhatAProjectAllows(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.owner(t)
	helper := h.account(t, "helper@example.com", false,
		store.Membership{ProjectID: h.project.ID, Role: store.RoleViewer})

	// A viewer annotates — that is the whole point of the role — and does
	// not edit.
	rec := h.call(t, "POST", "/api/v1/score-configs/quality",
		mustJSON(t, map[string]any{"data_type": "numeric", "direction": "higher"}),
		asSession(helper), inProject(h.project.ID))
	if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusForbidden {
		t.Errorf("POST to a PUT-only route = %d", rec.Code)
	}
	rec = h.call(t, "PUT", "/api/v1/score-configs/quality",
		mustJSON(t, map[string]any{"data_type": "numeric", "direction": "higher"}),
		asSession(helper), inProject(h.project.ID))
	expectError(t, rec, http.StatusForbidden, "viewer")

	rec = h.call(t, "PUT", "/api/v1/accounts/"+helper.account.ID+"/projects/"+h.project.ID,
		mustJSON(t, map[string]any{"role": store.RoleEditor}), asSession(owner))
	expectStatus(t, rec, 200)

	rec = h.call(t, "PUT", "/api/v1/score-configs/quality",
		mustJSON(t, map[string]any{"data_type": "numeric", "direction": "higher"}),
		asSession(helper), inProject(h.project.ID))
	expectStatus(t, rec, 200)

	// Taking the project away is a 403 on the next request, not on the
	// next sign-in.
	rec = h.call(t, "DELETE", "/api/v1/accounts/"+helper.account.ID+"/projects/"+h.project.ID,
		nil, asSession(owner))
	expectStatus(t, rec, http.StatusNoContent)
	rec = h.call(t, "GET", "/api/v1/traces", nil, asSession(helper), inProject(h.project.ID))
	expectError(t, rec, http.StatusForbidden, "not a member")
}

// TestAdminTokenIsTheRecovery is Decision 16's promise: with every owner's
// password lost, the token still mints a link.
func TestAdminTokenIsTheRecovery(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.owner(t)

	rec := h.call(t, "GET", "/api/v1/accounts", nil, asAdmin)
	expectStatus(t, rec, 200)
	listed := decodeJSON[struct {
		Accounts []accountBody `json:"accounts"`
	}](t, rec).Accounts
	if len(listed) != 1 || listed[0].Email != "owner@example.com" {
		t.Fatalf("accounts = %+v", listed)
	}

	rec = h.call(t, "POST", "/api/v1/accounts/"+owner.account.ID+"/invite", nil, asAdmin)
	expectStatus(t, rec, http.StatusCreated)
	link := decodeJSON[struct {
		InviteURL       string `json:"invite_url"`
		InviteExpiresAt string `json:"invite_expires_at"`
	}](t, rec)
	if link.InviteExpiresAt == "" {
		t.Error("the link's expiry is what says how long the owner has to use it")
	}

	rec = h.call(t, "POST", "/api/v1/auth/accept-invite", mustJSON(t, map[string]any{
		"token": tokenOf(t, link.InviteURL), "password": "a brand new password",
	}), anonymous, func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) })
	expectStatus(t, rec, 200)
	if sessionCookieOf(rec) == nil {
		t.Error("the recovery must end with the owner signed in")
	}
}

// TestAccountListingShape is what the Accounts table reads: standing, the last
// login, and the projects with roles on the row rather than behind a click.
func TestAccountListingShape(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.owner(t)
	h.invited(t, "pending@example.com", false,
		store.Membership{ProjectID: h.project.ID, Role: store.RoleEditor})

	rec := h.call(t, "GET", "/api/v1/accounts", nil, asSession(owner))
	expectStatus(t, rec, 200)
	listed := decodeJSON[struct {
		Accounts []accountBody `json:"accounts"`
	}](t, rec).Accounts
	if len(listed) != 2 {
		t.Fatalf("accounts = %+v", listed)
	}
	// Sorted by email, so `owner@` follows `pending@`.
	if listed[0].Email != "owner@example.com" || listed[1].Email != "pending@example.com" {
		t.Fatalf("order = %q, %q; want by email", listed[0].Email, listed[1].Email)
	}
	if listed[0].LastLoginAt == nil {
		t.Error("the owner has signed in, so it has a last login")
	}
	if !listed[1].Pending || listed[1].LastLoginAt != nil {
		t.Errorf("pending account = %+v", listed[1])
	}
	if len(listed[1].Projects) != 1 || listed[1].Projects[0].Name != "test" {
		t.Errorf("projects = %+v, want the membership with its project's name", listed[1].Projects)
	}
	// An owner carries no membership rows: the flag is the membership.
	if len(listed[0].Projects) != 0 {
		t.Errorf("an owner's membership rows = %+v, want none", listed[0].Projects)
	}
}

// TestInviteLinkHonoursTheConfiguredURL: the owner is on the interface
// already, so the request's Host is the right guess — and `TRACEPAD_URL` wins
// when the guess is wrong, which is what happens behind a proxy (Decision 11).
func TestInviteLinkHonoursTheConfiguredURL(t *testing.T) {
	h := newAccountHarness(t)
	owner := h.owner(t)
	h.server.publicURL = "https://traces.example.com"

	rec := h.call(t, "POST", "/api/v1/accounts",
		mustJSON(t, map[string]any{"email": "helper@example.com"}), asSession(owner))
	expectStatus(t, rec, http.StatusCreated)
	url := decodeJSON[invitationBody](t, rec).InviteURL
	if !strings.HasPrefix(url, "https://traces.example.com/invite#token=") {
		t.Errorf("invite_url = %q, want TRACEPAD_URL to win", url)
	}
}

// TestInvitationExpires: seven days, and a stale link says so with the same
// sentence a spent one does (Decision 10).
func TestInvitationExpires(t *testing.T) {
	h := newAccountHarness(t)
	account := h.invited(t, "helper@example.com", false)

	// Re-mint the token with an expiry in the past, which is the state a
	// link a fortnight old is in.
	if err := h.writer.Submit(t.Context(), &store.InviteMint{
		AccountID: account.ID, TokenID: store.SessionID("stale"),
		ExpiresAt: time.Now().Add(-time.Hour).UnixNano(), Now: time.Now().UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	rec := h.call(t, "POST", "/api/v1/auth/accept-invite", mustJSON(t, map[string]any{
		"token": "stale", "password": testAccountPassword,
	}), anonymous, func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) })
	expectError(t, rec, http.StatusForbidden, "not valid")
}
