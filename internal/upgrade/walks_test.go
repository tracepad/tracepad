//go:build unix

package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// The walk matrix (spec 054 #39). A branch is a whole walk a person makes:
// the upgrade, its way back, and the event the branch is named for — a
// server that does not stop in time, one that stops late, a new version
// that does not start. Each branch's walk is first run uncut, recording
// every kill point the command passes: each step's moment between its act
// and its write, and right after its write. Then for each point a cell kills
// the command exactly there, and the person meets the run in either order:
// --check first, or --back first. A cell fails on a step off the table
// anywhere, a state that no longer loads, a --back that never reaches 0 (a
// refusal for good), or an end that is not the old version on its data. The
// branches together must pass every step of every table: a step no branch
// reaches fails the matrix, so a branch cannot be missing its cells.

// walkBranch is a walk: the kind of run it is, what its upgrade and its
// first --back end with uncut, and its events.
type walkBranch struct {
	name, kind string
	upgrade    int
	firstBack  int
	// late: the server does not stop on SIGTERM until its stop completes;
	// stubborn, a container likewise.
	late, stubborn bool
	// fail are the acts that fail, each the given number of times, in
	// every cell of the branch.
	fail map[string]int
	// like names an earlier branch whose walk this one's is, point for
	// point, until they part: the cells before that point are like's, and
	// this branch's begin where the walks part — or at from, when its world
	// or its meeting differs from there on before its points do (the gate's
	// budget, spec 054 #43). The matrix checks the walks are the same up to
	// where this one's cells begin.
	like, from string
	// binaries are what the install path may end with: the old version, or
	// the one there before the run when the run never stopped anything.
	binaries []string
	// setup changes the world before the upgrade; between happens after the
	// upgrade and before the first --back; again, when set, happens after
	// the first --back, and another --back follows it; beforeMeet happens
	// after a kill, before the person meets the run.
	setup, between, again, beforeMeet func(c *walk)
}

var walkBranches = []walkBranch{
	{name: "process", kind: kindProcess, upgrade: exitOK},
	{name: "binary", kind: kindBinary, upgrade: exitOK},
	// The install script put the new binary in place first: the server is
	// restarted on it, and nothing is replaced.
	{name: "binaryfirst", kind: kindProcess, upgrade: exitOK, binaries: []string{fOld, fNew}, setup: func(c *walk) { scriptBinary(c.t, c.k.w.install, fNew) }},
	// The server does not stop on SIGTERM in time: the upgrade is stuck,
	// --back keeps the old server (#37 (c)), its stop completes later, and
	// the next --back starts it again.
	{name: "kept", kind: kindProcess, upgrade: exitStuck, late: true, again: (*walk).stopLate},
	// The same, its stop completing before the person meets a run cut short.
	{name: "keptexits", kind: kindProcess, upgrade: exitStuck, late: true, again: (*walk).stopLate, beforeMeet: (*walk).stopLate},
	// The stop times out, and completes before the person's --back.
	{name: "slowstop", kind: kindProcess, upgrade: exitStuck, late: true, between: (*walk).stopLate, like: "kept"},
	// A new version that does not start: the upgrade goes back by itself.
	{name: "wentback", kind: kindProcess, upgrade: exitWentBack, setup: func(c *walk) {
		c.k.args = []string{"--to", fBroken, "--data-dir", c.k.w.data}
	}},
	// After the way back, the person restarts the old server, and runs
	// --back again: the server is another process, the same one.
	{name: "restarted", kind: kindProcess, upgrade: exitOK, again: (*walk).restart, beforeMeet: (*walk).restart, like: "process", from: stepBackStarted + writtenPoint},
	// A container's (#47): setup's container upgraded, the host's binary
	// brought along, and its way back.
	{name: "container", kind: kindContainer, upgrade: exitOK},
	// The host's binary is the target already: nothing is replaced.
	{name: "containerbinaryfirst", kind: kindContainer, upgrade: exitOK, binaries: []string{fOld, fNew}, setup: func(c *walk) { scriptBinary(c.t, c.k.w.install, fNew) }},
	// The host's binary cannot be replaced: a note, and the skill after it.
	{name: "containerbinaryfails", kind: kindContainer, upgrade: exitOK, fail: map[string]int{stepBinaryReplacing: 1}, like: "container"},
	// The host's binary cannot be put back by the first --back: the next
	// puts it back.
	{name: "containerbackbinaryfails", kind: kindContainer, upgrade: exitOK, fail: map[string]int{stepBackBinary: 1}, again: func(*walk) {}, like: "container"},
	// A new image that does not start: the upgrade goes back by itself.
	{name: "containerwentback", kind: kindContainer, upgrade: exitWentBack, setup: func(c *walk) { c.k.args = []string{"--to", fBroken} },
		like: "container", from: stepStarted + recordPoint},
	// The container does not stop on SIGTERM in time: the upgrade is stuck,
	// --back keeps it, its stop completes later, and the next --back starts
	// it again.
	{name: "containerkept", kind: kindContainer, upgrade: exitStuck, stubborn: true, again: (*walk).stopLate, like: "container"},
	// The old image on the restored volume does not answer: the first
	// --back is not done; once it answers, stopped since, the next --back
	// starts it again — after the host's binary was put back, and without.
	// Met after a kill, it answers again.
	{name: "containersilent", kind: kindContainer, upgrade: exitOK, firstBack: exitStuck, between: (*walk).silence, again: (*walk).stopSilenced, beforeMeet: (*walk).unsilence, like: "container"},
	{name: "containersilentnobinary", kind: kindContainer, upgrade: exitOK, firstBack: exitStuck, binaries: []string{fOld, fNew},
		setup: func(c *walk) { scriptBinary(c.t, c.k.w.install, fNew) }, between: (*walk).silence, again: (*walk).stopSilenced, beforeMeet: (*walk).unsilence, like: "containerbinaryfirst"},
}

// silence makes the old image, on a volume a way back restored, answer
// nothing.
func (c *walk) silence() {
	c.k.d.set(func() { c.k.d.silentRef = "ghcr.io/tracepad/tracepad:" + fOld })
}

// unsilence lets it answer again.
func (c *walk) unsilence() {
	c.k.d.set(func() { c.k.d.silentRef = "" })
}

// stopSilenced lets it answer again, and stops it: the person's, say.
func (c *walk) stopSilenced() {
	c.k.d.set(func() {
		c.k.d.silentRef = ""
		if ctr := c.k.d.byName["tracepad-app"]; ctr != nil {
			ctr.State.Running = false
		}
	})
}

// stopLate completes a late stop, when there is one.
func (c *walk) stopLate() {
	if c.late != nil {
		c.late.exit()
	}
	if c.k.d != nil {
		c.k.d.stopLate()
	}
}

// restart is the person restarting the server that holds the data, as the
// run's server.json starts it, once the way back has started one.
func (c *walk) restart() {
	_, dir := c.runID()
	st, err := loadState(dir)
	if err != nil || !st.has(stepBackStarted) {
		return
	}
	spec, err := readSpec(dir)
	if err != nil {
		return
	}
	w := c.k.w
	if pid, err := lockedBy(w.data); err == nil {
		_ = w.host.Signal(pid, syscall.SIGTERM) // ignored: one already gone is stopped
	}
	if _, err := w.host.Start(StartSpec{Path: spec.Exe, Argv: spec.Argv, Env: spec.Env, Dir: spec.Dir, Log: st.Process.Log}); err != nil {
		c.problem("the person's restart: %v", err)
	}
}

// lateStopper is a server that does not stop on SIGTERM until exit is
// called: a stop that times out.
type lateStopper struct {
	*fakeHost
	mu     sync.Mutex
	pid    int
	late   bool
	termed bool // the command asked it to stop
}

func (s *lateStopper) Signal(pid int, sig syscall.Signal) error {
	s.mu.Lock()
	late := s.late && pid == s.pid
	if late {
		s.termed = true
	}
	s.mu.Unlock()
	if !late {
		return s.fakeHost.Signal(pid, sig)
	}
	if s.fakeHost.Alive(pid) {
		return nil
	}
	return syscall.ESRCH
}

// exit completes the late stop: a server the command asked to stop goes,
// as a real one would.
func (s *lateStopper) exit() {
	s.mu.Lock()
	termed := s.termed
	s.late = false
	s.mu.Unlock()
	if termed {
		_ = s.fakeHost.Signal(s.pid, syscall.SIGTERM) // ignored: the fake's own stop
	}
}

// walk is one cell: a branch's walk, killed at its target-th point (-1:
// none, recording the points).
type walk struct {
	t        *testing.T
	b        walkBranch
	k        *killWorld
	late     *lateStopper
	failLeft map[string]int
	target   int

	seen     []string
	killedAt string
	problems []string
	log      []string
}

func newWalk(t *testing.T, b walkBranch, target int) *walk {
	c := &walk{t: t, b: b, k: newKillWorld(t, b.kind), target: target, failLeft: maps.Clone(b.fail)}
	if b.stubborn {
		c.k.d.stubborn = true
	}
	if b.late {
		procs, _, _ := c.k.w.host.Candidates(context.Background())
		c.late = &lateStopper{fakeHost: c.k.w.host, pid: procs[0].PID, late: true}
		c.k.deps.Sys = c.late
	}
	if b.setup != nil {
		b.setup(c)
	}
	return c
}

func (c *walk) fault(point string) error {
	if c.failLeft[point] > 0 {
		c.failLeft[point]--
		return errInjected
	}
	if !strings.HasSuffix(point, recordPoint) && !strings.HasSuffix(point, writtenPoint) {
		return nil
	}
	c.seen = append(c.seen, point)
	if len(c.seen)-1 == c.target {
		c.killedAt = point
		panic(killed{})
	}
	return nil
}

func (c *walk) problem(format string, args ...any) {
	c.problems = append(c.problems, fmt.Sprintf(format, args...))
}

// cmd runs the command, turning the matrix's kill into its code and a step
// off the table into a problem rather than a crash.
func (c *walk) cmd(deps Deps, args ...string) int {
	var out bytes.Buffer
	code, off := 0, ""
	func() {
		defer func() {
			if v := recover(); v != nil {
				switch x := v.(type) {
				case killed:
					code = codeKilled
				case string:
					if !strings.Contains(x, "off its table") {
						panic(v)
					}
					code, off = -100, x
				default:
					panic(v)
				}
			}
		}()
		code = run(context.Background(), Options{Args: append(args, "--json"), Stdout: &out, Stderr: io.Discard}, deps)
	}()
	var rep Report
	if code >= 0 {
		_ = json.Unmarshal(out.Bytes(), &rep) // ignored: a report that does not decode leaves the summary empty
	}
	if off == "" && strings.Contains(rep.Summary, "Report this") {
		off = rep.Summary
	}
	c.log = append(c.log, fmt.Sprintf("%s -> %d %s", strings.Join(args, " "), code, rep.Summary))
	if off != "" {
		c.problem("a step off the table on `%s`: %s", strings.Join(args, " "), off)
	}
	return code
}

func (c *walk) runID() (string, string) {
	states, _ := filepath.Glob(filepath.Join(c.k.deps.Backups, "*", stateFile))
	if len(states) != 1 {
		return "", ""
	}
	dir := filepath.Dir(states[0])
	return filepath.Base(dir), dir
}

// run walks the branch uncut, or until the kill; it answers whether the
// command was killed.
func (c *walk) run() bool {
	deps := c.k.deps
	deps.Fault = c.fault
	step := func(want int, args ...string) bool {
		code := c.cmd(deps, args...)
		if code == codeKilled {
			return false
		}
		if c.target < 0 && code != want {
			c.problem("uncut: `%s` ended %d, not %d", strings.Join(args, " "), code, want)
		}
		return true
	}
	if !step(c.b.upgrade, c.k.args...) {
		return true
	}
	if c.b.between != nil {
		c.b.between(c)
	}
	id, _ := c.runID()
	if id == "" {
		c.problem("uncut: no run after the upgrade")
		return false
	}
	if !step(c.b.firstBack, "--back", id) {
		return true
	}
	if c.b.again != nil {
		c.b.again(c)
		if !step(exitOK, "--back", id) {
			return true
		}
	}
	return false
}

// meet is what a person does after the kill, in one order.
func (c *walk) meet(backFirst bool) {
	id, dir := c.runID()
	if id == "" {
		c.end() // killed before the run had a directory: nothing changed
		return
	}
	if c.b.beforeMeet != nil {
		c.b.beforeMeet(c)
	}
	deps := c.k.deps
	back := func(when string) {
		for i := 0; i < 4; i++ {
			switch c.cmd(deps, "--back", id) {
			case exitOK:
				return
			case -100:
				i = 4
			}
		}
		c.problem("%s: --back did not reach 0 in four tries", when)
	}
	check := func() {
		switch code := c.cmd(deps, "--check", id); code {
		case exitOK, exitRefused, exitDecide, exitStuck:
		default:
			c.problem("--check ended %d", code)
		}
		if _, err := loadState(dir); err != nil {
			c.problem("after --check the state does not load: %v", err)
		}
	}
	if !backFirst {
		check()
	}
	back("after the kill")
	if backFirst {
		check()
	}
	if c.late != nil && c.late.fakeHost.Alive(c.late.pid) {
		// The late stop completes after all: the next --back finishes too.
		c.late.exit()
		back("after the late stop")
	}
	if c.k.d != nil && c.b.stubborn {
		c.k.d.stopLate()
		back("after the late stop")
	}
	if _, err := loadState(dir); err != nil {
		c.problem("the state does not load at the end: %v", err)
	}
	took(dir)
	c.end()
}

// end: the old version on its data, and the old binary in place.
func (c *walk) end() {
	w := c.k.w
	want := c.b.binaries
	if want == nil {
		want = []string{fOld}
	}
	if v, _ := scriptVersion(context.Background(), w.install); !slices.Contains(want, v) {
		c.problem("the installed binary is %q, not one of %q", v, want)
	}
	switch c.b.kind {
	case kindBinary:
		return
	case kindContainer:
		if v := c.k.d.answers(); v != fOld {
			c.problem("the container answers %q, not %s", v, fOld)
		}
		if n := c.k.d.traces("tracepad-app"); n < 2 {
			c.problem("the container's volume holds %d traces", n)
		}
		return
	}
	if v := w.answers(); v != fOld {
		c.problem("the server answers %q, not %s", v, fOld)
	}
	if n, err := countTraces(filepath.Join(w.data, dataDBName)); err != nil || n < 2 {
		c.problem("traces: %d (%v)", n, err)
	}
}

func (c *walk) report() {
	if len(c.problems) > 0 {
		c.t.Errorf("%s\n%s", strings.Join(c.problems, "\n"), strings.Join(c.log, "\n"))
	}
}

// walked are the table's edges the cells' runs took, "kind: last -> next".
var walked = struct {
	sync.Mutex
	edges map[string]bool
}{edges: map[string]bool{}}

// took records the edges of a run's steps.
func took(dir string) {
	st, err := loadState(dir)
	if err != nil {
		return
	}
	walked.Lock()
	defer walked.Unlock()
	last := ""
	for _, s := range st.Steps {
		walked.edges[st.Kind+": "+last+" -> "+s.Name] = true
		last = s.Name
	}
}

func theWalkMatrix(t *testing.T) {
	// Once every cell has run: every edge of every table is one some run
	// took, or a branch of the table has no cells.
	t.Cleanup(func() {
		if t.Failed() {
			return
		}
		for kind, m := range machines {
			for next, lasts := range m {
				for _, last := range lasts {
					if !walked.edges[kind+": "+last+" -> "+next] {
						t.Errorf("no walk of a %s run takes %q -> %q: a branch of the table has no cells", kind, last, next)
					}
				}
			}
		}
	})
	reached := map[string]map[string]bool{kindProcess: {}, kindContainer: {}, kindBinary: {}}
	walks := map[string][]string{}
	for _, b := range walkBranches {
		uncut := newWalk(t, b, -1)
		uncut.run()
		if _, dir := uncut.runID(); dir != "" {
			took(dir)
			if _, err := loadState(dir); err != nil {
				uncut.problem("uncut: the state does not load: %v", err)
			}
		}
		uncut.report()
		if len(uncut.seen) == 0 {
			t.Errorf("%s: the walk passes no point, and the branch has no cells", b.name)
		}
		for _, p := range uncut.seen {
			reached[b.kind][p] = true
		}
		walks[b.name] = uncut.seen
		first := 0
		if b.like != "" {
			other, ok := walks[b.like]
			if !ok {
				t.Fatalf("%s is like %s, which is no earlier branch", b.name, b.like)
			}
			for first < len(other) && first < len(uncut.seen) && other[first] == uncut.seen[first] {
				first++
			}
			if i := slices.Index(uncut.seen, b.from); b.from != "" && (i < 0 || i > first) {
				t.Errorf("%s: its cells are to begin at %s, which its walk does not pass before it parts from %s's", b.name, b.from, b.like)
			} else if i >= 0 {
				first = i
			}
			if first == 0 {
				t.Errorf("%s is like %s and shares no point with it", b.name, b.like)
			}
		}
		for i, point := range uncut.seen {
			if i < first {
				continue
			}
			for order, backFirst := range orders {
				t.Run(fmt.Sprintf("%s/%02d-%s/%s", b.name, i, point, order), func(t *testing.T) {
					t.Parallel()
					c := newWalk(t, b, i)
					switch {
					case !c.run():
						c.problem("the walk did not reach point %d (%s)", i, point)
					case c.killedAt != point:
						c.problem("killed at %s, not %s", c.killedAt, point)
					}
					c.meet(backFirst)
					c.report()
				})
			}
		}
	}
	// Every step of every table, at both moments, is some branch's cell: a
	// step no walk reaches is a branch the matrix is missing.
	for kind, m := range machines {
		for step := range m {
			for _, moment := range []string{recordPoint, writtenPoint} {
				if !reached[kind][step+moment] {
					t.Errorf("no walk of a %s run reaches %s%s: a branch of the table has no cells", kind, step, moment)
				}
			}
		}
	}
}
