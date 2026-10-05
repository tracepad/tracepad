//go:build unix

package upgrade

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// slowSystem is a server that was asked to stop and is still on its way out
// for a few looks, and a start that answers at once.
type slowSystem struct {
	t         *testing.T
	old       Process
	looksLeft int
	events    []string
}

func (s *slowSystem) Candidates() ([]Process, int, error) { return nil, 0, nil }
func (s *slowSystem) Inspect(pid int) (Process, error) {
	if pid == s.old.PID && s.looksLeft > 0 {
		return s.old, nil
	}
	return Process{}, fmt.Errorf("pid %d is gone", pid)
}
func (s *slowSystem) Alive(pid int) bool {
	if pid != s.old.PID || s.looksLeft <= 0 {
		return false
	}
	s.looksLeft--
	if s.looksLeft == 0 {
		s.events = append(s.events, "old gone")
	}
	return true
}
func (s *slowSystem) Signal(int, syscall.Signal) error { return nil }
func (s *slowSystem) Start(spec StartSpec) (Started, error) {
	if s.looksLeft > 0 {
		s.events = append(s.events, "started while the old one still ran")
	} else {
		s.events = append(s.events, "started")
	}
	done := make(chan struct{})
	return &started{pid: 999999, done: done}, nil
}

// The stop timed out, and the way back runs while the old server is still
// shutting down: it waits for it to go, then starts the old version and
// checks it, and is done only then (the second review).
func TestAWayBackWaitsForAServerStillShuttingDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"ok","version":"0.1.0"}`)
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	listen := "127.0.0.1:" + port

	root, _ := filepath.EvalSymlinks(t.TempDir())
	dir := filepath.Join(root, "backups", "20261006-120000-0.1.0-abc123")
	data := filepath.Join(root, "data")
	install := filepath.Join(root, "bin", "tracepad")
	for _, d := range []string{dir, data, filepath.Dir(install)} {
		_ = os.MkdirAll(d, 0o700)
	}
	scriptBinary(t, install, "0.1.0")
	scriptBinary(t, filepath.Join(dir, "tracepad-0.1.0"), "0.1.0")
	argv := []string{"tracepad", "serve", "--listen", listen, "--data-dir", data}
	_ = os.WriteFile(filepath.Join(data, "tracepad.db.lock"), []byte("4242\n"), 0o600)
	st := &State{Run: filepath.Base(dir), Kind: kindProcess, From: "0.1.0", To: "0.2.0",
		Binary:  &BinaryState{Path: install, From: "0.1.0"},
		Process: &ProcessState{PID: 4242, DataDir: data, Listen: listen, URL: "http://" + listen, Log: filepath.Join(data, "server.log"), Old: filepath.Join(dir, "tracepad-0.1.0")},
		Steps:   []Step{{Name: stepPrepared}, {Name: stepStopSent}}}
	if err := st.save(dir); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "server.json"), ServerSpec{Exe: install, Argv: argv, Dir: root}); err != nil {
		t.Fatal(err)
	}
	sys := &slowSystem{t: t, old: Process{PID: 4242, Exe: install, Argv: argv}, looksLeft: 4}
	now := time.Unix(0, 0)
	deps := Deps{Sys: sys, HTTP: srv.Client(), Backups: filepath.Dir(dir), Getenv: func(string) string { return "" },
		Version: binaryVersion, Now: func() time.Time { return now },
		Sleep:    func(context.Context, time.Duration) error { now = now.Add(time.Second); return nil },
		StopWait: time.Minute, HealthWait: 3 * time.Second}
	rep, code := runReport(t, deps, "--back", filepath.Base(dir))
	if code != exitOK || rep.BackCheck == nil || rep.BackCheck.Verdict != verdictHealthy {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	if strings.Join(sys.events, ", ") != "old gone, started" {
		t.Errorf("events: %q", sys.events)
	}
	if got, _ := loadState(dir); got == nil || !got.has(stepBackDone) || got.Process.BackPID != 999999 {
		t.Errorf("state: %+v", got)
	}
}
