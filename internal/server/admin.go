package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tracepad/tracepad/internal/store"
)

// The admin surface (spec 005): projects, keys, retention windows and
// user-data erasure. Two rules shape every handler in this file.
//
// Authorisation (#11): a project's `sk` key is the administrator of its own
// project — retention, erasure, restore — while anything cross-project needs
// `TRACEPAD_ADMIN_TOKEN`. Its keys are the exception: no key lists, mints or
// revokes keys (spec 045 #4), which the guard enforces. Deleting a project is the exception that needs
// the token even for one's own: an `sk` lives in application config and CI,
// and a leaked application credential must not be able to destroy the data.
//
// Safety (#8): every destructive endpoint is a dry run until confirmed, and it
// executes only when `?confirm=` echoes the identity of what is destroyed —
// the project's name, or the user id. An id is a string you paste; a name is a
// thing you mean, so a typoed or hallucinated target cannot match.

// eraseChunk is how many of a user's traces one erasure transaction removes,
// and eraseChunkHours how many distinct hours it takes them from, whichever
// bound comes first. Erasure is synchronous (#7) but not unbounded: the
// request loops over chunks so that a user with a year of traffic does not
// hold the writer for the length of a single transaction — and since each
// chunk re-rolls the hours it empties in that same transaction (spec 023
// #19), a chunk is one hour's traces: a whole-hour recompute of a dense hour
// is seconds, which is what the aggregator's own jobs already cost the
// writer, and a transaction of several would stall ingest for their sum.
const (
	eraseChunk      = 500
	eraseChunkHours = 1
)

// authorize is who is asking, as the guard already worked it out (spec 028
// Decision 7). It reads the request's context and refuses nothing: a handler
// that runs is a handler whose caller the policy admitted.
//
// A soft-deleted project's key reaches these routes and is refused by the
// handlers that are not part of undoing the deletion (#10): killing the keys
// outright would leave a token-less deployment that deleted its only project
// with no credential able to restore it. `target` is where that is decided,
// because the reach of each route is known there and only there.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (*caller, bool) {
	c := callerFrom(r.Context())
	if c == nil {
		slog.Error("an administrative handler ran without a caller", "path", r.URL.Path)
		writeError(w, http.StatusInternalServerError, "the request was not authorized")
		return nil, false
	}
	return c, true
}

// requireAdmin gates what only the deployment's own token or an owner may do.
// A server with no token configured says so instead of answering
// "unauthorized" to a caller holding a perfectly good key (#11).
func (s *Server) requireAdmin(w http.ResponseWriter, c *caller) bool {
	if c.admin || (c.isSession() && c.account.Owner) {
		return true
	}
	if c.isSession() {
		writeError(w, http.StatusForbidden, "this needs an owner account")
		return false
	}
	if s.adminToken == "" {
		writeError(w, http.StatusForbidden,
			"this needs an owner account or the cross-project admin token, "+
				"and TRACEPAD_ADMIN_TOKEN is not configured on this server")
		return false
	}
	writeError(w, http.StatusForbidden,
		"this needs an owner account or the cross-project admin token; "+
			"a project key administers its own project only")
	return false
}

// grace names the two endpoints a soft-deleted project's key still reaches
// (#10), which are the two that can undo the deletion.
type reach bool

const (
	liveOnly    reach = false
	insideGrace reach = true
)

// target resolves the `{id}` of an administrative request against the caller's
// reach: a project key reaches its own project and nothing else, a session
// reaches the projects its account is a member of (the guard has already said
// so), and the admin token reaches any.
//
// What is left here that the guard cannot do is the soft-deleted project, and
// the reason is `allow`: whether a deleted project is still reachable depends
// on which route this is, and the route is what the handler knows (#10).
func (s *Server) target(w http.ResponseWriter, r *http.Request, c *caller, allow reach) (*store.Project, bool) {
	id := r.PathValue("id")

	project := c.project
	if c.admin {
		found, err := s.store.ProjectByID(r.Context(), id)
		if err != nil {
			slog.Error("project lookup failed", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to read the project")
			return nil, false
		}
		if found == nil {
			writeError(w, http.StatusNotFound, "no such project")
			return nil, false
		}
		project = found
	} else if c.isKey() && project.ID != id {
		writeError(w, http.StatusForbidden,
			"a project key administers its own project only; another project needs an owner account or the admin token")
		return nil, false
	}
	if project == nil {
		// Only reachable by a handler called outside the mux.
		writeError(w, http.StatusNotFound, "no such project")
		return nil, false
	}
	if !project.Deleted() || allow == insideGrace {
		return project, true
	}

	switch {
	case c.admin:
		writeError(w, http.StatusConflict,
			"project "+project.Name+" is deleted; restore it before changing it")
	case c.isSession():
		// A deleted project is not there as far as a person is
		// concerned; the Server tab lists it with `?include=deleted`
		// and restores it from there (spec 028, edge cases).
		writeError(w, http.StatusNotFound, "no such project")
	default:
		// The key is dead for everything but restoring, and saying
		// "unauthorized" is exactly what it now is.
		writeError(w, http.StatusUnauthorized, "unauthorized")
	}
	return nil, false
}

// handleListProjects answers each kind of caller with the projects it can
// reach: every one for the admin token, its own for a project key, and for a
// session the same rows as `me.projects` — with the retention fields and the
// role, which is what the Settings screen reads (spec 028, API contract).
//
// `?include=deleted` shows soft-deleted projects with their purge dates, which
// only an owner or the token can ask for: for anyone else the question can
// only be about somebody else's project.
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "include", "activity")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// `activity=24h` puts each project's traces of the last day on its row
	// (spec 029 #8). One value today, and a parameter rather than a flag,
	// so that a second window can exist without a second field name.
	withActivity := false
	if activity := values.Get("activity"); activity != "" {
		if activity != "24h" {
			writeError(w, http.StatusBadRequest, "activity: only 24h")
			return
		}
		withActivity = true
	}
	includeDeleted := false
	if include := values.Get("include"); include != "" {
		if include != "deleted" {
			writeError(w, http.StatusBadRequest,
				`include takes only "deleted", which adds soft-deleted projects to the listing`)
			return
		}
		if !s.requireAdmin(w, c) {
			return
		}
		includeDeleted = true
	}

	var (
		projects []*store.Project
		roles    map[string]string
	)
	switch {
	case c.admin:
		projects, err = s.store.ListProjects(includeDeleted)
		if err != nil {
			slog.Error("list projects failed", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to read the projects")
			return
		}
	case c.isSession():
		// An owner has every project, so it is the same listing the admin
		// token gets — and `?include=deleted` is the Server tab's table.
		// A member's is `me.projects`, which never holds a soft-deleted
		// one (spec 028, edge cases).
		if c.account.Owner {
			projects, err = s.store.ListProjects(includeDeleted)
			if err != nil {
				slog.Error("list projects failed", "err", err)
				writeError(w, http.StatusInternalServerError, "failed to read the projects")
				return
			}
			roles = make(map[string]string, len(projects))
			for _, project := range projects {
				roles[project.ID] = store.RoleOwner
			}
			break
		}
		reachable, err := s.store.Memberships(c.account.ID)
		if err != nil {
			slog.Error("list projects failed", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to read the projects")
			return
		}
		roles = make(map[string]string, len(reachable))
		for _, one := range reachable {
			// Read one at a time rather than joined: the retention
			// fields belong to `projectResponse`, and a second shape
			// for the same row is how two answers to "what is this
			// project set to" start to disagree. A member's list is a
			// handful of rows.
			project, err := s.store.ProjectByID(r.Context(), one.ProjectID)
			if err != nil {
				slog.Error("project lookup failed", "err", err)
				writeError(w, http.StatusInternalServerError, "failed to read the projects")
				return
			}
			if project == nil {
				continue
			}
			roles[project.ID] = one.Role
			projects = append(projects, project)
		}
	case !c.project.Deleted():
		projects = []*store.Project{c.project}
	}

	rendered := make([]object, 0, len(projects))
	for _, project := range projects {
		body := projectResponse(project)
		if role, ok := roles[project.ID]; ok {
			body = body.put("role", role)
		}
		if withActivity {
			count, err := s.tracesLastDay(project)
			if err != nil {
				slog.Error("count the day's traces failed", "err", err)
				writeError(w, http.StatusInternalServerError, "failed to read the projects")
				return
			}
			body = body.put("traces_24h", count)
		}
		rendered = append(rendered, body)
	}
	writeJSON(w, http.StatusOK, object{}.put("projects", rendered))
}

// tracesLastDay counts a project's traces of the last 24 hours the way
// `GET /api/v1/stats` counts them (spec 029 #8): the rolled hours behind the
// watermark and the raw rows for the tail, so the number costs what one stats
// call costs and trails live traffic by the same lag. A soft-deleted project
// is not there as far as a person is concerned, and its count says so.
func (s *Server) tracesLastDay(project *store.Project) (int64, error) {
	if project.Deleted() {
		return 0, nil
	}
	now := time.Now()
	from, to := now.Add(-24*time.Hour).UnixNano(), now.UnixNano()
	// One bucket for every key: only the count is read, and a day split
	// over two calendar dates is still one day of traces.
	var tally bucket
	err := s.readStats(project, store.StatsFilter{From: &from, To: &to, GroupBy: defaultGroupBy},
		func(string) *bucket { return &tally })
	return tally.count, err
}

// handleCreateProject mints a project and its first key pair. The secret is in
// this response and in no other, ever: only its hash is stored (spec 001 #4).
func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok || !s.requireAdmin(w, c) {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var request struct {
		Name string `json:"name"`
	}
	if !s.readJSON(w, r, &request) {
		return
	}
	if err := validName("project name", request.Name); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	keys, err := store.GenerateKeyPair()
	if err != nil {
		slog.Error("key generation failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to generate a key pair")
		return
	}

	create := &store.ProjectCreate{Name: request.Name, Keys: keys, Origin: keyOrigin(c)}
	if !s.submit(w, r, create) {
		return
	}
	writeJSON(w, http.StatusCreated, projectResponse(create.Project).
		put("public_key", keys.PublicKey).
		put("secret_key", keys.Secret).
		put("note", "the secret key is shown only here; only its hash is stored"))
}

// handleGetProject reads one project. A soft-deleted project's own key still
// reaches this, because a caller about to restore has to be able to see what
// it is restoring and until when (#10).
func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	project, ok := s.target(w, r, c, insideGrace)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, projectResponse(project))
}

// handlePatchProject changes a name or a retention window. Renaming is
// cross-project administration and needs the token; the windows are the
// project's own business (#11).
//
// A window that shrinks destroys data on the next sweep, so it is a dry run
// until confirmed like every other destructive change (#8). A window that
// grows or stays is applied straight away.
func (s *Server) handlePatchProject(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "confirm")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	project, ok := s.target(w, r, c, liveOnly)
	if !ok {
		return
	}

	var request struct {
		Name               string          `json:"name"`
		RetentionDays      json.RawMessage `json:"retention_days"`
		RawRetentionDays   json.RawMessage `json:"raw_retention_days"`
		StatsRetentionDays json.RawMessage `json:"stats_retention_days"`
		// Media is the setting of spec 041 #6: `store` or `placeholder`.
		Media *string `json:"media"`
	}
	if !s.readJSON(w, r, &request) {
		return
	}

	update := &store.ProjectUpdate{ProjectID: project.ID}
	if request.Name != "" {
		if !s.requireAdmin(w, c) {
			return
		}
		if err := validName("project name", request.Name); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		update.Name = &request.Name
	}
	if update.Retention, err = optionalDays("retention_days", request.RetentionDays); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if update.RawWindow, err = optionalDays("raw_retention_days", request.RawRetentionDays); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if update.StatsWindow, err = optionalDays("stats_retention_days", request.StatsRetentionDays); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if request.Media != nil {
		// Asked here as well as in the write, so that a bad value is a 400
		// before a shrinking window's dry run rather than after it.
		if err := store.CheckMediaSetting(*request.Media); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		update.Media = request.Media
	}
	if update.Name == nil && !update.Retention.Set && !update.RawWindow.Set && !update.StatsWindow.Set &&
		update.Media == nil {
		writeError(w, http.StatusBadRequest,
			`nothing to change: send "name", "retention_days", "raw_retention_days", `+
				`"stats_retention_days" or "media"`)
		return
	}

	// The store decides what counts as a shrink, and decides it again from
	// the stored row inside the write transaction; this is the same
	// question asked early, only to choose between previewing and going
	// ahead.
	if !update.Shrinks(project) {
		if !s.submit(w, r, update) {
			return
		}
		writeJSON(w, http.StatusOK, projectResponse(update.Project))
		return
	}

	// From here the change is destructive: what it will cost is shown
	// first, and only an echo of the project's name applies it. Counting is
	// the dry run's job and only the dry run's — on the confirmed path the
	// numbers would be computed and thrown away, and a read that failed
	// would 500 a request that was going to succeed.
	confirm := values.Get("confirm")
	if confirm == "" {
		retention, rawWindow, statsWindow := update.Windows(project)
		counts, err := s.store.RetentionPreview(
			project.ID, retention, rawWindow, statsWindow, time.Now().UnixNano())
		if err != nil {
			slog.Error("retention preview failed", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to read what the new window would delete")
			return
		}
		writeJSON(w, http.StatusOK, dryRun(project.Name, counts,
			object{}.
				put("traces", counts.Traces).
				put("observations", counts.Observations).
				put("scores", counts.Scores).
				put("raw_batches", counts.RawBatches).
				// The rolled hours are named separately because they
				// are the one thing here the trace sweep spares by
				// design (spec 013 #6).
				put("stats_hours", counts.StatsHours).
				// The media bodies the two windows together would
				// make the project stop holding (spec 041 #11, #27).
				put("media", counts.Media).
				put("media_bytes", counts.MediaBytes)).
			put("note", "the shorter window takes effect on the next sweep"))
		return
	}
	update.Confirm = confirm
	if !s.submit(w, r, update) {
		return
	}
	writeJSON(w, http.StatusOK, projectResponse(update.Project))
}

// handleDeleteProject soft-deletes a project: the keys stop authenticating
// now, the data goes after a fixed seven-day grace, and `restore` undoes it
// until then (#9). The admin token is required even for one's own project
// (#11).
func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok || !s.requireAdmin(w, c) {
		return
	}
	values, err := queryParams(r, "confirm")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	project, ok := s.target(w, r, c, insideGrace)
	if !ok {
		return
	}

	confirm := values.Get("confirm")
	if confirm == "" {
		counts, err := s.store.ProjectPreview(project.ID)
		if err != nil {
			slog.Error("project preview failed", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to read what the project holds")
			return
		}
		writeJSON(w, http.StatusOK, dryRun(project.Name, counts,
			object{}.
				put("traces", counts.Traces).
				put("observations", counts.Observations).
				put("scores", counts.Scores).
				put("prompts", counts.Prompts).
				put("raw_batches", counts.RawBatches).
				put("api_keys", counts.APIKeys).
				// The cascade takes both of spec 024's tables, so the
				// preview names both: a review backlog somebody else
				// is working through is a reason not to press this.
				put("annotation_queues", counts.AnnotationQueues).
				put("annotation_items", counts.AnnotationItems).
				put("media", counts.Media).
				put("media_bytes", counts.MediaBytes)).
			put("note", "the keys stop working immediately; the data is restorable for seven days"))
		return
	}

	deletion := &store.ProjectDelete{ProjectID: project.ID, Confirm: confirm, Now: time.Now().UnixNano()}
	if !s.submit(w, r, deletion) {
		return
	}
	// 202, not 200: the row is stamped now, but destroying the data is the
	// sweeper's job after the grace window (#9).
	writeJSON(w, http.StatusAccepted, projectResponse(deletion.Project).
		put("note", "restore it with POST /api/v1/projects/"+deletion.Project.ID+"/restore until the purge date"))
}

// handleRestoreProject undoes a soft delete. It needs no confirmation: it
// destroys nothing, and a safety net that is awkward to pull is not one.
func (s *Server) handleRestoreProject(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	project, ok := s.target(w, r, c, insideGrace)
	if !ok {
		return
	}
	restore := &store.ProjectRestore{ProjectID: project.ID}
	if !s.submit(w, r, restore) {
		return
	}
	writeJSON(w, http.StatusOK, projectResponse(restore.Project))
}

// handleListKeys lists a project's public keys, each with who minted it and
// when it was last used (spec 045 #8, #9). The secrets are not here because
// they are not anywhere: the store keeps only their hashes.
func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
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
	// Before the stored times, never after: see `unwritten`.
	unwritten := s.keyUses.unwritten()
	keys, err := s.store.ProjectKeys(project.ID)
	if err != nil {
		slog.Error("list keys failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the keys")
		return
	}
	rendered := make([]object, 0, len(keys))
	for _, key := range keys {
		rendered = append(rendered, keyResponse(key, unwritten))
	}
	writeJSON(w, http.StatusOK, object{}.put("keys", rendered))
}

// maxKeyName bounds a key's name (spec 045 #6): enough to say which program
// holds it, short enough to fit a row. In characters, as an account's name is
// (spec 028 #26).
const maxKeyName = 64

// readKeyName trims a key's name and checks it, answering 422 itself as an
// account's name does (spec 045 #20). A name is printed in the listing, the
// Keys card and an account's deletion preview, so a character that is not
// what it looks like is refused rather than rendered: a control character or a
// line or paragraph separator, which opens a line of its own, and a format
// character — a bidirectional override, a zero-width space, a joiner — which
// makes one key's name read as another's.
func readKeyName(w http.ResponseWriter, raw string) (string, bool) {
	name := strings.TrimSpace(raw)
	if utf8.RuneCountInString(name) > maxKeyName {
		writeError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("a key's name must be at most %d characters", maxKeyName))
		return "", false
	}
	for _, r := range name {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			writeError(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("a key's name cannot contain the invisible or control character %U", r))
			return "", false
		}
	}
	return name, true
}

// handleCreateKey mints another key pair for a project. Several active pairs
// are what makes rotation zero-downtime: create, move the SDKs, revoke (#12).
// The body is optional and carries the key's name (spec 045 #6).
func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
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
	body, ok := s.readAPIBody(w, r)
	if !ok {
		return
	}
	var request struct {
		Name string `json:"name"`
	}
	if len(bytes.TrimSpace(body)) > 0 {
		if err := decodeStrict(body, &request); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	name, ok := readKeyName(w, request.Name)
	if !ok {
		return
	}
	keys, err := store.GenerateKeyPair()
	if err != nil {
		slog.Error("key generation failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to generate a key pair")
		return
	}
	create := &store.KeyCreate{ProjectID: project.ID, Keys: keys, Name: name, Origin: keyOrigin(c)}
	if !s.submit(w, r, create) {
		return
	}
	writeJSON(w, http.StatusCreated, object{}.
		put("public_key", keys.PublicKey).
		put("secret_key", keys.Secret).
		put("name", name).
		put("scopes", strings.Fields(store.AllScopes)).
		put("created_at", create.CreatedAt).
		put("created_by", mintedBy(create.Origin, c.role)).
		put("note", "the secret key is shown only here; only its hash is stored"))
}

// keyOrigin is who is minting, as the key records it (spec 045 #8). A key
// never mints (#4), so the guard has left a session or the admin token.
func keyOrigin(c *caller) store.KeyOrigin {
	if c.isSession() {
		return store.OriginAccount(c.account)
	}
	return store.KeyOrigin{Via: store.MintedByAdminToken}
}

// mintedBy renders a key's `created_by` (spec 045, API contract): the kind
// always, and for an account its id while it exists, the email it had when it
// minted, and its standing in the project now.
func mintedBy(origin store.KeyOrigin, standing string) object {
	by := object{}.put("kind", origin.Via)
	if origin.Via != store.MintedByAccount {
		return by
	}
	if origin.AccountID != "" {
		by = by.put("account_id", origin.AccountID)
	}
	return by.put("email", origin.Email).put("standing", standing)
}

// keyResponse is one key as the listing answers it. Its last use is the later
// of what is stored and what is waiting for the next flush, so a key used a
// second ago does not read as idle (spec 045 #9).
func keyResponse(key store.KeyInfo, unwritten map[string]int64) object {
	return object{}.
		put("public_key", key.PublicKey).
		put("name", key.Name).
		put("scopes", key.Scopes).
		put("created_at", key.CreatedAt).
		put("created_by", mintedBy(key.CreatedBy, key.Standing)).
		put("last_used_at", lastUsed(key, unwritten))
}

// lastUsed is a key's last use for a response: RFC 3339, or nil — which the
// API renders as null, its way of saying "never". `unwritten` is the uses the
// store may not have yet, taken before the key was read.
func lastUsed(key store.KeyInfo, unwritten map[string]int64) any {
	at := int64(0)
	if key.LastUsedAt != nil {
		at = *key.LastUsedAt
	}
	if pending := unwritten[key.PublicKey]; pending > at {
		at = pending
	}
	if at == 0 {
		return nil
	}
	return formatTime(at)
}

// handleRevokeKey removes one key pair. Revoking the last one leaves a project
// that cannot ingest, so that one asks for the echo (#12).
func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "confirm")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	project, ok := s.target(w, r, c, liveOnly)
	if !ok {
		return
	}
	publicKey := r.PathValue("public_key")

	keys, err := s.store.ProjectKeyIDs(project.ID)
	if err != nil {
		slog.Error("list keys failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the keys")
		return
	}
	known := false
	for _, key := range keys {
		known = known || key == publicKey
	}
	if !known {
		writeError(w, http.StatusNotFound, "this project has no key "+publicKey)
		return
	}

	confirm := values.Get("confirm")
	if len(keys) == 1 && confirm == "" {
		writeJSON(w, http.StatusOK, object{}.
			put("dry_run", true).
			put("would_delete", object{}.put("api_keys", 1)).
			put("confirm", project.Name).
			put("note", "this is the project's last key: revoking it stops ingest until another is created"))
		return
	}

	revoke := &store.KeyRevoke{ProjectID: project.ID, PublicKey: publicKey, Confirm: confirm}
	if !s.submit(w, r, revoke) {
		return
	}
	writeJSON(w, http.StatusOK, object{}.
		put("dry_run", false).
		put("deleted", object{}.put("api_keys", 1)).
		put("public_key", publicKey))
}

// handleEraseUserData erases everything the queryable stores hold about one
// user (#7): the traces filed under the id, their observations, payloads and
// scores. Raw bodies are deliberately untouched — they are an archive on its
// own schedule, and `docs/retention.md` states that position and its two
// caveats rather than hiding them.
func (s *Server) handleEraseUserData(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "confirm")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	project, ok := s.target(w, r, c, liveOnly)
	if !ok {
		return
	}
	userID := r.PathValue("user_id")
	if userID == "" {
		writeError(w, http.StatusBadRequest, "the user id must not be empty")
		return
	}

	// Counting is the dry run's job and only the dry run's. On the way to a
	// confirmed erasure the numbers would be computed and thrown away, and
	// a read that failed would 500 a request that was going to succeed —
	// the count is what the caller is told, never what the erasure needs.
	if values.Get("confirm") == "" {
		counts, runs, err := s.store.UserDataPreview(project.ID, userID)
		if err != nil {
			slog.Error("user data preview failed", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to read what this user's data is")
			return
		}
		// Erasure overrides the pin a run puts on its traces (spec 014
		// #14): the runs that will lose traces are named, so the operator
		// sees the hole before it opens. The echo here is the user id,
		// because the user is what is being erased (#8). The shape is the
		// one every deletion of traces answers with (spec 035 #1).
		writeJSON(w, http.StatusOK, deletionPreview(counts, runs, userID,
			"raw OTLP bodies are not erased; they expire on the raw retention window"))
		return
	}

	// Every chunk is a transaction that leaves the store consistent on its
	// own: the traces go, and the hours they occupied are re-rolled in the
	// same commit (spec 013 #7, spec 023 #19). So a client that hangs up
	// between chunks — a closed tab, the interface's thirty-second clock
	// (spec 010 #10) — loses nothing but the answer, and repeating the
	// request finishes the rest. `Now` is read once: the freeze is a
	// question about the retention window, and a request is not long
	// enough to move it.
	var erased store.DeleteCounts
	now := time.Now().UnixNano()
	for {
		chunk := &store.UserDataErase{
			ProjectID: project.ID,
			UserID:    userID,
			Confirm:   values.Get("confirm"),
			Limit:     eraseChunk,
			HourLimit: eraseChunkHours,
			Now:       now,
		}
		if !s.submit(w, r, chunk) {
			return
		}
		erased.Traces += chunk.Counts.Traces
		erased.Observations += chunk.Counts.Observations
		erased.Scores += chunk.Counts.Scores
		erased.Payloads += chunk.Counts.Payloads
		erased.AnnotationItems += chunk.Counts.AnnotationItems
		erased.Media += chunk.Counts.Media
		erased.MediaBytes += chunk.Counts.MediaBytes
		if !chunk.More {
			break
		}
	}

	writeJSON(w, http.StatusOK, object{}.
		put("dry_run", false).
		put("deleted", deletedCounts(erased)).
		put("user_id", userID))
}

// dryRun renders the preview shape every destructive endpoint answers with
// before it is confirmed (#8): what would go, how far back it reaches, and the
// exact string that would make it happen.
func dryRun(confirm string, counts store.DeleteCounts, wouldDelete object) object {
	return object{}.
		put("dry_run", true).
		put("would_delete", wouldDelete).
		putSome("oldest", oldestTime(counts)).
		put("confirm", confirm)
}

// oldestTime renders the arrival of the oldest affected row, or nothing when
// the operation affects nothing.
func oldestTime(counts store.DeleteCounts) string {
	if counts.Oldest == 0 {
		return ""
	}
	return formatTime(counts.Oldest)
}

func projectResponse(p *store.Project) object {
	body := object{}.
		put("id", p.ID).
		put("name", p.Name).
		// null is a value here, not an absence: it is how the API says
		// "kept forever", which is the default (#2).
		put("retention_days", p.RetentionDays).
		put("raw_retention_days", p.RawRetentionDays).
		// The rollup's own window: null is forever here too, and it is
		// what keeps the charts when the traces go (spec 013 #6).
		put("stats_retention_days", p.StatsRetentionDays).
		// What ingest does with an image it takes out of a payload
		// (spec 041 #6).
		put("media", p.Media).
		put("created_at", p.CreatedAt)
	if p.Deleted() {
		body = body.
			put("deleted_at", formatTime(*p.DeletedAt)).
			put("purge_at", formatTime(p.PurgeAt()))
	}
	return body
}

// optionalDays reads a window out of a PATCH body. Absent, `null` and a number
// are three different intentions, and collapsing the first two would make
// "leave it alone" indistinguishable from "keep forever".
func optionalDays(field string, raw json.RawMessage) (store.OptionalDays, error) {
	if len(raw) == 0 {
		return store.OptionalDays{}, nil
	}
	if string(raw) == "null" {
		return store.OptionalDays{Set: true}, nil
	}
	var days int
	if err := json.Unmarshal(raw, &days); err != nil || days < 1 || days > store.MaxRetentionDays {
		// The ceiling is not taste: a window is turned into a nanosecond
		// cutoff, and a big enough one overflows int64 into the future,
		// where it matches every row. "Keep it for a million days" is
		// spelled null.
		return store.OptionalDays{}, fmt.Errorf(
			"%s must be a whole number of days between 1 and %d, or null to keep the data forever",
			field, store.MaxRetentionDays)
	}
	return store.OptionalDays{Set: true, Value: &days}, nil
}
