package server

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// The API half of spec 024. The queue is declarative, the two ways in are
// idempotent, `next` is a claim with a clock on it, and `complete` is only
// ever true when the scores the queue asked for are where the queue said.

// putConfigs declares the score configs a queue may reference.
func (h *harness) putConfigs(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		rec := h.send(t, "PUT", "/api/v1/score-configs/"+name,
			map[string]any{"data_type": "numeric", "direction": "higher"})
		expectStatus(t, rec, 200)
	}
}

// putQueue declares a queue over those configs.
func (h *harness) putQueue(t *testing.T, name string, configs ...string) *httptest.ResponseRecorder {
	t.Helper()
	return h.send(t, "PUT", "/api/v1/queues/"+name,
		map[string]any{"description": "what to review", "score_configs": configs})
}

// addTarget adds one target and returns its item id.
func (h *harness) addTarget(t *testing.T, queue string, target map[string]any) string {
	t.Helper()
	rec := h.send(t, "POST", "/api/v1/queues/"+queue+"/items", target)
	expectStatus(t, rec, 201)
	body := decodeJSON[struct {
		IDs []string `json:"ids"`
	}](t, rec)
	if len(body.IDs) != 1 {
		t.Fatalf("ids = %v, want the one item", body.IDs)
	}
	return body.IDs[0]
}

// postScore writes one score on a target, as a harness or a reviewer would.
func (h *harness) postScore(t *testing.T, body map[string]any) {
	t.Helper()
	rec := h.send(t, "POST", "/api/v1/scores", body)
	expectStatus(t, rec, 201)
}

func TestQueuePutIsDeclarative(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.putConfigs(t, "accuracy", "tone")

	created := h.putQueue(t, "review", "accuracy", "tone")
	expectStatus(t, created, 201)
	first := decodeJSON[queueResponse](t, created)
	if len(first.ScoreConfigs) != 2 || first.ScoreConfigs[0] != "accuracy" {
		t.Fatalf("score_configs = %v, want both in the order given", first.ScoreConfigs)
	}
	if first.Counts.Pending != 0 || first.Counts.Completed != 0 {
		t.Errorf("counts = %+v, want an empty queue", first.Counts)
	}

	// The same body again: a 200, and nothing written — `updated_at` is
	// where a re-PUT would show up if it had (#1).
	again := h.putQueue(t, "review", "accuracy", "tone")
	expectStatus(t, again, 200)
	if decodeJSON[queueResponse](t, again).UpdatedAt != first.UpdatedAt {
		t.Errorf("a re-PUT of the same body moved updated_at")
	}

	// A different body replaces the whole queue.
	changed := h.putQueue(t, "review", "tone")
	expectStatus(t, changed, 200)
	if got := decodeJSON[queueResponse](t, changed).ScoreConfigs; len(got) != 1 || got[0] != "tone" {
		t.Errorf("score_configs = %v, want the replacement", got)
	}
}

func TestQueuePutRefusesWhatItCannotAskFor(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.putConfigs(t, "accuracy")

	// A config that does not exist: the queue would ask for a score no
	// name means anything as, so it is a 400 naming the missing one (#1).
	expectError(t, h.putQueue(t, "review", "accuracy", "tone"), 400, "tone")
	// And no queue was created by the refusal.
	expectStatus(t, h.get(t, "/api/v1/queues/review"), 404)

	expectError(t, h.putQueue(t, "review"), 400, "at least one")
	expectError(t, h.send(t, "PUT", "/api/v1/queues/review",
		map[string]any{"score_configs": []string{"accuracy", "accuracy"}}), 400, "twice")
	// The name is one URL path segment under the prompt grammar, so a
	// space is a 400 rather than a name with a space in it.
	expectError(t, h.send(t, "PUT", "/api/v1/queues/not%20a%20name",
		map[string]any{"score_configs": []string{"accuracy"}}), 400, "queue name")
}

func TestAddItemsIsAllOrNothingAndIdempotent(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.putConfigs(t, "accuracy")
	expectStatus(t, h.putQueue(t, "review", "accuracy"), 201)

	// A single object, and a target whose trace has not arrived: an item
	// may be queued for a trace still in flight (#2).
	first := h.addTarget(t, "review", map[string]any{"trace_id": traceHex(1)})

	// An array, one of whose targets is already there.
	rec := h.send(t, "POST", "/api/v1/queues/review/items", []map[string]any{
		{"trace_id": traceHex(1)},
		{"trace_id": traceHex(2)},
		{"trace_id": traceHex(2), "observation_id": spanHex(2)},
		// The same target twice in one batch is one row, counted as
		// existing the second time.
		{"trace_id": traceHex(2)},
	})
	expectStatus(t, rec, 201)
	body := decodeJSON[struct {
		IDs      []string `json:"ids"`
		Added    int      `json:"added"`
		Existing int      `json:"existing"`
	}](t, rec)
	if body.Added != 2 || body.Existing != 2 {
		t.Errorf("added/existing = %d/%d, want 2 added and 2 already there", body.Added, body.Existing)
	}
	if body.IDs[0] != first {
		t.Errorf("ids[0] = %s, want the item the target already had", body.IDs[0])
	}
	if body.IDs[1] != body.IDs[3] {
		t.Errorf("the same target twice in a batch produced two ids")
	}
	// The trace item and the observation item of one trace are different
	// items: they are different verdicts about different things (#2).
	if body.IDs[1] == body.IDs[2] {
		t.Errorf("the observation item and the trace item collapsed into one")
	}

	// All or nothing: one malformed target refuses the batch, and nothing
	// of it lands.
	expectError(t, h.send(t, "POST", "/api/v1/queues/review/items", []map[string]any{
		{"trace_id": traceHex(9)},
		{"trace_id": "not-an-id"},
	}), 400, "trace_id")
	if got := h.queueCounts(t, "review"); got.Pending != 3 {
		t.Errorf("pending = %d, want the refused batch to have added nothing", got.Pending)
	}

	expectStatus(t, h.send(t, "POST", "/api/v1/queues/nowhere/items",
		map[string]any{"trace_id": traceHex(1)}), 404)
}

// queueCounts reads one queue's counts.
func (h *harness) queueCounts(t *testing.T, name string) queueCounts {
	t.Helper()
	rec := h.get(t, "/api/v1/queues/"+name)
	expectStatus(t, rec, 200)
	return decodeJSON[queueResponse](t, rec).Counts
}

func TestAddFromTracesTakesTheListingsFilters(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)
	h.putConfigs(t, "accuracy")
	expectStatus(t, h.putQueue(t, "review", "accuracy"), 201)

	// Every filter family the listing has, against the corpus of three
	// (#4, Testing): the same names, the same meanings.
	for _, tc := range []struct {
		name    string
		query   string
		matched int
	}{
		{"no filter at all", "", 3},
		{"environment", "?environment=production", 2},
		{"tag", "?tag=beta", 1},
		{"status", "?status=error", 1},
		{"a window", fmt.Sprintf("?from=%s", formatTime(seedBase)), 3},
		{"q", "?q=judge", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A queue of its own per case, so the counts are about
			// this filter and not about the one before it.
			queue := strings.NewReplacer(" ", "-").Replace(tc.name)
			expectStatus(t, h.putQueue(t, queue, "accuracy"), 201)
			rec := h.send(t, "POST", "/api/v1/queues/"+queue+"/items/from-traces"+tc.query, nil)
			expectStatus(t, rec, 201)
			body := decodeJSON[fromTracesResponse](t, rec)
			if body.Matched != tc.matched || body.Added != tc.matched {
				t.Fatalf("matched/added = %d/%d, want %d of each",
					body.Matched, body.Added, tc.matched)
			}
			if body.Capped {
				t.Errorf("capped on a filter that fits under the limit")
			}
		})
	}

	// Re-running the same filter adds nothing: that is what makes a filter
	// safe to run twice (#2).
	rec := h.send(t, "POST", "/api/v1/queues/review/items/from-traces?environment=production", nil)
	expectStatus(t, rec, 201)
	again := h.send(t, "POST", "/api/v1/queues/review/items/from-traces?environment=production", nil)
	expectStatus(t, again, 201)
	repeat := decodeJSON[fromTracesResponse](t, again)
	if repeat.Added != 0 || repeat.Existing != 2 {
		t.Errorf("second run = %+v, want nothing added and both already there", repeat)
	}

	// The cap: `limit` bounds what is taken, `matched` says how many there
	// were, and `capped` says the difference is real.
	expectStatus(t, h.putQueue(t, "capped", "accuracy"), 201)
	rec = h.send(t, "POST", "/api/v1/queues/capped/items/from-traces?limit=1", nil)
	expectStatus(t, rec, 201)
	bounded := decodeJSON[fromTracesResponse](t, rec)
	if bounded.Added != 1 || bounded.Matched != 3 || !bounded.Capped {
		t.Errorf("capped run = %+v, want one of three added and capped", bounded)
	}
	// The newest first, so a second call with `to=` continues downwards.
	items := h.items(t, "capped", "")
	if len(items) != 1 || items[0].TraceID != traceHex(3) {
		t.Fatalf("items = %+v, want the newest trace", items)
	}

	// An unknown filter is a 400, exactly as it is on the listing.
	expectError(t, h.send(t, "POST", "/api/v1/queues/review/items/from-traces?nope=1", nil),
		400, "unknown query parameter")
	expectError(t, h.send(t, "POST", "/api/v1/queues/review/items/from-traces?limit=0", nil),
		400, "limit")
	expectError(t, h.send(t, "POST", "/api/v1/queues/review/items/from-traces?status=maybe", nil),
		400, "status")
}

// items reads a queue's items through the endpoint.
func (h *harness) items(t *testing.T, queue, query string) []queueItemResponse {
	t.Helper()
	rec := h.get(t, "/api/v1/queues/"+queue+"/items"+query)
	expectStatus(t, rec, 200)
	return decodeJSON[queueItemListResponse](t, rec).Items
}

func TestNextClaimsAndResumes(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.putConfigs(t, "accuracy")
	expectStatus(t, h.putQueue(t, "review", "accuracy"), 201)
	h.addTarget(t, "review", map[string]any{"trace_id": traceHex(1)})
	h.addTarget(t, "review", map[string]any{"trace_id": traceHex(2)})

	first := h.next(t, "review", "ada")
	if first.Item == nil || first.Item.Seq != 1 {
		t.Fatalf("next = %+v, want the oldest pending item", first.Item)
	}
	if first.Item.ClaimedBy != "ada" || first.Item.ClaimedUntil == "" {
		t.Errorf("item = %+v, want it claimed with a deadline", first.Item)
	}
	if first.Pending != 2 {
		t.Errorf("pending = %d, want both still to do", first.Pending)
	}

	// Asking again hands back the same one: a reload must not move a
	// reviewer to a different trace mid-verdict (#5).
	resumed := h.next(t, "review", "ada")
	if resumed.Item == nil || resumed.Item.ID != first.Item.ID {
		t.Fatalf("resume = %+v, want the same item back", resumed.Item)
	}

	// Somebody else skips the live claim and takes the next one.
	second := h.next(t, "review", "bob")
	if second.Item == nil || second.Item.ID == first.Item.ID {
		t.Fatalf("next for bob = %+v, want the item ada is not holding", second.Item)
	}

	// Nothing claimable, and `pending` counts what the other two hold.
	third := h.next(t, "review", "cleo")
	if third.Item != nil {
		t.Fatalf("next for cleo = %+v, want nothing claimable", third.Item)
	}
	if third.Pending != 2 {
		t.Errorf("pending = %d, want the two items other people are holding", third.Pending)
	}
	// What a claim does when it runs out is the store's clock rather than
	// this endpoint's, and it is tested where that clock can be moved
	// (`TestNextTakesAClaimThatRanOut`).

	expectError(t, h.get(t, "/api/v1/queues/review/next"), 400, "annotator")
	expectStatus(t, h.get(t, "/api/v1/queues/nowhere/next?annotator=ada"), 404)
}

func (h *harness) next(t *testing.T, queue, annotator string) nextResponse {
	t.Helper()
	rec := h.get(t, "/api/v1/queues/"+queue+"/next?annotator="+annotator)
	expectStatus(t, rec, 200)
	return decodeJSON[nextResponse](t, rec)
}

func TestCompleteChecksTheScoresAreThere(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.putConfigs(t, "accuracy", "tone")
	expectStatus(t, h.putQueue(t, "review", "accuracy", "tone"), 201)
	item := h.addTarget(t, "review", map[string]any{"trace_id": traceHex(1)})

	// Nothing scored yet: both names are missing, and the refusal says so
	// in a form the desk can mark controls with (#7).
	rec := h.send(t, "POST", "/api/v1/queues/review/items/"+item+"/complete",
		map[string]any{"annotator": "ada"})
	expectStatus(t, rec, 409)
	missing := decodeJSON[struct {
		Missing []string `json:"missing"`
	}](t, rec).Missing
	if len(missing) != 2 || missing[0] != "accuracy" {
		t.Fatalf("missing = %v, want both names in the queue's order", missing)
	}

	// A score on an *observation* of the trace is a different verdict and
	// does not answer for the trace item (Testing).
	h.postScore(t, map[string]any{"trace_id": traceHex(1), "observation_id": spanHex(1),
		"name": "accuracy", "value": 1})
	rec = h.send(t, "POST", "/api/v1/queues/review/items/"+item+"/complete",
		map[string]any{"annotator": "ada"})
	expectStatus(t, rec, 409)
	if got := decodeJSON[struct {
		Missing []string `json:"missing"`
	}](t, rec).Missing; len(got) != 2 {
		t.Errorf("missing = %v, want an observation score not to count for the trace", got)
	}

	h.postScore(t, map[string]any{"trace_id": traceHex(1), "name": "accuracy", "value": 1})
	h.postScore(t, map[string]any{"trace_id": traceHex(1), "name": "tone", "value": 0.5})
	rec = h.send(t, "POST", "/api/v1/queues/review/items/"+item+"/complete",
		map[string]any{"annotator": "ada"})
	expectStatus(t, rec, 200)
	completed := decodeJSON[queueItemResponse](t, rec)
	if completed.Status != "completed" || completed.CompletedBy != "ada" {
		t.Fatalf("item = %+v, want it completed by ada", completed)
	}
	if completed.ClaimedBy != "" {
		t.Errorf("the claim outlived the completion")
	}

	// The second completion is refused rather than absorbed, so the
	// reviewer who lost the race sees that they did.
	rec = h.send(t, "POST", "/api/v1/queues/review/items/"+item+"/complete",
		map[string]any{"annotator": "bob"})
	expectError(t, rec, 409, "ada")

	expectError(t, h.send(t, "POST", "/api/v1/queues/review/items/"+item+"/complete",
		map[string]any{}), 400, "annotator")
}

// TestCompleteOfAnObservationItemLooksAtItsOwnScores is the mirror of the case
// above: a trace-level score of the right name does not answer for the
// observation the item points at.
func TestCompleteOfAnObservationItemLooksAtItsOwnScores(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.putConfigs(t, "accuracy")
	expectStatus(t, h.putQueue(t, "review", "accuracy"), 201)
	item := h.addTarget(t, "review",
		map[string]any{"trace_id": traceHex(1), "observation_id": spanHex(1)})

	h.postScore(t, map[string]any{"trace_id": traceHex(1), "name": "accuracy", "value": 1})
	expectStatus(t, h.send(t, "POST", "/api/v1/queues/review/items/"+item+"/complete",
		map[string]any{"annotator": "ada"}), 409)

	h.postScore(t, map[string]any{"trace_id": traceHex(1), "observation_id": spanHex(1),
		"name": "accuracy", "value": 1})
	expectStatus(t, h.send(t, "POST", "/api/v1/queues/review/items/"+item+"/complete",
		map[string]any{"annotator": "ada"}), 200)
}

func TestSkipReopenAndRemove(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.putConfigs(t, "accuracy")
	expectStatus(t, h.putQueue(t, "review", "accuracy"), 201)
	item := h.addTarget(t, "review", map[string]any{"trace_id": traceHex(1)})

	rec := h.send(t, "POST", "/api/v1/queues/review/items/"+item+"/skip",
		map[string]any{"annotator": "ada", "reason": "no answer in it"})
	expectStatus(t, rec, 200)
	skipped := decodeJSON[queueItemResponse](t, rec)
	if skipped.Status != "skipped" || skipped.SkipReason != "no answer in it" {
		t.Fatalf("item = %+v, want it skipped with the reason", skipped)
	}
	if counts := h.queueCounts(t, "review"); counts.Skipped != 1 || counts.Pending != 0 {
		t.Errorf("counts = %+v, want the skip counted", counts)
	}

	rec = h.send(t, "POST", "/api/v1/queues/review/items/"+item+"/reopen",
		map[string]any{"annotator": "bob"})
	expectStatus(t, rec, 200)
	reopened := decodeJSON[queueItemResponse](t, rec)
	if reopened.Status != "pending" || reopened.SkipReason != "" || reopened.CompletedBy != "" {
		t.Fatalf("item = %+v, want it pending again with the skip cleared", reopened)
	}
	// Reopening what is already pending is a 409, not a quiet no-op.
	expectError(t, h.send(t, "POST", "/api/v1/queues/review/items/"+item+"/reopen",
		map[string]any{"annotator": "bob"}), 409, "already pending")

	expectStatus(t, h.send(t, "DELETE", "/api/v1/queues/review/items/"+item, nil), 200)
	expectStatus(t, h.send(t, "DELETE", "/api/v1/queues/review/items/"+item, nil), 404)
	if counts := h.queueCounts(t, "review"); counts.Pending != 0 {
		t.Errorf("counts = %+v, want the removed item gone", counts)
	}
}

func TestQueueItemsListingPagesBothWays(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.putConfigs(t, "accuracy")
	expectStatus(t, h.putQueue(t, "review", "accuracy"), 201)
	var ids []string
	for i := 1; i <= 3; i++ {
		ids = append(ids, h.addTarget(t, "review", map[string]any{"trace_id": traceHex(i)}))
	}
	expectStatus(t, h.send(t, "POST", "/api/v1/queues/review/items/"+ids[1]+"/skip",
		map[string]any{"annotator": "ada", "reason": "nothing to judge"}), 200)

	// Oldest first: the order they were added is the order they are worked
	// in (#8).
	if got := h.items(t, "review", ""); len(got) != 3 || got[0].ID != ids[0] || got[2].ID != ids[2] {
		t.Fatalf("items = %+v, want all three in seq order", got)
	}
	if got := h.items(t, "review", "?status=pending"); len(got) != 2 {
		t.Errorf("pending items = %d, want the two that are", len(got))
	}
	if got := h.items(t, "review", "?annotator=ada"); len(got) != 1 || got[0].ID != ids[1] {
		t.Errorf("ada's items = %+v, want the one she skipped", got)
	}
	expectError(t, h.get(t, "/api/v1/queues/review/items?status=maybe"), 400, "status")

	// One row at a time, forwards to the end and backwards to the start.
	page := h.page(t, "review", "?limit=1")
	seen := []string{page.Items[0].ID}
	for page.NextCursor != nil {
		page = h.page(t, "review", "?limit=1&cursor="+*page.NextCursor)
		seen = append(seen, page.Items[0].ID)
	}
	if strings.Join(seen, ",") != strings.Join(ids, ",") {
		t.Fatalf("walked %v, want %v", seen, ids)
	}
	var back []string
	for page.PrevCursor != nil {
		page = h.page(t, "review", "?limit=1&direction=prev&cursor="+*page.PrevCursor)
		back = append(back, page.Items[0].ID)
	}
	if strings.Join(back, ",") != ids[1]+","+ids[0] {
		t.Fatalf("walked back %v, want the two above the last", back)
	}

	counted := h.page(t, "review", "?count=1&status=pending")
	if counted.Total == nil || *counted.Total != 2 {
		t.Errorf("total = %v, want the two pending", counted.Total)
	}
	expectStatus(t, h.get(t, "/api/v1/queues/nowhere/items"), 404)
}

func (h *harness) page(t *testing.T, queue, query string) queueItemListResponse {
	t.Helper()
	rec := h.get(t, "/api/v1/queues/"+queue+"/items"+query)
	expectStatus(t, rec, 200)
	return decodeJSON[queueItemListResponse](t, rec)
}

func TestDeleteQueueIsADryRunUntilItIsEchoed(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seed(t, &model.Trace{ID: traceHex(1), Environment: "production"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + 100*ms})
	h.putConfigs(t, "accuracy")
	expectStatus(t, h.putQueue(t, "review", "accuracy"), 201)
	h.addTarget(t, "review", map[string]any{"trace_id": traceHex(1)})
	h.postScore(t, map[string]any{"trace_id": traceHex(1), "name": "accuracy", "value": 1})

	rec := h.send(t, "DELETE", "/api/v1/queues/review", nil)
	expectStatus(t, rec, 200)
	dry := decodeJSON[struct {
		DryRun      bool `json:"dry_run"`
		WouldDelete struct {
			Items int64 `json:"items"`
		} `json:"would_delete"`
		Confirm string `json:"confirm"`
		Note    string `json:"note"`
	}](t, rec)
	if !dry.DryRun || dry.WouldDelete.Items != 1 || dry.Confirm != "review" {
		t.Fatalf("dry run = %+v, want the one item and the echo", dry)
	}
	if !strings.Contains(dry.Note, "scores") {
		t.Errorf("note = %q, want it to say the scores stay", dry.Note)
	}
	// Still there: a dry run changes nothing.
	expectStatus(t, h.get(t, "/api/v1/queues/review"), 200)

	expectError(t, h.send(t, "DELETE", "/api/v1/queues/review?confirm=reviews", nil), 400, "review")
	expectStatus(t, h.get(t, "/api/v1/queues/review"), 200)

	expectStatus(t, h.send(t, "DELETE", "/api/v1/queues/review?confirm=review", nil), 200)
	expectStatus(t, h.get(t, "/api/v1/queues/review"), 404)
	// The verdict written while annotating is the product of the work and
	// is attached to the trace (#3).
	scores := decodeJSON[struct {
		Scores []struct {
			Name string `json:"name"`
		} `json:"scores"`
	}](t, h.get(t, "/api/v1/scores?trace_id="+traceHex(1)))
	if len(scores.Scores) != 1 {
		t.Errorf("scores = %+v, want the verdict to survive the queue", scores.Scores)
	}
}

func TestQueueListingAndSystemCounts(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.putConfigs(t, "accuracy")
	expectStatus(t, h.putQueue(t, "zeta", "accuracy"), 201)
	expectStatus(t, h.putQueue(t, "alpha", "accuracy"), 201)
	h.addTarget(t, "alpha", map[string]any{"trace_id": traceHex(1)})

	rec := h.get(t, "/api/v1/queues")
	expectStatus(t, rec, 200)
	listing := decodeJSON[queueListResponse](t, rec)
	if len(listing.Queues) != 2 || listing.Queues[0].Name != "alpha" {
		t.Fatalf("queues = %+v, want both in name order", listing.Queues)
	}
	if listing.Queues[0].Counts.Pending != 1 || listing.Queues[1].Counts.Pending != 0 {
		t.Errorf("counts = %+v, want the item counted against its own queue", listing.Queues)
	}

	// Both tables are the operator's to watch (data contract).
	rows := decodeJSON[struct {
		Database struct {
			Rows map[string]int64 `json:"rows"`
		} `json:"database"`
	}](t, h.get(t, "/api/v1/system")).Database.Rows
	if rows["annotation_queues"] != 2 || rows["annotation_items"] != 1 {
		t.Errorf("rows = %v, want both annotation tables counted", rows)
	}
}
