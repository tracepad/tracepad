package server

import (
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// The session listing (spec 007 #1, #2). The corpus of `traces_test.go`
// already holds two traces in `s1` and one trace in no session at all, which
// is the shape the aggregation has to get right.

// sessionRow is one row of the listing as the endpoint renders it.
type sessionRow struct {
	ID         string   `json:"id"`
	TraceCount int      `json:"trace_count"`
	ErrorCount int      `json:"error_count"`
	TotalCost  *float64 `json:"total_cost"`
	FirstSeen  string   `json:"first_seen"`
	LastSeen   string   `json:"last_seen"`
}

type sessionPage struct {
	Sessions   []sessionRow `json:"sessions"`
	NextCursor *string      `json:"next_cursor"`
}

func TestListSessionsAggregatesFromTraces(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)
	// A second session, older than `s1`, so the ordering has something to
	// order.
	h.seed(t, &model.Trace{ID: traceHex(4), Name: "chat", UserID: "u3", SessionID: "s2",
		Environment: "staging"},
		&model.Observation{TraceID: traceHex(4), ID: spanHex(4), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase - 5000*ms, EndTime: seedBase - 4900*ms})

	rec := h.get(t, "/api/v1/sessions")
	expectStatus(t, rec, 200)
	page := decodeJSON[sessionPage](t, rec)

	if len(page.Sessions) != 2 {
		t.Fatalf("sessions = %#v, want two (trace 3 named none and must not appear)", page.Sessions)
	}
	// Most recent activity first.
	if page.Sessions[0].ID != "s1" || page.Sessions[1].ID != "s2" {
		t.Fatalf("order = %s, %s; want s1, s2", page.Sessions[0].ID, page.Sessions[1].ID)
	}

	first := page.Sessions[0]
	if first.TraceCount != 2 || first.ErrorCount != 1 {
		t.Errorf("s1 = %d traces, %d failed; want 2 and 1", first.TraceCount, first.ErrorCount)
	}
	if first.TotalCost == nil || *first.TotalCost != 0.25 {
		t.Errorf("total_cost = %v, want the 0.25 the one priced trace carried", first.TotalCost)
	}
	if first.FirstSeen == "" || first.LastSeen == "" || first.FirstSeen >= first.LastSeen {
		t.Errorf("window = %q .. %q, want the first trace before the last", first.FirstSeen, first.LastSeen)
	}
	// A session nothing priced carries no cost at all, rather than zero
	// (spec 002 #14).
	if page.Sessions[1].TotalCost != nil {
		t.Errorf("total_cost = %v for an unpriced session, want the field absent", *page.Sessions[1].TotalCost)
	}
	if page.NextCursor != nil {
		t.Errorf("next_cursor = %q on the last page, want null", *page.NextCursor)
	}
}

func TestListSessionsFilters(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)
	h.seed(t, &model.Trace{ID: traceHex(4), SessionID: "s2", UserID: "u3", Environment: "staging"},
		&model.Observation{TraceID: traceHex(4), ID: spanHex(4), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase - 5000*ms, EndTime: seedBase - 4900*ms})

	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"unfiltered", "", []string{"s1", "s2"}},
		{"environment", "?environment=staging", []string{"s2"}},
		{"user", "?user_id=u1", []string{"s1"}},
		{"from is inclusive", "?from=2026-09-01T00:00:00Z", []string{"s1"}},
		{"to is exclusive", "?to=2026-09-01T00:00:00Z", []string{"s2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.get(t, "/api/v1/sessions"+tc.query)
			expectStatus(t, rec, 200)
			var ids []string
			for _, row := range decodeJSON[sessionPage](t, rec).Sessions {
				ids = append(ids, row.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("ids = %v, want %v", ids, tc.want)
			}
		})
	}
}

// TestListSessionsWalksTheCursor pages one row at a time over sessions whose
// last activity is the same instant — the tie the id half of the keyset is
// there to break.
func TestListSessionsWalksTheCursor(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	for i := 1; i <= 4; i++ {
		// Sessions 1 and 2 end together, and so do 3 and 4.
		at := seedBase + int64((i+1)/2)*1000*ms
		h.seed(t, &model.Trace{ID: traceHex(i), SessionID: "s" + string(rune('0'+i))},
			&model.Observation{TraceID: traceHex(i), ID: spanHex(i), Type: model.TypeSpan,
				Level: model.LevelDefault, StartTime: at, EndTime: at})
	}

	var (
		seen []string
		path = "/api/v1/sessions?limit=1"
	)
	for range 8 {
		rec := h.get(t, path)
		expectStatus(t, rec, 200)
		page := decodeJSON[sessionPage](t, rec)
		if len(page.Sessions) == 0 {
			break
		}
		seen = append(seen, page.Sessions[0].ID)
		if page.NextCursor == nil {
			break
		}
		path = "/api/v1/sessions?limit=1&cursor=" + *page.NextCursor
	}

	if len(seen) != 4 {
		t.Fatalf("walked %v, want all four sessions exactly once", seen)
	}
	unique := map[string]bool{}
	for _, id := range seen {
		if unique[id] {
			t.Fatalf("session %s appeared twice in %v", id, seen)
		}
		unique[id] = true
	}
}

func TestListSessionsRefusesNonsense(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	expectError(t, h.get(t, "/api/v1/sessions?status=error"), 400, "unknown query parameter")
	expectError(t, h.get(t, "/api/v1/sessions?from=yesterday"), 400, "RFC 3339")
	expectError(t, h.get(t, "/api/v1/sessions?limit=0"), 400, "limit must be")
	expectError(t, h.get(t, "/api/v1/sessions?cursor=not-a-cursor"), 400, "invalid cursor")
}

// TestEmptySessionListing: no sessions is an empty array, not null and not a
// 404 — the same shape the trace listing answers an empty project with.
func TestEmptySessionListing(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	rec := h.get(t, "/api/v1/sessions")
	expectStatus(t, rec, 200)
	want := `{"sessions":[],"next_cursor":null,"prev_cursor":null}`
	if body := strings.TrimSpace(rec.Body.String()); body != want {
		t.Errorf("body = %s, want an empty page", body)
	}
}
