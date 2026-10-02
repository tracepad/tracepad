package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/tracepad/tracepad/internal/rawid"
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
	// headerScrubbedAt says a body is the batch as received minus the spans
	// an erasure took out, and when (spec 044 #2).
	headerScrubbedAt = "X-Tracepad-Scrubbed-At"
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
		// A cursor and a `since` that contradict each other are a client
		// that changed the window and kept its place. The cursor wins for
		// position and the filters still bound the end, so a cursor
		// *before* `since` is not a narrower request — it is two answers
		// to "where does this page start", and answering either would be
		// guessing which one the caller meant (spec 019, edge cases).
		if filter.Since != nil && cursor.ReceivedAt < *filter.Since {
			writeError(w, http.StatusBadRequest,
				"the cursor points at a batch older than since; drop one of the two")
			return
		}
		filter.After = cursor
	}

	batches, err := s.store.RawBatches(r.Context(), project.ID, filter)
	if err != nil {
		readFailed(w, r, "failed to list the raw batches", err)
		return
	}
	batches, prev, next := trimPage(batches, limit, backward, raw,
		func(row *store.RawBatchRow) string {
			return rawid.Cursor(row.ReceivedAt, row.Number)
		})

	rows := make([]object, 0, len(batches))
	for _, row := range batches {
		rows = append(rows, object{}.
			// The batch's number within the project, not the table's
			// rowid: one sequence for every tenant told each how much
			// the others sent, and when (spec 019 #17).
			put("id", rawid.ID(row.Number)).
			put("received_at", formatTime(row.ReceivedAt)).
			put("dialect", row.Dialect).
			put("content_type", row.ContentType).
			put("content_encoding", row.ContentEncoding).
			put("size_bytes", row.SizeBytes).
			// When an erasure rewrote the batch without the erased
			// spans, null for a batch as received (spec 044 #2).
			put("scrubbed_at", instantOrNull(row.ScrubbedAt)))
	}
	answer := object{}.
		put("batches", rows).
		put("next_cursor", next).
		put("prev_cursor", prev)
	if counting {
		total, err := s.store.CountRawBatches(r.Context(), project.ID, filter, rawCountCap+1)
		if err != nil {
			readFailed(w, r, "failed to count the raw batches", err)
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
	// The id is the batch's number within this project, tagged (spec 019
	// #17): there is no other project's batch it could name, and a bare
	// integer is not an id at all.
	id, err := rawid.ParseID(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%q is %s", raw, err.Error()))
		return
	}
	batch, err := s.store.RawBatchBody(r.Context(), project.ID, id)
	if err != nil {
		readFailed(w, r, "failed to read the raw batch", err)
		return
	}
	if batch == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("raw batch %s not found", rawid.ID(id)))
		return
	}

	// As received, with the media put back where ingest factored it out
	// (spec 041 #8): what leaves through here is the batch the client sent,
	// minus what an erasure took out of it (spec 044 #2). Put back before
	// the status is written, which is where a read gives its slot back
	// (spec 043 #16): the bodies are reads like any other.
	body := s.inlineRawMedia(r.Context(), project.ID, batch)
	if r.Context().Err() != nil {
		// The deadline or a hang-up cut the media reads short: the body
		// would not be the batch the client sent, so none is sent, and the
		// read gate answers the deadline (spec 043 #15).
		return
	}
	w.Header().Set("Content-Type", batch.ContentType)
	w.Header().Set(headerReceivedAt, formatTime(batch.ReceivedAt))
	if batch.Dialect != "" {
		w.Header().Set(headerDialect, batch.Dialect)
	}
	if batch.ScrubbedAt != nil {
		w.Header().Set(headerScrubbedAt, formatTime(*batch.ScrubbedAt))
	}
	w.WriteHeader(http.StatusOK)
	w.Write(body)
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

// decodeRawCursor restores the `(received_at, number)` keyset (spec 019 #17).
func decodeRawCursor(value string) (*store.RawCursor, error) {
	receivedAt, number, err := rawid.ParseCursor(value)
	if err != nil {
		return nil, err
	}
	return &store.RawCursor{ReceivedAt: receivedAt, Number: number}, nil
}

// rawBlock renders what `GET /api/v1/system` says about the archive
// (spec 019 #4): whether it is being written, how big it is, the window it
// covers, and the traces that fall before it — the honest edge of the promise
// that the data can leave whole.
func (s *Server) rawBlock(ctx context.Context, projectID string) (object, error) {
	summary, err := s.store.RawSummary(ctx, projectID)
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
