package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/tracepad/tracepad/internal/store"
)

// Sessions (spec 004): a session is not a stored entity but the set of traces
// that named it, so the endpoint is a roll-up plus a page of that set.
//
// The listing (spec 007 #1, #2) is the same idea one level up: the roll-up of
// every session, aggregated from `traces` on the way out. No new table and no
// ingest-time aggregate — a session is a grouping, and a stored grouping is a
// second source of truth to keep consistent.

// sessionListFilters is every query parameter the session listing accepts.
var sessionListFilters = []string{"from", "to", "environment", "user_id"}

// handleListSessions serves the filtered, cursor-paginated session listing,
// most recent activity first.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	known := append(append([]string{}, sessionListFilters...),
		"limit", "cursor", "direction", "count")
	values, err := queryParams(r, known...)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := pageSize(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	backward, err := pageDirection(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	counting, err := wantsCount(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	filter, err := sessionFilter(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// One row beyond the page tells us whether there is another one in the
	// direction we are scanning.
	filter.Limit = limit + 1
	filter.Backward = backward
	raw := values.Get("cursor")
	if raw != "" {
		cursor, err := decodeSessionCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		filter.After = cursor
	}

	sessions, err := s.store.Sessions(r.Context(), project.ID, filter)
	if err != nil {
		readFailed(w, r, "failed to list sessions", err)
		return
	}

	sessions, prev, next := trimPage(sessions, limit, backward, raw,
		func(row *store.SessionRow) string {
			return encodeCursor(strconv.FormatInt(row.LastSeen, 10), row.ID)
		})
	rows := make([]object, 0, len(sessions))
	for _, row := range sessions {
		rows = append(rows, renderSessionRow(row))
	}
	answer := object{}.
		put("sessions", rows).
		put("next_cursor", next).
		put("prev_cursor", prev)
	if counting {
		total, err := s.store.CountSessions(r.Context(), project.ID, filter, countCap+1)
		if err != nil {
			readFailed(w, r, "failed to count sessions", err)
			return
		}
		value, stopped := capped(total)
		answer = answer.put("total", value).put("total_capped", stopped)
	}
	writeJSON(w, http.StatusOK, answer)
}

// renderSessionRow renders one row of the listing: the same six fields
// `GET /api/v1/sessions/{id}` puts around its page of traces, in the order
// spec 007 declares them. The single-session response keeps the order spec 004
// wrote down for it — the same fields either way, and neither contract is
// rearranged to save six lines here.
func renderSessionRow(row *store.SessionRow) object {
	return object{}.
		put("id", row.ID).
		put("trace_count", row.TraceCount).
		put("error_count", row.ErrorCount).
		putSome("total_cost", row.TotalCost).
		putSome("first_seen", formatInstant(row.FirstSeen)).
		putSome("last_seen", formatInstant(row.LastSeen))
}

// sessionFilter reads the listing's filters. The window bounds the *traces*,
// half-open like everywhere else, so a session appears when any of its traces
// falls inside it.
func sessionFilter(values url.Values) (store.SessionFilter, error) {
	filter := store.SessionFilter{UserID: values.Get("user_id")}
	environment, err := filterList(values, "environment")
	if err != nil {
		return filter, err
	}
	filter.Environment = environment
	for _, bound := range []struct {
		name   string
		target **int64
	}{
		{"from", &filter.From},
		{"to", &filter.To},
	} {
		raw := values.Get(bound.name)
		if raw == "" {
			continue
		}
		instant, err := parseTime(bound.name, raw)
		if err != nil {
			return filter, err
		}
		*bound.target = &instant
	}
	return filter, nil
}

func decodeSessionCursor(raw string) (*store.SessionCursor, error) {
	parts, err := decodeCursor(raw, 2)
	if err != nil {
		return nil, err
	}
	lastSeen, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor")
	}
	return &store.SessionCursor{LastSeen: lastSeen, ID: parts[1]}, nil
}

// handleGetSession serves one session with its traces, paginated by the same
// cursor as the trace listing.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	// No `count` here, unlike the two listings: `trace_count` below is that
	// number already, and exactly, so a capped second one beside it would
	// be worse than none (spec 009, API contract).
	values, err := queryParams(r, "limit", "cursor", "direction")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := pageSize(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	backward, err := pageDirection(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "session id must not be empty")
		return
	}

	session, err := s.store.Session(r.Context(), project.ID, id)
	if err != nil {
		readFailed(w, r, "failed to read the session", err)
		return
	}
	if session == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("session %q not found", id))
		return
	}

	filter := store.TraceFilter{SessionID: id, Limit: limit + 1, Backward: backward}
	raw := values.Get("cursor")
	if raw != "" {
		cursor, err := decodeTraceCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		filter.After = cursor
	}
	traces, err := s.store.Traces(r.Context(), project.ID, filter)
	if err != nil {
		readFailed(w, r, "failed to list the session's traces", err)
		return
	}

	traces, prev, next := trimPage(traces, limit, backward, raw,
		func(row *store.TraceRow) string {
			return encodeCursor(strconv.FormatInt(row.Timestamp, 10), row.ID)
		})
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
		put("next_cursor", next).
		put("prev_cursor", prev))
}
