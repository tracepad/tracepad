//go:build unix

package upgrade

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A container's refusals (#47): the reasons its way back refuses on that the
// fake docker can make, each before the upgrade — which must refuse before
// it stops the container — and after a healthy one, where --back reaches 0
// once the condition is gone. They are rows of refusalReasons, as a server's
// are.

type containerCell struct {
	t        *testing.T
	d        *fakeDocker
	deps     Deps
	problems []string
	log      []string
}

func newContainerCell(t *testing.T) *containerCell {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	c := &containerCell{t: t, d: d, deps: containerDeps(t, d, "0.1.0")}
	c.deps.StopWait = 100 * time.Millisecond
	return c
}

func (c *containerCell) problem(format string, args ...any) {
	c.problems = append(c.problems, fmt.Sprintf(format, args...))
}

func (c *containerCell) cmd(deps Deps, args ...string) (Report, int) {
	rep, code, off := cellRun(deps, args...)
	c.log = append(c.log, fmt.Sprintf("%s -> %d %s", strings.Join(args, " "), code, oneLine(rep.Summary)))
	if off != "" {
		c.problem("offTable on `%s`: %s", strings.Join(args, " "), oneLine(off))
	}
	return rep, code
}

func (c *containerCell) report(name string) {
	if len(c.problems) > 0 {
		c.t.Errorf("%s: %s\n%s", name, strings.Join(c.problems, " | "), strings.Join(c.log, "\n"))
	}
}

// untouched: setup's container runs as it did — its image, its policy.
func (c *containerCell) untouched(label string, image string) {
	cur := c.d.container("tracepad-app")
	if cur == nil || !cur.State.Running || cur.Config.Image != image || cur.HostConfig.RestartPolicy.Name != "always" {
		c.problem("%s: the container is not as it was: %+v", label, cur)
		return
	}
	if n := c.d.traces("tracepad-app"); n != 7 {
		c.problem("%s: its volume holds %d traces", label, n)
	}
}

func (c *containerCell) backUntil(id string, n int) bool {
	for i := 0; i < n; i++ {
		if _, code := c.cmd(c.deps, "--back", id); code == exitOK {
			return true
		}
	}
	return false
}

// aCell: the condition holds before the upgrade, which must refuse before
// it stops the container, the container untouched.
func (c *containerCell) aCell(make func() (undo func())) {
	undo := make()
	rep, code := c.cmd(c.deps, "--to", "0.2.0")
	if code != exitRefused || rep.Run != nil {
		c.problem("not refused before the stop: %d %s", code, oneLine(rep.Summary))
	}
	c.untouched("refused", "ghcr.io/tracepad/tracepad:0.1.0")
	undo()
}

// bAfterHealthy: a healthy upgrade, the condition, --back refused; once it
// is gone, --back reaches 0 and the old image runs on its traces.
func (c *containerCell) bAfterHealthy(make func(id, dir string) (undo func())) {
	rep, code := c.cmd(c.deps, "--to", "0.2.0")
	if code != exitOK {
		c.problem("healthy upgrade: %d %s", code, oneLine(rep.Summary))
		return
	}
	undo := make(rep.Run.ID, rep.Run.Dir)
	if _, code := c.cmd(c.deps, "--back", rep.Run.ID); code == exitOK {
		c.problem("--back was not refused")
	}
	undo()
	if !c.backUntil(rep.Run.ID, 3) {
		c.problem("with the condition removed, --back never reached 0")
	}
	c.untouched("end", "ghcr.io/tracepad/tracepad:0.1.0")
}

func containerRefusalCases() []refusalCase {
	type cc struct {
		name string
		run  func(c *containerCell)
	}
	var out []refusalCase
	for _, tc := range []cc{
		// C03: busybox, which restores the archive, gone and not to be had.
		{"C03-busybox-gone/a", func(c *containerCell) {
			c.aCell(func() func() {
				img := c.d.images[busybox]
				c.d.set(func() { delete(c.d.images, busybox) })
				return func() { c.d.set(func() { c.d.images[busybox] = img }) }
			})
		}},
		{"C03-busybox-gone/b-healthy", func(c *containerCell) {
			c.bAfterHealthy(func(string, string) func() {
				img := c.d.images[busybox]
				c.d.set(func() { delete(c.d.images, busybox) })
				return func() { c.d.set(func() { c.d.images[busybox] = img }) }
			})
		}},
		// C08/C09: the room for a restore beside the volume, untold or short.
		{"C08-room-untold/a", func(c *containerCell) {
			c.aCell(func() func() {
				c.d.set(func() { c.d.failDf = true })
				return func() { c.d.set(func() { c.d.failDf = false }) }
			})
		}},
		{"C08-room-untold/b-healthy", func(c *containerCell) {
			c.bAfterHealthy(func(string, string) func() {
				c.d.set(func() { c.d.failDf = true })
				return func() { c.d.set(func() { c.d.failDf = false }) }
			})
		}},
		{"C09-no-room/a", func(c *containerCell) {
			c.aCell(func() func() {
				c.d.set(func() { c.d.room = 1 })
				return func() { c.d.set(func() { c.d.room = 0 }) }
			})
		}},
		{"C09-no-room/b-healthy", func(c *containerCell) {
			c.bAfterHealthy(func(string, string) func() {
				c.d.set(func() { c.d.room = 1 })
				return func() { c.d.set(func() { c.d.room = 0 }) }
			})
		}},
		// C02: the old image gone since.
		{"C02-old-image-gone/b-healthy", func(c *containerCell) {
			c.bAfterHealthy(func(string, string) func() {
				const ref = "ghcr.io/tracepad/tracepad:0.1.0"
				img := c.d.images[ref]
				c.d.set(func() { delete(c.d.images, ref); delete(c.d.images, img.ID) })
				return func() { c.d.set(func() { c.d.images[ref], c.d.images[img.ID] = img, img }) }
			})
		}},
		// C07: a container made since under the name a way back sets aside to.
		{"C07-after-exists/b-healthy", func(c *containerCell) {
			c.bAfterHealthy(func(id, _ string) func() {
				other := c.d.run(c.t, "--name", "tracepad-app-after-"+id, "ghcr.io/tracepad/tracepad:0.1.0")
				return func() {
					c.d.set(func() {
						delete(c.d.byName, "tracepad-app-after-"+id)
						other.Name = "/elsewhere"
						c.d.byName["elsewhere"] = other
					})
				}
			})
		}},
		// C10: the run's copy of the host's binary changed since.
		{"C10-copy-wrong/b-healthy", func(c *containerCell) {
			c.bAfterHealthy(func(_, dir string) func() {
				copyPath := filepath.Join(dir, "tracepad-0.1.0")
				b, _ := os.ReadFile(copyPath)
				scriptBinary(c.t, copyPath, "0.0.9")
				return func() { _ = os.WriteFile(copyPath, b, 0o755) }
			})
		}},
		// C11: docker gone from PATH since.
		{"C11-docker-gone/b-healthy", func(c *containerCell) {
			c.bAfterHealthy(func(string, string) func() {
				saved := c.deps.Docker
				c.deps.Docker = nil
				return func() { c.deps.Docker = saved }
			})
		}},
		// C12: the run's record of the container gone, or changed to give
		// it a setting the command does not recreate.
		{"C12-image-json-missing/b-healthy", func(c *containerCell) {
			c.bAfterHealthy(func(_, dir string) func() { return moveAway(filepath.Join(dir, "image.json")) })
		}},
		{"C12-container-json-privileged/b-healthy", func(c *containerCell) {
			c.bAfterHealthy(func(_, dir string) func() {
				path := filepath.Join(dir, "container.json")
				b, _ := os.ReadFile(path)
				_ = os.WriteFile(path, []byte(strings.Replace(string(b), `"Privileged":false`, `"Privileged":true`, 1)), 0o600)
				return func() { _ = os.WriteFile(path, b, 0o600) }
			})
		}},
		// C33: the new container does not stop when the way back asks.
		{"C33-new-container-slow-to-stop/b-healthy", func(c *containerCell) {
			c.bAfterHealthy(func(string, string) func() {
				c.d.set(func() { c.d.stubborn = true })
				return c.d.stopLate
			})
		}},
	} {
		out = append(out, refusalCase{name: tc.name, run: func(rc *refusalCell) {
			c := newContainerCell(rc.t)
			tc.run(c)
			c.report(tc.name)
		}})
	}
	return out
}

// Every start of the old version a container's way back makes — the old
// container again, put back under its name, or run from the run's record —
// refuses when docker does not start it, and the next --back starts it
// (#47): never a refusal for good.
func testAContainersStartsThatFailAreTakenUpAgain(t *testing.T) {
	inject := func(points ...string) func(string) error {
		left := map[string]bool{}
		for _, p := range points {
			left[p] = true
		}
		return func(point string) error {
			if left[point] {
				delete(left, point)
				return errInjected
			}
			return nil
		}
	}
	for _, tc := range []struct {
		name, says string
		points     []string
		between    func(c *containerCell, id string)
	}{
		// The archive failed: the old container, stopped, starts again.
		{"the old container again", "did not start again", []string{stepArchived, stepBackStarted}, nil},
		// The run failed after the rename: the old container goes back under
		// its name.
		{"the old container put back", "could not be put back and started", []string{stepStarted, stepBackStarted}, nil},
		// The same, the old container removed by the person since: the old
		// image runs from the run's record.
		{"the old image from the record", "did not start (", []string{stepStarted, stepBackStarted}, func(c *containerCell, id string) {
			c.d.set(func() { delete(c.d.byName, "tracepad-app-before-"+id) })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newContainerCell(t)
			deps := c.deps
			if tc.between != nil {
				// The upgrade stops where the way back is to begin: a kill.
				deps.Fault = func(point string) error {
					if point == stepStarted {
						return errInjected
					}
					if point == stepBackBegun+recordPoint {
						panic(killed{})
					}
					return nil
				}
				c.cmd(deps, "--to", "0.2.0")
				id, _ := runOf(t, deps)
				tc.between(c, id)
				deps.Fault = inject(stepBackStarted)
				if rep, code := c.cmd(deps, "--back", id); code != exitStuck || !strings.Contains(rep.Summary, tc.says) {
					c.problem("--back: %d %s", code, oneLine(rep.Summary))
				}
			} else {
				deps.Fault = inject(tc.points...)
				if rep, code := c.cmd(deps, "--to", "0.2.0"); code != exitStuck || !strings.Contains(rep.Summary, tc.says) {
					c.problem("the upgrade: %d %s", code, oneLine(rep.Summary))
				}
			}
			id, _ := runOf(t, c.deps)
			if !c.backUntil(id, 1) {
				c.problem("--back did not start it once it could")
			}
			c.untouched("end", "ghcr.io/tracepad/tracepad:0.1.0")
			c.report(tc.name)
		})
	}
}

// containerScenarios are a container's tests that run the command: they run
// side by side with the matrices, under inProcess, as a case of them (the
// gate's budget, spec 054 #43, #47). A refusal's row names one by its name.
var containerScenarios = []struct {
	name string
	run  func(t *testing.T)
}{
	{"AContainerUpgradeKeepsMountsPolicyAndVariables", testAContainerUpgradeKeepsMountsPolicyAndVariables},
	{"ABrokenImageGoesBackIntoANewVolume", testABrokenImageGoesBackIntoANewVolume},
	{"AFailedRunPutsTheOldContainerBack", testAFailedRunPutsTheOldContainerBack},
	{"AWayBackWhoseVolumeExistsTouchesNothing", testAWayBackWhoseVolumeExistsTouchesNothing},
	{"AMissingImageStopsNothing", testAMissingImageStopsNothing},
	{"AWayBackLeavesALaterRunsContainerAlone", testAWayBackLeavesALaterRunsContainerAlone},
	{"AWayBackThatIsNotConfirmedSaysSo", testAWayBackThatIsNotConfirmedSaysSo},
	{"AFailedRestoreNamesWhatItLeft", testAFailedRestoreNamesWhatItLeft},
	{"TheRemovalsRemoveContainersBeforeVolumes", testTheRemovalsRemoveContainersBeforeVolumes},
	{"AnInterruptAfterTheContainerStopsStillBringsItBack", testAnInterruptAfterTheContainerStopsStillBringsItBack},
	{"AContainerArchiveChangedSinceIsNotRestored", testAContainerArchiveChangedSinceIsNotRestored},
	{"AFailedRunInTheWayBackDoesNotBlockTheNext", testAFailedRunInTheWayBackDoesNotBlockTheNext},
	{"AWayBackAfterTheNewContainerWasRemoved", testAWayBackAfterTheNewContainerWasRemoved},
	{"ADeadlineSpentByTheSwapIsNotTheWayBacks", testADeadlineSpentByTheSwapIsNotTheWayBacks},
	{"AnInterruptedHealthySwapFinishes", testAnInterruptedHealthySwapFinishes},
	{"AContainerThatDoesNotStopIsNeverKilled", testAContainerThatDoesNotStopIsNeverKilled},
	{"AContainerWayBackStartsAgainAfterTheBinaryWasPutBack", testAContainerWayBackStartsAgainAfterTheBinaryWasPutBack},
	{"ADockerThatDoesNotAnswerSettlesNothing", testADockerThatDoesNotAnswerSettlesNothing},
	{"AContainerWayBackIsNotRefusedForABinaryItNeverReplaced", testAContainerWayBackIsNotRefusedForABinaryItNeverReplaced},
	{"AVolumeWithOptionsIsRefusedBeforeTheStop", testAVolumeWithOptionsIsRefusedBeforeTheStop},
	{"AContainersStartsThatFailAreTakenUpAgain", testAContainersStartsThatFailAreTakenUpAgain},
	{"AWayBackAsksDockerAgainForAPolicy", testAWayBackAsksDockerAgainForAPolicy},
	{"AProcessOnTheBinaryKeepsItAndTheContainerGoesOn", testAProcessOnTheBinaryKeepsItAndTheContainerGoesOn},
}
