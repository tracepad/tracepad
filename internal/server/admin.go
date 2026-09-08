package server

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// The admin surface (spec 005): projects, keys, retention windows and
// user-data erasure. Two rules shape every handler in this file.
//
// Authorisation (#11): a project's `sk` key is the administrator of its own
// project — retention, keys, erasure, restore — while anything cross-project
// needs `TRACEPAD_ADMIN_TOKEN`. Deleting a project is the exception that needs
// the token even for one's own: an `sk` lives in application config and CI,
// and a leaked application credential must not be able to destroy the data.
//
// Safety (#8): every destructive endpoint is a dry run until confirmed, and it
// executes only when `?confirm=` echoes the identity of what is destroyed —
// the project's name, or the user id. An id is a string you paste; a name is a
// thing you mean, so a typoed or hallucinated target cannot match.

// eraseChunk is how many of a user's traces one erasure transaction removes.
// Erasure is synchronous (#7) but not unbounded: the request loops over
// chunks so that a user with a year of traffic does not hold the writer for
// the length of a single transaction.
const eraseChunk = 500

// caller is who is asking. Exactly one of the two is set: the admin token
// belongs to the deployment and has no project, and a project key has no
// cross-project reach.
type caller struct {
	admin   bool
	project *store.Project
}

// authorize resolves the credentials of an administrative request. A
// soft-deleted project's key resolves here and is refused by the handlers that
// are not part of undoing the deletion (#10): killing the keys outright would
// leave a token-less deployment that deleted its only project with no
// credential able to restore it.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) (*caller, bool) {
	if s.store == nil {
		writeError(w, http.StatusServiceUnavailable, "the API is not available")
		return nil, false
	}
	secret, ok := credential(r.Header.Get("Authorization"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	// Constant time, because this one compares a whole shared secret
	// rather than looking a hash up in an index.
	if s.adminToken != "" &&
		subtle.ConstantTimeCompare([]byte(secret), []byte(s.adminToken)) == 1 {
		return &caller{admin: true}, true
	}
	project, err := s.store.ProjectBySecret(secret)
	if err != nil {
		slog.Error("key lookup failed", "err", err)
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	if project == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return nil, false
	}
	return &caller{project: project}, true
}

// requireAdmin gates what only the deployment's own token may do. A server
// with no token configured says so instead of answering "unauthorized" to a
// caller holding a perfectly good key (#11).
func (s *Server) requireAdmin(w http.ResponseWriter, c *caller) bool {
	if c.admin {
		return true
	}
	if s.adminToken == "" {
		writeError(w, http.StatusForbidden,
			"this needs the cross-project admin token, and TRACEPAD_ADMIN_TOKEN is not configured on this server")
		return false
	}
	writeError(w, http.StatusForbidden,
		"this needs the cross-project admin token; a project key administers its own project only")
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
// reach: a project key reaches its own project and nothing else, the admin
// token reaches any.
func (s *Server) target(w http.ResponseWriter, r *http.Request, c *caller, allow reach) (*store.Project, bool) {
	id := r.PathValue("id")

	if !c.admin {
		if c.project.ID != id {
			writeError(w, http.StatusForbidden,
				"a project key administers its own project only; another project needs the admin token")
			return nil, false
		}
		if c.project.Deleted() && allow == liveOnly {
			// The key is dead for everything but restoring, and
			// saying "unauthorized" is exactly what it now is.
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return nil, false
		}
		return c.project, true
	}

	project, err := s.store.ProjectByID(id)
	if err != nil {
		slog.Error("project lookup failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the project")
		return nil, false
	}
	if project == nil {
		writeError(w, http.StatusNotFound, "no such project")
		return nil, false
	}
	if project.Deleted() && allow == liveOnly {
		writeError(w, http.StatusConflict,
			"project "+project.Name+" is deleted; restore it before changing it")
		return nil, false
	}
	return project, true
}

// handleListProjects answers with every project for the admin token and with
// the caller's own for a project key. `?include=deleted` shows soft-deleted
// projects with their purge dates, which only an administrator can ask for:
// for anyone else the question can only be about somebody else's project.
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	c, ok := s.authorize(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "include")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
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

	var projects []*store.Project
	if c.admin {
		projects, err = s.store.ListProjects(includeDeleted)
		if err != nil {
			slog.Error("list projects failed", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to read the projects")
			return
		}
	} else if !c.project.Deleted() {
		projects = []*store.Project{c.project}
	}

	rendered := make([]object, 0, len(projects))
	for _, project := range projects {
		rendered = append(rendered, projectResponse(project))
	}
	writeJSON(w, http.StatusOK, object{}.put("projects", rendered))
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

	create := &store.ProjectCreate{Name: request.Name, Keys: keys}
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
	if update.Name == nil && !update.Retention.Set && !update.RawWindow.Set && !update.StatsWindow.Set {
		writeError(w, http.StatusBadRequest,
			`nothing to change: send "name", "retention_days", "raw_retention_days" `+
				`or "stats_retention_days"`)
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
				put("stats_hours", counts.StatsHours)).
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
				put("annotation_items", counts.AnnotationItems)).
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

// handleListKeys lists a project's public keys. The secrets are not here
// because they are not anywhere: the store keeps only their hashes.
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
	keys, err := s.store.ProjectKeys(project.ID)
	if err != nil {
		slog.Error("list keys failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the keys")
		return
	}
	rendered := make([]object, 0, len(keys))
	for _, key := range keys {
		rendered = append(rendered, object{}.
			put("public_key", key.PublicKey).
			put("created_at", key.CreatedAt))
	}
	writeJSON(w, http.StatusOK, object{}.put("keys", rendered))
}

// handleCreateKey mints another key pair for a project. Several active pairs
// are what makes rotation zero-downtime: create, move the SDKs, revoke (#12).
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
	keys, err := store.GenerateKeyPair()
	if err != nil {
		slog.Error("key generation failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to generate a key pair")
		return
	}
	create := &store.KeyCreate{ProjectID: project.ID, Keys: keys}
	if !s.submit(w, r, create) {
		return
	}
	writeJSON(w, http.StatusCreated, object{}.
		put("public_key", keys.PublicKey).
		put("secret_key", keys.Secret).
		put("created_at", create.CreatedAt).
		put("note", "the secret key is shown only here; only its hash is stored"))
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

	keys, err := s.store.ProjectKeys(project.ID)
	if err != nil {
		slog.Error("list keys failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the keys")
		return
	}
	known := false
	for _, key := range keys {
		known = known || key.PublicKey == publicKey
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
		// #14): the runs that will lose traces are named here, so the
		// operator sees the hole before it opens.
		affected := make([]object, 0, len(runs))
		for _, run := range runs {
			affected = append(affected, object{}.
				put("id", run.ID).
				put("dataset", run.Dataset).
				put("traces", run.Traces))
		}
		// The echo here is the user id, because the user is what is
		// being erased (#8).
		writeJSON(w, http.StatusOK, object{}.
			put("dry_run", true).
			put("would_delete", object{}.
				put("traces", counts.Traces).
				put("observations", counts.Observations).
				put("scores", counts.Scores).
				// The queue items pointing at those traces (spec 024 #3):
				// they go with the traces, so the preview says so rather
				// than leaving the docs to promise it alone.
				put("annotation_items", counts.AnnotationItems)).
			putSome("oldest", oldestTime(counts)).
			// `affected_runs`, not `runs`: the dataset deletion's dry
			// run already answers with a `runs` count, and one key
			// that is a number on one destructive preview and a list
			// of objects on another is a trap for the shared client
			// type that reads both.
			put("affected_runs", affected).
			put("confirm", userID).
			put("note", "raw OTLP bodies are not erased; they expire on the raw retention window"))
		return
	}

	var erased store.DeleteCounts
	touched := map[int64]bool{}
	for {
		chunk := &store.UserDataErase{
			ProjectID: project.ID,
			UserID:    userID,
			Confirm:   values.Get("confirm"),
			Limit:     eraseChunk,
		}
		if !s.submit(w, r, chunk) {
			return
		}
		erased.Traces += chunk.Counts.Traces
		erased.Observations += chunk.Counts.Observations
		erased.Scores += chunk.Counts.Scores
		erased.Payloads += chunk.Counts.Payloads
		erased.AnnotationItems += chunk.Counts.AnnotationItems
		for _, hour := range chunk.Hours {
			touched[hour] = true
		}
		if chunk.Counts.Traces < int64(eraseChunk) {
			break
		}
	}

	// The statistics are data derived from what was just erased, so the
	// hours it emptied are recomputed before the 200 that promises the
	// user's data is gone (spec 013 #7). An hour whose raw rows retention
	// already took is frozen and the job leaves it alone (#11) — the
	// aggregates carry no user id, and `docs/retention.md` states that
	// position rather than hiding it.
	now := time.Now().UnixNano()
	for hour := range touched {
		if !s.submit(w, r, store.RollHour(project.ID, hour, now)) {
			return
		}
	}

	writeJSON(w, http.StatusOK, object{}.
		put("dry_run", false).
		put("deleted", object{}.
			put("traces", erased.Traces).
			put("observations", erased.Observations).
			put("scores", erased.Scores).
			put("payloads", erased.Payloads).
			put("annotation_items", erased.AnnotationItems)).
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
