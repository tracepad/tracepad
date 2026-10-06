//go:build unix

package upgrade

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// A server something brings back on the data after the stop (a supervisor,
// a shell loop) cannot take the database while the command archives and
// swaps it: the command holds its lock from the stop to the start (spec 054
// #26, B).
func TestAServerBroughtBackAfterTheStopCannotTakeTheData(t *testing.T) {
	w := newFakeWorld(t, 2)
	deps := w.deps()
	var intruder Started
	deps.Fault = func(point string) error {
		if point == "swap.archive" {
			argv := []string{"tracepad", "serve", "--listen", freeAddr(w.t), "--data-dir", w.data}
			intruder, _ = w.host.Start(StartSpec{Path: w.install, Argv: argv, Dir: w.home, Log: filepath.Join(w.data, "server.log")})
		}
		return nil
	}
	rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
	if code != exitOK {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	if intruder == nil {
		t.Fatal("no intruder started")
	}
	select {
	case <-intruder.Exited():
	default:
		t.Error("the server brought back took the data while the command held it")
	}
	if n, _ := countTraces(filepath.Join(w.data, dataDBName)); n != 2 {
		t.Errorf("traces: %d", n)
	}
}

// Nothing may run from the binary at a version later than the one a
// replacement puts there: a way back of a binary-only run, after a server
// started on the newer binary, is refused (spec 054 #26, C).
func TestNoReplacementPutsAnOlderBinaryUnderANewerServer(t *testing.T) {
	w := newFakeWorld(t, 2)
	deps := w.deps()
	// The binary alone: stop the world's server first, so no server is the
	// command's.
	procs, _, _ := w.host.Candidates()
	for _, p := range procs {
		_ = w.host.Signal(p.PID, 15)
	}
	rep, code := runIn(t, context.Background(), deps, "--to", fNew)
	if code != exitOK || rep.Run == nil {
		t.Fatalf("binary-only upgrade: %d %s", code, rep.Summary)
	}
	argv := []string{"tracepad", "serve", "--listen", w.listen, "--data-dir", w.data}
	if _, err := w.host.Start(StartSpec{Path: w.install, Argv: argv, Dir: w.home, Log: filepath.Join(w.data, "server.log")}); err != nil {
		t.Fatal(err)
	}
	w.waitVersion(fNew)
	back, code := runIn(t, context.Background(), deps, "--back", rep.Run.ID)
	if code == exitOK || !strings.Contains(back.Summary, "its next restart would be "+fOld) {
		t.Fatalf("%d %s", code, back.Summary)
	}
	if v, _ := scriptVersion(context.Background(), w.install); v != fNew {
		t.Errorf("the binary is %s", v)
	}
}

// A --check that finds healthy what the upgrade left to decide does what a
// healthy upgrade does after it (spec 054 #26, E).
func TestACheckThatFindsItHealthyFinishesTheRun(t *testing.T) {
	w := newFakeWorld(t, 2)
	deps := w.deps()
	w.host.uncounted[fNew] = true
	rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
	if code != exitDecide {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	if st, _ := loadState(rep.Run.Dir); st.has(stepSkill) {
		t.Fatal("finished before it was healthy")
	}
	w.host.mu.Lock()
	w.host.uncounted[fNew] = false
	w.host.mu.Unlock()
	chk, code := runIn(t, context.Background(), deps, "--check", rep.Run.ID)
	if code != exitOK {
		t.Fatalf("--check: %d %s", code, chk.Summary)
	}
	if st, _ := loadState(rep.Run.Dir); !st.has(stepSkill) {
		t.Error("--check found it healthy and did not finish the run")
	}
}
