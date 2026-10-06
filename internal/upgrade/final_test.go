//go:build unix

package upgrade

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A zombie answers kill(pid, 0) and runs nothing: it is gone.
func TestAZombieIsGone(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for alive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(pid) {
		t.Error("a zombie counted as alive")
	}
	_ = cmd.Wait()
}

// A link at the install path is someone else's install, never the command's
// to replace.
func TestALinkedBinaryIsNotTheCommands(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "cellar", "tracepad")
	_ = os.MkdirAll(filepath.Dir(real), 0o755)
	scriptBinary(t, real, "0.5.0")
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0o755)
	if err := os.Symlink(real, filepath.Join(bin, "tracepad")); err != nil {
		t.Fatal(err)
	}
	r := &runner{deps: Deps{InstallDir: bin, Version: scriptVersion, LookPath: func(string) string { return "" }}}
	b := r.installedBinary(context.Background())
	if b.Ours || !strings.Contains(b.Reason, "symbolic link") {
		t.Errorf("%+v", b)
	}
}

// Whichever source a data directory came from, a relative one is relative.
func TestADataDirectoryFromAnySource(t *testing.T) {
	serve := []string{"tracepad", "serve"}
	for _, tc := range []struct {
		argv []string
		env  []string
		rel  bool
	}{
		{append(serve, "--data-dir", "/d"), nil, false},
		{append(serve, "--data-dir", "d"), nil, true},
		{serve, []string{"TRACEPAD_DATA_DIR=../shared/data", "HOME=/h"}, true},
		{serve, []string{"TRACEPAD_DATA_DIR=/srv/data"}, false},
		{serve, []string{"XDG_DATA_HOME=share", "HOME=/h"}, true},
		{serve, []string{"HOME=/h"}, false},
		{serve, nil, true},
	} {
		if got := relativeDataDir(Process{Argv: tc.argv, Env: tc.env}); got != tc.rel {
			t.Errorf("%q %q: relative %v", tc.argv, tc.env, got)
		}
	}
}

// What answers must have opened this data directory.
func TestANewServerThatOpenedAnotherDirectoryIsNotHealthy(t *testing.T) {
	w := newFakeWorld(t, 2)
	w.host.elsewhere[fNew] = t.TempDir()
	deps := w.deps()
	deps.StopWait = 300 * time.Millisecond // how long the start waits for the lock to be the new server's
	rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
	if code != exitWentBack || !strings.Contains(rep.Summary, "did not open") {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	w.waitVersion(fOld)
}

// The upgrade and the plan reach one verdict on one state: a named server
// already current, another of the command's behind, is pending for both.
func TestThePlanAndTheUpgradeAgree(t *testing.T) {
	p := &plan{to: "0.2.0", later: []string{"server pid 2 (0.1.0): tracepad upgrade --data-dir /b"}}
	code, summary := verdictOf(p)
	if code != exitPending || !strings.Contains(summary, "server pid 2") {
		t.Errorf("%d %s", code, summary)
	}
}
