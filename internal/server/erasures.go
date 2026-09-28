package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// An erasure is a task (spec 047 #6): the confirmed request records it and
// answers `202` with the resource, or `200` when it ended within `?wait`
// (#7), and the two routes here are where it is watched (#14).

// maxEraseWait is the longest `?wait` a confirmed erasure may ask for (#7):
// under the interface's thirty-second clock with its margin, and far under
// the write deadline.
const maxEraseWait = 30

// eraseWait reads `?wait`: whole seconds from 0 to 30, zero when absent.
func eraseWait(raw string) (time.Duration, error) {
	if raw == "" {
		return 0, nil
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 0 || seconds > maxEraseWait {
		return 0, errors.New("wait must be a whole number of seconds from 0 to 30")
	}
	return time.Duration(seconds) * time.Second, nil
}

// erasureLocation is where an erasure's resource is read.
func erasureLocation(projectID, id string) string {
	return "/api/v1/projects/" + projectID + "/erasures/" + id
}

// erasureResource renders an erasure (spec 047 #8). The user id is the
// record's while it runs and null once it has ended (#9), except in the
// answer to the request that started it, which names it: that caller knows
// the id anyway.
//
// backup is the answer's `pre_migration_backup`, read once per request: a
// listing renders a hundred of these, and it is the same file for all.
func (s *Server) erasureResource(e *store.Erasure, requested string, backup any) object {
	var user, phase, started, finished, failure, traces any
	switch {
	case requested != "":
		user = requested
	case e.UserID != "":
		user = e.UserID
	}
	if e.Phase != "" {
		phase = e.Phase
	}
	if e.StartedAt != 0 {
		started = formatTime(e.StartedAt)
	}
	if e.FinishedAt != 0 {
		finished = formatTime(e.FinishedAt)
	}
	if e.Error != "" {
		failure = e.Error
	}
	if e.TracesAtStart != nil {
		traces = *e.TracesAtStart
	}
	answer := object{}.
		put("id", e.ID).
		put("state", e.State).
		put("phase", phase).
		put("user_id", user).
		// The confirmed answer of before is a subset of this one (#8).
		put("dry_run", false).
		put("created_at", formatTime(e.CreatedAt)).
		put("started_at", started).
		put("finished_at", finished).
		put("progress", object{}.
			put("traces_at_start", traces).
			put("traces_deleted", e.Counts.Traces)).
		put("deleted", erasureCounts(e.Counts, true)).
		put("compaction", s.compactionAnswer(e.Compaction))
	// The one copy of the database the erasure does not rewrite, and the
	// day it goes (spec 044 #12).
	if backup != nil {
		answer = answer.put("pre_migration_backup", backup)
	}
	return answer.put("error", failure)
}

// startErasure records a confirmed erasure and answers with it: `202` while it
// runs, `200` once it has ended, within `wait` when the caller asked to wait
// (#6, #7). A second request for the same user answers the first's (#11).
func (s *Server) startErasure(w http.ResponseWriter, r *http.Request, project *store.Project, userID,
	confirm string, wait time.Duration) {
	if s.writer == nil {
		writeError(w, http.StatusServiceUnavailable, "writes are not available")
		return
	}
	// `Now` is kept with the erasure: the freeze is a question about the
	// retention window, and a chunk asks it of the later of this clock and
	// its own (spec 047 #27 d).
	e, err := s.store.StartErasure(r.Context(), s.writer, store.UserErasure{
		ProjectID: project.ID,
		UserID:    userID,
		Confirm:   confirm,
		Now:       time.Now().UnixNano(),
	})
	if err != nil {
		submitFailure(w, err, apiWrite)
		return
	}
	if wait > 0 && !e.Ended() {
		// The erasure is recorded whatever this read does: a failure
		// here answers what the request already knows.
		waited, err := s.store.AwaitErasure(r.Context(), project.ID, e.ID, wait)
		switch {
		case err != nil && !errors.Is(err, context.Canceled):
			slog.Error("failed to read an erasure while waiting for it", "erasure", e.ID, "err", err)
		case waited != nil:
			e = waited
		}
	}
	status := http.StatusAccepted
	if e.Ended() {
		status = http.StatusOK
	}
	w.Header().Set("Location", erasureLocation(project.ID, e.ID))
	writeJSON(w, status, s.erasureResource(e, userID, s.backupAnswer()))
}

// handleGetErasure is one erasure of the project (#14).
func (s *Server) handleGetErasure(w http.ResponseWriter, r *http.Request) {
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
	var e *store.Erasure
	if !s.readInSlot(w, r, "failed to read the erasure", func(ctx context.Context) (err error) {
		e, err = s.store.Erasure(ctx, project.ID, r.PathValue("erasure_id"))
		return err
	}) {
		return
	}
	if e == nil {
		// Unknown, removed 30 days after its end (#15), or another
		// project's: the same answer for all three.
		writeError(w, http.StatusNotFound, "no such erasure")
		return
	}
	writeJSON(w, http.StatusOK, s.erasureResource(e, "", s.backupAnswer()))
}

// handleListErasures is the project's erasures, those under way first and then
// newest first, at most a hundred and not paginated (#14, #28): what the
// Settings card and the user page look in after a reload.
func (s *Server) handleListErasures(w http.ResponseWriter, r *http.Request) {
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
	var erasures []*store.Erasure
	if !s.readInSlot(w, r, "failed to read the erasures", func(ctx context.Context) (err error) {
		erasures, err = s.store.Erasures(ctx, project.ID)
		return err
	}) {
		return
	}
	rendered := make([]object, 0, len(erasures))
	backup := s.backupAnswer()
	for _, e := range erasures {
		rendered = append(rendered, s.erasureResource(e, "", backup))
	}
	writeJSON(w, http.StatusOK, object{}.put("erasures", rendered))
}
