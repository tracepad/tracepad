package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
)

// The read-API queries (spec 004, Testing #1): what the filters mean, and —
// the reason keyset pagination was chosen at all — that a page seeks to its
// cursor instead of scanning from the newest row.

const day = int64(24 * 60 * 60 * 1_000_000_000)

func readStore(t *testing.T) (*Store, *Project) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "tracepad.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	project, err := s.CreateProject("test", KeyPair{PublicKey: "tp-pk-test", Secret: "tp-sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	return s, project
}

// seedTrace writes one trace with one observation through the same path ingest
// uses, so the aggregates under test are the ones ingest maintains.
func seedTrace(t *testing.T, s *Store, projectID string, trace *model.Trace, observations ...*model.Observation) {
	t.Helper()
	writer, err := s.NewWriter(WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	batch := &IngestBatch{ProjectID: projectID, Traces: []*model.Trace{trace}, Observations: observations}
	if err := writer.Submit(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
}

func hexTrace(n int) string { return fmt.Sprintf("%032x", n) }
func hexSpan(n int) string  { return fmt.Sprintf("%016x", n) }

// TestTraceKeysetSeeks is the check spec 004 #4 asks for by name: the shipped
// SQL, handed to EXPLAIN QUERY PLAN, must seek to the cursor. Spec 003 #25
// found the scores cursor scanning from the newest row on every page because
// the index lacked the tie-break; the trace list is the hottest read and gets
// the same verification rather than the same bug.
func TestTraceKeysetSeeks(t *testing.T) {
	s, project := readStore(t)

	query, args := traceQuery(project.ID, TraceFilter{
		Limit: 50,
		After: &TraceCursor{Timestamp: 1, ID: hexTrace(1)},
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
	// The cursor itself has to be part of the seek: an index that is used
	// only for `project_id=?` still walks every row of the project.
	if !strings.Contains(strings.ReplaceAll(joined, " ", ""), "(timestamp,id)<(?,?)") {
		t.Errorf("the cursor is not part of the index seek:\n%s", joined)
	}
	// A temporary B-tree means SQLite could not satisfy the ORDER BY from
	// the index, which is the other half of the spec 003 #25 finding.
	if strings.Contains(joined, "TEMP B-TREE") {
		t.Errorf("plan sorts through a temporary B-tree:\n%s", joined)
	}
}

func TestTraceFilters(t *testing.T) {
	s, project := readStore(t)

	seedTrace(t, s, project.ID,
		&model.Trace{ID: hexTrace(1), Name: "chat", UserID: "u1", SessionID: "s1",
			Environment: "production", Tags: []string{"beta", "support"}},
		&model.Observation{TraceID: hexTrace(1), ID: hexSpan(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: day, EndTime: day + 5_000_000,
			CostDetails: map[string]any{"total": 0.5}})
	seedTrace(t, s, project.ID,
		&model.Trace{ID: hexTrace(2), Name: "eval", UserID: "u2", Environment: "staging",
			Tags: []string{"beta"}},
		&model.Observation{TraceID: hexTrace(2), ID: hexSpan(2), Type: model.TypeGeneration,
			Level: model.LevelError, StartTime: 2 * day, EndTime: 2*day + 1_000_000})

	cost := 0.4
	for _, tc := range []struct {
		name   string
		filter TraceFilter
		want   []string
	}{
		{"all newest first", TraceFilter{}, []string{hexTrace(2), hexTrace(1)}},
		{"environment", TraceFilter{Environment: []string{"production"}}, []string{hexTrace(1)}},
		{"user", TraceFilter{UserID: "u2"}, []string{hexTrace(2)}},
		{"session", TraceFilter{SessionID: "s1"}, []string{hexTrace(1)}},
		{"name", TraceFilter{Name: []string{"eval"}}, []string{hexTrace(2)}},
		{"one tag", TraceFilter{Tags: []string{"beta"}}, []string{hexTrace(2), hexTrace(1)}},
		{"tags are ANDed", TraceFilter{Tags: []string{"beta", "support"}}, []string{hexTrace(1)}},
		{"status error", TraceFilter{Status: TraceStatusError}, []string{hexTrace(2)}},
		{"status ok", TraceFilter{Status: TraceStatusOK}, []string{hexTrace(1)}},
		{"min cost skips traces with none", TraceFilter{MinCost: &cost}, []string{hexTrace(1)}},
		{"from is inclusive", TraceFilter{From: ptr(2 * day)}, []string{hexTrace(2)}},
		{"to is exclusive", TraceFilter{To: ptr(2 * day)}, []string{hexTrace(1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.filter.Limit = 50
			rows, err := s.Traces(project.ID, tc.filter)
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

// TestTraceCursorWalksEveryRow pages one row at a time, which is where an
// ambiguous page boundary would show as a repeat or a gap.
func TestTraceCursorWalksEveryRow(t *testing.T) {
	s, project := readStore(t)

	// Two traces share a timestamp: without the id tie-break the boundary
	// between them is undefined.
	for i := 1; i <= 5; i++ {
		start := day * int64((i+1)/2)
		seedTrace(t, s, project.ID,
			&model.Trace{ID: hexTrace(i)},
			&model.Observation{TraceID: hexTrace(i), ID: hexSpan(i), Type: model.TypeSpan,
				Level: model.LevelDefault, StartTime: start, EndTime: start + 1})
	}

	var (
		seen   []string
		cursor *TraceCursor
	)
	for range 10 {
		rows, err := s.Traces(project.ID, TraceFilter{Limit: 1, After: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			break
		}
		seen = append(seen, rows[0].ID)
		cursor = &TraceCursor{Timestamp: rows[0].Timestamp, ID: rows[0].ID}
	}
	if len(seen) != 5 {
		t.Fatalf("walked %d rows (%v), want all 5 exactly once", len(seen), seen)
	}
	unique := map[string]bool{}
	for _, id := range seen {
		if unique[id] {
			t.Fatalf("row %s appeared twice in %v", id, seen)
		}
		unique[id] = true
	}
}

// TestTimestampSurvivesUnsetStartTimes is Decision 26: a trace whose spans all
// carried an unset start time still gets a sort key, because a NULL one is
// invisible to the keyset predicate and the trace would vanish from every page
// after the first.
func TestTimestampSurvivesUnsetStartTimes(t *testing.T) {
	s, project := readStore(t)

	seedTrace(t, s, project.ID,
		&model.Trace{ID: hexTrace(1)},
		&model.Observation{TraceID: hexTrace(1), ID: hexSpan(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: 0, EndTime: 0})
	seedTrace(t, s, project.ID,
		&model.Trace{ID: hexTrace(2)},
		&model.Observation{TraceID: hexTrace(2), ID: hexSpan(2), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: day, EndTime: day + 1})

	first, err := s.Traces(project.ID, TraceFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].ID != hexTrace(2) {
		t.Fatalf("first page = %v, want the timestamped trace", first)
	}
	next, err := s.Traces(project.ID, TraceFilter{Limit: 1,
		After: &TraceCursor{Timestamp: first[0].Timestamp, ID: first[0].ID}})
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != 1 || next[0].ID != hexTrace(1) {
		t.Fatalf("second page = %v, want the trace whose spans never started", next)
	}
}

func TestSessionRollup(t *testing.T) {
	s, project := readStore(t)

	seedTrace(t, s, project.ID,
		&model.Trace{ID: hexTrace(1), SessionID: "s1"},
		&model.Observation{TraceID: hexTrace(1), ID: hexSpan(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: day, EndTime: day + 1,
			CostDetails: map[string]any{"total": 0.25}})
	seedTrace(t, s, project.ID,
		&model.Trace{ID: hexTrace(2), SessionID: "s1"},
		&model.Observation{TraceID: hexTrace(2), ID: hexSpan(2), Type: model.TypeSpan,
			Level: model.LevelError, StartTime: 2 * day, EndTime: 2*day + 1})

	session, err := s.Session(project.ID, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if session.TraceCount != 2 || session.ErrorCount != 1 {
		t.Errorf("session = %+v, want 2 traces of which 1 failed", session)
	}
	if session.TotalCost == nil || *session.TotalCost != 0.25 {
		t.Errorf("total_cost = %v, want the one trace that carried a cost", session.TotalCost)
	}
	if session.FirstSeen != day || session.LastSeen != 2*day {
		t.Errorf("seen = %d..%d, want %d..%d", session.FirstSeen, session.LastSeen, day, 2*day)
	}

	missing, err := s.Session(project.ID, "nope")
	if err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Errorf("session %+v, want nothing for a session no trace named", missing)
	}
}

// TestObservationTracesFindsAmbiguity is what the 409 on `/observations/{id}/io`
// is built on: a span id is unique only inside its trace.
func TestObservationTracesFindsAmbiguity(t *testing.T) {
	s, project := readStore(t)

	for i := 1; i <= 2; i++ {
		seedTrace(t, s, project.ID,
			&model.Trace{ID: hexTrace(i)},
			&model.Observation{TraceID: hexTrace(i), ID: hexSpan(7), Type: model.TypeSpan,
				Level: model.LevelDefault, StartTime: day, EndTime: day + 1})
	}
	candidates, err := s.ObservationTraces(project.ID, hexSpan(7))
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %v, want both traces", candidates)
	}
}

func ptr[T any](v T) *T { return &v }
