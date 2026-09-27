package server

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

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

// setupTokenLife is how long a printed setup link works (Decision 32). A day
// is "deployed tonight, set up in the morning"; past it, a link still sitting
// in a log is a standing way to take the deployment, and a restart prints a
// fresh one for whoever still needs it.
const setupTokenLife = 24 * time.Hour

// mintSetupToken generates the token for this start, or clears it when the
// server already has an owner (Decision 9) or setup is off (Decision 32).
//
// In memory and per start, so a token from a log file yesterday opens nothing
// today, and a restart is the recovery if the link was lost.
func (s *Server) mintSetupToken() {
	s.setSetupToken("")
	if s.store == nil || s.setupOff {
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
	s.setSetupToken(base64.RawURLEncoding.EncodeToString(raw))
}

// currentSetupToken reads the token this start minted, or "" once it is spent
// or has expired.
func (s *Server) currentSetupToken() string {
	s.setupMu.RLock()
	defer s.setupMu.RUnlock()
	if s.setupToken == "" || !time.Now().Before(s.setupExpires) {
		return ""
	}
	return s.setupToken
}

func (s *Server) setSetupToken(value string) {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	s.setupToken = value
	s.setupExpires = time.Now().Add(setupTokenLife)
}

// SetupURL is the link the operator clicks on a server that has no owner yet,
// or "" once it has one. `printStartup` prints it where the pre-authed key
// link used to be (Decision 9).
//
// The fragment is what keeps the token out of the server's own logs: fragments
// never leave the browser, and the interface strips it from the URL as soon as
// it has read it (spec 006 #8).
func (s *Server) SetupURL() string {
	token := s.currentSetupToken()
	if token == "" {
		return ""
	}
	return s.originForPrint() + "/setup#token=" + token
}

// SetupRequired reports whether this server still needs its first owner. It is
// asked of the store rather than of the token, because an owner can be created
// while the process runs and the answer has to change with it.
func (s *Server) SetupRequired() (bool, error) {
	owners, err := s.store.EnabledOwners()
	return owners == 0, err
}

// handleGetSetup is the one thing the interface can learn without a
// credential: whether to show the setup screen or the login form, and whether
// that screen can do anything — `enabled` is false under TRACEPAD_SETUP=off,
// where the first owner comes from the admin token instead (Decision 32).
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
	writeJSON(w, http.StatusOK, object{}.put("required", required).put("enabled", !s.setupOff))
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
	// Before the body is read: whatever it says, the answer is this one
	// (Decision 32). The router's own limits on a public body (#26) come
	// earlier still.
	if s.setupOff {
		writeError(w, http.StatusForbidden, setupIsOff)
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
	token := s.currentSetupToken()
	if token == "" ||
		subtle.ConstantTimeCompare([]byte(request.Token), []byte(token)) != 1 {
		writeError(w, http.StatusForbidden,
			"this setup link is not valid; restart the server to have it print a new one")
		return
	}
	email, ok := readEmail(w, request.Email)
	if !ok {
		return
	}
	name, ok := readAccountName(w, request.Name)
	if !ok {
		return
	}
	hash, ok := s.readPassword(w, r, request.Password)
	if !ok {
		return
	}

	create := &store.SetupOwner{Email: email, Name: name, Hash: hash}
	if !s.signIn(w, r, create) {
		return
	}
	// The token is spent the moment it works: `GET /api/v1/setup` flips to
	// `{required: false}` and there is nothing left to replay.
	s.setSetupToken("")
	writeJSON(w, http.StatusCreated, object{}.put("account", accountResponse(create.Account)))
}

// setupIsOff is the answer to a setup request on a server started with
// TRACEPAD_SETUP=off, and says what to do instead.
const setupIsOff = "setup is turned off on this server (TRACEPAD_SETUP=off); " +
	"create the first owner with the admin token: tracepad accounts create <email> --owner"

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
	// No account can have an email or a password this long, so the answer is
	// already known — and the limiter below keeps the email as a key for
	// fifteen minutes, so its size must never be the caller's to choose
	// (Decision 26). Both spellings are measured: the one an account can
	// have, and the lower-cased one the limiter keys by, which is longer for
	// the few letters whose lower case takes more bytes. The same 401 as
	// every other failure (Decision 8).
	if len(email) > maxEmailLength || len(strings.ToLower(email)) > maxEmailLength ||
		len(request.Password) > store.MaxPasswordLength {
		writeError(w, http.StatusUnauthorized, wrongCredentials)
		return
	}

	// The attempt is counted before anything is compared, so a burst
	// cannot read the same count and all go on to guess (spec 028 #31).
	attempt, wait := s.limiter.reserve(email, time.Now())
	if wait > 0 {
		w.Header().Set("Retry-After", retryAfterSeconds(wait))
		writeError(w, http.StatusTooManyRequests,
			"too many sign-in attempts for this email; try again shortly")
		return
	}
	// Then a place at the password gate. Turned away there, the password
	// was never compared, so the attempt is given back.
	slot, ok := s.enterPasswordGate(w, r)
	if !ok {
		attempt.cancel()
		return
	}
	account, verified, err := s.checkLogin(slot, email, request.Password)
	slot.Release()
	if err != nil {
		attempt.cancel()
		slog.Error("account lookup failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the account")
		return
	}
	if account == nil || account.Disabled || !verified {
		attempt.failed(time.Now())
		writeError(w, http.StatusUnauthorized, wrongCredentials)
		return
	}
	attempt.succeeded()

	if !s.signIn(w, r, &store.SessionOpen{AccountID: account.ID}) {
		return
	}
	writeJSON(w, http.StatusOK, object{}.put("account", accountResponse(account)))
}

// checkLogin looks the account up and compares the password.
func (s *Server) checkLogin(slot *store.PasswordSlot, email, password string) (*store.Account, bool, error) {
	account, err := s.store.AccountByEmail(email)
	if err != nil {
		return nil, false, err
	}
	// Compared whatever the lookup found, because skipping the comparison
	// is itself an answer: a login that returns in a millisecond for an
	// unknown address and in a quarter of a second for a real one has told
	// you which it was (Decision 8). `Verify` is nil-safe and spends the
	// comparison against a decoy when there is no stored hash, so every one
	// of the four failures costs the same — and costs it inside the
	// password gate, like a real one (spec 028 #31).
	return account, account.Verify(slot, password), nil
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
	if err := store.CheckPasswordLength(request.Password); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// The token before the hash (spec 028 #31): hashing first let anyone
	// with no link at all spend a quarter of a second of the server per
	// request. A token that is live here is checked again when it is
	// spent, since it can be spent or voided in between.
	tokenID := store.SessionID(request.Token)
	live, err := s.store.InviteTokenLive(r.Context(), tokenID, time.Now().UnixNano())
	if err != nil {
		slog.Error("could not read the invitation", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the invitation")
		return
	}
	if !live {
		writeError(w, http.StatusForbidden, store.ErrBadToken.Error())
		return
	}
	slot, ok := s.enterPasswordGate(w, r)
	if !ok {
		return
	}
	hash, ok := hashPassword(w, slot, request.Password)
	slot.Release()
	if !ok {
		return
	}

	accept := &store.InviteAccept{TokenID: tokenID, NewHash: hash}
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

// maxPreferencesBytes caps the preferences object (spec 034 #9): the size is
// what keeps an opaque object from becoming a store.
const maxPreferencesBytes = 16 * 1024

// handlePatchMe changes the display name, the password, the preferences, or
// any of them together.
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
		Preferences json.RawMessage `json:"preferences"`
	}
	if !s.readJSON(w, r, &request) {
		return
	}
	if request.Name == nil && request.Password == nil && request.Preferences == nil {
		writeError(w, http.StatusBadRequest, `nothing to change: send "name", "password" or "preferences"`)
		return
	}
	if request.Name != nil {
		name, ok := readAccountName(w, *request.Name)
		if !ok {
			return
		}
		request.Name = &name
	}
	preferences, ok := readPreferences(w, request.Preferences)
	if !ok {
		return
	}

	account := c.account
	if request.Password == nil {
		update := &store.AccountUpdate{AccountID: account.ID, Name: request.Name,
			Preferences: preferences, Now: time.Now().UnixNano()}
		if !s.submit(w, r, update) {
			return
		}
		writeJSON(w, http.StatusOK, object{}.put("account", accountResponse(update.Account)))
		return
	}

	if err := store.CheckPasswordLength(request.Password.New); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	// Both halves of a password change are bcrypt, and both run here,
	// under one place at the gate that is given back before the job waits
	// for the writer: in the job they held the one writer, and every
	// ingest behind it, for half a second (spec 028 #31). The current
	// password is compared against the hash this request read; the job
	// then refuses unless that hash is still the stored one. A pending
	// account has no hash and no session to be here with, so it spends
	// nothing.
	if account.Pending {
		writeError(w, http.StatusForbidden, store.ErrWrongPassword.Error())
		return
	}
	// A wrong current password is a guess at the password, so it counts
	// where a wrong one at the login does, against the account's email
	// and before the comparison: a session held by somebody else cannot
	// guess any faster here, nor keep the gate full for everybody signing
	// in (spec 028 #31).
	attempt, wait := s.limiter.reserve(account.Email, time.Now())
	if wait > 0 {
		w.Header().Set("Retry-After", retryAfterSeconds(wait))
		writeError(w, http.StatusTooManyRequests,
			"too many wrong passwords for this account; try again shortly")
		return
	}
	slot, ok := s.enterPasswordGate(w, r)
	if !ok {
		attempt.cancel()
		return
	}
	if !account.Verify(slot, request.Password.Current) {
		slot.Release()
		attempt.failed(time.Now())
		writeError(w, http.StatusForbidden, store.ErrWrongPassword.Error())
		return
	}
	attempt.succeeded()
	hash, ok := hashPassword(w, slot, request.Password.New)
	slot.Release()
	if !ok {
		return
	}
	// One job, so that a wrong current password leaves the display name
	// alone too: two would commit the rename and then answer 403, and the
	// person would be reading an error beside their new name.
	//
	// A password change signs every other session out (Decision 4):
	// changing it is what a person does when they think somebody else has
	// it.
	change := &store.PasswordChange{
		AccountID: account.ID, Checked: account,
		NewHash: hash, Keep: c.session.ID, Name: request.Name, Preferences: preferences,
	}
	if err := s.writer.Submit(r.Context(), change); err != nil {
		if errors.Is(err, store.ErrWrongPassword) {
			writeError(w, http.StatusForbidden, store.ErrWrongPassword.Error())
			return
		}
		submitFailure(w, err, apiWrite)
		return
	}
	writeJSON(w, http.StatusOK, object{}.put("account", accountResponse(change.Account)))
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
			submitFailure(w, err, apiWrite)
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
// uses: who it is, whether it runs the server, and how its screens are
// arranged. The standing fields belong to the owner's view of somebody else
// (see fullAccountResponse), and the hash is not reachable from here at all.
func accountResponse(a *store.Account) object {
	preferences := a.Preferences
	if len(preferences) == 0 {
		preferences = json.RawMessage("{}")
	}
	return object{}.
		put("id", a.ID).
		put("email", a.Email).
		put("name", a.Name).
		put("owner", a.Owner).
		put("preferences", preferences)
}

// readPreferences validates what `PATCH /auth/me` may store under
// `preferences` (spec 034 #9): a JSON object under the cap, and nothing more
// is asked of it — the interface owns the shape, the server owns the bytes.
// Stored compacted, so the cap measures the object and not its whitespace.
// A nil message is the field's absence, which changes nothing.
func readPreferences(w http.ResponseWriter, raw json.RawMessage) (json.RawMessage, bool) {
	if raw == nil {
		return nil, true
	}
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(raw, &shape); err != nil || shape == nil {
		writeError(w, http.StatusUnprocessableEntity, "preferences: must be an object")
		return nil, false
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "preferences: must be an object")
		return nil, false
	}
	if compact.Len() > maxPreferencesBytes {
		writeError(w, http.StatusUnprocessableEntity, "preferences: larger than 16 KiB")
		return nil, false
	}
	return json.RawMessage(compact.Bytes()), true
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

// maxAccountNameLength bounds an account's display name, in characters
// (Decision 26): a name is read by people, and a limit in bytes would give a
// Cyrillic name half the room of a Latin one.
const maxAccountNameLength = 200

// readAccountName trims a display name and checks its length, answering 422
// itself when it is too long. Empty is allowed: the name is optional
// everywhere.
func readAccountName(w http.ResponseWriter, raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if utf8.RuneCountInString(name) > maxAccountNameLength {
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("a name must be at most %d characters", maxAccountNameLength))
		return "", false
	}
	return name, true
}

// readPassword checks the length and hashes under the password gate,
// answering 422 itself when the length is wrong and 503 when the gate is full.
// The length is checked first, so a password that was never going to be
// accepted does not wait for a place.
func (s *Server) readPassword(w http.ResponseWriter, r *http.Request, password string) ([]byte, bool) {
	if err := store.CheckPasswordLength(password); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return nil, false
	}
	slot, ok := s.enterPasswordGate(w, r)
	if !ok {
		return nil, false
	}
	defer slot.Release()
	return hashPassword(w, slot, password)
}

// hashPassword hashes a password whose length the caller has already checked
// — the one check that answers the person, with a 422 — for a caller that
// holds its place at the gate. The store checks the length again as part of
// its own contract; failing that here would be a mistake in this file, and it
// is answered like any other failure to hash.
func hashPassword(w http.ResponseWriter, slot *store.PasswordSlot, password string) ([]byte, bool) {
	hash, err := store.HashPassword(slot, password)
	if err != nil {
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

// setPublicURL reads `TRACEPAD_URL` once, when the server is built, so that
// the checks that consult it on every sign-in and cookie write neither parse
// it again nor, when it is malformed, log about it each time. A value that
// will not parse is ignored rather than fatal — it is the CLI's "which
// server" as well, and a link is not worth refusing to start over — and the
// start says so, once.
func (s *Server) setPublicURL(raw string) {
	s.configured = nil
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		// Any scheme but the two a browser loads this server over would
		// be printed in every link and taken as the site's own, refusing
		// each http origin on its host.
		slog.Warn("TRACEPAD_URL is not an http or https URL; printed links use the request's host instead",
			"value", raw)
		return
	}
	s.configured = &publicAddress{
		origin:   parsed.Scheme + "://" + parsed.Host,
		scheme:   parsed.Scheme,
		host:     withoutDefaultPort(parsed.Scheme, parsed.Host),
		hostname: parsed.Hostname(),
	}
}

// publicAddress is `TRACEPAD_URL` as the checks that consult it on every
// request need it, worked out once: the origin a printed link starts with,
// its scheme, its host as a browser writes it (withoutDefaultPort), and the
// bare name an https one vouches for on any port (spec 028 #30).
type publicAddress struct {
	origin, scheme, host, hostname string
}

// configuredOrigin is `TRACEPAD_URL` reduced to a scheme and a host, or "".
func (s *Server) configuredOrigin() string {
	if s.configured == nil {
		return ""
	}
	return s.configured.origin
}
