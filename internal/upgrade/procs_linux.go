//go:build linux

package upgrade

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// procSystem reads /proc.
type procSystem struct{}

func newSystem() System { return procSystem{} }

func (procSystem) Candidates() ([]Process, int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, 0, err
	}
	self, _ := os.Readlink("/proc/self/ns/pid")
	uid := os.Getuid()
	var procs []Process
	unread := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		dir := "/proc/" + e.Name()
		if owner, ok := procUID(dir); !ok || owner != uid {
			continue
		}
		// Another PID namespace is a container's process: the container's.
		if ns, err := os.Readlink(dir + "/ns/pid"); err == nil && self != "" && ns != self {
			continue
		}
		exe, exeErr := readExe(dir)
		argv, argErr := readNulList(dir + "/cmdline")
		if exeErr != nil && argErr != nil {
			if alive(pid) {
				unread++
			}
			continue
		}
		if !isTracepadName(exe) && (len(argv) == 0 || !isTracepadName(argv[0])) {
			continue
		}
		p, err := inspectLinux(pid)
		if err != nil {
			if alive(pid) {
				unread++
			}
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
	if owner, ok := procUID(dir); !ok || owner != os.Getuid() {
		return Process{}, fmt.Errorf("pid %d is not this user's, or is gone", pid)
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
	p.Cwd, _ = os.Readlink(dir + "/cwd")
	p.Cwd = strings.TrimSuffix(p.Cwd, " (deleted)")
	if out, err := os.Readlink(dir + "/fd/1"); err == nil && strings.HasPrefix(out, "/") {
		if st, err := os.Stat(out); err == nil && st.Mode().IsRegular() {
			p.Stdout = out
		}
	}
	p.PPID = procPPID(dir)
	p.Manager, p.Unasked = systemdManager(p, cgroupOf(dir))
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

// cgroupOf is /proc/<pid>/cgroup; a test gives a runner's.
var cgroupOf = func(dir string) string {
	b, _ := os.ReadFile(dir + "/cgroup")
	return string(b)
}
