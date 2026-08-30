package cli

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
)

// `--search` (spec 011, CLI contract) and the parity that keeps the usage text
// and the flags one thing.

func seedSearchable(t *testing.T, h *harness) {
	t.Helper()
	h.seed(t, &model.Trace{ID: traceHex(1), Name: "support-chat", Environment: "production"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeGeneration,
			Name: "answer", Level: model.LevelDefault,
			StartTime: seedBase, EndTime: seedBase + 100*ms,
			Output: "the refund failed for the order because the card issuer declined it"})
	h.seed(t, &model.Trace{ID: traceHex(2), Name: "eval", Environment: "staging"},
		&model.Observation{TraceID: traceHex(2), ID: spanHex(2), Type: model.TypeSpan,
			Name: "judge", Level: model.LevelDefault,
			StartTime: seedBase + 1000*ms, EndTime: seedBase + 1300*ms,
			Output: "the answer was accepted"})
}

func TestTracesListSearch(t *testing.T) {
	h := newHarness(t)
	seedSearchable(t, h)

	got := h.run(t.Context(), true, "traces", "ls", "--search", "refund")
	if got.code != ExitOK {
		t.Fatalf("exit = %d (stderr: %s)", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, traceHex(1)) {
		t.Errorf("the matching trace is missing:\n%s", got.stdout)
	}
	if strings.Contains(got.stdout, traceHex(2)) {
		t.Errorf("a trace that does not match is listed:\n%s", got.stdout)
	}
	// The second line under the row: where it matched, and the text there.
	if !strings.Contains(got.stdout, spanHex(1)+" output") {
		t.Errorf("the row does not say where it matched:\n%s", got.stdout)
	}
	if !strings.Contains(got.stdout, "refund failed") {
		t.Errorf("the row carries no snippet:\n%s", got.stdout)
	}
	// Dimmed on a terminal, and nowhere else: piped output is bytes
	// somebody parses (spec 004 #12).
	if !strings.Contains(got.stdout, "\x1b[2m") {
		t.Errorf("the snippet is not dimmed on a terminal:\n%q", got.stdout)
	}
	piped := h.run(t.Context(), false, "traces", "ls", "--search", "refund", "--json")
	if strings.Contains(piped.stdout, "\x1b[") {
		t.Errorf("piped output carries terminal escapes:\n%q", piped.stdout)
	}
	if !strings.Contains(piped.stdout, `"match"`) {
		t.Errorf("JSON output carries no match:\n%s", piped.stdout)
	}
}

func TestTracesLastSearch(t *testing.T) {
	h := newHarness(t)
	seedSearchable(t, h)

	got := h.run(t.Context(), true, "traces", "last", "--search", "refund")
	if got.code != ExitOK {
		t.Fatalf("exit = %d (stderr: %s)", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, traceHex(1)) {
		t.Errorf("--search did not reach the shortcut:\n%s", got.stdout)
	}

	// A search nothing matches is the endpoint's 404, named.
	empty := h.run(t.Context(), true, "traces", "last", "--search", "aardvark")
	if empty.code != ExitFailure {
		t.Errorf("exit = %d, want a request failure for a search that found nothing", empty.code)
	}
	if !strings.Contains(empty.stderr, "aardvark") {
		t.Errorf("the failure does not name the search: %s", empty.stderr)
	}
}

// TestTailTakesNoSearch: `tail` follows the newest page and is not a question
// about text (spec 011, CLI contract).
func TestTailTakesNoSearch(t *testing.T) {
	h := newHarness(t)
	got := h.run(t.Context(), true, "tail", "--search", "refund")
	if got.code != ExitUsage {
		t.Fatalf("exit = %d, want a usage error", got.code)
	}
	if !strings.Contains(got.stderr, "not defined") {
		t.Errorf("stderr = %q, want the flag refused", got.stderr)
	}
}

// TestSearchRefusalIsTheServersOwn: a query with no word in it is a 400 the
// CLI passes through, rather than an empty table (spec 011 #4).
func TestSearchRefusalIsTheServersOwn(t *testing.T) {
	h := newHarness(t)
	got := h.run(t.Context(), true, "traces", "ls", "--search", "()")
	if got.code != ExitFailure {
		t.Fatalf("exit = %d, want the server's refusal", got.code)
	}
	if !strings.Contains(got.stderr, "word") {
		t.Errorf("stderr = %q, want the server's own message about it", got.stderr)
	}
}

// usageFlag finds the flags the usage text names, per command.
var (
	usageCommand = regexp.MustCompile(`^\s+tracepad ([a-z]+(?: [a-z-]+)?)`)
	usageFlag    = regexp.MustCompile(`--([a-z][a-z-]*)`)
)

// TestUsageAndFlagsAgree is the parity spec 004 #9 asks for, pointed at the
// command line: every flag the usage text offers is a flag the command it is
// listed under actually defines.
//
// A usage line naming a flag nobody registered is the same class of defect as
// an endpoint in `openapi.json` that nobody serves — a promise a reader acts
// on. `--flag=1` rather than `--flag 1`: the value is refused by half of them
// and that is fine, because the only answer this test refuses is "not defined".
func TestUsageAndFlagsAgree(t *testing.T) {
	h := newHarness(t)
	// A context that is already over: flags are parsed before anything is
	// asked of the server, and `tail` would otherwise follow for ever.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	command := ""
	for _, line := range strings.Split(Usage, "\n") {
		if match := usageCommand.FindStringSubmatch(line); match != nil {
			command = match[1]
		}
		if command == "" {
			continue
		}
		if strings.TrimSpace(line) == "" {
			command = ""
			continue
		}
		for _, flag := range usageFlag.FindAllStringSubmatch(line, -1) {
			name := flag[1]
			t.Run(command+" --"+name, func(t *testing.T) {
				args := append(strings.Fields(command), "--"+name+"=1")
				got := h.run(ctx, false, args...)
				if strings.Contains(got.stderr, "not defined") {
					t.Errorf("the usage text offers `tracepad %s --%s`, "+
						"which the command does not define: %s", command, name, got.stderr)
				}
			})
		}
	}
	if command != "" {
		t.Fatal("the usage text ended inside a command block; the parser did not walk it")
	}
}
