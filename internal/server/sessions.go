package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/tracepad/tracepad/internal/store"
)

// Sessions (spec 004): a session is not a stored entity but the set of traces
// that named it, so the endpoint is a roll-up plus a page of that set.

// handleGetSession serves one session with its traces, paginated by the same
// cursor as the trace listing.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "limit", "cursor")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := pageSize(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "session id must not be empty")
		return
	}

	session, err := s.store.Session(project.ID, id)
	if err != nil {
		slog.Error("read session failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the session")
		return
	}
	if session == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", id))
		return
	}

	filter := store.TraceFilter{SessionID: id, Limit: limit + 1}
	if raw := values.Get("cursor"); raw != "" {
		cursor, err := decodeTraceCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		filter.After = cursor
	}
	traces, err := s.store.Traces(project.ID, filter)
	if err != nil {
		slog.Error("list session traces failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list the session's traces")
		return
	}

	var next *string
	if len(traces) > limit {
		traces = traces[:limit]
		last := traces[len(traces)-1]
		cursor := encodeCursor(strconv.FormatInt(last.Timestamp, 10), last.ID)
		next = &cursor
	}
	rows := make([]object, 0, len(traces))
	for _, row := range traces {
		rows = append(rows, renderTraceRow(row))
	}

	writeJSON(w, http.StatusOK, object{}.
		put("id", session.ID).
		put("trace_count", session.TraceCount).
		putSome("total_cost", session.TotalCost).
		// Counted in traces, like everything else about a session: this
		// is how many of its traces failed, not how many spans did.
		put("error_count", session.ErrorCount).
		putSome("first_seen", formatInstant(session.FirstSeen)).
		putSome("last_seen", formatInstant(session.LastSeen)).
		put("traces", rows).
		put("next_cursor", next))
}
