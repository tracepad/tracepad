//go:build darwin

package upgrade

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
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

func (kernSystem) Candidates(ctx context.Context) ([]Process, int, error) {
	table, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, 0, err
	}
	uid := uint32(os.Getuid())
	var pids []int
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
		// Its arguments first, which are cheap: `tracepad mcp`, `tail`,
		// `version` are no server, and are not asked lsof
		// about (the final review).
		if raw, err := unix.SysctlRaw("kern.procargs2", pid); err == nil {
			if _, argv, _, err := parseProcargs2(raw); err == nil {
				if _, ok := serverFlags(argv); !ok {
					continue
				}
			}
		}
		pids = append(pids, pid)
	}
	// One lsof for all of them, under the caller's deadline (the final
	// review): one each, in turn, took as long as there were servers.
	files := lsofFiles(ctx, pids)
	if err := ctx.Err(); err != nil {
		return nil, 0, fmt.Errorf("the processes were not all read in time: %w", err)
	}
	read := func(pid int) (Process, error) { return inspectWith(pid, files[pid]) }
	var procs []Process
	unread := 0
	for _, pid := range pids {
		p, err := readSettled(pid, read)
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

func (kernSystem) Inspect(pid int) (Process, error) { return inspectDarwin(pid) }
func (kernSystem) Alive(pid int) bool               { return alive(pid) }
func (kernSystem) Signal(pid int, sig syscall.Signal) error {
	return syscall.Kill(pid, sig)
}
func (kernSystem) Start(spec StartSpec) (Started, error) { return startDetached(spec) }

func inspectDarwin(pid int) (Process, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return inspectWith(pid, lsofFiles(ctx, []int{pid})[pid])
}

// inspectWith reads a process, with what lsof said of its working directory
// and output.
func inspectWith(pid int, f openFiles) (Process, error) {
	// The table first: another user's process is not ours, and its
	// arguments are not this user's to read (the tenth review).
	k, kerr := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if kerr == nil && k.Eproc.Pcred.P_ruid != uint32(os.Getuid()) {
		return Process{}, fmt.Errorf("pid %d: %w", pid, errNotMine)
	}
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return Process{}, fmt.Errorf("pid %d: its arguments cannot be read (another user's, or gone): %w", pid, err)
	}
	exe, argv, env, err := parseProcargs2(raw)
	if err != nil {
		return Process{}, fmt.Errorf("pid %d: %w", pid, err)
	}
	p := Process{PID: pid, Argv: argv, Env: env, Exe: exe}
	p.Cwd, p.Stdout = f.cwd, f.stdout
	if p.Exe != "" && !filepath.IsAbs(p.Exe) {
		if p.Cwd == "" {
			p.Exe = ""
		} else {
			p.Exe = filepath.Join(p.Cwd, p.Exe)
		}
	}
	if kerr == nil {
		p.PPID = int(k.Eproc.Ppid)
		p.Start = k.Proc.P_starttime.Sec*1_000_000 + int64(k.Proc.P_starttime.Usec)
	}
	p.Manager = launchdJob(p)
	return p, nil
}

// isZombie reads the process table's state: SZOMB is 5.
func isZombie(pid int) bool {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return err == nil && k.Proc.P_stat == 5
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

// openFiles is what lsof says of a process: its working directory, and
// descriptor 1 when that is a regular file. Either is empty when lsof cannot
// say.
type openFiles struct{ cwd, stdout string }

// lsofFiles asks lsof, once, for the working directory and descriptor 1 of
// each of pids.
func lsofFiles(ctx context.Context, pids []int) map[int]openFiles {
	files := map[int]openFiles{}
	if len(pids) == 0 {
		return files
	}
	list := make([]string, len(pids))
	for i, pid := range pids {
		list[i] = strconv.Itoa(pid)
	}
	// lsof exits 1 when one of them is gone; what it printed of the others
	// stands.
	out, _ := child(ctx, "lsof", "-a", "-p", strings.Join(list, ","), "-d", "cwd,1", "-Fpftn").Output() // ignored: what lsof could not say is empty, and the start refuses on it
	pid, fd, typ := 0, "", ""
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:]) // ignored: a number lsof printed; one that is not names no process asked
			fd, typ = "", ""
		case 'f':
			fd, typ = line[1:], ""
		case 't':
			typ = line[1:]
		case 'n':
			f := files[pid]
			switch {
			case fd == "cwd":
				f.cwd = line[1:]
			case fd == "1" && typ == "REG":
				f.stdout = line[1:]
			}
			files[pid] = f
		}
	}
	return files
}
