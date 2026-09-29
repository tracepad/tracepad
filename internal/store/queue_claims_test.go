package store

import (
	"testing"
	"time"
)

// TestAnExpiredClaimIsNobodysToList: `account=` and `annotator=` list a pending
// item as the reviewer's only while the claim holds (spec 048 #19, spec 024
// #20): an expired one is anybody's `next`.
func TestAnExpiredClaimIsNobodysToList(t *testing.T) {
	f := newAccountFixture(t)
	f.submit(t, &QueuePut{ProjectID: f.project.ID, Queue: &AnnotationQueue{Name: "review", ScoreConfigs: []string{}},
		Now: 1})
	f.submit(t, &QueueItemsAdd{ProjectID: f.project.ID, Queue: "review",
		Targets: []QueueTarget{{TraceID: "t1"}, {TraceID: "t2"}}, Now: 1})
	at := time.Now().UnixNano()
	f.submit(t, &QueueNext{ProjectID: f.project.ID, Queue: "review", Reviewer: Reviewer{Account: "acc"}, Now: at})
	f.submit(t, &QueueNext{ProjectID: f.project.ID, Queue: "review", Reviewer: Reviewer{Name: "judge"}, Now: at})

	for _, c := range []struct {
		name   string
		filter QueueItemFilter
	}{
		{"account", QueueItemFilter{Account: "acc"}},
		{"annotator", QueueItemFilter{Annotator: "judge"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			held := c.filter
			held.Limit, held.Now = 10, at+1
			if items, err := f.QueueItems(t.Context(), f.project.ID, "review", held); err != nil || len(items) != 1 {
				t.Errorf("while the claim holds: %d items, %v; want the one", len(items), err)
			}
			expired := c.filter
			expired.Limit, expired.Now = 10, at+int64(ClaimTTL)+1
			if items, err := f.QueueItems(t.Context(), f.project.ID, "review", expired); err != nil || len(items) != 0 {
				t.Errorf("after the claim expired: %d items, %v; want none", len(items), err)
			}
		})
	}
}
