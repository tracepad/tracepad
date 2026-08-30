package server

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// `q` on the trace listing (spec 011, API contract): one more filter, one more
// row field, and the same ordering and paging as without it.

// seedSearchable writes traces whose text is worth searching for.
func seedSearchable(t *testing.T, h *harness) {
	t.Helper()
	h.seed(t, &model.Trace{ID: traceHex(1), Name: "support-chat", Environment: "production"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeGeneration,
			Name: "answer", Level: model.LevelDefault,
			StartTime: seedBase, EndTime: seedBase + 100*ms,
			Output: "the refund failed for the order because the card issuer declined the charge"})
	h.seed(t, &model.Trace{ID: traceHex(2), Name: "eval", Environment: "staging"},
		&model.Observation{TraceID: traceHex(2), ID: spanHex(2), Type: model.TypeSpan,
			Name: "judge", Level: model.LevelError, StatusMessage: "upstream timed out",
			StartTime: seedBase + 1000*ms, EndTime: seedBase + 1300*ms})
}

type matchBody struct {
	ObservationID *string `json:"observation_id"`
	Field         string  `json:"field"`
	Snippet       string  `json:"snippet"`
}

func searchRows(t *testing.T, rec *httptest.ResponseRecorder) []struct {
	ID    string     `json:"id"`
	Name  *string    `json:"name"`
	Match *matchBody `json:"match"`
} {
	t.Helper()
	expectStatus(t, rec, 200)
	return decodeJSON[struct {
		Traces []struct {
			ID    string     `json:"id"`
			Name  *string    `json:"name"`
			Match *matchBody `json:"match"`
		} `json:"traces"`
	}](t, rec).Traces
}

func TestSearchFiltersTheListing(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedSearchable(t, h)

	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"a word in a completion", "?q=refund", []string{traceHex(1)}},
		{"a word in a status message", "?q=upstream", []string{traceHex(2)}},
		{"a word in an observation name", "?q=judge", []string{traceHex(2)}},
		{"the trace's own name", "?q=support-chat", []string{traceHex(1)}},
		{"a phrase", `?q=%22refund+failed%22`, []string{traceHex(1)}},
		{"a phrase in the wrong order", `?q=%22failed+refund%22`, nil},
		{"a prefix", "?q=refun*", []string{traceHex(1)}},
		{"nothing matches", "?q=aardvark", nil},
		{"q and another filter both apply", "?q=refund&environment=staging", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := listedIDs(t, h.get(t, "/api/v1/traces"+tc.query))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("GET /api/v1/traces%s = %v, want %v", tc.query, got, tc.want)
			}
		})
	}
}

// TestSearchRowsCarryTheirMatch: what people want from a search is not "which
// trace" but "where in it" (spec 011 #6).
func TestSearchRowsCarryTheirMatch(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedSearchable(t, h)

	rows := searchRows(t, h.get(t, "/api/v1/traces?q=refund"))
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want the one match", len(rows))
	}
	match := rows[0].Match
	if match == nil {
		t.Fatal("a row of a search has no match")
	}
	if match.ObservationID == nil || *match.ObservationID != spanHex(1) {
		t.Errorf("observation_id = %v, want the observation that matched", match.ObservationID)
	}
	if match.Field != "output" {
		t.Errorf("field = %q, want the field that matched", match.Field)
	}
	if !strings.Contains(match.Snippet, "refund failed") {
		t.Errorf("snippet = %q, want the text around the hit", match.Snippet)
	}
	if len([]rune(match.Snippet)) > store.SearchSnippetLength {
		t.Errorf("snippet is %d characters, over the limit", len([]rune(match.Snippet)))
	}

	// A trace matched by its own name says so with a null observation id.
	rows = searchRows(t, h.get(t, "/api/v1/traces?q=support-chat"))
	if len(rows) != 1 || rows[0].Match == nil {
		t.Fatalf("rows = %+v, want the trace matched by name", rows)
	}
	if rows[0].Match.ObservationID != nil {
		t.Errorf("observation_id = %v, want null for a trace-name match", *rows[0].Match.ObservationID)
	}
	if rows[0].Match.Field != "trace_name" {
		t.Errorf("field = %q, want trace_name", rows[0].Match.Field)
	}
}

// TestMatchIsAbsentWithoutSearchAndSelectable: `match` is a row field like the
// others — it answers to `?fields=` — and it never appears without a `q`.
func TestMatchIsAbsentWithoutSearchAndSelectable(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedSearchable(t, h)

	for _, row := range searchRows(t, h.get(t, "/api/v1/traces")) {
		if row.Match != nil {
			t.Errorf("a listing without q carries a match: %+v", row.Match)
		}
	}
	// Selected away, it is not in the row — and not computed either, which
	// is the point of asking before reading the payload.
	rows := searchRows(t, h.get(t, "/api/v1/traces?q=refund&fields=id,name"))
	if len(rows) != 1 || rows[0].Match != nil {
		t.Errorf("?fields=id,name still carried a match: %+v", rows)
	}
	if rows[0].Name == nil || *rows[0].Name != "support-chat" {
		t.Errorf("?fields=id,name dropped the name: %+v", rows[0])
	}
	// Selected in, it is there.
	rows = searchRows(t, h.get(t, "/api/v1/traces?q=refund&fields=id,match"))
	if len(rows) != 1 || rows[0].Match == nil {
		t.Fatalf("?fields=id,match dropped the match: %+v", rows)
	}
	if rows[0].Name != nil {
		t.Errorf("?fields=id,match kept the name: %+v", rows[0])
	}
}

// TestSearchRefusals: a query with no word in it is a 400 rather than an empty
// listing, and so is one past the length limit.
func TestSearchRefusals(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedSearchable(t, h)

	for _, tc := range []struct {
		name  string
		query string
	}{
		{"only punctuation", "?q=%28%29%3A"},
		{"past the length limit", "?q=" + strings.Repeat("a", store.MaxSearchQueryLength+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.get(t, "/api/v1/traces"+tc.query)
			expectStatus(t, rec, 400)
		})
	}
	// A `q` given without a value is the general rule about empty
	// parameters (spec 004 #23), not a search-specific one.
	expectStatus(t, h.get(t, "/api/v1/traces?q="), 400)
}

// TestSearchKeepsTheOrderAndTheCursors: `q` is a condition, not a sort. The
// rows stay newest first and a cursor still means what it meant (spec 011 #5).
func TestSearchKeepsTheOrderAndTheCursors(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	for i := 1; i <= 5; i++ {
		h.seed(t, &model.Trace{ID: traceHex(i), Name: "run"},
			&model.Observation{TraceID: traceHex(i), ID: spanHex(i), Type: model.TypeSpan,
				Level: model.LevelDefault, StartTime: seedBase + int64(i)*1000*ms,
				EndTime: seedBase + int64(i)*1000*ms + ms,
				Output:  "the refund failed"})
	}

	first := h.get(t, "/api/v1/traces?q=refund&limit=2&count=1")
	ids := listedIDs(t, first)
	if strings.Join(ids, ",") != strings.Join([]string{traceHex(5), traceHex(4)}, ",") {
		t.Fatalf("first page = %v, want the two newest matches", ids)
	}
	page := decodeJSON[struct {
		NextCursor  *string `json:"next_cursor"`
		Total       int     `json:"total"`
		TotalCapped bool    `json:"total_capped"`
	}](t, first)
	if page.Total != 5 || page.TotalCapped {
		t.Errorf("total = %d (capped %v), want the five matches", page.Total, page.TotalCapped)
	}
	if page.NextCursor == nil {
		t.Fatal("the search listing offers no next page")
	}

	next := listedIDs(t, h.get(t, "/api/v1/traces?q=refund&limit=2&cursor="+*page.NextCursor))
	if strings.Join(next, ",") != strings.Join([]string{traceHex(3), traceHex(2)}, ",") {
		t.Errorf("second page = %v, want the next two in the same order", next)
	}
}

// TestLastTraceTakesSearch is the edge case: the newest matching trace, and a
// 404 naming the query when there is none.
func TestLastTraceTakesSearch(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedSearchable(t, h)

	rec := h.get(t, "/api/v1/traces/last?q=refund")
	expectStatus(t, rec, 200)
	body := decodeJSON[struct {
		ID string `json:"id"`
	}](t, rec)
	if body.ID != traceHex(1) {
		t.Errorf("id = %s, want the newest trace matching the search", body.ID)
	}

	rec = h.get(t, "/api/v1/traces/last?q=aardvark")
	expectStatus(t, rec, 404)
	message := decodeJSON[map[string]string](t, rec)["error"]
	if !strings.Contains(message, "aardvark") {
		t.Errorf("404 = %q, want it to name the query that found nothing", message)
	}
}
