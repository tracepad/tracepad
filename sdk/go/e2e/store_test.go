package e2e

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	tracepad "github.com/tracepad/tracepad/sdk/go"
)

func TestATracedCallArrivesWhole(t *testing.T) {
	s := serve(t)
	ctx := context.Background()
	s.call("POST", "/api/v1/prompts/support-answer/versions",
		map[string]any{"type": "text", "prompt": "Answer {topic}.", "labels": []string{"production"}})

	support, err := tracepad.Prompt(ctx, "support-answer", tracepad.WithLabel("production"))
	if err != nil {
		t.Fatal(err)
	}
	if support.Version != 1 || support.Compile(map[string]any{"topic": "resets"}).Text != "Answer resets." {
		t.Fatalf("prompt = %+v", support)
	}

	ctx, request := application.Tracer("the.framework").Start(ctx, "GET /answer")
	ctx, step := tracepad.Span(ctx, "answer-question", tracepad.WithInput(map[string]any{"question": "how do I reset my password?"}))
	tracepad.UpdateTrace(ctx, tracepad.WithTraceName("support-chat"), tracepad.WithUserID("user-4821"),
		tracepad.WithSessionID("session-77"), tracepad.WithTags("support", "beta"),
		tracepad.WithTraceVersion("retrieval-v2"))
	genCtx, call := tracepad.Generation(ctx, "chat-completion", tracepad.WithModel("claude-sonnet-5"),
		tracepad.WithPrompt(support), tracepad.WithModelParameters(map[string]any{"temperature": 0.2}),
		tracepad.WithInput([]tracepad.Message{{Role: "user", Content: "how do I reset my password?"}}))
	call.FirstToken()
	cost := 0.0011
	call.End(tracepad.Result{
		Model:  "claude-sonnet-5-2026-08-01",
		Usage:  tracepad.Usage{"input_tokens": 128, "output_tokens": 41},
		Cost:   &cost,
		Output: "Open Settings and choose Reset.",
	})
	if err := tracepad.Score(genCtx, "helpful", tracepad.WithValue(0.9), tracepad.WithComment("cited the source")); err != nil {
		t.Fatal(err)
	}
	_, failed := tracepad.Span(ctx, "fails")
	failed.Fail(errors.New("upstream timeout"))
	step.End()
	request.End()
	flush(t)

	stored := s.trace(step.TraceID())
	for field, want := range map[string]any{
		"name": "support-chat", "user_id": "user-4821", "session_id": "session-77", "version": "retrieval-v2", "error_count": 1.0,
	} {
		if stored[field] != want {
			t.Errorf("%s = %v, want %v", field, stored[field], want)
		}
	}
	if fmt.Sprint(stored["tags"]) != "[beta support]" && fmt.Sprint(stored["tags"]) != "[support beta]" {
		t.Errorf("tags = %v", stored["tags"])
	}
	if cost, _ := stored["total_cost"].(float64); cost < 0.00109 || cost > 0.00111 {
		t.Errorf("total_cost = %v, want the cost as charged", stored["total_cost"])
	}

	observations := walk(stored["observations"])
	if got := names(observations); !reflect.DeepEqual(got, []string{"GET /answer", "answer-question", "chat-completion", "fails"}) {
		t.Fatalf("tree = %v", got)
	}
	_, root, generation, failure := observations[0], observations[1], observations[2], observations[3]
	if !reflect.DeepEqual(root["input"], map[string]any{"question": "how do I reset my password?"}) {
		t.Errorf("input = %v", root["input"])
	}
	for field, want := range map[string]any{
		"type": "generation", "model": "claude-sonnet-5", "output": "Open Settings and choose Reset.",
	} {
		if generation[field] != want {
			t.Errorf("generation %s = %v, want %v", field, generation[field], want)
		}
	}
	if !reflect.DeepEqual(generation["usage"], map[string]any{"input_tokens": 128.0, "output_tokens": 41.0}) {
		t.Errorf("usage = %v", generation["usage"])
	}
	if !reflect.DeepEqual(generation["model_parameters"], map[string]any{"temperature": 0.2}) {
		t.Errorf("model_parameters = %v", generation["model_parameters"])
	}
	if !reflect.DeepEqual(generation["prompt"], map[string]any{"name": "support-answer", "version": 1.0}) {
		t.Errorf("prompt = %v", generation["prompt"])
	}
	if generation["ttft_ms"] == nil {
		t.Error("ttft_ms is not set: FirstToken did not land")
	}
	if failure["level"] != "ERROR" || failure["status_message"] != "upstream timeout" {
		t.Errorf("failed step = level %v, status %v", failure["level"], failure["status_message"])
	}

	scores, _ := s.call("GET", "/api/v1/scores?trace_id="+step.TraceID(), nil)["scores"].([]any)
	if len(scores) != 1 {
		t.Fatalf("scores = %v", scores)
	}
	score := scores[0].(map[string]any)
	if score["name"] != "helpful" || score["value"] != 0.9 || score["comment"] != "cited the source" {
		t.Errorf("score = %v", score)
	}
}
