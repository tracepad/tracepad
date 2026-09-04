package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// Reading an eval back over HTTP (spec 014, Testing — summary and compare,
// budgets, HTTP): the three endpoints part 2 adds, against a corpus small
// enough to state the truth about by hand.

const evalStart = 1788220800 * int64(time.Second)

// evalTrace seeds one attempt: a trace stamped with the run and item, and a
// root observation with the answer it produced.
func (h *harness) evalTrace(t *testing.T, run, item, traceID string, opts ...func(*model.Observation)) {
	t.Helper()
	observation := &model.Observation{
		TraceID: traceID, ID: traceID[:16], Type: model.TypeGeneration,
		Name: "answer", Level: model.LevelDefault, Model: "claude-sonnet-5",
		StartTime: evalStart, EndTime: evalStart + 100*int64(time.Millisecond),
		Output: map[string]any{"answer": "because " + traceID[:4]},
	}
	for _, opt := range opts {
		opt(observation)
	}
	h.seed(t, &model.Trace{ID: traceID, Name: "case", Environment: "production",
		RunID: run, ItemID: item}, observation)
}

// score posts one score against a trace, the way a harness does.
func (h *harness) score(t *testing.T, traceID, name string, value any) {
	t.Helper()
	body := map[string]any{"trace_id": traceID, "name": name}
	switch v := value.(type) {
	case string:
		body["data_type"] = "categorical"
		body["string_value"] = v
	default:
		body["value"] = v
	}
	expectStatus(t, h.send(t, "POST", "/api/v1/scores", body), http.StatusCreated)
}

// seedRun declares a dataset with two items and runs it, so that every test
// below starts from the same hand-checkable corpus.
func seedRun(t *testing.T, h *harness, runID string, scores map[string]float64) {
	t.Helper()
	h.postItems(t, "golden",
		map[string]any{"id": itemHex(1), "input": map[string]any{"q": "one"},
			"expected_output": map[string]any{"a": "1"}},
		item(itemHex(2), `{"q":"two"}`))
	h.createRun(t, "golden", map[string]any{"id": runID, "name": "prompt v7",
		"metadata": map[string]any{"prompt": "v7", "judge": "claude-sonnet-5"}})
	first, second := traceOf(runID, 1), traceOf(runID, 2)
	h.evalTrace(t, runID, itemHex(1), first)
	h.evalTrace(t, runID, itemHex(2), second)
	h.score(t, first, "accuracy", scores["first"])
	h.score(t, second, "accuracy", scores["second"])
}

// traceOf makes a trace id that belongs to one run. The run's own tail is part
// of it: two runs sharing a trace id would not be two runs at all, since the
// second delivery would move the trace to the second run (spec 014 #27).
func traceOf(runID string, n int) string {
	return runID[:24] + runID[28:] + fmt.Sprintf("%04x", n)
}

// The summary of a seeded run equals the arithmetic a reader would do by hand.
func TestRunSummaryOverHTTP(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	expectStatus(t, h.send(t, "PUT", "/api/v1/score-configs/accuracy",
		map[string]any{"data_type": "numeric", "direction": "higher", "min": 0, "max": 1}), http.StatusOK)
	runID := runHex(1)
	seedRun(t, h, runID, map[string]float64{"first": 1, "second": 0.5})
	// A third trace of an item the dataset does not have.
	h.evalTrace(t, runID, itemHex(9), traceOf(runID, 3))

	rec := h.get(t, "/api/v1/runs/"+runID)
	expectStatus(t, rec, http.StatusOK)
	body := decodeJSON[struct {
		Summary struct {
			Items struct {
				Total, Covered, Missing, Unknown int64
			} `json:"items"`
			Traces struct {
				Count       int64 `json:"count"`
				AttemptsMax int64 `json:"attempts_max"`
				ErrorCount  int64 `json:"error_count"`
				TotalCost   *float64
				LatencyMs   struct{ P50, P95 *int64 } `json:"latency_ms"`
			} `json:"traces"`
			Scores map[string]struct {
				DataType  string   `json:"data_type"`
				Direction *string  `json:"direction"`
				Count     int64    `json:"count"`
				Mean      *float64 `json:"mean"`
			} `json:"scores"`
			Models  []string `json:"models"`
			Prompts []any    `json:"prompts"`
		} `json:"summary"`
	}](t, rec)

	if body.Summary.Items.Total != 2 || body.Summary.Items.Covered != 2 ||
		body.Summary.Items.Missing != 0 || body.Summary.Items.Unknown != 1 {
		t.Errorf("items = %+v, want both covered and one unknown trace", body.Summary.Items)
	}
	if body.Summary.Traces.Count != 3 || body.Summary.Traces.AttemptsMax != 1 {
		t.Errorf("traces = %+v", body.Summary.Traces)
	}
	// No observation carried a cost, and absent cost stays absent
	// (spec 002 #14).
	if body.Summary.Traces.TotalCost != nil {
		t.Errorf("total_cost = %v, want null when nothing carried one", *body.Summary.Traces.TotalCost)
	}
	accuracy := body.Summary.Scores["accuracy"]
	if accuracy.Count != 2 || accuracy.Mean == nil || *accuracy.Mean != 0.75 {
		t.Errorf("accuracy = %+v, want the mean of 1 and 0.5", accuracy)
	}
	if accuracy.Direction == nil || *accuracy.Direction != "higher" {
		t.Errorf("direction = %v, want the config's", accuracy.Direction)
	}
	if len(body.Summary.Models) != 1 || body.Summary.Models[0] != "claude-sonnet-5" {
		t.Errorf("models = %v", body.Summary.Models)
	}

	// The listing leaves the summary out: choosing a run must not cost what
	// reading one costs.
	list := h.get(t, "/api/v1/datasets/golden/runs")
	expectStatus(t, list, http.StatusOK)
	if strings.Contains(list.Body.String(), `"summary"`) {
		t.Errorf("the run listing carries summaries: %s", list.Body.String())
	}
}

// The trace listing filters by the link: "the traces of this run", and "the
// attempts at this case" (deferred from part 1).
func TestTraceListingFiltersByTheLink(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	runID := runHex(1)
	seedRun(t, h, runID, map[string]float64{"first": 1, "second": 1})
	// A trace of another run, and one of no run at all.
	other := runHex(2)
	h.postItems(t, "golden", item(itemHex(1), `{"q":"one"}`))
	h.createRun(t, "golden", map[string]any{"id": other})
	h.evalTrace(t, other, itemHex(1), traceOf(other, 1))
	seedCorpus(t, h)

	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"by run", "?run_id=" + runID, []string{traceOf(runID, 2), traceOf(runID, 1)}},
		{"by item", "?item_id=" + itemHex(1), []string{traceOf(other, 1), traceOf(runID, 1)}},
		{"by both", "?run_id=" + runID + "&item_id=" + itemHex(1), []string{traceOf(runID, 1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids := listedIDs(t, h.get(t, "/api/v1/traces"+tc.query))
			if strings.Join(ids, ",") != strings.Join(tc.want, ",") {
				t.Errorf("ids = %v, want %v", ids, tc.want)
			}
		})
	}
	// An id of another shape could only match nothing, so it is a 400
	// rather than an empty listing reporting a typo as a fact.
	expectError(t, h.get(t, "/api/v1/traces?run_id=nightly"), http.StatusBadRequest,
		"run_id must be 32 lower-case hex characters")
	expectError(t, h.get(t, "/api/v1/traces?item_id=case-17"), http.StatusBadRequest,
		"item_id must be 32 lower-case hex characters")
}

// The item view shows each case with the attempts made at it, and
// `?unknown=true` adds the traces no item accounts for.
func TestRunItemsView(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	runID := runHex(1)
	seedRun(t, h, runID, map[string]float64{"first": 1, "second": 0})
	// A second attempt at item 1, and a trace of an unknown item.
	h.evalTrace(t, runID, itemHex(1), traceOf(runID, 4))
	h.evalTrace(t, runID, itemHex(9), traceOf(runID, 3))

	body := decodeJSON[runItemsBody](t, h.get(t, "/api/v1/runs/"+runID+"/items"))
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want the version's two", len(body.Items))
	}
	first := body.Items[0]
	if first.ID == nil || *first.ID != itemHex(1) || len(first.Attempts) != 2 {
		t.Fatalf("item 1 = %+v, want two attempts", first)
	}
	if string(first.ExpectedOutput) != `{"a":"1"}` {
		t.Errorf("expected_output = %s", first.ExpectedOutput)
	}
	attempt := first.Attempts[0]
	if attempt.TraceID != traceOf(runID, 1) || attempt.LatencyMs == nil || *attempt.LatencyMs != 100 {
		t.Errorf("attempt = %+v", attempt)
	}
	if len(attempt.Scores) != 1 || attempt.Scores[0].Name != "accuracy" || *attempt.Scores[0].Value != 1 {
		t.Errorf("scores = %+v", attempt.Scores)
	}
	var output struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(attempt.Output, &output); err != nil || output.Answer == "" {
		t.Errorf("output = %s, want the root observation's payload", attempt.Output)
	}

	withUnknown := decodeJSON[runItemsBody](t, h.get(t, "/api/v1/runs/"+runID+"/items?unknown=true"))
	if len(withUnknown.Items) != 3 {
		t.Fatalf("with unknown = %d items, want the unknown one appended", len(withUnknown.Items))
	}
	last := withUnknown.Items[2]
	if last.ID == nil || *last.ID != itemHex(9) || !last.Unknown || len(last.Attempts) != 1 {
		t.Errorf("unknown row = %+v", last)
	}
	if last.Seq != nil {
		t.Errorf("an unknown item reported a seq: %v", *last.Seq)
	}
	expectError(t, h.get(t, "/api/v1/runs/"+runID+"/items?unknown=yes"),
		http.StatusBadRequest, "must be true or false")
}

// runItemsBody is the item view as a client reads it.
type runItemsBody struct {
	Items []struct {
		ID             *string         `json:"id"`
		Seq            *int64          `json:"seq"`
		Unknown        bool            `json:"unknown"`
		ExpectedOutput json.RawMessage `json:"expected_output"`
		Attempts       []struct {
			TraceID   string          `json:"trace_id"`
			LatencyMs *int64          `json:"latency_ms"`
			Output    json.RawMessage `json:"output"`
			Scores    []struct {
				Name  string   `json:"name"`
				Value *float64 `json:"value"`
			} `json:"scores"`
		} `json:"attempts"`
	} `json:"items"`
	NextCursor *string `json:"next_cursor"`
	PrevCursor *string `json:"prev_cursor"`
}

// A payload too big for the budget is cut and says where the whole of it
// lives — the marker's pair is what `/observations/{id}/io` takes.
func TestRunItemsBudgetsThePayloads(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	runID := runHex(1)
	h.postItems(t, "golden", item(itemHex(1), `{"q":"one"}`))
	h.createRun(t, "golden", map[string]any{"id": runID})
	traceID := traceOf(runID, 1)
	h.evalTrace(t, runID, itemHex(1), traceID, func(o *model.Observation) {
		o.Output = map[string]any{"answer": strings.Repeat("x", 20000)}
	})

	rec := h.get(t, "/api/v1/runs/"+runID+"/items?budget=4096")
	expectStatus(t, rec, http.StatusOK)
	body := decodeJSON[struct {
		Items []struct {
			Attempts []struct {
				Output struct {
					Truncated     bool   `json:"truncated"`
					Size          int    `json:"size"`
					TraceID       string `json:"trace_id"`
					ObservationID string `json:"observation_id"`
					Full          string `json:"full"`
				} `json:"output"`
			} `json:"attempts"`
		} `json:"items"`
	}](t, rec)
	marker := body.Items[0].Attempts[0].Output
	if !marker.Truncated || marker.Size < 20000 {
		t.Fatalf("output = %+v, want a marker naming the real size", marker)
	}
	if marker.TraceID != traceID || marker.ObservationID != traceID[:16] {
		t.Fatalf("marker pair = %s/%s, want the trace and its root observation",
			marker.TraceID, marker.ObservationID)
	}
	// The pair resolves: the follow-up the marker names actually works.
	io := h.get(t, marker.Full)
	expectStatus(t, io, http.StatusOK)
	if !strings.Contains(io.Body.String(), strings.Repeat("x", 100)) {
		t.Errorf("the marker's follow-up did not return the payload")
	}
}

// Two runs, compared: the header, the per-name counts and the per-item
// verdicts, all against hand-computed truth.
func TestCompareRuns(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	expectStatus(t, h.send(t, "PUT", "/api/v1/score-configs/accuracy",
		map[string]any{"data_type": "numeric", "direction": "higher"}), http.StatusOK)

	a, b := runHex(1), runHex(2)
	seedRun(t, h, a, map[string]float64{"first": 0.5, "second": 1})
	// The second run answers the same two items: item 1 improves, item 2
	// stays.
	// The same judge, a different prompt: the diff must name the one they
	// disagree about and leave the one they share alone.
	h.createRun(t, "golden", map[string]any{"id": b,
		"metadata": map[string]any{"prompt": "v8", "judge": "claude-sonnet-5"}})
	h.evalTrace(t, b, itemHex(1), traceOf(b, 1))
	h.evalTrace(t, b, itemHex(2), traceOf(b, 2))
	h.score(t, traceOf(b, 1), "accuracy", 1.0)
	h.score(t, traceOf(b, 2), "accuracy", 1.0)

	body := decodeJSON[compareBody](t, h.get(t, "/api/v1/runs/"+a+"/compare/"+b))
	if body.Dataset != "golden" || !body.SameVersion {
		t.Errorf("header = %+v, want one dataset at one version", body)
	}
	if body.A.ID != a || body.B.ID != b {
		t.Errorf("sides = %s and %s", body.A.ID, body.B.ID)
	}
	if len(body.Scores) != 1 {
		t.Fatalf("scores = %+v, want the one name both runs carried", body.Scores)
	}
	accuracy := body.Scores[0]
	if accuracy.A.Mean == nil || *accuracy.A.Mean != 0.75 || accuracy.B.Mean == nil || *accuracy.B.Mean != 1 {
		t.Errorf("means = %v and %v, want 0.75 and 1", accuracy.A.Mean, accuracy.B.Mean)
	}
	if accuracy.Delta == nil || *accuracy.Delta != 0.25 {
		t.Errorf("delta = %v, want 0.25", accuracy.Delta)
	}
	if accuracy.Improved != 1 || accuracy.Regressed != 0 || accuracy.Same != 1 {
		t.Errorf("verdict counts = %+v, want one improved and one unchanged", accuracy)
	}
	if len(body.Items) != 2 {
		t.Fatalf("items = %d, want both", len(body.Items))
	}
	if got := body.Items[0].Scores["accuracy"]; got.Verdict != "improved" || *got.Delta != 0.5 {
		t.Errorf("item 1 = %+v, want improved by 0.5", got)
	}
	if got := body.Items[1].Scores["accuracy"]; got.Verdict != "same" {
		t.Errorf("item 2 = %+v, want same", got)
	}
	if body.Items[0].In != "both" {
		t.Errorf("in = %q, want both", body.Items[0].In)
	}
	// Only the differing metadata keys, both sides.
	if len(body.Metadata) != 1 || string(body.Metadata["prompt"].B) != `"v8"` {
		t.Errorf("metadata diff = %+v, want the one key they disagree about", body.Metadata)
	}

	expectError(t, h.get(t, "/api/v1/runs/"+a+"/compare/"+a), http.StatusBadRequest, "with itself")
	expectError(t, h.get(t, "/api/v1/runs/"+a+"/compare/"+runHex(9)), http.StatusNotFound, "not found")
}

type compareBody struct {
	A struct {
		ID string `json:"id"`
	} `json:"a"`
	B struct {
		ID string `json:"id"`
	} `json:"b"`
	Dataset     string `json:"dataset"`
	SameVersion bool   `json:"same_version"`
	Metadata    map[string]struct {
		A json.RawMessage `json:"a"`
		B json.RawMessage `json:"b"`
	} `json:"metadata"`
	Scores []struct {
		Name      string `json:"name"`
		Direction string `json:"direction"`
		A         struct {
			Mean  *float64 `json:"mean"`
			Count int64    `json:"count"`
		} `json:"a"`
		B struct {
			Mean  *float64 `json:"mean"`
			Count int64    `json:"count"`
		} `json:"b"`
		Delta     *float64 `json:"delta"`
		Improved  int64    `json:"improved"`
		Regressed int64    `json:"regressed"`
		Same      int64    `json:"same"`
		Changed   *int64   `json:"changed"`
	} `json:"scores"`
	Items []struct {
		ID     string `json:"id"`
		Seq    int64  `json:"seq"`
		In     string `json:"in"`
		Scores map[string]struct {
			A       *float64 `json:"a"`
			B       *float64 `json:"b"`
			Delta   *float64 `json:"delta"`
			Verdict string   `json:"verdict"`
		} `json:"scores"`
	} `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// A name with no config cannot improve: the most a comparison can say about it
// is that it changed (#16).
func TestCompareWithoutADirectionSaysChanged(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	a, b := runHex(1), runHex(2)
	seedRun(t, h, a, map[string]float64{"first": 0.5, "second": 1})
	h.createRun(t, "golden", map[string]any{"id": b})
	h.evalTrace(t, b, itemHex(1), traceOf(b, 1))
	h.evalTrace(t, b, itemHex(2), traceOf(b, 2))
	h.score(t, traceOf(b, 1), "accuracy", 1.0)
	h.score(t, traceOf(b, 2), "accuracy", 1.0)

	body := decodeJSON[compareBody](t, h.get(t, "/api/v1/runs/"+a+"/compare/"+b))
	if got := body.Items[0].Scores["accuracy"].Verdict; got != "changed" {
		t.Errorf("verdict = %q, want changed without a direction", got)
	}
	if body.Scores[0].Changed == nil || *body.Scores[0].Changed != 1 {
		t.Errorf("changed = %v, want the one item that moved", body.Scores[0].Changed)
	}
	if body.Scores[0].Improved != 0 {
		t.Errorf("an undirected name reported %d improved", body.Scores[0].Improved)
	}
}

// Runs of two datasets have no items in common, so comparing them cannot mean
// anything (#18).
func TestCompareRefusesDifferentDatasets(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.postItems(t, "golden", item(itemHex(1), `1`))
	h.postItems(t, "other", item(itemHex(2), `2`))
	a := h.createRun(t, "golden", map[string]any{})
	b := h.createRun(t, "other", map[string]any{})
	expectError(t, h.get(t, "/api/v1/runs/"+a.ID+"/compare/"+b.ID),
		http.StatusBadRequest, "different datasets")
}

// A dataset that moved between the two runs: the items outside the
// intersection are labelled rather than dropped or silently compared.
func TestCompareLabelsTheVersionEdges(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	a, b := runHex(1), runHex(2)
	seedRun(t, h, a, map[string]float64{"first": 1, "second": 1})

	// Item 2 is archived and item 3 arrives, so the second run's version
	// holds a different set.
	expectStatus(t, h.send(t, "DELETE", "/api/v1/datasets/golden/items/"+itemHex(2), nil), http.StatusOK)
	h.postItems(t, "golden", item(itemHex(3), `{"q":"three"}`))
	h.createRun(t, "golden", map[string]any{"id": b})
	h.evalTrace(t, b, itemHex(1), traceOf(b, 1))
	h.evalTrace(t, b, itemHex(3), traceOf(b, 3))

	body := decodeJSON[compareBody](t, h.get(t, "/api/v1/runs/"+a+"/compare/"+b))
	if body.SameVersion {
		t.Errorf("same_version = true for runs of two versions")
	}
	labels := map[string]string{}
	for _, item := range body.Items {
		labels[item.ID] = item.In
	}
	want := map[string]string{
		itemHex(1): "both",
		itemHex(2): "only_in_version_a",
		itemHex(3): "only_in_version_b",
	}
	for id, expected := range want {
		if labels[id] != expected {
			t.Errorf("item %s = %q, want %q", id, labels[id], expected)
		}
	}
}

// The two new listings walk at limit=1 in both directions, seeing every row
// once (spec 009 #2).
func TestRunListingsWalkBothWays(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	a, b := runHex(1), runHex(2)
	seedRun(t, h, a, map[string]float64{"first": 1, "second": 1})
	h.createRun(t, "golden", map[string]any{"id": b})
	h.evalTrace(t, b, itemHex(1), traceOf(b, 1))
	h.evalTrace(t, b, itemHex(2), traceOf(b, 2))
	h.evalTrace(t, b, itemHex(9), traceOf(b, 3))

	for _, tc := range []struct {
		name string
		path string
		want int
	}{
		{"run items", "/api/v1/runs/" + b + "/items?unknown=true", 3},
		{"compare", "/api/v1/runs/" + a + "/compare/" + b, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forward := walkIDs(t, h, tc.path, "next")
			if len(forward) != tc.want {
				t.Fatalf("forward walk saw %v, want %d rows", forward, tc.want)
			}
			backward := walkIDs(t, h, tc.path, "prev")
			for i, j := 0, len(backward)-1; i < j; i, j = i+1, j-1 {
				backward[i], backward[j] = backward[j], backward[i]
			}
			if strings.Join(forward, ",") != strings.Join(backward, ",") {
				t.Errorf("forward %v and backward %v disagree", forward, backward)
			}
		})
	}
}

// walkIDs pages one row at a time and returns the ids in the order it saw them.
func walkIDs(t *testing.T, h *harness, path, direction string) []string {
	t.Helper()
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	var (
		seen   []string
		cursor string
	)
	for range 10 {
		url := path + separator + "limit=1&direction=" + direction
		if cursor != "" {
			url += "&cursor=" + cursor
		}
		rec := h.get(t, url)
		expectStatus(t, rec, http.StatusOK)
		body := decodeJSON[struct {
			Items []struct {
				ID *string `json:"id"`
			} `json:"items"`
			NextCursor *string `json:"next_cursor"`
			PrevCursor *string `json:"prev_cursor"`
		}](t, rec)
		if len(body.Items) == 0 {
			break
		}
		id := "null"
		if body.Items[0].ID != nil {
			id = *body.Items[0].ID
		}
		seen = append(seen, id)
		next := body.NextCursor
		if direction == "prev" {
			next = body.PrevCursor
		}
		if next == nil {
			break
		}
		cursor = *next
	}
	return seen
}

// Every row of the item view carries each key once. The renderer appends
// members rather than replacing them, so a row assembled in two steps can
// carry `id` twice — JSON that Go's decoder forgives, by keeping the last
// occurrence, and a strict parser rejects outright (found in review of PR #31).
func TestItemRowsCarryEachKeyOnce(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	runID := runHex(1)
	seedRun(t, h, runID, map[string]float64{"first": 1, "second": 0})
	h.evalTrace(t, runID, itemHex(9), traceOf(runID, 3))
	// And a trace that named the run and no item at all (Decision 29).
	h.seed(t, &model.Trace{ID: traceOf(runID, 4), Name: "case",
		Environment: "production", RunID: runID})

	rec := h.get(t, "/api/v1/runs/"+runID+"/items?unknown=true")
	expectStatus(t, rec, http.StatusOK)
	page := decodeJSON[struct {
		Items []json.RawMessage `json:"items"`
	}](t, rec)
	if len(page.Items) != 4 {
		t.Fatalf("items = %d, want the two known rows and the two unknown ones", len(page.Items))
	}
	for i, row := range page.Items {
		if key, twice := repeatedKey(row); twice {
			t.Errorf("item %d carries %q twice: %s", i, key, row)
		}
	}
}

// repeatedKey reports a key an object carries more than once. It reads the
// tokens because `json.Unmarshal` cannot see the duplicate: it keeps the last
// value and says nothing.
func repeatedKey(raw json.RawMessage) (string, bool) {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if _, err := decoder.Token(); err != nil {
		return "", false
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return "", false
		}
		key, ok := token.(string)
		if !ok {
			return "", false
		}
		if seen[key] {
			return key, true
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return "", false
		}
	}
	return "", false
}

// The skeleton the budget is measured against carries no payloads, so a page
// whose payloads fit is served rather than refused. Measuring the skeleton with
// markers in it charges every payload twice — once as structure, once out of
// the share that structure shrank — and a page of ordinary answers comes back
// as a 400 with a `?budget=` that is wrong by the same amount (found in review
// of PR #31).
func TestRunItemsSkeletonExcludesThePayloads(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	runID := runHex(1)
	var cases []map[string]any
	for i := 1; i <= 4; i++ {
		cases = append(cases, map[string]any{"id": itemHex(i),
			"input":           map[string]any{"q": strings.Repeat("q", 40)},
			"expected_output": map[string]any{"a": strings.Repeat("a", 120)}})
	}
	h.postItems(t, "golden", cases...)
	h.createRun(t, "golden", map[string]any{"id": runID})
	for i := 1; i <= 4; i++ {
		h.evalTrace(t, runID, itemHex(i), traceOf(runID, i), func(o *model.Observation) {
			o.Output = map[string]any{"answer": strings.Repeat("y", 120)}
		})
	}

	rec := h.get(t, "/api/v1/runs/"+runID+"/items?budget=4096")
	expectStatus(t, rec, http.StatusOK)
	body := decodeJSON[runItemsBody](t, rec)
	if len(body.Items) != 4 {
		t.Fatalf("items = %d, want four", len(body.Items))
	}
	for i, item := range body.Items {
		if !strings.Contains(string(item.ExpectedOutput), strings.Repeat("a", 120)) {
			t.Errorf("item %d expected_output was cut: %s", i, item.ExpectedOutput)
		}
		if !strings.Contains(string(item.Attempts[0].Output), strings.Repeat("y", 120)) {
			t.Errorf("item %d output was cut: %s", i, item.Attempts[0].Output)
		}
	}
}

// A config that disagrees with the scores already stored does not relabel
// them. #15 lets a name carry scores of one type under a config declaring
// another — the config governs what comes next and never re-validates the past
// — and Decision 30 reports the type most of the run's scores used. Taking the
// config's type instead makes compare read the string side of a column of
// numbers, find it empty, and call every case `same` (found in review of PR
// #31).
func TestSummaryKeepsTheTypeItsScoresUsed(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	first, second := runHex(1), runHex(2)
	seedRun(t, h, first, map[string]float64{"first": 0, "second": 0})
	h.createRun(t, "golden", map[string]any{"id": second})
	for i := 1; i <= 2; i++ {
		h.evalTrace(t, second, itemHex(i), traceOf(second, i))
		h.score(t, traceOf(second, i), "accuracy", 1)
	}
	// Only now does a config arrive, declaring the name a categorical one.
	expectStatus(t, h.send(t, "PUT", "/api/v1/score-configs/accuracy",
		map[string]any{"data_type": "categorical", "categories": []string{"pass", "fail"}}),
		http.StatusOK)

	summary := decodeJSON[struct {
		Summary struct {
			Scores map[string]struct {
				DataType  string   `json:"data_type"`
				Direction *string  `json:"direction"`
				Mean      *float64 `json:"mean"`
			} `json:"scores"`
		} `json:"summary"`
	}](t, h.get(t, "/api/v1/runs/"+second))
	accuracy := summary.Summary.Scores["accuracy"]
	if accuracy.DataType != "numeric" {
		t.Errorf("data_type = %q, want the numeric type its scores used", accuracy.DataType)
	}
	if accuracy.Mean == nil || *accuracy.Mean != 1 {
		t.Errorf("mean = %v, want the 1 both scores carried", accuracy.Mean)
	}
	if accuracy.Direction != nil {
		t.Errorf("direction = %v, want none: a categorical config has no axis to lend", *accuracy.Direction)
	}

	compared := decodeJSON[compareBody](t, h.get(t,
		"/api/v1/runs/"+first+"/compare/"+second))
	for _, item := range compared.Items {
		verdict := item.Scores["accuracy"]
		if verdict.Verdict != "changed" || verdict.Delta == nil || *verdict.Delta != 1 {
			t.Errorf("item %s = %+v, want the numbers compared, not two empty strings",
				item.ID, verdict)
		}
	}
}

// A cursor the comparison cannot read is a 400, the way it is on every other
// listing: dropping it and serving the first page turns a walk into a loop
// (spec 003 #23).
func TestCompareRefusesAnUnreadableCursor(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	first, second := runHex(1), runHex(2)
	seedRun(t, h, first, map[string]float64{"first": 1, "second": 1})
	h.createRun(t, "golden", map[string]any{"id": second})
	h.evalTrace(t, second, itemHex(1), traceOf(second, 1))

	for _, cursor := range []string{"nonsense", encodeCursor("not-a-number")} {
		expectError(t, h.get(t, "/api/v1/runs/"+first+"/compare/"+second+"?cursor="+cursor),
			http.StatusBadRequest, "cursor")
	}
}

// The project-wide listing (spec 016 #2, Testing — Go): newest first across
// datasets, the two filters, the capped count, and the cursor walk in both
// directions at `limit=1` — the same walk every listing is held to.
func TestProjectRunsListing(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.postItems(t, "a-set", item(itemHex(1), `1`))
	h.postItems(t, "b-set", item(itemHex(2), `2`))
	h.createRun(t, "a-set", map[string]any{"id": runHex(1)})
	h.createRun(t, "a-set", map[string]any{"id": runHex(2)})
	h.createRun(t, "b-set", map[string]any{"id": runHex(3)})
	expectStatus(t, h.send(t, "POST", "/api/v1/runs/"+runHex(2)+"/finish", map[string]any{}), http.StatusOK)

	ids := func(t *testing.T, path string) ([]string, runListResponse) {
		t.Helper()
		rec := h.get(t, path)
		expectStatus(t, rec, http.StatusOK)
		page := decodeJSON[runListResponse](t, rec)
		var out []string
		for _, run := range page.Runs {
			out = append(out, run.ID)
		}
		return out, page
	}

	all, page := ids(t, "/api/v1/runs")
	if want := []string{runHex(3), runHex(2), runHex(1)}; strings.Join(all, ",") != strings.Join(want, ",") {
		t.Errorf("runs = %v, want newest first across datasets %v", all, want)
	}
	if page.Total != nil || page.TotalCapped != nil {
		t.Errorf("a count rode along unasked: total=%v capped=%v", page.Total, page.TotalCapped)
	}
	if page.Runs[0].Dataset != "b-set" {
		t.Errorf("the row does not say which dataset it belongs to: %+v", page.Runs[0])
	}

	byDataset, _ := ids(t, "/api/v1/runs?dataset=a-set")
	if want := []string{runHex(2), runHex(1)}; strings.Join(byDataset, ",") != strings.Join(want, ",") {
		t.Errorf("dataset=a-set = %v, want %v", byDataset, want)
	}
	byStatus, _ := ids(t, "/api/v1/runs?status=finished")
	if want := []string{runHex(2)}; strings.Join(byStatus, ",") != strings.Join(want, ",") {
		t.Errorf("status=finished = %v, want %v", byStatus, want)
	}
	both, _ := ids(t, "/api/v1/runs?dataset=b-set&status=finished")
	if len(both) != 0 {
		t.Errorf("dataset=b-set&status=finished = %v, want nothing", both)
	}
	// A dataset that does not exist is an empty page, not a 404: the name is
	// a filter here, not an address.
	unknown, _ := ids(t, "/api/v1/runs?dataset=nope")
	if len(unknown) != 0 {
		t.Errorf("dataset=nope = %v, want nothing", unknown)
	}

	_, counted := ids(t, "/api/v1/runs?count=1&limit=1&dataset=a-set")
	if counted.Total == nil || *counted.Total != 2 || counted.TotalCapped == nil || *counted.TotalCapped {
		t.Errorf("count=1 → total=%v capped=%v, want 2 and false", counted.Total, counted.TotalCapped)
	}

	expectError(t, h.get(t, "/api/v1/runs?status=done"), http.StatusBadRequest, "status must be")
	expectError(t, h.get(t, "/api/v1/runs?dataset=not%20a%20name"), http.StatusBadRequest, "dataset")
	expectError(t, h.get(t, "/api/v1/runs?cursor=nope"), http.StatusBadRequest, "cursor")

	forward := walkCursor(t, h, "/api/v1/runs?limit=1", false, runIDs)
	backward := walkCursor(t, h, "/api/v1/runs?limit=1", true, runIDs)
	if strings.Join(forward, ",") != strings.Join(all, ",") {
		t.Errorf("forward walk = %v, want %v", forward, all)
	}
	if strings.Join(backward, ",") != strings.Join(all, ",") {
		t.Errorf("backward walk = %v, want %v", backward, all)
	}
}

// runIDs reads the ids off a page of runs, for the cursor walk.
func runIDs(body []byte) []string {
	var page runListResponse
	json.Unmarshal(body, &page)
	var out []string
	for _, run := range page.Runs {
		out = append(out, run.ID)
	}
	return out
}
