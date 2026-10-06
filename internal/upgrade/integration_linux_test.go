//go:build linux && upgradeint

package upgrade

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// sessionScope is a terminal's: what a server started by hand, or by
// setup.md's `nohup … &`, sits in. On a CI runner every process sits in the
// agent's unit instead, which by #29 makes it the person's; so the servers
// these tests start are read as in a session, and the tests below read the
// cgroup as it is.
const sessionScope = "0::/user.slice/user-1000.slice/session-1.scope\n"

func init() { cgroupOf = func(string) string { return sessionScope } }

// runnerCgroup is where every process of a GitHub-hosted runner sits.
const runnerCgroup = "0::/system.slice/hosted-compute-agent.service\n"

// In a service's unit — the CI runner's agent's, whatever started the shell —
// a server is the person's, and the command leaves it (#29).
func TestAServerInAServicesUnitIsThePersons(t *testing.T) {
	w := newWorld(t)
	pid := w.start()
	saved := cgroupOf
	t.Cleanup(func() { cgroupOf = saved })
	cgroupOf = func(string) string { return runnerCgroup }
	rep, code := w.run(w.deps(), "--to", vNew, "--data-dir", w.data)
	if code != exitRefused || !strings.Contains(rep.Summary, "the systemd unit hosted-compute-agent.service") {
		t.Fatalf("exit %d, %s", code, rep.Summary)
	}
	if !alive(pid) || w.installedVersion() != vOld {
		t.Error("something was touched")
	}
}

// The machine's own cgroup, read as the command reads it, gives the rule's
// answer: on a runner, a service's unit, and in a container without systemd
// the root, which rules no manager out (the audit of #223): either way the
// server is the person's; only a session's or a terminal's scope makes it the
// command's.
func TestTheRealCgroupDecidesByTheRule(t *testing.T) {
	saved := cgroupOf
	t.Cleanup(func() { cgroupOf = saved })
	cgroupOf = func(dir string) string {
		b, _ := os.ReadFile(dir + "/cgroup")
		return string(b)
	}
	w := newWorld(t)
	pid := w.start()
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	want := systemdUnit(string(b))
	p, err := newSystem().Inspect(pid)
	if err != nil {
		t.Fatal(err)
	}
	if p.Manager != want {
		t.Errorf("the process reads %q, the rule says %q", p.Manager, want)
	}
	s, _ := classifyServer(p, w.install)
	if s.Ours == (want != "") {
		t.Errorf("cgroup %q: ours=%v (%s)", b, s.Ours, s.Reason)
	}
	t.Logf("this machine: %q → %q", strings.TrimSpace(string(b)), want)
}
