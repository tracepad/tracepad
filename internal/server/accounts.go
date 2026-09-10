package server

import (
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

/*
Account management (spec 028 Decision 12).

An owner manages people from the account side — a person and their projects,
which is what `/api/v1/accounts` is — and reads from the project side, which is
`GET /api/v1/projects/{id}/members`. Those are the two questions the two
screens ask, so they are two routes and not one with a filter.

Everything here is `owner` policy: an owner session, or the admin token, which
is what makes `tracepad accounts invite <email>` the documented recovery when
every owner's password is lost (#16).

The safety rules are spec 005's, unchanged. Deleting is a dry run until
`?confirm=` echoes the account's email, and the echo is checked inside the
write transaction, not here, because between a preview and a commit an account
can be renamed.
*/

// inviteTokenBytes is the size of the token in an invitation link.
const inviteTokenBytes = 32

// handleListAccounts answers with every account, its standing and its
// projects: the page an owner opens to answer "who can see what", so the
// projects and roles are on the row rather than behind a click.
func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	accounts, err := s.store.ListAccounts()
	if err != nil {
		slog.Error("could not read the accounts", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the accounts")
		return
	}
	rendered := make([]object, 0, len(accounts))
	for _, account := range accounts {
		body, ok := s.fullAccount(w, account)
		if !ok {
			return
		}
		rendered = append(rendered, body)
	}
	writeJSON(w, http.StatusOK, object{}.put("accounts", rendered))
}

// handleCreateAccount invites somebody: an account with no password, and the
// single-use link that sets one.
//
// The link is in this response and in no other, ever — only its hash is
// stored, the way a project key's secret is (spec 001 #8). Carrying it to the
// person is the owner's job: there is no mail here, and an owner pasting a
// link into a chat is a smaller ask than an operator configuring SMTP.
func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var request struct {
		Email       string `json:"email"`
		Name        string `json:"name"`
		Owner       bool   `json:"owner"`
		Memberships []struct {
			ProjectID string `json:"project_id"`
			Role      string `json:"role"`
		} `json:"memberships"`
	}
	if !s.readJSON(w, r, &request) {
		return
	}
	email, ok := readEmail(w, request.Email)
	if !ok {
		return
	}
	if request.Owner && len(request.Memberships) > 0 {
		writeError(w, http.StatusUnprocessableEntity, "an owner has every project")
		return
	}
	memberships := make([]store.Membership, 0, len(request.Memberships))
	for _, m := range request.Memberships {
		if !store.ValidRole(m.Role) {
			writeError(w, http.StatusUnprocessableEntity,
				"a role is "+store.RoleViewer+" or "+store.RoleEditor+", not "+m.Role)
			return
		}
		project, err := s.store.ProjectByID(m.ProjectID)
		if err != nil {
			slog.Error("project lookup failed", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to read the project")
			return
		}
		if project == nil || project.Deleted() {
			writeError(w, http.StatusNotFound, "no such project: "+m.ProjectID)
			return
		}
		memberships = append(memberships, store.Membership{ProjectID: m.ProjectID, Role: m.Role})
	}

	token, err := newInviteToken()
	if err != nil {
		slog.Error("could not mint an invitation", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to create the invitation")
		return
	}
	now := time.Now()
	expires := now.Add(inviteWindow)
	create := &store.AccountCreate{
		Email: email, Name: strings.TrimSpace(request.Name), Owner: request.Owner,
		Memberships: memberships,
		TokenID:     store.SessionID(token), ExpiresAt: expires.UnixNano(), Now: now.UnixNano(),
	}
	if !s.submit(w, r, create) {
		return
	}
	body, ok := s.fullAccount(w, create.Account)
	if !ok {
		return
	}
	writeJSON(w, http.StatusCreated, object{}.
		put("account", body).
		put("invite_url", s.inviteURL(r, token)).
		put("invite_expires_at", formatTime(expires.UnixNano())).
		put("note", "the invitation link is shown only here; only its hash is stored"))
}

// handleGetAccount reads one account with its projects.
func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	account, ok := s.account(w, r)
	if !ok {
		return
	}
	body, ok := s.fullAccount(w, account)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, object{}.put("account", body))
}

// handlePatchAccount changes a name, the owner flag or the disabled flag.
//
// The last enabled owner cannot lose either: it is the one invariant a server
// needs to never be locked out of itself, and the check lives inside the write
// transaction so that two owners demoting each other at once get one 409 and
// not two successes (spec 028, edge cases).
func (s *Server) handlePatchAccount(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	account, ok := s.account(w, r)
	if !ok {
		return
	}
	var request struct {
		Name     *string `json:"name"`
		Owner    *bool   `json:"owner"`
		Disabled *bool   `json:"disabled"`
	}
	if !s.readJSON(w, r, &request) {
		return
	}
	if request.Name == nil && request.Owner == nil && request.Disabled == nil {
		writeError(w, http.StatusBadRequest,
			`nothing to change: send "name", "owner" or "disabled"`)
		return
	}
	update := &store.AccountUpdate{
		AccountID: account.ID, Name: request.Name, Owner: request.Owner,
		Disabled: request.Disabled, Now: time.Now().UnixNano(),
	}
	if !s.submit(w, r, update) {
		return
	}
	body, ok := s.fullAccount(w, update.Account)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, object{}.put("account", body))
}

// handleDeleteAccount removes a person who is gone. It takes their
// memberships, sessions and invitations and nothing else: a score does not
// name its author, and nothing else in the database references an account.
//
// Disabling is the reversible way to take access away today; this is the other
// one, so it wears the echo (spec 005 #8) and the echo is the email.
func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	values, err := queryParams(r, "confirm")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	account, ok := s.account(w, r)
	if !ok {
		return
	}

	if values.Get("confirm") == "" {
		memberships, err := s.store.Memberships(account.ID)
		if err != nil {
			slog.Error("could not read the memberships", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to read the memberships")
			return
		}
		sessions, err := s.store.AccountSessions(account.ID, time.Now().UnixNano())
		if err != nil {
			slog.Error("could not read the sessions", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to read the sessions")
			return
		}
		body, ok := s.fullAccount(w, account)
		if !ok {
			return
		}
		writeJSON(w, http.StatusOK, object{}.
			put("dry_run", true).
			put("account", body).
			put("would_delete", object{}.
				put("memberships", len(memberships)).
				put("sessions", len(sessions))).
			put("confirm", account.Email).
			put("note", "disabling the account instead takes access away without losing its roles"))
		return
	}

	if !s.submit(w, r, &store.AccountDelete{AccountID: account.ID, Confirm: values.Get("confirm")}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleInviteAccount mints a fresh link for an account that already exists.
//
// This is the password reset, and it is deliberately gentle: it ends the
// account's sessions, but the old password goes on working until the link is
// used, because the failure mode of a reset is somebody locked out on a
// Friday.
func (s *Server) handleInviteAccount(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	account, ok := s.account(w, r)
	if !ok {
		return
	}
	token, err := newInviteToken()
	if err != nil {
		slog.Error("could not mint an invitation", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to create the invitation")
		return
	}
	now := time.Now()
	expires := now.Add(inviteWindow)
	mint := &store.InviteMint{
		AccountID: account.ID, TokenID: store.SessionID(token),
		ExpiresAt: expires.UnixNano(), Now: now.UnixNano(),
	}
	if !s.submit(w, r, mint) {
		return
	}
	writeJSON(w, http.StatusCreated, object{}.
		put("invite_url", s.inviteURL(r, token)).
		put("invite_expires_at", formatTime(expires.UnixNano())).
		put("note", "the earlier link and every session of this account are now void; "+
			"the old password works until this link is used"))
}

// handlePutMembership gives an account a role in a project, or changes it.
func (s *Server) handlePutMembership(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	account, ok := s.account(w, r)
	if !ok {
		return
	}
	var request struct {
		Role string `json:"role"`
	}
	if !s.readJSON(w, r, &request) {
		return
	}
	if !store.ValidRole(request.Role) {
		writeError(w, http.StatusUnprocessableEntity,
			"a role is "+store.RoleViewer+" or "+store.RoleEditor+", not "+request.Role)
		return
	}
	projectID := r.PathValue("project_id")
	project, err := s.store.ProjectByID(projectID)
	if err != nil {
		slog.Error("project lookup failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the project")
		return
	}
	if project == nil || project.Deleted() {
		writeError(w, http.StatusNotFound, "no such project")
		return
	}
	put := &store.MembershipPut{
		AccountID: account.ID, ProjectID: projectID,
		Role: request.Role, Now: time.Now().UnixNano(),
	}
	if !s.submit(w, r, put) {
		return
	}
	writeJSON(w, http.StatusOK, object{}.put("membership", object{}.
		put("account_id", account.ID).
		put("project_id", projectID).
		put("name", project.Name).
		put("role", request.Role)))
}

// handleDeleteMembership takes one project away from one account.
func (s *Server) handleDeleteMembership(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r); !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	account, ok := s.account(w, r)
	if !ok {
		return
	}
	if !s.submit(w, r, &store.MembershipDelete{
		AccountID: account.ID, ProjectID: r.PathValue("project_id"),
	}) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleProjectMembers is the project side: who can see this project. Owners
// are not listed, because they are not membership rows — the screen says "and
// every owner" instead of pretending they are.
func (s *Server) handleProjectMembers(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	project, ok := s.target(w, r, c, liveOnly)
	if !ok {
		return
	}
	members, err := s.store.ProjectMembers(project.ID)
	if err != nil {
		slog.Error("could not read the members", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the members")
		return
	}
	rendered := make([]object, 0, len(members))
	for _, m := range members {
		rendered = append(rendered, object{}.
			put("account_id", m.AccountID).
			put("email", m.Email).
			put("name", m.Name).
			put("role", m.Role))
	}
	writeJSON(w, http.StatusOK, object{}.put("members", rendered))
}

// --- Shared -----------------------------------------------------------------

// account resolves the `{id}` of an account route, answering 404 itself.
func (s *Server) account(w http.ResponseWriter, r *http.Request) (*store.Account, bool) {
	account, err := s.store.AccountByID(r.PathValue("id"))
	if err != nil {
		slog.Error("account lookup failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the account")
		return nil, false
	}
	if account == nil {
		writeError(w, http.StatusNotFound, "no such account")
		return nil, false
	}
	return account, true
}

// fullAccount is an owner's view of somebody: the standing fields the person
// themselves has no use for, and the memberships.
//
// `pending` is derived rather than stored — it is "has no password yet" — so
// that there is one fact about an account and not two that can disagree.
func (s *Server) fullAccount(w http.ResponseWriter, a *store.Account) (object, bool) {
	memberships, err := s.store.Memberships(a.ID)
	if err != nil {
		slog.Error("could not read the memberships", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the memberships")
		return nil, false
	}
	var lastLogin any
	if a.LastLoginAt != nil {
		lastLogin = formatTime(*a.LastLoginAt)
	}
	return accountResponse(a).
		put("disabled", a.Disabled).
		put("pending", a.Pending).
		put("created_at", formatTime(a.CreatedAt)).
		// null is a value here, not an absence: it is how the API says
		// "has never signed in".
		put("last_login_at", lastLogin).
		put("projects", membershipsResponse(memberships)), true
}

// newInviteToken mints the secret in an invitation link.
func newInviteToken() (string, error) {
	raw := make([]byte, inviteTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// inviteURL is where the person the owner is inviting has to go. The token
// rides in the fragment, which never leaves the browser, and the interface
// strips it from the URL as soon as it has read it (spec 006 #8): a link in a
// chat must not keep the secret in the history of whoever opens it.
func (s *Server) inviteURL(r *http.Request, token string) string {
	return s.originFor(r) + "/invite#token=" + token
}
