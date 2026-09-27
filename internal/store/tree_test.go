package store

import (
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
