package crashtest

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The two projects the server declares. `main` is the one whose guarantees
// are asserted: nothing deletes its traces but the test's own requests.
// `ret` carries a retention window the test backdates its rows past, so that
// the sweeper has work to be killed in the middle of.
const (
	mainPublic = "tp-pk-crash-main"
	mainSecret = "tp-sk-crash-main-00000000000000000000"
	retPublic  = "tp-pk-crash-ret"
	retSecret  = "tp-sk-crash-ret-000000000000000000000"
	adminToken = "tp-admin-crash-0000000000000000000000000"
)

// server is one process of the binary on the test's data directory.
type server struct {
	t       *testing.T
	cmd     *exec.Cmd
	host    string
	log     *os.File
	exited  chan error
	started time.Time
	// killed is set once the process has been sent SIGKILL and reaped.
	killed bool
}

// startServer launches `serve` on dir and returns without waiting for it to
// answer; a caller that wants it healthy calls waitHealthy, and one that
// wants to kill it in the middle of its start does not.
func startServer(t *testing.T, binary, dir, logPath string) *server {
	t.Helper()
	port, err := freePort()
	if err != nil {
		t.Fatalf("a free port: %v", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("server log: %v", err)
	}
	cmd := exec.Command(binary, "serve")
	cmd.Env = append(cleanEnv(),
		"TRACEPAD_DATA_DIR="+dir,
		fmt.Sprintf("TRACEPAD_LISTEN=127.0.0.1:%d", port),
		"TRACEPAD_PROJECTS=main:"+mainPublic+":"+mainSecret+",ret:"+retPublic+":"+retSecret,
		"TRACEPAD_ADMIN_TOKEN="+adminToken,
		// The two background workers whose transactions the kill should
		// land in the middle of, at their shortest cadence.
		"TRACEPAD_SWEEP_INTERVAL=1s",
		"TRACEPAD_ROLLUP_INTERVAL=1s",
	)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	fmt.Fprintf(logFile, "\n=== start %s ===\n", time.Now().Format(time.RFC3339Nano))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the server: %v", err)
	}
	s := &server{
		t: t, cmd: cmd, log: logFile, host: fmt.Sprintf("http://127.0.0.1:%d", port),
		exited: make(chan error, 1), started: time.Now(),
	}
	go func() { s.exited <- cmd.Wait() }()
	return s
}

// cleanEnv is the test process's environment without any TRACEPAD_ variable,
// so that nothing of the developer's own configuration reaches the server.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TRACEPAD_") {
			env = append(env, kv)
		}
	}
	return env
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitHealthy polls /health until the server answers, and fails if the
// process exits first or does not answer within the limit — a restart after
// a kill that does not come up is the first thing this test exists to see.
func (s *server) waitHealthy(limit time.Duration) {
	s.t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		select {
		case err := <-s.exited:
			s.exited <- err
			s.t.Fatalf("the server exited before it answered (%v):\n%s", err, tail(s.log.Name(), 4000))
		default:
		}
		resp, err := http.Get(s.host + "/health")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	s.t.Fatalf("the server did not answer /health within %s:\n%s", limit, tail(s.log.Name(), 4000))
}

// kill sends SIGKILL to this server's own process and reaps it. Never a
// pattern: the stand of the owner and the servers of other sessions are
// running on this machine.
func (s *server) kill() {
	s.t.Helper()
	if s.killed {
		return
	}
	if err := s.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		s.t.Fatalf("SIGKILL pid %d: %v", s.cmd.Process.Pid, err)
	}
	<-s.exited
	s.killed = true
	s.log.Close()
}

// stop ends the server the way an operator does, with SIGTERM, and reports
// how it left.
func (s *server) stop() error {
	s.t.Helper()
	if s.killed {
		return nil
	}
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	select {
	case err := <-s.exited:
		s.killed = true
		s.log.Close()
		return err
	case <-time.After(20 * time.Second):
		s.kill()
		return fmt.Errorf("the server did not stop within 20s of SIGTERM")
	}
}

// snapshot copies the database's files, WAL and shared-memory file included,
// into a fresh directory: the image of what the kill left, which can then be
// opened, recovered and read without touching what the next start will meet.
func snapshot(t *testing.T, dataDir, into string) string {
	t.Helper()
	if err := os.MkdirAll(into, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tracepad.db", "tracepad.db-wal", "tracepad.db-shm"} {
		data, err := os.ReadFile(filepath.Join(dataDir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("snapshot %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(into, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(into, "tracepad.db")
}

// tail is the last n bytes of a file, for a failure message.
func tail(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return "(no log: " + err.Error() + ")"
	}
	if len(data) > n {
		data = append([]byte("…"), data[len(data)-n:]...)
	}
	return string(bytes.TrimSpace(data))
}
