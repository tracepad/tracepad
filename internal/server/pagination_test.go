package server

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// Paging both ways over the HTTP surface (spec 009): the two cursors, the end
// anchors, and the capped count. The store tests prove the SQL; these prove
// the bookkeeping around it — which end the probe row lands on, and which of
// the two cursors a page is allowed to claim.

// tracePage is a listing response with everything spec 009 added.
type tracePage struct {
	Traces []struct {
		ID string `json:"id"`
	} `json:"traces"`
	NextCursor  *string `json:"next_cursor"`
	PrevCursor  *string `json:"prev_cursor"`
	Total       *int    `json:"total"`
	TotalCapped *bool   `json:"total_capped"`
}

func readPage(t *testing.T, rec *httptest.ResponseRecorder) tracePage {
	t.Helper()
	expectStatus(t, rec, 200)
	return decodeJSON[tracePage](t, rec)
}

func (p tracePage) ids() []string {
	out := make([]string, len(p.Traces))
	for i, row := range p.Traces {
		out[i] = row.ID
	}
	return out
}

// TestNewestPageHasNoPreviousOne: the newest page is where « lands and where a
// reader starts, and claiming a page before it would light a control that goes
// nowhere.
func TestNewestPageHasNoPreviousOne(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)

	page := readPage(t, h.get(t, "/api/v1/traces?limit=2"))
	if page.PrevCursor != nil {
		t.Errorf("prev_cursor = %q, want null on the newest page", *page.PrevCursor)
	}
	if page.NextCursor == nil {
		t.Error("next_cursor is null with a third trace waiting")
	}
}

// TestOldestPageHasNoNextOne: `?direction=prev` with no cursor is the other
// end, and it is an anchor rather than an offset (#2).
func TestOldestPageHasNoNextOne(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)

	page := readPage(t, h.get(t, "/api/v1/traces?limit=2&direction=prev"))
	if page.NextCursor != nil {
		t.Errorf("next_cursor = %q, want null on the oldest page", *page.NextCursor)
	}
	if page.PrevCursor == nil {
		t.Error("prev_cursor is null with a newer trace above the page")
	}
	// Newest first, whichever way the page was found.
	if got := page.ids(); got[0] != traceHex(2) || got[1] != traceHex(1) {
		t.Errorf("oldest page = %v, want the two oldest traces newest first", got)
	}
}

// TestBackAndForthLandsOnTheSameRows is the round trip a reader makes with
// › then ‹: the page they come back to has to be the page they left.
func TestBackAndForthLandsOnTheSameRows(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)

	first := readPage(t, h.get(t, "/api/v1/traces?limit=2"))
	forward := readPage(t, h.get(t,
		fmt.Sprintf("/api/v1/traces?limit=2&cursor=%s", *first.NextCursor)))
	if forward.PrevCursor == nil {
		t.Fatal("a page reached with a cursor claims no previous page")
	}
	back := readPage(t, h.get(t,
		fmt.Sprintf("/api/v1/traces?limit=2&direction=prev&cursor=%s", *forward.PrevCursor)))

	want, got := first.ids(), back.ids()
	if fmt.Sprint(want) != fmt.Sprint(got) {
		t.Errorf("came back to %v, want the page we left %v", got, want)
	}
}

// TestAnEmptyCursoredPageKeepsAWayBack: the rows a cursor named can be gone
// by the time somebody reloads — swept by retention while they sat on that
// page. The answer is empty, but it is not a dead end: the cursor it came
// from, read the other way, is the page before it (PR #11 review).
func TestAnEmptyCursoredPageKeepsAWayBack(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)

	// A cursor below every row: what a reader is left holding when the rows
	// it named have been swept out from under them. Built here rather than
	// walked to, because the endpoint never hands out a cursor past its own
	// last row — the situation only arises once the rows are gone.
	dead := encodeCursor("1", strings.Repeat("0", 32))
	beyond := readPage(t, h.get(t, "/api/v1/traces?limit=2&cursor="+dead))

	if len(beyond.Traces) != 0 {
		t.Fatalf("expected an empty page past the end, got %v", beyond.ids())
	}
	if beyond.PrevCursor == nil {
		t.Fatal("an empty page reached by a cursor claims no way back")
	}
	if beyond.NextCursor != nil {
		t.Error("an empty page claims a page after it")
	}

	// And going back from it lands on rows: the ones above the dead cursor.
	back := readPage(t, h.get(t,
		fmt.Sprintf("/api/v1/traces?limit=2&direction=prev&cursor=%s", *beyond.PrevCursor)))
	if len(back.Traces) == 0 {
		t.Error("the way back leads nowhere")
	}

	// An empty *first* page is empty in both directions: there is nothing on
	// either side of nothing.
	fresh := newHarness(t, nil, store.WriterOptions{})
	blank := readPage(t, fresh.get(t, "/api/v1/traces"))
	if blank.PrevCursor != nil || blank.NextCursor != nil {
		t.Error("an empty listing claims pages around it")
	}
}

// TestCountIsOptionalAndCapped: the count is opt-in because it answers a
// question about the filters and not about the page (#4).
func TestCountIsOptionalAndCapped(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)

	quiet := readPage(t, h.get(t, "/api/v1/traces?limit=1"))
	if quiet.Total != nil || quiet.TotalCapped != nil {
		t.Error("the listing counted without being asked to")
	}

	counted := readPage(t, h.get(t, "/api/v1/traces?limit=1&count=1"))
	if counted.Total == nil || *counted.Total != 3 {
		t.Errorf("total = %v, want 3 — every trace the filter matches, not the page", counted.Total)
	}
	if counted.TotalCapped == nil || *counted.TotalCapped {
		t.Error("total_capped is true well below the cap")
	}

	// And it is the filter's count, not the project's.
	narrowed := readPage(t, h.get(t, "/api/v1/traces?limit=1&count=1&environment=staging"))
	if narrowed.Total == nil || *narrowed.Total != 1 {
		t.Errorf("filtered total = %v, want 1", narrowed.Total)
	}
}

// TestPageParametersAreChecked: a value that cannot mean what it says is a
// 400, in the house style of spec 003 #23 — silently reading `direction=back`
// as "next" would page the wrong way with no sign of it.
func TestPageParametersAreChecked(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	expectError(t, h.get(t, "/api/v1/traces?direction=back"), 400, "direction")
	expectError(t, h.get(t, "/api/v1/traces?count=yes"), 400, "count")
	expectError(t, h.get(t, "/api/v1/sessions?direction=back"), 400, "direction")
	// The single session pages, but its `trace_count` is already the exact
	// total, so it takes no `count` at all.
	expectError(t, h.get(t, "/api/v1/sessions/s1?count=1"), 400, "count")
}

// TestSessionListingPagesBothWays: the same contract one level up, where the
// keyset lives in a HAVING over an aggregate.
func TestSessionListingPagesBothWays(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)
	// A second session, older than `s1`, so there is an order to walk.
	h.seed(t, &model.Trace{ID: traceHex(4), Name: "chat", UserID: "u3", SessionID: "s2",
		Environment: "staging"},
		&model.Observation{TraceID: traceHex(4), ID: spanHex(4), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase - 5000*ms, EndTime: seedBase - 4900*ms})

	page := decodeJSON[struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
		NextCursor  *string `json:"next_cursor"`
		PrevCursor  *string `json:"prev_cursor"`
		Total       *int    `json:"total"`
		TotalCapped *bool   `json:"total_capped"`
	}](t, h.get(t, "/api/v1/sessions?limit=1&count=1"))

	if page.PrevCursor != nil {
		t.Error("the newest session page claims a page before it")
	}
	if page.NextCursor == nil {
		t.Fatal("no next page with two sessions and a limit of one")
	}
	if page.Total == nil || *page.Total != 2 {
		t.Errorf("total = %v, want 2 sessions", page.Total)
	}

	oldest := decodeJSON[struct {
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
		NextCursor *string `json:"next_cursor"`
	}](t, h.get(t, "/api/v1/sessions?limit=1&direction=prev"))
	if oldest.NextCursor != nil {
		t.Error("the oldest session page claims a page after it")
	}
	if len(oldest.Sessions) != 1 || oldest.Sessions[0].ID != "s2" {
		t.Errorf("oldest session page = %v, want s2", oldest.Sessions)
	}
}
