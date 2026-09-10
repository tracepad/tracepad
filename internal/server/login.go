package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/store"
)

/*
The three ways in, and the four questions a signed-in person asks about
themselves (spec 028 Decisions 8–11).

The ways in are `POST /api/v1/setup` (the first owner, from the token the
server printed at startup), `POST /api/v1/auth/login` (an email and a
password) and `POST /api/v1/auth/accept-invite` (a link an owner handed over).
All three end the same way: a row in `account_sessions` and an `HttpOnly`
cookie holding the only copy of its value.

The rest of `/api/v1/auth` is `session`-only, which is a policy and not a
check in here: a key or the admin token is refused by the guard with "not a
session" before any of these handlers runs.
*/

// setupTokenBytes is the size of the token the server prints in the setup
// link. Thirty-two random bytes, like a session's value.
const setupTokenBytes = 32

// mintSetupToken generates the token for this start, or clears it when the
// server already has an owner (Decision 9).
//
// In memory and per start, so a token from a log file yesterday opens nothing
// today, and a restart is the recovery if the link was lost.
func (s *Server) mintSetupToken() {
	s.setupToken = ""
	if s.store == nil {
		return
	}
	owners, err := s.store.EnabledOwners()
	if err != nil {
		slog.Error("could not count the owners", "err", err)
		return
	}
	if owners > 0 {
		return
	}
	raw := make([]byte, setupTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		slog.Error("could not mint a setup token", "err", err)
		return
	}
	s.setupToken = base64.RawURLEncoding.EncodeToString(raw)
}

// SetupURL is the link the operator clicks on a server that has no owner yet,
// or "" once it has one. `printStartup` prints it where the pre-authed key
// link used to be (Decision 9).
//
// The fragment is what keeps the token out of the server's own logs: fragments
// never leave the browser, and the interface strips it from the URL as soon as
// it has read it (spec 006 #8).
func (s *Server) SetupURL() string {
	if s.setupToken == "" {
		return ""
	}
	return s.originForPrint() + "/setup#token=" + s.setupToken
}

// SetupRequired reports whether this server still needs its first owner. It is
// asked of the store rather than of the token, because an owner can be created
// while the process runs and the answer has to change with it.
func (s *Server) SetupRequired() (bool, error) {
	owners, err := s.store.EnabledOwners()
	return owners == 0, err
}

// handleGetSetup is the one thing the interface can learn without a
// credential: whether to show the setup screen or the login form.
func (s *Server) handleGetSetup(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "the API is not available")
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	required, err := s.SetupRequired()
	if err != nil {
		slog.Error("could not count the owners", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the accounts")
		return
	}
	writeJSON(w, http.StatusOK, object{}.put("required", required))
}

// handleSetup creates the first owner and signs it in.
//
// No environment variable for the password: one in `docker-compose.yml` is the
// thing this spec exists to stop pasting. The one moment the operator is
// provably at the console is the moment the server prints a link, so the link
// is the credential.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "the API is not available")
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var request struct {
		Token    string `json:"token"`
		Email    string `json:"email"`
		Password string `json:"password"`
		Name     string `json:"name"`
	}
	if !s.readJSON(w, r, &request) {
		return
	}
	if s.setupToken == "" ||
		subtle.ConstantTimeCompare([]byte(request.Token), []byte(s.setupToken)) != 1 {
		writeError(w, http.StatusForbidden,
			"this setup link is not valid; restart the server to have it print a new one")
		return
	}
	email, ok := readEmail(w, request.Email)
	if !ok {
		return
	}
	hash, ok := readPassword(w, request.Password)
	if !ok {
		return
	}

	create := &store.SetupOwner{
		Email: email, Name: strings.TrimSpace(request.Name), Hash: hash,
	}
	if !s.signIn(w, r, create) {
		return
	}
	// The token is spent the moment it works: `GET /api/v1/setup` flips to
	// `{required: false}` and there is nothing left to replay.
	s.setupToken = ""
	writeJSON(w, http.StatusCreated, object{}.put("account", accountResponse(create.Account)))
}

// handleLogin is an email and a password.
//
// Wrong email, wrong password, a disabled account and one that has never
// accepted its invitation all answer the same 401 with the same sentence: any
// difference between them is a way to find out who has an account here
// (Decision 8).
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "the API is not available")
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var request struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !s.readJSON(w, r, &request) {
		return
	}
	email := strings.TrimSpace(request.Email)

	now := time.Now()
	if wait := s.limiter.retryAfter(email, now); wait > 0 {
		w.Header().Set("Retry-After", retryAfterSeconds(wait))
		writeError(w, http.StatusTooManyRequests,
			"too many sign-in attempts for this email; try again shortly")
		return
	}

	account, err := s.store.AccountByEmail(email)
	if err != nil {
		slog.Error("account lookup failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the account")
		return
	}
	if account == nil || account.Disabled || !account.Verify(request.Password) {
		s.limiter.failed(email, now)
		writeError(w, http.StatusUnauthorized, wrongCredentials)
		return
	}
	s.limiter.succeeded(email)

	if !s.signIn(w, r, &store.SessionOpen{AccountID: account.ID}) {
		return
	}
	writeJSON(w, http.StatusOK, object{}.put("account", accountResponse(account)))
}

// wrongCredentials is the one sentence every login failure gets.
const wrongCredentials = "wrong email or password"

// handleAcceptInvite spends an invitation or a reset link: it sets the
// password, ends whatever sessions the account had, and signs the person in.
func (s *Server) handleAcceptInvite(w http.ResponseWriter, r *http.Request) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "the API is not available")
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var request struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if !s.readJSON(w, r, &request) {
		return
	}
	if strings.TrimSpace(request.Token) == "" {
		writeError(w, http.StatusForbidden, store.ErrBadToken.Error())
		return
	}
	hash, ok := readPassword(w, request.Password)
	if !ok {
		return
	}

	accept := &store.InviteAccept{TokenID: store.SessionID(request.Token), NewHash: hash}
	if !s.signIn(w, r, accept) {
		return
	}
	writeJSON(w, http.StatusOK, object{}.put("account", accountResponse(accept.Account)))
}

// handleLogout ends this session and clears the cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.submit(w, r, &store.SessionsEnd{SessionID: c.session.ID}) {
		return
	}
	s.clearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// handleMe is the one call the interface makes on load. The project list rides
// in it because the shell needs both at once (Decision 8).
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.writeMe(w, c.account)
}

func (s *Server) writeMe(w http.ResponseWriter, account *store.Account) {
	projects, err := s.store.AccountProjects(account)
	if err != nil {
		slog.Error("could not read the account's projects", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the projects")
		return
	}
	writeJSON(w, http.StatusOK, object{}.
		put("account", accountResponse(account)).
		put("projects", membershipsResponse(projects)))
}

// handlePatchMe changes the display name, the password, or both.
func (s *Server) handlePatchMe(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var request struct {
		Name     *string `json:"name"`
		Password *struct {
			Current string `json:"current"`
			New     string `json:"new"`
		} `json:"password"`
	}
	if !s.readJSON(w, r, &request) {
		return
	}
	if request.Name == nil && request.Password == nil {
		writeError(w, http.StatusBadRequest, `nothing to change: send "name" or "password"`)
		return
	}

	account := c.account
	if request.Name != nil {
		update := &store.AccountUpdate{AccountID: account.ID, Name: request.Name, Now: time.Now().UnixNano()}
		if !s.submit(w, r, update) {
			return
		}
		account = update.Account
	}
	if request.Password != nil {
		hash, ok := readPassword(w, request.Password.New)
		if !ok {
			return
		}
		// A password change signs every other session out (Decision 4):
		// changing it is what a person does when they think somebody
		// else has it.
		change := &store.PasswordChange{
			AccountID: account.ID, Current: request.Password.Current,
			NewHash: hash, Keep: c.session.ID,
		}
		if err := s.writer.Submit(r.Context(), change); err != nil {
			if errors.Is(err, store.ErrWrongPassword) {
				writeError(w, http.StatusForbidden, store.ErrWrongPassword.Error())
				return
			}
			s.submitFailure(w, err)
			return
		}
		account = change.Account
	}
	writeJSON(w, http.StatusOK, object{}.put("account", accountResponse(account)))
}

// handleListSessionsOfAccount lists where this account is signed in. It is the
// reason sessions are rows: "sign out everywhere" is a decision a person makes
// by looking at the list.
func (s *Server) handleListSessionsOfAccount(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	sessions, err := s.store.AccountSessions(c.account.ID, time.Now().UnixNano())
	if err != nil {
		slog.Error("could not read the sessions", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the sessions")
		return
	}
	rendered := make([]object, 0, len(sessions))
	for _, one := range sessions {
		rendered = append(rendered, object{}.
			// The id here is the row's — sha256 of the cookie — which
			// is not a credential and cannot be turned back into one.
			put("id", one.ID).
			put("created_at", formatTime(one.CreatedAt)).
			put("last_seen_at", formatTime(one.LastSeenAt)).
			put("expires_at", formatTime(one.ExpiresAt)).
			put("user_agent", one.UserAgent).
			put("ip", one.IP).
			put("current", one.ID == c.session.ID))
	}
	writeJSON(w, http.StatusOK, object{}.put("sessions", rendered))
}

// handleEndOtherSessions is "sign out everywhere", which is what a person does
// after a laptop goes missing. The current session survives, because the
// person doing it is using it.
func (s *Server) handleEndOtherSessions(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	end := &store.SessionsEnd{AccountID: c.account.ID, Keep: c.session.ID}
	if !s.submit(w, r, end) {
		return
	}
	writeJSON(w, http.StatusOK, object{}.put("ended", end.Ended))
}

// --- Shared shapes ----------------------------------------------------------

// opensSession is a write job that opens a browser session as part of what it
// does: a login, the setup of the first owner, an accepted invitation.
type opensSession interface {
	store.WriteJob
	Seed() *store.SessionSeed
}

// signIn is the tail every way in shares: mint a cookie value, fill in the
// session the job is about to write, commit it, and set the cookie.
//
// The cookie value exists here and in the browser and nowhere else — the row
// is keyed by its sha256 — which is what makes a read of the database hand out
// no live session.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request, job opensSession) bool {
	if s.writer == nil {
		writeError(w, http.StatusServiceUnavailable, "writes are not available")
		return false
	}
	value, err := newSessionValue()
	if err != nil {
		slog.Error("could not mint a session", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to start a session")
		return false
	}
	at := time.Now()
	expires := at.Add(s.sessionLife)
	*job.Seed() = store.SessionSeed{
		SessionID: store.SessionID(value),
		ExpiresAt: expires.UnixNano(),
		UserAgent: trimUserAgent(r.Header.Get("User-Agent")),
		IP:        clientIP(r),
		Now:       at.UnixNano(),
	}

	if err := s.writer.Submit(r.Context(), job); err != nil {
		switch {
		case errors.Is(err, store.ErrBadToken):
			writeError(w, http.StatusForbidden, store.ErrBadToken.Error())
		case errors.Is(err, store.ErrSetupDone):
			writeError(w, http.StatusForbidden, store.ErrSetupDone.Error())
		default:
			s.submitFailure(w, err)
		}
		return false
	}
	http.SetCookie(w, s.sessionCookieFor(r, value, expires))
	return true
}

// maxUserAgentLength bounds what is stored from a header a client controls.
// The session list shows it to a person; it is not parsed and nothing depends
// on it, so the only thing worth defending is the size of the row.
const maxUserAgentLength = 400

func trimUserAgent(raw string) string {
	if len(raw) > maxUserAgentLength {
		return raw[:maxUserAgentLength]
	}
	return raw
}

// accountResponse is the shape every endpoint that answers with one account
// uses: who it is and whether it runs the server. The standing fields belong
// to the owner's view of somebody else (see fullAccountResponse), and the hash
// is not reachable from here at all.
func accountResponse(a *store.Account) object {
	return object{}.
		put("id", a.ID).
		put("email", a.Email).
		put("name", a.Name).
		put("owner", a.Owner)
}

// membershipsResponse renders the projects an account can reach, with its role
// in each.
func membershipsResponse(projects []store.Membership) []object {
	rendered := make([]object, 0, len(projects))
	for _, project := range projects {
		rendered = append(rendered, object{}.
			put("id", project.ProjectID).
			put("name", project.Name).
			put("role", project.Role))
	}
	return rendered
}

// readEmail validates the sign-in name. The rule is deliberately thin: the
// email is an identifier the owner types, never an address the server writes
// to, so anything that is one token with an `@` in it is one this server can
// carry (Decision 1).
func readEmail(w http.ResponseWriter, raw string) (string, bool) {
	email := strings.TrimSpace(raw)
	if err := validEmail(email); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return "", false
	}
	return email, true
}

// maxEmailLength is the longest address RFC 5321 allows.
const maxEmailLength = 254

func validEmail(email string) error {
	if email == "" {
		return errors.New("an email is required")
	}
	if len(email) > maxEmailLength {
		return fmt.Errorf("an email must be at most %d characters", maxEmailLength)
	}
	local, domain, found := strings.Cut(email, "@")
	if !found || local == "" || domain == "" || strings.Contains(domain, "@") ||
		strings.ContainsAny(email, " \t\r\n,;") {
		return errors.New("that does not look like an email address")
	}
	return nil
}

// readPassword checks the length and hashes, answering 422 itself when the
// length is wrong.
func readPassword(w http.ResponseWriter, password string) ([]byte, bool) {
	hash, err := store.HashPassword(password)
	if err != nil {
		if errors.Is(err, store.ErrPasswordLength) {
			writeError(w, http.StatusUnprocessableEntity, err.Error())
			return nil, false
		}
		slog.Error("could not hash a password", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to store the password")
		return nil, false
	}
	return hash, true
}

// --- Where a link points ----------------------------------------------------

// originForPrint is the origin of a link the server prints with no request in
// hand: the setup link at startup (Decision 11). `TRACEPAD_URL` wins, because
// behind a proxy the listen address is not an address anybody can use.
func (s *Server) originForPrint() string {
	if base := s.configuredOrigin(); base != "" {
		return base
	}
	return "http://" + config.DisplayHost(s.http.Addr)
}

// originFor is the origin of a link handed to somebody who is on the interface
// already — an invitation. The request's own `Host` is the best guess there
// is, because it is the address that person just used (Decision 11).
func (s *Server) originFor(r *http.Request) string {
	if base := s.configuredOrigin(); base != "" {
		return base
	}
	scheme := "http"
	if overTLS(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// configuredOrigin is `TRACEPAD_URL` reduced to a scheme and a host, or "".
// A value that will not parse is ignored rather than fatal: it is the CLI's
// "which server" as well, and a link is not worth refusing to start over.
func (s *Server) configuredOrigin() string {
	raw := strings.TrimSpace(s.publicURL)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		slog.Warn("TRACEPAD_URL is not a URL; printed links use the request's host instead",
			"value", raw)
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}
