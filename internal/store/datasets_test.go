package store

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
)

// Datasets and their version clock (spec 014, Testing — store): what a write
// changes, what it leaves alone, and that "the dataset at version V" is
// recoverable for every V the history ever had.

func itemInput(id, input string) *DatasetItemInput {
	return &DatasetItemInput{ID: id, Input: []byte(input)}
}

func itemID(n int) string { return fmt.Sprintf("%032x", 0xd000+n) }

// postItems submits one batch and returns the write with its outcome.
func (f *sweepFixture) postItems(t *testing.T, dataset string, items ...*DatasetItemInput) *DatasetItemsWrite {
	t.Helper()
	write := &DatasetItemsWrite{ProjectID: f.project.ID, Dataset: dataset, Items: items, Now: sweepNow.UnixNano()}
	if err := f.writer.Submit(t.Context(), write); err != nil {
		t.Fatal(err)
	}
	return write
}

// itemsAt reads the ids of the live items at a version, in listing order.
func (f *sweepFixture) itemsAt(t *testing.T, dataset string, version int) []string {
	t.Helper()
	items, err := f.store.DatasetItems(f.project.ID, dataset, DatasetItemFilter{Version: version, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func (f *sweepFixture) datasetVersion(t *testing.T, name string) int {
	t.Helper()
	dataset, err := f.store.Dataset(f.project.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	if dataset == nil {
		t.Fatalf("dataset %q does not exist", name)
	}
	return dataset.Version
}

// TestDatasetVersionClock is Decisions 5 and 6 end to end: a batch is one
// tick, a batch that changes nothing is none, an edit is one row, and an
// archive hides at the new version while showing at the old.
func TestDatasetVersionClock(t *testing.T) {
	f := newSweepFixture(t)
	const name = "golden"
	three := []*DatasetItemInput{
		itemInput(itemID(1), `{"q":"one"}`),
		itemInput(itemID(2), `{"q":"two"}`),
		itemInput(itemID(3), `{"q":"three"}`),
	}

	first := f.postItems(t, name, three...)
	if first.Version != 1 || first.Changed != 3 {
		t.Fatalf("first batch: version %d changed %d, want version 1 with 3 changed", first.Version, first.Changed)
	}

	again := f.postItems(t, name, three...)
	if again.Version != 1 || again.Changed != 0 {
		t.Errorf("re-post: version %d changed %d, want the same version and nothing changed", again.Version, again.Changed)
	}
	// Key order is not a change: the comparison is on compacted JSON.
	reordered := f.postItems(t, name,
		&DatasetItemInput{ID: itemID(1), Input: []byte(`{"q":"one"}`), Metadata: []byte(`{"b":1,"a":2}`)})
	if reordered.Version != 2 || reordered.Changed != 1 {
		t.Fatalf("adding metadata: version %d changed %d, want 2 and 1", reordered.Version, reordered.Changed)
	}
	sameOtherOrder := f.postItems(t, name,
		&DatasetItemInput{ID: itemID(1), Input: []byte(`{"q":"one"}`), Metadata: []byte(`{"b":1,"a":2}`)})
	if sameOtherOrder.Changed != 0 {
		t.Errorf("the same metadata again counted as a change")
	}

	// Item 1 is re-posted as it now is — with its metadata — so only the
	// revised item counts; posting it without the metadata would be an
	// edit too, and the right one to count.
	withMetadata := &DatasetItemInput{ID: itemID(1), Input: []byte(`{"q":"one"}`), Metadata: []byte(`{"b":1,"a":2}`)}
	edit := f.postItems(t, name, withMetadata, itemInput(itemID(2), `{"q":"two, revised"}`), three[2])
	if edit.Version != 3 || edit.Changed != 1 {
		t.Errorf("editing one of three: version %d changed %d, want 3 and 1", edit.Version, edit.Changed)
	}
	if rows := f.count(t, `SELECT COUNT(*) FROM dataset_items WHERE dataset = ?`, name); rows != 5 {
		t.Errorf("rows = %d, want 3 originals + 1 metadata edit + 1 body edit", rows)
	}
	old, err := f.store.DatasetItem(f.project.ID, name, itemID(2), 2)
	if err != nil {
		t.Fatal(err)
	}
	if old == nil || string(old.Input) != `{"q":"two"}` || old.Version != 1 {
		t.Errorf("item 2 at version 2 = %+v, want the original body written at version 1", old)
	}
	current, err := f.store.DatasetItem(f.project.ID, name, itemID(2), 3)
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || string(current.Input) != `{"q":"two, revised"}` {
		t.Errorf("item 2 at version 3 = %+v, want the edit", current)
	}

	archive := &DatasetItemArchive{ProjectID: f.project.ID, Dataset: name, ItemID: itemID(3), Now: sweepNow.UnixNano()}
	if err := f.writer.Submit(t.Context(), archive); err != nil {
		t.Fatal(err)
	}
	if archive.Version != 4 {
		t.Errorf("archive landed on version %d, want 4", archive.Version)
	}
	if got := f.itemsAt(t, name, 4); !slices.Equal(got, []string{itemID(1), itemID(2)}) {
		t.Errorf("items at version 4 = %v, want the archived one hidden", got)
	}
	if got := f.itemsAt(t, name, 3); !slices.Equal(got, []string{itemID(1), itemID(2), itemID(3)}) {
		t.Errorf("items at version 3 = %v, want the archived one still there", got)
	}
	// Archiving again, or archiving a stranger, is a 404 in kind.
	if err := f.writer.Submit(t.Context(), &DatasetItemArchive{
		ProjectID: f.project.ID, Dataset: name, ItemID: itemID(3), Now: sweepNow.UnixNano()}); !rejected(err) {
		t.Errorf("archiving an archived item: err = %v, want a rejection", err)
	}
	if err := f.writer.Submit(t.Context(), &DatasetItemArchive{
		ProjectID: f.project.ID, Dataset: name, ItemID: itemID(99), Now: sweepNow.UnixNano()}); !rejected(err) {
		t.Errorf("archiving an unknown item: err = %v, want a rejection", err)
	}
	if got := f.datasetVersion(t, name); got != 4 {
		t.Errorf("a refused archive moved the version to %d", got)
	}
	dataset, err := f.store.Dataset(f.project.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	if dataset.ItemCount != 2 {
		t.Errorf("item_count = %d, want the two live items", dataset.ItemCount)
	}
}

// TestItemsAtEveryVersion scripts a history and checks the resolved set at
// every version against what the script says it should be.
func TestItemsAtEveryVersion(t *testing.T) {
	f := newSweepFixture(t)
	const name = "history"

	type step struct {
		post    []*DatasetItemInput
		archive string
		want    []string
	}
	script := []step{
		{post: []*DatasetItemInput{itemInput(itemID(1), `1`), itemInput(itemID(2), `2`)}, want: []string{itemID(1), itemID(2)}},
		{post: []*DatasetItemInput{itemInput(itemID(3), `3`)}, want: []string{itemID(1), itemID(2), itemID(3)}},
		{archive: itemID(1), want: []string{itemID(2), itemID(3)}},
		{post: []*DatasetItemInput{itemInput(itemID(2), `2b`), itemInput(itemID(4), `4`)}, want: []string{itemID(2), itemID(3), itemID(4)}},
		// The archived item returns to its place (edge cases).
		{post: []*DatasetItemInput{itemInput(itemID(1), `1b`)}, want: []string{itemID(1), itemID(2), itemID(3), itemID(4)}},
		{archive: itemID(3), want: []string{itemID(1), itemID(2), itemID(4)}},
	}
	expected := map[int][]string{0: {}}
	for i, s := range script {
		if s.archive != "" {
			if err := f.writer.Submit(t.Context(), &DatasetItemArchive{
				ProjectID: f.project.ID, Dataset: name, ItemID: s.archive, Now: sweepNow.UnixNano()}); err != nil {
				t.Fatal(err)
			}
		} else {
			f.postItems(t, name, s.post...)
		}
		if got := f.datasetVersion(t, name); got != i+1 {
			t.Fatalf("after step %d the version is %d, want %d", i, got, i+1)
		}
		expected[i+1] = s.want
	}
	for version := 0; version <= len(script); version++ {
		if got := f.itemsAt(t, name, version); !slices.Equal(got, expected[version]) {
			t.Errorf("items at version %d = %v, want %v", version, got, expected[version])
		}
	}

	// And an item's history is every row of it, newest first, the
	// archived one included.
	history, err := f.store.DatasetItemVersions(f.project.ID, name, itemID(1))
	if err != nil {
		t.Fatal(err)
	}
	var versions []int
	var archived []bool
	for _, row := range history {
		versions = append(versions, row.Version)
		archived = append(archived, row.Archived)
	}
	if !slices.Equal(versions, []int{5, 3, 1}) || !slices.Equal(archived, []bool{false, true, false}) {
		t.Errorf("history of item 1 = versions %v archived %v, want [5 3 1] with the middle one archived", versions, archived)
	}
}

// TestSeqIsStableAcrossEdits is Decision 21: an item's place in the listing
// is fixed at its first appearance, survives every edit, and survives an
// archive followed by a re-post.
func TestSeqIsStableAcrossEdits(t *testing.T) {
	f := newSweepFixture(t)
	const name = "ordered"
	f.postItems(t, name, itemInput(itemID(1), `1`), itemInput(itemID(2), `2`), itemInput(itemID(3), `3`))
	f.postItems(t, name, itemInput(itemID(1), `1b`))
	if err := f.writer.Submit(t.Context(), &DatasetItemArchive{
		ProjectID: f.project.ID, Dataset: name, ItemID: itemID(2), Now: sweepNow.UnixNano()}); err != nil {
		t.Fatal(err)
	}
	f.postItems(t, name, itemInput(itemID(4), `4`), itemInput(itemID(2), `2b`))

	items, err := f.store.DatasetItems(f.project.ID, name, DatasetItemFilter{Version: 4, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	var seqs []int64
	for _, item := range items {
		order = append(order, item.ID)
		seqs = append(seqs, item.Seq)
	}
	if !slices.Equal(order, []string{itemID(1), itemID(2), itemID(3), itemID(4)}) {
		t.Errorf("order = %v, want first-appearance order with the re-posted item back in its place", order)
	}
	if !slices.Equal(seqs, []int64{0, 1, 2, 3}) {
		t.Errorf("seq = %v, want 0..3 in first-appearance order", seqs)
	}
}

// TestConcurrentItemWritesGetConsecutiveVersions is spec 003 #10's
// concurrency test re-aimed at the version clock: writers racing on one
// dataset get consecutive numbers with no gaps and no duplicates, because the
// number is assigned inside the write transaction.
func TestConcurrentItemWritesGetConsecutiveVersions(t *testing.T) {
	f := newSweepFixture(t)
	const name = "raced"
	const writers = 24

	var wg sync.WaitGroup
	versions := make(chan int, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			write := &DatasetItemsWrite{
				ProjectID: f.project.ID, Dataset: name, Now: sweepNow.UnixNano(),
				Items: []*DatasetItemInput{itemInput(itemID(100+i), fmt.Sprintf(`%d`, i))},
			}
			if err := f.writer.Submit(t.Context(), write); err != nil {
				t.Error(err)
				return
			}
			versions <- write.Version
		}()
	}
	wg.Wait()
	close(versions)

	var got []int
	for v := range versions {
		got = append(got, v)
	}
	slices.Sort(got)
	want := make([]int, 0, writers)
	for i := 1; i <= writers; i++ {
		want = append(want, i)
	}
	if !slices.Equal(got, want) {
		t.Errorf("versions = %v, want 1..%d with no gaps", got, writers)
	}
	if n := len(f.itemsAt(t, name, writers)); n != writers {
		t.Errorf("items at the last version = %d, want every writer's", n)
	}
}

// TestDatasetEnvelopeAndDeletion: PUT touches the envelope and not the clock,
// the dry run counts, the echo gates the delete, and the cascade takes items
// and runs but leaves traces.
func TestDatasetEnvelopeAndDeletion(t *testing.T) {
	f := newSweepFixture(t)
	const name = "envelope"
	f.postItems(t, name, itemInput(itemID(1), `1`))

	upsert := &DatasetUpsert{ProjectID: f.project.ID, Name: name, Description: "the golden set",
		Metadata: []byte(`{"owner":"qa"}`), Now: sweepNow.UnixNano()}
	if err := f.writer.Submit(t.Context(), upsert); err != nil {
		t.Fatal(err)
	}
	if upsert.Dataset.Version != 1 || upsert.Dataset.Description != "the golden set" || upsert.Dataset.ItemCount != 1 {
		t.Errorf("after PUT: %+v, want the description set and the version untouched", upsert.Dataset)
	}

	run := &RunCreate{ProjectID: f.project.ID, Dataset: name, ID: strings.Repeat("a", 32), Now: sweepNow.UnixNano()}
	if err := f.writer.Submit(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1), func(tr *modelTrace) { tr.RunID = run.ID })

	dataset, counts, err := f.store.DatasetPreview(f.project.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	if dataset == nil || counts != (DatasetCounts{Items: 1, Runs: 1, PinnedTraces: 1}) {
		t.Errorf("preview = %+v, want one item, one run, one pinned trace", counts)
	}

	wrong := &DatasetDelete{ProjectID: f.project.ID, Name: name, Confirm: "envelop"}
	if err := f.writer.Submit(t.Context(), wrong); !rejected(err) {
		t.Fatalf("a wrong echo: err = %v, want a rejection", err)
	}
	if d, _ := f.store.Dataset(f.project.ID, name); d == nil {
		t.Fatal("a wrong echo deleted the dataset")
	}
	deletion := &DatasetDelete{ProjectID: f.project.ID, Name: name, Confirm: name}
	if err := f.writer.Submit(t.Context(), deletion); err != nil {
		t.Fatal(err)
	}
	if deletion.Counts != counts {
		t.Errorf("deleted = %+v, want the preview's %+v", deletion.Counts, counts)
	}
	for _, table := range []string{"datasets", "dataset_items", "dataset_runs"} {
		if got := f.count(t, `SELECT COUNT(*) FROM `+table); got != 0 {
			t.Errorf("%s = %d after the delete, want the cascade to have taken it", table, got)
		}
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces WHERE run_id = ?`, run.ID); got != 1 {
		t.Errorf("the trace went with the dataset; it must only be released")
	}
}

// TestRunLifecycleInTheStore: create pins the version, a known id is returned
// unchanged, a version above the current is refused, finish closes once, and
// delete reports what it released.
func TestRunLifecycleInTheStore(t *testing.T) {
	f := newSweepFixture(t)
	const name = "runs"
	f.postItems(t, name, itemInput(itemID(1), `1`))
	f.postItems(t, name, itemInput(itemID(2), `2`))
	id := strings.Repeat("b", 32)

	create := &RunCreate{ProjectID: f.project.ID, Dataset: name, ID: id, Name: "v7", Now: sweepNow.UnixNano()}
	if err := f.writer.Submit(t.Context(), create); err != nil {
		t.Fatal(err)
	}
	if create.Existed || create.Run.DatasetVersion != 2 || create.Run.Status != RunRunning {
		t.Errorf("created run = %+v existed=%v, want a running run pinned at version 2", create.Run, create.Existed)
	}
	again := &RunCreate{ProjectID: f.project.ID, Dataset: name, ID: id, Name: "renamed", Now: sweepNow.UnixNano() + 1}
	if err := f.writer.Submit(t.Context(), again); err != nil {
		t.Fatal(err)
	}
	if !again.Existed || again.Run.Name != "v7" {
		t.Errorf("re-create = %+v existed=%v, want the existing run unchanged", again.Run, again.Existed)
	}
	older := 1
	pinned := &RunCreate{ProjectID: f.project.ID, Dataset: name, ID: strings.Repeat("c", 32), DatasetVersion: &older, Now: sweepNow.UnixNano()}
	if err := f.writer.Submit(t.Context(), pinned); err != nil {
		t.Fatal(err)
	}
	if pinned.Run.DatasetVersion != 1 {
		t.Errorf("explicit pin = %d, want 1", pinned.Run.DatasetVersion)
	}
	future := 3
	if err := f.writer.Submit(t.Context(), &RunCreate{ProjectID: f.project.ID, Dataset: name,
		ID: strings.Repeat("d", 32), DatasetVersion: &future, Now: sweepNow.UnixNano()}); !rejected(err) {
		t.Errorf("a pin above the current version: err = %v, want a rejection", err)
	}
	if err := f.writer.Submit(t.Context(), &RunCreate{ProjectID: f.project.ID, Dataset: "nope",
		ID: strings.Repeat("e", 32), Now: sweepNow.UnixNano()}); !rejected(err) {
		t.Errorf("a run on an unknown dataset: err = %v, want a rejection", err)
	}

	f.arrive(t, f.project.ID, hexTrace(1), daysAgo(1), func(tr *modelTrace) { tr.RunID = id })
	f.arrive(t, f.project.ID, hexTrace(2), daysAgo(1), func(tr *modelTrace) { tr.RunID = id })

	finish := &RunFinish{ProjectID: f.project.ID, ID: id, Status: RunFailed, Error: "judge timed out", Now: sweepNow.UnixNano()}
	if err := f.writer.Submit(t.Context(), finish); err != nil {
		t.Fatal(err)
	}
	if finish.Run.Status != RunFailed || finish.Run.Error != "judge timed out" || finish.Run.FinishedAt == 0 {
		t.Errorf("finished run = %+v", finish.Run)
	}
	if err := f.writer.Submit(t.Context(), &RunFinish{ProjectID: f.project.ID, ID: id, Status: RunFinished, Now: sweepNow.UnixNano()}); !rejected(err) {
		t.Errorf("finishing twice: err = %v, want a rejection", err)
	}

	deletion := &RunDelete{ProjectID: f.project.ID, ID: id}
	if err := f.writer.Submit(t.Context(), deletion); err != nil {
		t.Fatal(err)
	}
	if deletion.Released != 2 {
		t.Errorf("released = %d, want the two traces the run held", deletion.Released)
	}
	if run, _ := f.store.Run(f.project.ID, id); run != nil {
		t.Errorf("the run is still there after its delete")
	}
	if got := f.count(t, `SELECT COUNT(*) FROM traces WHERE run_id = ?`, id); got != 2 {
		t.Errorf("traces = %d, want both kept: a run's delete releases, it does not delete", got)
	}
}

// TestScoreConfigPutIsDeclarative: a re-PUT of the same body writes nothing,
// a different one replaces the row, and delete of a stranger is a rejection.
func TestScoreConfigPutIsDeclarative(t *testing.T) {
	f := newSweepFixture(t)
	config := func() *ScoreConfig {
		return &ScoreConfig{Name: "accuracy", DataType: ScoreNumeric, Direction: DirectionHigher,
			Min: floatPtr(0), Max: floatPtr(1), Description: "judge"}
	}
	first := &ScoreConfigPut{ProjectID: f.project.ID, Config: config(), Now: 1000}
	if err := f.writer.Submit(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	same := &ScoreConfigPut{ProjectID: f.project.ID, Config: config(), Now: 2000}
	if err := f.writer.Submit(t.Context(), same); err != nil {
		t.Fatal(err)
	}
	if same.Stored.UpdatedAt != 1000 {
		t.Errorf("updated_at = %d after an identical PUT, want it untouched", same.Stored.UpdatedAt)
	}
	changed := config()
	changed.Max = floatPtr(10)
	replaced := &ScoreConfigPut{ProjectID: f.project.ID, Config: changed, Now: 3000}
	if err := f.writer.Submit(t.Context(), replaced); err != nil {
		t.Fatal(err)
	}
	if replaced.Stored.UpdatedAt != 3000 || *replaced.Stored.Max != 10 || replaced.Stored.CreatedAt != 1000 {
		t.Errorf("replaced = %+v, want the new max, a new updated_at and the original created_at", replaced.Stored)
	}
	if err := f.writer.Submit(t.Context(), &ScoreConfigDelete{ProjectID: f.project.ID, Name: "nope"}); !rejected(err) {
		t.Errorf("deleting an unknown config: err = %v, want a rejection", err)
	}
	if err := f.writer.Submit(t.Context(), &ScoreConfigDelete{ProjectID: f.project.ID, Name: "accuracy"}); err != nil {
		t.Fatal(err)
	}
	if c, _ := f.store.ScoreConfig(f.project.ID, "accuracy"); c != nil {
		t.Errorf("the config is still there after its delete")
	}
}
