package upgrade

import (
	"bufio"
	"context"
	"fmt"
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

// jobPID reads a launchd job's PID and its program's arguments (`launchctl
// print`), in this user's GUI domain or its user domain; PID 0 when it runs
// nothing.
var jobPID = func(uid int, label string) (pid int, arguments string, err error) {
	var last error
	for _, domain := range []string{"gui/", "user/"} {
		out, err := runQuiet("launchctl", "print", domain+strconv.Itoa(uid)+"/"+label)
		if err != nil {
			last = err
			continue
		}
		var args []string
		inArgs := false
		s := bufio.NewScanner(strings.NewReader(out))
		for s.Scan() {
			line := strings.TrimSpace(s.Text())
			switch {
			case inArgs && line == "}":
				inArgs = false
			case inArgs:
				args = append(args, line)
			case line == "arguments = {":
				inArgs = true
			case strings.HasPrefix(line, "program = "):
				args = append(args, strings.TrimPrefix(line, "program = "))
			case strings.HasPrefix(line, "pid = "):
				pid, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "pid = ")))
			}
		}
		return pid, strings.Join(args, " "), nil
	}
	return 0, "", last
}

func runQuiet(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// systemdManager decides, from p's /proc/<pid>/cgroup, which unit runs p:
// managed names it; unasked says why the unit p sits in could not be asked,
// which makes p the person's all the same.
func systemdManager(p Process, cgroup string) (managed, unasked string) {
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
			return "", fmt.Sprintf("it sits in the systemd unit %s, which could not be asked whether it runs it (%v): restart it yourself, or with sudo systemctl restart %s if the unit is what runs it", unit, firstLine(err.Error()), unit)
		}
		_, argv, _ := strings.Cut(execStart, "argv[]=")
		argv, _, _ = strings.Cut(argv, " ;")
		if mainPID == p.PID || startsThis(execStart, p) || (mainPID > 0 && mainPID == p.PPID && namesTracepad(argv)) {
			return "the systemd unit " + unit, ""
		}
		return "", ""
	}
	return "", ""
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

// namesTracepad says whether a service's command runs the binary through a
// wrapper — `sh -c 'tracepad serve …'` without exec, whose PID is the
// server's parent's: a word of it is a path to a `tracepad`.
func namesTracepad(command string) bool {
	for _, word := range strings.FieldsFunc(command, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\'' || r == '"' || r == ';' || r == '&' || r == '|'
	}) {
		if isTracepadName(word) {
			return true
		}
	}
	return false
}

// launchdManager decides which launchd job runs p: the job its
// XPC_SERVICE_NAME names, when that job's PID is p's, or p's parent's and
// the job's program runs a tracepad. A job that cannot be asked makes p the
// person's.
func launchdManager(p Process, uid int) (managed, unasked string) {
	label := p.Getenv("XPC_SERVICE_NAME")
	if label == "" || label == "0" || strings.HasPrefix(label, "application.") {
		return "", ""
	}
	pid, arguments, err := jobPID(uid, label)
	if err != nil {
		return "", fmt.Sprintf("it sits in the launchd job %s, which could not be asked whether it runs it (%v): restart it yourself, or with launchctl kickstart -k gui/%d/%s if the job is what runs it", label, firstLine(err.Error()), uid, label)
	}
	if pid > 0 && (pid == p.PID || pid == p.PPID && namesTracepad(arguments)) {
		return "the launchd job " + label, ""
	}
	return "", ""
}
