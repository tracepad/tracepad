//go:build unix

package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	t.Parallel()
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
	t.Parallel()
	w := newFakeWorld(t, 2)
	deps := w.deps()
	// The binary alone: stop the world's server first, so no server is the
	// command's.
	procs, _, _ := w.host.Candidates(context.Background())
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
	t.Parallel()
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
	t.Parallel()
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
					if held, err := lockHeld(w.data); !held || err != nil {
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
	t.Parallel()
	w := newFakeWorld(t, 2)
	procs, _, _ := w.host.Candidates(context.Background())
	for _, p := range procs {
		_ = w.host.Signal(p.PID, 15)
	}
	scriptBinary(t, w.install, fNew)
	h := &startHost{fakeHost: w.host}
	deps := w.deps()
	deps.Sys = h
	st := &State{Kind: kindProcess, Process: &ProcessState{DataDir: w.data, Log: filepath.Join(w.data, "server.log")}}
	j := &job{r: &runner{deps: deps}, rep: &Report{}, dir: aRun(t, st), st: st,
		spec: ServerSpec{Exe: w.install, Argv: []string{"tracepad", "serve", "--listen", w.listen, "--data-dir", w.data}, Dir: w.home}}
	if ok, _ := j.hold(w.data); !ok {
		t.Fatal("the data could not be held")
	}
	h.start = func(StartSpec) (Started, error) {
		if held, _ := lockHeld(w.data); j.holds(w.data) || held {
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
// aRun gives a state a run's directory and fills in what a run records and
// the test does not care about, so it saves and loads as a run's (a state is
// written only when it loads back, the review of #226).
func aRun(t *testing.T, st *State) string {
	t.Helper()
	st.Run = newRunID(time.Now(), fOld)
	dir := filepath.Join(t.TempDir(), st.Run)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if st.From == "" {
		st.From, st.To = fOld, fNew
	}
	if ps := st.Process; ps != nil {
		if ps.PID == 0 {
			ps.PID = 1
		}
		if ps.Listen == "" {
			ps.Listen = "127.0.0.1:1"
		}
		ps.URL, _ = loopbackURL(ps.Listen)
		if ps.DataDir == "" {
			ps.DataDir = filepath.Join(dir, "data")
		}
		if ps.Log == "" {
			ps.Log = filepath.Join(ps.DataDir, "server.log")
		}
		ps.Old = filepath.Join(dir, "tracepad-"+st.From)
	}
	return dir
}

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
	t.Parallel()
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

// No binary is put under another server (spec 054 #38): an upgrade refuses
// before anything stops while any server but its own runs from the
// installed binary — of the old version or a later one, the command's, a
// service's, one open beyond this machine — since that server's next
// restart would be the new version with no backup and no check. A binary-
// only run is refused the same way.
func TestNoBinaryIsPutUnderAnotherServer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		listen func(t *testing.T) string
		args   func(w *fakeWorld) []string
		manage bool
	}{
		{"an old one beside the run's", func(t *testing.T) string { return freeAddr(t) }, func(w *fakeWorld) []string { return []string{"--to", fNew, "--data-dir", w.data} }, false},
		{"one open beyond this machine", func(t *testing.T) string { return "0.0.0.0:" + strings.Split(freeAddr(t), ":")[1] }, func(w *fakeWorld) []string { return []string{"--to", fNew, "--data-dir", w.data} }, false},
		{"a service's, under a binary-only run", nil, func(*fakeWorld) []string { return []string{"--to", fNew} }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newFakeWorld(t, 2)
			if tc.listen != nil {
				other := t.TempDir()
				argv := []string{"tracepad", "serve", "--listen", tc.listen(t), "--data-dir", other}
				if _, err := w.host.Start(StartSpec{Path: w.install, Argv: argv, Dir: w.home, Log: filepath.Join(other, "server.log")}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.manage {
				w.host.mu.Lock()
				for _, s := range w.host.procs {
					s.p.Manager = "the systemd unit tracepad.service"
				}
				w.host.mu.Unlock()
			}
			procs, _, _ := w.host.Candidates(context.Background())
			for _, mode := range [][]string{{"--plan"}, nil} {
				rep, code := runIn(t, context.Background(), w.deps(), append(mode, tc.args(w)...)...)
				if code != exitRefused || !strings.Contains(rep.Summary, "is not this run's") {
					t.Fatalf("%q: %d %s", mode, code, rep.Summary)
				}
			}
			for _, p := range procs {
				if !w.host.Alive(p.PID) {
					t.Errorf("pid %d was stopped", p.PID)
				}
			}
			if v, _ := scriptVersion(context.Background(), w.install); v != fOld {
				t.Errorf("the binary is %s", v)
			}
		})
	}
}

// A step off the table is never written (#34): the run ends stuck, saying
// what it was about to record, and the state on disk is as it was.
func TestAStepOffTheTableIsNeverWritten(t *testing.T) {
	saved := badStep
	badStep = func(string, string, string) {}
	t.Cleanup(func() { badStep = saved })
	st := &State{Kind: kindProcess, Steps: []Step{{Name: stepPrepared}}, Process: &ProcessState{}}
	dir := aRun(t, st)
	if err := st.save(dir); err != nil {
		t.Fatal(err)
	}
	j := &job{r: &runner{deps: Deps{Now: time.Now}}, rep: &Report{}, dir: dir, st: st}
	_ = j.st.save(dir)
	rep := &Report{}
	func() {
		defer stopOffTable(rep)
		j.step(stepStarted)
		t.Error("the step off the table was let through")
	}()
	if rep.ExitCode != exitStuck || !strings.Contains(rep.Summary, `"started" after "prepared"`) {
		t.Errorf("%d %s", rep.ExitCode, rep.Summary)
	}
	st, err := loadStateUnchecked(dir)
	if err != nil || len(st.Steps) != 1 {
		t.Errorf("written: %+v %v", st, err)
	}
}

// --check knows the run's server by the data's lock (the seventh review): the
// person who restarts it after a decide, the same version on the same data,
// gets healthy, not "exited".
func TestACheckFindsTheServerThePersonRestarted(t *testing.T) {
	t.Parallel()
	w := newFakeWorld(t, 2)
	deps := w.deps()
	w.host.uncounted[fNew] = true
	rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
	if code != exitDecide {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	st, _ := loadState(rep.Run.Dir)
	_ = w.host.Signal(st.Process.NewPID, 15)
	spec, _ := readSpec(rep.Run.Dir)
	if _, err := w.host.Start(StartSpec{Path: spec.Exe, Argv: spec.Argv, Env: spec.Env, Dir: spec.Dir, Log: st.Process.Log}); err != nil {
		t.Fatal(err)
	}
	w.host.mu.Lock()
	w.host.uncounted[fNew] = false
	w.host.mu.Unlock()
	chk, code := runIn(t, context.Background(), deps, "--check", rep.Run.ID)
	if code != exitOK {
		t.Fatalf("--check: %d %s", code, chk.Summary)
	}
}

// A way back takes back only what its run put at the install path (#31 (b),
// the seventh review): after a later run put a newer version there, the
// binary-only run's --back is refused with nothing touched.
func TestAWayBackLeavesALaterRunsBinary(t *testing.T) {
	t.Parallel()
	t.Run("binary only", func(t *testing.T) {
		w := newFakeWorld(t, 2)
		procs, _, _ := w.host.Candidates(context.Background())
		for _, p := range procs {
			_ = w.host.Signal(p.PID, 15)
		}
		deps := w.deps()
		a, code := runIn(t, context.Background(), deps, "--to", fNew)
		if code != exitOK {
			t.Fatalf("run A: %d %s", code, a.Summary)
		}
		if b, code := runIn(t, context.Background(), deps, "--to", fBroken); code != exitOK {
			t.Fatalf("run B: %d %s", code, b.Summary)
		}
		back, code := runIn(t, context.Background(), deps, "--back", a.Run.ID)
		if code == exitOK || !strings.Contains(back.Summary, "nothing was touched") {
			t.Errorf("--back A: %d %s", code, back.Summary)
		}
		if v, _ := scriptVersion(context.Background(), w.install); v != fBroken {
			t.Errorf("the binary is %s", v)
		}
	})
}

// Every check of the way back comes before its first act (spec 054 #37): a
// refusal on any of them leaves the server running, its data and the
// installed binary as they were, and nothing begun.
func TestAWayBackRefusedOnAPreconditionStopsNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		after func(w *fakeWorld, dir string)
		says  string
	}{
		{"a newer server on the binary", func(w *fakeWorld, _ string) {
			other := w.t.TempDir()
			argv := []string{"tracepad", "serve", "--listen", freeAddr(w.t), "--data-dir", other}
			if _, err := w.host.Start(StartSpec{Path: w.install, Argv: argv, Dir: w.home, Log: filepath.Join(other, "server.log")}); err != nil {
				w.t.Fatal(err)
			}
		}, "its next restart would be " + fOld},
		{"the binary changed since", func(w *fakeWorld, _ string) {
			scriptBinary(w.t, w.install+".next", fBroken)
			_ = os.Rename(w.install+".next", w.install)
		}, "which this run did not put there"},
		{"the archive changed since", func(w *fakeWorld, dir string) {
			f, _ := os.OpenFile(filepath.Join(dir, "data.tar.gz"), os.O_APPEND|os.O_WRONLY, 0)
			_, _ = f.Write([]byte("x"))
			f.Close()
		}, "data.tar.gz"},
		{"a foreign holder of the data", func(w *fakeWorld, _ string) {
			procs, _, _ := w.host.Candidates(context.Background())
			for _, p := range procs {
				_ = w.host.Signal(p.PID, 15)
			}
			// The same arguments, from another binary: someone else's.
			elsewhere := filepath.Join(w.t.TempDir(), "tracepad")
			scriptBinary(w.t, elsewhere, fNew)
			argv := []string{"tracepad", "serve", "--listen", w.listen, "--data-dir", w.data}
			if _, err := w.host.Start(StartSpec{Path: elsewhere, Argv: argv, Dir: w.home, Log: filepath.Join(w.data, "server.log")}); err != nil {
				w.t.Fatal(err)
			}
		}, "not this directory's server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newFakeWorld(t, 2)
			deps := w.deps()
			rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
			if code != exitOK {
				t.Fatalf("%d %s", code, rep.Summary)
			}
			w.addTrace()
			tc.after(w, rep.Run.Dir)
			holder, _ := lockedBy(w.data)
			bin, _ := scriptVersion(context.Background(), w.install)
			back, code := runIn(t, context.Background(), deps, "--back", rep.Run.ID)
			if code == exitOK || !strings.Contains(back.Summary, tc.says) || !strings.Contains(back.Summary, "nothing was touched") {
				t.Fatalf("%d %s", code, back.Summary)
			}
			if now, _ := lockedBy(w.data); now != holder || !w.host.Alive(holder) {
				t.Errorf("the server on the data changed: %d, was %d", now, holder)
			}
			if n, _ := countTraces(filepath.Join(w.data, dataDBName)); n != 3 {
				t.Errorf("the data changed: %d traces", n)
			}
			if v, _ := scriptVersion(context.Background(), w.install); v != bin {
				t.Errorf("the binary changed: %s, was %s", v, bin)
			}
			if st, _ := loadState(rep.Run.Dir); st.has(stepBackBegun) {
				t.Errorf("the way back began: %+v", st.Steps)
			}
		})
	}
}

// An older tracepad first on PATH runs nothing (the eighth review): the plan
// names it with what upgrades it, in a note, and does not call anything
// behind — exit 0, not the 4 the install script reads as "still running an
// older version".
func TestAnOlderBinaryElsewhereIsNamedNotCounted(t *testing.T) {
	t.Parallel()
	w := newFakeWorld(t, 2)
	deps := w.deps()
	if rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data); code != exitOK {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	brew := filepath.Join(t.TempDir(), "homebrew", "bin", "tracepad")
	_ = os.MkdirAll(filepath.Dir(brew), 0o700)
	scriptBinary(t, brew, fOld)
	deps.LookPath = func(string) string { return brew }
	plan, code := runIn(t, context.Background(), deps, "--plan", "--to", fNew)
	if code != exitOK || !strings.Contains(strings.Join(plan.Notes, "\n"), "comes first on PATH: "+brew+" ("+fOld+")") || len(plan.Person) > 0 {
		t.Errorf("%d %s %q %q", code, plan.Summary, plan.Person, plan.Notes)
	}
}

// A state the disk will not take ends the run there, stuck (spec 054 #37):
// going on would act on what is not recorded. Once the disk takes writes,
// --back reads what is on disk and goes back.
func TestAStateThatCannotBeWrittenStopsTheRun(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("root writes any directory")
	}
	w := newFakeWorld(t, 2)
	deps := w.deps()
	var dir string
	deps.Fault = func(point string) error {
		if point == stepArchived {
			states, _ := filepath.Glob(filepath.Join(deps.Backups, "*", stateFile))
			dir = filepath.Dir(states[0])
			_ = os.Chmod(dir, 0o500)
		}
		return nil
	}
	rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
	_ = os.Chmod(dir, 0o700)
	if code != exitStuck || !strings.Contains(rep.Summary, "could not be written") {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	deps.Fault = nil
	backUntilDone(t, deps, filepath.Base(dir))
	w.waitVersion(fOld)
	if n, _ := countTraces(filepath.Join(w.data, dataDBName)); n != 2 {
		t.Errorf("traces: %d", n)
	}
}

// A state the disk will not take at "prepared" — the last step before the
// stop — is a refusal with nothing stopped, and the run's directory, which
// holds the server's environment, is removed (the final review).
func TestAStateUnwrittenBeforeTheStopIsARefusal(t *testing.T) {
	t.Parallel()
	w := newFakeWorld(t, 2)
	deps := w.deps()
	deps.Fault = func(point string) error {
		if point == stepPrepared+recordPoint {
			// The state's name taken by a directory with something in it:
			// the rename that writes the state fails.
			states, _ := filepath.Glob(filepath.Join(deps.Backups, "*", stateFile))
			_ = os.Remove(states[0])
			_ = os.MkdirAll(filepath.Join(states[0], "x"), 0o700)
		}
		return nil
	}
	pid, _ := lockedBy(w.data)
	rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
	if code != exitRefused || !strings.Contains(rep.Summary, "could not be written") || !strings.Contains(rep.Summary, "nothing was stopped") || rep.Run != nil {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	if !w.host.Alive(pid) || w.answers() != fOld {
		t.Error("the server was stopped")
	}
	if runs, _ := filepath.Glob(filepath.Join(deps.Backups, "*", "server.json")); len(runs) > 0 {
		t.Errorf("the run's environment was left: %v", runs)
	}
}

// A server whose working directory is gone starts again in its data
// directory only when nothing it reads is relative to the one gone: a token
// file named relative to it, as its data directory, refuses before the stop
// (the twelfth review), the server still running.
func TestARelativePathWithItsDirectoryGoneRefuses(t *testing.T) {
	t.Parallel()
	w := newFakeWorld(t, 2)
	pid, _ := lockedBy(w.data)
	gone := filepath.Join(w.home, "worktree")
	w.host.mu.Lock()
	w.host.procs[pid].p.Cwd = gone
	w.host.procs[pid].p.Env = append(w.host.procs[pid].p.Env, "TRACEPAD_ADMIN_TOKEN_FILE=secrets/admin-token")
	w.host.mu.Unlock()
	rep, code := runIn(t, context.Background(), w.deps(), "--to", fNew, "--data-dir", w.data)
	if code != exitRefused || !strings.Contains(rep.Summary, `its TRACEPAD_ADMIN_TOKEN_FILE "secrets/admin-token" may be relative to it`) {
		t.Errorf("%d %s", code, rep.Summary)
	}
	if !w.host.Alive(pid) || w.answers() != fOld {
		t.Error("the server was stopped")
	}
}

// What settling cannot look at is neither done nor undone (spec 054 #37): a
// set-aside whose directories cannot be read, or an install path that cannot
// be — which would read as "taken off" — refuses, and is not taken back.
func TestSettlingRefusesWhatItCannotSee(t *testing.T) {
	t.Parallel()
	if os.Getuid() == 0 {
		t.Skip("root reads any directory")
	}
	parent := filepath.Join(t.TempDir(), "p")
	if err := os.MkdirAll(filepath.Join(parent, "data"), 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "bin")
	_ = os.Mkdir(bin, 0o700)
	scriptBinary(t, filepath.Join(bin, "tracepad"), "0.2.0")
	steps := []Step{{Name: stepPrepared}, {Name: stepStopSent}, {Name: stepStopped}, {Name: stepArchived}, {Name: stepStarted}, {Name: stepBackBegun}, {Name: stepBackRestored}}
	for _, tc := range []struct {
		pending string
		steps   []Step
		locked  string
	}{
		{stepBackAside, append(slices.Clone(steps), Step{Name: stepBackCleared}), parent},
		{stepBackCleared, steps, bin},
	} {
		j := &job{r: &runner{deps: Deps{Now: time.Now, Version: scriptVersion}}, rep: &Report{}, dir: t.TempDir(),
			st: &State{Kind: kindProcess, Run: "r", From: "0.1.0", Process: &ProcessState{DataDir: filepath.Join(parent, "data")},
				Binary: &BinaryState{Path: filepath.Join(bin, "tracepad")}, Steps: tc.steps,
				Pending: &Intent{Step: tc.pending, What: "the act"}}}
		if err := os.Chmod(tc.locked, 0); err != nil {
			t.Fatal(err)
		}
		err := j.settle(context.Background())
		_ = os.Chmod(tc.locked, 0o700)
		if err == nil || j.st.Pending == nil || j.st.has(tc.pending) {
			t.Errorf("%s: settled what it could not see: %v, pending %v", tc.pending, err, j.st.Pending)
		}
	}
}

// A server named that does not say its version is not taken for current
// (the eighth review): it is the person's (classifyServer), and the plan's
// order of versions refuses one too, as a second line.
func TestANamedServerThatDoesNotAnswerIsNotCurrent(t *testing.T) {
	t.Parallel()
	w := newFakeWorld(t, 2)
	deps := w.deps()
	w.host.setSlow(time.Second)
	deps.HTTP = &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 50 * time.Millisecond}
	rep, code := runIn(t, context.Background(), deps, "--plan", "--to", fNew, "--data-dir", w.data)
	w.host.setSlow(0)
	if code != exitRefused || !strings.Contains(rep.Summary, "does not answer") && !strings.Contains(rep.Summary, "does not say its version") {
		t.Errorf("%d %s", code, rep.Summary)
	}
}

// The plan calls a binary in Homebrew's Cellar the person's, a link or not,
// with what upgrades it (the ninth review).
func TestABinaryInTheCellarIsThePersons(t *testing.T) {
	t.Parallel()
	cellar := filepath.Join(t.TempDir(), "Cellar", "tracepad", "0.1.0", "bin")
	_ = os.MkdirAll(cellar, 0o755)
	scriptBinary(t, filepath.Join(cellar, "tracepad"), "0.1.0")
	deps := containerDeps(t, newFakeDocker(t))
	deps.InstallDir = cellar
	rep, code := runReport(t, deps, "--plan")
	if rep.Binary == nil || rep.Binary.Whose != "person" || !strings.Contains(rep.Binary.Reason, "brew upgrade tracepad") || code == exitPending {
		t.Errorf("%d %+v", code, rep.Binary)
	}
}

// On a file system that cannot lock, "cannot tell" is never "free" (spec 054
// #39). Where the runs are, every mode refuses before it reads a run; where
// the data is, a way back refuses with nothing touched, the new version
// still running on its data.
func TestAFileSystemThatCannotLockIsNeverFree(t *testing.T) {
	w := newFakeWorld(t, 2)
	deps := w.deps()
	rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
	if code != exitOK {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	savedTry, savedLocked := tryLock, locked
	t.Cleanup(func() { tryLock, locked = savedTry, savedLocked })
	cannot := func(under string) {
		tryLock = func(db string) (func(), bool, error) {
			if strings.HasPrefix(db, under) {
				return nil, false, store.ErrLocksUnsupported
			}
			return savedTry(db)
		}
		locked = func(db string) (bool, error) {
			if strings.HasPrefix(db, under) {
				return false, store.ErrLocksUnsupported
			}
			return savedLocked(db)
		}
	}
	cannot(deps.Backups)
	for _, args := range [][]string{{"--back", rep.Run.ID}, {"--check", rep.Run.ID}} {
		if r, code := runIn(t, context.Background(), deps, args...); code != exitRefused || !strings.Contains(r.Summary, "cannot lock") {
			t.Errorf("%q, runs on a file system without locks: %d %s", args, code, r.Summary)
		}
	}
	cannot(w.data)
	pid, _ := lockedBy(w.data)
	back, code := runIn(t, context.Background(), deps, "--back", rep.Run.ID)
	if code == exitOK || !strings.Contains(back.Summary, "cannot lock") || !strings.Contains(back.Summary, "nothing was touched") {
		t.Errorf("--back, data on a file system without locks: %d %s", code, back.Summary)
	}
	if !w.host.Alive(pid) || w.answers() != fNew {
		t.Error("the new version was stopped")
	}
}

// The high fail-open sites of the audit of #223, each injected: what could
// not be read, listed or measured refuses, and nothing is stopped or moved
// (spec 054 #39).
func TestWhatCannotBeReadRefuses(t *testing.T) {
	setProc := func(w *fakeWorld, f func(p *Process)) {
		w.host.mu.Lock()
		defer w.host.mu.Unlock()
		for _, s := range w.host.procs {
			if !s.stopped {
				f(&s.p)
			}
		}
	}
	// Before the stop: the upgrade and its plan refuse, the server runs.
	for _, tc := range []struct {
		name  string
		setup func(w *fakeWorld)
		says  string
		// prepared: only the preparation reads it, and the plan does not.
		prepared bool
	}{
		{"processes that could not be read (1)", func(w *fakeWorld) { w.host.unread = 1 }, "could not be read", false},
		{"a server whose executable could not be read (5)", func(w *fakeWorld) {
			other := w.t.TempDir()
			argv := []string{"tracepad", "serve", "--listen", freeAddr(w.t), "--data-dir", other}
			s, err := w.host.Start(StartSpec{Path: w.install, Argv: argv, Dir: w.home, Log: filepath.Join(other, "server.log")})
			if err != nil {
				w.t.Fatal(err)
			}
			w.host.mu.Lock()
			w.host.procs[s.PID()].p.Exe = ""
			w.host.mu.Unlock()
		}, "is not this run's", false},
		{"a working directory that could not be read (6)", func(w *fakeWorld) { setProc(w, func(p *Process) { p.Cwd = "" }) }, "working directory could not be read", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newFakeWorld(t, 2)
			tc.setup(w)
			pid, _ := lockedBy(w.data)
			for _, mode := range [][]string{{"--plan"}, nil} {
				rep, code := runIn(t, context.Background(), w.deps(), append(mode, "--to", fNew, "--data-dir", w.data)...)
				if mode != nil && tc.prepared && code == exitPending {
					continue
				}
				if code != exitRefused || !strings.Contains(rep.Summary, tc.says) {
					t.Errorf("%q: %d %s", mode, code, rep.Summary)
				}
			}
			if !w.host.Alive(pid) || w.answers() != fOld {
				t.Error("the server was stopped")
			}
		})
	}
	// During a way back: it refuses with nothing touched, the new version
	// running on its data.
	for _, tc := range []struct {
		name   string
		setup  func(w *fakeWorld, newPID int)
		says   string
		global bool // sets a package seam: not beside the others
	}{
		{"processes that could not be read (1)", func(w *fakeWorld, _ int) { w.host.unread = 1 }, "could not be read", false},
		{"the room beside the data that could not be told (7)", func(w *fakeWorld, _ int) {
			saved := freeBytes
			freeBytes = func(string) (int64, error) { return 0, errors.New("statfs: not implemented") }
			w.t.Cleanup(func() { freeBytes = saved })
		}, "for a restore cannot be told", true},
		{"the run's own server that could not be read (9)", func(w *fakeWorld, newPID int) { w.host.unreadable[newPID] = true }, "nothing was touched", false},
	} {
		t.Run("back/"+tc.name, func(t *testing.T) {
			if !tc.global {
				t.Parallel()
			}
			w := newFakeWorld(t, 2)
			deps := w.deps()
			rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
			if code != exitOK {
				t.Fatalf("%d %s", code, rep.Summary)
			}
			st, _ := loadState(rep.Run.Dir)
			tc.setup(w, st.Process.NewPID)
			back, code := runIn(t, context.Background(), deps, "--back", rep.Run.ID)
			if code == exitOK || !strings.Contains(back.Summary, tc.says) {
				t.Errorf("%d %s", code, back.Summary)
			}
			if !w.host.Alive(st.Process.NewPID) || w.answers() != fNew {
				t.Error("the new version was stopped")
			}
			if after, _ := filepath.Glob(w.data + ".after-*"); len(after) > 0 {
				t.Errorf("set aside: %q", after)
			}
		})
	}
}

// Only what is not there is absent at the install path (8): one that cannot
// be looked at is not cleared, and the step is not written.
func TestAPathThatCannotBeLookedAtIsNotCleared(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root looks at any directory")
	}
	bin := filepath.Join(t.TempDir(), "bin")
	_ = os.Mkdir(bin, 0o700)
	scriptBinary(t, filepath.Join(bin, "tracepad"), fNew)
	data := t.TempDir()
	j := &job{r: &runner{deps: Deps{Now: time.Now, Version: scriptVersion}}, rep: &Report{}, dir: t.TempDir(),
		st: &State{Kind: kindProcess, From: fOld, Process: &ProcessState{DataDir: data}, Binary: &BinaryState{Path: filepath.Join(bin, "tracepad")},
			Steps: []Step{{Name: stepPrepared}, {Name: stepStopSent}, {Name: stepStopped}, {Name: stepArchived}, {Name: stepBackBegun}}}}
	if ok, err := j.hold(data); !ok {
		t.Fatal(err)
	}
	defer j.release()
	_ = os.Chmod(bin, 0)
	defer os.Chmod(bin, 0o700)
	if _, ok := j.clearPath(context.Background()); ok || j.st.has(stepBackCleared) {
		t.Errorf("cleared a path it could not look at: %+v", j.st.Steps)
	}
}

// The check at the replacement counts a server whose executable could not be
// read, as the plan does (5): it may run from the binary.
func TestAServerOfUnknownBinaryIsCounted(t *testing.T) {
	t.Parallel()
	w := newFakeWorld(t, 2)
	w.host.mu.Lock()
	for _, s := range w.host.procs {
		s.p.Exe = ""
	}
	w.host.mu.Unlock()
	r := &runner{deps: w.deps()}
	for _, upgrade := range []bool{true, false} {
		if err := r.serversOn(context.Background(), w.install, fNew, upgrade); err == nil || !strings.Contains(err.Error(), "does not say which binary") {
			t.Errorf("upgrade %v: %v", upgrade, err)
		}
	}
}

// A skill that was not installed again is a note, and its step is not
// written: the next --check tries again (17).
func TestASkillNotInstalledIsNotRecorded(t *testing.T) {
	t.Parallel()
	w := newFakeWorld(t, 2)
	deps := w.deps()
	marker := filepath.Join(w.home, ".claude", "skills", "tracepad", ".version")
	_ = os.MkdirAll(filepath.Dir(marker), 0o700)
	_ = os.WriteFile(marker, []byte(fOld), 0o600)
	deps.Skills = func(context.Context, string, string, ...string) (string, error) {
		return "no room", errors.New("exit 1")
	}
	rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
	if code != exitOK || !strings.Contains(strings.Join(rep.Notes, " "), "was not installed again") {
		t.Fatalf("%d %s %q", code, rep.Summary, rep.Notes)
	}
	if st, _ := loadState(rep.Run.Dir); st.has(stepSkill) {
		t.Error("a skill not installed is recorded as installed")
	}
}

// An interrupt during the preparation ends the run before the stop, with
// nothing changed (the tenth review).
func TestAnInterruptBeforeTheStopStopsNothing(t *testing.T) {
	t.Parallel()
	w := newFakeWorld(t, 2)
	deps := w.deps()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The last thing the preparation does is ask the count; the interrupt
	// arrives with it.
	deps.Getenv = func(k string) string {
		if k == "TRACEPAD_API_KEY" {
			cancel()
			return testSKFake
		}
		return ""
	}
	pid, _ := lockedBy(w.data)
	rep, code := runIn(t, ctx, deps, "--to", fNew, "--data-dir", w.data)
	if code != exitRefused || !strings.Contains(rep.Summary, "Interrupted before anything stopped") {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	if !w.host.Alive(pid) || w.answers() != fOld {
		t.Error("the server was stopped")
	}
	if entries, _ := os.ReadDir(deps.Backups); len(entries) > 1 {
		t.Errorf("runs left: %v", entries)
	}
}

// A plan an interrupt cuts short — the install script's watchdog sends
// SIGTERM — ends with exit 1 and no verdict, whatever it had found by then
// (the final review): the install script reads that as "could not check".
// The upgrade's own plan stops the same way, with nothing made.
func TestAnInterruptedPlanSaysNothing(t *testing.T) {
	t.Parallel()
	w := newFakeWorld(t, 2)
	for _, mode := range [][]string{{"--plan"}, {}} {
		deps := w.deps()
		ctx, cancel := context.WithCancel(context.Background())
		version := deps.Version
		deps.Version = func(c context.Context, path string) (string, error) {
			cancel()
			return version(c, path)
		}
		rep, code := runIn(t, ctx, deps, append(mode, "--to", fNew)...)
		cancel()
		if code != exitRefused || !strings.HasPrefix(rep.Summary, "Interrupted:") || len(rep.Servers) > 0 || len(rep.Person) > 0 || rep.Run != nil {
			t.Errorf("%q: %d %s %+v", mode, code, rep.Summary, rep)
		}
		if entries, _ := os.ReadDir(deps.Backups); len(entries) > 0 {
			t.Errorf("%q: made %v", mode, entries)
		}
	}
	if w.answers() != fOld {
		t.Error("the server changed")
	}
}

// Past the latest stable release — a candidate installed — with no version
// named, there is nothing to do: not a downgrade (the tenth review). Named,
// the older version is refused as before.
func TestACandidatePastTheLatestIsNothingToDo(t *testing.T) {
	t.Parallel()
	w := newFakeWorld(t, 2)
	deps := w.deps()
	if rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data); code != exitOK {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	// The latest stable release is the old one.
	latest := filepath.Join(t.TempDir(), "mirror")
	mirrorRelease(t, latest, fOld, fOld, true)
	mirrorRelease(t, latest, fNew, fNew, false)
	deps.Releases = fakeReleases(t, latest)
	for _, mode := range [][]string{{"--plan"}, nil} {
		rep, code := runIn(t, context.Background(), deps, mode...)
		if code != exitOK || !strings.Contains(rep.Summary, "Nothing to do") {
			t.Errorf("%q: %d %s", mode, code, rep.Summary)
		}
	}
	if rep, code := runIn(t, context.Background(), deps, "--plan", "--to", fOld); code != exitRefused {
		t.Errorf("--to the older: %d %s", code, rep.Summary)
	}
	if w.answers() != fNew {
		t.Error("something changed")
	}
}

// The way back's check of what runs from the install path comes before the
// stop too (the twelfth review): with the binary already the target, a
// server that is not the run's runs it at that version — a way back would
// put the old binary under it — so the upgrade refuses with the run's
// server still running.
func TestTheWayBacksServerCheckComesBeforeTheStop(t *testing.T) {
	t.Parallel()
	w := newFakeWorld(t, 2)
	scriptBinary(t, w.install, fNew)
	other := t.TempDir()
	argv := []string{"tracepad", "serve", "--listen", "0.0.0.0:" + strings.Split(freeAddr(t), ":")[1], "--data-dir", other}
	if _, err := w.host.Start(StartSpec{Path: w.install, Argv: argv, Dir: w.home, Log: filepath.Join(other, "server.log")}); err != nil {
		t.Fatal(err)
	}
	pid, _ := lockedBy(w.data)
	rep, code := runIn(t, context.Background(), w.deps(), "--to", fNew, "--data-dir", w.data)
	if code != exitRefused || !strings.Contains(rep.Summary, "nothing changed: its way back could not be taken") || !strings.Contains(rep.Summary, "at "+fNew+" on ") || !strings.Contains(rep.Summary, "its next restart would be "+fOld) {
		t.Errorf("%d %s", code, rep.Summary)
	}
	if !w.host.Alive(pid) || w.answers() != fOld {
		t.Error("the server was stopped")
	}
}

// A server listening on every address of the machine — 0.0.0.0, or [::] —
// is asked its version on the loopback address of the same family, which
// reaches it (the thirteenth review): the plan says what it runs, rather
// than that it does not say.
func TestAServerOnEveryAddressIsAskedOnLoopback(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"0.0.0.0", "[::]"} {
		w := newFakeWorld(t, 2)
		addr := host + ":" + strings.Split(freeAddr(t), ":")[1]
		if host == "[::]" {
			l, err := net.Listen("tcp", "[::1]:0")
			if err != nil {
				t.Logf("no IPv6 loopback here: %v", err)
				continue
			}
			addr = host + ":" + strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
			l.Close()
		}
		other := t.TempDir()
		argv := []string{"tracepad", "serve", "--listen", addr, "--data-dir", other}
		s, err := w.host.Start(StartSpec{Path: w.install, Argv: argv, Dir: w.home, Log: filepath.Join(other, "server.log")})
		if err != nil {
			t.Fatal(err)
		}
		rep, _ := runIn(t, context.Background(), w.deps(), "--plan", "--to", fNew)
		found := false
		for _, srv := range rep.Servers {
			if srv.PID == s.PID() {
				found = true
				if srv.Version != fOld {
					t.Errorf("%s: the server on every address reads %q, not %s", host, srv.Version, fOld)
				}
			}
		}
		if !found {
			t.Errorf("%s: the server on every address is not in the report: %+v", host, rep.Servers)
		}
	}
}

// The final review's example: a release candidate installed past the latest
// stable release, and the command's server still running the stable one
// from it. With no version named the target is the candidate, not the
// stable release: the plan never calls that "nothing to do", and never takes
// the server to a candidate unasked — it is the person's, with the command
// that does it (exit 4). Named, the candidate is upgraded to as any version.
func TestAServerBehindAnInstalledCandidateIsThePersons(t *testing.T) {
	t.Parallel()
	const rc = "0.5.1-rc.1"
	w := newFakeWorld(t, 2)
	scriptBinary(t, w.install, rc)
	deps := w.deps()
	mirror := filepath.Join(t.TempDir(), "mirror")
	mirrorRelease(t, mirror, fOld, fOld, true)
	mirrorRelease(t, mirror, rc, rc, false)
	deps.Releases = fakeReleases(t, mirror)
	for _, mode := range [][]string{{"--plan"}, nil} {
		rep, code := runIn(t, context.Background(), deps, mode...)
		person := strings.Join(rep.Person, "\n")
		if code != exitDecide || rep.To != rc || rep.Run != nil || !strings.Contains(person, "upgrade --to "+rc+" --data-dir "+w.data) {
			t.Errorf("%q: %d to %s: %s\n%s", mode, code, rep.To, rep.Summary, person)
		}
	}
	if w.answers() != fOld {
		t.Fatal("the server was taken to the candidate unasked")
	}
	if rep, code := runIn(t, context.Background(), deps, "--to", rc, "--data-dir", w.data); code != exitOK {
		t.Fatalf("named: %d %s", code, rep.Summary)
	}
	w.waitVersion(rc)
	if rep, code := runIn(t, context.Background(), deps, "--plan"); code != exitOK || !strings.Contains(rep.Summary, "Nothing to do") {
		t.Errorf("after: %d %s", code, rep.Summary)
	}
}

// Two of the command's servers on one binary that needs replacing: the plan
// refuses up front, as an upgrade of either one would (the tenth review),
// and says how to go on.
func TestTwoServersOnOneBinaryAreRefusedUpFront(t *testing.T) {
	t.Parallel()
	w := newFakeWorld(t, 2)
	other := filepath.Join(w.home, "other")
	_ = os.MkdirAll(other, 0o700)
	_ = os.WriteFile(filepath.Join(other, dataDBName), templateDB(t, 1), 0o600)
	argv := []string{"tracepad", "serve", "--listen", freeAddr(t), "--data-dir", other}
	if _, err := w.host.Start(StartSpec{Path: w.install, Argv: argv, Dir: w.home, Log: filepath.Join(other, "server.log")}); err != nil {
		t.Fatal(err)
	}
	deps := w.deps()
	plan, code := runIn(t, context.Background(), deps, "--plan", "--to", fNew)
	if code != exitRefused || !strings.Contains(plan.Summary, "stop all of them but one") {
		t.Errorf("--plan: %d %s", code, plan.Summary)
	}
	for _, dir := range []string{w.data, other} {
		if rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", dir); code != exitRefused || !strings.Contains(rep.Summary, "is not this run's") {
			t.Errorf("--data-dir %s: %d %s", dir, code, rep.Summary)
		}
	}
}

// A PID with its start names one process (spec 054 #40): the server as the
// run recorded it, or someone else's — another start, another user's — and
// only one of this user's that cannot be read is an error.
func TestAProcessIsItsPIDAndItsStart(t *testing.T) {
	t.Parallel()
	w := newFakeWorld(t, 2)
	r := &runner{deps: w.deps()}
	procs, _, _ := w.host.Candidates(context.Background())
	p := procs[0]
	spec := ServerSpec{Exe: p.Exe, Argv: p.Argv}
	for _, tc := range []struct {
		name  string
		start int64
		ok    bool
	}{{"as recorded", p.Start, true}, {"no start recorded", 0, true}, {"another start: the pid reused", p.Start + 1, false}} {
		if ok, err := r.isServer(p.PID, tc.start, w.data, spec); ok != tc.ok || err != nil {
			t.Errorf("%s: %v %v", tc.name, ok, err)
		}
		if ok, err := r.startedAs(p.PID, tc.start, spec); ok != tc.ok || err != nil {
			t.Errorf("startedAs, %s: %v %v", tc.name, ok, err)
		}
	}
	w.host.foreign[p.PID] = true
	if ok, err := r.isServer(p.PID, 0, w.data, spec); ok || err != nil {
		t.Errorf("another user's: %v %v", ok, err)
	}
	delete(w.host.foreign, p.PID)
	w.host.unreadable[p.PID] = true
	if _, err := r.isServer(p.PID, 0, w.data, spec); err == nil {
		t.Error("one of this user's that cannot be read: no error")
	}
}

// Counts not compared are said by the check in one sentence, whatever its
// verdict, with the reason the read before the stop found (the fourth and
// fifth reviews of #228): no key, and a server that exits (exit 3) or does
// not answer (exit 4); a key the old server refused. The run's notes do not
// say it again.
func TestCountsNotComparedAreSaidOnceWithWhy(t *testing.T) {
	t.Parallel()
	const noKeyNote = "the trace counts were not compared: no TRACEPAD_API_KEY in the environment"
	for name, c := range map[string]struct {
		to      string
		key     bool
		slow    bool
		refused bool
		code    int
		want    string
	}{
		"no key, a server that exits":           {to: fBroken, code: exitWentBack, want: noKeyNote},
		"no key, a server that does not answer": {to: fNew, slow: true, code: exitDecide, want: noKeyNote},
		"a key the old server refused": {to: fNew, key: true, refused: true, code: exitOK,
			want: "the trace counts were not compared: /api/v1/system answered HTTP 401"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newFakeWorld(t, 2)
			deps := w.deps()
			if !c.key {
				deps.Getenv = func(string) string { return "" }
			}
			if c.refused {
				w.host.mu.Lock()
				w.host.uncounted[fOld] = true
				w.host.mu.Unlock()
			}
			deps.HealthWait = 300 * time.Millisecond
			// Not answering is a request past the client's deadline,
			// kept short: the wait is the test's whole length.
			deps.HTTP = &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 500 * time.Millisecond}
			deps.Fault = func(point string) error {
				if c.slow && point == stepStarted {
					w.host.setSlow(2 * time.Second)
				}
				return nil
			}
			rep, code := runIn(t, context.Background(), deps, "--to", c.to, "--data-dir", w.data)
			w.host.setSlow(0)
			if code != c.code || rep.Check == nil || rep.Check.CountNote != c.want ||
				strings.Contains(strings.Join(rep.Notes, "\n"), "not compared") {
				t.Fatalf("exit %d, %s, check %+v, notes %q", code, rep.Summary, rep.Check, rep.Notes)
			}
		})
	}
}

// A tracepad first on PATH is a fact, in a note, sorted into neither "Yours,
// to do" nor "Yours, nothing to do" (the eighth and ninth reviews of #228):
// Homebrew's at the target, an older one first on PATH.
func TestATracepadFirstOnPathIsANoteOnly(t *testing.T) {
	t.Parallel()
	cellar := filepath.Join(t.TempDir(), "Cellar", "tracepad", "0.2.0", "bin")
	_ = os.MkdirAll(cellar, 0o755)
	scriptBinary(t, filepath.Join(cellar, "tracepad"), "0.2.0")
	first := filepath.Join(t.TempDir(), "tracepad")
	scriptBinary(t, first, "0.1.0")
	deps := containerDeps(t, newFakeDocker(t))
	deps.InstallDir = cellar
	deps.LookPath = func(string) string { return first }
	var out bytes.Buffer
	run(context.Background(), Options{Args: []string{"--plan"}, Stdout: &out, Stderr: io.Discard}, deps)
	text := out.String()
	_, notes, _ := strings.Cut(text, "\nNotes:\n")
	if strings.Count(text, first) != 1 || !strings.Contains(notes, "another tracepad comes first on PATH: "+first+" (0.1.0)") {
		t.Errorf("%s", text)
	}
	rep, _ := runReport(t, deps, "--plan")
	if rep.Binary.First != first || rep.Binary.FirstVersion != "0.1.0" || !rep.Binary.Idle {
		t.Errorf("%+v", rep.Binary)
	}
}
