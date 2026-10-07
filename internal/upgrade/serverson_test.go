//go:build unix

package upgrade

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// versionAt is a server's /health answering v, on loopback; its address is
// what a server listening there names with --listen.
func versionAt(t *testing.T, v string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"status":"ok","version":%q}`, v)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// The upgrade's check of what runs from the binary it replaces refuses every
// server the way back's check of that path would (the review of #223): one
// look at the processes before the stop is enough only because of it, and
// prepare skips the second (spec 054 #43). Over every kind of server that
// can run from the path — older, the same, newer, silent, one whose binary
// or version cannot be read — whatever the way back refuses, the upgrade
// refuses too.
func TestTheUpgradesServerCheckHoldsTheWayBacks(t *testing.T) {
	t.Parallel()
	const from, to = "0.5.0", "0.5.1"
	bin := filepath.Join(t.TempDir(), "tracepad")
	server := func(exe, listen string) Process {
		return Process{PID: 4242, Exe: exe, Cwd: "/", Argv: []string{"tracepad", "serve", "--listen", listen, "--data-dir", filepath.Join(t.TempDir(), "data")}}
	}
	cases := map[string][]Process{
		"none":             nil,
		"older":            {server(bin, versionAt(t, "0.4.0"))},
		"the same":         {server(bin, versionAt(t, from))},
		"newer":            {server(bin, versionAt(t, to))},
		"a development":    {server(bin, versionAt(t, "dev"))},
		"silent":           {server(bin, "127.0.0.1:1")},
		"another address":  {server(bin, "192.0.2.10:4318")},
		"an unread binary": {server("", versionAt(t, from))},
		"another binary":   {server(filepath.Join(t.TempDir(), "tracepad"), versionAt(t, to))},
		"not a server":     {{PID: 4243, Exe: bin, Argv: []string{"tracepad", "mcp"}}},
	}
	// What the way back refuses, so the property is not held vacuously.
	backRefuses := map[string]bool{"newer": true, "a development": true, "silent": true, "another address": true, "an unread binary": true}
	for name, procs := range cases {
		r := &runner{deps: Deps{Sys: listed{procs: procs}, HTTP: &http.Client{Timeout: time.Second}}}
		back := r.serversOn(context.Background(), bin, from, false)
		up := r.serversOn(context.Background(), bin, to, true)
		if (back != nil) != backRefuses[name] {
			t.Errorf("%s: the way back's check says %v", name, back)
		}
		if back != nil && up == nil {
			t.Errorf("%s: the way back refuses (%v), and the upgrade does not", name, back)
		}
	}
}

// A server listening on one address of this machine's that is not a
// loopback one is not asked its version (the review of #223): the plan says
// it was not checked, rather than that it does not say its version, and
// does not call it behind; a binary put under it is refused as one whose
// version could not be checked.
func TestAServerOnOneInterfaceIsSaidNotChecked(t *testing.T) {
	t.Parallel()
	w := newFakeWorld(t, 2)
	bin := w.install
	p := Process{PID: 4242, Exe: bin, Cwd: "/", Argv: []string{"tracepad", "serve", "--listen", "192.0.2.10:4318", "--data-dir", filepath.Join(t.TempDir(), "data")}}
	procs, _, _ := w.host.Candidates(context.Background())
	deps := w.deps()
	deps.Sys = listed{procs: append(procs, p)}
	rep, code := runIn(t, context.Background(), deps, "--plan", "--to", fOld)
	notes := strings.Join(rep.Notes, "\n")
	if !strings.Contains(notes, "server pid 4242 was not checked: it listens on 192.0.2.10:4318 only") || strings.Contains(strings.Join(rep.Person, "\n"), "pid 4242") || code != exitOK {
		t.Errorf("%d %s\nnotes: %s\nperson: %q", code, rep.Summary, notes, rep.Person)
	}
	r := &runner{deps: deps}
	err := r.serversOn(context.Background(), bin, fOld, false)
	if err == nil || !strings.Contains(err.Error(), "its version could not be checked: it listens on 192.0.2.10:4318") {
		t.Errorf("the way back's check: %v", err)
	}
}

// A tracepad first on PATH that no package manager put there is upgraded
// by the install script, into its own directory (the live run of rc.3): the
// plan gives that line, not "its package manager upgrades it".
func TestABinaryOnPathGetsTheInstallScriptsLine(t *testing.T) {
	t.Parallel()
	w := newFakeWorld(t, 2)
	if _, code := runIn(t, context.Background(), w.deps(), "--to", fNew, "--data-dir", w.data); code != exitOK {
		t.Fatal(code)
	}
	first := filepath.Join(t.TempDir(), "bin", "tracepad")
	_ = os.MkdirAll(filepath.Dir(first), 0o700)
	scriptBinary(t, first, fOld)
	deps := w.deps()
	deps.LookPath = func(string) string { return first }
	rep, _ := runIn(t, context.Background(), deps, "--plan", "--to", fNew)
	// Unpinned: the script never steps back from a newer one (the tenth
	// review of #228).
	want := "what upgrades it is the install script: curl -fsSL https://tracepad.github.io/tracepad/install.sh | TRACEPAD_INSTALL_DIR=" + shq(filepath.Dir(first)) + " sh"
	if all := strings.Join(rep.Notes, "\n"); !strings.Contains(all, want) {
		t.Errorf("the plan says:\n%s\nnot %s", all, want)
	}
	r := &runner{deps: Deps{Home: "/home/a"}}
	if got := r.installLine("/home/a/.local/bin", "0.2.0"); got != "curl -fsSL https://tracepad.github.io/tracepad/install.sh | TRACEPAD_VERSION=0.2.0 sh" {
		t.Errorf("the script's own directory: %s", got)
	}
	if got := r.installLine("/home/a/.local/bin", ""); got != "curl -fsSL https://tracepad.github.io/tracepad/install.sh | sh" {
		t.Errorf("unpinned: %s", got)
	}
}

// After a way back the skill's copies are put back at the version that runs
// again (the live run of rc.3: they stayed the newer one's), by the old
// binary, back at the install path.
func TestAWayBackPutsTheSkillBack(t *testing.T) {
	t.Parallel()
	w := newFakeWorld(t, 2)
	deps := w.deps()
	skillCopyAt(t, deps.Home, fOld)
	var by []string
	deps.Skills = func(ctx context.Context, bin, _ string, _ ...string) (string, error) {
		v, _ := scriptVersion(ctx, bin)
		by = append(by, v)
		return "installed " + v, nil
	}
	rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
	if code != exitOK {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	back, code := runIn(t, context.Background(), deps, "--back", rep.Run.ID)
	// The world's working directory is its home: one copy, installed once
	// each way, never again as the project's.
	if code != exitOK || len(by) != 2 || by[0] != fNew || by[1] != fOld || !strings.Contains(strings.Join(back.Done, "\n"), "skill: installed "+fOld) {
		t.Errorf("%d %s; installed by %q; done %q", code, back.Summary, by, back.Done)
	}
}

// A server whose configuration cannot be read — a relative data directory,
// its working directory unread — keeps that reason (the review of #225):
// the plan names it as one that may be behind, with the reason, not as one
// listening on an address it does not ask, and the check of what runs from
// the install path refuses with it, never with an empty address.
func TestAServerThatCannotBeResolvedKeepsItsReason(t *testing.T) {
	t.Parallel()
	w := newFakeWorld(t, 2)
	p := Process{PID: 4242, Exe: w.install, Argv: []string{"tracepad", "serve", "--data-dir", "relative/data"}}
	procs, _, _ := w.host.Candidates(context.Background())
	deps := w.deps()
	deps.Sys = listed{procs: append(procs, p)}
	rep, code := runIn(t, context.Background(), deps, "--plan", "--to", fOld)
	person := strings.Join(rep.Person, "\n")
	if code != exitDecide || !strings.Contains(person, "server pid 4242 does not say its version") || !strings.Contains(person, "its working directory could not be read") || strings.Contains(strings.Join(rep.Notes, "\n"), "pid 4242") {
		t.Errorf("%d %s\nperson: %s\nnotes: %q", code, rep.Summary, person, rep.Notes)
	}
	r := &runner{deps: deps}
	err := r.serversOn(context.Background(), w.install, fOld, false)
	if err == nil || !strings.Contains(err.Error(), "cannot be told (its data directory \"relative/data\" is relative") {
		t.Errorf("the way back's check: %v", err)
	}
}
