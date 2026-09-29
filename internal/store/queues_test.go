package store

import (
	"strings"
	"testing"
)

// The store half of spec 024: the three indexes the new queries were given,
// and the three paths that take a trace away — the sweep, the erasure and the
// purge — which items must follow (#3).

// queue puts a queue over one config, so a fixture can add items to it.
func (f *sweepFixture) queue(t *testing.T, name string, configs ...string) {
	t.Helper()
	for _, config := range configs {
		if err := f.writer.Submit(t.Context(), &ScoreConfigPut{
			ProjectID: f.project.ID, Now: 1,
			Config: &ScoreConfig{Name: config, DataType: ScoreNumeric, Direction: DirectionHigher},
		}); err != nil {
			t.Fatal(err)
		}
	}
	put := &QueuePut{
		ProjectID: f.project.ID,
		Queue:     &AnnotationQueue{Name: name, ScoreConfigs: configs},
		Now:       1,
	}
	if err := f.writer.Submit(t.Context(), put); err != nil {
		t.Fatal(err)
	}
}

// enqueue adds one trace to a queue and returns the item.
func (f *sweepFixture) enqueue(t *testing.T, queue, traceID string) *AnnotationItem {
	t.Helper()
	add := &QueueItemsAdd{
		ProjectID: f.project.ID, Queue: queue,
		Targets: []QueueTarget{{TraceID: traceID}}, Now: 2,
	}
	if err := f.writer.Submit(t.Context(), add); err != nil {
		t.Fatal(err)
	}
	if len(add.Items) != 1 {
		t.Fatalf("items = %d, want the one added", len(add.Items))
	}
	return add.Items[0]
}

// TestQueueQueriesSeekTheirIndexes is the EXPLAIN check spec 023 asked every
// new query for: this store never runs ANALYZE, so an index that is not
// actually chosen is an index that is not there. `next` and the items listing
// go through `idx_annotation_items_next`, the sweep's delete joins
// `idx_annotation_items_trace`, and the `seq` clock rides
// `idx_annotation_items_seq` — the one this test did not cover on the first
// push, which is how it went out as a scan of the whole queue per add.
func TestQueueQueriesSeekTheirIndexes(t *testing.T) {
	f := newSweepFixture(t)
	seq := int64(7)

	for _, tc := range []struct {
		name   string
		query  string
		args   []any
		index  string
		sorted bool
	}{
		{
			name:  "the items listing",
			index: "idx_annotation_items_next",
		},
		{
			name:  "the items listing after a cursor",
			index: "idx_annotation_items_next",
		},
		{
			name:  "the items listing backward",
			index: "idx_annotation_items_next",
		},
		{
			name: "next, resuming a claim",
			query: `SELECT ` + annotationItemColumns + ` FROM annotation_items
			         WHERE project_id = ? AND queue = ? AND status = ?
			           AND claimed_by = ? AND claimed_until > ?
			         ORDER BY seq LIMIT 1`,
			args:  []any{f.project.ID, "review", ItemPending, "ada", int64(1)},
			index: "idx_annotation_items_next",
		},
		{
			name: "next, taking a free item",
			query: `SELECT ` + annotationItemColumns + ` FROM annotation_items
			         WHERE project_id = ? AND queue = ? AND status = ?
			           AND (claimed_until IS NULL OR claimed_until <= ?)
			         ORDER BY seq LIMIT 1`,
			args:  []any{f.project.ID, "review", ItemPending, int64(1)},
			index: "idx_annotation_items_next",
		},
		{
			name:  "the sweep's delete",
			query: `DELETE FROM annotation_items WHERE project_id = ? AND trace_id IN (?)`,
			args:  []any{f.project.ID, hexTrace(1)},
			index: "idx_annotation_items_trace",
		},
		{
			// The queue's clock, on every add. On `_next` it read one
			// entry per item already queued, because `status` sits
			// between the prefix and `seq` and the largest `seq` is
			// therefore not the last entry of the range (Decision 19).
			name:  "the seq clock",
			query: nextSeqQuery,
			args:  []any{f.project.ID, "review"},
			index: "idx_annotation_items_seq",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query, args := tc.query, tc.args
			if query == "" {
				filter := QueueItemFilter{Status: ItemPending, Limit: 50}
				switch tc.name {
				case "the items listing after a cursor":
					filter.After = &seq
				case "the items listing backward":
					filter.After, filter.Backward = &seq, true
				}
				query, args = itemQuery(f.project.ID, "review", filter)
			}
			plan, err := f.store.explainQueryPlan(query, args...)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(plan, "\n")
			if !strings.Contains(joined, tc.index) {
				t.Errorf("does not use %s:\n%s", tc.index, joined)
			}
			if !strings.Contains(joined, "SEARCH") {
				t.Errorf("scans rather than seeks:\n%s", joined)
			}
			if strings.Contains(joined, "TEMP B-TREE") {
				t.Errorf("sorts through a temporary B-tree:\n%s", joined)
			}
		})
	}
}

// TestNextTakesAClaimThatRanOut is the other half of Decision 5, tested where
// the clock can be moved: a claim expires, and an abandoned tab therefore does
// not hold an item for ever. Ten minutes is the constant; what matters is that
// the comparison is against it and not against "is it claimed at all".
func TestNextTakesAClaimThatRanOut(t *testing.T) {
	f := newSweepFixture(t)
	f.queue(t, "review", "accuracy")
	f.enqueue(t, "review", hexTrace(1))

	at := sweepNow.UnixNano()
	taken := &QueueNext{ProjectID: f.project.ID, Queue: "review", Reviewer: Reviewer{Name: "ada"}, Now: at}
	if err := f.writer.Submit(t.Context(), taken); err != nil {
		t.Fatal(err)
	}
	if taken.Item == nil || taken.Item.ClaimedUntil != at+int64(ClaimTTL) {
		t.Fatalf("item = %+v, want it claimed for the TTL", taken.Item)
	}

	// A second annotator, one second before the claim runs out: nothing to
	// take, and `pending` says the queue is not empty, somebody is on it.
	early := &QueueNext{ProjectID: f.project.ID, Queue: "review", Reviewer: Reviewer{Name: "bob"},
		Now: at + int64(ClaimTTL) - int64(1e9)}
	if err := f.writer.Submit(t.Context(), early); err != nil {
		t.Fatal(err)
	}
	if early.Item != nil || early.Pending != 1 {
		t.Fatalf("next = %+v (pending %d), want a live claim to hold it",
			early.Item, early.Pending)
	}

	// One second after: the item is claimable, and claiming it moves the
	// deadline to the new holder's.
	late := &QueueNext{ProjectID: f.project.ID, Queue: "review", Reviewer: Reviewer{Name: "bob"},
		Now: at + int64(ClaimTTL) + int64(1e9)}
	if err := f.writer.Submit(t.Context(), late); err != nil {
		t.Fatal(err)
	}
	if late.Item == nil || late.Item.ClaimedBy != "bob" {
		t.Fatalf("next after the claim ran out = %+v, want bob to get it", late.Item)
	}
	if late.Item.ClaimedUntil != late.Now+int64(ClaimTTL) {
		t.Errorf("claimed_until = %d, want the new holder's deadline", late.Item.ClaimedUntil)
	}
}

// TestSweepTakesTheItemsOfSweptTraces is #3: an item is a pointer, and the
// pass that deletes a trace deletes what points at it — and nothing else.
func TestSweepTakesTheItemsOfSweptTraces(t *testing.T) {
	f := newSweepFixture(t)
	f.setRetention(t, f.project.ID, days(30), nil)
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(31)) // expired
	f.arrive(t, f.project.ID, hexTrace(2), daysAgo(29)) // inside the window
	f.queue(t, "review", "accuracy")
	f.enqueue(t, "review", hexTrace(1))
	kept := f.enqueue(t, "review", hexTrace(2))

	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}

	if got := f.count(t, `SELECT COUNT(*) FROM annotation_items`); got != 1 {
		t.Fatalf("items = %d, want only the live trace's", got)
	}
	if got := f.count(t,
		`SELECT COUNT(*) FROM annotation_items WHERE id = ?`, kept.ID); got != 1 {
		t.Errorf("the sweep took the item of a trace inside the window")
	}
	// The queue itself is untouched: what expired is the evidence, not the
	// review programme.
	if got := f.count(t, `SELECT COUNT(*) FROM annotation_queues`); got != 1 {
		t.Errorf("queues = %d, want the queue to survive its swept items", got)
	}
}

// TestErasureTakesTheItemsOfTheUsersTraces: spec 005 #7 promises the person's
// data is gone, and a queue row naming their trace is a row about them.
func TestErasureTakesTheItemsOfTheUsersTraces(t *testing.T) {
	f := newSweepFixture(t)
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1))
	f.queue(t, "review", "accuracy")
	f.enqueue(t, "review", hexTrace(1))

	erase := &UserDataErase{ProjectID: f.project.ID, UserID: "u1", Confirm: "u1", Limit: 100}
	if err := f.writer.Submit(t.Context(), erase); err != nil {
		t.Fatal(err)
	}
	if erase.Counts.Traces != 1 {
		t.Fatalf("erased %d traces, want the one", erase.Counts.Traces)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM annotation_items`); got != 0 {
		t.Errorf("items = %d, want the erased trace's item gone with it", got)
	}
}

// TestProjectPurgeLeavesNoQueueRows: both tables hang off the project through
// schema 0014's cascades, the items by way of their queue.
func TestProjectPurgeLeavesNoQueueRows(t *testing.T) {
	f := newSweepFixture(t)
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1))
	f.queue(t, "review", "accuracy")
	f.enqueue(t, "review", hexTrace(1))

	if _, err := f.store.db.Exec(`UPDATE projects SET deleted_at = ? WHERE id = ?`,
		daysAgo(8), f.project.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.sweeper.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"projects", "annotation_queues", "annotation_items"} {
		if got := f.count(t, `SELECT COUNT(*) FROM `+table); got != 0 {
			t.Errorf("%s = %d after the purge, want nothing left", table, got)
		}
	}
}

// TestDeletingAQueueTakesItsItemsAndNoScores is #3 from the other end: the
// list goes, the verdicts stay. They are the product of the work and they are
// attached to the trace.
func TestDeletingAQueueTakesItsItemsAndNoScores(t *testing.T) {
	f := newSweepFixture(t)
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1))
	f.queue(t, "review", "accuracy")
	f.enqueue(t, "review", hexTrace(1))
	value := 1.0
	if err := f.writer.Submit(t.Context(), &ScoreWrite{ProjectID: f.project.ID, Scores: []*Score{{
		ID: strings.Repeat("e", 32), TraceID: hexTrace(1), Name: "accuracy",
		DataType: ScoreNumeric, Value: &value, Timestamp: 1, CreatedAt: 1,
	}}}); err != nil {
		t.Fatal(err)
	}

	deletion := &QueueDelete{ProjectID: f.project.ID, Name: "review", Confirm: "review"}
	if err := f.writer.Submit(t.Context(), deletion); err != nil {
		t.Fatal(err)
	}
	if deletion.Items != 1 {
		t.Errorf("items = %d, want the one it held", deletion.Items)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM annotation_items`); got != 0 {
		t.Errorf("items = %d, want the queue's items gone with it", got)
	}
	if got := f.count(t, `SELECT COUNT(*) FROM scores`); got != 1 {
		t.Errorf("scores = %d, want the verdict to survive the queue", got)
	}
}

// TestQueueSeqIsGaplessPerQueue: `seq` is the queue's own clock, assigned
// inside the write transaction like a prompt version, and one queue's items
// do not push another's along.
func TestQueueSeqIsGaplessPerQueue(t *testing.T) {
	f := newSweepFixture(t)
	f.queue(t, "review", "accuracy")
	f.queue(t, "tone", "accuracy")

	first := f.enqueue(t, "review", hexTrace(1))
	second := f.enqueue(t, "review", hexTrace(2))
	other := f.enqueue(t, "tone", hexTrace(1))

	if first.Seq != 1 || second.Seq != 2 {
		t.Errorf("seq = %d, %d; want 1 then 2", first.Seq, second.Seq)
	}
	if other.Seq != 1 {
		t.Errorf("seq = %d in the second queue, want its own clock to start at 1", other.Seq)
	}
	// `MAX(seq) + 1` is what the data contract says, so removing the last
	// item hands its number to the next one. That is the whole promise:
	// `seq` orders the queue, it does not count what has ever been in it,
	// and the cursor that walks it is the order.
	if err := f.writer.Submit(t.Context(),
		&QueueItemDelete{ProjectID: f.project.ID, Queue: "review", ID: second.ID}); err != nil {
		t.Fatal(err)
	}
	third := f.enqueue(t, "review", hexTrace(3))
	if third.Seq != 2 {
		t.Errorf("seq = %d after the last item was removed, want the contract's MAX + 1", third.Seq)
	}
}
