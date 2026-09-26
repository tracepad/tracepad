package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// Deleting traces (spec 035): one by id, or every trace the listing's filters
// match. Both wear the ceremony every destructive act in this store wears
// (spec 005 #8): a dry run that says what would go, and the exact string that
// makes it happen — the trace id for one trace, because the id is its only
// identity (#1), and the project's name for a slice of the project (#2).
//
// The bulk form works in rounds (#4): a confirmed request deletes at most
// `limit` traces, in chunks of one hour each, and answers whether there is
// more; the client — the CLI, the dialog — repeats the call. Erasure loops to
// completion inside one request because a user's history is rare and
// bounded; a filter can name a month, and a request that runs for minutes is
// one the interface's thirty-second clock cuts off every time.

const (
	// deleteRound is the most traces one confirmed bulk request removes,
	// and the default.
	deleteRound = 1000
	// deleteRoundChunks is the most chunks — hours, since a chunk is one
	// hour's traces — one round runs, whichever bound comes first (#14). A
	// chunk is a transaction and a commit of its own, and a set spread
	// thinly over many hours is many small chunks: measured on a copy of a
	// real database, 300 traces over 118 hours took 9.2 s, and a round of
	// 1,000 such traces would run past the interface's thirty-second clock
	// every time. Fifty chunks is a few seconds; the answer says `more`.
	deleteRoundChunks = 50
	// deleteNote is the position on raw bodies, stated in every preview
	// rather than left to the docs: a raw batch holds many traces of many
	// kinds, and a trace cannot be cut out of one (#3).
	deleteNote = "raw OTLP bodies are not deleted; they expire on the raw retention window"
)

// handleDeleteTrace is DELETE /api/v1/traces/{id} (#1).
func (s *Server) handleDeleteTrace(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	values, err := queryParams(r, "confirm")
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := r.PathValue("id")
	if !traceID.MatchString(id) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("trace id must be 32 lower-case hex characters, got %q", id))
		return
	}

	// The preview is a read and only the dry run's (#11): a confirmed
	// request computes none of it, so a count that failed cannot 500 a
	// deletion that was going to succeed.
	if values.Get("confirm") == "" {
		counts, runs, err := s.store.TracePreview(project.ID, id)
		if err != nil {
			slog.Error("trace delete preview failed", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to read what this trace holds")
			return
		}
		if counts.Traces == 0 {
			writeError(w, http.StatusNotFound, fmt.Sprintf("trace %q not found", id))
			return
		}
		writeJSON(w, http.StatusOK, deletionPreview(counts, runs, id, deleteNote))
		return
	}

	job := &store.TraceDelete{
		ProjectID: project.ID,
		IDs:       []string{id},
		Confirm:   values.Get("confirm"),
		Now:       time.Now().UnixNano(),
	}
	if !s.submit(w, r, job) {
		return
	}
	writeJSON(w, http.StatusOK, object{}.
		put("dry_run", false).
		put("deleted", deletedCounts(job.Counts)).
		put("id", id).
		put("compaction", s.compactionAnswer(job.CompactionRequested)))
}

// handleDeleteTraces is DELETE /api/v1/traces?<filters>&to= (#2, #4, #5).
func (s *Server) handleDeleteTraces(w http.ResponseWriter, r *http.Request) {
	project, ok := s.apiProject(w, r)
	if !ok {
		return
	}
	known := append(append([]string{}, traceListFilters...), "confirm", "limit")
	values, err := queryParams(r, known...)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := deleteRound
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > deleteRound {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"limit must be a whole number between 1 and %d", deleteRound))
			return
		}
		limit = parsed
	}
	filter, err := traceFilter(values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// `to` is what makes the set a closed one (#2): the listing is half-open
	// on it, so a trace that starts after it can never qualify, and the
	// operator who previewed a thousand does not delete a thousand and
	// forty because ingest kept flowing between the preview and the
	// confirmation.
	if filter.To == nil {
		writeError(w, http.StatusBadRequest,
			"to is required: a deletion by filter names the moment before which traces go, so that what was previewed is what is deleted")
		return
	}

	if values.Get("confirm") == "" {
		// Counted exactly rather than at the listing's cap (spec 009 #12):
		// this is one deliberate act, and "1000+" would leave the operator
		// unable to tell a filter they can finish from one they cannot.
		counts, runs, err := s.store.TraceDeletePreview(project.ID, filter)
		if err != nil {
			slog.Error("trace delete preview failed", "err", err)
			writeError(w, http.StatusInternalServerError, "failed to count the matching traces")
			return
		}
		writeJSON(w, http.StatusOK, object{}.
			put("dry_run", true).
			put("matched", counts.Traces).
			put("would_delete", wouldDelete(counts)).
			putSome("oldest", oldestTime(counts)).
			put("affected_runs", affectedRuns(runs)).
			put("confirm", project.Name).
			put("note", deleteNote))
		return
	}

	// One round: the newest `limit` matches, selected with the listing's own
	// query and nothing recounted (#5) — the set can only have shrunk since
	// the preview, and a set that shrank is not a reason to refuse. One row
	// past the round says whether there is another.
	filter.Limit = limit + 1
	rows, err := s.store.Traces(project.ID, filter)
	if err != nil {
		slog.Error("select the traces to delete failed", "err", err)
		writeError(w, http.StatusInternalServerError, "failed to select the matching traces")
		return
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}

	// Chunks of one hour, at most `TraceDeleteChunk` traces each (#3), each
	// its own transaction: the rows arrive newest first, so an hour's
	// traces are contiguous and a chunk ends where the hour does. A client
	// that hangs up between chunks loses nothing but the answer. The round
	// ends early at `deleteRoundChunks` (#14) and says so with `more`: the
	// traces past it are still there, and the next request takes them.
	var deleted store.DeleteCounts
	var compaction int64
	now := time.Now().UnixNano()
	confirm := values.Get("confirm")
	var chunk []string
	var hour int64
	chunks := 0
	flush := func() bool {
		if len(chunk) == 0 {
			return true
		}
		job := &store.TraceDelete{
			ProjectID: project.ID, IDs: chunk, Confirm: confirm, ByFilter: true, Now: now,
		}
		if !s.submit(w, r, job) {
			return false
		}
		deleted.Traces += job.Counts.Traces
		deleted.Observations += job.Counts.Observations
		deleted.Scores += job.Counts.Scores
		deleted.Payloads += job.Counts.Payloads
		deleted.AnnotationItems += job.Counts.AnnotationItems
		deleted.Media += job.Counts.Media
		deleted.MediaBytes += job.Counts.MediaBytes
		compaction = max(compaction, job.CompactionRequested)
		chunk = nil
		chunks++
		return true
	}
	for _, row := range rows {
		at := store.HourOf(row.Timestamp)
		if len(chunk) > 0 && (at != hour || len(chunk) == store.TraceDeleteChunk) {
			if !flush() {
				return
			}
			if chunks == deleteRoundChunks {
				more = true
				break
			}
		}
		hour = at
		chunk = append(chunk, row.ID)
	}
	if !flush() {
		return
	}
	// A filter that matched nothing ran no chunk, and so checked no echo:
	// one empty job checks it inside the transaction like every other
	// confirmed request's, so a wrong project name is a 400 whether or not
	// the range holds anything (review of PR #74).
	if chunks == 0 && !s.submit(w, r, &store.TraceDelete{
		ProjectID: project.ID, Confirm: confirm, ByFilter: true, Now: now,
	}) {
		return
	}

	writeJSON(w, http.StatusOK, object{}.
		put("dry_run", false).
		put("deleted", deletedCounts(deleted)).
		put("more", more).
		put("compaction", s.compactionAnswer(compaction)))
}

// deletionPreview renders the dry run of a deletion of traces — one trace's,
// or one user's (spec 005 #7): what would go, how far back it reaches, the
// runs that would lose traces, the exact string that makes it happen, and the
// note on raw bodies in the words of the act.
func deletionPreview(counts store.DeleteCounts, runs []store.AffectedRun, confirm, note string) object {
	return object{}.
		put("dry_run", true).
		put("would_delete", wouldDelete(counts)).
		putSome("oldest", oldestTime(counts)).
		put("affected_runs", affectedRuns(runs)).
		put("confirm", confirm).
		put("note", note)
}

// wouldDelete is the preview's counts, in the shape every deletion of traces
// answers with so that the interface's one `DryRun` type reads them all.
func wouldDelete(counts store.DeleteCounts) object {
	return object{}.
		put("traces", counts.Traces).
		put("observations", counts.Observations).
		put("scores", counts.Scores).
		// The queue items pointing at those traces (spec 024 #3): they go
		// with the traces, so the preview says so rather than leaving the
		// docs to promise it alone.
		put("annotation_items", counts.AnnotationItems).
		// The media bodies only these traces point at (spec 041 #11).
		put("media", counts.Media).
		put("media_bytes", counts.MediaBytes)
}

// deletedCounts is the confirmed answer's counts: the preview's, and the
// payloads that went with them.
func deletedCounts(counts store.DeleteCounts) object {
	return object{}.
		put("traces", counts.Traces).
		put("observations", counts.Observations).
		put("scores", counts.Scores).
		put("payloads", counts.Payloads).
		put("annotation_items", counts.AnnotationItems).
		put("media", counts.Media).
		put("media_bytes", counts.MediaBytes)
}

// affectedRuns renders the runs a deletion would take traces from (spec 014
// #14, spec 035 #6), so the operator sees the hole before it opens.
//
// `affected_runs`, not `runs`: the dataset deletion's dry run already answers
// with a `runs` count, and one key that is a number on one destructive
// preview and a list of objects on another is a trap for the shared client
// type that reads both.
func affectedRuns(runs []store.AffectedRun) []object {
	affected := make([]object, 0, len(runs))
	for _, run := range runs {
		affected = append(affected, object{}.
			put("id", run.ID).
			put("dataset", run.Dataset).
			put("traces", run.Traces))
	}
	return affected
}
