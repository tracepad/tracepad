//go:build unix

package upgrade

// The way back's refusals (spec 054 #42): every reason --back can refuse, or
// fail to start the old server, is a row of refusalReasons, and a lint holds
// the table to the code — every refusal on the way back (a fail, an Errorf,
// an errors.New in back.go, settle.go, and the shared checks of apply.go it
// calls) is a row's, so a new reason without a row fails the gate, as a step
// off the table does. A reason the machine can hold before the upgrade (kind
// M) has a cell that makes it, and the upgrade must refuse it before its stop;
// cells make the others too, where the fake host can:
//   (a) before the upgrade: the upgrade refuses before stop_sent, the server
//       untouched;
//   (b) after the stop: --back reaches exit 0 once the condition is gone;
//   (c) a check a recorded step made needless is not made again.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// refusalHost is the fake host with the faults the cells need: processes that
// cannot be read, an unread count, servers that ignore SIGTERM, and a start
// that behaves as exec and the server's config do — a working directory that
// is gone fails the start, and a relative TRACEPAD_ADMIN_TOKEN_FILE is read
// from the working directory, a missing one ending the server at once
// (config.AdminToken reads the path as given).
type refusalHost struct {
	*fakeHost
	mu          sync.Mutex
	inspectFail map[int]bool
	ignoreTerm  map[int]bool
	unread      int
}

func (s *refusalHost) Candidates(ctx context.Context) ([]Process, int, error) {
	procs, unread, err := s.fakeHost.Candidates(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	return procs, unread + s.unread, err
}

func (s *refusalHost) Inspect(pid int) (Process, error) {
	s.mu.Lock()
	fail := s.inspectFail[pid]
	s.mu.Unlock()
	if fail && s.fakeHost.Alive(pid) {
		return Process{}, fmt.Errorf("pid %d: permission denied (injected)", pid)
	}
	return s.fakeHost.Inspect(pid)
}

func (s *refusalHost) Signal(pid int, sig syscall.Signal) error {
	s.mu.Lock()
	ig := s.ignoreTerm[pid]
	s.mu.Unlock()
	if ig && s.fakeHost.Alive(pid) {
		return nil
	}
	return s.fakeHost.Signal(pid, sig)
}

func (s *refusalHost) set(m map[int]bool, pid int, v bool) {
	s.mu.Lock()
	m[pid] = v
	s.mu.Unlock()
}

func (s *refusalHost) setUnread(n int) {
	s.mu.Lock()
	s.unread = n
	s.mu.Unlock()
}

func (s *refusalHost) Start(spec StartSpec) (Started, error) {
	if spec.Dir != "" {
		if info, err := os.Stat(spec.Dir); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("chdir %s: no such file or directory", spec.Dir)
		}
	}
	for _, kv := range spec.Env {
		if f, ok := strings.CutPrefix(kv, "TRACEPAD_ADMIN_TOKEN_FILE="); ok && f != "" {
			if !filepath.IsAbs(f) {
				f = filepath.Join(spec.Dir, f)
			}
			if _, err := os.Stat(f); err != nil {
				return s.exited(spec), nil
			}
		}
	}
	return s.fakeHost.Start(spec)
}

// exited is a server that started and ended at once, as one whose config
// does not load does.
func (s *refusalHost) exited(spec StartSpec) Started {
	h := s.fakeHost
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	srv := &fakeServer{p: Process{PID: h.next, PPID: os.Getpid(), Argv: spec.Argv, Env: spec.Env, Exe: spec.Path, Cwd: spec.Dir}, done: make(chan struct{})}
	h.procs[h.next] = srv
	srv.stop()
	return &started{pid: h.next, done: srv.done}
}

type refusalCell struct {
	t        *testing.T
	w        *fakeWorld
	deps     Deps
	sys      *refusalHost
	problems []string
	log      []string
}

func newRefusalCell(t *testing.T) *refusalCell {
	w := newFakeWorld(t, 2)
	c := &refusalCell{t: t, w: w, deps: w.deps()}
	c.sys = &refusalHost{fakeHost: w.host, inspectFail: map[int]bool{}, ignoreTerm: map[int]bool{}}
	c.deps.Sys = c.sys
	c.deps.StopWait = 400 * time.Millisecond
	c.deps.HealthWait = 1500 * time.Millisecond
	return c
}

func (c *refusalCell) problem(format string, args ...any) {
	c.problems = append(c.problems, fmt.Sprintf(format, args...))
}

func (c *refusalCell) cmd(deps Deps, args ...string) (Report, int) {
	rep, code, off := cellRun(deps, args...)
	c.log = append(c.log, fmt.Sprintf("%s -> %d %s", strings.Join(args, " "), code, oneLine(rep.Summary)))
	if off != "" {
		c.problem("offTable on `%s`: %s", strings.Join(args, " "), oneLine(off))
	}
	return rep, code
}

func (c *refusalCell) serverPID() int {
	procs, _, _ := c.w.host.Candidates(context.Background())
	for _, p := range procs {
		if d, _, err := resolveServer(p); err == nil && d == c.w.data {
			return p.PID
		}
	}
	return 0
}

func (c *refusalCell) runDir() (string, string) {
	states, _ := filepath.Glob(filepath.Join(c.deps.Backups, "*", stateFile))
	if len(states) == 0 {
		return "", ""
	}
	dir := filepath.Dir(states[len(states)-1])
	return filepath.Base(dir), dir
}

func (c *refusalCell) state() *State {
	_, dir := c.runDir()
	if dir == "" {
		return nil
	}
	st, err := loadState(dir)
	if err != nil {
		c.problem("state does not load: %v", err)
		return nil
	}
	return st
}

// backUntil runs --back up to n times; it answers whether it reached 0.
func (c *refusalCell) backUntil(id string, n int) bool {
	for i := 0; i < n; i++ {
		if _, code := c.cmd(c.deps, "--back", id); code == exitOK {
			return true
		}
	}
	return false
}

func (c *refusalCell) endOld(label string) {
	if v := c.w.answers(); v != fOld {
		c.problem("%s: the server answers %q, not %s", label, v, fOld)
	}
	if n, err := countTraces(filepath.Join(c.w.data, dataDBName)); err != nil || n < 2 {
		c.problem("%s: traces %d (%v)", label, n, err)
	}
}

func (c *refusalCell) report(name string) {
	verdict := "PASS"
	if len(c.problems) > 0 {
		verdict = "FAIL"
	}
	c.t.Logf("RESULT\t%s\t%s\t%s", name, verdict, strings.Join(c.problems, " | "))
	c.t.Logf("LOG\t%s\t%s", name, strings.Join(c.log, " || "))
	if verdict == "FAIL" {
		c.t.Fail()
	}
}

// aCell: the condition holds before the upgrade. The upgrade must refuse
// before stop_sent, the server untouched. When it does not, what follows is
// logged: the upgrade's end, and whether --back recovers once the condition
// is removed.
func (c *refusalCell) aCell(target string, make func() (undo func())) {
	pid := c.serverPID()
	undo := make()
	rep, code := c.cmd(c.deps, "--to", target, "--data-dir", c.w.data)
	st := c.state()
	stopped := st != nil && st.has(stepStopSent)
	refusedBefore := code != exitOK && !stopped
	if !refusedBefore {
		c.problem("not refused before the stop: the upgrade ended %d (%s), server %q", code, oneLine(rep.Summary), c.w.answers())
	}
	if refusedBefore && (!c.w.host.Alive(pid) || c.w.answers() != fOld) {
		c.problem("refused, but the server was touched: answers %q", c.w.answers())
	}
	undo()
	if stopped && rep.Run != nil {
		if code != exitWentBack && code != exitOK && !c.backUntil(rep.Run.ID, 3) {
			c.problem("with the condition removed, --back never reached 0")
		}
	}
}

// bAfterHealthy: a healthy upgrade, then the condition, then --back; once the
// condition is removed --back must reach 0 and the old version run.
func (c *refusalCell) bAfterHealthy(make func(id string, st *State) (undo func())) {
	rep, code := c.cmd(c.deps, "--to", fNew, "--data-dir", c.w.data)
	if code != exitOK {
		c.problem("healthy upgrade: %d %s", code, oneLine(rep.Summary))
		return
	}
	undo := make(rep.Run.ID, c.state())
	_, code = c.cmd(c.deps, "--back", rep.Run.ID)
	undo()
	if code != exitOK && !c.backUntil(rep.Run.ID, 3) {
		c.problem("with the condition removed, --back never reached 0")
	}
	c.endOld("end")
}

// bDuringUpgrade: the condition appears right after the stop of an upgrade
// to a broken version, so the upgrade's own way back meets it; once it is
// removed --back must reach 0.
func (c *refusalCell) bDuringUpgrade(make func() (undo func())) {
	var undo func()
	deps := c.deps
	deps.Fault = func(point string) error {
		if point == stepStopped+writtenPoint && undo == nil {
			undo = make()
		}
		return nil
	}
	rep, code := c.cmd(deps, "--to", fBroken, "--data-dir", c.w.data)
	if undo == nil {
		c.problem("the upgrade never reached its stop: %d %s", code, oneLine(rep.Summary))
		return
	}
	undo()
	if code != exitWentBack && !c.backUntil(rep.Run.ID, 3) {
		c.problem("with the condition removed, --back never reached 0")
	}
	c.endOld("end")
}

// Conditions.

func (c *refusalCell) readOnly(dir string) func() {
	if os.Geteuid() == 0 {
		c.t.Skip("root writes any directory")
	}
	_ = os.MkdirAll(c.deps.Backups, 0o700)
	if err := os.Chmod(dir, 0o500); err != nil {
		c.problem("chmod: %v", err)
	}
	return func() { _ = os.Chmod(dir, 0o700) }
}

func (c *refusalCell) otherServer(version string) (pid int, undo func()) {
	data := filepath.Join(c.w.home, "other-data")
	_ = os.MkdirAll(data, 0o700)
	addr := freeAddr(c.t)
	s, err := c.w.host.Start(StartSpec{Path: c.w.install, Argv: []string{"tracepad", "serve", "--listen", addr, "--data-dir", data}, Dir: c.w.home, Log: filepath.Join(data, "server.log")})
	if err != nil {
		c.problem("other server: %v", err)
		return 0, func() {}
	}
	for i := 0; i < 100; i++ {
		if v, _ := health(context.Background(), c.deps.HTTP, "http://"+addr); v == version {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return s.PID(), func() { _ = c.w.host.Signal(s.PID(), syscall.SIGTERM); <-s.Exited() }
}

// tokenServer replaces the world's server with one started from a working
// directory of its own, reading its admin token from a relative path there.
func (c *refusalCell) tokenServer() (cwd string) {
	if pid := c.serverPID(); pid > 0 {
		_ = c.w.host.Signal(pid, syscall.SIGTERM)
	}
	cwd = filepath.Join(c.w.home, "work")
	_ = os.MkdirAll(cwd, 0o700)
	_ = os.WriteFile(filepath.Join(cwd, "token.txt"), []byte(strings.Repeat("t", 40)), 0o600)
	argv := []string{"tracepad", "serve", "--listen", c.w.listen, "--data-dir", c.w.data}
	if _, err := c.sys.Start(StartSpec{Path: c.w.install, Argv: argv, Env: []string{"HOME=" + c.w.home, "TRACEPAD_ADMIN_TOKEN_FILE=token.txt"}, Dir: cwd, Log: filepath.Join(c.w.data, "server.log")}); err != nil {
		c.problem("token server: %v", err)
	}
	c.w.waitVersion(fOld)
	return cwd
}

func restoreDir(dir string) {
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "token.txt"), []byte(strings.Repeat("t", 40)), 0o600)
}

func moveAway(path string) func() {
	_ = os.Rename(path, path+".away")
	return func() { _ = os.Rename(path+".away", path) }
}

type refusalCase struct {
	name string
	run  func(c *refusalCell)
}

func refusalCases() []refusalCase {
	parentOf := func(c *refusalCell) string { return filepath.Dir(c.w.data) }
	return []refusalCase{
		// R6/R7: the data directory's parent takes no new directory or rename.
		{"R06-readonly-parent/a", func(c *refusalCell) {
			c.aCell(fBroken, func() func() { return c.readOnly(parentOf(c)) })
		}},
		{"R06-readonly-parent/b-healthy", func(c *refusalCell) {
			c.bAfterHealthy(func(string, *State) func() { return c.readOnly(parentOf(c)) })
		}},
		// --back meets it before its first act: refused with nothing begun,
		// the new version left running.
		{"R06-readonly-parent/b-before-first-act", func(c *refusalCell) {
			rep, code := c.cmd(c.deps, "--to", fNew, "--data-dir", c.w.data)
			if code != exitOK {
				c.problem("healthy upgrade: %d %s", code, oneLine(rep.Summary))
				return
			}
			undo := c.readOnly(parentOf(c))
			back, code := c.cmd(c.deps, "--back", rep.Run.ID)
			undo()
			if st := c.state(); code == exitOK || st == nil || st.has(stepBackBegun) || !strings.Contains(back.Summary, "a restore cannot be made beside") || c.w.answers() != fNew {
				c.problem("--back did not refuse before its first act: %d %s", code, oneLine(back.Summary))
			}
			if !c.backUntil(rep.Run.ID, 3) {
				c.problem("with the condition removed, --back never reached 0")
			}
			c.endOld("end")
		}},
		{"R06-readonly-parent/b-during", func(c *refusalCell) {
			c.bDuringUpgrade(func() func() { return c.readOnly(parentOf(c)) })
		}},
		// R5: .after-<run> already there.
		{"R05-after-exists/b-healthy", func(c *refusalCell) {
			c.bAfterHealthy(func(id string, _ *State) func() {
				p := c.w.data + ".after-" + id
				_ = os.Mkdir(p, 0o700)
				return func() { _ = os.Remove(p) }
			})
		}},
		// R11: another server on the install path.
		{"R11-other-server-same-binary/a-binary-replaced", func(c *refusalCell) {
			var undo func()
			c.aCell(fBroken, func() func() { _, undo = c.otherServer(fOld); return undo })
		}},
		{"R11-other-server-same-binary/a-binary-already-target", func(c *refusalCell) {
			// The installed binary is already the target; the command's
			// server still runs the old one from it, and another server runs
			// the target from it. The new version then fails its check
			// (it opens another directory), so the way back is needed.
			scriptBinary(c.t, c.w.install, fNew)
			var undo func()
			c.aCell(fNew, func() func() {
				_, undo = c.otherServer(fNew)
				c.w.host.mu.Lock()
				c.w.host.elsewhere[fNew] = filepath.Join(c.w.home, "elsewhere")
				c.w.host.mu.Unlock()
				_ = os.MkdirAll(filepath.Join(c.w.home, "elsewhere"), 0o700)
				return undo
			})
		}},
		{"R11-other-server-same-binary/b-healthy", func(c *refusalCell) {
			c.bAfterHealthy(func(string, *State) func() { _, undo := c.otherServer(fNew); return undo })
		}},
		{"R11-unread-processes/a", func(c *refusalCell) {
			c.aCell(fBroken, func() func() { c.sys.setUnread(1); return func() { c.sys.setUnread(0) } })
		}},
		{"R11-unread-processes/b-healthy", func(c *refusalCell) {
			c.bAfterHealthy(func(string, *State) func() { c.sys.setUnread(1); return func() { c.sys.setUnread(0) } })
		}},
		// R28: the old version's start fails.
		{"R28-relative-token-file-cwd-deleted/a", func(c *refusalCell) {
			cwd := c.tokenServer()
			c.aCell(fBroken, func() func() { _ = os.RemoveAll(cwd); return func() { restoreDir(cwd) } })
		}},
		{"R28-relative-token-file-cwd-deleted/b-during", func(c *refusalCell) {
			cwd := c.tokenServer()
			c.bDuringUpgrade(func() func() { _ = os.RemoveAll(cwd); return func() { restoreDir(cwd) } })
		}},
		{"R28-address-taken/b-during", func(c *refusalCell) {
			c.bDuringUpgrade(func() func() {
				l, err := net.Listen("tcp", c.w.listen)
				if err != nil {
					c.problem("listen: %v", err)
					return func() {}
				}
				// Another program's: it takes each connection and closes
				// it, so an ask there fails at once rather than at its
				// deadline.
				go func() {
					for {
						conn, err := l.Accept()
						if err != nil {
							return
						}
						conn.Close() // ignored: closed is the answer
					}
				}()
				return func() { l.Close() }
			})
		}},
		// R15: a foreign process holds the data.
		{"R15-foreign-holder/b-healthy", func(c *refusalCell) {
			c.bAfterHealthy(func(_ string, st *State) func() {
				_ = c.w.host.Signal(st.Process.NewPID, syscall.SIGTERM)
				argv := []string{"tracepad", "serve", "--listen", freeAddr(c.t), "--data-dir", c.w.data}
				s, err := c.w.host.Start(StartSpec{Path: c.w.install, Argv: argv, Dir: c.w.home, Log: filepath.Join(c.w.data, "x.log")})
				if err != nil {
					c.problem("intruder: %v", err)
					return func() {}
				}
				return func() { _ = c.w.host.Signal(s.PID(), syscall.SIGTERM); <-s.Exited() }
			})
		}},
		// R14: the old server, asked to stop and still running, cannot be read.
		{"R14-old-server-unreadable/b", func(c *refusalCell) {
			pid := c.serverPID()
			c.sys.set(c.sys.ignoreTerm, pid, true)
			rep, code := c.cmd(c.deps, "--to", fNew, "--data-dir", c.w.data)
			if code != exitStuck {
				c.problem("a stop that times out: %d %s", code, oneLine(rep.Summary))
				return
			}
			c.sys.set(c.sys.inspectFail, pid, true)
			_, code = c.cmd(c.deps, "--back", rep.Run.ID)
			c.sys.set(c.sys.inspectFail, pid, false)
			if code != exitOK && !c.backUntil(rep.Run.ID, 3) {
				c.problem("with the condition removed, --back never reached 0")
			}
			c.endOld("end")
		}},
		// R24: the run's new server cannot be read.
		{"R24-new-server-unreadable/b-healthy", func(c *refusalCell) {
			c.bAfterHealthy(func(_ string, st *State) func() {
				c.sys.set(c.sys.inspectFail, st.Process.NewPID, true)
				return func() { c.sys.set(c.sys.inspectFail, st.Process.NewPID, false) }
			})
		}},
		// R25: the new server is slow to stop.
		{"R25-new-server-slow-to-stop/b-healthy", func(c *refusalCell) {
			c.bAfterHealthy(func(_ string, st *State) func() {
				c.sys.set(c.sys.ignoreTerm, st.Process.NewPID, true)
				return func() {
					c.sys.set(c.sys.ignoreTerm, st.Process.NewPID, false)
					_ = c.w.host.Signal(st.Process.NewPID, syscall.SIGTERM)
				}
			})
		}},
		// R2: the copy of the old version in the run's directory.
		{"R02-old-copy-wrong/b-healthy", func(c *refusalCell) {
			c.bAfterHealthy(func(_ string, st *State) func() { return moveAway(st.Process.Old) })
		}},
		// R17: the archive.
		{"R17-archive-missing/b-healthy", func(c *refusalCell) {
			c.bAfterHealthy(func(id string, _ *State) func() {
				_, dir := c.runDir()
				return moveAway(filepath.Join(dir, "data.tar.gz"))
			})
		}},
		// R1: server.json.
		{"R01-server-json-missing/b-healthy", func(c *refusalCell) {
			c.bAfterHealthy(func(string, *State) func() {
				_, dir := c.runDir()
				return moveAway(filepath.Join(dir, "server.json"))
			})
		}},
		// R10: the install path holds a version the run did not put there.
		{"R10-install-path-other-version/b-healthy", func(c *refusalCell) {
			c.bAfterHealthy(func(string, *State) func() {
				scriptBinary(c.t, c.w.install+".x", fBroken)
				_ = os.Rename(c.w.install, c.w.install+".keep")
				_ = os.Rename(c.w.install+".x", c.w.install)
				return func() { _ = os.Rename(c.w.install+".keep", c.w.install) }
			})
		}},
		// R27: the install directory takes no change.
		{"R27-install-dir-readonly/a", func(c *refusalCell) {
			c.aCell(fBroken, func() func() { return c.readOnly(filepath.Dir(c.w.install)) })
		}},
		{"R27-install-dir-readonly/b-healthy", func(c *refusalCell) {
			c.bAfterHealthy(func(string, *State) func() { return c.readOnly(filepath.Dir(c.w.install)) })
		}},
		// R30/R31: the backups directory, the run directory.
		{"R30-backups-not-private/a", func(c *refusalCell) {
			_ = os.MkdirAll(c.deps.Backups, 0o700)
			c.aCell(fBroken, func() func() {
				_ = os.Chmod(c.deps.Backups, 0o755)
				return func() { _ = os.Chmod(c.deps.Backups, 0o700) }
			})
		}},
		{"R30-backups-not-private/b-healthy", func(c *refusalCell) {
			c.bAfterHealthy(func(string, *State) func() {
				_ = os.Chmod(c.deps.Backups, 0o755)
				return func() { _ = os.Chmod(c.deps.Backups, 0o700) }
			})
		}},
		{"R31-run-dir-not-private/b-healthy", func(c *refusalCell) {
			c.bAfterHealthy(func(string, *State) func() {
				_, dir := c.runDir()
				_ = os.Chmod(dir, 0o755)
				return func() { _ = os.Chmod(dir, 0o700) }
			})
		}},
		// R13: something at the data's place after it was set aside.
		{"R13-data-dir-back-after-aside/b", func(c *refusalCell) {
			rep, code := c.cmd(c.deps, "--to", fNew, "--data-dir", c.w.data)
			if code != exitOK {
				c.problem("healthy upgrade: %d", code)
				return
			}
			deps := c.deps
			deps.Fault = func(point string) error {
				if point == stepBackAside+writtenPoint {
					panic(killed{})
				}
				return nil
			}
			c.cmd(deps, "--back", rep.Run.ID)
			_ = os.Mkdir(c.w.data, 0o700)
			_, code = c.cmd(c.deps, "--back", rep.Run.ID)
			_ = os.Remove(c.w.data)
			if code != exitOK && !c.backUntil(rep.Run.ID, 3) {
				c.problem("with the condition removed, --back never reached 0")
			}
			c.endOld("end")
		}},
	}
}

// theWayBacksRefusals runs every cell but the room's, side by side, under
// TestTheMatrices, whose seams they share.
func theWayBacksRefusals(t *testing.T) {
	for _, pc := range refusalCases() {
		t.Run(pc.name, func(t *testing.T) {
			t.Parallel()
			c := newRefusalCell(t)
			pc.run(c)
			c.report(pc.name)
		})
	}
	// A container's (#47): each cell makes its own world.
	for _, pc := range containerRefusalCases() {
		t.Run(pc.name, func(t *testing.T) {
			t.Parallel()
			pc.run(&refusalCell{t: t})
		})
	}
}

// The room cells change freeBytes, a package seam: they run one at a time,
// as a test of their own.
func TestTheWayBacksRefusalsOfRoom(t *testing.T) {
	inProcess(t)
	saved := freeBytes
	t.Cleanup(func() { freeBytes = saved })
	for _, pc := range roomCases(saved) {
		t.Run(pc.name, func(t *testing.T) {
			c := newRefusalCell(t)
			pc.run(c)
			freeBytes = saved
			c.report(pc.name)
		})
	}
}

// roomCases are the cells that set the room on the data's file system; saved
// is freeBytes as it was.
func roomCases(saved func(string) (int64, error)) []refusalCase {
	lowBeside := func(c *refusalCell, room func() int64) {
		parent := filepath.Dir(c.w.data)
		freeBytes = func(dir string) (int64, error) {
			if dir == parent {
				return room(), nil
			}
			return 1 << 40, nil
		}
	}
	return []refusalCase{
		// R9: no room beside the data for a restore.
		{"R09-no-room-beside-data/a", func(c *refusalCell) {
			size, _ := survey(c.w.data)
			c.aCell(fBroken, func() func() {
				lowBeside(c, func() int64 { return size + mib100 - 1 })
				return func() { freeBytes = saved }
			})
		}},
		// R9 with the archive on the same file system: room for the
		// restore, not for the archive too.
		{"R09-no-room-for-archive-and-restore/a", func(c *refusalCell) {
			size, _ := survey(c.w.data)
			c.aCell(fBroken, func() func() {
				lowBeside(c, func() int64 { return size + mib100 + size/2 })
				return func() { freeBytes = saved }
			})
		}},
		{"R09-no-room-beside-data/b-healthy", func(c *refusalCell) {
			c.bAfterHealthy(func(_ string, st *State) func() {
				lowBeside(c, func() int64 { return st.Archive.Bytes + mib100 - 1 })
				return func() { freeBytes = saved }
			})
		}},
		// (c) R18: after back_restored the restore exists; its room is not
		// asked again. Killed right after the step is written, then the
		// room beside the data is what the restore left.
		{"R18-room-after-restored/c-killed", func(c *refusalCell) {
			rep, code := c.cmd(c.deps, "--to", fNew, "--data-dir", c.w.data)
			if code != exitOK {
				c.problem("healthy upgrade: %d", code)
				return
			}
			st := c.state()
			deps := c.deps
			deps.Fault = func(point string) error {
				if point == stepBackRestored+writtenPoint {
					panic(killed{})
				}
				return nil
			}
			c.cmd(deps, "--back", rep.Run.ID)
			lowBeside(c, func() int64 { return st.Archive.Bytes + mib100 - 1 })
			ok := c.backUntil(rep.Run.ID, 3)
			freeBytes = saved
			if !ok {
				c.problem("after back_restored, --back asked for room the restore already has, and never reached 0")
				c.backUntil(rep.Run.ID, 1)
			}
			c.endOld("end")
		}},
		// (c) the review's scenario: the restore is made, the new server is
		// slow to stop; on the next --back the room is what the restore left.
		{"R18-room-after-restored/c-slow-stop", func(c *refusalCell) {
			rep, code := c.cmd(c.deps, "--to", fNew, "--data-dir", c.w.data)
			if code != exitOK {
				c.problem("healthy upgrade: %d", code)
				return
			}
			st := c.state()
			c.sys.set(c.sys.ignoreTerm, st.Process.NewPID, true)
			c.cmd(c.deps, "--back", rep.Run.ID)
			if s2 := c.state(); s2 == nil || !s2.has(stepBackRestored) {
				c.problem("the first --back did not get as far as the restore")
			}
			c.sys.set(c.sys.ignoreTerm, st.Process.NewPID, false)
			_ = c.w.host.Signal(st.Process.NewPID, syscall.SIGTERM)
			lowBeside(c, func() int64 { return st.Archive.Bytes + mib100 - 1 })
			ok := c.backUntil(rep.Run.ID, 3)
			freeBytes = saved
			if !ok {
				c.problem("after back_restored, --back asked for room the restore already has, and never reached 0")
				c.backUntil(rep.Run.ID, 1)
			}
			c.endOld("end")
		}},
	}
}

// cellRun runs the command as a cell does: a kill the cell makes ends it
// where it is, and a step off the table is said, not a crash of the test.
func cellRun(deps Deps, args ...string) (rep Report, code int, off string) {
	var out bytes.Buffer
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
					code, off = exitStuck, x
				default:
					panic(v)
				}
			}
		}()
		code = run(context.Background(), Options{Args: append(args, "--json"), Stdout: &out, Stderr: io.Discard}, deps)
	}()
	if code != codeKilled && off == "" {
		_ = json.Unmarshal(out.Bytes(), &rep) // ignored: a report that does not read is an empty one, which the cell's checks fail on
	}
	if off == "" && strings.Contains(rep.Summary, "Report this") {
		off = rep.Summary
	}
	return rep, code, off
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// A refusal reason: the refusals on the way back whose function is fn and
// whose message begins with prefix (the longest prefix of fn's rows takes a
// site), n of them; its kind — M, a condition of the machine, which can hold
// before the upgrade and which the upgrade must refuse before its stop; R,
// the run's own files and steps; T, what appears only after the stop or in a
// race — and the cells (here, or a test's name) that make it, or why none
// can. A row with no fn is a refusal outside the sites the lint reads.
type refusalReason struct {
	id, fn, prefix string
	n              int
	kind           byte
	cells          []string
	why            string
}

// refusalReasons are every reason the way back refuses, or fails to start
// the old server (the precondition audit of #223, held to the code by
// TestEveryRefusalOfTheWayBackIsListed).
var refusalReasons = []refusalReason{
	// The entry: backMode, openRun, settle.
	{id: "R30", kind: 'M', cells: []string{"R30-backups-not-private/a", "R30-backups-not-private/b-healthy"}},
	{id: "R31", kind: 'R', cells: []string{"R31-run-dir-not-private/b-healthy"}},
	{id: "R32", kind: 'R', cells: []string{"TestTheMatrices"}},
	{id: "R33", fn: "settle", n: 2, kind: 'R', cells: []string{"TestTheMatrices"}},
	// backProcess and its preconditions.
	{id: "R01", fn: "backProcess", prefix: "the run's server.json does not read", n: 1, kind: 'R', cells: []string{"R01-server-json-missing/b-healthy"}},
	{id: "R02-R11", fn: "backProcess", prefix: "; nothing was touched", n: 2, kind: 'R', cells: []string{"R10-install-path-other-version/b-healthy", "R06-readonly-parent/b-healthy"},
		why: "passes on pathAsRecorded's refusal and wayBackPreconditions'"},
	{id: "R02", fn: "wayBackPreconditions", prefix: "the copy %s does not answer", n: 1, kind: 'R', cells: []string{"R02-old-copy-wrong/b-healthy"}},
	{id: "R03", fn: "wayBackPreconditions", prefix: "the room a restore of %s needs cannot be told", n: 1, kind: 'R', why: "a process run records its archive's size; only an archive recorded without one measures the data instead"},
	{id: "R04", fn: "wayBackPreconditions", prefix: "%s cannot be looked at (%v)", n: 2, kind: 'M', why: "a parent, or a set-aside name in it, that cannot be looked at hides the data itself, which the survey before it refuses"},
	{id: "R05", fn: "wayBackPreconditions", prefix: "%s exists already, and this run did not put it there", n: 1, kind: 'T', cells: []string{"R05-after-exists/b-healthy"}},
	{id: "R06", fn: "wayBackPreconditions", prefix: "a restore cannot be made beside", n: 1, kind: 'M', cells: []string{"R06-readonly-parent/a", "R06-readonly-parent/b-healthy", "R06-readonly-parent/b-before-first-act", "R06-readonly-parent/b-during"}},
	{id: "R06b", fn: "wayBackPreconditions", prefix: "nothing can be renamed in", n: 1, kind: 'M', why: "a directory that takes a new entry and refuses its rename is a rule no test file system has; the same probe as R06, before the stop"},
	{id: "R06c", fn: "wayBackPreconditions", prefix: "%s does not let the way back remove", n: 1, kind: 'M', why: "as R06b"},
	{id: "R07", fn: "wayBackPreconditions", prefix: "%s is in %s, another user's sticky directory", n: 1, kind: 'M', why: "needs a second user"},
	{id: "R08", fn: "restoreRoom", prefix: "the room beside %s for a restore cannot be told", n: 1, kind: 'M', cells: []string{"TestWhatCannotBeReadRefuses"}},
	{id: "R09", fn: "restoreRoom", prefix: "no room beside %s for the restore a way back may need", n: 1, kind: 'M', cells: []string{"R09-no-room-beside-data/a", "R09-no-room-for-archive-and-restore/a", "R09-no-room-beside-data/b-healthy"}},
	{id: "R10", fn: "pathAsRecorded", n: 1, kind: 'T', cells: []string{"R10-install-path-other-version/b-healthy"}},
	{id: "R11", fn: "serversOn", n: 8, kind: 'M', cells: []string{"R11-other-server-same-binary/a-binary-replaced", "R11-other-server-same-binary/a-binary-already-target", "R11-other-server-same-binary/b-healthy", "R11-unread-processes/a", "R11-unread-processes/b-healthy", "TestTheWayBacksServerCheckComesBeforeTheStop"}},
	// backProcess's body.
	{id: "R13", fn: "backProcess", prefix: "%s is set aside as %s, and something is at %s again", n: 1, kind: 'T', cells: []string{"R13-data-dir-back-after-aside/b"}},
	{id: "R13b", fn: "backProcess", prefix: "%s cannot be looked at (%v); nothing was touched", n: 1, kind: 'T', why: "the data's place made unreadable after it was set aside; as R13"},
	{id: "R14", fn: "backProcess", prefix: "pid %d, the server the run asked to stop", n: 1, kind: 'T', cells: []string{"R14-old-server-unreadable/b"}},
	{id: "R15a", fn: "decide", prefix: "pid %d, the server the run stopped, may still run", n: 1, kind: 'T', why: "as R14, once the stop is recorded"},
	{id: "R15a2", fn: "decide", prefix: "server pid %d is still shutting down", n: 1, kind: 'T', cells: []string{"TestAWayBackWaitsForAServerStillShuttingDown"}},
	{id: "R15b", fn: "decide", prefix: "the lock of %s c", n: 2, kind: 'T', cells: []string{"TestAFileSystemThatCannotLockIsNeverFree"}, why: "a flock that fails on a file system that has them is not one the fake host can make"},
	{id: "R15c", fn: "decide", prefix: "pid %d holds %s, and it is not", n: 1, kind: 'T', cells: []string{"R15-foreign-holder/b-healthy"}},
	{id: "R15d", fn: "decide", prefix: "the lock of ", n: 1, kind: 'T', why: "a race: the lock changing hands three times"},
	{id: "R28a", fn: "backProcess", prefix: "%s did not start", n: 1, kind: 'T', cells: []string{"R28-relative-token-file-cwd-deleted/b-during", "R28-address-taken/b-during"}},
	{id: "R29", fn: "backProcess", prefix: "%s could not be put back at %s (%v); nothing was started", n: 1, kind: 'T', cells: []string{"R27-install-dir-readonly/b-healthy"}},
	{id: "R29b", fn: "putInPlace", n: 2, kind: 'T', cells: []string{"R27-install-dir-readonly/b-healthy"}},
	{id: "R34", fn: "backProcess", prefix: "the lock of %s cannot be read (%v); nothing was started", n: 1, kind: 'T', why: "as R15b"},
	{id: "R35", fn: "backProcess", prefix: "; nothing was started", n: 1, kind: 'T', cells: []string{"R15-foreign-holder/b-healthy"}},
	{id: "R28b", kind: 'M', cells: []string{"R28-relative-token-file-cwd-deleted/a", "TestARelativePathWithItsDirectoryGoneRefuses"}},
	{id: "R28d", fn: "launch", n: 4, kind: 'T', why: "a race: the data's lock taken in the moment it is free"},
	// swapBack, clearPath, moveRestore.
	{id: "R16", fn: "swapBack", prefix: "tracepad %s may have had %s, and the run took no archive", n: 1, kind: 'R', why: "a process run archives before it may start the new version"},
	{id: "R17", fn: "swapBack", prefix: "; nothing was touched", n: 1, kind: 'R', cells: []string{"R17-archive-missing/b-healthy"}},
	{id: "R18", fn: "swapBack", prefix: "%v; nothing was touched", n: 1, kind: 'M', cells: []string{"R09-no-room-beside-data/a", "R18-room-after-restored/c-killed", "R18-room-after-restored/c-slow-stop", "TestWhatCannotBeReadRefuses"},
		why: "passes on restoreRoom's refusal (R08, R09), made again only while the restore is to be made"},
	{id: "R19", fn: "swapBack", prefix: " exists already", n: 1, kind: 'T', cells: []string{"R05-after-exists/b-healthy"}},
	{id: "R19b", fn: "swapBack", prefix: "%s cannot be looked at (%v); nothing was touched", n: 1, kind: 'T', why: "as R04"},
	{id: "R20", fn: "swapBack", prefix: "a restore begun before and not finished", n: 1, kind: 'T', why: "a partial restore that cannot be removed: a file in it made another's"},
	{id: "R21", fn: "swapBack", prefix: "", n: 1, kind: 'T', why: "the data directory gone after the decision; as R13"},
	{id: "R21b", fn: "swapBack", prefix: "the restore into %s failed", n: 1, kind: 'M', cells: []string{"R06-readonly-parent/a", "R06-readonly-parent/b-healthy"}},
	{id: "R22", fn: "swapBack", prefix: "the restored database fails its check", n: 1, kind: 'R', why: "the archive's checksum fails first"},
	{id: "R23", fn: "swapBack", prefix: "the restore this way back made and checked is not in", n: 1, kind: 'T', why: "a restore removed by hand between two --back"},
	{id: "R23b", fn: "swapBack", prefix: "the restore's lock could not be taken", n: 1, kind: 'T', why: "a race"},
	{id: "R24", fn: "swapBack", prefix: "the run's server ", n: 1, kind: 'T', cells: []string{"R24-new-server-unreadable/b-healthy"}},
	{id: "R24b", fn: "startedAs", n: 1, kind: 'T', cells: []string{"R24-new-server-unreadable/b-healthy", "R14-old-server-unreadable/b"},
		why: "isServer reads a process through it too"},
	{id: "R25", fn: "swapBack", prefix: "; nothing was stopped, and the restore waits in ", n: 1, kind: 'T', cells: []string{"R15-foreign-holder/b-healthy"}},
	{id: "R25b", fn: "swapBack", prefix: "server pid %d was asked to stop and has not", n: 1, kind: 'T', cells: []string{"R25-new-server-slow-to-stop/b-healthy"}},
	{id: "R25c", fn: "swapBack", prefix: "the run's server pid %d was asked to stop and has not", n: 1, kind: 'T', cells: []string{"R25-new-server-slow-to-stop/b-healthy"}},
	{id: "R26", fn: "swapBack", prefix: ": ", n: 1, kind: 'T', why: "a race: the data's lock taken between the stop and the swap"},
	{id: "R27b", fn: "swapBack", prefix: "%s could not be set aside", n: 1, kind: 'M', why: "the parent's rename, which R06's probe makes before the stop"},
	{id: "R27", fn: "clearPath", n: 5, kind: 'M', cells: []string{"R27-install-dir-readonly/a", "R27-install-dir-readonly/b-healthy"}},
	{id: "R27c", fn: "moveRestore", n: 2, kind: 'T', cells: []string{"R13-data-dir-back-after-aside/b"}},
	// A container's way back and its preconditions (#47).
	{id: "C01", fn: "backContainer", prefix: "docker is not on PATH", n: 1, kind: 'T', cells: []string{"C11-docker-gone/b-healthy"}},
	{id: "C02", fn: "containerBackPreconditions", prefix: "the old image %s is not there", n: 1, kind: 'T', cells: []string{"C02-old-image-gone/b-healthy"},
		why: "before the upgrade the container runs on it"},
	{id: "C03", fn: "containerBackPreconditions", prefix: "%s, which restores the archive, is not there", n: 1, kind: 'M', cells: []string{"C03-busybox-gone/a", "C03-busybox-gone/b-healthy"}},
	{id: "C04", fn: "containerBackPreconditions", prefix: "the volume %s exists already", n: 1, kind: 'T', cells: []string{"AWayBackWhoseVolumeExistsTouchesNothing"},
		why: "its name holds the run's id"},
	{id: "C05", fn: "containerBackPreconditions", prefix: "whether the ", n: 2, kind: 'T', why: dockerBetween},
	{id: "C06", fn: "containerBackPreconditions", prefix: "the container %s exists already", n: 1, kind: 'T', cells: []string{"C07-after-exists/b-healthy"},
		why: "its name holds the run's id"},
	{id: "C07", fn: "containerBackPreconditions", prefix: "the room for a restore of the volume", n: 1, kind: 'M', cells: []string{"C08-room-untold/a", "C08-room-untold/b-healthy"}},
	{id: "C08", fn: "containerBackPreconditions", prefix: "no room beside the volume", n: 1, kind: 'M', cells: []string{"C09-no-room/a", "C09-no-room/b-healthy"}},
	{id: "C09", fn: "containerBackPreconditions", prefix: "the copy %s does not answer", n: 1, kind: 'R', cells: []string{"C10-copy-wrong/b-healthy"}},
	{id: "C10", fn: "backContainer", prefix: "the run's container.json or run.json does not read", n: 1, kind: 'R', cells: []string{"C12-run-json-missing/b-healthy"}},
	{id: "C11", fn: "backContainer", prefix: "tracepad %s ran on the volume %s, and the run records no archive", n: 1, kind: 'R', why: "a container run archives before it renames and runs"},
	{id: "C12", fn: "backContainer", prefix: "; nothing was touched", n: 1, kind: 'R', cells: []string{"C02-old-image-gone/b-healthy", "C09-no-room/b-healthy", "C10-copy-wrong/b-healthy"},
		why: "passes on the preconditions' refusals and pathAsRecorded's"},
	{id: "C13", fn: "backContainer", prefix: "docker cannot say", n: 1, kind: 'T', why: dockerBetween},
	{id: "C14", fn: "backContainer", prefix: "; nothing was started", n: 1, kind: 'T', why: "passes on oldRef's: " + dockerBetween},
	{id: "C15", fn: "backContainer", prefix: "%s did not start on %s", n: 1, kind: 'T', cells: []string{"AFailedRunInTheWayBackDoesNotBlockTheNext"}},
	{id: "C16", fn: "startOld", prefix: "docker cannot say", n: 1, kind: 'T', why: dockerBetween},
	{id: "C17", fn: "startOld", prefix: "%s did not start again", n: 1, kind: 'T', cells: []string{"AContainersStartsThatFailAreTakenUpAgain"}},
	{id: "C18", fn: "renameBack", prefix: "docker cannot say", n: 3, kind: 'T', why: dockerBetween},
	{id: "C19", fn: "renameBack", prefix: "the container named ", n: 2, kind: 'T',
		why: "a container someone made under the run's names since; nothing is started over it, as TestAWayBackLeavesALaterRunsContainerAlone holds for the restore"},
	{id: "C20", fn: "renameBack", prefix: "%s did not start", n: 2, kind: 'T', cells: []string{"AContainersStartsThatFailAreTakenUpAgain"},
		why: "the run from the record; the other, the same container started again after it ran, is C17's act"},
	{id: "C21", fn: "renameBack", prefix: "; nothing was started", n: 1, kind: 'T', why: "passes on oldRef's: " + dockerBetween},
	{id: "C22", fn: "renameBack", prefix: "%s could not be put back and started", n: 1, kind: 'T', cells: []string{"AContainersStartsThatFailAreTakenUpAgain"}},
	{id: "C23", fn: "restoreVolume", prefix: "; nothing was touched", n: 1, kind: 'R', cells: []string{"AContainerArchiveChangedSinceIsNotRestored"}},
	{id: "C24", fn: "restoreVolume", prefix: "docker cannot say", n: 1, kind: 'T', why: dockerBetween},
	{id: "C25", fn: "restoreVolume", prefix: "the container named ", n: 1, kind: 'T', cells: []string{"AWayBackLeavesALaterRunsContainerAlone"}},
	{id: "C26", fn: "restoreVolume", prefix: "the archive's database fails its check", n: 1, kind: 'R', why: "the archive's checksum fails first"},
	{id: "C27", fn: "restoreVolume", prefix: "the volume ", n: 2, kind: 'T', cells: []string{"TestTheMatrices"},
		why: "the fault matrix's back/fail/back_volume makes the first; the second is a daemon that refuses a volume it can make"},
	{id: "C28", fn: "restoreVolume", prefix: "the restore into the new volume ", n: 1, kind: 'T', cells: []string{"AFailedRestoreNamesWhatItLeft"}},
	{id: "C29", fn: "setNewAside", prefix: "%s was asked to stop and has not", n: 1, kind: 'T', cells: []string{"C33-new-container-slow-to-stop/b-healthy"}},
	{id: "C30", fn: "setNewAside", prefix: "the new container was not set aside", n: 1, kind: 'T', cells: []string{"C33-new-container-slow-to-stop/b-healthy"},
		why: "the fault matrix's back/fail/back_set_aside makes it too"},
	{id: "C31", fn: "oldRef", prefix: "docker cannot say which image", n: 1, kind: 'T', why: dockerBetween},
	{id: "C32", fn: "clearName", prefix: "docker cannot say", n: 2, kind: 'T', why: dockerBetween},
	{id: "C33", fn: "clearName", prefix: "a container named ", n: 1, kind: 'T',
		why: "a container someone runs under the run's name since; nothing is started over it"},
	{id: "C34", fn: "clearName", prefix: "the container left as ", n: 1, kind: 'T', why: "a rename docker refuses of a container it just listed"},
	{id: "C35", fn: "inspectOne", prefix: "docker inspect %s answered", n: 1, kind: 'T', why: "docker answering two containers for one name or id"},
	// Binary-only runs.
	{id: "B1", fn: "backBinary", prefix: "; nothing was touched", n: 1, kind: 'T', cells: []string{"TestAWayBackDoesNotPutAnOlderBinaryUnderANewerServer"}},
	{id: "B2", fn: "backBinary", prefix: "%s could not be put back at %s: %v", n: 1, kind: 'T', cells: []string{"TestNoReplacementPutsAnOlderBinaryUnderANewerServer"}},
}

// dockerBetween is why a refusal on docker's silence has no cell: a daemon
// that stops answering between two of its own answers. The fake answers
// whole or not at all, and ADockerThatDoesNotAnswerSettlesNothing holds
// the settling of a run to it.
const dockerBetween = "a daemon that stops answering between two of its own answers"

// refusalSite is one refusal on the way back, as the lint reads it.
type refusalSite struct{ at, fn, msg string }

// refusalSites reads every fail, Errorf and errors.New on the way back: in
// back.go and settle.go's settle, in the checks of apply.go the way back
// calls (serversOn, putInPlace, launch, isServer), and in a container's way
// back and what it asks docker (container.go, docker.go's inspectOne). A
// site's message is the first string in it.
func refusalSites(t *testing.T) []refusalSite {
	t.Helper()
	scope := map[string]map[string]bool{
		"back.go":   nil,
		"settle.go": {"settle": true},
		"apply.go":  {"serversOn": true, "putInPlace": true, "launch": true, "isServer": true},
		// A container's way back and its preconditions (#47).
		"container.go": {"backContainer": true, "startOld": true, "renameBack": true, "restoreVolume": true, "setNewAside": true,
			"oldRef": true, "clearName": true, "containerBackPreconditions": true, "containerRunning": true},
		"docker.go": {"inspectOne": true},
	}
	fset := token.NewFileSet()
	var sites []refusalSite
	for file, funcs := range scope {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || (funcs != nil && !funcs[fd.Name.Name]) {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				name := sel.Sel.Name
				if x, ok := sel.X.(*ast.Ident); ok {
					name = x.Name + "." + name
				}
				if name != "j.fail" && name != "fmt.Errorf" && name != "errors.New" {
					return true
				}
				msg := ""
				ast.Inspect(call, func(m ast.Node) bool {
					if b, ok := m.(*ast.BasicLit); ok && b.Kind == token.STRING && msg == "" {
						msg, _ = strconv.Unquote(b.Value) // ignored: a literal the parser read unquotes
					}
					return msg == ""
				})
				sites = append(sites, refusalSite{at: fset.Position(call.Pos()).String(), fn: fd.Name.Name, msg: msg})
				return true
			})
		}
	}
	return sites
}

// Every refusal on the way back is a row of refusalReasons, and every row
// is refusals that are there (the twelfth review: what --back refuses and
// what the upgrade checks before its stop were two lists, five times over).
// A reason of the machine's (M) has a cell that makes it before the upgrade,
// or says why none can; every cell named is one that runs.
func TestEveryRefusalOfTheWayBackIsListed(t *testing.T) {
	t.Parallel()
	counts := make([]int, len(refusalReasons))
	for _, s := range refusalSites(t) {
		best := -1
		for i, r := range refusalReasons {
			if r.fn == s.fn && strings.HasPrefix(s.msg, r.prefix) && (best < 0 || len(r.prefix) > len(refusalReasons[best].prefix)) {
				best = i
			}
		}
		if best < 0 {
			t.Errorf("%s: a refusal in %s that no reason lists: %q", s.at, s.fn, s.msg)
			continue
		}
		counts[best]++
	}
	cells := map[string]bool{}
	for _, c := range refusalCases() {
		cells[c.name] = true
	}
	for _, c := range roomCases(freeBytes) {
		cells[c.name] = true
	}
	for _, c := range containerRefusalCases() {
		cells[c.name] = true
	}
	for _, c := range containerScenarios {
		cells[c.name] = true
	}
	tests := testNames(t)
	for i, r := range refusalReasons {
		if counts[i] != r.n {
			t.Errorf("%s: %d refusals in %s begin %q, the row says %d", r.id, counts[i], r.fn, r.prefix, r.n)
		}
		before := false
		for _, c := range r.cells {
			switch {
			case strings.HasPrefix(c, "Test"):
				if !tests[c] {
					t.Errorf("%s names %s, which is no test", r.id, c)
				}
			case !cells[c]:
				t.Errorf("%s names the cell %s, which is not one", r.id, c)
			}
			before = before || strings.Contains(c, "/a") || strings.HasPrefix(c, "Test")
		}
		if len(r.cells) == 0 && r.why == "" {
			t.Errorf("%s has no cell, and does not say why", r.id)
		}
		if r.kind == 'M' && !before && r.why == "" {
			t.Errorf("%s is the machine's, and nothing makes it before the upgrade", r.id)
		}
	}
}

// testNames are the package's tests, whatever their build tags.
func testNames(t *testing.T) map[string]bool {
	t.Helper()
	files, _ := filepath.Glob("*_test.go")
	names := map[string]bool{}
	fset := token.NewFileSet()
	for _, file := range files {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && strings.HasPrefix(fd.Name.Name, "Test") {
				names[fd.Name.Name] = true
			}
		}
	}
	return names
}
