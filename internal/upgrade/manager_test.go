package upgrade

import (
	"errors"
	"testing"
)

func TestOnlyTheUnitItselfProvesItRunsTheServer(t *testing.T) {
	p := Process{PID: 4242, PPID: 4100, Exe: "/home/u/.local/bin/tracepad", Argv: []string{"tracepad", "serve", "--data-dir", "/home/u/data"}}
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
		unasked      bool
	}{
		{"a CI runner's agent, which started the shell", "0::/system.slice/hosted-compute-agent.service\n",
			shown{pid: 812, exec: "{ path=/opt/runner/agent ; argv[]=/opt/runner/agent --run ; ignore_errors=no }"}, false, "hosted-compute-agent.service", false},
		{"cron, which started the shell", "0::/system.slice/cron.service\n", shown{pid: 600}, false, "cron.service", false},
		{"the unit whose main PID it is", "0::/system.slice/tracepad.service\n", shown{pid: 4242}, true, "tracepad.service", false},
		{"a unit that starts this binary so", "0::/system.slice/tracepad.service\n",
			shown{pid: 3900, exec: "{ path=/home/u/.local/bin/tracepad ; argv[]=/home/u/.local/bin/tracepad serve --data-dir /home/u/data ; ignore_errors=no }"}, true, "tracepad.service", false},
		{"a unit that runs tracepad, with another directory", "0::/system.slice/tracepad.service\n",
			shown{pid: 3900, exec: "{ path=/home/u/.local/bin/tracepad ; argv[]=/home/u/.local/bin/tracepad serve --data-dir /var/lib/tp ; ignore_errors=no }"}, true, "tracepad.service", false},
		{"a wrapper script, the server's parent", "0::/system.slice/obs.service\n",
			shown{pid: 4100, exec: "{ path=/opt/bin/start.sh ; argv[]=/opt/bin/start.sh ; ignore_errors=no }"}, true, "obs.service", false},
		{"a wrapper script further up, named for it", "0::/system.slice/obs.service\n",
			shown{pid: 3000, exec: "{ path=/opt/bin/run-tracepad.sh ; argv[]=/opt/bin/run-tracepad.sh ; ignore_errors=no }"}, true, "obs.service", false},
		{"a user unit", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/tracepad.service\n", shown{pid: 4242}, true, "--user tracepad.service", false},
		{"a unit systemctl cannot show (no user bus)", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/tracepad.service\n", shown{err: errors.New("Failed to connect to bus")}, false, "--user tracepad.service", true},
		{"a wrapper without exec, the server's parent", "0::/system.slice/tracepad.service\n",
			shown{pid: 4100, exec: "{ path=/bin/sh ; argv[]=/bin/sh -c tracepad serve --data-dir /home/u/data ; ignore_errors=no }"}, true, "tracepad.service", false},
		{"a unit whose main process is the server's parent, whatever it runs", "0::/system.slice/hosted-compute-agent.service\n",
			shown{pid: 4100, exec: "{ path=/opt/runner/agent ; argv[]=/opt/runner/agent --run ; ignore_errors=no }"}, true, "hosted-compute-agent.service", false},
		{"tmux under a user manager", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/tmux-spawn-1.scope\n", shown{pid: 4242}, false, "", false},
		{"a terminal's session", "0::/user.slice/user-1000.slice/session-3.scope\n", shown{pid: 4242}, false, "", false},
	} {
		asked = nil
		unitShow = show(tc.shown)
		got, unasked := systemdManager(p, tc.cgroup)
		if (got != "") != tc.managed || (unasked != "") != tc.unasked {
			t.Errorf("%s: managed = %q, unasked = %q", tc.name, got, unasked)
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
		args        string
		err         error
		managed     bool
		unasked     bool
	}{
		{"its job", "dev.tracepad.server", 4242, "/home/u/.local/bin/tracepad serve", nil, true, false},
		{"its job, through sh -c without exec", "dev.tracepad.server", 4100, "/bin/sh -c tracepad serve --data-dir /home/u/data", nil, true, false},
		{"a job that runs another process", "com.example.agent", 77, "/usr/libexec/agent", nil, false, false},
		{"its job, through a script of its own", "com.example.obs", 4100, "/bin/sh /Users/u/start.sh", nil, true, false},
		{"a job far up that runs something else", "com.example.agent", 300, "/usr/libexec/agent", nil, false, false},
		{"a job launchctl does not know", "com.example.gone", 0, "", errors.New("not found"), false, true},
		{"a terminal", "0", 4242, "", nil, false, false},
		{"an application", "application.com.apple.Terminal.123", 4242, "", nil, false, false},
	} {
		jobPID = func(int, string) (int, string, error) { return tc.pid, tc.args, tc.err }
		q := p
		q.Env = []string{"XPC_SERVICE_NAME=" + tc.label}
		got, unasked := launchdManager(q, 501)
		if (got != "") != tc.managed || (unasked != "") != tc.unasked {
			t.Errorf("launchd, %s: managed = %q, unasked = %q", tc.name, got, unasked)
		}
	}
}
