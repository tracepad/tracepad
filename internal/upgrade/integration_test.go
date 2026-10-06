//go:build unix && upgradeint

package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// The integration tests run `tracepad upgrade` against real servers: the
// binary built here twice, with two versions stamped, and served from a
// file:// mirror with a real checksums.txt (spec 054, Testing).

const (
	vOld    = "0.9.0"
	vNew    = "0.9.1"
	vBroken = "0.9.2"
	testPK  = "tp-pk-0123456789abcdef0123456789abcdef"
	testSK  = "tp-sk-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

var (
	buildOnce sync.Once
	buildDir  string
	buildErr  error
)

// builtBinaries builds the server at two versions, and a release whose binary
// answers its version and does not serve.
func builtBinaries(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the binary twice")
	}
	buildOnce.Do(func() {
		buildDir, buildErr = os.MkdirTemp("", "tracepad-upgrade-bin-")
		if buildErr != nil {
			return
		}
		for _, v := range []string{vOld, vNew} {
			cmd := exec.Command("go", "build", "-ldflags", "-X main.version="+v, "-o", filepath.Join(buildDir, v), "github.com/tracepad/tracepad/cmd/tracepad")
			if out, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("go build %s: %v\n%s", v, err, out)
				return
			}
		}
		broken := "#!/bin/sh\ncase \"$1\" in version) echo " + vBroken + " ;; *) echo 'this release does not serve' >&2; exit 1 ;; esac\n"
		buildErr = os.WriteFile(filepath.Join(buildDir, vBroken), []byte(broken), 0o755)
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return buildDir
}

// publish writes release v of bin into the mirror for this machine.
func publish(t *testing.T, mirror, v, bin string) {
	t.Helper()
	dir := filepath.Join(mirror, "download", "v"+v)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("tracepad_%s_%s_%s.tar.gz", v, runtime.GOOS, runtime.GOARCH)
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "tracepad", Mode: 0o755, Size: int64(len(data)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(data)
	_ = tw.Close()
	_ = gz.Close()
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	if err := os.WriteFile(filepath.Join(dir, "checksums.txt"), []byte(hex.EncodeToString(sum[:])+"  "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// world is one test's machine: a home, an install directory, a mirror, a
// data directory and a server.
type world struct {
	t       *testing.T
	home    string
	bin     string
	install string
	data    string
	listen  string
	url     string
	mirror  string
	env     map[string]string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	bins := builtBinaries(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, home: filepath.Join(root, "home"), mirror: filepath.Join(root, "mirror")}
	w.bin = filepath.Join(w.home, ".local", "bin")
	w.install = filepath.Join(w.bin, "tracepad")
	w.data = filepath.Join(w.home, "data")
	port := freePort(t)
	w.listen = "127.0.0.1:" + strconv.Itoa(port)
	w.url = "http://" + w.listen
	for _, d := range []string{w.bin, w.data} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := copyFile(filepath.Join(bins, vOld), w.install, 0o755); err != nil {
		t.Fatal(err)
	}
	publish(t, w.mirror, vNew, filepath.Join(bins, vNew))
	publish(t, w.mirror, vBroken, filepath.Join(bins, vBroken))
	publish(t, w.mirror, vOld, filepath.Join(bins, vOld))
	w.env = map[string]string{"TRACEPAD_API_KEY": testSK}
	t.Cleanup(w.stopAll)
	return w
}

// start runs the installed binary as setup.md does, with a variable of the
// person's (a sweep interval) that the upgrade must keep.
func (w *world) start() int {
	w.t.Helper()
	return w.startIn(w.home)
}

// startIn starts the server with dir as its working directory.
func (w *world) startIn(dir string) int {
	w.t.Helper()
	return w.startWith(dir, true)
}

// startWith starts the server; reap false leaves it a zombie once it exits,
// as a harness that spawns and never waits does.
func (w *world) startWith(dir string, reap bool) int {
	w.t.Helper()
	log, err := os.OpenFile(filepath.Join(w.data, "server.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		w.t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(w.install, "serve", "--listen", w.listen, "--data-dir", w.data)
	cmd.Args[0] = "tracepad"
	cmd.Env = []string{"HOME=" + w.home, "PATH=/usr/bin:/bin",
		"TRACEPAD_PROJECTS=demo:" + testPK + ":" + testSK, "TRACEPAD_SWEEP_INTERVAL=2h", "TRACEPAD_SETUP=off"}
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		w.t.Fatal(err)
	}
	if reap {
		go func() { _ = cmd.Wait() }()
	} else {
		w.t.Cleanup(func() { _ = cmd.Wait() })
	}
	if err := os.WriteFile(filepath.Join(w.data, "server.pid"), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600); err != nil {
		w.t.Fatal(err)
	}
	w.waitVersion(vOld)
	return cmd.Process.Pid
}

// startAt starts the installed binary, which is version v.
func (w *world) startAt(v string) {
	w.t.Helper()
	log, err := os.OpenFile(filepath.Join(w.data, "server.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		w.t.Fatal(err)
	}
	defer log.Close()
	cmd := exec.Command(w.install, "serve", "--listen", w.listen, "--data-dir", w.data)
	cmd.Args[0] = "tracepad"
	cmd.Env = []string{"HOME=" + w.home, "PATH=/usr/bin:/bin", "TRACEPAD_PROJECTS=demo:" + testPK + ":" + testSK, "TRACEPAD_SETUP=off"}
	cmd.Dir = w.home
	cmd.Stdout, cmd.Stderr = log, log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		w.t.Fatal(err)
	}
	go func() { _ = cmd.Wait() }()
	w.waitVersion(v)
}

func (w *world) waitVersion(v string) {
	w.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if got, err := health(context.Background(), http.DefaultClient, w.url); err == nil && got == v {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	w.t.Fatalf("%s did not answer as %s", w.url, v)
}

// stopAll stops whatever server holds this world's data directory: only a
// process whose arguments name it.
func (w *world) stopAll() {
	for _, dir := range []string{w.data} {
		pid, err := lockedBy(dir)
		if err != nil || !alive(pid) {
			continue
		}
		p, err := newSystem().Inspect(pid)
		if err != nil || !slices.Contains(p.Argv, w.data) {
			continue
		}
		_ = syscall.Kill(pid, syscall.SIGTERM)
		for i := 0; i < 100 && alive(pid); i++ {
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func (w *world) sendTrace(id int) {
	w.t.Helper()
	body := fmt.Sprintf(`{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"%032x","spanId":"%016x","name":"upgrade-test","startTimeUnixNano":"1700000000000000000","endTimeUnixNano":"1700000001000000000"}]}]}]}`, id, id)
	req, _ := http.NewRequest(http.MethodPost, w.url+"/v1/traces", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testSK)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		w.t.Fatalf("ingest answered %d", resp.StatusCode)
	}
}

func (w *world) count() int64 {
	w.t.Helper()
	r := &runner{deps: Deps{HTTP: http.DefaultClient, Getenv: func(string) string { return testSK }}}
	deadline := time.Now().Add(10 * time.Second)
	var n *int64
	for time.Now().Before(deadline) {
		n, _ = r.traceCount(context.Background(), w.url)
		if n != nil {
			return *n
		}
		time.Sleep(100 * time.Millisecond)
	}
	w.t.Fatal("no trace count")
	return 0
}

func (w *world) waitCount(want int64) {
	w.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if w.count() == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	w.t.Fatalf("the count did not reach %d", want)
}

func (w *world) deps() Deps {
	getenv := func(k string) string { return w.env[k] }
	return Deps{
		Sys:    newSystem(),
		HTTP:   &http.Client{Transport: &http.Transport{Proxy: nil}},
		Getenv: getenv,
		Releases: &Releases{Base: "file://" + w.mirror, Mirror: true, OS: runtime.GOOS, Arch: runtime.GOARCH,
			Version: binaryVersion,
			Attest:  func(context.Context, string) (string, error) { return "attestation not checked: a test mirror", nil }},
		InstallDir: w.bin,
		Backups:    filepath.Join(w.home, "tracepad-backups"),
		Home:       w.home,
		Cwd:        w.home,
		Self:       w.install,
		LookPath:   func(string) string { return "" },
		Version:    binaryVersion,
		Skills:     runSkills,
		Now:        time.Now,
		Sleep:      sleepCtx,
		StopWait:   30 * time.Second,
		HealthWait: 30 * time.Second,
		ProbeWait:  200 * time.Millisecond,
	}
}

// upgrade runs the command and decodes its --json report.
func (w *world) run(deps Deps, args ...string) (Report, int) {
	w.t.Helper()
	return w.runCtx(context.Background(), deps, args...)
}

func (w *world) runCtx(ctx context.Context, deps Deps, args ...string) (Report, int) {
	w.t.Helper()
	var out, errOut bytes.Buffer
	code := run(ctx, Options{Args: append(args, "--json"), Version: vOld, Stdout: &out, Stderr: &errOut, Getenv: deps.Getenv}, deps)
	var rep Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		w.t.Fatalf("the report is not JSON (%v): %s %s", err, out.String(), errOut.String())
	}
	w.t.Logf("upgrade %v → %d: %s\n%s", args, code, rep.Summary, strings.Join(rep.Done, "\n"))
	return rep, code
}

func (w *world) installedVersion() string {
	v, _ := binaryVersion(context.Background(), w.install)
	return v
}

func TestUpgradeARunningServerKeepsItsArgumentsEnvironmentAndTraces(t *testing.T) {
	w := newWorld(t)
	oldPID := w.start()
	for i := 1; i <= 3; i++ {
		w.sendTrace(i)
	}
	w.waitCount(3)

	plan, code := w.run(w.deps(), "--plan", "--to", vNew, "--data-dir", w.data)
	if code != exitPending || len(plan.Plan) == 0 {
		t.Fatalf("plan: exit %d, %+v", code, plan)
	}
	if !alive(oldPID) || w.installedVersion() != vOld {
		t.Fatal("the plan changed something")
	}

	rep, code := w.run(w.deps(), "--to", vNew, "--data-dir", w.data)
	if code != exitOK || rep.Check == nil || rep.Check.Verdict != verdictHealthy {
		t.Fatalf("upgrade: exit %d, %+v", code, rep)
	}
	w.waitVersion(vNew)
	if w.installedVersion() != vNew {
		t.Errorf("installed binary is %s", w.installedVersion())
	}
	if got := w.count(); got != 3 {
		t.Errorf("traces after: %d", got)
	}
	if !strings.Contains(rep.Check.LogLine, "tracepad "+vNew) {
		t.Errorf("first log line: %q", rep.Check.LogLine)
	}
	newPID, err := lockedBy(w.data)
	if err != nil || newPID == oldPID {
		t.Fatalf("lock records %d (%v)", newPID, err)
	}
	p, err := newSystem().Inspect(newPID)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"tracepad", "serve", "--listen", w.listen, "--data-dir", w.data}; !slices.Equal(p.Argv, want) {
		t.Errorf("arguments %q, want %q", p.Argv, want)
	}
	if p.Getenv("TRACEPAD_SWEEP_INTERVAL") != "2h" || !strings.Contains(p.Getenv("TRACEPAD_PROJECTS"), testPK) {
		t.Errorf("environment not kept: %q", p.Env)
	}
	if b, _ := os.ReadFile(filepath.Join(w.data, "server.pid")); strings.TrimSpace(string(b)) != strconv.Itoa(newPID) {
		t.Errorf("server.pid holds %q, the server is %d", b, newPID)
	}
	if rep.Run == nil {
		t.Fatal("no run")
	}
	if fi, err := os.Stat(rep.Run.Dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("run directory %v %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Join(rep.Run.Dir, "server.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("server.json %v %v", fi, err)
	}

	// A trace after the upgrade, then the way back: the old version, its three
	// traces, and what the new one held set aside.
	w.sendTrace(4)
	w.waitCount(4)
	back, code := w.run(w.deps(), "--back", rep.Run.ID)
	if code != exitOK {
		t.Fatalf("back: exit %d, %+v", code, back)
	}
	w.waitVersion(vOld)
	if got := w.count(); got != 3 {
		t.Errorf("traces after the way back: %d", got)
	}
	if w.installedVersion() != vOld {
		t.Errorf("installed binary is %s after the way back", w.installedVersion())
	}
	after := w.data + ".after-" + rep.Run.ID
	if _, err := os.Stat(filepath.Join(after, "tracepad.db")); err != nil {
		t.Errorf("what the new version left is not set aside: %v", err)
	}
	if !slices.Contains(back.SetAside, after) {
		t.Errorf("set aside %q, want %s in it", back.SetAside, after)
	}

	// Twice is refused, and changes nothing.
	again, code := w.run(w.deps(), "--back", rep.Run.ID)
	if code != exitRefused {
		t.Fatalf("second way back: exit %d, %+v", code, again)
	}
	w.waitVersion(vOld)
}

func TestABrokenReleaseIsNotHealthyAndTheWayBackRuns(t *testing.T) {
	w := newWorld(t)
	w.start()
	w.sendTrace(1)
	w.waitCount(1)
	rep, code := w.run(w.deps(), "--to", vBroken, "--data-dir", w.data)
	if code != exitWentBack {
		t.Fatalf("exit %d, %+v", code, rep)
	}
	w.waitVersion(vOld)
	if got := w.count(); got != 1 {
		t.Errorf("traces after the way back: %d", got)
	}
	if w.installedVersion() != vOld {
		t.Errorf("installed binary is %s", w.installedVersion())
	}
	if _, err := os.Stat(w.data + ".after-" + rep.Run.ID); err != nil {
		t.Errorf("nothing set aside: %v", err)
	}
	// The report names what runs after the run: the old version the way
	// back started (Decision 19 (d)).
	running, _ := lockedBy(w.data)
	for _, s := range rep.Servers {
		if s.Target && (s.PID != running || s.Version != vOld) {
			t.Errorf("the report names pid %d (%s); pid %d runs %s", s.PID, s.Version, running, vOld)
		}
	}
}

func TestATruncatedArchiveStartsTheOldVersionOnTheDataAsItWas(t *testing.T) {
	w := newWorld(t)
	w.start()
	w.sendTrace(1)
	w.waitCount(1)
	afterArchive = func(path string) {
		fi, _ := os.Stat(path)
		_ = os.Truncate(path, fi.Size()/2)
	}
	t.Cleanup(func() { afterArchive = func(string) {} })
	rep, code := w.run(w.deps(), "--to", vNew, "--data-dir", w.data)
	if code != exitWentBack {
		t.Fatalf("exit %d, %+v", code, rep)
	}
	w.waitVersion(vOld)
	if got := w.count(); got != 1 {
		t.Errorf("traces: %d", got)
	}
	if _, err := os.Stat(w.data + ".after-" + rep.Run.ID); err == nil {
		t.Error("the data was set aside, though nothing had run on it")
	}
}

// The live run of spec 054 found it: a later run's way back starts a server
// of its own, and an earlier run's way back must stop that one, not rename
// the data directory under it.
func TestAnEarlierRunGoesBackPastALaterRunsWayBack(t *testing.T) {
	w := newWorld(t)
	w.start()
	w.sendTrace(1)
	w.sendTrace(2)
	w.waitCount(2)
	first, code := w.run(w.deps(), "--to", vNew, "--data-dir", w.data)
	if code != exitOK {
		t.Fatalf("first: %d %+v", code, first)
	}
	w.sendTrace(3)
	w.waitCount(3)
	second, code := w.run(w.deps(), "--to", vBroken, "--data-dir", w.data)
	if code != exitWentBack {
		t.Fatalf("second: %d %+v", code, second)
	}
	held, _ := lockedBy(w.data)
	back, code := w.run(w.deps(), "--back", first.Run.ID)
	if code != exitOK {
		t.Fatalf("back: %d %s", code, back.Summary)
	}
	w.waitVersion(vOld)
	if got := w.count(); got != 2 {
		t.Errorf("traces: %d", got)
	}
	if alive(held) {
		t.Errorf("pid %d, the later run's server, still runs", held)
	}
	for _, run := range []string{first.Run.ID, second.Run.ID} {
		if _, err := os.Stat(filepath.Join(w.data+".after-"+run, "tracepad.db")); err != nil {
			t.Errorf("not set aside for %s: %v", run, err)
		}
	}
}

func TestALockThatRecordsAnotherProcessIsThePersons(t *testing.T) {
	w := newWorld(t)
	pid := w.start()
	other := exec.Command("sleep", "60")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	lock := filepath.Join(w.data, "tracepad.db"+store.LockSuffix)
	if err := os.WriteFile(lock, []byte(strconv.Itoa(other.Process.Pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep, code := w.run(w.deps(), "--to", vNew, "--data-dir", w.data)
	if code != exitRefused || !strings.Contains(rep.Summary, "does not record it") {
		t.Fatalf("exit %d, %s", code, rep.Summary)
	}
	if !alive(pid) || !alive(other.Process.Pid) || w.installedVersion() != vOld {
		t.Error("something was touched")
	}
	_ = os.WriteFile(lock, []byte(strconv.Itoa(pid)+"\n"), 0o600)
}

func TestTheWayBackStopsOnlyThisServer(t *testing.T) {
	w := newWorld(t)
	w.start()
	rep, code := w.run(w.deps(), "--to", vNew, "--data-dir", w.data)
	if code != exitOK {
		t.Fatalf("upgrade: exit %d, %+v", code, rep)
	}
	newPID, _ := lockedBy(w.data)
	other := exec.Command("sleep", "60")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Process.Kill(); _ = other.Wait() })
	lock := filepath.Join(w.data, "tracepad.db"+store.LockSuffix)
	_ = os.WriteFile(lock, []byte(strconv.Itoa(other.Process.Pid)+"\n"), 0o600)
	back, code := w.run(w.deps(), "--back", rep.Run.ID)
	if code != exitStuck || !strings.Contains(back.Summary, "no longer this server") {
		t.Fatalf("back: exit %d, %s", code, back.Summary)
	}
	if !alive(other.Process.Pid) || !alive(newPID) {
		t.Error("a process was stopped")
	}
	if _, err := os.Stat(w.data + ".after-" + rep.Run.ID); err == nil {
		t.Error("the data was set aside")
	}
	_ = os.WriteFile(lock, []byte(strconv.Itoa(newPID)+"\n"), 0o600)
}

func TestACountReadBeforeAndNotAfterIsTheirsToDecide(t *testing.T) {
	w := newWorld(t)
	oldPID := w.start()
	w.sendTrace(1)
	w.waitCount(1)
	deps := w.deps()
	// The key reads while the old server runs, and not once it has stopped.
	deps.Getenv = func(k string) string {
		if k != "TRACEPAD_API_KEY" || !alive(oldPID) {
			return ""
		}
		return testSK
	}
	rep, code := w.run(deps, "--to", vNew, "--data-dir", w.data)
	if code != exitDecide || rep.Check == nil || rep.Check.Verdict != verdictDecide {
		t.Fatalf("exit %d, %+v", code, rep)
	}
	w.waitVersion(vNew)
	if len(rep.Person) == 0 || !strings.Contains(strings.Join(rep.Person, " "), "--back "+rep.Run.ID) {
		t.Errorf("no way on for the person: %q", rep.Person)
	}
}

func TestWithoutAKeyTheCountsAreNotCompared(t *testing.T) {
	w := newWorld(t)
	w.start()
	delete(w.env, "TRACEPAD_API_KEY")
	rep, code := w.run(w.deps(), "--to", vNew, "--data-dir", w.data)
	if code != exitOK || rep.Check == nil || rep.Check.CountNote == "" {
		t.Fatalf("exit %d, %+v", code, rep)
	}
}

// An interrupt after the stop (Ctrl-C, an agent's timeout) must not leave the
// server down: the swap and its way back finish on a context of their own
// (the review of #1).
func TestAnInterruptAfterTheStopStillBringsTheServerBack(t *testing.T) {
	w := newWorld(t)
	w.start()
	w.sendTrace(1)
	w.waitCount(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	afterArchive = func(path string) {
		cancel()
		fi, _ := os.Stat(path)
		_ = os.Truncate(path, fi.Size()/2)
	}
	t.Cleanup(func() { afterArchive = func(string) {} })
	rep, code := w.runCtx(ctx, w.deps(), "--to", vNew, "--data-dir", w.data)
	if code != exitWentBack {
		t.Fatalf("exit %d, %s", code, rep.Summary)
	}
	w.waitVersion(vOld)
	if got := w.count(); got != 1 {
		t.Errorf("traces: %d", got)
	}
}

// The install script already put the target in place and the server still
// runs the old version: nothing is downloaded but the old version's copy.
func TestAnInstalledTargetIsNotDownloadedAgain(t *testing.T) {
	w := newWorld(t)
	w.start()
	next := filepath.Join(filepath.Dir(w.install), ".next")
	if err := copyFile(filepath.Join(builtBinaries(t), vNew), next, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, w.install); err != nil {
		t.Fatal(err)
	}
	archives, _ := filepath.Glob(filepath.Join(w.mirror, "download", "v"+vNew, "*.tar.gz"))
	for _, a := range archives {
		_ = os.Remove(a)
	}
	rep, code := w.run(w.deps(), "--to", vNew, "--data-dir", w.data)
	if code != exitOK {
		t.Fatalf("exit %d, %s", code, rep.Summary)
	}
	w.waitVersion(vNew)
	if _, err := os.Stat(filepath.Join(rep.Run.Dir, "tracepad-"+vNew)); err == nil {
		t.Error("the target was downloaded again")
	}
}

// A run refused before anything stopped leaves no directory behind.
func TestARefusedRunLeavesNoDirectory(t *testing.T) {
	w := newWorld(t)
	pid := w.start()
	archives, _ := filepath.Glob(filepath.Join(w.mirror, "download", "v"+vNew, "*.tar.gz"))
	for _, a := range archives {
		_ = os.Remove(a)
	}
	rep, code := w.run(w.deps(), "--to", vNew, "--data-dir", w.data)
	if code != exitRefused || rep.Run != nil {
		t.Fatalf("exit %d, run %+v: %s", code, rep.Run, rep.Summary)
	}
	entries, _ := os.ReadDir(filepath.Join(w.home, "tracepad-backups"))
	for _, e := range entries {
		if e.Name() != "runs.lock" {
			t.Errorf("left behind: %s", e.Name())
		}
	}
	if !alive(pid) {
		t.Error("the server was stopped")
	}
}

// A way back that stopped after it had moved the data is taken up where it
// stopped, and its refusal never asks for what the new version wrote to be
// removed (the second review).
func TestAWayBackCutShortAfterTheMoveIsTakenUpAgain(t *testing.T) {
	w := newWorld(t)
	w.start()
	w.sendTrace(1)
	w.sendTrace(2)
	w.waitCount(2)
	rep, code := w.run(w.deps(), "--to", vNew, "--data-dir", w.data)
	if code != exitOK {
		t.Fatalf("upgrade: %d %s", code, rep.Summary)
	}
	w.sendTrace(3)
	w.waitCount(3)
	deps := w.deps()
	failing := true
	deps.Version = func(ctx context.Context, path string) (string, error) {
		if failing && strings.Contains(filepath.Base(path), ".tracepad.") {
			return "", fmt.Errorf("a disk that is full")
		}
		return binaryVersion(ctx, path)
	}
	first, code := w.run(deps, "--back", rep.Run.ID)
	after := w.data + ".after-" + rep.Run.ID
	if code != exitStuck || strings.Contains(first.Summary, "rm -r") {
		t.Fatalf("first --back: %d %s", code, first.Summary)
	}
	if _, err := os.Stat(filepath.Join(after, "tracepad.db")); err != nil {
		t.Fatalf("not set aside: %v", err)
	}
	failing = false
	second, code := w.run(deps, "--back", rep.Run.ID)
	if code != exitOK {
		t.Fatalf("second --back: %d %s", code, second.Summary)
	}
	w.waitVersion(vOld)
	if got := w.count(); got != 2 {
		t.Errorf("traces: %d", got)
	}
}

// Cut short between its two moves — the data set aside, the restore not yet
// in its place — the way back is taken up at the second move.
func TestAWayBackCutShortBetweenItsMovesIsTakenUpAgain(t *testing.T) {
	w := newWorld(t)
	w.start()
	w.sendTrace(1)
	w.waitCount(1)
	rep, code := w.run(w.deps(), "--to", vNew, "--data-dir", w.data)
	if code != exitOK {
		t.Fatalf("upgrade: %d %s", code, rep.Summary)
	}
	w.sendTrace(2)
	w.waitCount(2)
	t.Cleanup(func() { renameDir = os.Rename })
	renameDir = func(from, to string) error {
		if to == w.data {
			return fmt.Errorf("an interrupted rename")
		}
		return os.Rename(from, to)
	}
	first, code := w.run(w.deps(), "--back", rep.Run.ID)
	if code != exitStuck || strings.Contains(first.Summary, "rm -r") {
		t.Fatalf("first --back: %d %s", code, first.Summary)
	}
	renameDir = os.Rename
	second, code := w.run(w.deps(), "--back", rep.Run.ID)
	if code != exitOK {
		t.Fatalf("second --back: %d %s", code, second.Summary)
	}
	w.waitVersion(vOld)
	if got := w.count(); got != 1 {
		t.Errorf("traces: %d", got)
	}
}

// A server started from a directory removed since starts again in its data
// directory, and the report says so (the third review).
func TestAServerWhoseWorkingDirectoryIsGoneStartsInItsData(t *testing.T) {
	w := newWorld(t)
	gone := filepath.Join(w.home, "worktree")
	if err := os.Mkdir(gone, 0o700); err != nil {
		t.Fatal(err)
	}
	w.startIn(gone)
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	rep, code := w.run(w.deps(), "--to", vNew, "--data-dir", w.data)
	if code != exitOK {
		t.Fatalf("exit %d, %s", code, rep.Summary)
	}
	w.waitVersion(vNew)
	if !strings.Contains(strings.Join(rep.Notes, "\n"), "is gone; it starts again in its data directory") {
		t.Errorf("notes: %q", rep.Notes)
	}
}

// A way back of a binary-only run, after the person started a server on the
// newer binary, would put an older binary under a migrated database: refused,
// as every downgrade is (the fourth review).
func TestAWayBackDoesNotPutAnOlderBinaryUnderANewerServer(t *testing.T) {
	w := newWorld(t)
	rep, code := w.run(w.deps(), "--to", vNew)
	if code != exitOK || rep.Run == nil {
		t.Fatalf("binary-only upgrade: %d %s", code, rep.Summary)
	}
	if w.installedVersion() != vNew {
		t.Fatalf("installed %s", w.installedVersion())
	}
	w.startAt(vNew)
	back, code := w.run(w.deps(), "--back", rep.Run.ID)
	if code == exitOK || !strings.Contains(back.Summary, "its next restart would be "+vOld) {
		t.Fatalf("--back: %d %s", code, back.Summary)
	}
	if w.installedVersion() != vNew {
		t.Errorf("the binary was put back: %s", w.installedVersion())
	}
}

// The old server's parent never reaps it: stopped, it is a zombie, which
// kill(pid, 0) still answers. The upgrade takes it for gone (the final
// review: it used to wait out its sixty seconds and end stuck).
func TestAnUnreapedServerIsGoneOnceItStops(t *testing.T) {
	w := newWorld(t)
	w.startWith(w.home, false)
	rep, code := w.run(w.deps(), "--to", vNew, "--data-dir", w.data)
	if code != exitOK {
		t.Fatalf("exit %d, %s", code, rep.Summary)
	}
	w.waitVersion(vNew)
}
