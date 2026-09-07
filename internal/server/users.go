package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// Users (spec 023): the listing of who the traffic belongs to, and one user's
// summary.
//
// The listing answers from the rollup alone (#4). Sorting and paging two
// sources on every page view would be the scan the rollup exists to remove, so
// the listing trails the raw data by the lag `docs/users.md` publishes — the
// same lag the Stats screen already lives with. The single user is the other
// way round: it is one id and its tail is minutes of one user's traces, so it
// merges the live rows and is exact for any id, listed or not.

// userListFilters is every filter `GET /api/v1/users` accepts, beside the page
// parameters every listing takes.
var userListFilters = []string{"sort", "prefix"}

// handleListUsers serves the cursor-paginated user listing.
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	known := append(append([]string{}, userListFilters...),
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
	filter, err := userFilter(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	filter.Limit = limit + 1
	filter.Backward = backward
	raw := values.Get("cursor")
	if raw != "" {
		cursor, err := decodeUserCursor(raw, filter.Sort)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		filter.After = cursor
	}

	users, err := s.store.Users(project.ID, filter)
	if err != nil {
		slog.Error("list users failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list the users")
		return
	}

	users, prev, next := trimPage(users, limit, backward, raw,
		func(row *store.UserRow) string {
			return encodeCursor(store.UserCursorKey(filter.Sort, row), row.UserID)
		})
	rows := make([]object, 0, len(users))
	for _, row := range users {
		rows = append(rows, renderUserRow(row))
	}
	answer := object{}.
		put("users", rows).
		put("next_cursor", next).
		put("prev_cursor", prev)
	if counting {
		total, err := s.store.CountUsers(project.ID, filter, countCap+1)
		if err != nil {
			slog.Error("count users failed", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to count the users")
			return
		}
		value, stopped := capped(total)
		answer = answer.put("total", value).put("total_capped", stopped)
	}
	writeJSON(w, http.StatusOK, answer)
}

// userFilter reads the two filters, refusing an unknown sort by name so the
// caller learns what it may ask for (spec 003's rule).
func userFilter(values url.Values) (store.UserFilter, error) {
	filter := store.UserFilter{
		Sort:   values.Get("sort"),
		Prefix: values.Get("prefix"),
	}
	if filter.Sort == "" {
		filter.Sort = store.UserSorts[0]
	}
	if !slices.Contains(store.UserSorts, filter.Sort) {
		return filter, fmt.Errorf("sort must be one of %s, got %q",
			strings.Join(store.UserSorts, ", "), filter.Sort)
	}
	return filter, nil
}

// decodeUserCursor restores a keyset. The sort decides how the key reads, so a
// cursor taken under one sort and replayed under another is refused here
// rather than silently paging from a number that means something else.
func decodeUserCursor(raw, sortBy string) (*store.UserCursor, error) {
	parts, err := decodeCursor(raw, 2)
	if err != nil {
		return nil, err
	}
	if _, err := store.ParseUserCursorKey(sortBy, parts[0]); err != nil {
		return nil, err
	}
	return &store.UserCursor{Key: parts[0], UserID: parts[1]}, nil
}

// renderUserRow is one row of the listing: the summary shape, repeated by
// `GET /api/v1/users/{id}` so that a client reads one type (spec 023 #5).
func renderUserRow(row *store.UserRow) object {
	return object{}.
		put("user_id", row.UserID).
		put("traces", row.Traces).
		put("error_count", row.ErrorCount).
		putSome("total_cost", row.TotalCost).
		put("sessions", row.Sessions).
		putSome("first_seen", formatInstant(row.FirstSeen*int64(time.Second))).
		putSome("last_seen", formatInstant(row.LastSeen*int64(time.Second)))
}

// handleGetUser serves one user: the rollup plus the live tail (spec 023 #4),
// so the answer is exact for an id the listing has not caught up with yet.
func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	userID := r.PathValue("id")
	if userID == "" {
		writeError(w, http.StatusBadRequest, "the user id must not be empty")
		return
	}

	summary, found, err := s.readUser(project.ID, userID)
	if err != nil {
		slog.Error("read user failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the user")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, fmt.Sprintf("user %q not found", userID))
		return
	}

	writeJSON(w, http.StatusOK, renderUserRow(&summary.UserRow).
		put("latency_ms", object{}.
			put("p50", percentile(summary.Latency, 50)).
			put("p95", percentile(summary.Latency, 95))))
}

// readUser merges the two halves. The instants it returns are Unix seconds,
// like the summary table's — the rolled half only knows hours, and the tail's
// exact instant is floored into the same unit rather than the two being
// rendered by different rules.
//
// `last_seen` is nonetheless exact to the second when the tail holds the user,
// which is the promise Decision 5 makes: a user first seen minutes ago is not
// in the listing at all, and this page still says when they were.
func (s *Server) readUser(projectID, userID string) (*store.UserSummary, bool, error) {
	state, err := s.store.RollupState(projectID)
	if err != nil {
		return nil, false, err
	}
	rolled, err := s.store.UserRollup(projectID, userID)
	if err != nil {
		return nil, false, err
	}
	// Everything at or past the watermark is the live tail's, which is the
	// same split `/stats` makes (spec 013 #5). A project nobody has rolled
	// has a watermark of zero, so the whole of it is live.
	tail, err := s.store.UserTail(projectID, userID, state.RolledUntil*int64(time.Second))
	if err != nil {
		return nil, false, err
	}
	if rolled == nil && tail.Traces == 0 {
		return nil, false, nil
	}

	merged := &store.UserSummary{UserRow: store.UserRow{UserID: userID}}
	if rolled != nil {
		merged.UserRow = rolled.UserRow
		merged.Latency.Merge(rolled.Latency)
	}
	if tail.Traces > 0 {
		merged.Traces += tail.Traces
		merged.ErrorCount += tail.ErrorCount
		merged.Sessions += tail.Sessions
		if tail.TotalCost != nil {
			total := *tail.TotalCost
			if merged.TotalCost != nil {
				total += *merged.TotalCost
			}
			merged.TotalCost = &total
		}
		merged.Latency.Merge(tail.Latency)
		first, last := tail.FirstSeen/int64(time.Second), tail.LastSeen/int64(time.Second)
		if rolled == nil || first < merged.FirstSeen {
			merged.FirstSeen = first
		}
		if last > merged.LastSeen {
			merged.LastSeen = last
		}
	}
	return merged, true, nil
}
