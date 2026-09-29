package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// `traces rm` (spec 035 #7, Testing — CLI): the single form shows the preview
// and stops without --yes, --yes sends the server's echo, and the bulk form
// refuses without --to, loops rounds and prints the total.

func (h *harness) seedTraces(t *testing.T, from, to int, environment string) {
	t.Helper()
	batch := &store.IngestBatch{ProjectID: h.projectID(t)}
	for n := from; n <= to; n++ {
		start := seedBase - int64(n)*ms
		batch.Traces = append(batch.Traces, &model.Trace{ID: traceHex(n), Name: "chat", Environment: environment})
		batch.Observations = append(batch.Observations, &model.Observation{
			TraceID: traceHex(n), ID: spanHex(n), Type: model.TypeSpan, Level: model.LevelDefault,
			StartTime: start, EndTime: start + ms})
	}
	if err := h.writer.Submit(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) traceCount(t *testing.T) int64 {
	t.Helper()
	counts, err := h.store.TableCounts(t.Context(), h.projectID(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, count := range counts {
		if count.Table == "traces" {
			return count.Rows
		}
	}
	t.Fatal("traces is not a counted table")
	return 0
}

func TestTracesRemoveOne(t *testing.T) {
	h := newHarness(t)
	h.seedTraces(t, 1, 2, "production")

	// Non-interactive and unconfirmed: the preview on stderr, nothing done.
	out := h.run(t.Context(), false, "traces", "rm", traceHex(1))
	if out.code != ExitFailure {
		t.Fatalf("unconfirmed traces rm exited %d, want 1: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stderr, "--yes") || !strings.Contains(out.stderr, "observations") {
		t.Errorf("stderr = %q, want the preview and the flag that goes ahead", out.stderr)
	}
	if got := h.traceCount(t); got != 2 {
		t.Fatalf("traces = %d after an unconfirmed rm, want both", got)
	}

	// Interactive: the echo is the trace id.
	h.stdin = "nope\n"
	out = h.run(t.Context(), true, "traces", "rm", traceHex(1))
	if out.code != ExitFailure || !strings.Contains(out.stderr, "nothing was done") {
		t.Fatalf("a wrong echo exited %d: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stderr, fmt.Sprintf("type %q to confirm", traceHex(1))) {
		t.Errorf("stderr = %q, want the trace id as the echo", out.stderr)
	}

	// --yes sends the server's own echo, and the preview is still shown.
	out = h.run(t.Context(), true, "traces", "rm", traceHex(1), "--yes")
	if out.code != ExitOK {
		t.Fatalf("traces rm --yes exited %d: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stderr, "this would delete trace "+traceHex(1)) {
		t.Errorf("stderr = %q, want the preview before the act", out.stderr)
	}
	if !strings.Contains(out.stdout, "deleted trace "+traceHex(1)) ||
		!strings.Contains(out.stdout, "raw OTLP bodies are not deleted") {
		t.Errorf("stdout = %q, want the deletion and the raw archive position stated", out.stdout)
	}
	if got := h.traceCount(t); got != 1 {
		t.Errorf("traces = %d, want the other one left", got)
	}

	// An unknown id is the server's 404, not a preview.
	out = h.run(t.Context(), false, "traces", "rm", traceHex(1), "--yes")
	if out.code != ExitFailure || !strings.Contains(out.stderr, "not found") {
		t.Errorf("rm of a deleted trace exited %d: %s", out.code, out.stderr)
	}

	// --json: the server's answer verbatim.
	out = h.run(t.Context(), false, "traces", "rm", traceHex(2), "--yes", "--json")
	if out.code != ExitOK {
		t.Fatalf("traces rm --json exited %d: %s", out.code, out.stderr)
	}
	var answer struct {
		DryRun  bool             `json:"dry_run"`
		Deleted map[string]int64 `json:"deleted"`
		ID      string           `json:"id"`
	}
	if err := json.Unmarshal([]byte(out.stdout), &answer); err != nil {
		t.Fatalf("stdout is not the answer: %v (%q)", err, out.stdout)
	}
	if answer.DryRun || answer.ID != traceHex(2) || answer.Deleted["traces"] != 1 {
		t.Errorf("answer = %+v, want the deletion of trace 2", answer)
	}
}

func TestTracesRemoveByFilter(t *testing.T) {
	h := newHarness(t)
	h.seedTraces(t, 1, 7, "staging")
	h.seedTraces(t, 8, 9, "production")

	// --to is required and the CLI does not fill it in (#7).
	out := h.run(t.Context(), false, "traces", "rm", "--env", "staging", "--yes")
	if out.code != ExitUsage || !strings.Contains(out.stderr, "--to") {
		t.Fatalf("rm without --to exited %d, want 2 naming --to: %s", out.code, out.stderr)
	}
	for _, args := range [][]string{
		{"traces", "rm"},
		{"traces", "rm", traceHex(1), "--to", "1h"},
		{"traces", "rm", traceHex(1), traceHex(2)},
		{"traces", "rm", "--to", "1h", "--limit", "1001"},
	} {
		if out := h.run(t.Context(), false, args...); out.code != ExitUsage {
			t.Errorf("%v exited %d, want 2 (usage): %s", args, out.code, out.stderr)
		}
	}
	if got := h.traceCount(t); got != 9 {
		t.Fatalf("traces = %d after the refusals, want all nine", got)
	}

	// Unconfirmed: the preview says how many match, and nothing is done.
	out = h.run(t.Context(), false, "traces", "rm", "--to", "2026-09-02T00:00:00Z", "--env", "staging")
	if out.code != ExitFailure {
		t.Fatalf("unconfirmed bulk rm exited %d, want 1: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stderr, "matched        7") || !strings.Contains(out.stderr, "--yes") {
		t.Errorf("stderr = %q, want the exact match count and the flag that goes ahead", out.stderr)
	}
	if got := h.traceCount(t); got != 9 {
		t.Fatalf("traces = %d after a preview, want all nine", got)
	}

	// Confirmed: rounds of --limit until the server says there is no more,
	// and the total printed once.
	h.stdin = "test\n"
	out = h.run(t.Context(), true, "traces", "rm", "--to", "2026-09-02T00:00:00Z",
		"--env", "staging", "--limit", "3")
	if out.code != ExitOK {
		t.Fatalf("bulk rm exited %d: %s", out.code, out.stderr)
	}
	if !strings.Contains(out.stderr, `type "test" to confirm`) {
		t.Errorf("stderr = %q, want the project name as the echo", out.stderr)
	}
	if !strings.Contains(out.stderr, "3 traces deleted so far") ||
		!strings.Contains(out.stderr, "6 traces deleted so far") {
		t.Errorf("stderr = %q, want the running total between rounds", out.stderr)
	}
	if !strings.Contains(out.stdout, "deleted 7 traces in 3 round(s)") {
		t.Errorf("stdout = %q, want the total over the rounds", out.stdout)
	}
	if got := h.traceCount(t); got != 2 {
		t.Errorf("traces = %d, want the two production ones left", got)
	}

	// --json sums the rounds into one answer.
	out = h.run(t.Context(), false, "traces", "rm", "--to", "2026-09-02T00:00:00Z", "--yes", "--json")
	if out.code != ExitOK {
		t.Fatalf("bulk rm --json exited %d: %s", out.code, out.stderr)
	}
	var answer struct {
		DryRun  bool             `json:"dry_run"`
		Deleted map[string]int64 `json:"deleted"`
		Rounds  int              `json:"rounds"`
	}
	if err := json.Unmarshal([]byte(out.stdout), &answer); err != nil {
		t.Fatalf("stdout is not the answer: %v (%q)", err, out.stdout)
	}
	if answer.DryRun || answer.Deleted["traces"] != 2 || answer.Rounds != 1 {
		t.Errorf("answer = %+v, want the two production traces in one round", answer)
	}
	if got := h.traceCount(t); got != 0 {
		t.Errorf("traces = %d, want none left", got)
	}
}

// `--tag` is repeatable and an AND on every command that filters traces
// (docs/cli.md, Lists): with two given, a trace must carry both. It used to
// keep the last alone, which for `traces rm` meant deleting traces that
// carried only that one.
func TestRepeatedTagsFilterByAllOfThem(t *testing.T) {
	h := newHarness(t)
	batch := &store.IngestBatch{ProjectID: h.projectID(t)}
	for n, tags := range [][]string{{"a", "b"}, {"a"}, {"b"}, {"a", "b", "c"}} {
		start := seedBase - int64(n+1)*ms
		batch.Traces = append(batch.Traces, &model.Trace{ID: traceHex(n + 1), Name: "chat", Tags: tags})
		batch.Observations = append(batch.Observations, &model.Observation{
			TraceID: traceHex(n + 1), ID: spanHex(n + 1), Type: model.TypeSpan, Level: model.LevelDefault,
			StartTime: start, EndTime: start + ms})
	}
	if err := h.writer.Submit(t.Context(), batch); err != nil {
		t.Fatal(err)
	}

	// The listing: traces 1 and 4 carry both.
	out := h.run(t.Context(), false, "traces", "ls", "--tag", "a", "--tag", "b", "--json")
	if out.code != ExitOK {
		t.Fatalf("traces ls exited %d: %s", out.code, out.stderr)
	}
	var listed struct {
		Traces []struct {
			ID string `json:"id"`
		} `json:"traces"`
	}
	if err := json.Unmarshal([]byte(out.stdout), &listed); err != nil {
		t.Fatalf("stdout is not the listing: %v (%q)", err, out.stdout)
	}
	if len(listed.Traces) != 2 {
		t.Errorf("traces ls --tag a --tag b listed %d traces, want the two that carry both", len(listed.Traces))
	}

	// The deletion: unconfirmed it counts the same two, confirmed it takes
	// exactly them.
	out = h.run(t.Context(), false, "traces", "rm", "--to", "2026-09-02T00:00:00Z", "--tag", "a", "--tag", "b")
	if !strings.Contains(out.stderr, "matched        2") {
		t.Errorf("preview = %q, want 2 matched", out.stderr)
	}
	h.stdin = "test\n"
	out = h.run(t.Context(), true, "traces", "rm", "--to", "2026-09-02T00:00:00Z", "--tag", "a", "--tag", "b", "--yes")
	if out.code != ExitOK {
		t.Fatalf("rm exited %d: %s", out.code, out.stderr)
	}
	if got := h.traceCount(t); got != 2 {
		t.Errorf("traces = %d after the deletion, want the two that lacked a tag", got)
	}
}
