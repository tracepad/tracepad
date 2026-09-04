package store

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
)

// Reading an eval back (spec 014, Testing — summary and compare): the summary
// of a seeded run against hand-computed truth, the item view with its
// attempts, and the union a comparison walks.

// attempt describes one trace of a run for the fixture below: which item it
// answered, what it cost, how long it took and whether it failed.
type attempt struct {
	trace     string
	item      string
	latencyMs int64
	cost      float64
	failed    bool
	model     string
	prompt    string
	output    any
	scores    []*Score
}

// seedRun writes a run over `dataset` at its current version and the traces
// the attempts describe. Every number the summary reports comes from these
// rows, so a test can state the truth in its own arithmetic.
func (f *sweepFixture) seedRun(t *testing.T, dataset, runID string, attempts []attempt) *DatasetRun {
	t.Helper()
	create := &RunCreate{ProjectID: f.project.ID, Dataset: dataset, ID: runID, Now: sweepNow.UnixNano()}
	if err := f.writer.Submit(t.Context(), create); err != nil {
		t.Fatal(err)
	}
	for _, a := range attempts {
		f.seedAttempt(t, runID, a)
	}
	return create.Run
}

func (f *sweepFixture) seedAttempt(t *testing.T, runID string, a attempt) {
	t.Helper()
	level := model.LevelDefault
	if a.failed {
		level = model.LevelError
	}
	start := sweepNow.UnixNano()
	observation := &model.Observation{
		TraceID: a.trace, ID: a.trace[:16], Type: model.TypeGeneration,
		Name: "answer", Level: level, StartTime: start,
		EndTime: start + a.latencyMs*int64(time.Millisecond),
		Model:   a.model, Output: a.output,
	}
	if a.cost > 0 {
		observation.CostDetails = map[string]any{"total": a.cost}
	}
	if a.prompt != "" {
		version := int64(7)
		observation.PromptName, observation.PromptVersion = a.prompt, &version
	}
	batch := &IngestBatch{
		ProjectID:    f.project.ID,
		IngestedAt:   sweepNow.UnixNano(),
		Traces:       []*model.Trace{{ID: a.trace, RunID: runID, ItemID: a.item}},
		Observations: []*model.Observation{observation},
	}
	if err := f.writer.Submit(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	if len(a.scores) == 0 {
		return
	}
	for i, score := range a.scores {
		// The id is the idempotency key (spec 003 #3), so every score of
		// the fixture needs its own or the later ones replace the
		// earlier ones and every count comes out as 1.
		score.ID = a.trace[8:] + fmt.Sprintf("%08x", i)
		score.TraceID = a.trace
		score.Timestamp = sweepNow.UnixNano()
		score.CreatedAt = sweepNow.UnixNano()
	}
	if err := f.writer.Submit(t.Context(), &ScoreWrite{ProjectID: f.project.ID, Scores: a.scores}); err != nil {
		t.Fatal(err)
	}
}

func numeric(name string, value float64) *Score {
	return &Score{Name: name, DataType: ScoreNumeric, Value: &value}
}

func categorical(name, value string) *Score {
	return &Score{Name: name, DataType: ScoreCategorical, StringValue: &value}
}

// TestRunSummaryIsHandComputable seeds three items, four traces and a handful
// of scores, then states what every field of the summary must be. The point of
// writing the truth out by hand is that a summary computed the wrong way still
// looks plausible.
func TestRunSummaryIsHandComputable(t *testing.T) {
	f := newSweepFixture(t)
	f.postItems(t, "golden",
		itemInput(itemID(1), `{"q":"one"}`),
		itemInput(itemID(2), `{"q":"two"}`),
		itemInput(itemID(3), `{"q":"three"}`))

	run := f.seedRun(t, "golden", strings.Repeat("a", 32), []attempt{
		// Item 1, attempted twice: 1.0 and 0.5 average to 0.75.
		{trace: hexTrace(1), item: itemID(1), latencyMs: 100, cost: 0.01, model: "gpt-5",
			prompt: "answer", scores: []*Score{numeric("accuracy", 1), categorical("verdict", "pass")}},
		{trace: hexTrace(2), item: itemID(1), latencyMs: 300, cost: 0.02, model: "gpt-5",
			scores: []*Score{numeric("accuracy", 0.5), categorical("verdict", "fail")}},
		// Item 2, attempted once and failed, with no cost at all.
		{trace: hexTrace(3), item: itemID(2), latencyMs: 900, failed: true, model: "gpt-5-mini",
			scores: []*Score{numeric("accuracy", 0)}},
		// A trace of an item the dataset does not have, and one that named
		// the run and no item at all. Both are unaccounted traffic, and
		// Decision 29 counts both.
		{trace: hexTrace(4), item: itemID(9), latencyMs: 200, model: "gpt-5"},
		{trace: hexTrace(5), latencyMs: 200, model: "gpt-5"},
	})

	summary, err := f.store.RunSummary(f.project.ID, run)
	if err != nil {
		t.Fatal(err)
	}
	wantItems := RunItemCounts{Total: 3, Covered: 2, Missing: 1, Unknown: 2}
	if summary.Items != wantItems {
		t.Errorf("items = %+v, want %+v", summary.Items, wantItems)
	}
	if summary.Traces.Count != 5 || summary.Traces.AttemptsMax != 2 || summary.Traces.ErrorCount != 1 {
		t.Errorf("traces = %+v, want 5 traces, 2 attempts at most, 1 failed", summary.Traces)
	}
	if got := summary.Traces.TotalCost; got == nil || *got != 0.03 {
		t.Errorf("total_cost = %v, want the two attempts that carried one", got)
	}
	// Latencies sorted: 100, 200, 200, 300, 900. Nearest rank puts p50 at
	// the 3rd and p95 at the 5th.
	if got := summary.Traces.LatencyP50; got == nil || *got != 200 {
		t.Errorf("p50 = %v, want 200", got)
	}
	if got := summary.Traces.LatencyP95; got == nil || *got != 900 {
		t.Errorf("p95 = %v, want 900", got)
	}
	if want := []string{"gpt-5", "gpt-5-mini"}; !slices.Equal(summary.Models, want) {
		t.Errorf("models = %v, want %v", summary.Models, want)
	}
	if len(summary.Prompts) != 1 || summary.Prompts[0].Name != "answer" {
		t.Errorf("prompts = %+v, want the one prompt an observation ran", summary.Prompts)
	}

	byName := map[string]RunScoreStat{}
	for _, stat := range summary.Scores {
		byName[stat.Name] = stat
	}
	accuracy := byName["accuracy"]
	if accuracy.Count != 3 || accuracy.Mean == nil || *accuracy.Mean != 0.5 {
		t.Errorf("accuracy = %+v, want three scores averaging 0.5", accuracy)
	}
	if accuracy.Min == nil || *accuracy.Min != 0 || accuracy.Max == nil || *accuracy.Max != 1 {
		t.Errorf("accuracy range = %v..%v, want 0..1", accuracy.Min, accuracy.Max)
	}
	verdict := byName["verdict"]
	if verdict.Distribution["pass"] != 1 || verdict.Distribution["fail"] != 1 {
		t.Errorf("verdict distribution = %v, want one of each", verdict.Distribution)
	}
	if verdict.Mean != nil {
		t.Errorf("a categorical name reported a mean: %v", *verdict.Mean)
	}
}

// TestRunSummaryTakesDirectionFromTheConfig: the summary reports what a name
// means, not merely what its numbers were, and the meaning lives in the config
// (#15, #16) — which is what lets a comparison say `improved` at all.
func TestRunSummaryTakesDirectionFromTheConfig(t *testing.T) {
	f := newSweepFixture(t)
	f.postItems(t, "golden", itemInput(itemID(1), `1`))
	if err := f.writer.Submit(t.Context(), &ScoreConfigPut{
		ProjectID: f.project.ID,
		Config:    &ScoreConfig{Name: "accuracy", DataType: ScoreNumeric, Direction: DirectionHigher},
		Now:       sweepNow.UnixNano(),
	}); err != nil {
		t.Fatal(err)
	}
	run := f.seedRun(t, "golden", strings.Repeat("a", 32), []attempt{
		{trace: hexTrace(1), item: itemID(1), latencyMs: 10,
			scores: []*Score{numeric("accuracy", 1), numeric("helpfulness", 1)}},
	})

	summary, err := f.store.RunSummary(f.project.ID, run)
	if err != nil {
		t.Fatal(err)
	}
	for _, stat := range summary.Scores {
		want := ""
		if stat.Name == "accuracy" {
			want = DirectionHigher
		}
		if stat.Direction != want {
			t.Errorf("%s direction = %q, want %q", stat.Name, stat.Direction, want)
		}
	}
}

// TestRunItemsCarryTheirAttempts: the item view is the dataset's items in
// `seq` order with the traces that answered them, and `?unknown=true` adds the
// traces no item of the version accounts for — after the known ones, so a
// cursor walk stays a walk.
func TestRunItemsCarryTheirAttempts(t *testing.T) {
	f := newSweepFixture(t)
	f.postItems(t, "golden", itemInput(itemID(1), `1`), itemInput(itemID(2), `2`))
	run := f.seedRun(t, "golden", strings.Repeat("a", 32), []attempt{
		{trace: hexTrace(1), item: itemID(1), latencyMs: 10, output: map[string]any{"a": "one"},
			scores: []*Score{numeric("accuracy", 1)}},
		{trace: hexTrace(2), item: itemID(1), latencyMs: 20, output: map[string]any{"a": "again"}},
		{trace: hexTrace(3), item: itemID(9), latencyMs: 30},
	})

	items, err := f.store.RunItems(f.project.ID, run, RunItemFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want the version's two, without the unknown one", len(items))
	}
	if len(items[0].Attempts) != 2 || len(items[1].Attempts) != 0 {
		t.Fatalf("attempts = %d and %d, want both traces of item 1 and none of item 2",
			len(items[0].Attempts), len(items[1].Attempts))
	}
	first := items[0].Attempts[0]
	if first.TraceID != hexTrace(1) || first.ObservationID != hexTrace(1)[:16] {
		t.Errorf("first attempt = %s/%s, want the earliest trace and its root observation",
			first.TraceID, first.ObservationID)
	}
	if output, _ := first.Output.(map[string]any); output["a"] != "one" {
		t.Errorf("output = %v, want the root observation's payload", first.Output)
	}
	if len(first.Scores) != 1 || first.Scores[0].Name != "accuracy" {
		t.Errorf("scores = %+v, want the one score the attempt got", first.Scores)
	}

	withUnknown, err := f.store.RunItems(f.project.ID, run, RunItemFilter{Limit: 10, IncludeUnknown: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(withUnknown) != 3 || withUnknown[2].Item != nil || withUnknown[2].ItemID != itemID(9) {
		t.Fatalf("with unknown = %d rows, want the unknown item last", len(withUnknown))
	}
	if len(withUnknown[2].Attempts) != 1 {
		t.Errorf("the unknown item lost its trace: %+v", withUnknown[2])
	}
}

// TestRunItemsWalkBothWays pages the view one row at a time, across the two
// buckets, and asserts the walk sees every row once in the same order in both
// directions (spec 009 #2, applied to a listing made of two kinds of row).
func TestRunItemsWalkBothWays(t *testing.T) {
	f := newSweepFixture(t)
	f.postItems(t, "golden", itemInput(itemID(1), `1`), itemInput(itemID(2), `2`))
	run := f.seedRun(t, "golden", strings.Repeat("a", 32), []attempt{
		{trace: hexTrace(1), item: itemID(1), latencyMs: 10},
		{trace: hexTrace(2), item: itemID(9), latencyMs: 10},
	})

	walk := func(backward bool) []string {
		var (
			seen   []string
			cursor *RunItemCursor
		)
		for range 10 {
			page, err := f.store.RunItems(f.project.ID, run, RunItemFilter{
				Limit: 1, After: cursor, Backward: backward, IncludeUnknown: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(page) == 0 {
				break
			}
			seen = append(seen, page[0].ItemID)
			key := RunItemKey(page[0])
			cursor = &key
		}
		return seen
	}
	forward := walk(false)
	want := []string{itemID(1), itemID(2), itemID(9)}
	if !slices.Equal(forward, want) {
		t.Errorf("forward walk = %v, want %v", forward, want)
	}
	backward := walk(true)
	slices.Reverse(backward)
	if !slices.Equal(backward, want) {
		t.Errorf("backward walk = %v, want %v", backward, want)
	}
}

// TestCompareItemsIsTheUnionOfWhatWasRun: the comparison walks the items of
// both versions that at least one run attempted, and says which version each
// belongs to — the labels a reader needs when the dataset moved between the
// two runs.
func TestCompareItemsIsTheUnionOfWhatWasRun(t *testing.T) {
	f := newSweepFixture(t)
	f.postItems(t, "golden", itemInput(itemID(1), `1`), itemInput(itemID(2), `2`))
	first := f.seedRun(t, "golden", strings.Repeat("a", 32), []attempt{
		{trace: hexTrace(1), item: itemID(1), latencyMs: 10, scores: []*Score{numeric("accuracy", 0.5)}},
		{trace: hexTrace(2), item: itemID(2), latencyMs: 10},
	})
	// The dataset moves on: item 2 is archived and item 3 arrives.
	if err := f.writer.Submit(t.Context(), &DatasetItemArchive{
		ProjectID: f.project.ID, Dataset: "golden", ItemID: itemID(2), Now: sweepNow.UnixNano()}); err != nil {
		t.Fatal(err)
	}
	f.postItems(t, "golden", itemInput(itemID(3), `3`))
	second := f.seedRun(t, "golden", strings.Repeat("b", 32), []attempt{
		{trace: hexTrace(3), item: itemID(1), latencyMs: 10, scores: []*Score{numeric("accuracy", 1)}},
		{trace: hexTrace(4), item: itemID(3), latencyMs: 10},
	})

	items, err := f.store.CompareItems(f.project.ID, first, second)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		id       string
		inA, inB bool
	}{
		{itemID(1), true, true},
		{itemID(2), true, false},
		{itemID(3), false, true},
	}
	if len(items) != len(want) {
		t.Fatalf("compared items = %d, want %d", len(items), len(want))
	}
	for i, expected := range want {
		got := items[i]
		if got.ItemID != expected.id || got.InVersionA != expected.inA || got.InVersionB != expected.inB {
			t.Errorf("item %d = %+v, want %+v", i, got, expected)
		}
	}

	values, err := f.store.RunValues(f.project.ID, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if score := values.Scores[itemID(1)]["accuracy"]; score.Mean == nil || *score.Mean != 1 {
		t.Errorf("item value = %+v, want the second run's 1", score)
	}
	if !values.Attempted[itemID(3)] {
		t.Errorf("an item with no scores is not attempted: %v", values.Attempted)
	}
}

// TestItemValueIsTheMeanOfItsAttempts: an item answered N times has one value,
// and it is the mean — the same rule as the run mean, so the item rows and the
// header cannot disagree (#18).
func TestItemValueIsTheMeanOfItsAttempts(t *testing.T) {
	f := newSweepFixture(t)
	f.postItems(t, "golden", itemInput(itemID(1), `1`))
	run := f.seedRun(t, "golden", strings.Repeat("a", 32), []attempt{
		{trace: hexTrace(1), item: itemID(1), latencyMs: 10,
			scores: []*Score{numeric("accuracy", 1), categorical("verdict", "pass")}},
		{trace: hexTrace(2), item: itemID(1), latencyMs: 10,
			scores: []*Score{numeric("accuracy", 0), categorical("verdict", "fail")}},
	})

	values, err := f.store.RunValues(f.project.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	accuracy := values.Scores[itemID(1)]["accuracy"]
	if accuracy.Count != 2 || accuracy.Mean == nil || *accuracy.Mean != 0.5 {
		t.Errorf("accuracy = %+v, want the mean of 1 and 0", accuracy)
	}
	// A word has no mean, so the newest attempt speaks for the item.
	if verdict := values.Scores[itemID(1)]["verdict"]; verdict.Text != "fail" {
		t.Errorf("verdict = %q, want the newest attempt's word", verdict.Text)
	}

	summary, err := f.store.RunSummary(f.project.ID, run)
	if err != nil {
		t.Fatal(err)
	}
	for _, stat := range summary.Scores {
		if stat.Name == "accuracy" && (stat.Mean == nil || *stat.Mean != 0.5) {
			t.Errorf("the header mean %v disagrees with the item mean 0.5", stat.Mean)
		}
	}
}

// One name, two data types, one item: the mean is over the rows that carried a
// number. #15 lets a name that already carried one type accept another, so a
// run can hold both (Decision 30) — and a divisor that counted the wordy rows
// too would halve the item's value while the header above it, which SQL's
// `AVG` takes over the numeric rows only, stayed right. The item rows must sum
// to the header (#18), and that is the invariant this states (found in review
// of PR #31).
func TestItemMeanCountsOnlyTheNumbers(t *testing.T) {
	f := newSweepFixture(t)
	f.postItems(t, "golden", itemInput(itemID(1), `1`))
	// The wordy attempt comes first, which is where a divisor counting it
	// does damage: last, its row lands after the mean is already right.
	run := f.seedRun(t, "golden", strings.Repeat("b", 32), []attempt{
		{trace: hexTrace(1), item: itemID(1), latencyMs: 10,
			scores: []*Score{categorical("accuracy", "pass")}},
		{trace: hexTrace(2), item: itemID(1), latencyMs: 10,
			scores: []*Score{numeric("accuracy", 1)}},
		{trace: hexTrace(3), item: itemID(1), latencyMs: 10,
			scores: []*Score{numeric("accuracy", 0)}},
	})

	values, err := f.store.RunValues(f.project.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	accuracy := values.Scores[itemID(1)]["accuracy"]
	if accuracy.Mean == nil {
		t.Fatalf("accuracy = %+v, want the mean of its two numbers", accuracy)
	}
	if *accuracy.Mean != 0.5 {
		t.Errorf("mean = %v, want the mean of 1 and 0 — the wordy row is not a zero",
			*accuracy.Mean)
	}

	summary, err := f.store.RunSummary(f.project.ID, run)
	if err != nil {
		t.Fatal(err)
	}
	for _, stat := range summary.Scores {
		if stat.Name != "accuracy" {
			continue
		}
		if stat.Mean == nil || *stat.Mean != *accuracy.Mean {
			t.Errorf("header mean %v disagrees with the item mean %v", stat.Mean, *accuracy.Mean)
		}
	}
}

// TestRunListingsSeekTheirIndex holds both run listings to the index built
// for each: the project-wide one (spec 016 #2) to `idx_dataset_runs_created`,
// the per-dataset one to spec 014's `idx_dataset_runs_dataset` — seeking,
// with the cursor and without, and never sorting through a temporary B-tree.
func TestRunListingsSeekTheirIndex(t *testing.T) {
	f := newSweepFixture(t)
	cursor := &RunCursor{CreatedAt: sweepNow.UnixNano(), ID: strings.Repeat("a", 32)}
	for _, tc := range []struct {
		name   string
		filter RunFilter
		index  string
	}{
		{"project-wide", RunFilter{Limit: 50}, "idx_dataset_runs_created"},
		{"project-wide after a cursor", RunFilter{Limit: 50, After: cursor}, "idx_dataset_runs_created"},
		{"project-wide backward", RunFilter{Limit: 50, After: cursor, Backward: true}, "idx_dataset_runs_created"},
		{"project-wide by status", RunFilter{Limit: 50, Status: RunRunning}, "idx_dataset_runs_created"},
		{"one dataset", RunFilter{Limit: 50, Dataset: "golden"}, "idx_dataset_runs_dataset"},
		{"one dataset after a cursor", RunFilter{Limit: 50, Dataset: "golden", After: cursor}, "idx_dataset_runs_dataset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			query, args := runsQuery(f.project.ID, tc.filter)
			plan, err := f.store.explainQueryPlan(query, args...)
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(plan, "\n")
			if !strings.Contains(joined, tc.index) {
				t.Errorf("the listing does not use %s:\n%s", tc.index, joined)
			}
			if !strings.Contains(joined, "SEARCH") {
				t.Errorf("the listing scans rather than seeks:\n%s", joined)
			}
			if strings.Contains(joined, "TEMP B-TREE") {
				t.Errorf("the listing sorts through a temporary B-tree:\n%s", joined)
			}
		})
	}
}
