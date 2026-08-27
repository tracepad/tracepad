package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// The read API's handler tests (spec 004, Testing #1). Rows are seeded through
// the same write job ingest submits, so what is read back is what an export
// would have produced.

// seedBase is a fixed instant (2026-09-01T00:00:00Z) in Unix nanoseconds, so
// that what a test asserts never depends on when it ran.
const seedBase int64 = 1788220800_000_000_000

const ms = int64(1_000_000)

func (h *harness) seed(t *testing.T, trace *model.Trace, observations ...*model.Observation) {
	t.Helper()
	err := h.writer.Submit(t.Context(), &store.IngestBatch{
		ProjectID:    h.project.ID,
		Traces:       []*model.Trace{trace},
		Observations: observations,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func traceHex(n int) string { return fmt.Sprintf("%032x", n) }
func spanHex(n int) string  { return fmt.Sprintf("%016x", n) }

// seedCorpus writes three traces a listing can be asked questions about.
func seedCorpus(t *testing.T, h *harness) {
	t.Helper()
	h.seed(t, &model.Trace{ID: traceHex(1), Name: "chat", UserID: "u1", SessionID: "s1",
		Environment: "production", Tags: []string{"beta", "support"}},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Name: "handle", Level: model.LevelDefault,
			StartTime: seedBase, EndTime: seedBase + 100*ms})
	h.seed(t, &model.Trace{ID: traceHex(2), Name: "eval", UserID: "u2", SessionID: "s1",
		Environment: "production"},
		&model.Observation{TraceID: traceHex(2), ID: spanHex(2), Type: model.TypeGeneration,
			Name: "judge", Level: model.LevelError, StatusMessage: "rate limited",
			Model: "claude-sonnet-5", StartTime: seedBase + 1000*ms, EndTime: seedBase + 1300*ms,
			CostDetails: map[string]any{"total": 0.25}})
	h.seed(t, &model.Trace{ID: traceHex(3), Name: "chat", Environment: "staging"},
		&model.Observation{TraceID: traceHex(3), ID: spanHex(3), Type: model.TypeSpan,
			Name: "handle", Level: model.LevelDefault,
			StartTime: seedBase + 2000*ms, EndTime: seedBase + 2100*ms})
}

// listedIDs reads the ids out of a listing response.
func listedIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	expectStatus(t, rec, 200)
	body := decodeJSON[struct {
		Traces []struct {
			ID string `json:"id"`
		} `json:"traces"`
	}](t, rec)
	ids := make([]string, 0, len(body.Traces))
	for _, row := range body.Traces {
		ids = append(ids, row.ID)
	}
	return ids
}

func TestListTraces(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)

	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"newest first", "", []string{traceHex(3), traceHex(2), traceHex(1)}},
		{"environment", "?environment=staging", []string{traceHex(3)}},
		{"user", "?user_id=u1", []string{traceHex(1)}},
		{"session", "?session_id=s1", []string{traceHex(2), traceHex(1)}},
		{"name", "?name=chat", []string{traceHex(3), traceHex(1)}},
		{"tags are ANDed", "?tag=beta&tag=support", []string{traceHex(1)}},
		{"a tag nothing carries", "?tag=beta&tag=absent", nil},
		{"status", "?status=error", []string{traceHex(2)}},
		{"min cost", "?min_cost=0.1", []string{traceHex(2)}},
		{"half-open range", "?from=2026-09-01T00:00:01Z&to=2026-09-01T00:00:02Z", []string{traceHex(2)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids := listedIDs(t, h.get(t, "/api/v1/traces"+tc.query))
			if strings.Join(ids, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("ids = %v, want %v", ids, tc.want)
			}
		})
	}
}

// TestListTracesRefusesWhatItCannotMean is the spec 003 #21/#23 lineage: a
// filter the server does not know, or a value it cannot honour, is an error
// rather than a silently wider listing.
func TestListTracesRefusesWhatItCannotMean(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	for _, tc := range []struct{ name, query, fragment string }{
		{"unknown parameter", "?trace=abc", "unknown query parameter"},
		{"empty value", "?environment=", "without a value"},
		{"unknown status", "?status=failed", "status must be"},
		{"negative cost", "?min_cost=-1", "min_cost must be"},
		{"unparseable cost", "?min_cost=cheap", "min_cost must be"},
		{"limit out of range", "?limit=5000", "limit must be"},
		{"unknown field", "?fields=id,nope", `unknown field "nope"`},
		{"malformed time", "?from=yesterday", "RFC 3339"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expectError(t, h.get(t, "/api/v1/traces"+tc.query), 400, tc.fragment)
		})
	}
}

// TestListTracesFields checks `?fields=`: the row keeps the API's own order,
// carries only what was asked for, and a field the trace never had stays
// absent rather than being invented as empty.
func TestListTracesFields(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)

	rec := h.get(t, "/api/v1/traces?fields=error_count,id,user_id&limit=1")
	expectStatus(t, rec, 200)
	var body struct {
		Traces []json.RawMessage `json:"traces"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// Trace 3 has no user id, so the field is absent; the two it has come
	// back in the row's declared order, not the order they were asked for.
	want := fmt.Sprintf(`{"id":"%s","error_count":0}`, traceHex(3))
	if string(body.Traces[0]) != want {
		t.Fatalf("row = %s, want %s", body.Traces[0], want)
	}
}

// TestListTracesCursorWalk pages one row at a time and asserts every trace is
// seen exactly once, which is the property keyset pagination exists for.
func TestListTracesCursorWalk(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)

	var (
		seen []string
		path = "/api/v1/traces?limit=1"
	)
	for range 5 {
		rec := h.get(t, path)
		expectStatus(t, rec, 200)
		body := decodeJSON[struct {
			Traces []struct {
				ID string `json:"id"`
			} `json:"traces"`
			NextCursor *string `json:"next_cursor"`
		}](t, rec)
		for _, row := range body.Traces {
			seen = append(seen, row.ID)
		}
		if body.NextCursor == nil {
			break
		}
		path = "/api/v1/traces?limit=1&cursor=" + *body.NextCursor
	}
	if strings.Join(seen, ",") != strings.Join([]string{traceHex(3), traceHex(2), traceHex(1)}, ",") {
		t.Fatalf("walk = %v, want every trace once, newest first", seen)
	}
}

// TestGetTraceTree checks the nesting, the sibling order, and that a span
// whose parent has not arrived renders at the root with its parent id intact
// (edge cases).
func TestGetTraceTree(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seed(t, &model.Trace{ID: traceHex(1), Name: "chat", Metadata: map[string]any{"channel": "web"}},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Name: "root", Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + 500*ms},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(2), ParentObservationID: spanHex(1),
			Type: model.TypeGeneration, Name: "second-child", Level: model.LevelDefault,
			StartTime: seedBase + 200*ms, EndTime: seedBase + 300*ms},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(3), ParentObservationID: spanHex(1),
			Type: model.TypeEvent, Name: "first-child", Level: model.LevelDefault,
			StartTime: seedBase + 100*ms, EndTime: seedBase + 100*ms},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(4), ParentObservationID: spanHex(9),
			Type: model.TypeSpan, Name: "orphan", Level: model.LevelDefault,
			StartTime: seedBase + 400*ms, EndTime: seedBase + 450*ms})

	rec := h.get(t, "/api/v1/traces/"+traceHex(1))
	expectStatus(t, rec, 200)
	body := decodeJSON[struct {
		Metadata     map[string]any `json:"metadata"`
		Observations []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Parent   string `json:"parent_observation_id"`
			Input    any    `json:"input"`
			Children []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"children"`
		} `json:"observations"`
	}](t, rec)

	if body.Metadata["channel"] != "web" {
		t.Errorf("metadata = %v, want the trace's own metadata inlined", body.Metadata)
	}
	if len(body.Observations) != 2 {
		t.Fatalf("roots = %d (%+v), want the root and the orphan", len(body.Observations), body.Observations)
	}
	root, orphan := body.Observations[0], body.Observations[1]
	if root.Name != "root" || orphan.Name != "orphan" {
		t.Fatalf("roots = %q, %q, want root then orphan by start time", root.Name, orphan.Name)
	}
	if orphan.Parent != spanHex(9) {
		t.Errorf("orphan parent = %q, want the id it named even though it is absent", orphan.Parent)
	}
	if len(root.Children) != 2 ||
		root.Children[0].Name != "first-child" || root.Children[1].Name != "second-child" {
		t.Fatalf("children = %+v, want both, ordered by start time", root.Children)
	}
	if root.Input != nil {
		t.Errorf("input = %v, want no payloads without ?expand=io", root.Input)
	}
}

func TestGetTraceRefusals(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)

	expectError(t, h.get(t, "/api/v1/traces/"+traceHex(9)), 404, "not found")
	expectError(t, h.get(t, "/api/v1/traces/not-a-trace-id"), 400, "32 lower-case hex")
	expectError(t, h.get(t, "/api/v1/traces/"+traceHex(1)+"?expand=all"), 400, "expand must be io")
	expectError(t, h.get(t, "/api/v1/traces/"+traceHex(1)+"?budget=10"), 400, "budget must be")
	expectError(t, h.get(t, "/api/v1/traces/"+traceHex(1)+"?budget=99999999"), 400, "budget must be")
	// A budget without an expansion binds nothing, which is not the same
	// as meaning nothing: it is accepted.
	expectStatus(t, h.get(t, "/api/v1/traces/"+traceHex(1)+"?budget=8192"), 200)
}

// TestExpandIOBudget is the adversarial case of #2: payloads far larger than
// any budget must come back as markers, the response must stay inside the
// budget, and each marker must name a follow-up that actually works.
func TestExpandIOBudget(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	huge := strings.Repeat("payload ", 400_000) // ~3 MiB
	h.seed(t, &model.Trace{ID: traceHex(1)},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeGeneration,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms,
			Input: huge, Output: huge, Metadata: map[string]any{"note": huge}},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(2), ParentObservationID: spanHex(1),
			Type: model.TypeSpan, Level: model.LevelDefault,
			StartTime: seedBase + ms, EndTime: seedBase + 2*ms,
			Input: map[string]any{"question": "how do I reset my password?"}})

	rec := h.get(t, "/api/v1/traces/"+traceHex(1)+"?expand=io")
	expectStatus(t, rec, 200)
	if rec.Body.Len() > config.DefaultResponseBudgetBytes {
		t.Fatalf("response is %d bytes, want it inside the %d-byte budget",
			rec.Body.Len(), config.DefaultResponseBudgetBytes)
	}

	body := decodeJSON[struct {
		Observations []struct {
			Input    json.RawMessage `json:"input"`
			Children []struct {
				Input map[string]any `json:"input"`
			} `json:"children"`
		} `json:"observations"`
	}](t, rec)

	var marker truncation
	if err := json.Unmarshal(body.Observations[0].Input, &marker); err != nil {
		t.Fatalf("oversized input is not a marker: %v", err)
	}
	if !marker.Truncated || marker.Size < len(huge) {
		t.Fatalf("marker = %+v, want it to report the real size", marker)
	}
	if marker.TraceID != traceHex(1) || marker.ObservationID != spanHex(1) {
		t.Errorf("marker = %+v, want the id pair get_observation_io takes", marker)
	}
	if marker.Preview == "" || !strings.Contains(marker.Preview, "payload") {
		t.Errorf("preview = %q, want a prefix of the payload", marker.Preview)
	}
	// The small payload fits and is inlined whole, which is the other half
	// of "an equal share of the remaining budget".
	if body.Observations[0].Children[0].Input["question"] != "how do I reset my password?" {
		t.Errorf("small input = %v, want it inlined whole", body.Observations[0].Children[0].Input)
	}

	// The marker's own URL is the follow-up, and it works.
	full := h.get(t, marker.Full)
	expectStatus(t, full, 200)
	whole := decodeJSON[struct {
		Input string `json:"input"`
	}](t, full)
	if whole.Input != huge {
		t.Errorf("the full payload came back cut (%d of %d bytes)", len(whole.Input), len(huge))
	}
}

// TestExpandIOInlinesWhatFits keeps the budget from being an excuse to
// truncate everything: payloads that fit come back untouched.
func TestExpandIOInlinesWhatFits(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seed(t, &model.Trace{ID: traceHex(1)},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeGeneration,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms,
			Input:  []any{map[string]any{"role": "user", "content": "hello"}},
			Output: map[string]any{"role": "assistant", "content": "hi"},
			// Metadata rides with the other two payloads (Decision 24).
			Metadata: map[string]any{"prompt_name": "support-v3"}})

	rec := h.get(t, "/api/v1/traces/"+traceHex(1)+"?expand=io")
	expectStatus(t, rec, 200)
	body := decodeJSON[struct {
		Observations []struct {
			Input    []map[string]any `json:"input"`
			Output   map[string]any   `json:"output"`
			Metadata map[string]any   `json:"metadata"`
		} `json:"observations"`
	}](t, rec)
	got := body.Observations[0]
	if len(got.Input) != 1 || got.Input[0]["content"] != "hello" {
		t.Errorf("input = %v, want it inlined whole", got.Input)
	}
	if got.Output["content"] != "hi" {
		t.Errorf("output = %v, want it inlined whole", got.Output)
	}
	if got.Metadata["prompt_name"] != "support-v3" {
		t.Errorf("metadata = %v, want it inlined with the other payloads", got.Metadata)
	}
}

func TestLastTrace(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)

	rec := h.get(t, "/api/v1/traces/last?status=error")
	expectStatus(t, rec, 200)
	body := decodeJSON[struct {
		ID           string `json:"id"`
		Observations []struct {
			StatusMessage string `json:"status_message"`
		} `json:"observations"`
	}](t, rec)
	if body.ID != traceHex(2) {
		t.Fatalf("id = %s, want the one failed trace", body.ID)
	}
	// The shortcut returns the same shape as GET /traces/{id}: the tree,
	// not a list row.
	if len(body.Observations) != 1 || body.Observations[0].StatusMessage != "rate limited" {
		t.Fatalf("observations = %+v, want the whole trace", body.Observations)
	}

	// Nothing matches: the message names the filters, so the caller can
	// see which query found nothing.
	missing := h.get(t, "/api/v1/traces/last?status=error&environment=staging")
	expectError(t, missing, 404, "environment=staging")
	expectError(t, missing, 404, "status=error")
}

func TestObservationIO(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	// The same span id in two traces: unique only inside its trace.
	for _, n := range []int{1, 2} {
		h.seed(t, &model.Trace{ID: traceHex(n)},
			&model.Observation{TraceID: traceHex(n), ID: spanHex(7), Type: model.TypeGeneration,
				Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms,
				Input: fmt.Sprintf("input of trace %d", n)})
	}
	h.seed(t, &model.Trace{ID: traceHex(3)},
		&model.Observation{TraceID: traceHex(3), ID: spanHex(8), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + ms,
			Output: "only one trace has this span"})

	// Unambiguous: no trace_id needed.
	rec := h.get(t, "/api/v1/observations/"+spanHex(8)+"/io")
	expectStatus(t, rec, 200)
	if decodeJSON[map[string]any](t, rec)["output"] != "only one trace has this span" {
		t.Errorf("body = %s, want the payload whole", rec.Body)
	}

	// Ambiguous: 409 naming the candidates, never a guess.
	conflict := h.get(t, "/api/v1/observations/"+spanHex(7)+"/io")
	expectError(t, conflict, 409, traceHex(1))
	expectError(t, conflict, 409, traceHex(2))

	// With the trace named, the ambiguity is gone.
	resolved := h.get(t, "/api/v1/observations/"+spanHex(7)+"/io?trace_id="+traceHex(2))
	expectStatus(t, resolved, 200)
	if decodeJSON[map[string]any](t, resolved)["input"] != "input of trace 2" {
		t.Errorf("body = %s, want the payload of the trace that was named", resolved.Body)
	}

	expectError(t, h.get(t, "/api/v1/observations/"+spanHex(99)+"/io"), 404, "not found")
	expectError(t, h.get(t, "/api/v1/observations/nothex/io"), 400, "16 lower-case hex")
	expectError(t, h.get(t, "/api/v1/observations/"+spanHex(8)+"/io?trace_id=short"), 400, "32 lower-case hex")
}

// TestEmptyProjectReads is the empty-project edge case: empty arrays, an
// honest 404, and no fabricated rows.
func TestEmptyProjectReads(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.get(t, "/api/v1/traces")
	expectStatus(t, rec, 200)
	if body := strings.TrimSpace(rec.Body.String()); body != `{"traces":[],"next_cursor":null}` {
		t.Errorf("body = %s, want an empty page rather than null", body)
	}
	expectError(t, h.get(t, "/api/v1/traces/last"), 404, "empty project")
	expectError(t, h.get(t, "/api/v1/sessions/nobody"), 404, "not found")
}
