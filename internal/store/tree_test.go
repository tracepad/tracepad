package store

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// A tree's prefix is a seek in (start_time, id) order, not a sort of the
// whole trace: without the order in the index, the LIMIT bounded the rows
// returned and not the work (spec 043 #18, #28).
func TestTheTreePrefixIsASeek(t *testing.T) {
	s, project := readStore(t)
	plan, err := s.explainQueryPlan(treeQuery, project.ID, "trace", MaxTreeObservations+1)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "idx_observations_trace") || strings.Contains(joined, "TEMP B-TREE") {
		t.Errorf("the tree's read sorts rather than seeks:\n%s", joined)
	}
}

// The listing's traffic counts in batches, so a deployment of more projects
// than one statement may bind still gets every count (spec 043 #29).
func TestTracesBetweenCountsPastOneBatch(t *testing.T) {
	s, project := readStore(t)
	seedListing(t, s, project.ID, 3)
	// Past the 32,766 variables SQLite binds in one statement.
	const many = 66 * inBatch
	ids := make([]string, 0, many)
	for i := range many - 1 {
		ids = append(ids, fmt.Sprintf("absent-%d", i))
	}
	ids = append(ids, project.ID)
	counts, err := s.TracesBetween(t.Context(), ids, 0, math.MaxInt64)
	if err != nil {
		t.Fatal(err)
	}
	var want int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM traces WHERE project_id = ?`, project.ID).Scan(&want); err != nil {
		t.Fatal(err)
	}
	if want == 0 || counts[project.ID] != want || len(counts) != 1 {
		t.Errorf("counts = %v, want %d for the project in the last batch and nothing else", counts, want)
	}
}
