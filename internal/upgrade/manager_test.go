package upgrade

import (
	"strings"
	"testing"
)

// A process whose own unit is a service is a manager's, whichever service,
// and the command leaves it (spec 054 #29); a session's or a scope's is not.
func TestAServiceManagersProcessIsThePersons(t *testing.T) {
	for _, tc := range []struct {
		name, cgroup, managed string
	}{
		{"a CI runner's agent, which started the shell", "0::/system.slice/hosted-compute-agent.service\n", "the systemd unit hosted-compute-agent.service"},
		{"a system service", "0::/system.slice/tracepad.service\n", "the systemd unit tracepad.service"},
		{"cron", "0::/system.slice/cron.service\n", "the systemd unit cron.service"},
		{"a user service", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/tracepad.service\n", "the user systemd unit tracepad.service"},
		{"a user service under cgroup v1, its controller lines cut at the manager",
			"12:cpu,cpuacct:/user.slice/user-1000.slice/user@1000.service\n11:memory:/user.slice/user-1000.slice/user@1000.service\n1:name=systemd:/user.slice/user-1000.slice/user@1000.service/app.slice/tracepad.service\n",
			"the user systemd unit tracepad.service"},
		{"a hybrid hierarchy", "1:name=systemd:/system.slice/obs.service\n0::/system.slice/obs.service\n", "the systemd unit obs.service"},
		{"a hybrid hierarchy whose unified line is its root", "1:name=systemd:/system.slice/obs.service\n0::/\n", "the systemd unit obs.service"},
		{"a terminal's session", "0::/user.slice/user-1000.slice/session-3.scope\n", ""},
		{"tmux under a user manager", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/tmux-spawn-1.scope\n", ""},
		{"a desktop terminal", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-gnome-terminal-7.scope\n", ""},
		{"a container's root, no systemd", "0::/\n", ""},
		{"a v1 session", "4:memory:/user.slice\n1:name=systemd:/user.slice/user-1000.slice/session-2.scope\n", ""},
	} {
		if got := systemdUnit(tc.cgroup); got != tc.managed {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.managed)
		}
	}
	for _, cgroup := range []string{"", "4:memory:/user.slice\n"} {
		if got := systemdUnit(cgroup); !strings.HasPrefix(got, "its cgroup") {
			t.Errorf("%q: %q, want it not ruled out", cgroup, got)
		}
	}

	for label, managed := range map[string]bool{
		"dev.tracepad.server":                 true,
		"com.example.agent":                   true,
		"0":                                   false,
		"application.com.apple.Terminal.1234": false,
		"":                                    false,
	} {
		p := Process{Env: []string{"XPC_SERVICE_NAME=" + label}}
		if got := launchdJob(p); (got != "") != managed {
			t.Errorf("launchd %q: %q", label, got)
		}
	}

	// And classifying says so: the reason, and the manager's own command.
	s := Server{Proc: Process{Manager: "the user systemd unit tracepad.service"}}
	if advice := serverAdvice(s); !strings.Contains(advice, "systemctl --user restart tracepad.service") {
		t.Errorf("advice: %s", advice)
	}
}
