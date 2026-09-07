package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
)

// The annotation commands (spec 024, Testing — CLI): the whole desk loop
// through the command line, both output modes, and the confirmation a queue
// deletion needs off a terminal.

// writeTemp writes a file for a command that takes one.
func writeTemp(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// declare puts a config and a queue over it, which every test here starts
// from.
func (h *harness) declare(t *testing.T, queue string, configs ...string) {
	t.Helper()
	for _, config := range configs {
		put := h.run(t.Context(), true, "score-configs", "push", config,
			"--file", writeTemp(t, `{"data_type": "numeric", "direction": "higher"}`))
		if put.code != ExitOK {
			t.Fatalf("score-configs push = %+v", put)
		}
	}
	args := []string{"queues", "put", queue, "--description", "what to review"}
	for _, config := range configs {
		args = append(args, "--config", config)
	}
	if got := h.run(t.Context(), true, args...); got.code != ExitOK {
		t.Fatalf("queues put = %+v", got)
	}
}

// TestAnnotationLoopOnTheCommandLine is the loop the docs print: declare the
// queue, fill it, take the next item, post the scores, complete. Every step is
// a command, which is what lets a judge-model script annotate through the same
// door a person uses (#9).
func TestAnnotationLoopOnTheCommandLine(t *testing.T) {
	h := newHarness(t)
	h.seed(t, &model.Trace{ID: traceHex(1), Name: "chat", Environment: "production"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Name: "handle", Level: model.LevelDefault,
			StartTime: seedBase, EndTime: seedBase + 100*ms})
	h.declare(t, "review", "accuracy")

	listed := h.run(t.Context(), true, "queues", "ls")
	if !strings.Contains(listed.stdout, "review") || !strings.Contains(listed.stdout, "accuracy") {
		t.Fatalf("queues ls = %q", listed.stdout)
	}

	added := h.run(t.Context(), true, "queues", "add", "review", "--trace", traceHex(1))
	if added.code != ExitOK || !strings.Contains(added.stdout, "added to review") {
		t.Fatalf("queues add = %+v", added)
	}
	// The same target again: already there, not a second row (#2).
	twice := h.run(t.Context(), true, "queues", "add", "review", "--trace", traceHex(1))
	if !strings.Contains(twice.stdout, "already in review") {
		t.Errorf("second add = %q", twice.stdout)
	}

	taken := h.run(t.Context(), false, "queues", "next", "review", "--annotator", "ada")
	var handed struct {
		Item *struct {
			ID        string `json:"id"`
			TraceID   string `json:"trace_id"`
			ClaimedBy string `json:"claimed_by"`
		} `json:"item"`
		Pending int64 `json:"pending"`
	}
	if err := json.Unmarshal([]byte(taken.stdout), &handed); err != nil {
		t.Fatalf("queues next = %+v: %v", taken, err)
	}
	if handed.Item == nil || handed.Item.TraceID != traceHex(1) || handed.Item.ClaimedBy != "ada" {
		t.Fatalf("next = %+v, want the queued trace claimed by ada", handed.Item)
	}

	// Completing before the score is posted is refused, and the refusal
	// says which name is missing (#7).
	early := h.run(t.Context(), true, "queues", "complete", "review", handed.Item.ID,
		"--annotator", "ada")
	if early.code != ExitFailure || !strings.Contains(early.stderr, "accuracy") {
		t.Fatalf("early complete = %+v, want a refusal naming the missing score", early)
	}

	scored := h.run(t.Context(), true, "scores", "add", "--trace", traceHex(1),
		"--name", "accuracy", "--value", "0.9")
	if scored.code != ExitOK {
		t.Fatalf("scores add = %+v", scored)
	}
	done := h.run(t.Context(), true, "queues", "complete", "review", handed.Item.ID,
		"--annotator", "ada")
	if done.code != ExitOK || !strings.Contains(done.stdout, "completed") {
		t.Fatalf("complete = %+v", done)
	}

	// And the queue is empty rather than merely quiet.
	empty := h.run(t.Context(), true, "queues", "next", "review", "--annotator", "ada")
	if !strings.Contains(empty.stdout, "review is done") {
		t.Errorf("next on a finished queue = %q", empty.stdout)
	}

	items := h.run(t.Context(), true, "queues", "items", "review")
	if !strings.Contains(items.stdout, "completed") || !strings.Contains(items.stdout, "ada") {
		t.Errorf("queues items = %q", items.stdout)
	}
}

func TestQueuesAddFromTracesTakesTheListingsFlags(t *testing.T) {
	h := newHarness(t)
	for i := 1; i <= 3; i++ {
		environment := "production"
		if i == 3 {
			environment = "staging"
		}
		h.seed(t, &model.Trace{ID: traceHex(i), Environment: environment},
			&model.Observation{TraceID: traceHex(i), ID: spanHex(i), Type: model.TypeSpan,
				Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + 100*ms})
	}
	h.declare(t, "review", "accuracy")

	filtered := h.run(t.Context(), true, "queues", "add", "review",
		"--from-traces", "--env", "production")
	if filtered.code != ExitOK || !strings.Contains(filtered.stdout, "2 matched, 2 traces added") {
		t.Fatalf("from-traces = %+v", filtered)
	}
	// The cap is a callful, not a refusal, and the message says how to
	// continue.
	h.declare(t, "capped", "accuracy")
	capped := h.run(t.Context(), true, "queues", "add", "capped", "--from-traces", "--limit", "1")
	if !strings.Contains(capped.stdout, "3 matched, 1 trace added") ||
		!strings.Contains(capped.stdout, "--until") {
		t.Fatalf("capped run = %+v", capped)
	}

	// The two ways in are exclusive, and neither being given is a usage
	// error rather than a queue filled with something nobody asked for.
	for _, args := range [][]string{
		{"queues", "add", "review"},
		{"queues", "add", "review", "--trace", traceHex(1), "--from-traces"},
	} {
		if got := h.run(t.Context(), true, args...); got.code != ExitUsage {
			t.Errorf("%v = %+v, want a usage error", args, got)
		}
	}
	if got := h.run(t.Context(), true, "queues", "add", "review",
		"--from-traces", "--observation", spanHex(1)); got.code != ExitUsage {
		t.Errorf("--observation without --trace = %+v, want a usage error", got)
	}
}

func TestQueuesSkipAndReopen(t *testing.T) {
	h := newHarness(t)
	h.seed(t, &model.Trace{ID: traceHex(1), Environment: "production"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + 100*ms})
	h.declare(t, "review", "accuracy")
	h.run(t.Context(), true, "queues", "add", "review", "--trace", traceHex(1))
	id := h.firstItemID(t, "review")

	skipped := h.run(t.Context(), true, "queues", "skip", "review", id,
		"--annotator", "ada", "--reason", "nothing to judge")
	if skipped.code != ExitOK || !strings.Contains(skipped.stdout, "skipped") {
		t.Fatalf("skip = %+v", skipped)
	}
	listed := h.run(t.Context(), true, "queues", "items", "review", "--status", "skipped")
	if !strings.Contains(listed.stdout, "nothing to judge") {
		t.Errorf("items = %q, want the reason on the row", listed.stdout)
	}

	reopened := h.run(t.Context(), true, "queues", "reopen", "review", id, "--annotator", "bob")
	if reopened.code != ExitOK || !strings.Contains(reopened.stdout, "pending") {
		t.Fatalf("reopen = %+v", reopened)
	}
	// `--reason` belongs to skip alone; the others refuse it rather than
	// dropping it silently.
	if got := h.run(t.Context(), true, "queues", "reopen", "review", id,
		"--annotator", "bob", "--reason", "why"); got.code != ExitUsage {
		t.Errorf("reopen --reason = %+v, want a usage error", got)
	}
	for _, verb := range []string{"complete", "skip", "reopen"} {
		if got := h.run(t.Context(), true, "queues", verb, "review", id); got.code != ExitUsage {
			t.Errorf("%s without --annotator = %+v, want a usage error", verb, got)
		}
	}
}

// firstItemID reads the id of the first item of a queue, through the command.
func (h *harness) firstItemID(t *testing.T, queue string) string {
	t.Helper()
	listed := h.run(t.Context(), false, "queues", "items", queue)
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(listed.stdout), &page); err != nil {
		t.Fatalf("queues items = %+v: %v", listed, err)
	}
	if len(page.Items) == 0 {
		t.Fatalf("queues items = %q, want at least one", listed.stdout)
	}
	return page.Items[0].ID
}

// TestQueuesRemoveAsksBeforeItDeletes: a queue deletion wears the same
// ceremony every destructive command does — the server's dry run, the echo
// typed back, and `--yes` for a script (spec 005 #13).
func TestQueuesRemoveAsksBeforeItDeletes(t *testing.T) {
	h := newHarness(t)
	h.seed(t, &model.Trace{ID: traceHex(1), Environment: "production"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase, EndTime: seedBase + 100*ms})
	h.declare(t, "review", "accuracy")
	h.run(t.Context(), true, "queues", "add", "review", "--trace", traceHex(1))

	// Off a terminal and without --yes: it says what it would do and does
	// nothing.
	refused := h.run(t.Context(), false, "queues", "rm", "review")
	if refused.code != ExitFailure || !strings.Contains(refused.stderr, "Re-run with --yes") {
		t.Fatalf("rm without --yes = %+v", refused)
	}
	if got := h.run(t.Context(), true, "queues", "ls"); !strings.Contains(got.stdout, "review") {
		t.Fatalf("the queue went without a confirmation")
	}

	deleted := h.run(t.Context(), true, "queues", "rm", "review", "--yes")
	if deleted.code != ExitOK || !strings.Contains(deleted.stdout, "1 item gone") {
		t.Fatalf("rm --yes = %+v", deleted)
	}
	if !strings.Contains(deleted.stderr, "the scores stay") {
		t.Errorf("the preview did not say the scores stay: %q", deleted.stderr)
	}
	if got := h.run(t.Context(), true, "queues", "ls"); !strings.Contains(got.stdout, "no queues") {
		t.Errorf("queues ls = %q, want it gone", got.stdout)
	}
}
