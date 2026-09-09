package store

import (
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
)

// The session listing (spec 007 #2): what the aggregation means, that the page
// boundary is unambiguous, and that the grouping rides the session index
// rather than sorting a project's whole trace table.

// seedSession writes one trace of a session at a given instant, with a cost
// and a level the aggregates can be read back from.
func seedSession(t *testing.T, s *Store, projectID, traceID, sessionID string,
	at int64, level string, cost float64) {
	t.Helper()
	trace := &model.Trace{ID: traceID, SessionID: sessionID, UserID: "u1", Environment: "production"}
	observation := &model.Observation{
		TraceID: traceID, ID: traceID[:16], Type: model.TypeGeneration,
		Level: level, StartTime: at, EndTime: at + 1_000_000,
	}
	if cost > 0 {
		observation.CostDetails = map[string]any{"total": cost}
	}
	seedTrace(t, s, projectID, trace, observation)
}

func TestSessionListingAggregates(t *testing.T) {
	s, project := readStore(t)

	// Two traces in `s1`, one of them failing and only one of them priced.
	seedSession(t, s, project.ID, hexTrace(1), "s1", day, model.LevelDefault, 0.25)
	seedSession(t, s, project.ID, hexTrace(2), "s1", 3*day, model.LevelError, 0)
	// One trace in `s2`, older, and priced by nobody.
	seedSession(t, s, project.ID, hexTrace(3), "s2", 2*day, model.LevelDefault, 0)
	// A trace that named no session is not a session of one.
	seedTrace(t, s, project.ID,
		&model.Trace{ID: hexTrace(4)},
		&model.Observation{TraceID: hexTrace(4), ID: hexSpan(4), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: 9 * day, EndTime: 9 * day})

	rows, err := s.Sessions(project.ID, SessionFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("listed %d sessions, want 2 (the session-less trace must not appear)", len(rows))
	}
	// Most recent activity first, whatever the sessions started.
	if rows[0].ID != "s1" || rows[1].ID != "s2" {
		t.Fatalf("order = %s, %s; want s1, s2", rows[0].ID, rows[1].ID)
	}

	first := rows[0]
	if first.TraceCount != 2 {
		t.Errorf("trace_count = %d, want 2", first.TraceCount)
	}
	// Counted in traces: one of the two traces failed, whatever its spans did.
	if first.ErrorCount != 1 {
		t.Errorf("error_count = %d, want 1", first.ErrorCount)
	}
	if first.TotalCost == nil || *first.TotalCost != 0.25 {
		t.Errorf("total_cost = %v, want 0.25 summed over the trace that had one", first.TotalCost)
	}
	if first.FirstSeen != day || first.LastSeen != 3*day {
		t.Errorf("window = %d..%d, want %d..%d", first.FirstSeen, first.LastSeen, day, 3*day)
	}
	// A session nobody priced has no cost at all — absent is not zero
	// (spec 002 #14).
	if rows[1].TotalCost != nil {
		t.Errorf("total_cost = %v for an unpriced session, want absent", *rows[1].TotalCost)
	}
}

func TestSessionListingFilters(t *testing.T) {
	s, project := readStore(t)

	seedSession(t, s, project.ID, hexTrace(1), "s1", day, model.LevelDefault, 0)
	seedSession(t, s, project.ID, hexTrace(2), "s1", 5*day, model.LevelDefault, 0)
	seedTrace(t, s, project.ID,
		&model.Trace{ID: hexTrace(3), SessionID: "s2", UserID: "u2", Environment: "staging"},
		&model.Observation{TraceID: hexTrace(3), ID: hexSpan(3), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: 2 * day, EndTime: 2 * day})

	for _, tc := range []struct {
		name   string
		filter SessionFilter
		want   []string
	}{
		{"all", SessionFilter{}, []string{"s1", "s2"}},
		{"environment", SessionFilter{Environment: []string{"staging"}}, []string{"s2"}},
		{"user", SessionFilter{UserID: "u1"}, []string{"s1"}},
		// The window bounds the traces; a session appears when any of
		// them falls inside it.
		{"from is inclusive", SessionFilter{From: ptr(5 * day)}, []string{"s1"}},
		{"to is exclusive", SessionFilter{To: ptr(2 * day)}, []string{"s1"}},
		{"a window with nothing in it", SessionFilter{From: ptr(6 * day)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.filter.Limit = 50
			rows, err := s.Sessions(project.ID, tc.filter)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, row := range rows {
				ids = append(ids, row.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("ids = %v, want %v", ids, tc.want)
			}
		})
	}
}

// TestSessionListingWindowNarrowsTheAggregates is the other half of the window
// semantics (spec 007 #10): the filter selects the traces, so the roll-up
// describes the traces in the window rather than the session's whole life.
func TestSessionListingWindowNarrowsTheAggregates(t *testing.T) {
	s, project := readStore(t)

	seedSession(t, s, project.ID, hexTrace(1), "s1", day, model.LevelDefault, 0.25)
	seedSession(t, s, project.ID, hexTrace(2), "s1", 5*day, model.LevelDefault, 0.75)

	rows, err := s.Sessions(project.ID, SessionFilter{Limit: 50, From: ptr(2 * day)})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TraceCount != 1 {
		t.Fatalf("rows = %#v, want one session holding one trace", rows)
	}
	if *rows[0].TotalCost != 0.75 || rows[0].FirstSeen != 5*day {
		t.Errorf("roll-up = %v at %d, want only the trace inside the window",
			*rows[0].TotalCost, rows[0].FirstSeen)
	}
}

// TestSessionCursorWalksEveryRow walks the listing one row at a time with two
// sessions whose last activity is the same instant — the boundary the id
// tie-break exists for.
func TestSessionCursorWalksEveryRow(t *testing.T) {
	s, project := readStore(t)

	for i := 1; i <= 5; i++ {
		// Sessions 2 and 3 (and 4 and 5) end at the same instant.
		at := day * int64((i+1)/2)
		seedSession(t, s, project.ID, hexTrace(i), "s"+string(rune('0'+i)), at, model.LevelDefault, 0)
	}

	var (
		seen   []string
		cursor *SessionCursor
	)
	for range 10 {
		rows, err := s.Sessions(project.ID, SessionFilter{Limit: 1, After: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		seen = append(seen, rows[0].ID)
		cursor = &SessionCursor{LastSeen: rows[0].LastSeen, ID: rows[0].ID}
	}
	if len(seen) != 5 {
		t.Fatalf("walked %d rows (%v), want all 5 exactly once", len(seen), seen)
	}
	unique := map[string]bool{}
	for _, id := range seen {
		if unique[id] {
			t.Fatalf("session %s appeared twice in %v", id, seen)
		}
		unique[id] = true
	}
}

// TestSessionListingUsesTheSessionIndex is the spec 003 #25 method pointed at
// the new listing: the grouping has to ride `idx_traces_session`, which is
// what keeps it from reading and sorting every trace of the project.
func TestSessionListingUsesTheSessionIndex(t *testing.T) {
	s, project := readStore(t)

	query, args := sessionQuery(project.ID, SessionFilter{
		Limit: 50,
		After: &SessionCursor{LastSeen: day, ID: "s1"},
	})
	plan, err := s.explainQueryPlan(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")

	if !strings.Contains(joined, "idx_traces_session") {
		t.Errorf("the grouping does not use the session index:\n%s", joined)
	}
	// The index also has to supply the grouping order; falling back to a
	// temporary B-tree for GROUP BY is the scan this test exists to catch.
	if strings.Contains(joined, "TEMP B-TREE FOR GROUP BY") {
		t.Errorf("the grouping sorts through a temporary B-tree:\n%s", joined)
	}
}

// TestSessionsAreScopedToTheirProject: every listing is scoped, and a grouping
// query is exactly where a forgotten project_id would go unnoticed (spec 004
// #33).
func TestSessionsAreScopedToTheirProject(t *testing.T) {
	s, mine := readStore(t)
	theirs, err := s.CreateProject("other", KeyPair{PublicKey: "tp-pk-other", Secret: "tp-sk-other"})
	if err != nil {
		t.Fatal(err)
	}

	seedSession(t, s, mine.ID, hexTrace(1), "shared", day, model.LevelDefault, 0)
	seedSession(t, s, theirs.ID, hexTrace(2), "shared", 2*day, model.LevelError, 9)
	seedSession(t, s, theirs.ID, hexTrace(3), "theirs-only", 2*day, model.LevelDefault, 0)

	rows, err := s.Sessions(mine.ID, SessionFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != "shared" {
		t.Fatalf("rows = %#v, want only this project's session", rows)
	}
	if rows[0].TraceCount != 1 || rows[0].ErrorCount != 0 || rows[0].TotalCost != nil {
		t.Errorf("roll-up = %#v, want the other project's traces excluded from it", rows[0])
	}
}
