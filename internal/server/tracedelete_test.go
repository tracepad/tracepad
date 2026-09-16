package server

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// Deleting traces (spec 035, Testing — server): the two doors, each a dry run
// until the echo, the bulk form's rounds, and who may open them.

type deletePreview struct {
	DryRun      bool             `json:"dry_run"`
	Matched     *int64           `json:"matched"`
	WouldDelete map[string]int64 `json:"would_delete"`
	Oldest      string           `json:"oldest"`
	Runs        []struct {
		ID      string `json:"id"`
		Dataset string `json:"dataset"`
		Traces  int64  `json:"traces"`
	} `json:"affected_runs"`
	Confirm string `json:"confirm"`
	Note    string `json:"note"`
}

type deleteAnswer struct {
	DryRun  bool             `json:"dry_run"`
	Deleted map[string]int64 `json:"deleted"`
	ID      string           `json:"id"`
	More    *bool            `json:"more"`
}

// TestDeleteOneTrace (#1): the preview names what hangs off the trace and the
// run holding it, an unknown id is 404 either way, a wrong echo is 400 and
// changes nothing, and the confirmed answer counts what went.
func TestDeleteOneTrace(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)
	// A fourth trace, held by a run — deleted like any other, and the
	// preview names the run (#6) — with a queue item and a score on it.
	h.postItems(t, "golden", map[string]any{"input": "q"})
	run := h.createRun(t, "golden", nil)
	h.seed(t, &model.Trace{ID: traceHex(4), Name: "chat", Environment: "production", RunID: run.ID},
		&model.Observation{TraceID: traceHex(4), ID: spanHex(4), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase + 3000*ms, EndTime: seedBase + 3100*ms})
	h.putConfigs(t, "accuracy")
	expectStatus(t, h.putQueue(t, "review", "accuracy"), 201)
	h.addTarget(t, "review", map[string]any{"trace_id": traceHex(4)})
	h.postScore(t, map[string]any{"trace_id": traceHex(4), "name": "accuracy", "value": 1})

	path := "/api/v1/traces/" + traceHex(4)
	rec := h.call(t, "DELETE", path, nil)
	expectStatus(t, rec, 200)
	preview := decodeJSON[deletePreview](t, rec)
	if !preview.DryRun || preview.Confirm != traceHex(4) {
		t.Fatalf("preview = %+v, want a dry run asking for the trace id", preview)
	}
	if preview.Matched != nil {
		t.Errorf("the single form answered with matched = %d; that is the bulk form's", *preview.Matched)
	}
	for kind, want := range map[string]int64{"traces": 1, "observations": 1, "scores": 1, "annotation_items": 1} {
		if preview.WouldDelete[kind] != want {
			t.Errorf("would_delete[%s] = %d, want %d", kind, preview.WouldDelete[kind], want)
		}
	}
	if preview.Oldest == "" || !strings.Contains(preview.Note, "raw OTLP bodies are not deleted") {
		t.Errorf("preview = %+v, want the oldest arrival and the raw-bodies note", preview)
	}
	if len(preview.Runs) != 1 || preview.Runs[0].ID != run.ID || preview.Runs[0].Dataset != "golden" || preview.Runs[0].Traces != 1 {
		t.Errorf("affected_runs = %+v, want the run named with its one trace", preview.Runs)
	}
	if got := h.countTraces(t); got != 4 {
		t.Fatalf("traces = %d after a dry run, want all four still there", got)
	}

	expectError(t, h.call(t, "DELETE", "/api/v1/traces/"+traceHex(9), nil), 404, "not found")
	expectError(t, h.call(t, "DELETE", "/api/v1/traces/"+traceHex(9)+"?confirm="+traceHex(9), nil), 404, "not found")
	expectError(t, h.call(t, "DELETE", "/api/v1/traces/nope", nil), 400, "32 lower-case hex")
	expectError(t, h.call(t, "DELETE", path+"?confirm="+traceHex(1), nil), 400, traceHex(4))
	expectError(t, h.call(t, "DELETE", path+"?limit=1", nil), 400, "unknown query parameter")
	if got := h.countTraces(t); got != 4 {
		t.Fatalf("a refused deletion took a trace: %d left", got)
	}

	rec = h.call(t, "DELETE", path+"?confirm="+traceHex(4), nil)
	expectStatus(t, rec, 200)
	answer := decodeJSON[deleteAnswer](t, rec)
	if answer.DryRun || answer.ID != traceHex(4) || answer.More != nil {
		t.Fatalf("answer = %+v, want the deletion reported with its id and no rounds", answer)
	}
	for kind, want := range map[string]int64{"traces": 1, "observations": 1, "scores": 1, "annotation_items": 1} {
		if answer.Deleted[kind] != want {
			t.Errorf("deleted[%s] = %d, want %d", kind, answer.Deleted[kind], want)
		}
	}
	if _, reported := answer.Deleted["payloads"]; !reported {
		t.Errorf("deleted = %v, want payloads counted", answer.Deleted)
	}
	if got := h.countTraces(t); got != 3 {
		t.Errorf("traces = %d, want the other three left", got)
	}
	expectStatus(t, h.get(t, "/api/v1/traces/"+traceHex(4)), 404)
	// The queue stands, one item fewer; the run stands, one trace fewer.
	if counts := h.queueCounts(t, "review"); counts.Pending != 0 {
		t.Errorf("queue counts = %+v, want the item gone with its trace", counts)
	}
	expectStatus(t, h.get(t, "/api/v1/runs/"+run.ID), 200)
}

// TestDeleteTracesByFilter (#2, #4, #5): the filters are the listing's, `to`
// is required, the preview counts exactly, and a confirmed request deletes one
// round at a time until `more` is false — after which a repeat deletes
// nothing.
func TestDeleteTracesByFilter(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	// A corpus past the listing's count cap, in two environments, over two
	// hours, in one batch so the suite does not pay the commit window a
	// thousand times.
	const (
		staging = 20
		total   = countCap + staging + 5
	)
	batch := &store.IngestBatch{ProjectID: h.project.ID}
	for n := 1; n <= total; n++ {
		environment := "production"
		if n <= staging {
			environment = "staging"
		}
		// The last few sit in the next hour, so a round crosses a chunk
		// boundary.
		start := seedBase + int64(n)*ms
		if n > total-3 {
			start += 3600 * 1000 * ms
		}
		batch.Traces = append(batch.Traces, &model.Trace{ID: traceHex(n), Name: "chat", Environment: environment})
		batch.Observations = append(batch.Observations, &model.Observation{
			TraceID: traceHex(n), ID: spanHex(n), Type: model.TypeSpan, Level: model.LevelDefault,
			StartTime: start, EndTime: start + ms})
	}
	if err := h.writer.Submit(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	to := url.QueryEscape(formatTime(seedBase + 2*3600*1000*ms))

	// The set has to be closed (#2).
	expectError(t, h.call(t, "DELETE", "/api/v1/traces", nil), 400, "to is required")
	expectError(t, h.call(t, "DELETE", "/api/v1/traces?environment=staging", nil), 400, "to is required")
	// The listing's own validation, and the round's bounds.
	expectError(t, h.call(t, "DELETE", "/api/v1/traces?to="+to+"&nope=1", nil), 400, "unknown query parameter")
	expectError(t, h.call(t, "DELETE", "/api/v1/traces?to="+to+"&status=maybe", nil), 400, "status")
	expectError(t, h.call(t, "DELETE", "/api/v1/traces?to="+to+"&limit=0", nil), 400, "limit")
	expectError(t, h.call(t, "DELETE", "/api/v1/traces?to="+to+"&limit=1001", nil), 400, "limit")
	if got := h.countTraces(t); got != total {
		t.Fatalf("traces = %d after the refusals, want all %d", got, total)
	}

	// Exact past the cap, and the echo is the project's name.
	rec := h.call(t, "DELETE", "/api/v1/traces?to="+to, nil)
	expectStatus(t, rec, 200)
	preview := decodeJSON[deletePreview](t, rec)
	if !preview.DryRun || preview.Matched == nil || *preview.Matched != total || preview.WouldDelete["traces"] != total {
		t.Fatalf("preview = %+v, want an exact match of %d", preview, total)
	}
	if preview.Confirm != h.project.Name || preview.Runs == nil {
		t.Errorf("preview = %+v, want the project name as the echo and affected_runs present", preview)
	}
	// A filter matching nothing is a successful dry run of nothing.
	rec = h.call(t, "DELETE", "/api/v1/traces?to="+to+"&environment=nowhere", nil)
	expectStatus(t, rec, 200)
	if empty := decodeJSON[deletePreview](t, rec); *empty.Matched != 0 || empty.Oldest != "" {
		t.Errorf("an empty filter previewed %+v, want zero and no oldest", empty)
	}
	if got := h.countTraces(t); got != total {
		t.Fatalf("traces = %d after the previews, want all %d", got, total)
	}

	expectError(t, h.call(t, "DELETE", "/api/v1/traces?to="+to+"&confirm=nope", nil), 400, h.project.Name)

	// Rounds over the staging slice: `limit` bounds one, `more` says there
	// is another, the running counts add up.
	filter := "/api/v1/traces?to=" + to + "&environment=staging&confirm=" + url.QueryEscape(h.project.Name)
	var deleted int64
	var rounds []bool
	for {
		rec = h.call(t, "DELETE", filter+"&limit=7", nil)
		expectStatus(t, rec, 200)
		answer := decodeJSON[deleteAnswer](t, rec)
		if answer.DryRun || answer.More == nil {
			t.Fatalf("round = %+v, want a confirmed answer that says whether there is more", answer)
		}
		deleted += answer.Deleted["traces"]
		rounds = append(rounds, *answer.More)
		if !*answer.More {
			break
		}
		if len(rounds) > 10 {
			t.Fatal("the rounds never ended")
		}
	}
	if deleted != staging || len(rounds) != 3 || !rounds[0] || !rounds[1] || rounds[2] {
		t.Errorf("rounds deleted %d over %v, want %d in three rounds with more, more, done", deleted, rounds, staging)
	}
	if got := h.countTraces(t); got != total-staging {
		t.Errorf("traces = %d, want the production ones left", got)
	}
	// A repeat after `more: false` deletes nothing and is not an error.
	rec = h.call(t, "DELETE", filter, nil)
	expectStatus(t, rec, 200)
	if again := decodeJSON[deleteAnswer](t, rec); again.Deleted["traces"] != 0 || *again.More {
		t.Errorf("the repeat = %+v, want nothing deleted and no more", again)
	}

	// The default round is the whole cap, and one round of the rest crosses
	// the hour boundary: the last three sit in the next hour and are the
	// newest, so they are the first chunk.
	rec = h.call(t, "DELETE", "/api/v1/traces?to="+to+"&confirm="+url.QueryEscape(h.project.Name), nil)
	expectStatus(t, rec, 200)
	answer := decodeJSON[deleteAnswer](t, rec)
	if answer.Deleted["traces"] != countCap || !*answer.More {
		t.Errorf("the default round = %+v, want %d deleted and more", answer, countCap)
	}
	if got := h.countTraces(t); got != total-staging-countCap {
		t.Errorf("traces = %d after the default round, want %d", got, total-staging-countCap)
	}
	// What is left is the oldest: selection is the listing's, newest first.
	ids := listedIDs(t, h.get(t, "/api/v1/traces"))
	if len(ids) != total-staging-countCap || ids[len(ids)-1] != traceHex(staging+1) {
		t.Errorf("left = %v, want the oldest production traces", ids)
	}
}

// TestDeleteTracesIsAnEditorsRoute (spec 028 #15): a viewer is refused both
// doors, an editor's session and the project key are let through, and the
// dry run is what the viewer is refused too.
func TestDeleteTracesIsAnEditorsRoute(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)
	viewer, editor := h.viewer(t), h.editor(t)
	to := url.QueryEscape(formatTime(seedBase + 3600*1000*ms))

	for _, path := range []string{
		"/api/v1/traces/" + traceHex(1),
		"/api/v1/traces?to=" + to,
	} {
		expectStatus(t, h.call(t, "DELETE", path, nil, asSession(viewer), inProject(h.project.ID)), http.StatusForbidden)
		expectStatus(t, h.call(t, "DELETE", path, nil, asSession(editor), inProject(h.project.ID)), 200)
		expectStatus(t, h.call(t, "DELETE", path, nil), 200)
	}
	if got := h.countTraces(t); got != 3 {
		t.Fatalf("traces = %d after the dry runs, want all three", got)
	}
	rec := h.call(t, "DELETE", fmt.Sprintf("/api/v1/traces/%s?confirm=%s", traceHex(1), traceHex(1)),
		nil, asSession(editor), inProject(h.project.ID))
	expectStatus(t, rec, 200)
	if got := h.countTraces(t); got != 2 {
		t.Errorf("traces = %d, want the editor's deletion done", got)
	}
}
