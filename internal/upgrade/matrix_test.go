//go:build unix

package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// The fault matrix (spec 054 #26, #31). Its cells are the steps of each kind
// of run's table, not a list kept here: every step at which the command acts
// is a point, and each point is three cells — the act failing there, an
// interrupt arriving there, and every server answering slower than the
// command waits from there on. A step added to the table gets its cells
// without anyone writing them, and a cell whose point no run passes fails:
// the table and the code cannot drift apart. After the fault, --back is run
// as a person would until it is done, then three times more, and every cell
// holds the same invariants: a server runs, the old version or the new; the
// data is whole; nothing is deleted; and a --back repeated changes nothing.

var errInjected = errors.New("injected")

// noAct are the steps no act happens at, so no cell: each is a record the
// steps around it act for.
var noAct = map[string]bool{
	stepPrepared:       true, // the plan's, before anything changes
	stepBackBegun:      true, // a record ahead of the way back's first act
	stepBinaryReplaced: true, // binary_replacing's act ends in it
	stepSkill:          true, // the skill's install is a note, never a fault
}

// stepOrder is the order the cells run in, and every step there is.
var stepOrder = []string{
	stepPrepared, stepStopSent, stepStopped, stepArchived, stepRenamedOld, stepBinaryReplacing, stepBinaryReplaced,
	stepStarted, stepChecked, stepSkill, stepBackBegun, stepBackRestored, stepBackVolume, stepBackCleared,
	stepBackAside, stepBackMoved, stepBackStarted, stepBackBinary, stepBackDone,
}

// pointsOf are a kind's cells: the swap's points, then the way back's.
func pointsOf(t *testing.T, kind string) (swap, back []string) {
	return stepsOf(t, kind, false)
}

// recordsOf are a kind's kill cells: every step there is, each killed between
// what it records and its record.
func recordsOf(t *testing.T, kind string) (swap, back []string) {
	return stepsOf(t, kind, true)
}

func stepsOf(t *testing.T, kind string, all bool) (swap, back []string) {
	t.Helper()
	m := machines[kind]
	for step := range m {
		if !slices.Contains(stepOrder, step) {
			t.Fatalf("%s is in the %s table and not in stepOrder", step, kind)
		}
	}
	for _, step := range stepOrder {
		if _, ok := m[step]; !ok || (noAct[step] && !all) {
			continue
		}
		if strings.HasPrefix(step, "back_") {
			back = append(back, step)
		} else {
			swap = append(swap, step)
		}
	}
	return swap, back
}

// inProcess is a matrix whose servers are this process's, and whose cells
// start no other: no fork holds a copy of a lock, so a lock found held is
// held, where asking again for 50 ms each time is most of a cell's time. Nor
// can a cell lose power: what reaches the disk is the integration tests' to
// show, and a full flush per step, from cells side by side, is the rest.
func inProcess(t *testing.T) {
	retry, sync := store.LockRetry, syncFile
	store.LockRetry = time.Millisecond
	syncFile = func(*os.File) error { return nil }
	t.Cleanup(func() { store.LockRetry, syncFile = retry, sync })
}

// The table is the steps the command records: every step any run records is
// in its kind's table, and every walk the tests make follows it (badStep).
func TestEveryStepIsInTheTable(t *testing.T) {
	for kind, m := range machines {
		for step, after := range m {
			if len(after) == 0 {
				t.Errorf("%s: %s follows nothing", kind, step)
			}
			for _, a := range after {
				if _, ok := m[a]; a != "" && !ok {
					t.Errorf("%s: %s follows %s, which is not in the table", kind, step, a)
				}
			}
		}
	}
	// A state off the table is not a run's.
	st := &State{Kind: kindProcess, Steps: []Step{{Name: stepPrepared}, {Name: stepStarted}}}
	if st.followsTable() == nil {
		t.Error("a start without a stop follows the table")
	}
}

// Every step a test run records must follow the table: a step off it fails
// the test that made it.
func init() {
	badStep = func(kind, last, next string) {
		panic(fmt.Sprintf("a %s run recorded %q after %q, off its table", kind, next, last))
	}
}

var faultKinds = []string{"fail", "interrupt", "slow"}

// slowBy is how long a slow cell's servers take to answer; slowWait, how long
// the command waits for an answer in such a cell.
const (
	slowBy   = 400 * time.Millisecond
	slowWait = 150 * time.Millisecond
)

// cell is one cell's fault: at its point, it fails, interrupts, or slows
// every server down; hit says whether a run passed the point.
type cell struct {
	point, kind string
	cancel      context.CancelFunc
	slow        func()
	hit         atomic.Bool
}

func (c *cell) fault(point string) error {
	if c.kind == "kill" {
		if point == c.point+recordPoint {
			c.hit.Store(true)
			panic(killed{})
		}
		return nil
	}
	if point != c.point {
		return nil
	}
	c.hit.Store(true)
	switch c.kind {
	case "interrupt":
		c.cancel()
		return nil
	case "slow":
		c.slow()
		return nil
	}
	return errInjected
}

// runIn runs the command with a context, and decodes its report.
func runIn(t *testing.T, ctx context.Context, deps Deps, args ...string) (Report, int) {
	t.Helper()
	var out bytes.Buffer
	code, killed := runKillable(ctx, Options{Args: append(args, "--json"), Stdout: &out, Stderr: io.Discard}, deps)
	if killed {
		return Report{}, codeKilled
	}
	var rep Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	return rep, code
}

// killed is the matrix's kill: the command stops where it is, between an
// act and the write of its step, as a SIGKILL or a lost machine stops it.
// What it held it lets go of, as a process that dies does.
type killed struct{}

// codeKilled is a killed run's status, for the tests.
const codeKilled = -9

// runKillable runs the command and says whether the matrix killed it.
func runKillable(ctx context.Context, opt Options, deps Deps) (code int, dead bool) {
	defer func() {
		if v := recover(); v != nil {
			if _, ok := v.(killed); !ok {
				panic(v)
			}
			code, dead = codeKilled, true
		}
	}()
	return run(ctx, opt, deps), false
}

// runOf is the one run a killed command left, found where runs are.
func runOf(t *testing.T, deps Deps) (id, dir string) {
	t.Helper()
	states, _ := filepath.Glob(filepath.Join(deps.Backups, "*", stateFile))
	if len(states) != 1 {
		t.Fatalf("runs left: %q", states)
	}
	dir = filepath.Dir(states[0])
	return filepath.Base(dir), dir
}

// afterKill is what a person does after a run was killed, in either order:
// --check, which finishes it or refuses with nothing touched, and --back
// until done. Whichever comes first meets the run as the kill left it.
func afterKill(t *testing.T, deps Deps, id string, backFirst bool) {
	t.Helper()
	st, err := loadState(filepath.Join(deps.Backups, id))
	if err != nil {
		t.Fatalf("the run a kill left does not load: %v", err)
	}
	// Killed once the new version was started on the data (its intent or
	// its step written), and before any way back: --check finds it healthy.
	started := (st.has(stepStarted) || (st.Pending != nil && st.Pending.Step == stepStarted)) && !st.has(stepBackBegun)
	check := func() {
		rep, code := runIn(t, context.Background(), deps, "--check", id)
		switch {
		case started && !backFirst && code != exitOK:
			t.Errorf("--check of a new version started before the kill: %d %s", code, rep.Summary)
		case code != exitOK && code != exitRefused:
			t.Errorf("--check after the kill: %d %s", code, rep.Summary)
		}
	}
	if !backFirst {
		check()
	}
	backUntilDone(t, deps, id)
	if backFirst {
		check()
	}
}

// orders are the two a person may meet a killed run in.
var orders = map[string]bool{"check-first": false, "back-first": true}

// backUntilDone runs --back as a person does, until it is done: at most
// three times.
func backUntilDone(t *testing.T, deps Deps, run string) {
	t.Helper()
	for i := 0; i < 3; i++ {
		rep, code := runIn(t, context.Background(), deps, "--back", run)
		if code == exitOK {
			return
		}
		t.Logf("--back %d: %d %s", i+1, code, rep.Summary)
	}
	t.Errorf("--back did not finish in three tries")
}

// backAgain runs --back three times more on a way back that is done: each
// ends as the first did, and changes nothing snapshot sees (#31).
func backAgain(t *testing.T, deps Deps, run string, snapshot func() string) {
	t.Helper()
	before := snapshot()
	for i := 0; i < 3; i++ {
		rep, code := runIn(t, context.Background(), deps, "--back", run)
		if code != exitOK {
			t.Errorf("--back again (%d): %d %s", i+1, code, rep.Summary)
		}
		if now := snapshot(); now != before {
			t.Errorf("--back again (%d) changed what runs or what is kept:\n%s\nwas\n%s", i+1, now, before)
		}
	}
}

// nothingDeleted: the run's directory, its archive when it took one, and
// everything it set aside are where the run left them.
func nothingDeleted(t *testing.T, dir string, exists func(item string) bool) {
	t.Helper()
	st, err := loadState(dir)
	if err != nil {
		t.Fatalf("the run's state: %v", err)
	}
	if st.Archive != nil {
		if _, err := os.Stat(filepath.Join(dir, "data.tar.gz")); err != nil {
			t.Errorf("the archive is gone: %v", err)
		}
	}
	for _, item := range st.SetAside {
		if !exists(item) {
			t.Errorf("set aside, and gone: %s", item)
		}
	}
}

// matrixContainer is the container the matrix upgrades, with the host's
// binary beside it, which a healthy run brings to the container's version.
func matrixContainer(t *testing.T) (*fakeDocker, Deps) {
	t.Helper()
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d)
	mirror := t.TempDir()
	for _, v := range []string{"0.1.0", "0.2.0", "0.2.1"} {
		mirrorRelease(t, mirror, v, v, v == "0.2.0")
	}
	deps.Releases = fakeReleases(t, mirror)
	deps.Version = scriptVersion
	if err := os.MkdirAll(deps.InstallDir, 0o700); err != nil {
		t.Fatal(err)
	}
	scriptBinary(t, filepath.Join(deps.InstallDir, "tracepad"), "0.1.0")
	return d, deps
}

func TestTheFaultMatrixOfAContainer(t *testing.T) {
	inProcess(t)
	swapPoints, backPoints := pointsOf(t, kindContainer)
	exists := func(d *fakeDocker) func(string) bool {
		return func(item string) bool {
			kind, name, _ := strings.Cut(item, " ")
			if kind == "volume" {
				_, ok := d.volumes[name]
				return ok
			}
			return d.byName[name] != nil
		}
	}
	// invariants: the container runs, the old image or the new, on a
	// volume that holds the traces it held; the fake fails any removal.
	invariants := func(t *testing.T, d *fakeDocker, dir string, images ...string) {
		t.Helper()
		c := d.byName["tracepad-app"]
		if c == nil || !c.State.Running || c.State.Restarting {
			t.Fatalf("no container runs as tracepad-app: %+v", c)
		}
		if img := c.Config.Image; !slices.Contains(images, img) {
			t.Errorf("it runs %s, not one of %q", img, images)
		}
		for _, m := range c.Mounts {
			if m.Destination == "/data" && d.volumes[m.Name] != 7 {
				t.Errorf("its volume %s holds %d traces, not 7", m.Name, d.volumes[m.Name])
			}
		}
		if dir != "" {
			nothingDeleted(t, dir, exists(d))
		}
	}
	snapshot := func(d *fakeDocker, dir string) func() string {
		return func() string {
			st, err := loadState(dir)
			if err != nil {
				return err.Error()
			}
			var names []string
			for name, c := range d.byName {
				names = append(names, fmt.Sprintf("%s=%s:%s:%v:%s", name, c.ID[60:], c.Config.Image, c.State.Running, c.HostConfig.RestartPolicy.Name))
			}
			slices.Sort(names)
			return fmt.Sprintf("containers %q\nvolumes %v\nset aside %q\nsteps %d", names, d.volumes, st.SetAside, len(st.Steps))
		}
	}
	const oldImage, newImage = "ghcr.io/tracepad/tracepad:0.1.0", "ghcr.io/tracepad/tracepad:0.2.0"
	slowDeps := func(deps *Deps) {
		deps.HTTP = &http.Client{Transport: deps.HTTP.Transport, Timeout: slowWait}
	}

	for _, kind := range faultKinds {
		for _, point := range swapPoints {
			t.Run("upgrade/"+kind+"/"+point, func(t *testing.T) {
				t.Parallel()
				d, deps := matrixContainer(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				c := &cell{point: point, kind: kind, cancel: cancel, slow: func() { d.slow = slowBy }}
				deps.Fault = c.fault
				fast := deps.HTTP
				if kind == "slow" {
					slowDeps(&deps)
				}
				rep, code := runIn(t, ctx, deps)
				deps.Fault, deps.HTTP, d.slow = nil, fast, 0
				if !c.hit.Load() {
					t.Fatalf("no upgrade passes %s, a step of the table", point)
				}
				if kind == "interrupt" && code != exitOK {
					t.Errorf("interrupted at %s, the upgrade did not finish: %d %s", point, code, rep.Summary)
				}
				if rep.Run == nil {
					invariants(t, d, "", oldImage)
					return
				}
				if code != exitStuck {
					// Stuck says what is down, and the --back that brings it up.
					invariants(t, d, rep.Run.Dir, oldImage, newImage)
				}
				backUntilDone(t, deps, rep.Run.ID)
				invariants(t, d, rep.Run.Dir, oldImage)
				backAgain(t, deps, rep.Run.ID, snapshot(d, rep.Run.Dir))
			})
		}
		for _, point := range backPoints {
			t.Run("back/"+kind+"/"+point, func(t *testing.T) {
				t.Parallel()
				d, deps := matrixContainer(t)
				rep, code := runIn(t, context.Background(), deps)
				if code != exitOK {
					t.Fatalf("upgrade: %d %s", code, rep.Summary)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				c := &cell{point: point, kind: kind, cancel: cancel, slow: func() { d.slow = slowBy }}
				deps.Fault = c.fault
				fast := deps.HTTP
				if kind == "slow" {
					slowDeps(&deps)
				}
				_, code = runIn(t, ctx, deps, "--back", rep.Run.ID)
				deps.Fault, deps.HTTP, d.slow = nil, fast, 0
				if !c.hit.Load() {
					t.Fatalf("no way back passes %s, a step of the table", point)
				}
				if kind == "interrupt" && code != exitOK {
					t.Errorf("interrupted at %s, the way back did not finish: %d", point, code)
				}
				backUntilDone(t, deps, rep.Run.ID)
				invariants(t, d, rep.Run.Dir, oldImage)
				backAgain(t, deps, rep.Run.ID, snapshot(d, rep.Run.Dir))
			})
		}
	}
}

// The kill cells (#34): every step, the command killed after what it records
// and before the record. The run is then met as a person meets it: --check,
// which finishes it or refuses with nothing touched, then --back until done
// and three times more. No state a kill leaves is stuck for good.
func TestTheKillMatrixOfAContainer(t *testing.T) {
	inProcess(t)
	swapSteps, backSteps := recordsOf(t, kindContainer)
	const oldImage = "ghcr.io/tracepad/tracepad:0.1.0"
	check := func(t *testing.T, d *fakeDocker, deps Deps, id, dir string, backFirst bool) {
		t.Helper()
		afterKill(t, deps, id, backFirst)
		c := d.byName["tracepad-app"]
		if c == nil || !c.State.Running || c.Config.Image != oldImage {
			t.Fatalf("after the way back: %+v", c)
		}
		// The new version ran on the volume: the old one runs on the
		// archive restored, never on what the new one may have migrated.
		if d.ranOn["tracepad-app ghcr.io/tracepad/tracepad:0.2.0"] {
			for _, m := range c.Mounts {
				if m.Destination == "/data" && m.Name != "tracepad-app-"+id {
					t.Errorf("the old version runs on %s, which the new one ran on", m.Name)
				}
			}
		}
		for _, m := range c.Mounts {
			if m.Destination == "/data" && d.volumes[m.Name] != 7 {
				t.Errorf("its volume %s holds %d traces, not 7", m.Name, d.volumes[m.Name])
			}
		}
		backAgain(t, deps, id, func() string {
			st, err := loadState(dir)
			if err != nil {
				return err.Error()
			}
			var names []string
			for name, c := range d.byName {
				names = append(names, fmt.Sprintf("%s=%s:%v:%s", name, c.Config.Image, c.State.Running, c.HostConfig.RestartPolicy.Name))
			}
			slices.Sort(names)
			return fmt.Sprintf("%q %v %q %d", names, d.volumes, st.SetAside, len(st.Steps))
		})
	}
	for order, backFirst := range orders {
		for _, point := range swapSteps {
			t.Run(order+"/upgrade/"+point, func(t *testing.T) {
				t.Parallel()
				d, deps := matrixContainer(t)
				c := &cell{point: point, kind: "kill"}
				deps.Fault = c.fault
				if _, code := runIn(t, context.Background(), deps); code != codeKilled || !c.hit.Load() {
					t.Fatalf("no upgrade records %s: %d", point, code)
				}
				deps.Fault = nil
				id, dir := runOf(t, deps)
				check(t, d, deps, id, dir, backFirst)
			})
		}
	}
	for order, backFirst := range orders {
		for _, point := range backSteps {
			t.Run(order+"/back/"+point, func(t *testing.T) {
				t.Parallel()
				d, deps := matrixContainer(t)
				rep, code := runIn(t, context.Background(), deps)
				if code != exitOK {
					t.Fatalf("upgrade: %d %s", code, rep.Summary)
				}
				c := &cell{point: point, kind: "kill"}
				deps.Fault = c.fault
				if _, code := runIn(t, context.Background(), deps, "--back", rep.Run.ID); code != codeKilled || !c.hit.Load() {
					t.Fatalf("no way back records %s: %d", point, code)
				}
				deps.Fault = nil
				check(t, d, deps, rep.Run.ID, rep.Run.Dir, backFirst)
			})
		}
	}
}

func TestTheKillMatrixOfAServer(t *testing.T) {
	inProcess(t)
	swapSteps, backSteps := recordsOf(t, kindProcess)
	check := func(t *testing.T, w *fakeWorld, deps Deps, id, dir string, traces int64, backFirst bool) {
		t.Helper()
		afterKill(t, deps, id, backFirst)
		if v := w.answers(); v != fOld {
			t.Fatalf("after the way back %s answers %q", w.url(), v)
		}
		// The new version had the data: what it left is set aside, and the
		// old one runs on the archive restored.
		w.host.mu.Lock()
		wrote := w.host.held[fNew+" "+w.data]
		w.host.mu.Unlock()
		if _, err := os.Stat(w.data + ".after-" + id); wrote && err != nil {
			t.Errorf("the new version had the data, and nothing was set aside: %v", err)
		}
		if n, err := countTraces(filepath.Join(w.data, dataDBName)); err != nil || n != traces {
			t.Errorf("traces: %d (%v), not %d", n, err, traces)
		}
		backAgain(t, deps, id, func() string {
			st, err := loadState(dir)
			if err != nil {
				return err.Error()
			}
			pid, _ := lockedBy(w.data)
			n, _ := countTraces(filepath.Join(w.data, dataDBName))
			beside, _ := filepath.Glob(w.data + ".*")
			v, _ := scriptVersion(context.Background(), w.install)
			return fmt.Sprintf("%s %d %d %s %q %q %d", w.answers(), pid, n, v, beside, st.SetAside, len(st.Steps))
		})
	}
	for order, backFirst := range orders {
		for _, point := range swapSteps {
			t.Run(order+"/upgrade/"+point, func(t *testing.T) {
				t.Parallel()
				w := newFakeWorld(t, 2)
				deps := w.deps()
				c := &cell{point: point, kind: "kill"}
				deps.Fault = c.fault
				if _, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data); code != codeKilled || !c.hit.Load() {
					t.Fatalf("no upgrade records %s: %d", point, code)
				}
				deps.Fault = nil
				id, dir := runOf(t, deps)
				check(t, w, deps, id, dir, 2, backFirst)
			})
		}
	}
	for order, backFirst := range orders {
		for _, point := range backSteps {
			t.Run(order+"/back/"+point, func(t *testing.T) {
				t.Parallel()
				w := newFakeWorld(t, 2)
				deps := w.deps()
				rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
				if code != exitOK {
					t.Fatalf("upgrade: %d %s", code, rep.Summary)
				}
				w.addTrace()
				c := &cell{point: point, kind: "kill"}
				deps.Fault = c.fault
				if _, code := runIn(t, context.Background(), deps, "--back", rep.Run.ID); code != codeKilled || !c.hit.Load() {
					t.Fatalf("no way back records %s: %d", point, code)
				}
				deps.Fault = nil
				check(t, w, deps, rep.Run.ID, rep.Run.Dir, 2, backFirst)
				if n, err := countTraces(filepath.Join(w.data+".after-"+rep.Run.ID, dataDBName)); err != nil || n != 3 {
					t.Errorf("set aside: %d traces (%v), not 3", n, err)
				}
			})
		}
	}
}

func TestTheFaultMatrixOfAServer(t *testing.T) {
	inProcess(t)
	swapPoints, backPoints := pointsOf(t, kindProcess)
	exists := func(item string) bool {
		_, err := os.Stat(item)
		return err == nil
	}
	// invariants: a server answers, the old version or the new, with the
	// traces the data held before the upgrade; every copy set aside opens
	// and passes quick_check; nothing is deleted.
	invariants := func(t *testing.T, w *fakeWorld, dir string, traces int64, want ...string) {
		t.Helper()
		if v := w.answers(); !slices.Contains(want, v) {
			t.Fatalf("%s answers %q, not one of %q", w.url(), v, want)
		}
		if n, err := countTraces(filepath.Join(w.data, dataDBName)); err != nil || n != traces {
			t.Errorf("traces: %d (%v), not %d", n, err, traces)
		}
		afters, _ := filepath.Glob(w.data + ".after-*")
		for _, a := range afters {
			if err := quickCheck(context.Background(), filepath.Join(a, dataDBName)); err != nil {
				t.Errorf("set aside, and not whole: %v", err)
			}
		}
		if dir != "" {
			nothingDeleted(t, dir, exists)
		}
	}
	snapshot := func(w *fakeWorld, dir string) func() string {
		return func() string {
			st, _ := loadState(dir)
			pid, _ := lockedBy(w.data)
			n, _ := countTraces(filepath.Join(w.data, dataDBName))
			afters, _ := filepath.Glob(w.data + ".*")
			v, _ := scriptVersion(context.Background(), w.install)
			return fmt.Sprintf("answers %s, pid %d, %d traces, binary %s\nbeside the data %q\nset aside %q\nsteps %d", w.answers(), pid, n, v, afters, st.SetAside, len(st.Steps))
		}
	}
	slowDeps := func(deps *Deps) {
		deps.HTTP = &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: slowWait}
		deps.HealthWait = slowWait
	}
	for _, kind := range faultKinds {
		for _, point := range swapPoints {
			t.Run("upgrade/"+kind+"/"+point, func(t *testing.T) {
				t.Parallel()
				w := newFakeWorld(t, 2)
				deps := w.deps()
				fast := deps
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				c := &cell{point: point, kind: kind, cancel: cancel, slow: func() { w.host.setSlow(slowBy) }}
				deps.Fault = c.fault
				if kind == "slow" {
					slowDeps(&deps)
				}
				rep, code := runIn(t, ctx, deps, "--to", fNew, "--data-dir", w.data)
				w.host.setSlow(0)
				deps = fast
				if !c.hit.Load() {
					t.Fatalf("no upgrade passes %s, a step of the table", point)
				}
				if kind == "interrupt" && code != exitOK {
					t.Errorf("interrupted at %s, the upgrade did not finish: %d %s", point, code, rep.Summary)
				}
				if rep.Run == nil {
					invariants(t, w, "", 2, fOld)
					return
				}
				if code != exitStuck {
					invariants(t, w, rep.Run.Dir, 2, fOld, fNew)
				}
				backUntilDone(t, deps, rep.Run.ID)
				invariants(t, w, rep.Run.Dir, 2, fOld)
				backAgain(t, deps, rep.Run.ID, snapshot(w, rep.Run.Dir))
			})
		}
		for _, point := range backPoints {
			t.Run("back/"+kind+"/"+point, func(t *testing.T) {
				t.Parallel()
				w := newFakeWorld(t, 2)
				deps := w.deps()
				fast := deps
				rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
				if code != exitOK {
					t.Fatalf("upgrade: %d %s", code, rep.Summary)
				}
				w.addTrace()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				c := &cell{point: point, kind: kind, cancel: cancel, slow: func() { w.host.setSlow(slowBy) }}
				deps.Fault = c.fault
				if kind == "slow" {
					slowDeps(&deps)
				}
				_, code = runIn(t, ctx, deps, "--back", rep.Run.ID)
				w.host.setSlow(0)
				deps = fast
				if !c.hit.Load() {
					t.Fatalf("no way back passes %s, a step of the table", point)
				}
				if kind == "interrupt" && code != exitOK {
					t.Errorf("interrupted at %s, the way back did not finish: %d", point, code)
				}
				backUntilDone(t, deps, rep.Run.ID)
				invariants(t, w, rep.Run.Dir, 2, fOld)
				// What the new version wrote is kept, set aside.
				if n, err := countTraces(filepath.Join(w.data+".after-"+rep.Run.ID, dataDBName)); err != nil || n != 3 {
					t.Errorf("set aside: %d traces (%v), not 3", n, err)
				}
				backAgain(t, deps, rep.Run.ID, snapshot(w, rep.Run.Dir))
			})
		}
	}
	// Cells that are not a step's: states the steps can meet (the final
	// review).
	intruder := func(w *fakeWorld) (Started, error) {
		// As a supervisor restarts it: the same arguments, on its own
		// address, free while the server is stopped; the lock is what
		// keeps it off the data.
		argv := []string{"tracepad", "serve", "--listen", w.listen, "--data-dir", w.data}
		return w.host.Start(StartSpec{Path: w.install, Argv: argv, Dir: w.home, Log: filepath.Join(w.data, "server.log")})
	}
	t.Run("scenario/a downgraded binary under a newer server", func(t *testing.T) {
		t.Parallel()
		w := newFakeWorld(t, 2)
		deps := w.deps()
		if rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data); code != exitOK {
			t.Fatalf("upgrade: %d %s", code, rep.Summary)
		}
		scriptBinary(t, w.install+".old", fOld)
		_ = os.Rename(w.install+".old", w.install)
		plan, code := runIn(t, context.Background(), deps, "--plan", "--to", fOld)
		if code != exitRefused || !strings.Contains(plan.Summary, "Leave it running") {
			t.Errorf("the plan after a downgrade: %d %s", code, plan.Summary)
		}
		invariants(t, w, "", 2, fNew)
	})
	t.Run("scenario/a server on the new binary during the way back", func(t *testing.T) {
		t.Parallel()
		w := newFakeWorld(t, 2)
		deps := w.deps()
		var in Started
		deps.Fault = func(point string) error {
			switch point {
			case stepStarted:
				return errInjected
			case stepBackCleared:
				// The install path holds the new binary still: the
				// command's hold on the data keeps it off.
				var err error
				if in, err = intruder(w); err != nil {
					t.Errorf("the stand-in for a supervisor did not start: %v", err)
				}
			}
			return nil
		}
		rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
		if code != exitWentBack {
			t.Fatalf("%d %s", code, rep.Summary)
		}
		if in != nil {
			select {
			case <-in.Exited():
			default:
				t.Error("a server on the new binary took the data during the way back")
			}
		}
		invariants(t, w, rep.Run.Dir, 2, fOld)
		backAgain(t, deps, rep.Run.ID, snapshot(w, rep.Run.Dir))
	})
	t.Run("scenario/a server on the new binary between runs", func(t *testing.T) {
		t.Parallel()
		w := newFakeWorld(t, 2)
		deps := w.deps()
		deps.Fault = func(point string) error {
			if point == stepChecked || point == stepBackCleared {
				return errInjected
			}
			return nil
		}
		rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
		if code != exitStuck {
			t.Fatalf("%d %s", code, rep.Summary)
		}
		deps.Fault = nil
		in, err := intruder(w)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-in.Exited():
			t.Fatal("the stand-in for a supervisor could not take the data")
		default:
		}
		w.addTrace() // what it wrote: a migration, say
		backUntilDone(t, deps, rep.Run.ID)
		invariants(t, w, rep.Run.Dir, 2, fOld)
		if n, err := countTraces(filepath.Join(w.data+".after-"+rep.Run.ID, dataDBName)); err != nil || n != 3 {
			t.Errorf("what it wrote, set aside: %d (%v)", n, err)
		}
		backAgain(t, deps, rep.Run.ID, snapshot(w, rep.Run.Dir))
	})
	// The new binary in place, never started by the run — the run stopped
	// short of its start — and started on the data by a shell loop before
	// the --back: the steps say what it may have been, and the archive is
	// restored, whatever it answers (#31 (a)).
	t.Run("scenario/the new binary started by something else", func(t *testing.T) {
		t.Parallel()
		w := newFakeWorld(t, 2)
		deps := w.deps()
		deps.Fault = func(point string) error {
			if point == stepStarted || point == stepBackCleared {
				return errInjected
			}
			return nil
		}
		rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
		if code != exitStuck {
			t.Fatalf("%d %s", code, rep.Summary)
		}
		if st, _ := loadState(rep.Run.Dir); st.Process.WroteAfterSwap {
			t.Fatal("a run that never started the new version records that it did")
		}
		deps.Fault = nil
		if _, err := intruder(w); err != nil {
			t.Fatal(err)
		}
		w.waitVersion(fNew)
		w.addTrace()
		backUntilDone(t, deps, rep.Run.ID)
		invariants(t, w, rep.Run.Dir, 2, fOld)
		if n, err := countTraces(filepath.Join(w.data+".after-"+rep.Run.ID, dataDBName)); err != nil || n != 3 {
			t.Errorf("what it wrote, set aside: %d (%v)", n, err)
		}
		backAgain(t, deps, rep.Run.ID, snapshot(w, rep.Run.Dir))
	})
	// Between the install path's clearing and the old binary's return,
	// nothing can start from it: a way back cut short there, and a shell loop
	// that starts the server before the next --back, start nothing (#31 (b)).
	t.Run("scenario/nothing starts from the install path while it is cleared", func(t *testing.T) {
		t.Parallel()
		w := newFakeWorld(t, 2)
		deps := w.deps()
		rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
		if code != exitOK {
			t.Fatalf("upgrade: %d %s", code, rep.Summary)
		}
		w.addTrace()
		deps.Fault = func(point string) error {
			if point == stepBackBinary {
				return errInjected
			}
			return nil
		}
		if back, code := runIn(t, context.Background(), deps, "--back", rep.Run.ID); code != exitStuck {
			t.Fatalf("%d %s", code, back.Summary)
		}
		deps.Fault = nil
		if in, err := intruder(w); err == nil {
			select {
			case <-in.Exited():
			case <-time.After(time.Second):
				t.Fatal("a server started from the install path while the way back had it cleared")
			}
		}
		backUntilDone(t, deps, rep.Run.ID)
		invariants(t, w, rep.Run.Dir, 2, fOld)
		backAgain(t, deps, rep.Run.ID, snapshot(w, rep.Run.Dir))
	})
	// The decision the sixth review asked for: an old version that answers
	// slowly is the old version still. Started by the person after a stop
	// that timed out, with the install path never changed, it ran on data
	// nothing else touched: the way back keeps it and restores nothing, as
	// many times as it is run, however slowly it answers.
	t.Run("scenario/a slow old version is the old version", func(t *testing.T) {
		t.Parallel()
		w := newFakeWorld(t, 2)
		deps := w.deps()
		deps.Fault = func(point string) error {
			if point == stepStopped {
				return errInjected
			}
			return nil
		}
		rep, code := runIn(t, context.Background(), deps, "--to", fNew, "--data-dir", w.data)
		if code != exitStuck {
			t.Fatalf("%d %s", code, rep.Summary)
		}
		deps.Fault = nil
		if _, err := intruder(w); err != nil {
			t.Fatal(err)
		}
		w.addTrace()
		w.host.setSlow(slowBy)
		slow := deps
		slowDeps(&slow)
		if back, code := runIn(t, context.Background(), slow, "--back", rep.Run.ID); code == exitOK {
			t.Errorf("a server that has not answered is called healthy: %s", back.Summary)
		}
		w.host.setSlow(0)
		backUntilDone(t, deps, rep.Run.ID)
		invariants(t, w, rep.Run.Dir, 3, fOld)
		st, _ := loadState(rep.Run.Dir)
		if st.Process.WroteAfterSwap || st.has(stepBackRestored) || len(st.SetAside) > 0 {
			t.Errorf("the old version was taken for one that may have migrated: %+v %q", st.Steps, st.SetAside)
		}
		backAgain(t, deps, rep.Run.ID, snapshot(w, rep.Run.Dir))
	})
}
