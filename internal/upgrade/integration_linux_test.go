//go:build linux && upgradeint

package upgrade

import (
	"strings"
	"testing"
)

// runnerCgroup is where every process of a GitHub-hosted runner sits: inside
// the runner agent's unit, which started the shell and runs nothing of ours.
const runnerCgroup = "0::/system.slice/hosted-compute-agent.service\n"

func asOnARunner(t *testing.T, mainPID func() int, execStart string) {
	t.Helper()
	savedCgroup, savedShow := cgroupOf, unitShow
	t.Cleanup(func() { cgroupOf, unitShow = savedCgroup, savedShow })
	cgroupOf = func(string) string { return runnerCgroup }
	unitShow = func(bool, string) (int, string, error) { return mainPID(), execStart, nil }
}

func TestAServerStartedInsideAnotherServicesUnitIsTheCommands(t *testing.T) {
	w := newWorld(t)
	w.start()
	asOnARunner(t, func() int { return 1 }, "{ path=/opt/runner/agent ; argv[]=/opt/runner/agent ; ignore_errors=no }")
	rep, code := w.run(w.deps(), "--to", vNew, "--data-dir", w.data)
	if code != exitOK {
		t.Fatalf("exit %d, %s", code, rep.Summary)
	}
	w.waitVersion(vNew)
}

func TestAServerItsUnitRunsIsThePersons(t *testing.T) {
	w := newWorld(t)
	pid := w.start()
	asOnARunner(t, func() int { return pid }, "")
	rep, code := w.run(w.deps(), "--to", vNew, "--data-dir", w.data)
	if code != exitRefused || !strings.Contains(rep.Summary, "runs as the systemd unit hosted-compute-agent.service") {
		t.Fatalf("exit %d, %s", code, rep.Summary)
	}
	if !alive(pid) || w.installedVersion() != vOld {
		t.Error("something was touched")
	}
}
