package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"github.com/tracepad/tracepad/internal/store"
)

// The raw archive as an API (spec 019 #2, #3). Every accepted export has been
// kept byte for byte since spec 002 #9 and had no reader but the sweeper;
// these two endpoints are the reader, and `tracepad export --otlp` is their
// first client. Nothing here is admin-scoped: the archive is the project's
// data as the traces are, so a key that reads traces reads the batches they
// came from (#9).

// Header names the body endpoint answers with, so a client can name a file and
// label a manifest without a second request (spec 019 #3).
const (
	headerReceivedAt = "X-Tracepad-Received-At"
	headerDialect    = "X-Tracepad-Dialect"
)

// The archive's own page and count bounds (spec 019, API contract).
const (
	// defaultRawPageSize is bigger than the read API's 50 because nobody
	// reads this listing: a replay walks the whole of it, and a page is a
	// round trip, not a screen.
	defaultRawPageSize = 100
	// rawCountCap is far above the read API's 1000. There the count answers
	// "roughly how much matches this filter" for a human choosing a page;
	// here it answers "how many batches is this export about to send", and
	// "1000+" would not answer it.
	rawCountCap = 100_000
)

// handleListRaw serves the archive oldest first. It is the one listing in this
// API whose natural order is forward: a replay has to preserve arrival order,
// because a receiver that does not upsert by span id — a file, a stream — must
// see the spans in the order the world produced them (spec 019 #3).
func (s *Server) handleListRaw(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "since", "until", "limit", "cursor", "direction", "count")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := defaultRawPageSize
	if values.Get("limit") != "" {
		if limit, err = pageSize(values); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
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
	filter, err := rawFilter(values)
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
		cursor, err := decodeRawCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		filter.After = cursor
	}

	batches, err := s.store.RawBatches(project.ID, filter)
	if err != nil {
		slog.Error("list raw batches failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to list the raw batches")
		return
	}
	batches, prev, next := trimPage(batches, limit, backward, raw,
		func(row *store.RawBatchRow) string {
			return encodeCursor(strconv.FormatInt(row.ReceivedAt, 10),
				strconv.FormatInt(row.ID, 10))
		})

	rows := make([]object, 0, len(batches))
	for _, row := range batches {
		rows = append(rows, object{}.
			put("id", row.ID).
			put("received_at", formatTime(row.ReceivedAt)).
			put("dialect", row.Dialect).
			put("content_type", row.ContentType).
			put("content_encoding", row.ContentEncoding).
			put("size_bytes", row.SizeBytes))
	}
	answer := object{}.
		put("batches", rows).
		put("next_cursor", next).
		put("prev_cursor", prev)
	if counting {
		total, err := s.store.CountRawBatches(project.ID, filter, rawCountCap+1)
		if err != nil {
			slog.Error("count raw batches failed", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to count the raw batches")
			return
		}
		stopped := total > rawCountCap
		answer = answer.put("total", min(total, rawCountCap)).put("total_capped", stopped)
	}
	writeJSON(w, http.StatusOK, answer)
}

// handleGetRawBatch serves one body as it was received. Like
// `/observations/{id}/io` (spec 004 #3) and a run's items (spec 014 #19) it is
// exempt from the response budget, and for the same reason: a cut body is not
// a smaller batch, it is a broken one.
func (s *Server) handleGetRawBatch(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	if _, err := queryParams(r); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("raw batch id must be a positive whole number, got %q", raw))
		return
	}

	batch, err := s.store.RawBatchBody(project.ID, id)
	if err != nil {
		slog.Error("read raw batch failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to read the raw batch")
		return
	}
	// Another project's id and one the sweeper has taken answer the same
	// way: neither exists to this key, and saying which would leak the
	// existence of the other project's archive.
	if batch == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("raw batch %d not found", id))
		return
	}

	w.Header().Set("Content-Type", batch.ContentType)
	w.Header().Set(headerReceivedAt, formatTime(batch.ReceivedAt))
	if batch.Dialect != "" {
		w.Header().Set(headerDialect, batch.Dialect)
	}
	w.WriteHeader(http.StatusOK)
	w.Write(batch.Body)
}

// rawFilter reads the window the listing pages over. Half-open like every
// other time window in this API: `since` inclusive, `until` exclusive, so
// walking a timeline never replays a batch twice.
func rawFilter(values url.Values) (store.RawFilter, error) {
	var filter store.RawFilter
	for _, bound := range []struct {
		name   string
		target **int64
	}{
		{"since", &filter.Since},
		{"until", &filter.Until},
	} {
		value := values.Get(bound.name)
		if value == "" {
			continue
		}
		instant, err := parseTime(bound.name, value)
		if err != nil {
			return filter, err
		}
		*bound.target = &instant
	}
	return filter, nil
}

// decodeRawCursor restores the `(received_at, id)` keyset.
func decodeRawCursor(value string) (*store.RawCursor, error) {
	parts, err := decodeCursor(value, 2)
	if err != nil {
		return nil, err
	}
	receivedAt, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor")
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid cursor")
	}
	return &store.RawCursor{ReceivedAt: receivedAt, ID: id}, nil
}

// rawBlock renders what `GET /api/v1/system` says about the archive
// (spec 019 #4): whether it is being written, how big it is, the window it
// covers, and the traces that fall before it — the honest edge of the promise
// that the data can leave whole.
func (s *Server) rawBlock(projectID string) (object, error) {
	summary, err := s.store.RawSummary(projectID)
	if err != nil {
		return nil, err
	}
	block := object{}.
		put("enabled", s.storeRaw).
		put("batches", summary.Batches).
		put("bytes", summary.StoredBytes).
		put("oldest_received_at", instantOrNull(summary.Oldest)).
		put("newest_received_at", instantOrNull(summary.Newest)).
		put("traces_before_window", summary.TracesBeforeWindow)
	return block, nil
}

// instantOrNull renders an optional instant: an empty archive has no window,
// and `null` says so where the epoch would read as 1970.
func instantOrNull(instant *int64) any {
	if instant == nil {
		return nil
	}
	return formatTime(*instant)
}
