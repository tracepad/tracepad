package cli

import (
	"context"
	"flag"
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

	for command, offered := range usageFlags(t) {
		for name := range offered {
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
}

// TestEveryFlagIsNamedInTheUsageText is that parity walked the other way:
// every flag a command registers is a flag its usage block names.
//
// The direction above catches a usage line that promises what nobody defined.
// This one catches its mirror — a flag a command quietly takes and never
// offers — which is how `tail` came to accept `--until`: the filters of
// `traces ls`, `traces last` and `tail` are registered once so the three
// cannot drift, and the one they must differ by went unnoticed for having no
// usage line to disagree with. A flag nobody documents is either a promise
// nobody made or a behaviour nobody can find; both are worse than the error a
// reader gets for a flag that does not exist.
//
// What a command registers is read from the command itself, through
// `observeFlags`: a list written out here would be the same guess the usage
// text already is.
func TestEveryFlagIsNamedInTheUsageText(t *testing.T) {
	h := newHarness(t)
	// Already over, for the same reason as above: `tail` would follow for
	// ever, and every flag is registered before anything is asked.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// The global flags belong to every command and to no usage line: they
	// are documented once, where the client commands are introduced.
	global := map[string]bool{"url": true, "key": true, "json": true}

	for command, offered := range usageFlags(t) {
		var sets []*flag.FlagSet
		h.observeFlags = func(fs *flag.FlagSet) { sets = append(sets, fs) }
		h.run(ctx, false, strings.Fields(command)...)
		h.observeFlags = nil
		if len(sets) == 0 {
			t.Errorf("`tracepad %s` built no flag set; the usage text names a command "+
				"that never reaches its flags", command)
			continue
		}
		for _, fs := range sets {
			fs.VisitAll(func(f *flag.Flag) {
				if global[f.Name] || offered[f.Name] || undocumented[command][f.Name] {
					return
				}
				t.Errorf("`tracepad %s` takes --%s, which its usage block does not name: "+
					"a flag that is not offered is one nobody can find and nobody promised",
					command, f.Name)
			})
		}
	}
}

// usageFlags is the usage text read as what it is: a table of commands and the
// flags each one offers. One parser for both directions of the parity, so that
// they can never disagree about what the text says.
func usageFlags(t *testing.T) map[string]map[string]bool {
	t.Helper()
	offered := map[string]map[string]bool{}
	command := ""
	for _, line := range strings.Split(Usage, "\n") {
		if match := usageCommand.FindStringSubmatch(line); match != nil {
			command = match[1]
			offered[command] = map[string]bool{}
		}
		if command == "" {
			continue
		}
		if strings.TrimSpace(line) == "" {
			command = ""
			continue
		}
		for _, flag := range usageFlag.FindAllStringSubmatch(line, -1) {
			offered[command][flag[1]] = true
		}
	}
	if command != "" {
		t.Fatal("the usage text ended inside a command block; the parser did not walk it")
	}
	return offered
}

// undocumented is what this parity does not hold yet: flags a command
// registers and its usage block does not name, found by the test above the
// first time it ran (`go test -run TestEveryFlagIsNamedInTheUsageText` on
// c2fcb8c reports these and `tail --until`).
//
// They are listed rather than fixed because each is a question with two
// answers — write the flag into the usage text, or take it away — and which
// one is right is the owner's call, not this test's. `tail --until` was such a
// question and was answered by taking it away (INBOX, PR #9); these are open.
// Listing them is what makes the test useful in the meantime: the debt is
// enumerated, and a *new* undocumented flag cannot arrive unnoticed.
var undocumented = map[string]map[string]bool{
	// The filters `traces ls` documents and its two siblings inherit.
	"traces last": {"min-cost": true, "name": true, "session": true, "tag": true, "user": true},
	"tail": {"min-cost": true, "name": true, "session": true, "since": true,
		"tag": true, "user": true},
	"scores ls":     {"observation": true, "type": true},
	"prompts ls":    {"limit": true},
	"projects show": {"project": true},
}
