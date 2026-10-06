package upgrade

import (
	"fmt"
	"path"
	"strings"
)

// A server a service manager runs is the person's, and the command does not
// touch it (spec 054 #29, which replaces #22 and #26 (D)): a manager restarts
// what the command stops, and the two then race for the data. The rule is
// one of membership, not of proof: a process whose own unit is a systemd
// service — system or user, under cgroup v1, v2 or both — or that carries a
// launchd job's label, is in a manager's hands, whether that unit started it
// directly, through a wrapper, or is a CI runner's agent that happened to
// start the shell. A process whose own unit is a session or a scope — a
// terminal, tmux, ssh, the `nohup … &` setup.md starts a server with — is
// not. What cannot be read cannot be ruled out, and is the person's too, and
// so is a process at a cgroup's root or in a slice: only a scope says no
// manager runs it.
// Nothing here asks the manager anything: the answer is in the process's
// own record.

// systemdUnit names the unit a process's cgroup puts it in, when that is a
// service: "the systemd unit X" or "the user systemd unit X"; or why one
// cannot be ruled out. Empty for a session's or a scope's process. The line
// read is the one that names the process's own unit: `name=systemd` under
// cgroup v1 or a hybrid hierarchy, else the unified `0::` line; a
// controller's line in v1 may stop at `user@N.service` and is not it.
func systemdUnit(cgroup string) string {
	if strings.TrimSpace(cgroup) == "" {
		return "its cgroup could not be read, so a service manager cannot be ruled out"
	}
	var unified, named string
	found := false
	for _, line := range strings.Split(strings.TrimSpace(cgroup), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		switch {
		case parts[1] == "name=systemd":
			named, found = parts[2], true
		case parts[0] == "0" && parts[1] == "":
			unified, found = parts[2], true
		}
	}
	if !found {
		return "its cgroup names no systemd hierarchy, so a service manager cannot be ruled out"
	}
	own := unified
	if named != "" {
		own = named
	}
	unit := path.Base(own)
	switch {
	case strings.HasSuffix(unit, ".scope"):
		// A session's or a terminal's: the one leaf that rules a manager
		// out (the audit of #223).
		return ""
	case !strings.HasSuffix(unit, ".service"):
		// The root — a cgroup namespace's, or an init that is not systemd's
		// (OpenRC, runit, s6) — or a slice: nothing says no manager runs it.
		return fmt.Sprintf("its cgroup (%s) is no session's or terminal's, so a service manager cannot be ruled out", own)
	}
	if strings.Contains(own, "/user@") && !strings.HasPrefix(unit, "user@") {
		return "the user systemd unit " + unit
	}
	return "the systemd unit " + unit
}

// launchdJob names the launchd job a process runs in: launchd sets
// XPC_SERVICE_NAME to a job's label; a terminal's processes carry `0`, an
// application's `application.…`.
func launchdJob(p Process) string {
	label := p.Getenv("XPC_SERVICE_NAME")
	if label == "" || label == "0" || strings.HasPrefix(label, "application.") {
		return ""
	}
	return "the launchd job " + label
}
