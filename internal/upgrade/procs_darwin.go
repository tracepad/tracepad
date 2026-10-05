//go:build darwin

package upgrade

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// kernSystem reads the kernel's process table, and `kern.procargs2` for a
// process's arguments, environment and the path it was executed from. That
// sysctl answers for this user's processes only. The working directory and
// where the output goes are `lsof`'s, which macOS ships.
type kernSystem struct{}

func newSystem() System { return kernSystem{} }

func (kernSystem) Candidates() ([]Process, int, error) {
	table, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, 0, err
	}
	uid := uint32(os.Getuid())
	var procs []Process
	unread := 0
	for _, k := range table {
		pid := int(k.Proc.P_pid)
		if pid <= 0 || pid == os.Getpid() || k.Eproc.Pcred.P_ruid != uid {
			continue
		}
		comm := k.Proc.P_comm[:]
		if i := bytes.IndexByte(comm, 0); i >= 0 {
			// What follows the terminator is whatever the buffer held.
			comm = comm[:i]
		}
		if string(comm) != "tracepad" {
			// The table's name is the executed file's, cut at sixteen bytes;
			// the arguments are read only for a process it could be.
			continue
		}
		p, err := inspectDarwin(pid)
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

func (kernSystem) Inspect(pid int) (Process, error) { return inspectDarwin(pid) }
func (kernSystem) Alive(pid int) bool               { return alive(pid) }
func (kernSystem) Signal(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}
func (kernSystem) Start(spec StartSpec) (Started, error) { return startDetached(spec) }

func inspectDarwin(pid int) (Process, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return Process{}, fmt.Errorf("pid %d: its arguments cannot be read (another user's, or gone): %w", pid, err)
	}
	exe, argv, env, err := parseProcargs2(raw)
	if err != nil {
		return Process{}, fmt.Errorf("pid %d: %w", pid, err)
	}
	p := Process{PID: pid, Argv: argv, Env: env, Exe: exe}
	p.Cwd, p.Stdout = lsofCwdStdout(pid)
	if p.Exe != "" && !filepath.IsAbs(p.Exe) {
		if p.Cwd == "" {
			p.Exe = ""
		} else {
			p.Exe = filepath.Join(p.Cwd, p.Exe)
		}
	}
	p.Manager = launchdManager(p, os.Getuid())
	return p, nil
}

// parseProcargs2 reads what `kern.procargs2` returns: argc as a 32-bit
// integer, the executed path, padding NULs, then argc arguments and the
// environment, each NUL-terminated.
func parseProcargs2(raw []byte) (exe string, argv, env []string, err error) {
	if len(raw) < 4 {
		return "", nil, nil, errors.New("its arguments are empty")
	}
	argc := int(binary.LittleEndian.Uint32(raw[:4]))
	rest := raw[4:]
	end := bytes.IndexByte(rest, 0)
	if end < 0 {
		return "", nil, nil, errors.New("its arguments are malformed")
	}
	exe = string(rest[:end])
	rest = bytes.TrimLeft(rest[end:], "\x00")
	for len(rest) > 0 {
		end := bytes.IndexByte(rest, 0)
		if end < 0 {
			end = len(rest)
		}
		item := string(rest[:end])
		if len(argv) < argc {
			argv = append(argv, item)
		} else if item == "" {
			// The environment ends at an empty string; what follows is the
			// kernel's own (the apple strings).
			break
		} else {
			env = append(env, item)
		}
		if end == len(rest) {
			break
		}
		rest = rest[end+1:]
	}
	if len(argv) != argc || argc == 0 {
		return "", nil, nil, errors.New("its arguments are malformed")
	}
	return exe, argv, env, nil
}

// lsofCwdStdout asks lsof for the working directory and descriptor 1. Either
// is empty when lsof cannot say; descriptor 1 counts only as a regular file.
func lsofCwdStdout(pid int) (cwd, stdout string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd,1", "-Fftn").Output()
	if err != nil && len(out) == 0 {
		return "", ""
	}
	var fd, typ string
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'f':
			fd, typ = line[1:], ""
		case 't':
			typ = line[1:]
		case 'n':
			switch {
			case fd == "cwd":
				cwd = line[1:]
			case fd == "1" && typ == "REG":
				stdout = line[1:]
			}
		}
	}
	return cwd, stdout
}
