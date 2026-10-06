//go:build unix

package upgrade

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	{"AContainerGoesBackWhateverTheHostBinary", testAContainerGoesBackWhateverTheHostBinary},
	{"ARunThatStartedDespiteAnErrorRestoresTheVolume", testARunThatStartedDespiteAnErrorRestoresTheVolume},
	{"TheOldImageKeepsAName", testTheOldImageKeepsAName},
	{"AVolumeWithALinkIsRefusedBeforeTheStop", testAVolumeWithALinkIsRefusedBeforeTheStop},
	{"ANewerContainerDoesNotHoldAnotherRun", testANewerContainerDoesNotHoldAnotherRun},
	{"AProcessOnTheBinaryKeepsItAndTheContainerGoesOn", testAProcessOnTheBinaryKeepsItAndTheContainerGoesOn},
}

// The host's binary cannot be put back — its copy in the run changed, a
// server runs it at the later version, the person put another there — and
// the container goes back all the same: the binary stays, said (the review
// of #226).
func testAContainerGoesBackWhateverTheHostBinary(t *testing.T) {
	for name, change := range map[string]func(c *containerCell, dir string){
		"the copy changed": func(c *containerCell, dir string) { scriptBinary(c.t, filepath.Join(dir, "tracepad-0.1.0"), "0.0.9") },
		"another version": func(c *containerCell, _ string) {
			scriptBinary(c.t, filepath.Join(c.deps.InstallDir, "tracepad"), "0.2.1")
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := newContainerCell(t)
			rep, code := c.cmd(c.deps, "--to", "0.2.0")
			if code != exitOK {
				t.Fatalf("%d %s", code, rep.Summary)
			}
			change(c, rep.Run.Dir)
			back, code := c.cmd(c.deps, "--back", rep.Run.ID)
			if code != exitOK || !strings.Contains(strings.Join(back.Notes, "\n"), "was not put back") || !strings.Contains(strings.Join(back.Notes, "\n"), "it stays as it is") {
				t.Errorf("%d %s %q", code, back.Summary, back.Notes)
			}
			c.untouched("end", "ghcr.io/tracepad/tracepad:0.1.0")
			c.report(name)
		})
	}
}

// docker run can answer an error after its container started and ran on
// the volume: that is a start, as settle reads it, and the way back restores
// the volume rather than start the old version on what the new one migrated
// (the review of #226).
func testARunThatStartedDespiteAnErrorRestoresTheVolume(t *testing.T) {
	c := newContainerCell(t)
	c.d.set(func() { c.d.failAfterStart = true })
	rep, code := c.cmd(c.deps, "--to", "0.2.0")
	if code != exitWentBack {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	st, _ := loadState(rep.Run.Dir)
	if !st.has(stepStarted) || !st.has(stepBackVolume) {
		t.Errorf("the volume the new version ran on was not restored: %+v", st.Steps)
	}
	cur := c.d.container("tracepad-app")
	if cur == nil || cur.Config.Image != "ghcr.io/tracepad/tracepad:0.1.0" || !slices.ContainsFunc(cur.Mounts, func(m mount) bool { return m.Name == "tracepad-app-"+rep.Run.ID }) {
		t.Errorf("after the way back: %+v", cur)
	}
}

// The tag the container was made from names another image by the way back
// — latest, pulled since: the old image runs by a name of the release's, its
// digest or its tag, never its bare ID, and the next plan still knows it as
// the image's (the review of #226).
func testTheOldImageKeepsAName(t *testing.T) {
	for _, digest := range []bool{true, false} {
		t.Run(fmt.Sprint("digest ", digest), func(t *testing.T) {
			d := newFakeDocker(t)
			old := d.images["ghcr.io/tracepad/tracepad:0.1.0"]
			d.images["ghcr.io/tracepad/tracepad:latest"] = old
			d.volumes["tracepad-app"] = 7
			d.run(t, "--name", "tracepad-app", "--mount", "type=volume,src=tracepad-app,dst=/data", "-p", "127.0.0.1:4318:4318", "ghcr.io/tracepad/tracepad:latest")
			deps := containerDeps(t, d)
			rep, code := runReport(t, deps, "--to", "0.2.0")
			if code != exitOK {
				t.Fatalf("%d %s", code, rep.Summary)
			}
			byDigest := "ghcr.io/tracepad/tracepad@sha256:" + strings.Repeat("f", 64)
			d.set(func() {
				d.images["ghcr.io/tracepad/tracepad:latest"] = d.images["ghcr.io/tracepad/tracepad:0.2.0"]
				delete(d.images, "ghcr.io/tracepad/tracepad:0.1.0")
				if digest {
					d.digests = map[string][]string{old.ID: {"elsewhere/other@sha256:" + strings.Repeat("e", 64), byDigest}}
					d.images[byDigest] = old
				}
			})
			if back, code := runReport(t, deps, "--back", rep.Run.ID); code != exitOK {
				t.Fatalf("--back: %d %s", code, back.Summary)
			}
			want := "ghcr.io/tracepad/tracepad:0.1.0"
			if digest {
				want = byDigest
			}
			if cur := d.container("tracepad-app"); cur.Config.Image != want {
				t.Errorf("the old image runs as %q, not %q", cur.Config.Image, want)
			}
			plan, _ := runReport(t, deps, "--plan", "--to", "0.2.0")
			if len(plan.Containers) == 0 || plan.Containers[0].Name != "tracepad-app" || plan.Containers[0].Whose != "command" {
				t.Errorf("the next plan does not know it: %+v", plan.Containers)
			}
		})
	}
}

// A volume that holds a link, which a restore would not make again, is
// refused before anything stops (the review of #226).
func testAVolumeWithALinkIsRefusedBeforeTheStop(t *testing.T) {
	c := newContainerCell(t)
	c.d.set(func() { c.d.links = "/data/tls/cert.pem\n" })
	rep, code := c.cmd(c.deps, "--to", "0.2.0")
	if code != exitRefused || !strings.Contains(rep.Summary, "/data/tls/cert.pem, a link or a special file") {
		t.Errorf("%d %s", code, rep.Summary)
	}
	c.untouched("refused", "ghcr.io/tracepad/tracepad:0.1.0")
	c.report("link")
}

// One of the command's containers running later than --to shares nothing
// with a run aimed at another: it is said, and the run goes on (the review
// of #226).
func testANewerContainerDoesNotHoldAnotherRun(t *testing.T) {
	c := newContainerCell(t)
	c.d.add("tracepad-later", "0.2.1", "127.0.0.1", "4400", "later", nil)
	rep, code := c.cmd(c.deps, "--to", "0.2.0", "--container", "tracepad-app")
	if code != exitOK || !strings.Contains(strings.Join(rep.Notes, "\n"), "container tracepad-later runs 0.2.1, later than 0.2.0; it is left as it is") {
		t.Errorf("%d %s %q", code, rep.Summary, rep.Notes)
	}
	if later := c.d.container("tracepad-later"); !later.State.Running || later.Config.Image != "ghcr.io/tracepad/tracepad:0.2.1" {
		t.Errorf("the later one was touched: %+v", later)
	}
	if rep, code := c.cmd(c.deps, "--plan", "--to", "0.2.0", "--container", "tracepad-later"); code != exitRefused || !strings.Contains(rep.Summary, "older than 0.2.1") {
		t.Errorf("named, it is a downgrade: %d %s", code, rep.Summary)
	}
}
