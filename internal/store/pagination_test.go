package store

import (
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
)

// Paging both ways and counting with a cap (spec 009, Testing). The backward
// page is where a keyset earns its keep or quietly stops being one: it is the
// same index read the other way, and a tie on `timestamp` is exactly where an
// off-by-one hides.

// seedListing writes n traces over n/2 days, so that pairs of them share a
// timestamp and the id tie-break is the only thing ordering them.
func seedListing(t *testing.T, s *Store, projectID string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		start := day * int64((i+1)/2)
		seedTrace(t, s, projectID,
			&model.Trace{ID: hexTrace(i), SessionID: "s" + string(rune('0'+i%3))},
			&model.Observation{TraceID: hexTrace(i), ID: hexSpan(i), Type: model.TypeSpan,
				Level: model.LevelDefault, StartTime: start, EndTime: start + 1})
	}
}

func traceIDs(rows []*TraceRow) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.ID
	}
	return out
}

// TestBackwardPageIsTheForwardPageBefore: `‹` has to land on exactly the rows
// `›` came from, in the same order. Anything else means the two directions
// disagree about where a page boundary is, and a reader walking back and forth
// sees rows appear or vanish.
func TestBackwardPageIsTheForwardPageBefore(t *testing.T) {
	s, project := readStore(t)
	seedListing(t, s, project.ID, 6)

	first, err := s.Traces(t.Context(), project.ID, TraceFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	last := first[len(first)-1]
	second, err := s.Traces(t.Context(), project.ID, TraceFilter{Limit: 2,
		After: &TraceCursor{Timestamp: last.Timestamp, ID: last.ID}})
	if err != nil {
		t.Fatal(err)
	}

	// Back from the first row of the second page.
	head := second[0]
	back, err := s.Traces(t.Context(), project.ID, TraceFilter{Limit: 2, Backward: true,
		After: &TraceCursor{Timestamp: head.Timestamp, ID: head.ID}})
	if err != nil {
		t.Fatal(err)
	}

	want, got := traceIDs(first), traceIDs(back)
	if strings.Join(want, ",") != strings.Join(got, ",") {
		t.Errorf("backward page = %v, want the forward page before it %v", got, want)
	}
}

// TestOldestPageIsTheTailOfTheListing: "jump to the end" is a direction with
// no cursor (spec 009 #2), and what it lands on is a *full* page anchored at
// the oldest row — not the ragged remainder that walking forward happens to
// end on. With five rows and pages of two, walking forward ends on one row and
// `»` shows two; both end on the same row, which is what "the end" means.
func TestOldestPageIsTheTailOfTheListing(t *testing.T) {
	s, project := readStore(t)
	seedListing(t, s, project.ID, 5)

	// Everything, in order, by walking forward.
	var (
		all    []*TraceRow
		cursor *TraceCursor
	)
	for range 10 {
		rows, err := s.Traces(t.Context(), project.ID, TraceFilter{Limit: 2, After: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		all = append(all, rows...)
		last := rows[len(rows)-1]
		cursor = &TraceCursor{Timestamp: last.Timestamp, ID: last.ID}
	}

	oldest, err := s.Traces(t.Context(), project.ID, TraceFilter{Limit: 2, Backward: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(oldest) != 2 {
		t.Fatalf("oldest page has %d rows, want a full page of 2", len(oldest))
	}
	want := traceIDs(all[len(all)-2:])
	if got := traceIDs(oldest); strings.Join(want, ",") != strings.Join(got, ",") {
		t.Errorf("oldest page = %v, want the tail of the listing %v", got, want)
	}
}

// TestBackwardKeysetSeeks is TestTraceKeysetSeeks pointed the other way: the
// backward page must ride the same index rather than sort its way there, or
// `‹` costs what an offset would and spec 009 #3's whole argument collapses.
func TestBackwardKeysetSeeks(t *testing.T) {
	s, project := readStore(t)

	query, args := traceQuery(project.ID, TraceFilter{
		Limit:    50,
		Backward: true,
		After:    &TraceCursor{Timestamp: 1, ID: hexTrace(1)},
	})
	plan, err := s.explainQueryPlan(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")

	if !strings.Contains(joined, "idx_traces_timestamp") {
		t.Errorf("plan does not use the keyset index:\n%s", joined)
	}
	if !strings.Contains(joined, "SEARCH") {
		t.Errorf("plan scans rather than seeks:\n%s", joined)
	}
	if !strings.Contains(strings.ReplaceAll(joined, " ", ""), "(timestamp,id)>(?,?)") {
		t.Errorf("the cursor is not part of the index seek:\n%s", joined)
	}
	if strings.Contains(joined, "TEMP B-TREE") {
		t.Errorf("plan sorts through a temporary B-tree:\n%s", joined)
	}
}

// TestCountTracesStopsAtTheCap: the number is exact below the cap and pinned
// at it above, which is what lets the interface print "847" or "5+" and never
// scan more than the cap (spec 009 #4).
func TestCountTracesStopsAtTheCap(t *testing.T) {
	s, project := readStore(t)
	seedListing(t, s, project.ID, 6)

	for _, test := range []struct {
		name string
		cap  int
		want int
	}{
		{"below the cap", 100, 6},
		{"exactly at it", 6, 6},
		{"pinned above it", 5, 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := s.CountTraces(t.Context(), project.ID, TraceFilter{}, test.cap)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Errorf("count = %d, want %d", got, test.want)
			}
		})
	}
}

// TestCountTracesObeysTheFilters: the count answers "how many match what is on
// screen", so it has to be the listing's own filter and not the project's
// total. The cursor, on the other hand, must *not* narrow it — that would
// count what is left rather than what there is.
func TestCountTracesObeysTheFilters(t *testing.T) {
	s, project := readStore(t)
	seedListing(t, s, project.ID, 6)

	rows, err := s.Traces(t.Context(), project.ID, TraceFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	last := rows[len(rows)-1]
	filter := TraceFilter{
		From:  ptr(day * 2),
		After: &TraceCursor{Timestamp: last.Timestamp, ID: last.ID},
	}

	got, err := s.CountTraces(t.Context(), project.ID, filter, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// Traces 3..6 sit on days 2 and 3; the cursor is not part of it.
	if got != 4 {
		t.Errorf("count = %d, want 4 — the window applies and the cursor does not", got)
	}
}

// TestCountSessionsCountsSessions: the cap has to bound *groups*, because the
// number stands next to a listing of sessions and a session is many traces.
func TestCountSessionsCountsSessions(t *testing.T) {
	s, project := readStore(t)
	seedListing(t, s, project.ID, 6) // six traces across three session ids

	got, err := s.CountSessions(t.Context(), project.ID, SessionFilter{}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got != 3 {
		t.Errorf("count = %d, want 3 sessions from 6 traces", got)
	}

	capped, err := s.CountSessions(t.Context(), project.ID, SessionFilter{}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if capped != 2 {
		t.Errorf("capped count = %d, want 2", capped)
	}
}

// TestSessionsPageBothWays is the trace test's twin: the session listing pages
// on an aggregate, so its backward keyset lives in a HAVING and is the easier
// of the two to get wrong.
func TestSessionsPageBothWays(t *testing.T) {
	s, project := readStore(t)
	seedListing(t, s, project.ID, 6)

	first, err := s.Sessions(t.Context(), project.ID, SessionFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	last := first[len(first)-1]
	second, err := s.Sessions(t.Context(), project.ID, SessionFilter{Limit: 2,
		After: &SessionCursor{LastSeen: last.LastSeen, ID: last.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(second) == 0 {
		t.Fatal("no second page to come back from")
	}

	head := second[0]
	back, err := s.Sessions(t.Context(), project.ID, SessionFilter{Limit: 2, Backward: true,
		After: &SessionCursor{LastSeen: head.LastSeen, ID: head.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != len(first) || back[0].ID != first[0].ID {
		t.Errorf("backward page = %v, want the forward page before it %v",
			sessionIDs(back), sessionIDs(first))
	}
}

func sessionIDs(rows []*SessionRow) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.ID
	}
	return out
}
