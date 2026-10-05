package upgrade

import (
	"errors"
	"testing"
)

func TestOnlyTheUnitItselfProvesItRunsTheServer(t *testing.T) {
	p := Process{PID: 4242, Exe: "/home/u/.local/bin/tracepad", Argv: []string{"tracepad", "serve", "--data-dir", "/home/u/data"}}
	type shown struct {
		pid  int
		exec string
		err  error
	}
	var asked []string
	show := func(s shown) func(bool, string) (int, string, error) {
		return func(user bool, unit string) (int, string, error) {
			if user {
				unit = "--user " + unit
			}
			asked = append(asked, unit)
			return s.pid, s.exec, s.err
		}
	}
	saved := unitShow
	t.Cleanup(func() { unitShow = saved })

	for _, tc := range []struct {
		name, cgroup string
		shown        shown
		managed      bool
		asked        string
	}{
		{"a CI runner's agent, which started the shell", "0::/system.slice/hosted-compute-agent.service\n",
			shown{pid: 812, exec: "{ path=/opt/runner/agent ; argv[]=/opt/runner/agent --run ; ignore_errors=no }"}, false, "hosted-compute-agent.service"},
		{"cron, which started the shell", "0::/system.slice/cron.service\n", shown{pid: 600}, false, "cron.service"},
		{"the unit whose main PID it is", "0::/system.slice/tracepad.service\n", shown{pid: 4242}, true, "tracepad.service"},
		{"a unit that starts this binary so", "0::/system.slice/tracepad.service\n",
			shown{pid: 4100, exec: "{ path=/home/u/.local/bin/tracepad ; argv[]=/home/u/.local/bin/tracepad serve --data-dir /home/u/data ; ignore_errors=no }"}, true, "tracepad.service"},
		{"a unit that starts it with another directory", "0::/system.slice/tracepad.service\n",
			shown{pid: 4100, exec: "{ path=/home/u/.local/bin/tracepad ; argv[]=/home/u/.local/bin/tracepad serve --data-dir /var/lib/tp ; ignore_errors=no }"}, false, "tracepad.service"},
		{"a user unit", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/tracepad.service\n", shown{pid: 4242}, true, "--user tracepad.service"},
		{"a unit systemctl cannot show", "0::/system.slice/tracepad.service\n", shown{err: errors.New("no systemd")}, false, "tracepad.service"},
		{"tmux under a user manager", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/tmux-spawn-1.scope\n", shown{pid: 4242}, false, ""},
		{"a terminal's session", "0::/user.slice/user-1000.slice/session-3.scope\n", shown{pid: 4242}, false, ""},
	} {
		asked = nil
		unitShow = show(tc.shown)
		got := systemdManager(p, tc.cgroup)
		if (got != "") != tc.managed {
			t.Errorf("%s: managed = %q", tc.name, got)
		}
		if tc.asked == "" && len(asked) > 0 || tc.asked != "" && (len(asked) != 1 || asked[0] != tc.asked) {
			t.Errorf("%s: asked %q", tc.name, asked)
		}
	}

	savedJob := jobPID
	t.Cleanup(func() { jobPID = savedJob })
	for _, tc := range []struct {
		name, label string
		pid         int
		err         error
		managed     bool
	}{
		{"its job", "dev.tracepad.server", 4242, nil, true},
		{"a job that runs another process", "com.example.agent", 77, nil, false},
		{"a job launchctl does not know", "com.example.gone", 0, errors.New("not found"), false},
		{"a terminal", "0", 4242, nil, false},
		{"an application", "application.com.apple.Terminal.123", 4242, nil, false},
	} {
		jobPID = func(int, string) (int, error) { return tc.pid, tc.err }
		q := p
		q.Env = []string{"XPC_SERVICE_NAME=" + tc.label}
		if got := launchdManager(q, 501); (got != "") != tc.managed {
			t.Errorf("launchd, %s: managed = %q", tc.name, got)
		}
	}
}
