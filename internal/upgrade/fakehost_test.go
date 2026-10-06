//go:build unix

package upgrade

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/store"
)

// fakeHost is a machine whose servers are in this process: a server is an
// HTTP listener on the address its arguments name, holding its data
// directory's lock with a real flock and recording its PID there as the
// server does, and counting the traces of the SQLite database in its data
// directory. Its version is what its binary — a script — answers. It is
// what the gate's fault matrix runs on (spec 054 #27): everything the
// command does to a server, without building one.
type fakeHost struct {
	t     *testing.T
	mu    sync.Mutex
	next  int
	procs map[int]*fakeServer
	// broken are versions whose server exits at once; uncounted, versions
	// whose /api/v1/system fails while set.
	broken    map[string]bool
	uncounted map[string]bool
	// elsewhere are versions whose server opens another data directory
	// than its arguments name.
	elsewhere map[string]string
	// held is every "<version> <data directory>" a server took: what may
	// have written the data.
	held map[string]bool
	// slow is how long every server takes to answer, while set: longer than
	// the command waits (the sixth review's "slow answer" cells).
	slow time.Duration
}

// lag waits as a slow server does before it answers.
func (h *fakeHost) lag(r *http.Request) bool {
	h.mu.Lock()
	d := h.slow
	h.mu.Unlock()
	if d == 0 {
		return true
	}
	select {
	case <-time.After(d):
		return true
	case <-r.Context().Done():
		return false
	}
}

func (h *fakeHost) setSlow(d time.Duration) {
	h.mu.Lock()
	h.slow = d
	h.mu.Unlock()
}

type fakeServer struct {
	p       Process
	release func()
	srv     *http.Server
	done    chan struct{}
	stopped bool
}

func newFakeHost(t *testing.T) *fakeHost {
	h := &fakeHost{t: t, next: 70000, procs: map[int]*fakeServer{}, broken: map[string]bool{}, uncounted: map[string]bool{}, elsewhere: map[string]string{}, held: map[string]bool{}}
	t.Cleanup(func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		for _, s := range h.procs {
			s.stop()
		}
	})
	return h
}

func (s *fakeServer) stop() {
	if s.stopped {
		return
	}
	s.stopped = true
	if s.srv != nil {
		_ = s.srv.Close()
	}
	if s.release != nil {
		s.release()
	}
	close(s.done)
}

func (h *fakeHost) Candidates() ([]Process, int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Process
	for _, s := range h.procs {
		if !s.stopped {
			out = append(out, s.p)
		}
	}
	slices.SortFunc(out, func(a, b Process) int { return a.PID - b.PID })
	return out, 0, nil
}

func (h *fakeHost) Inspect(pid int) (Process, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok := h.procs[pid]; ok && !s.stopped {
		return s.p, nil
	}
	return Process{}, fmt.Errorf("pid %d is gone", pid)
}

func (h *fakeHost) Alive(pid int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.procs[pid]
	return ok && !s.stopped
}

func (h *fakeHost) Signal(pid int, _ syscall.Signal) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.procs[pid]
	if !ok || s.stopped {
		return syscall.ESRCH
	}
	s.stop()
	return nil
}

// Start runs a fake server as the binary at spec.Path would: it takes the
// data directory's lock or exits, records its PID, and listens.
func (h *fakeHost) Start(spec StartSpec) (Started, error) {
	version, err := scriptVersion(context.Background(), spec.Path)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	h.next++
	pid := h.next
	s := &fakeServer{p: Process{PID: pid, PPID: os.Getpid(), Argv: spec.Argv, Env: spec.Env, Exe: spec.Path, Cwd: spec.Dir}, done: make(chan struct{})}
	h.procs[pid] = s
	h.mu.Unlock()

	dataDir, listen, err := resolveServer(s.p)
	exit := func() (Started, error) {
		h.mu.Lock()
		s.stop()
		h.mu.Unlock()
		return &started{pid: pid, done: s.done}, nil
	}
	if err != nil || h.broken[version] {
		return exit()
	}
	if other := h.elsewhere[version]; other != "" {
		dataDir = other
	}
	lock := filepath.Join(dataDir, dataDBName+store.LockSuffix)
	release, ok, err := store.TryLock(filepath.Join(dataDir, dataDBName))
	if err != nil || !ok {
		return exit()
	}
	s.release = release
	h.mu.Lock()
	h.held[version+" "+dataDir] = true
	h.mu.Unlock()
	_ = os.WriteFile(lock, []byte(strconv.Itoa(pid)+"\n"), 0o600)
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return exit()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if !h.lag(r) {
			return
		}
		fmt.Fprintf(w, `{"status":"ok","version":%q}`, version)
	})
	mux.HandleFunc("/api/v1/system", func(w http.ResponseWriter, r *http.Request) {
		if !h.lag(r) {
			return
		}
		h.mu.Lock()
		failing := h.uncounted[version]
		h.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+testSKFake || failing {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		n, err := countTraces(filepath.Join(dataDir, dataDBName))
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `{"database":{"rows":{"traces":%d}}}`, n)
	})
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = s.srv.Serve(ln) }()
	return &started{pid: pid, done: s.done}, nil
}

const testSKFake = "tp-sk-fake"

func countTraces(db string) (int64, error) {
	conn, err := sql.Open("sqlite", db)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var n int64
	err = conn.QueryRow("SELECT count(*) FROM traces").Scan(&n)
	return n, err
}

// fakeWorld is one machine of the gate's matrix: a home, an install
// directory holding the old version, a mirror, a data directory with a real
// database of n traces, and its server.
type fakeWorld struct {
	t       *testing.T
	host    *fakeHost
	home    string
	install string
	data    string
	listen  string
	mirror  string
}

const (
	fOld    = "0.5.0"
	fNew    = "0.5.1"
	fBroken = "0.5.2"
)

func newFakeWorld(t *testing.T, traces int) *fakeWorld {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	w := &fakeWorld{t: t, host: newFakeHost(t), home: filepath.Join(root, "home"), mirror: filepath.Join(root, "mirror")}
	w.install = filepath.Join(w.home, ".local", "bin", "tracepad")
	w.data = filepath.Join(w.home, "data")
	for _, d := range []string{filepath.Dir(w.install), w.data} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	scriptBinary(t, w.install, fOld)
	for _, v := range []string{fOld, fNew, fBroken} {
		mirrorRelease(t, w.mirror, v, v, false)
	}
	w.host.broken[fBroken] = true
	db, err := sql.Open("sqlite", filepath.Join(w.data, dataDBName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE traces (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < traces; i++ {
		if _, err := db.Exec("INSERT INTO traces VALUES (?)", i); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	w.listen = l.Addr().String()
	l.Close()
	argv := []string{"tracepad", "serve", "--listen", w.listen, "--data-dir", w.data}
	if _, err := w.host.Start(StartSpec{Path: w.install, Argv: argv, Env: []string{"HOME=" + w.home}, Dir: w.home, Log: filepath.Join(w.data, "server.log")}); err != nil {
		t.Fatal(err)
	}
	w.waitVersion(fOld)
	return w
}

// freeAddr is a loopback address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

func (w *fakeWorld) url() string { return "http://" + w.listen }

func (w *fakeWorld) answers() string {
	v, _ := health(context.Background(), http.DefaultClient, w.url())
	return v
}

func (w *fakeWorld) waitVersion(v string) {
	w.t.Helper()
	for i := 0; i < 100; i++ {
		if w.answers() == v {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	w.t.Fatalf("%s does not answer as %s", w.url(), v)
}

// addTrace writes a trace as the running server would.
func (w *fakeWorld) addTrace() {
	w.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(w.data, dataDBName))
	if err != nil {
		w.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("INSERT INTO traces VALUES (99)"); err != nil {
		w.t.Fatal(err)
	}
}

func (w *fakeWorld) deps() Deps {
	return Deps{
		Sys:        w.host,
		HTTP:       &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 2 * time.Second},
		Releases:   fakeReleases(w.t, w.mirror),
		InstallDir: filepath.Dir(w.install),
		Backups:    filepath.Join(w.home, "tracepad-backups"),
		Home:       w.home,
		Cwd:        w.home,
		Self:       w.install,
		Getenv: func(k string) string {
			if k == "TRACEPAD_API_KEY" {
				return testSKFake
			}
			return ""
		},
		LookPath:     func(string) string { return "" },
		Version:      scriptVersion,
		Skills:       func(context.Context, string, string, ...string) (string, error) { return "", nil },
		Now:          time.Now,
		Sleep:        sleepCtx,
		StopWait:     2 * time.Second,
		HealthWait:   2 * time.Second,
		ProbeWait:    50 * time.Millisecond,
		DiscoverWait: 2 * time.Second,
	}
}

// scriptVersion answers what a stand-in binary — a script that echoes its
// version — would, without running it: a machine whose every new file is
// assessed on its first run (macOS) spends most of a cell starting them, and
// what running one is like is the integration tests' to show.
func scriptVersion(_ context.Context, path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	_, after, ok := strings.Cut(string(b), "echo ")
	if !ok {
		return "", fmt.Errorf("%s is not a stand-in binary", path)
	}
	v, _, _ := strings.Cut(after, "\n")
	return strings.TrimSpace(v), nil
}

func fakeReleases(t *testing.T, mirror string) *Releases {
	r := testReleases(t, mirror)
	r.Version = scriptVersion
	return r
}
