//go:build unix

package upgrade

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/store"
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
		if point == stepArchived {
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

// startHost is a fake machine whose starts of one version a test decides.
type startHost struct {
	*fakeHost
	start func(spec StartSpec) (Started, error)
}

func (h *startHost) Start(spec StartSpec) (Started, error) {
	if v, _ := scriptVersion(context.Background(), spec.Path); v == fNew && h.start != nil {
		return h.start(spec)
	}
	return h.fakeHost.Start(spec)
}

// Every start goes through launch (spec 054 #32): the command lets go of the
// data only for the start itself, and holds it again whenever the start does
// not leave it to the server it started — so the way back that follows finds
// the data as the command left it, and nothing else on it.
func TestTheDataIsHeldAcrossEveryStart(t *testing.T) {
	for _, tc := range []struct {
		name string
		with func(w *fakeWorld, h *startHost)
	}{
		{"a start that fails", func(w *fakeWorld, h *startHost) {
			h.start = func(StartSpec) (Started, error) { return nil, errInjected }
		}},
		{"a server that exits at once", func(w *fakeWorld, h *startHost) { w.host.broken[fNew] = true }},
		{"a server that opens another directory", func(w *fakeWorld, h *startHost) { w.host.elsewhere[fNew] = t.TempDir() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newFakeWorld(t, 2)
			h := &startHost{fakeHost: w.host}
			tc.with(w, h)
			deps := w.deps()
			deps.Sys = h
			deps.StopWait = 300 * time.Millisecond
			held := false
			deps.Fault = func(point string) error {
				if point == stepBackRestored && !held {
					held = true
					if !lockHeld(w.data) {
						t.Error("the way back began with the data let go")
					}
				}
				return nil
			}
			rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
			if code != exitWentBack || !held {
				t.Fatalf("%d %s", code, rep.Summary)
			}
			w.waitVersion(fOld)
		})
	}
	// Something that takes the lock in the moment it is free is named, and
	// the way back takes the data from it.
	t.Run("a thief in the moment the lock is free", func(t *testing.T) {
		t.Parallel()
		w := newFakeWorld(t, 2)
		h := &startHost{fakeHost: w.host}
		h.start = func(spec StartSpec) (Started, error) {
			if _, err := w.host.Start(spec); err != nil { // the thief, as a supervisor starts it
				return nil, err
			}
			return w.host.Start(spec) // the command's, which finds the lock taken
		}
		deps := w.deps()
		deps.Sys = h
		rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
		if code != exitWentBack || !strings.Contains(rep.Summary, "in the moment it was free") {
			t.Fatalf("%d %s", code, rep.Summary)
		}
		w.waitVersion(fOld)
		if n, _ := countTraces(filepath.Join(w.data, dataDBName)); n != 2 {
			t.Errorf("traces: %d", n)
		}
	})
}

// launch itself (#32): the data is the command's up to the start, let go
// for the start alone, and the command's again after a start that fails;
// after one that works it is the server's.
func TestLaunchHoldsTheDataOnEitherSideOfTheStart(t *testing.T) {
	w := newFakeWorld(t, 2)
	procs, _, _ := w.host.Candidates()
	for _, p := range procs {
		_ = w.host.Signal(p.PID, 15)
	}
	scriptBinary(t, w.install, fNew)
	h := &startHost{fakeHost: w.host}
	deps := w.deps()
	deps.Sys = h
	j := &job{r: &runner{deps: deps}, rep: &Report{}, dir: t.TempDir(),
		st:   &State{Kind: kindProcess, Process: &ProcessState{DataDir: w.data, Log: filepath.Join(w.data, "server.log")}},
		spec: ServerSpec{Exe: w.install, Argv: []string{"tracepad", "serve", "--listen", w.listen, "--data-dir", w.data}, Dir: w.home}}
	if ok, _ := j.hold(w.data); !ok {
		t.Fatal("the data could not be held")
	}
	h.start = func(StartSpec) (Started, error) {
		if j.holds(w.data) || lockHeld(w.data) {
			t.Error("the data is held at the start itself")
		}
		return nil, errInjected
	}
	if _, err := j.launch(context.Background(), true); err == nil {
		t.Fatal("a start that failed: no error")
	}
	if !j.holds(w.data) {
		t.Error("a start that failed left the data let go")
	}
	if st, err := loadStateUnchecked(j.dir); err != nil || !st.Process.WroteAfterSwap {
		t.Errorf("the new version's start was not recorded before it: %v", err)
	}
	h.start = nil
	started, err := j.launch(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if j.holds(w.data) || !recordsPID(w.data, started.PID()) {
		t.Error("after a start that worked, the data is not the server's")
	}
	j.release()
}

// loadStateUnchecked reads a state.json as written, for a job made in a test.
func loadStateUnchecked(dir string) (*State, error) {
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return nil, err
	}
	var st State
	return &st, json.Unmarshal(b, &st)
}

// The lock on runs is taken before a run's state is read (#30): a state
// another invocation holds is not read at all.
func TestARunIsReadOnlyUnderTheLock(t *testing.T) {
	w := newFakeWorld(t, 2)
	deps := w.deps()
	dir := filepath.Join(deps.Backups, "20261006-120000-0.5.0-abc123")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, stateFile), []byte("not a state"), 0o600)
	release, ok, err := store.TryLock(filepath.Join(deps.Backups, "runs"))
	if err != nil || !ok {
		t.Fatal("the lock on runs", err)
	}
	defer release()
	for _, mode := range []string{"--back", "--check"} {
		rep, code := runIn(t, context.Background(), deps, mode, filepath.Base(dir))
		if code != exitRefused || !strings.Contains(rep.Summary, "another tracepad upgrade is running") {
			t.Errorf("%s: %d %s", mode, code, rep.Summary)
		}
	}
}

// What runs from the installed binary at a version that cannot be shown
// not to be later refuses the run before anything stops (the sixth review):
// the server the run would upgrade is never stopped.
func TestANewerServerOnTheBinaryRefusesBeforeTheStop(t *testing.T) {
	w := newFakeWorld(t, 2)
	other := t.TempDir()
	// Open beyond this machine: the person's, and not asked its version.
	argv := []string{"tracepad", "serve", "--listen", "0.0.0.0:" + strings.Split(freeAddr(t), ":")[1], "--data-dir", other}
	if _, err := w.host.Start(StartSpec{Path: w.install, Argv: argv, Dir: w.home, Log: filepath.Join(other, "server.log")}); err != nil {
		t.Fatal(err)
	}
	procs, _, _ := w.host.Candidates()
	rep, code := runIn(t, context.Background(), w.deps(), "--to", fNew, "--data-dir", w.data)
	if code != exitRefused || !strings.Contains(rep.Summary, "cannot be shown to be safe") {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	for _, p := range procs {
		if !w.host.Alive(p.PID) {
			t.Errorf("pid %d was stopped", p.PID)
		}
	}
}
