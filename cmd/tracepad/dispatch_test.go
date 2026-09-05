package main

import (
	"slices"
	"testing"

	"github.com/tracepad/tracepad/internal/cli"
)

// The seam between "the CLI implements a command" and "the binary routes that
// word to the CLI" (spec 020 #14).
//
// It went untested until the image's HEALTHCHECK needed it, and it was broken
// the whole time: `cmd/tracepad` kept a hand-written copy of the command list,
// and `datasets`, `runs`, `score-configs` and `export` were never added to it.
// All four shipped — documented in `docs/cli.md`, `docs/datasets.md` and
// `docs/export.md` — answering `unknown command` from the only binary anyone
// runs. Nothing caught it because every CLI test calls `cli.Run` directly,
// which is the one path that skips this file.

// TestEveryCLICommandIsRoutedToTheCLI: the parity itself. It holds by
// construction now — `clientCommands` is built from `cli.Commands()` — and this
// test is what keeps that true if someone reintroduces a hand-written list.
func TestEveryCLICommandIsRoutedToTheCLI(t *testing.T) {
	for _, name := range cli.Commands() {
		if !clientCommands[name] {
			t.Errorf("the CLI serves %q and the binary does not route it: "+
				"the command is documented, implemented, and answers "+
				"`unknown command` to anyone who runs the binary", name)
		}
	}
	if len(clientCommands) != len(cli.Commands()) {
		t.Errorf("the binary routes %d commands, the CLI serves %d: "+
			"the extra ones reach cli.Run and are refused there instead",
			len(clientCommands), len(cli.Commands()))
	}
}

// TestTheCommandsThatWentMissingAreRouted names the four by hand, on purpose.
// The test above is a rule and would stay green if the CLI lost a command as
// well as the binary; this one is the regression, and it fails if any of these
// stops being reachable however that happens.
func TestTheCommandsThatWentMissingAreRouted(t *testing.T) {
	for _, name := range []string{"datasets", "runs", "score-configs", "export"} {
		if !clientCommands[name] {
			t.Errorf("%q is unreachable from the binary again", name)
		}
	}
}

// TestTheCLIClaimsNoServerCommand: `clientCommands` is consulted before this
// file's own words, so a CLI command called `serve` would quietly take the
// server away. Nothing in the type system says it cannot.
func TestTheCLIClaimsNoServerCommand(t *testing.T) {
	for _, name := range cli.Commands() {
		if slices.Contains(serverCommands, name) {
			t.Errorf("the CLI serves %q, which is the binary's own command: "+
				"routing consults the CLI first, so the server half would "+
				"become unreachable", name)
		}
	}
}

// TestSplitCommandFindsEveryCLICommand: routing is two steps, and the parity
// above only covers the second. `splitCommand` has to hand the word over in the
// first place — a command named like a flag, or an empty table, would never
// reach the map at all.
func TestSplitCommandFindsEveryCLICommand(t *testing.T) {
	if len(cli.Commands()) == 0 {
		t.Fatal("the CLI reports no commands at all")
	}
	for _, name := range cli.Commands() {
		cmd, args := splitCommand([]string{name, "--json"})
		if cmd != name {
			t.Errorf("splitCommand(%q) = %q, want the command itself", name, cmd)
		}
		if len(args) != 1 {
			t.Errorf("splitCommand(%q) kept %d arguments, want 1", name, len(args))
		}
	}
}
