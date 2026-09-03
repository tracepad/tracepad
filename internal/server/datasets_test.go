package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// Datasets, items and runs over HTTP (spec 014, Testing — HTTP, budgets,
// ingest): real requests through the real handlers into a real database.

func itemHex(n int) string { return fmt.Sprintf("%032x", 0xd000+n) }
func runHex(n int) string  { return fmt.Sprintf("%032x", 0xe000+n) }

// postItems writes a batch and returns the ids, the version and the change
// count it answered with.
func (h *harness) postItems(t *testing.T, dataset string, items ...map[string]any) itemsWrittenResponse {
	t.Helper()
	rec := h.send(t, "POST", "/api/v1/datasets/"+dataset+"/items", items)
	expectStatus(t, rec, http.StatusCreated)
	return decodeJSON[itemsWrittenResponse](t, rec)
}

func (h *harness) createRun(t *testing.T, dataset string, body map[string]any) runResponse {
	t.Helper()
	rec := h.send(t, "POST", "/api/v1/datasets/"+dataset+"/runs", body)
	expectStatus(t, rec, http.StatusCreated)
	return decodeJSON[runResponse](t, rec)
}

func item(id, input string) map[string]any {
	return map[string]any{"id": id, "input": json.RawMessage(input)}
}

// The envelope: PUT creates and replaces, GET reads, the listing pages by
// name, and a name outside the grammar is refused.
func TestDatasetEnvelopeCRUD(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.send(t, "PUT", "/api/v1/datasets/support-golden", map[string]any{
		"description": "the support golden set", "metadata": map[string]any{"owner": "qa"},
	})
	expectStatus(t, rec, http.StatusOK)
	created := decodeJSON[datasetResponse](t, rec)
	if created.Name != "support-golden" || *created.Description != "the support golden set" ||
		string(created.Metadata) != `{"owner":"qa"}` || created.Version != 0 || created.ItemCount != 0 {
		t.Errorf("created = %+v", created)
	}

	// A PUT replaces the envelope whole: a field left out is cleared.
	rec = h.send(t, "PUT", "/api/v1/datasets/support-golden", map[string]any{"description": "renamed"})
	expectStatus(t, rec, http.StatusOK)
	if got := decodeJSON[datasetResponse](t, rec); got.Metadata != nil && string(got.Metadata) != "null" {
		t.Errorf("metadata = %s after a PUT without it, want null", got.Metadata)
	}
	got := decodeJSON[datasetResponse](t, h.get(t, "/api/v1/datasets/support-golden"))
	if *got.Description != "renamed" {
		t.Errorf("GET = %+v", got)
	}

	expectError(t, h.get(t, "/api/v1/datasets/nope"), http.StatusNotFound, "not found")
	expectError(t, h.send(t, "PUT", "/api/v1/datasets/bad%20name", map[string]any{}), http.StatusBadRequest, "must match")
	expectError(t, h.send(t, "PUT", "/api/v1/datasets/support-golden", map[string]any{"metadata": "text"}),
		http.StatusBadRequest, `"metadata" must be a JSON object`)

	// The listing is by name and pages both ways.
	for _, name := range []string{"alpha", "beta"} {
		expectStatus(t, h.send(t, "PUT", "/api/v1/datasets/"+name, map[string]any{}), http.StatusOK)
	}
	list := decodeJSON[datasetListResponse](t, h.get(t, "/api/v1/datasets"))
	var names []string
	for _, d := range list.Datasets {
		names = append(names, d.Name)
	}
	if strings.Join(names, ",") != "alpha,beta,support-golden" {
		t.Errorf("names = %v, want alphabetical", names)
	}
}

// Items: a batch is one version tick, a re-post none, an edit one row; the
// version parameter resolves history; an archive hides at the new version and
// the history endpoint shows every row.
func TestDatasetItemsRoundTrip(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	first := h.postItems(t, "golden",
		map[string]any{"id": itemHex(1), "input": map[string]any{"q": "one"},
			"expected_output": map[string]any{"a": "1"}, "metadata": map[string]any{"tags": []string{"easy"}},
			"source_trace_id": traceHex(1), "source_observation_id": spanHex(1)},
		item(itemHex(2), `{"q":"two"}`),
		item(itemHex(3), `{"q":"three"}`))
	if first.Version != 1 || first.Changed != 3 || len(first.IDs) != 3 {
		t.Fatalf("first = %+v", first)
	}
	// A dataset came into being on the first POST to its name.
	dataset := decodeJSON[datasetResponse](t, h.get(t, "/api/v1/datasets/golden"))
	if dataset.Version != 1 || dataset.ItemCount != 3 {
		t.Errorf("dataset = %+v", dataset)
	}

	again := h.postItems(t, "golden", item(itemHex(2), `{"q":"two"}`))
	if again.Version != 1 || again.Changed != 0 {
		t.Errorf("re-post = %+v, want unchanged at version 1", again)
	}
	// Whitespace is not a change: bodies are compared compacted.
	spaced := h.postItems(t, "golden", item(itemHex(2), `{ "q" : "two" }`))
	if spaced.Changed != 0 {
		t.Errorf("a re-post with different whitespace counted as a change")
	}
	edit := h.postItems(t, "golden", item(itemHex(2), `{"q":"two, revised"}`))
	if edit.Version != 2 || edit.Changed != 1 {
		t.Errorf("edit = %+v, want version 2 with 1 changed", edit)
	}

	// The single-object shape works too, and generates an id.
	rec := h.send(t, "POST", "/api/v1/datasets/golden/items", map[string]any{"input": "plain text is a value"})
	expectStatus(t, rec, http.StatusCreated)
	generated := decodeJSON[itemsWrittenResponse](t, rec)
	if len(generated.IDs) != 1 || !hexID.MatchString(generated.IDs[0]) || generated.Version != 3 {
		t.Errorf("single = %+v", generated)
	}

	current := decodeJSON[itemListResponse](t, h.get(t, "/api/v1/datasets/golden/items"))
	if current.Version != 3 || len(current.Items) != 4 {
		t.Fatalf("current = version %d with %d items", current.Version, len(current.Items))
	}
	if got := current.Items[1]; got.ID != itemHex(2) || string(got.Input) != `{"q":"two, revised"}` || got.Version != 2 || got.Seq != 1 {
		t.Errorf("item 2 now = %+v", got)
	}
	if got := current.Items[0]; *got.SourceTraceID != traceHex(1) || string(got.ExpectedOutput) != `{"a":"1"}` {
		t.Errorf("item 1 = %+v", got)
	}
	old := decodeJSON[itemListResponse](t, h.get(t, "/api/v1/datasets/golden/items?version=1"))
	if old.Version != 1 || len(old.Items) != 3 || string(old.Items[1].Input) != `{"q":"two"}` {
		t.Errorf("at version 1 = %+v", old)
	}
	expectError(t, h.get(t, "/api/v1/datasets/golden/items?version=9"), http.StatusBadRequest, "there is no version 9")
	expectError(t, h.get(t, "/api/v1/datasets/golden/items?version=-1"), http.StatusBadRequest, "at least 0")
	if empty := decodeJSON[itemListResponse](t, h.get(t, "/api/v1/datasets/golden/items?version=0")); len(empty.Items) != 0 {
		t.Errorf("version 0 = %d items, want the dataset before its first item", len(empty.Items))
	}

	one := decodeJSON[itemResponse](t, h.get(t, "/api/v1/datasets/golden/items/"+itemHex(2)+"?version=1"))
	if string(one.Input) != `{"q":"two"}` {
		t.Errorf("item 2 at version 1 = %+v", one)
	}

	rec = h.send(t, "DELETE", "/api/v1/datasets/golden/items/"+itemHex(2), nil)
	expectStatus(t, rec, http.StatusOK)
	if archived := decodeJSON[itemArchivedResponse](t, rec); archived.Version != 4 || archived.ID != itemHex(2) {
		t.Errorf("archive = %+v", archived)
	}
	expectError(t, h.get(t, "/api/v1/datasets/golden/items/"+itemHex(2)), http.StatusNotFound, "at version 4")
	expectStatus(t, h.get(t, "/api/v1/datasets/golden/items/"+itemHex(2)+"?version=3"), http.StatusOK)
	expectError(t, h.send(t, "DELETE", "/api/v1/datasets/golden/items/"+itemHex(2), nil), http.StatusNotFound, "current version")

	history := decodeJSON[itemVersionsResponse](t, h.get(t, "/api/v1/datasets/golden/items/"+itemHex(2)+"/versions"))
	if len(history.Versions) != 3 || history.Versions[0].Version != 4 || !*history.Versions[0].Archived ||
		history.Versions[2].Version != 1 || *history.Versions[2].Archived {
		t.Errorf("history = %+v, want three rows newest first with the last one archived", history.Versions)
	}
	expectError(t, h.get(t, "/api/v1/datasets/golden/items/"+itemHex(9)+"/versions"), http.StatusNotFound, "has no item")

	// Refusals: no input, an empty batch, a duplicate id, a bad id.
	expectError(t, h.send(t, "POST", "/api/v1/datasets/golden/items", map[string]any{"metadata": map[string]any{}}),
		http.StatusBadRequest, `"input" is required`)
	expectError(t, h.send(t, "POST", "/api/v1/datasets/golden/items", []map[string]any{}), http.StatusBadRequest, "no items")
	expectError(t, h.send(t, "POST", "/api/v1/datasets/golden/items", []map[string]any{item(itemHex(1), `1`), item(itemHex(1), `2`)}),
		http.StatusBadRequest, "item at index 1: id")
	expectError(t, h.send(t, "POST", "/api/v1/datasets/golden/items", item("case-1", `1`)),
		http.StatusBadRequest, "32 lower-case hex")
	expectError(t, h.send(t, "POST", "/api/v1/datasets/golden/items", map[string]any{"input": 1, "source_trace_id": ""}),
		http.StatusBadRequest, "must not be empty")
	expectError(t, h.get(t, "/api/v1/datasets/golden/items/not-hex"), http.StatusBadRequest, "item id must be")
}

// Deleting a dataset is a dry run until the name is echoed (spec 014 #20).
// The items and the runs go; the traces the runs held do not.
func TestDatasetDeleteDryRunAndConfirm(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.postItems(t, "golden", item(itemHex(1), `1`), item(itemHex(2), `2`))
	run := h.createRun(t, "golden", map[string]any{"id": runHex(1)})
	h.seed(t, &model.Trace{ID: traceHex(1), RunID: run.ID, ItemID: itemHex(1)},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})

	rec := h.send(t, "DELETE", "/api/v1/datasets/golden", nil)
	expectStatus(t, rec, http.StatusOK)
	preview := decodeJSON[struct {
		DryRun       bool   `json:"dry_run"`
		Dataset      string `json:"dataset"`
		Items        int64  `json:"items"`
		Runs         int64  `json:"runs"`
		PinnedTraces int64  `json:"pinned_traces"`
		Confirm      string `json:"confirm"`
	}](t, rec)
	if !preview.DryRun || preview.Items != 2 || preview.Runs != 1 || preview.PinnedTraces != 1 || preview.Confirm != "golden" {
		t.Errorf("preview = %+v", preview)
	}
	expectStatus(t, h.get(t, "/api/v1/datasets/golden"), http.StatusOK)

	expectError(t, h.send(t, "DELETE", "/api/v1/datasets/golden?confirm=gold", nil), http.StatusBadRequest, "confirm must be")
	expectStatus(t, h.get(t, "/api/v1/datasets/golden"), http.StatusOK)

	rec = h.send(t, "DELETE", "/api/v1/datasets/golden?confirm=golden", nil)
	expectStatus(t, rec, http.StatusOK)
	deleted := decodeJSON[struct {
		DryRun       bool  `json:"dry_run"`
		Items        int64 `json:"items"`
		Runs         int64 `json:"runs"`
		PinnedTraces int64 `json:"pinned_traces"`
	}](t, rec)
	if deleted.DryRun || deleted.Items != 2 || deleted.Runs != 1 || deleted.PinnedTraces != 1 {
		t.Errorf("deleted = %+v", deleted)
	}
	expectError(t, h.get(t, "/api/v1/datasets/golden"), http.StatusNotFound, "not found")
	expectError(t, h.get(t, "/api/v1/runs/"+run.ID), http.StatusNotFound, "not found")
	expectStatus(t, h.get(t, "/api/v1/traces/"+traceHex(1)), http.StatusOK)
	expectError(t, h.send(t, "DELETE", "/api/v1/datasets/golden", nil), http.StatusNotFound, "not found")
}

// A run's life: created pinned at the current version, re-created unchanged
// by id, closed once, listed newest first, and deleted with the count of
// traces it released.
func TestRunLifecycle(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.postItems(t, "golden", item(itemHex(1), `1`))
	h.postItems(t, "golden", item(itemHex(2), `2`))

	run := h.createRun(t, "golden", map[string]any{
		"id": runHex(1), "name": "prompt v7", "metadata": map[string]any{"prompt": "support-answer@7"},
	})
	if run.Dataset != "golden" || run.DatasetVersion != 2 || run.Status != "running" ||
		*run.Name != "prompt v7" || string(run.Metadata) != `{"prompt":"support-answer@7"}` ||
		run.FinishedAt != nil || run.Error != nil {
		t.Errorf("run = %+v", run)
	}
	rec := h.send(t, "POST", "/api/v1/datasets/golden/runs", map[string]any{"id": runHex(1), "name": "something else"})
	expectStatus(t, rec, http.StatusOK)
	if again := decodeJSON[runResponse](t, rec); *again.Name != "prompt v7" {
		t.Errorf("re-create = %+v, want the existing run unchanged", again)
	}
	expectError(t, h.send(t, "POST", "/api/v1/datasets/other/runs", map[string]any{}), http.StatusNotFound, "not found")
	h.postItems(t, "other", item(itemHex(3), `3`))
	expectError(t, h.send(t, "POST", "/api/v1/datasets/other/runs", map[string]any{"id": runHex(1)}),
		http.StatusConflict, "already exists in dataset")
	expectError(t, h.send(t, "POST", "/api/v1/datasets/golden/runs", map[string]any{"dataset_version": 3}),
		http.StatusBadRequest, "cannot pin version 3")
	expectError(t, h.send(t, "POST", "/api/v1/datasets/golden/runs", map[string]any{"dataset_version": -1}),
		http.StatusBadRequest, "at least 0")
	expectError(t, h.send(t, "POST", "/api/v1/datasets/golden/runs", map[string]any{"id": "run-1"}),
		http.StatusBadRequest, "32 lower-case hex")
	older := h.createRun(t, "golden", map[string]any{"dataset_version": 1})
	if older.DatasetVersion != 1 || !hexID.MatchString(older.ID) {
		t.Errorf("older = %+v, want an explicit pin and a generated id", older)
	}

	h.seed(t, &model.Trace{ID: traceHex(1), RunID: run.ID, ItemID: itemHex(1)},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})

	rec = h.send(t, "POST", "/api/v1/runs/"+run.ID+"/finish", map[string]any{})
	expectStatus(t, rec, http.StatusOK)
	if finished := decodeJSON[runResponse](t, rec); finished.Status != "finished" || finished.FinishedAt == nil {
		t.Errorf("finished = %+v", finished)
	}
	expectError(t, h.send(t, "POST", "/api/v1/runs/"+run.ID+"/finish", map[string]any{}), http.StatusConflict, "already finished")
	rec = h.send(t, "POST", "/api/v1/runs/"+older.ID+"/finish", map[string]any{"status": "failed", "error": "harness crashed"})
	expectStatus(t, rec, http.StatusOK)
	if failed := decodeJSON[runResponse](t, rec); failed.Status != "failed" || *failed.Error != "harness crashed" {
		t.Errorf("failed = %+v", failed)
	}
	expectError(t, h.send(t, "POST", "/api/v1/runs/"+runHex(9)+"/finish", map[string]any{}), http.StatusNotFound, "not found")
	expectError(t, h.send(t, "POST", "/api/v1/runs/"+run.ID+"/finish", map[string]any{"status": "done"}),
		http.StatusBadRequest, `"status" must be`)
	expectError(t, h.send(t, "POST", "/api/v1/runs/"+run.ID+"/finish", map[string]any{"error": "but it finished"}),
		http.StatusBadRequest, `"error" belongs to a run that failed`)

	list := decodeJSON[runListResponse](t, h.get(t, "/api/v1/datasets/golden/runs"))
	if len(list.Runs) != 2 || list.Runs[0].ID != older.ID || list.Runs[1].ID != run.ID {
		t.Errorf("runs = %+v, want newest first", list.Runs)
	}
	expectError(t, h.get(t, "/api/v1/datasets/nope/runs"), http.StatusNotFound, "not found")
	got := decodeJSON[runResponse](t, h.get(t, "/api/v1/runs/"+run.ID))
	if got.Status != "finished" {
		t.Errorf("GET run = %+v", got)
	}

	rec = h.send(t, "DELETE", "/api/v1/runs/"+run.ID, nil)
	expectStatus(t, rec, http.StatusOK)
	if deleted := decodeJSON[runDeletedResponse](t, rec); deleted.ReleasedTraces != 1 || deleted.ID != run.ID {
		t.Errorf("deleted = %+v", deleted)
	}
	expectError(t, h.get(t, "/api/v1/runs/"+run.ID), http.StatusNotFound, "not found")
	expectError(t, h.send(t, "DELETE", "/api/v1/runs/"+run.ID, nil), http.StatusNotFound, "not found")
	expectStatus(t, h.get(t, "/api/v1/traces/"+traceHex(1)), http.StatusOK)
}

// Every new route refuses an unknown query parameter, and every one with a
// body refuses an unknown field (spec 003 #17, #21) — before it looks
// anything up, so a typo is a 400 and never a 404 that hides it.
func TestDatasetRoutesAreStrict(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	fill := strings.NewReplacer(
		"{name}", "golden",
		"{id}", strings.Repeat("a", 32),
		"{a}", strings.Repeat("a", 32),
		"{b}", strings.Repeat("b", 32),
	)
	var covered int
	for _, route := range h.server.routes() {
		if !strings.Contains(route.Path, "/datasets") && !strings.Contains(route.Path, "/runs/") &&
			!strings.Contains(route.Path, "/score-configs") {
			continue
		}
		covered++
		path := fill.Replace(route.Path)
		t.Run(route.Method+" "+route.Path, func(t *testing.T) {
			expectError(t, h.call(t, route.Method, path+"?bogus=1", []byte(`{}`)),
				http.StatusBadRequest, "unknown query parameter")
			if route.Method == "POST" || route.Method == "PUT" {
				expectError(t, h.call(t, route.Method, path, []byte(`{"bogus":1}`)),
					http.StatusBadRequest, `unknown field "bogus"`)
				expectError(t, h.call(t, route.Method, path, []byte(`{"name":`)),
					http.StatusBadRequest, "malformed JSON")
			}
		})
	}
	if covered != 20 {
		t.Errorf("covered %d routes, want the 20 spec 014 adds", covered)
	}
}

// The three listings walk at limit=1 in both directions without a gap or a
// repeat (spec 009 #2).
func TestDatasetListingsWalkBothWays(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	for _, name := range []string{"a-set", "b-set", "c-set"} {
		h.postItems(t, name, item(itemHex(1), `1`), item(itemHex(2), `2`), item(itemHex(3), `3`))
	}
	var runs []string
	for i := 1; i <= 3; i++ {
		runs = append(runs, h.createRun(t, "a-set", map[string]any{"id": runHex(i)}).ID)
	}

	walk := func(t *testing.T, base string, key func([]byte) []string) []string {
		t.Helper()
		forward := walkCursor(t, h, base, false, key)
		backward := walkCursor(t, h, base, true, key)
		if strings.Join(forward, ",") != strings.Join(backward, ",") {
			t.Errorf("forward %v and backward %v disagree", forward, backward)
		}
		return forward
	}
	datasets := walk(t, "/api/v1/datasets?limit=1", func(body []byte) []string {
		var page datasetListResponse
		json.Unmarshal(body, &page)
		var out []string
		for _, d := range page.Datasets {
			out = append(out, d.Name)
		}
		return out
	})
	if strings.Join(datasets, ",") != "a-set,b-set,c-set" {
		t.Errorf("datasets walk = %v", datasets)
	}
	items := walk(t, "/api/v1/datasets/a-set/items?limit=1", func(body []byte) []string {
		var page itemListResponse
		json.Unmarshal(body, &page)
		var out []string
		for _, i := range page.Items {
			out = append(out, i.ID)
		}
		return out
	})
	if strings.Join(items, ",") != strings.Join([]string{itemHex(1), itemHex(2), itemHex(3)}, ",") {
		t.Errorf("items walk = %v", items)
	}
	listed := walk(t, "/api/v1/datasets/a-set/runs?limit=1", func(body []byte) []string {
		var page runListResponse
		json.Unmarshal(body, &page)
		var out []string
		for _, r := range page.Runs {
			out = append(out, r.ID)
		}
		return out
	})
	if strings.Join(listed, ",") != strings.Join([]string{runs[2], runs[1], runs[0]}, ",") {
		t.Errorf("runs walk = %v, want newest first", listed)
	}
}

// walkCursor follows next_cursor from the first page, or prev_cursor from the
// last, and returns the rows in listing order either way.
func walkCursor(t *testing.T, h *harness, base string, backward bool, key func([]byte) []string) []string {
	t.Helper()
	type cursors struct {
		Next *string `json:"next_cursor"`
		Prev *string `json:"prev_cursor"`
	}
	path := base
	if backward {
		path += "&direction=prev"
	}
	var pages [][]string
	for range 10 {
		rec := h.get(t, path)
		expectStatus(t, rec, http.StatusOK)
		var edges cursors
		json.Unmarshal(rec.Body.Bytes(), &edges)
		rows := key(rec.Body.Bytes())
		if len(rows) == 0 {
			break
		}
		pages = append(pages, rows)
		cursor := edges.Next
		if backward {
			cursor = edges.Prev
		}
		if cursor == nil {
			break
		}
		path = base + "&cursor=" + *cursor
		if backward {
			path += "&direction=prev"
		}
	}
	if backward {
		for i, j := 0, len(pages)-1; i < j; i, j = i+1, j-1 {
			pages[i], pages[j] = pages[j], pages[i]
		}
	}
	var out []string
	for _, page := range pages {
		out = append(out, page...)
	}
	return out
}

// The items listing is budget-exempt (spec 014 #19): a 400 KB item comes
// back whole, with no truncation marker, under the default budget that would
// cut it anywhere else.
func TestItemsListingIsBudgetExempt(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	document := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 400*1024/45+1)
	h.postItems(t, "golden", map[string]any{"id": itemHex(1), "input": map[string]any{"document": document}})

	rec := h.get(t, "/api/v1/datasets/golden/items")
	expectStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), `"truncated"`) {
		t.Fatalf("the items listing carries a truncation marker")
	}
	page := decodeJSON[itemListResponse](t, rec)
	var input struct {
		Document string `json:"document"`
	}
	if err := json.Unmarshal(page.Items[0].Input, &input); err != nil {
		t.Fatal(err)
	}
	if len(input.Document) != len(document) {
		t.Errorf("document came back %d bytes, want all %d", len(input.Document), len(document))
	}
	one := decodeJSON[itemResponse](t, h.get(t, "/api/v1/datasets/golden/items/"+itemHex(1)))
	if len(one.Input) < len(document) {
		t.Errorf("the single item was cut to %d bytes", len(one.Input))
	}
}

// Ingest end to end (spec 014 #2, #3): the fixture's traces name a run; with
// no such run they are stored and counted as orphans, and once the run exists
// a re-delivery links them, two attempts of one item side by side.
func TestIngestLinksTracesToTheirRun(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	const fixtureRun = "0e5a7c1d2b3f4a6980c1d2e3f4a5b6c7"
	const itemA = "a1b2c3d4e5f60718293a4b5c6d7e8f90"

	system := func() (pinned, orphans int64) {
		rec := h.get(t, "/api/v1/system")
		expectStatus(t, rec, http.StatusOK)
		body := decodeJSON[struct {
			Runs struct {
				Pinned  int64 `json:"pinned_traces"`
				Orphans int64 `json:"orphan_traces"`
			} `json:"runs"`
		}](t, rec)
		return body.Runs.Pinned, body.Runs.Orphans
	}

	expectStatus(t, h.post(t, "/v1/traces", fixtureBody(t, "009-eval-run")), http.StatusOK)
	row, err := h.store.Trace(h.project.ID, "e0a1b2c3d4e5f60718293a4b5c6d7e8f")
	if err != nil {
		t.Fatal(err)
	}
	if row.RunID != fixtureRun || row.ItemID != itemA {
		t.Errorf("orphan trace = run %q item %q, want the columns written as sent", row.RunID, row.ItemID)
	}
	if pinned, orphans := system(); pinned != 0 || orphans != 3 {
		t.Errorf("system = pinned %d orphans %d, want 0 and the fixture's three traces of an unknown run", pinned, orphans)
	}

	h.postItems(t, "support-golden", item(itemA, `{"q":"reset"}`))
	h.createRun(t, "support-golden", map[string]any{"id": fixtureRun})
	expectStatus(t, h.post(t, "/v1/traces", fixtureBody(t, "009-eval-run")), http.StatusOK)
	if pinned, orphans := system(); pinned != 3 || orphans != 3 {
		t.Errorf("system = pinned %d orphans %d, want the three traces pinned and the counter unmoved", pinned, orphans)
	}
	var attempts int
	for _, id := range []string{"e0a1b2c3d4e5f60718293a4b5c6d7e8f", "e2c3d4e5f60718293a4b5c6d7e8f90a1"} {
		row, err := h.store.Trace(h.project.ID, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.RunID == fixtureRun && row.ItemID == itemA {
			attempts++
		}
	}
	if attempts != 2 {
		t.Errorf("attempts of item A = %d, want both traces linked beside each other", attempts)
	}
	// The trace that stamped names rather than ids has no link at all.
	named, _ := h.store.Trace(h.project.ID, "e3d4e5f60718293a4b5c6d7e8f90a1b2")
	if named.RunID != "" || named.ItemID != "" {
		t.Errorf("a trace with unusable ids got a link: %q/%q", named.RunID, named.ItemID)
	}
}

// TestTraceReadsCarryTheLink: the link is on the trace, so the three ways of
// reading a trace show it — the listing row, the row under `?fields=`, and the
// whole trace — and a trace that carries none says nothing rather than null,
// which is how every other field a trace did not carry renders (spec 014 #2).
func TestTraceReadsCarryTheLink(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	linked, plain := traceHex(1), traceHex(2)
	h.seed(t, &model.Trace{ID: linked, Name: "attempt", Environment: "production",
		RunID: runHex(1), ItemID: itemHex(1)},
		&model.Observation{TraceID: linked, ID: spanHex(1), Type: model.TypeSpan,
			Name: "case", Level: model.LevelDefault,
			StartTime: seedBase, EndTime: seedBase + 100*ms})
	h.seed(t, &model.Trace{ID: plain, Environment: "production"},
		&model.Observation{TraceID: plain, ID: spanHex(2), Type: model.TypeSpan,
			Name: "chat", Level: model.LevelDefault,
			StartTime: seedBase + 1000*ms, EndTime: seedBase + 1100*ms})

	type link struct {
		ID     string `json:"id"`
		RunID  string `json:"run_id"`
		ItemID string `json:"item_id"`
	}
	rec := h.get(t, "/api/v1/traces")
	expectStatus(t, rec, http.StatusOK)
	rows := decodeJSON[struct {
		Traces []link `json:"traces"`
	}](t, rec).Traces
	for _, row := range rows {
		want := link{ID: linked, RunID: runHex(1), ItemID: itemHex(1)}
		if row.ID == plain {
			want = link{ID: plain}
		}
		if row != want {
			t.Errorf("listing row = %+v, want %+v", row, want)
		}
	}

	// `?fields=` selects them by name, and the untouched trace keeps them
	// out of its row entirely rather than answering null.
	rec = h.get(t, "/api/v1/traces?fields=id,run_id,item_id")
	expectStatus(t, rec, http.StatusOK)
	raw := decodeJSON[struct {
		Traces []json.RawMessage `json:"traces"`
	}](t, rec).Traces
	if got, want := string(raw[0]), fmt.Sprintf(`{"id":"%s"}`, plain); got != want {
		t.Errorf("unlinked row = %s, want %s", got, want)
	}
	if got, want := string(raw[1]),
		fmt.Sprintf(`{"id":"%s","run_id":"%s","item_id":"%s"}`, linked, runHex(1), itemHex(1)); got != want {
		t.Errorf("linked row = %s, want %s", got, want)
	}

	// The whole trace, both ways in: by id and through the shortcut, which
	// promises the same shape.
	for _, path := range []string{"/api/v1/traces/" + linked, "/api/v1/traces/last?name=attempt"} {
		rec := h.get(t, path)
		expectStatus(t, rec, http.StatusOK)
		if got := decodeJSON[link](t, rec); got.RunID != runHex(1) || got.ItemID != itemHex(1) {
			t.Errorf("%s = %+v, want the link", path, got)
		}
	}
}

// The system endpoint counts the new tables within the asking project.
func TestSystemCountsEvalTables(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.postItems(t, "golden", item(itemHex(1), `1`), item(itemHex(2), `2`))
	h.createRun(t, "golden", map[string]any{})
	expectStatus(t, h.send(t, "PUT", "/api/v1/score-configs/accuracy",
		map[string]any{"data_type": "numeric", "direction": "higher"}), http.StatusOK)

	rec := h.get(t, "/api/v1/system")
	expectStatus(t, rec, http.StatusOK)
	body := decodeJSON[struct {
		Database struct {
			Rows map[string]int64 `json:"rows"`
		} `json:"database"`
	}](t, rec)
	for table, want := range map[string]int64{"datasets": 1, "dataset_items": 2, "dataset_runs": 1, "score_configs": 1} {
		if got, reported := body.Database.Rows[table]; !reported || got != want {
			t.Errorf("rows[%s] = %d (reported %v), want %d", table, got, reported, want)
		}
	}
}

// The user-data dry run names the runs that will lose traces (spec 014 #14).
func TestUserDataPreviewNamesAffectedRuns(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.postItems(t, "golden", item(itemHex(1), `1`))
	run := h.createRun(t, "golden", map[string]any{})
	h.seed(t, &model.Trace{ID: traceHex(1), UserID: "u1", RunID: run.ID, ItemID: itemHex(1)},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms})

	rec := h.send(t, "DELETE", "/api/v1/projects/"+h.project.ID+"/users/u1/data", nil)
	expectStatus(t, rec, http.StatusOK)
	body := decodeJSON[struct {
		Runs []struct {
			ID      string `json:"id"`
			Dataset string `json:"dataset"`
			Traces  int64  `json:"traces"`
		} `json:"affected_runs"`
	}](t, rec)
	if len(body.Runs) != 1 || body.Runs[0].ID != run.ID || body.Runs[0].Dataset != "golden" || body.Runs[0].Traces != 1 {
		t.Errorf("affected_runs = %+v, want the one run named with its one trace", body.Runs)
	}
	// Not under `runs`: the dataset deletion's dry run spends that key on a
	// count, and one client type reads every preview.
	if strings.Contains(rec.Body.String(), `"runs"`) {
		t.Errorf("the erasure preview took the `runs` key: %s", rec.Body.String())
	}

	expectStatus(t, h.send(t, "DELETE", "/api/v1/projects/"+h.project.ID+"/users/u1/data?confirm=u1", nil), http.StatusOK)
	expectError(t, h.get(t, "/api/v1/traces/"+traceHex(1)), http.StatusNotFound, "not found")
}
