package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// A container's run (spec 054 #47): the swap and the way back of Decision
// 10 and 11, on the machine of #31 and #34 — every act written as an intent
// first, settled from what docker says on the next load, and a way back that
// checks what it needs before its first act.

func (j *job) before() string { return j.st.Container.Name + "-before-" + j.st.Run }
func (j *job) after() string  { return j.st.Container.Name + "-after-" + j.st.Run }
func (j *job) failed() string { return j.st.Container.Name + "-failed-" + j.st.Run }

// prepareContainer looks at the container again, by the plan's rule, pulls
// the new image, writes what the way back recreates the container from, and
// measures the volume. It answers the bytes the volume holds.
func (j *job) prepareContainer(ctx context.Context, p *plan) (int64, string) {
	r, c, st := j.r, p.container, j.st
	// What it is now, read by the same functions as the plan's: a container
	// changed since is refused, not recreated from the plan's reading.
	ic, err := r.inspectOne(ctx, c.Name)
	switch {
	case err != nil:
		return 0, "could not inspect " + c.Name + ": " + firstLine(err.Error())
	case ic == nil || ic.ID != c.inspect.ID:
		return 0, c.Name + " is not the container the plan found any more"
	}
	images, imgErr := r.inspectImages(ctx, []string{ic.Image})
	info := r.info(ctx)
	now, _ := asContainer(*ic) // ignored: the plan found it of the image, and its ID is the same
	r.recreate(&now, images, imgErr, info)
	now.Version = c.Version
	if reason := containerRefusal(now, r.flags.container, info); reason != "" {
		return 0, c.Name + " changed since the plan: " + reason
	}
	// The volume a way back restores beside is a plain local one: its
	// options would not come with a new one (#47).
	out, err := r.deps.Docker.Run(ctx, "volume", "inspect", "--format", "{{.Driver}} {{json .Options}}", now.Volume)
	if err != nil {
		return 0, "could not inspect the volume " + now.Volume + ": " + firstLine(err.Error())
	}
	if f := strings.Fields(string(out)); len(f) != 2 || f[0] != "local" || (f[1] != "null" && f[1] != "{}") {
		return 0, fmt.Sprintf("the volume %s has options of its own (%s), which a way back's new volume would not have: back it up and upgrade it yourself", now.Volume, strings.TrimSpace(string(out)))
	}
	newRef := now.Repo + ":" + p.to
	if _, err := r.deps.Docker.Run(ctx, "pull", "-q", newRef); err != nil {
		return 0, "could not pull " + newRef + ": " + firstLine(err.Error())
	}
	j.done("pulled %s", newRef)
	if err := r.ensureBusybox(ctx); err != nil {
		return 0, err.Error()
	}
	// One look at the volume, from one busybox container (the review of
	// #226): what a restore would not make again — a link, a device, a pipe —
	// refuses the run here, before anything stops, as the archive's
	// read-back and the way back refuse it; what it holds; and the room
	// beside it.
	look, err := r.lookAt(ctx, now.Volume)
	if err != nil {
		return 0, "could not look at the volume " + now.Volume + ": " + firstLine(err.Error())
	}
	if len(look.odd) > 0 {
		return 0, fmt.Sprintf("the volume %s holds %s, a link or a special file, which a way back's restore would not make again: back it up and upgrade it yourself", now.Volume, look.odd[0])
	}
	j.ctr = now
	// What the way back recreates the container from: its inspect and its
	// image's, from which it writes the run again — never a run read back
	// from a file (the security review of #47).
	raw, err := json.Marshal(now.inspect.raw)
	if err == nil {
		err = writeFile(filepath.Join(j.dir, "container.json"), raw)
	}
	if err == nil {
		err = writeJSON(filepath.Join(j.dir, "image.json"), now.image)
	}
	if err != nil {
		return 0, err.Error()
	}
	// The variables the person gave it, for --env-file: a file of the run's,
	// never a command line (Decision 10).
	env := strings.Join(personEnv(now.inspect, now.EnvNames), "\n")
	if env != "" {
		env += "\n"
	}
	if err := os.WriteFile(filepath.Join(j.dir, "env"), []byte(env), 0o600); err != nil {
		return 0, err.Error()
	}
	hc := now.inspect.HostConfig
	st.Container = &ContainerState{Name: now.Name, ID: now.inspect.ID, Volume: now.Volume, URL: now.URL,
		OldRef: now.Ref, OldImage: now.inspect.Image, NewRef: newRef, LogDriver: info.logDriver,
		Restart: restartArg(hc.RestartPolicy.Name, hc.RestartPolicy.MaximumRetryCount)}
	j.ctr.freeKB = look.freeKB
	return look.usedKB << 10, ""
}

// ensureBusybox has busybox here, pulling it when it is not: it archives and
// restores a volume.
func (r *runner) ensureBusybox(ctx context.Context) error {
	if _, err := r.deps.Docker.Run(ctx, "image", "inspect", busybox); err == nil {
		return nil
	}
	if _, err := r.deps.Docker.Run(ctx, "pull", "-q", busybox); err != nil {
		return fmt.Errorf("%s, which archives and restores a volume, is not there and could not be pulled (%s)", busybox, firstLine(err.Error()))
	}
	return nil
}

// volumeLook is one look at a volume from busybox: the paths of what a
// restore would not make again, the kilobytes it holds, and the kilobytes
// free on its file system.
type volumeLook struct {
	odd            []string
	usedKB, freeKB int64
}

// lookScript is lookAt's: set -e, so a find, du or df that fails fails the
// look, never reads as "nothing odd" (the review of #226).
const lookScript = `set -e; find /data \( -type l -o -type f -links +1 -o ! -type f ! -type d \) -print; echo ===; du -sk /data; df -Pk /data`

// lookAt looks at a volume, mounted read-only, from one busybox container.
func (r *runner) lookAt(ctx context.Context, volume string) (volumeLook, error) {
	out, err := r.deps.Docker.Run(ctx, "run", "--rm", "--mount", csvField("type=volume", "src="+volume, "dst=/data", "readonly"), busybox, "sh", "-c", lookScript)
	if err != nil {
		return volumeLook{}, err
	}
	odd, rest, ok := strings.Cut(string(out), "===\n")
	lines := strings.Split(strings.TrimSpace(rest), "\n")
	if !ok || len(lines) < 3 {
		return volumeLook{}, fmt.Errorf("busybox said %q", strings.TrimSpace(string(out)))
	}
	look := volumeLook{odd: strings.Fields(odd)}
	used := strings.Fields(lines[0])
	free := strings.Fields(lines[len(lines)-1]) // Filesystem 1024-blocks Used Available Capacity Mounted-on
	if len(used) == 0 || len(free) < 4 {
		return volumeLook{}, fmt.Errorf("busybox said %q", strings.TrimSpace(string(out)))
	}
	if look.usedKB, err = strconv.ParseInt(used[0], 10, 64); err == nil {
		look.freeKB, err = strconv.ParseInt(free[3], 10, 64)
	}
	return look, err
}

// loadContainer reads what a way back recreates the container from — its
// inspect and its image's, as the upgrade saved them — and writes its run
// again with the plan's own function. What it reads must still be a
// container the command recreates, by the plan's rule: a file changed since
// to give it a setting of the person's (a privilege, a bind of their whole
// disk) refuses the way back, rather than run (the security review of #47).
func (j *job) loadContainer() error {
	if j.ctr.Run != nil {
		return nil
	}
	cs := j.st.Container
	b, err := os.ReadFile(filepath.Join(j.dir, "container.json"))
	if err != nil {
		return err
	}
	var ic inspectContainer
	if err := json.Unmarshal(b, &ic); err != nil {
		return err
	}
	if err := json.Unmarshal(b, &ic.raw); err != nil {
		return err
	}
	b, err = os.ReadFile(filepath.Join(j.dir, "image.json"))
	if err != nil {
		return err
	}
	var img imageConfig
	if err := json.Unmarshal(b, &img); err != nil {
		return err
	}
	if ic.ID != cs.ID || img.ID != cs.OldImage {
		return errors.New("they are not this run's container's")
	}
	c, _ := asContainer(ic) // ignored: not the image's is refused just below, as a container whose run cannot be written
	info := dockerInfo{logDriver: cs.LogDriver}
	(&runner{}).recreate(&c, map[string]imageConfig{img.ID: img}, nil, info)
	if reason := containerRefusal(c, cs.Name, info); reason != "" {
		return errors.New("they describe a container the command does not recreate: " + reason)
	}
	if c.Name != cs.Name || c.Volume != cs.Volume || c.URL != cs.URL {
		return errors.New("they describe another name, volume or address than the run's")
	}
	j.ctr = c
	return nil
}

// swapContainer stops the container, archives its volume, sets it aside and
// runs the new image as the old one was created (Decision 10, #47).
func (j *job) swapContainer(ctx context.Context, p *plan) {
	r, st, rep := j.r, j.st, j.rep
	cs := st.Container
	docker := r.deps.Docker
	j.step(stepStopSent)
	err := j.at(stepStopSent)
	if err == nil {
		err = r.askToStop(ctx, cs.Name)
	}
	if err != nil {
		if running, rerr := r.containerRunning(ctx, cs.ID, false); rerr == nil && running {
			// It runs as it did: nothing changed, its restart policy too.
			j.unstep(stepStopSent)
			rep.ExitCode, rep.Summary = exitRefused, "Refused, and nothing changed: "+cs.Name+" could not be stopped: "+firstLine(err.Error())
			if policyErr := r.setPolicy(ctx, cs.Name, cs.Restart); policyErr != nil {
				rep.ExitCode, rep.Summary = exitStuck, cs.Name+" could not be stopped ("+firstLine(err.Error())+"), and its restart policy, set to no for the stop, could not be put back to "+cs.Restart+": "+firstLine(policyErr.Error())
				rep.Next = append(rep.Next, "docker update --restart "+shq(cs.Restart)+" "+shq(cs.Name))
				return
			}
			r.discard(rep, j.dir)
			rep.Run = nil
			return
		}
		j.goBack(ctx, cs.Name+" did not stop cleanly: "+firstLine(err.Error()))
		return
	}
	if err := j.at(stepStopped); err != nil || !r.waitStopped(ctx, cs.ID) {
		// As a server's stop: never a SIGKILL (the sixth review).
		rep.ExitCode = exitStuck
		rep.Summary = fmt.Sprintf("%s was asked to stop and has not stopped in %s; it may still, and its restart policy is no until the way back puts %s back. Nothing else changed.", cs.Name, r.deps.StopWait, cs.Restart)
		rep.Next = append(rep.Next, "when it has stopped, start it again as it was: "+j.upgradeCmd("--back "+st.Run))
		return
	}
	j.step(stepStopped)
	j.done("stopped %s", cs.Name)

	// The archive, written in busybox as docker.md's backup is, the size of
	// the database said first: the read-back on the host holds it to that, as
	// a data directory's is held (the review of #1).
	archive := filepath.Join(j.dir, "data.tar.gz")
	script := fmt.Sprintf("umask 077 && set -C && stat -c %%s /data/%s && tar czf - -C /data . > /backup/data.tar.gz && chown %d:%d /backup/data.tar.gz", dataDBName, os.Getuid(), os.Getgid())
	var a Archived
	err = j.act(stepArchived, "archive the volume "+cs.Volume+" into "+archive, func() error {
		out, err := docker.Run(ctx, "run", "--rm",
			"--mount", csvField("type=volume", "src="+cs.Volume, "dst=/data", "readonly"),
			"--mount", csvField("type=bind", "src="+j.dir, "dst=/backup"),
			busybox, "sh", "-c", script)
		if err != nil {
			return err
		}
		size, err := strconv.ParseInt(strings.TrimSpace(firstLine(string(out))), 10, 64)
		if err != nil {
			return fmt.Errorf("the size of %s was not said: %q", dataDBName, firstLine(string(out)))
		}
		afterArchive(archive)
		rb, err := readBack(archive, Archived{DBSize: size})
		if err != nil {
			return err
		}
		// Its bytes are pinned, as a data directory's archive's are: one
		// changed between the upgrade and a way back is not restored (the
		// second review).
		a = Archived{SHA256: rb.SHA256, DBSize: size, Bytes: rb.Bytes}
		return nil
	})
	if err != nil {
		j.goBack(ctx, "the archive of the volume "+cs.Volume+" failed: "+firstLine(err.Error()))
		return
	}
	st.Archive = &a
	j.step(stepArchived)
	j.done("archived the volume %s into %s and read it back whole", cs.Volume, archive)

	err = j.act(stepRenamedOld, "rename the container "+cs.Name+" to "+j.before(), func() error {
		_, err := docker.Run(ctx, "rename", cs.Name, j.before())
		return err
	})
	if err != nil {
		j.goBack(ctx, "the rename of "+cs.Name+" failed: "+firstLine(err.Error()))
		return
	}
	// Its restart policy is no already, since the stop, and a rename keeps
	// it (the eighth review).
	st.SetAside = append(st.SetAside, "container "+j.before())
	j.step(stepRenamedOld)
	j.done("renamed %s to %s, restart policy no", cs.Name, j.before())

	args := runArgs(j.ctr, cs.Name, cs.NewRef, filepath.Join(j.dir, "env"), "", st.Run, stepStarted)
	var out []byte
	err = j.act(stepStarted, "run "+cs.NewRef+" as "+cs.Name, func() (err error) {
		out, err = docker.Run(ctx, args...)
		return err
	})
	if err != nil {
		// docker run can answer an error after the container started — and
		// ran on the volume: read as a cut-short run is read (settle), it is
		// a start, and the way back restores the volume (the review of
		// #226). One docker cannot say of is the way back's to meet: its
		// first look refuses.
		if started, serr := j.settleContainer(ctx, stepStarted); serr == nil && started {
			j.step(stepStarted)
		}
		j.goBack(ctx, "the new container did not start: "+firstLine(err.Error()))
		return
	}
	cs.NewID = strings.TrimSpace(string(out))
	j.step(stepStarted)
	j.done("ran %s as %s, as it was created: %s", cs.NewRef, cs.Name, runSummary(j.ctr))

	if err := j.at(stepChecked); err != nil {
		j.goBack(ctx, "not healthy: "+err.Error())
		return
	}
	c := r.checkContainer(ctx, cs.URL, p.to, st.before(), cs.NewID, true)
	j.verdict(ctx, c)
}

// askToStop stops a container the way a server is stopped: SIGTERM, never
// the SIGKILL `docker stop` sends after its timeout (the sixth review). Its
// restart policy is set to no first, or docker would start again what the
// signal stopped; the way back puts it back. A container that is not
// running — one a crash loop has between two starts — has nothing to stop.
func (r *runner) askToStop(ctx context.Context, name string) error {
	if err := r.setPolicy(ctx, name, "no"); err != nil {
		return err
	}
	err := r.transient(ctx, func() error {
		_, err := r.deps.Docker.Run(ctx, "kill", "--signal", "TERM", name)
		return err
	})
	if err != nil {
		if c, ierr := r.inspectOne(ctx, name); ierr == nil && c != nil && !c.State.Running {
			return nil
		}
	}
	return err
}

// setPolicy sets a container's restart policy.
func (r *runner) setPolicy(ctx context.Context, ref, policy string) error {
	return r.transient(ctx, func() error {
		_, err := r.deps.Docker.Run(ctx, "update", "--restart", policy, ref)
		return err
	})
}

// dockerRaces are what docker answers while its restart manager moves a
// crash-looping container between two of its states, and not a moment
// later: "cannot update a stopped container" to an update (the real docker
// of the integration tests), "is restarting" to a kill.
var dockerRaces = []string{"cannot update a stopped container", "is restarting"}

// transient runs a docker call, and again — a few times, a quarter of a
// second apart — only while it answers one of dockerRaces (the review of
// #226): a container that is gone or a daemon that does not answer is said
// at once.
func (r *runner) transient(ctx context.Context, call func() error) error {
	for try := 0; ; try++ {
		err := call()
		if err == nil || try == 20 || !slices.ContainsFunc(dockerRaces, func(race string) bool { return strings.Contains(err.Error(), race) }) {
			return err
		}
		if r.deps.Sleep(ctx, 250*time.Millisecond) != nil {
			return err
		}
	}
}

// waitStopped waits, as a server's stop does, for a container asked to stop.
// One docker cannot say of is not stopped.
func (r *runner) waitStopped(ctx context.Context, id string) bool {
	return r.waitUntil(ctx, func() bool {
		ok, err := r.containerRunning(ctx, id, false)
		return err == nil && !ok
	})
}

// containerRunning says whether the container with this id runs. Docker
// keeps a crash-looping container Running under a restart policy, with
// Restarting set and its count growing (the review of #1): that is not
// running. fresh is a container this run just made, which has no business
// having restarted at all. One that is not there does not run; a docker that
// does not answer is an error, never "not running" (the eighth review).
func (r *runner) containerRunning(ctx context.Context, id string, fresh bool) (bool, error) {
	if id == "" {
		return false, nil
	}
	c, err := r.inspectOne(ctx, id)
	if err != nil || c == nil {
		return false, err
	}
	return c.State.Running && !c.State.Restarting && (!fresh || c.RestartCount == 0), nil
}

// checkContainer is a check of the container id at base (Decision 9): alive
// while docker says it runs — a docker that cannot say is not an exit — and,
// once healthy, read again: what answered must still be the run's container,
// running.
func (r *runner) checkContainer(ctx context.Context, base, want string, before counted, id string, fresh bool) Checked {
	alive := func() bool {
		ok, err := r.containerRunning(ctx, id, fresh)
		return ok || err != nil
	}
	c := r.check(ctx, base, want, before, alive)
	c.LogLine = r.containerFirstLog(ctx, id)
	if c.Verdict != verdictHealthy {
		return c
	}
	switch ok, err := r.containerRunning(ctx, id, fresh); {
	case err != nil:
		c.Verdict, c.Why = verdictDecide, fmt.Sprintf("it answers as %s, but docker cannot say whether container %s still runs (%v)", want, id[:12], err)
	case !ok:
		c.Verdict, c.Why = verdictNotHealthy, fmt.Sprintf("%s answered, but container %s does not run", base, id[:12])
	}
	return c
}

// containerFirstLog is the first line the container logged in this start,
// not since its creation: a container the way back starts again has months
// of it (the final review).
func (r *runner) containerFirstLog(ctx context.Context, id string) string {
	args := []string{"logs"}
	if c, err := r.inspectOne(ctx, id); err == nil && c != nil && c.State.StartedAt != "" {
		args = append(args, "--since", c.State.StartedAt)
	}
	out, err := r.deps.Docker.Run(ctx, append(args, id)...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(firstLine(string(out)))
}

// containerBackPreconditions are what a container's way back needs from the
// machine rather than from the run — docker, the old image, busybox, the
// names it makes free, room for a restore, and the install path as the run
// left it when it replaced the binary there — checked by the upgrade before
// its stop and by --back before its first act (#41, #47). restoreBytes is
// what a restore takes, -1 when none is ahead; freeKB is the room on the
// volume's file system as a look just found it, 0 to look now — the
// preparation's look serves its own check, and a way back looks again (the
// review of #226). The host's binary is not one: what keeps it from being put
// back is a note (hostBinaryBack).
func (j *job) containerBackPreconditions(ctx context.Context, restoreBytes, freeKB int64) error {
	r, st := j.r, j.st
	cs := st.Container
	if _, err := r.deps.Docker.Run(ctx, "image", "inspect", cs.OldImage); err != nil {
		return fmt.Errorf("the old image %s is not there to run again (%s)", cs.OldImage, firstLine(err.Error()))
	}
	if restoreBytes >= 0 {
		if err := r.ensureBusybox(ctx); err != nil {
			return err
		}
		// The volume a restore makes must be free, unless an earlier
		// attempt of this run made it (its intent still pending): that one
		// is filled again (#34).
		vol := cs.Volume + "-" + st.Run
		ours := st.Pending != nil && st.Pending.Step == stepBackVolume
		if _, err := r.deps.Docker.Run(ctx, "volume", "inspect", vol); err == nil && !ours {
			return fmt.Errorf("the volume %s exists already, and this run did not make it", vol)
		} else if err != nil && !strings.Contains(err.Error(), "no such volume") {
			return fmt.Errorf("whether the volume %s exists cannot be told (%s)", vol, firstLine(err.Error()))
		}
		if !st.has(stepBackAside) {
			// One that is the run's new container is an earlier attempt's
			// set-aside, cut short before its record.
			if c, err := r.inspectOne(ctx, j.after()); err != nil {
				return fmt.Errorf("whether the container %s exists cannot be told (%s)", j.after(), firstLine(err.Error()))
			} else if c != nil && c.ID != cs.NewID {
				return fmt.Errorf("the container %s exists already, and this run did not put it there", j.after())
			}
		}
		// The restore goes into a new volume on the old one's file system:
		// room for it there, and 100 MiB.
		free := freeKB
		if free <= 0 {
			look, err := r.lookAt(ctx, cs.Volume)
			if err != nil {
				return fmt.Errorf("the room for a restore of the volume %s cannot be told (%s)", cs.Volume, firstLine(err.Error()))
			}
			free = look.freeKB
		}
		if need := restoreBytes + mib100; free<<10 < need {
			return fmt.Errorf("no room beside the volume %s for the restore a way back may need: %d MiB free, %d MiB needed", cs.Volume, free>>10, need>>20)
		}
		// Its check reads the archive's database on this machine, in the
		// run's directory: room for it there too (the review of #226).
		if st.Archive != nil {
			hostFree, err := freeBytes(j.dir)
			if err != nil {
				return fmt.Errorf("the room in %s for the check of the archive's database cannot be told (%v)", j.dir, err)
			}
			if need := st.Archive.DBSize + mib100; hostFree < need {
				return fmt.Errorf("no room in %s for the check of the archive's database: %d MiB free, %d MiB needed", j.dir, hostFree>>20, need>>20)
			}
		}
	}
	return nil
}

// hostBinaryBack says whether the host's binary can be put back by a
// container's way back: the run replaced it, the install path holds what
// the run left there, its copy answers, and nothing newer runs from it (#35
// (a), #38). When it cannot, the container goes back all the same and the
// binary stays, said (the review of #226): the container is what the way
// back is for.
func (j *job) hostBinaryBack(ctx context.Context) error {
	r, st := j.r, j.st
	b := st.Binary
	if b == nil || b.Old == "" || !st.has(stepBinaryReplacing) || st.has(stepBackBinary) {
		return errNoBinaryBack
	}
	if err := j.pathAsRecorded(ctx); err != nil {
		return err
	}
	if v, err := r.deps.Version(ctx, b.Old); err != nil || v != b.From {
		return fmt.Errorf("the copy %s does not answer %s", b.Old, b.From)
	}
	return r.serversOn(ctx, b.Path, b.From, false)
}

// backContainer is a container's way back (Decision 11, #47): the old
// container started again when the new one never ran, else the archive
// restored into a new volume, the new container set aside and the old image
// run on the restored volume — each move recorded, so a way back cut short
// is taken up where it stopped.
func (j *job) backContainer(ctx context.Context) wentBack {
	r, st := j.r, j.st
	cs := st.Container
	docker := r.deps.Docker
	if docker == nil {
		return j.fail("docker is not on PATH; nothing was touched")
	}
	if err := j.loadContainer(); err != nil {
		return j.fail("the run's container.json or image.json does not read (" + err.Error() + "); nothing was touched")
	}
	restoreBytes := int64(-1)
	if st.has(stepStarted) && !st.has(stepBackVolume) {
		if st.Archive == nil {
			return j.fail(fmt.Sprintf("tracepad %s ran on the volume %s, and the run records no archive of it to restore; nothing was touched", st.To, cs.Volume))
		}
		restoreBytes = st.Archive.Bytes
	}
	if err := j.containerBackPreconditions(ctx, restoreBytes, 0); err != nil {
		return j.fail(err.Error() + "; nothing was touched")
	}
	if !st.has(stepRenamedOld) {
		return j.startOld(ctx)
	}
	if !st.has(stepStarted) {
		return j.renameBack(ctx)
	}
	if out, ok := j.restoreVolume(ctx); !ok {
		return out
	}
	if out, ok := j.setNewAside(ctx); !ok {
		return out
	}
	vol := cs.Volume + "-" + st.Run
	fresh := false
	running, out, ok := j.runs(ctx, cs.BackID)
	if !ok {
		return out
	}
	if !running || !st.has(stepBackStarted) {
		if fresh, out, ok = j.runOld(ctx, vol); !ok {
			return out
		}
	}
	j.backHostBinary(ctx)
	return j.checkBackContainer(ctx, cs.BackID, fresh)
}

// runs says whether the container id runs, as a way back asks before it
// acts: a docker that cannot say refuses, with nothing touched.
func (j *job) runs(ctx context.Context, id string) (bool, wentBack, bool) {
	ok, err := j.r.containerRunning(ctx, id, false)
	if err != nil {
		return false, j.fail(fmt.Sprintf("docker cannot say whether %s runs (%s); nothing was touched", j.st.Container.Name, firstLine(err.Error()))), false
	}
	return ok, wentBack{}, true
}

// runOld runs the old image under the run's name — on vol, the restored
// volume, or on its own when vol is "" — or starts again the container an
// earlier attempt ran (BackID). It answers whether it made a container. A
// docker run that answers an error after its container started is a start,
// read as settle reads it, and the run's own: recorded, never "someone
// else's" to the next attempt, which refused for good (the review of #226).
func (j *job) runOld(ctx context.Context, vol string) (bool, wentBack, bool) {
	r, cs := j.r, j.st.Container
	j.begin()
	ref, err := j.oldRef(ctx)
	if err != nil {
		return false, j.fail(err.Error() + "; nothing was started"), false
	}
	if out, ok := j.clearName(ctx, cs.BackID); !ok {
		return false, out, false
	}
	on := ""
	if vol != "" {
		on = " on " + vol
	}
	fresh := false
	var made []byte
	err = j.act(stepBackStarted, "run "+ref+" as "+cs.Name+on, func() error {
		if cs.BackID != "" {
			if c, err := r.inspectOne(ctx, cs.BackID); err != nil {
				return err
			} else if c != nil {
				_, err := r.deps.Docker.Run(ctx, "start", cs.BackID)
				return err
			}
		}
		out, err := r.deps.Docker.Run(ctx, runArgs(j.ctr, cs.Name, ref, filepath.Join(j.dir, "env"), vol, j.st.Run, stepBackStarted)...)
		made, fresh = out, err == nil
		return err
	})
	if err != nil {
		if started, serr := j.settleContainer(ctx, stepBackStarted); serr == nil && started {
			j.step(stepBackStarted)
			j.done("%s answered an error and started %s all the same", ref, cs.Name)
			return false, wentBack{}, true
		}
		return false, j.fail(fmt.Sprintf("%s did not start%s (%s); run --back again", j.st.From, on, firstLine(err.Error()))), false
	}
	if fresh {
		cs.BackID = strings.TrimSpace(string(made))
		j.done("ran %s as %s%s; the volume %s keeps what the new version left", ref, cs.Name, on, cs.Volume)
	} else {
		j.done("started %s again%s", cs.Name, on)
	}
	j.step(stepBackStarted)
	return fresh, wentBack{}, true
}

// startOld starts the old container again, under its name, with the restart
// policy the stop set to no put back: a run stopped, or asked to stop, and
// renamed nothing. One whose stop timed out runs still, and is kept.
func (j *job) startOld(ctx context.Context) wentBack {
	r, cs := j.r, j.st.Container
	if j.st.has(stepBackStarted) {
		running, out, ok := j.runs(ctx, cs.ID)
		if !ok {
			return out
		}
		if running {
			return j.checkBackContainer(ctx, cs.ID, false)
		}
	}
	// Removed by the person since — after a run cut short at its rename,
	// say: the old image runs again from the run's record, on the volume the
	// new version never ran on (the review of #226).
	if c, err := r.inspectOne(ctx, cs.ID); err != nil {
		return j.fail(fmt.Sprintf("docker cannot say what %s is (%s); nothing was touched", cs.ID[:12], firstLine(err.Error())))
	} else if c == nil {
		fresh, out, ok := j.runOld(ctx, "")
		if !ok {
			return out
		}
		return j.checkBackContainer(ctx, cs.BackID, fresh)
	}
	j.begin()
	err := j.act(stepBackStarted, "start "+cs.Name+" again with its restart policy "+cs.Restart, func() error {
		if err := r.setPolicy(ctx, cs.ID, cs.Restart); err != nil {
			return err
		}
		_, err := r.deps.Docker.Run(ctx, "start", cs.ID)
		return err
	})
	if err != nil {
		return j.fail(fmt.Sprintf("%s did not start again (%s); run --back again", cs.Name, firstLine(err.Error())))
	}
	cs.BackID = cs.ID
	j.step(stepBackStarted)
	j.done("started %s again, with its restart policy %s", cs.Name, cs.Restart)
	return j.checkBackContainer(ctx, cs.ID, false)
}

// renameBack puts the old container back under its name and starts it: the
// run renamed it aside, and the new one never ran on its volume. A container
// the failed run left under the name is set aside first; one the person
// removed since is a fact, and the old image runs again from the run's
// record, on the volume nothing else ran on (the third review).
func (j *job) renameBack(ctx context.Context) wentBack {
	r, st := j.r, j.st
	cs := st.Container
	if st.has(stepBackStarted) {
		// Taken up after it ran: started again if it was stopped since.
		running, out, ok := j.runs(ctx, cs.BackID)
		if !ok {
			return out
		}
		if !running {
			if is, err := j.nameIs(ctx, cs.BackID); err != nil || !is {
				return j.fail("the container named " + cs.Name + " is not the one this run's way back started; nothing was touched")
			}
			j.begin()
			err := j.act(stepBackStarted, "start "+cs.Name+" again", func() error {
				_, err := r.deps.Docker.Run(ctx, "start", cs.BackID)
				return err
			})
			if err != nil {
				return j.fail(fmt.Sprintf("%s did not start again (%s); run --back again", cs.Name, firstLine(err.Error())))
			}
			j.step(stepBackStarted)
			j.done("started %s again", cs.Name)
		}
		return j.checkBackContainer(ctx, cs.BackID, false)
	}
	before, err := r.inspectOne(ctx, j.before())
	if err != nil {
		return j.fail(fmt.Sprintf("docker cannot say what %s is (%s); nothing was touched", j.before(), firstLine(err.Error())))
	}
	named, err := j.nameIs(ctx, cs.ID)
	if err != nil {
		return j.fail(fmt.Sprintf("docker cannot say what %s is (%s); nothing was touched", cs.Name, firstLine(err.Error())))
	}
	j.begin()
	if before == nil && !named {
		// Removed by the person since: the old image runs again, as the
		// run recorded it was created, on the volume nothing else ran on.
		fresh, out, ok := j.runOld(ctx, "")
		if !ok {
			return out
		}
		j.done("%s was gone; the old image runs as %s again", j.before(), cs.Name)
		return j.checkBackContainer(ctx, cs.BackID, fresh)
	}
	if before != nil {
		if before.ID != cs.ID {
			return j.fail("the container named " + j.before() + " is not the one this run renamed; nothing was touched")
		}
		if out, ok := j.clearName(ctx, ""); !ok {
			return out
		}
	}
	err = j.act(stepBackStarted, "rename "+j.before()+" back to "+cs.Name+" and start it with its restart policy "+cs.Restart, func() error {
		if before != nil {
			if _, err := r.deps.Docker.Run(ctx, "rename", j.before(), cs.Name); err != nil {
				return err
			}
			st.SetAside = slices.DeleteFunc(st.SetAside, func(s string) bool { return s == "container "+j.before() })
		}
		if err := r.setPolicy(ctx, cs.ID, cs.Restart); err != nil {
			return err
		}
		_, err := r.deps.Docker.Run(ctx, "start", cs.ID)
		return err
	})
	if err != nil {
		return j.fail(fmt.Sprintf("%s could not be put back and started (%s); run --back again", cs.Name, firstLine(err.Error())))
	}
	cs.BackID = cs.ID
	j.step(stepBackStarted)
	j.done("renamed %s back to %s, with its restart policy %s, and started it", j.before(), cs.Name, cs.Restart)
	return j.checkBackContainer(ctx, cs.ID, false)
}

// restoreVolume restores the archive into a new volume, <vol>-<run>, owned
// as the old volume's root is: before anything that runs is touched, so a
// restore that fails leaves the new container running on its volume. The
// intent stays until the volume is full: a volume an earlier attempt made is
// the run's, and is filled again (#34).
func (j *job) restoreVolume(ctx context.Context) (wentBack, bool) {
	r, st := j.r, j.st
	cs := st.Container
	if st.has(stepBackVolume) {
		return wentBack{}, true
	}
	vol := cs.Volume + "-" + st.Run
	ours := st.Pending != nil && st.Pending.Step == stepBackVolume
	archive := filepath.Join(j.dir, "data.tar.gz")
	if err := checkArchive(ctx, archive, *st.Archive, j.dir); err != nil {
		return j.fail(err.Error() + "; nothing was touched"), false
	}
	if is, err := j.nameIs(ctx, cs.NewID); err != nil {
		return j.fail(fmt.Sprintf("docker cannot say what %s is (%s); nothing was touched", cs.Name, firstLine(err.Error()))), false
	} else if !is {
		if c, err := r.inspectOne(ctx, cs.Name); err != nil || c != nil {
			return j.fail("the container named " + cs.Name + " now is not the one this run started: a later run's, or one made since. Go back from the run that made it first; nothing was touched"), false
		}
	}
	if err := j.intend(stepBackVolume, "make the volume %s and restore the archive into it", vol); err != nil {
		panic(unrecorded{err: err})
	}
	if err := j.at(stepBackVolume); err != nil {
		if !ours {
			j.drop()
		}
		return j.fail("the volume " + vol + " was not made (" + err.Error() + "); nothing was touched"), false
	}
	if _, err := r.deps.Docker.Run(ctx, "volume", "inspect", vol); err != nil {
		if _, err := r.deps.Docker.Run(ctx, "volume", "create", vol); err != nil {
			if !ours {
				j.drop()
			}
			return j.fail("the volume " + vol + " could not be made (" + firstLine(err.Error()) + "); nothing was touched"), false
		}
	}
	_, err := r.deps.Docker.Run(ctx, "run", "--rm",
		"--mount", csvField("type=volume", "src="+vol, "dst=/data"),
		"--mount", csvField("type=volume", "src="+cs.Volume, "dst=/old", "readonly"),
		"--mount", csvField("type=bind", "src="+j.dir, "dst=/backup", "readonly"),
		busybox, "sh", "-c", `tar xzf /backup/data.tar.gz -C /data && chown -R "$(stat -c %u:%g /old)" /data && test -s /data/`+dataDBName)
	if err != nil {
		return j.fail("the restore into the new volume " + vol + " failed (" + firstLine(err.Error()) + "); the new container still runs on " + cs.Volume + ", untouched. " +
			"The volume is this run's: run --back again, and it is filled again"), false
	}
	// Only now is the old volume what the way back sets aside: a failed
	// restore leaves it the live one (the review of #1).
	st.SetAside = append(st.SetAside, "volume "+cs.Volume)
	j.step(stepBackVolume)
	j.done("restored the archive into a new volume, %s, owned as %s is", vol, cs.Volume)
	return wentBack{}, true
}

// setNewAside stops the new container and renames it <name>-after-<run>,
// restart policy no. One the person removed since is nothing to set aside
// (the third review).
func (j *job) setNewAside(ctx context.Context) (wentBack, bool) {
	r, st := j.r, j.st
	cs := st.Container
	if st.has(stepBackAside) {
		return wentBack{}, true
	}
	j.begin()
	err := j.act(stepBackAside, "stop the container "+cs.Name+" and rename it to "+j.after()+", restart policy no", func() error {
		if is, err := j.nameIs(ctx, cs.NewID); err != nil {
			return err
		} else if is {
			if err := r.askToStop(ctx, cs.Name); err != nil {
				return err
			}
			if !r.waitStopped(ctx, cs.NewID) {
				return fmt.Errorf("%s was asked to stop and has not in %s; run --back again once it has", cs.Name, r.deps.StopWait)
			}
			if _, err := r.deps.Docker.Run(ctx, "rename", cs.Name, j.after()); err != nil {
				return err
			}
		}
		c, err := r.inspectOne(ctx, j.after())
		switch {
		case err != nil:
			return err
		case c == nil:
			j.done("the new container is gone already (removed since the upgrade); nothing to set aside")
			return nil
		}
		if err := r.setPolicy(ctx, j.after(), "no"); err != nil {
			return err
		}
		st.SetAside = append(st.SetAside, "container "+j.after())
		j.done("stopped the new container and set it aside as %s, restart policy no", j.after())
		return nil
	})
	if err != nil {
		return j.fail(fmt.Sprintf("the new container was not set aside (%s); nothing was started; run --back again", firstLine(err.Error()))), false
	}
	j.step(stepBackAside)
	return wentBack{}, true
}

// backHostBinary puts the host's binary back when the run replaced it. One
// that cannot be put back is a note: the container is what the way back is
// for, and the next --back puts the binary back.
func (j *job) backHostBinary(ctx context.Context) {
	st := j.st
	b := st.Binary
	if err := j.hostBinaryBack(ctx); errors.Is(err, errNoBinaryBack) {
		return
	} else if err != nil {
		j.rep.Notes = append(j.rep.Notes, fmt.Sprintf("tracepad %s was not put back at %s: %v; it stays as it is", b.From, b.Path, err))
		return
	}
	err := j.act(stepBackBinary, "put tracepad "+b.From+" back at "+b.Path, func() error {
		return j.restoreBinary(ctx, b.Old, b.From)
	})
	if err != nil {
		j.rep.Notes = append(j.rep.Notes, fmt.Sprintf("tracepad %s was not put back at %s (%v); run --back again to put it back", b.From, b.Path, err))
		return
	}
	j.step(stepBackBinary)
}

// oldRef is how the old image is named to run it again: its tag while that
// still names the old image, else the image's id (the review of #1).
//
// Never the bare ID: a container run from it is no longer one of the image's
// to the plan, and drops out of every later upgrade (the review of #226).
// The tag while it still names the old image; else the release's own tag,
// <repo>:<from>, when that does; else the image's digest in its repository,
// <repo>@sha256:…; else the release's tag given to the old image here.
func (j *job) oldRef(ctx context.Context) (string, error) {
	r, cs := j.r, j.st.Container
	repo, _ := imageRepo(cs.OldRef) // ignored: the run's own reference, of the image's repositories by the plan's rule
	names := func(ref string) (bool, error) {
		out, err := r.deps.Docker.Run(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
		switch {
		case err == nil:
			return strings.TrimSpace(string(out)) == cs.OldImage, nil
		case strings.Contains(err.Error(), "No such image"):
			return false, nil
		}
		return false, fmt.Errorf("docker cannot say which image %s names (%s)", ref, firstLine(err.Error()))
	}
	release := repo + ":" + j.st.From
	for _, ref := range []string{cs.OldRef, release} {
		if ok, err := names(ref); err != nil || ok {
			return ref, err
		}
	}
	taken := func(ref string) (bool, error) {
		_, err := r.deps.Docker.Run(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
		switch {
		case err == nil:
			return true, nil
		case strings.Contains(err.Error(), "No such image"):
			return false, nil
		}
		return false, fmt.Errorf("docker cannot say which image %s names (%s)", ref, firstLine(err.Error()))
	}
	out, err := r.deps.Docker.Run(ctx, "image", "inspect", "--format", "{{json .RepoDigests}}", cs.OldImage)
	if err != nil {
		return "", fmt.Errorf("docker cannot say the digests of %s (%s)", cs.OldImage, firstLine(err.Error()))
	}
	var digests []string
	if json.Unmarshal(out, &digests) == nil {
		for _, d := range digests {
			if r, ok := imageRepo(d); ok && r == repo {
				return d, nil
			}
		}
	}
	// A tag of the person's is never moved (the review of #226): the
	// release's when it names nothing, else one of the run's own.
	tag := release
	if busy, err := taken(release); err != nil {
		return "", err
	} else if busy {
		tag = release + "-before-" + j.st.Run
		if ok, err := names(tag); err != nil || ok {
			return tag, err
		}
		if busy, err := taken(tag); err != nil {
			return "", err
		} else if busy {
			return "", fmt.Errorf("the old image %s has no name of the release's to run it by, and %s and %s name other images", cs.OldImage, release, tag)
		}
	}
	if _, err := r.deps.Docker.Run(ctx, "tag", cs.OldImage, tag); err != nil {
		return "", fmt.Errorf("the old image %s has no name of the release's to run it by, and could not be given %s (%s)", cs.OldImage, tag, firstLine(err.Error()))
	}
	// A name of the run's own is the run's to say: set aside, with the
	// command that removes it (the review of #226).
	if item := "image " + tag; tag != release && !slices.Contains(j.st.SetAside, item) {
		j.st.SetAside = append(j.st.SetAside, item)
		j.persist()
	}
	return tag, nil
}

// nameIs says whether the container under the run's name is the one with
// this id.
func (j *job) nameIs(ctx context.Context, id string) (bool, error) {
	c, err := j.r.inspectOne(ctx, j.st.Container.Name)
	return err == nil && c != nil && c.ID == id, err
}

// clearName sets aside, as <name>-failed-<run> with restart policy no, a
// container left under the name the way back is about to run: one a docker
// run made and could not start (a port still taken), which would otherwise
// make every later attempt fail on the name (the third review). A container
// under the name that runs, and is not the run's own (keep), stops the way
// back with nothing touched.
func (j *job) clearName(ctx context.Context, keep string) (wentBack, bool) {
	r, cs := j.r, j.st.Container
	c, err := r.inspectOne(ctx, cs.Name)
	switch {
	case err != nil:
		return j.fail(fmt.Sprintf("docker cannot say what %s is (%s); nothing was started", cs.Name, firstLine(err.Error()))), false
	case c == nil, keep != "" && c.ID == keep:
		return wentBack{}, true
	case c.State.Running:
		return j.fail("a container named " + cs.Name + " runs, and it is not this run's; nothing was started"), false
	}
	name := j.failed()
	for i := 2; ; i++ {
		other, err := r.inspectOne(ctx, name)
		if err != nil {
			return j.fail(fmt.Sprintf("docker cannot say what %s is (%s); nothing was started", name, firstLine(err.Error()))), false
		}
		if other == nil {
			break
		}
		name = j.failed() + "-" + strconv.Itoa(i)
	}
	if _, err := r.deps.Docker.Run(ctx, "rename", c.ID, name); err != nil {
		return j.fail("the container left as " + cs.Name + " could not be set aside: " + firstLine(err.Error())), false
	}
	j.st.SetAside = append(j.st.SetAside, "container "+name)
	j.persist()
	if err := r.setPolicy(ctx, name, "no"); err != nil {
		j.rep.Notes = append(j.rep.Notes, fmt.Sprintf("%s, set aside, keeps its restart policy (%s): docker update --restart no %s", name, firstLine(err.Error()), shq(name)))
	}
	j.done("set the container left as %s aside as %s, restart policy no", cs.Name, name)
	return wentBack{}, true
}

// checkArchive reads a container's archive back against what the run
// recorded and checks its database on the host — one pass over the archive,
// which writes the database's files into a directory of the command's own,
// removed after.
func checkArchive(ctx context.Context, archive string, want Archived, runDir string) error {
	check := filepath.Join(runDir, "check-"+strconv.Itoa(os.Getpid()))
	if err := os.Mkdir(check, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(check) // ignored: the command's own copy in the run's directory; a leftover is named for this pid and removed by none
	if _, err := readBackInto(archive, want, check); err != nil {
		return err
	}
	if err := quickCheck(ctx, filepath.Join(check, dataDBName)); err != nil {
		return fmt.Errorf("the archive's database fails its check (%v)", err)
	}
	return nil
}

// checkBackContainer checks the old version a way back started, or found
// running.
func (j *job) checkBackContainer(ctx context.Context, id string, fresh bool) wentBack {
	if err := j.at(stepBackDone); err != nil {
		return wentBack{ok: true, unconfirmed: true, why: "its check failed: " + err.Error()}
	}
	cs := j.st.Container
	c := j.r.checkContainer(ctx, cs.URL, j.st.From, j.st.before(), id, fresh)
	j.rep.BackCheck = &c
	return checked(j.st.From+" started again but is not healthy", c)
}

// everStarted: a container docker has started at least once.
func everStarted(c *inspectContainer) bool {
	return c.State.Running || (c.State.StartedAt != "" && !strings.HasPrefix(c.State.StartedAt, "0001-01-01"))
}

// settleContainer says whether a container act is done, from what docker
// says (#34). A docker that does not answer says nothing either way, and the
// settling refuses (the eighth review: a failed inspect was read as "never
// started", and the intent dropped).
func (j *job) settleContainer(ctx context.Context, step string) (bool, error) {
	r, st := j.r, j.st
	cs := st.Container
	if r.deps.Docker == nil {
		return false, errors.New("docker is not on PATH")
	}
	under, err := r.inspectOne(ctx, cs.Name)
	if err != nil {
		return false, err
	}
	switch step {
	case stepRenamedOld:
		before, err := r.inspectOne(ctx, j.before())
		switch {
		case err != nil:
			return false, err
		case before != nil && before.ID == cs.ID:
			if item := "container " + j.before(); !slices.Contains(st.SetAside, item) {
				st.SetAside = append(st.SetAside, item)
			}
			return true, nil
		case under != nil && under.ID == cs.ID:
			return false, nil
		}
		// Under neither name: removed since, by the person — the rename's
		// outcome no longer matters, and the way back runs the old image
		// again from the run's record (the review of #226).
		if gone, err := r.inspectOne(ctx, cs.ID); err != nil {
			return false, err
		} else if gone == nil {
			return false, nil
		}
		return false, fmt.Errorf("the container %s is neither %s nor %s", cs.ID[:12], cs.Name, j.before())
	case stepStarted:
		// Run, and started: the new version may have written the volume.
		// One docker made and could not start is no start.
		if under != nil && under.ID != cs.ID && under.Config.Image == cs.NewRef && j.made(under, stepStarted) && everStarted(under) {
			cs.NewID = under.ID
			return true, nil
		}
	case stepBackVolume:
		// Kept: the way back fills a volume it began again, and knows it
		// for the run's by this intent.
		return false, errKeep
	case stepBackStarted:
		// The old container again, running; or the old image run on the
		// restored volume, and started. One docker made and could not start
		// is set aside by the next attempt.
		switch {
		case under == nil:
		case under.ID == cs.ID && under.State.Running:
			cs.BackID = cs.ID
			return true, nil
		case under.ID != cs.ID && under.ID != cs.NewID && under.Image == cs.OldImage && j.made(under, stepBackStarted) && everStarted(under):
			cs.BackID = under.ID
			return true, nil
		}
	}
	return false, nil
}

// made says whether a container is one this run made for step, by its
// labels (runLabel): only such a one is adopted.
func (j *job) made(c *inspectContainer, step string) bool {
	return c.Config.Labels[runLabel] == j.st.Run && c.Config.Labels[runLabel+".step"] == step
}

// errKeep: the intent stays pending, for the act's own path to take up.
var errKeep = errors.New("kept")

// errNoBinaryBack: the run has no host binary to put back.
var errNoBinaryBack = errors.New("no binary to put back")
