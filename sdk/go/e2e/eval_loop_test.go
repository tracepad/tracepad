package e2e

import (
	"context"
	"errors"
	"fmt"
	"testing"

	tracepad "github.com/tracepad/tracepad/sdk/go"
)

// The harness against a real binary (spec 033, Testing): declare the
// configs, push the cases, open the run, fetch the items at the version it
// pinned, run each case inside its context, score it, close the run — then
// read back through the API what the store made of it.

var cases = []tracepad.Item{
	{ID: tracepad.ItemID("reset"), Input: map[string]any{"question": "where does a span land?"},
		ExpectedOutput: map[string]any{"answer": "In the trace you are reading."}},
	{ID: tracepad.ItemID("refund"), Input: map[string]any{"question": "what does a run pin?"},
		ExpectedOutput: map[string]any{"answer": "A dataset version."}},
	{ID: tracepad.ItemID("versions"), Input: map[string]any{"question": "how many roots?"},
		ExpectedOutput: map[string]any{"answer": "Two."}},
}

// answer is the application under test: its own span, with a generation
// inside — called twice per case, so that every attempt has two roots.
func answer(ctx context.Context, question string) string {
	ctx, step := tracepad.Span(ctx, "answer-case", tracepad.WithInput(question))
	defer step.End()
	_, call := tracepad.Generation(ctx, "chat", tracepad.WithModel("claude-sonnet-5"), tracepad.WithInput(question))
	cost := 0.0002
	call.End(tracepad.Result{Model: "claude-sonnet-5-2026-08-01", Usage: tracepad.Usage{"input_tokens": 18, "output_tokens": 9},
		Cost: &cost, Output: "In the trace you are reading."})
	return "In the trace you are reading."
}

// aRun is one pass of the whole loop: the run as the store reports it, and
// the traces every attempt produced, by item.
func aRun(t *testing.T, ctx context.Context, name string) (map[string]any, map[string][]string) {
	t.Helper()
	min, max := 0.0, 1.0
	if err := tracepad.ScoreConfigs(ctx, []tracepad.ScoreConfig{
		{Name: "accuracy", DataType: "numeric", Direction: "higher", Min: &min, Max: &max},
		{Name: "verdict", DataType: "categorical", Categories: []string{"pass", "fail"}},
	}); err != nil {
		t.Fatal(err)
	}
	golden := tracepad.NewDataset("support-golden")
	if _, _, err := golden.PutItems(ctx, cases); err != nil {
		t.Fatal(err)
	}
	run, err := golden.Run(ctx, name, tracepad.WithRunMetadata(map[string]any{"prompt": "support-answer@7"}))
	if err != nil {
		t.Fatal(err)
	}
	traces := map[string][]string{}
	for item, err := range golden.Items(ctx, run.DatasetVersion) {
		if err != nil {
			t.Fatal(err)
		}
		itemCtx, attempt := run.Item(ctx, item)
		question, _ := item.Input.(map[string]any)["question"].(string)
		produced := answer(itemCtx, question)
		answer(itemCtx, question+" (again)")
		traces[item.ID] = attempt.Traces()
		score := 0.0
		if produced != "" {
			score = 1
		}
		if err := attempt.Score(ctx, "accuracy", tracepad.WithValue(score)); err != nil {
			t.Fatal(err)
		}
		if err := attempt.Score(ctx, "verdict", tracepad.WithStringValue("pass"), tracepad.WithDataType("categorical")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := run.Finish(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := run.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return got, traces
}

// One item more than a request takes is written in two, against the real
// binary: the package's chunk is the number the server takes (spec 033 #19).
func TestMoreItemsThanARequestTakesAreWrittenInTwo(t *testing.T) {
	serve(t)
	items := make([]tracepad.Item, 10_001)
	for n := range items {
		items[n] = tracepad.Item{Input: map[string]any{"n": n}}
	}
	version, changed, err := tracepad.NewDataset("over-the-cap").PutItems(context.Background(), items)
	if err != nil || version != 2 || changed != 10_001 {
		t.Errorf("version %d, changed %d, err %v; want two writes, two versions", version, changed, err)
	}
}

func TestTheWholeLoop(t *testing.T) {
	s := serve(t)
	ctx := context.Background()

	first, traces := aRun(t, ctx, "prompt v7 / claude-sonnet-5")
	if first["status"] != "finished" || first["dataset"] != "support-golden" {
		t.Fatalf("run = %v", first)
	}
	summary := first["summary"].(map[string]any)
	items := summary["items"].(map[string]any)
	if items["total"] != 3.0 || items["covered"] != 3.0 || items["missing"] != 0.0 {
		t.Errorf("items = %v", items)
	}
	// Two roots per attempt, all counted (spec 014 #2).
	if count := summary["traces"].(map[string]any)["count"]; count != 6.0 {
		t.Errorf("traces.count = %v, want 6", count)
	}
	scores := summary["scores"].(map[string]any)
	if scores["accuracy"].(map[string]any)["count"] != 3.0 || scores["accuracy"].(map[string]any)["direction"] != "higher" {
		t.Errorf("accuracy = %v", scores["accuracy"])
	}
	if fmt.Sprint(scores["verdict"].(map[string]any)["distribution"]) != "map[pass:3]" {
		t.Errorf("verdict = %v", scores["verdict"])
	}
	if fmt.Sprint(summary["models"]) != "[claude-sonnet-5]" {
		t.Errorf("models = %v", summary["models"])
	}

	// Every trace carries the run and the item, on spans the harness never
	// touched; the score went against the attempt's last trace.
	for itemID, ids := range traces {
		if len(ids) != 2 {
			t.Fatalf("item %s produced %d traces, want 2", itemID, len(ids))
		}
		for _, id := range ids {
			stored := s.trace(id)
			if stored["run_id"] != first["id"] || stored["item_id"] != itemID {
				t.Errorf("trace %s links run %v item %v", id, stored["run_id"], stored["item_id"])
			}
		}
		found, _ := s.call("GET", "/api/v1/scores?trace_id="+ids[1], nil)["scores"].([]any)
		if len(found) != 2 {
			t.Errorf("scores on the last trace of %s = %d, want 2", itemID, len(found))
		}
	}

	var rows int
	for _, err := range (&tracepad.Run{ID: first["id"].(string)}).Items(ctx) {
		if err != nil {
			t.Fatal(err)
		}
		rows++
	}
	if rows != 3 {
		t.Errorf("run items = %d", rows)
	}

	// A second run, a failed one, and the comparison the store computes.
	second, _ := aRun(t, ctx, "prompt v8 / claude-sonnet-5")
	verdicts, err := tracepad.Compare(ctx, first["id"].(string), second["id"].(string))
	if err != nil || verdicts["a"].(map[string]any)["id"] != first["id"] {
		t.Errorf("compare = %v, err = %v", verdicts, err)
	}
	if compared := verdicts["items"].([]any); len(compared) != 3 || compared[0].(map[string]any)["in"] != "both" {
		t.Errorf("compared = %v", compared)
	}

	doomed, err := tracepad.NewDataset("support-golden").Run(ctx, "doomed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doomed.Fail(ctx, errors.New("judge timed out")); err != nil {
		t.Fatal(err)
	}
	closed, _ := doomed.Get(ctx)
	if closed["status"] != "failed" || closed["error"] != "judge timed out" {
		t.Errorf("failed run = %v", closed)
	}

	// The same cases again write nothing.
	version, changed, err := tracepad.NewDataset("support-golden").PutItems(ctx, cases)
	if err != nil || changed != 0 || version != int(first["dataset_version"].(float64)) {
		t.Errorf("version %d, changed %d, err %v", version, changed, err)
	}
}
