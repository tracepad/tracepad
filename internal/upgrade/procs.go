package upgrade

import (
	"errors"
	"path/filepath"
	"strings"
	"syscall"
)

// Process is what the machine says about a running process of this user.
type Process struct {
	PID  int
	Argv []string
	// Env is the environment the process was started with.
	Env []string
	// Exe is the executable's path: /proc/<pid>/exe on Linux, the path it was
	// executed from on macOS. Empty when it could not be read.
	Exe string
	// Cwd is its working directory, empty when it could not be read.
	Cwd string
	// Stdout is the regular file its output goes to, or empty (a terminal, a
	// pipe, /dev/null, or not known).
	Stdout string
	// PPID is its parent's.
	PPID int
	// Start is when it started, in the system's own units: with its PID, it
	// names one process, which a PID alone does not once the PID is reused
	// (spec 054 #40). 0 when it could not be read.
	Start int64
	// Manager names the service manager it runs under — "the systemd unit
	// tracepad.service", "the user systemd unit X", "the launchd job X" — or
	// says why one cannot be ruled out ("its cgroup …"). Such a server is
	// the person's (#29).
	Manager string
}

// Getenv reads the process's environment.
func (p Process) Getenv(key string) string {
	for _, kv := range p.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// StartSpec is how a server is started again: its own arguments, environment
// and working directory, with its output appended to Log.
type StartSpec struct {
	Path string
	Argv []string
	Env  []string
	Dir  string
	Log  string
}

// Started is a server this command started.
type Started interface {
	PID() int
	// Exited is closed when the process has exited.
	Exited() <-chan struct{}
}

// System is the machine's processes, as the command needs them. The real one
// is /proc or the kernel's process table; tests give a fake.
type System interface {
	// Candidates lists this user's processes, in this PID namespace, whose
	// executable or first argument is named `tracepad`. unread counts the
	// ones that could not be read, which makes "none found" a weaker claim.
	Candidates() (procs []Process, unread int, err error)
	// Inspect reads one process; an error when it is gone or not readable.
	Inspect(pid int) (Process, error)
	Alive(pid int) bool
	Signal(pid int, sig syscall.Signal) error
	Start(spec StartSpec) (Started, error)
}

// errNotMine is a process of another user's: never one this command started
// or may touch, so never "cannot be read" either (the tenth review).
var errNotMine = errors.New("another user's process")

// isTracepadName says whether a path's last element is the binary's name.
func isTracepadName(path string) bool { return filepath.Base(path) == "tracepad" }
