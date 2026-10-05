package upgrade

import (
	"bufio"
	"context"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
)

// A service manager runs a server when the service itself says so, not when
// the server merely sits inside one (spec 054 #22): on a CI runner every
// process is in the agent's unit, under tmux a shell is in a user unit, and a
// server a person started from such a shell is theirs to stop and start like
// any other. So the unit a process's cgroup names, or the launchd job its
// environment names, is asked: it manages the process when its main PID is
// the process's, or when what it starts is this binary with these arguments.
// A unit that cannot be asked proves nothing.

// unitShow reads a systemd unit's MainPID and ExecStart (`systemctl show`):
// the user manager's for a unit under `user@N.service`.
var unitShow = func(user bool, unit string) (mainPID int, execStart string, err error) {
	args := []string{"show", "--property=MainPID", "--property=ExecStart", "--", unit}
	if user {
		args = append([]string{"--user"}, args...)
	}
	out, err := runQuiet("systemctl", args...)
	if err != nil {
		return 0, "", err
	}
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(line, "=")
		switch k {
		case "MainPID":
			mainPID, _ = strconv.Atoi(strings.TrimSpace(v))
		case "ExecStart":
			execStart = v
		}
	}
	return mainPID, execStart, nil
}

// jobPID reads a launchd job's PID (`launchctl print`), in this user's GUI
// domain or its user domain; 0 when it runs nothing.
var jobPID = func(uid int, label string) (int, error) {
	var last error
	for _, domain := range []string{"gui/", "user/"} {
		out, err := runQuiet("launchctl", "print", domain+strconv.Itoa(uid)+"/"+label)
		if err != nil {
			last = err
			continue
		}
		s := bufio.NewScanner(strings.NewReader(out))
		for s.Scan() {
			if v, ok := strings.CutPrefix(strings.TrimSpace(s.Text()), "pid = "); ok {
				return strconv.Atoi(strings.TrimSpace(v))
			}
		}
		return 0, nil
	}
	return 0, last
}

func runQuiet(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// systemdManager names the unit that runs p, given its /proc/<pid>/cgroup:
// empty when no unit can be shown to run it.
func systemdManager(p Process, cgroup string) string {
	for _, line := range strings.Split(strings.TrimSpace(cgroup), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		unit := path.Base(parts[2])
		if !strings.HasSuffix(unit, ".service") {
			continue
		}
		user := strings.Contains(parts[2], "/user@") && !strings.HasPrefix(unit, "user@")
		mainPID, execStart, err := unitShow(user, unit)
		if err != nil {
			return ""
		}
		if mainPID == p.PID || startsThis(execStart, p) {
			return "the systemd unit " + unit
		}
		return ""
	}
	return ""
}

// startsThis says whether a unit's ExecStart starts this binary with this
// process's arguments: systemd writes it as `{ path=… ; argv[]=… ; … }`.
func startsThis(execStart string, p Process) bool {
	_, argv, ok := strings.Cut(execStart, "argv[]=")
	if !ok || p.Exe == "" {
		return false
	}
	argv, _, _ = strings.Cut(argv, " ;")
	words := strings.Fields(argv)
	if len(words) == 0 || !sameFile(words[0], p.Exe) {
		return false
	}
	return strings.Join(words[1:], " ") == strings.Join(p.Argv[1:], " ")
}

// launchdManager names the launchd job that runs p: the job its
// XPC_SERVICE_NAME names, when that job's PID is p's.
func launchdManager(p Process, uid int) string {
	label := p.Getenv("XPC_SERVICE_NAME")
	if label == "" || label == "0" || strings.HasPrefix(label, "application.") {
		return ""
	}
	pid, err := jobPID(uid, label)
	if err != nil || pid != p.PID {
		return ""
	}
	return "the launchd job " + label
}
