package tracepad

import (
	"context"
	"testing"
	"time"
)

func TestGenerationWritesTheRequestAndTheResult(t *testing.T) {
	r := setup(t)
	prompt := &PromptVersion{Name: "support-answer", Version: 3}
	_, call := Generation(context.Background(), "chat",
		WithModel("claude-sonnet-5"), WithPrompt(prompt),
		WithModelParameters(map[string]any{"temperature": 0.2, "max_tokens": 512, "stream": true, "stop": []string{"\n"}}),
		WithInput([]Message{{Role: "user", Content: "hi"}}))
	cost := 0.0011
	call.End(Result{
		Model:  "claude-sonnet-5-2026-08-01",
		Usage:  Usage{"input_tokens": 128, "output_tokens": 41, "cache_read_input_tokens": 96},
		Cost:   &cost,
		Output: "In the trace you are reading.",
	})

	attrs := r.attrs(t, "chat")
	want := map[string]string{
		attrObservationType:                         "generation",
		attrRequestModel:                            "claude-sonnet-5",
		attrResponseModel:                           "claude-sonnet-5-2026-08-01",
		attrRequestPrefix + "temperature":           "0.2",
		attrRequestPrefix + "max_tokens":            "512",
		attrRequestPrefix + "stream":                "true",
		attrRequestPrefix + "stop":                  `["\n"]`,
		attrInput:                                   `[{"role":"user","content":"hi"}]`,
		attrOutput:                                  "In the trace you are reading.",
		attrUsagePrefix + "input_tokens":            "128",
		attrUsagePrefix + "output_tokens":           "41",
		attrUsagePrefix + "cache_read_input_tokens": "96",
		attrUsageCost:                               "0.0011",
		attrPromptName:                              "support-answer",
		attrPromptVersion:                           "3",
	}
	for key, value := range want {
		if got := str(t, attrs, key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
}

func TestANilCostWritesNoCost(t *testing.T) {
	r := setup(t)
	_, call := Generation(context.Background(), "chat")
	call.End(Result{Model: "m", Usage: Usage{"input_tokens": 1}})
	attrs := r.attrs(t, "chat")
	for _, key := range []string{attrUsageCost, attrOutput, attrRequestModel} {
		if _, set := attrs[key]; set {
			t.Errorf("%s is set to %v, want absent", key, attrs[key].Emit())
		}
	}
}

func TestFirstTokenStampsOnce(t *testing.T) {
	r := setup(t)
	_, call := Generation(context.Background(), "chat")
	first := time.Now()
	call.FirstToken()
	time.Sleep(20 * time.Millisecond)
	call.FirstToken()
	call.End(Result{})

	stamp, err := time.Parse(time.RFC3339Nano, str(t, r.attrs(t, "chat"), attrCompletionStartTime))
	if err != nil {
		t.Fatal(err)
	}
	if stamp.Before(first.Add(-time.Second)) || stamp.After(first.Add(10*time.Millisecond)) {
		t.Errorf("completion start = %v, want the first call's instant %v", stamp, first)
	}
}

func TestEndAfterFailWritesNothingMore(t *testing.T) {
	r := setup(t)
	_, call := Generation(context.Background(), "chat")
	call.Fail(context.DeadlineExceeded)
	call.End(Result{Model: "late"})
	if _, set := r.attrs(t, "chat")[attrResponseModel]; set {
		t.Error("a result after the span ended must not be written")
	}
}
