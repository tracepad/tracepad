package upgrade

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// listed is a machine whose processes are given.
type listed struct{ procs []Process }

func (l listed) Candidates(context.Context) ([]Process, int, error) { return l.procs, 0, nil }
func (listed) Inspect(int) (Process, error)                         { return Process{}, os.ErrNotExist }
func (listed) Alive(int) bool                                       { return false }
func (listed) Signal(int, syscall.Signal) error                     { return nil }
func (listed) Start(StartSpec) (Started, error)                     { return nil, os.ErrInvalid }

// Servers that take a connection and never answer are asked side by side,
// under one deadline: three of them must not add up past it (the fourth
// review: the install script stops waiting at fifteen seconds).
func TestTheLookAtTheMachineKeepsItsDeadline(t *testing.T) {
	t.Parallel()
	var procs []Process
	for i := 0; i < 3; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				defer c.Close()
			}
		}()
		port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
		procs = append(procs, Process{PID: 100 + i, Exe: "/x/tracepad",
			Argv: []string{"tracepad", "serve", "--listen", "127.0.0.1:" + port, "--data-dir", filepath.Join(t.TempDir(), "d")}})
	}
	r := &runner{deps: Deps{Sys: listed{procs}, HTTP: &http.Client{}, InstallDir: t.TempDir(),
		Version: binaryVersion, LookPath: func(string) string { return "" }, Getenv: func(string) string { return "" },
		ProbeWait: 50 * time.Millisecond, DiscoverWait: 300 * time.Millisecond}}
	started := time.Now()
	f := r.discover(context.Background())
	if took := time.Since(started); took > 800*time.Millisecond {
		t.Errorf("the look took %s for three silent servers", took)
	}
	if len(f.Servers) != 3 {
		t.Errorf("servers: %d", len(f.Servers))
	}
}
