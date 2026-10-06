//go:build linux

package upgrade

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// procSystem reads /proc.
type procSystem struct{}

func newSystem() System { return procSystem{} }

func (procSystem) Candidates(ctx context.Context) ([]Process, int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, 0, err
	}
	self, _ := os.Readlink("/proc/self/ns/pid") // ignored: unread, no process is passed over as another namespace's: each stays a candidate
	uid := os.Getuid()
	var procs []Process
	unread := 0
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, 0, fmt.Errorf("the processes were not all read in time: %w", err)
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		dir := "/proc/" + e.Name()
		owner, ok := procOwner(dir)
		if !ok {
			// One whose owner cannot be told may be this user's, and counts
			// as unread (the audit of #223).
			if alive(pid) {
				unread++
			}
			continue
		}
		if owner != uid {
			continue
		}
		// Another PID namespace is a container's process: the container's.
		if ns, err := os.Readlink(dir + "/ns/pid"); err == nil && self != "" && ns != self {
			continue
		}
		exe, exeErr := readExe(dir)
		argv, argErr := readNulList(dir + "/cmdline")
		if exeErr != nil && argErr != nil {
			// Read once more after a moment: one on its way out is gone, and
			// one that reads now is looked at as any other.
			p, err := readSettled(pid, inspectLinux)
			switch {
			case errors.Is(err, errNotMine), errors.Is(err, errGone):
				continue
			case err != nil:
				unread++
				continue
			}
			exe, argv, exeErr, argErr = p.Exe, p.Argv, nil, nil
		}
		if !isTracepadName(exe) && (len(argv) == 0 || !isTracepadName(argv[0])) {
			continue
		}
		// Not a server (`tracepad mcp`, `tail`): its cgroup is not read.
		if _, ok := serverFlags(argv); argErr == nil && !ok {
			continue
		}
		p, err := readSettled(pid, inspectLinux)
		switch {
		case errors.Is(err, errNotMine), errors.Is(err, errGone):
			continue
		case err != nil:
			unread++
			continue
		}
		procs = append(procs, p)
	}
	return procs, unread, nil
}

func (procSystem) Inspect(pid int) (Process, error) { return inspectLinux(pid) }
func (procSystem) Alive(pid int) bool               { return alive(pid) }
func (procSystem) Signal(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}
func (procSystem) Start(spec StartSpec) (Started, error) { return startDetached(spec) }

func inspectLinux(pid int) (Process, error) {
	dir := "/proc/" + strconv.Itoa(pid)
	owner, ok := procOwner(dir)
	switch {
	case !ok:
		return Process{}, fmt.Errorf("pid %d: its owner cannot be read, or it is gone", pid)
	case owner != os.Getuid():
		return Process{}, fmt.Errorf("pid %d: %w", pid, errNotMine)
	}
	exe, err := readExe(dir)
	if err != nil {
		return Process{}, fmt.Errorf("pid %d: its executable cannot be read: %w", pid, err)
	}
	argv, err := readNulList(dir + "/cmdline")
	if err != nil || len(argv) == 0 {
		return Process{}, fmt.Errorf("pid %d: its arguments cannot be read", pid)
	}
	env, err := readNulList(dir + "/environ")
	if err != nil {
		return Process{}, fmt.Errorf("pid %d: its environment cannot be read: %w", pid, err)
	}
	p := Process{PID: pid, Argv: argv, Env: env, Exe: exe}
	// A working directory removed since reads as "<dir> (deleted)", as an
	// executable does (the third review).
	p.Cwd, _ = os.Readlink(dir + "/cwd") // ignored: unread, no working directory: the start falls back, or refuses (prepareProcess)
	p.Cwd = strings.TrimSuffix(p.Cwd, " (deleted)")
	if out, err := os.Readlink(dir + "/fd/1"); err == nil && strings.HasPrefix(out, "/") {
		if st, err := os.Stat(out); err == nil && st.Mode().IsRegular() {
			p.Stdout = out
		}
	}
	p.PPID = procPPID(dir)
	p.Start = procStart(dir)
	p.Manager = systemdUnit(cgroupOf(dir))
	return p, nil
}

// readExe is /proc/<pid>/exe, without the " (deleted)" the kernel adds when
// the file was replaced since: it is still the path it ran from.
func readExe(dir string) (string, error) {
	exe, err := os.Readlink(dir + "/exe")
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(exe, " (deleted)"), nil
}

func readNulList(file string) ([]string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	b = bytes.TrimSuffix(b, []byte{0})
	if len(b) == 0 {
		return nil, nil
	}
	parts := bytes.Split(b, []byte{0})
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = string(p)
	}
	return out, nil
}

// procPPID is the parent's PID in /proc/<pid>/status.
func procPPID(dir string) int {
	b, err := os.ReadFile(dir + "/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "PPid:"); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
	return 0
}

// procUID is the real UID in /proc/<pid>/status.
func procUID(dir string) (int, bool) {
	f, err := os.Open(dir + "/status")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if rest, ok := strings.CutPrefix(s.Text(), "Uid:"); ok {
			fields := strings.Fields(rest)
			if len(fields) == 0 {
				return 0, false
			}
			uid, err := strconv.Atoi(fields[0])
			return uid, err == nil
		}
	}
	return 0, false
}

// isZombie reads the state in /proc/<pid>/stat: the field after the
// command's closing parenthesis.
func isZombie(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 || i+2 >= len(b) {
		return false
	}
	return b[i+2] == 'Z'
}

// cgroupOf is /proc/<pid>/cgroup; a test gives a runner's.
var cgroupOf = func(dir string) string {
	b, _ := os.ReadFile(dir + "/cgroup") // ignored: unread, a manager cannot be ruled out (systemdUnit), and the server is the person's
	return string(b)
}

// procOwner is a process's owner: its status's, else — its status unread,
// under hidepid or in a race — its directory's.
func procOwner(dir string) (int, bool) {
	if owner, ok := procUID(dir); ok {
		return owner, true
	}
	if info, err := os.Stat(dir); err == nil {
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			return int(st.Uid), true
		}
	}
	return 0, false
}

// procStart is the process's start time, /proc/<pid>/stat's 22nd field, in
// clock ticks since the boot; 0 when it cannot be read.
func procStart(dir string) int64 {
	b, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return 0
	}
	// The command's name, in parentheses, may hold spaces: the fields are
	// counted after its closing one, where the 3rd field begins.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0
	}
	fields := strings.Fields(string(b[i+1:]))
	if len(fields) < 20 {
		return 0
	}
	n, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0
	}
	return n
}
