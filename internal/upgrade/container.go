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
)

// A container's run (spec 054 #47): the swap and the way back of Decision
// 10 and 11, on the machine of #31 and #34 — every act written as an intent
// first, settled from what docker says on the next load, and a way back that
// checks what it needs before its first act.

// recreated is a container run's run.json: the container's run as
// createdAs wrote it — the plan's own words for it — and the names of the
// variables it was given, whose values are in env beside it.
type recreated struct {
	Run      []string `json:"run"`
	EnvNames []string `json:"env_names"`
}

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
	if _, err := r.deps.Docker.Run(ctx, "image", "inspect", busybox); err != nil {
		if _, err := r.deps.Docker.Run(ctx, "pull", "-q", busybox); err != nil {
			return 0, "could not pull " + busybox + ", which archives the volume: " + firstLine(err.Error())
		}
	}
	j.ctr = now
	raw, err := json.Marshal(now.inspect.raw)
	if err == nil {
		err = writeFileAtomic(filepath.Join(j.dir, "container.json"), raw)
	}
	if err == nil {
		err = writeJSON(filepath.Join(j.dir, "run.json"), recreated{Run: now.Run, EnvNames: now.EnvNames})
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
	kb, err := r.busyboxKB(ctx, now.Volume, "du", "-sk", "/data")
	if err != nil {
		return 0, "could not measure the volume " + now.Volume + ": " + firstLine(err.Error())
	}
	hc := now.inspect.HostConfig
	st.Container = &ContainerState{Name: now.Name, ID: now.inspect.ID, Volume: now.Volume, URL: now.URL,
		OldRef: now.Ref, OldImage: now.inspect.Image, NewRef: newRef,
		Restart: restartArg(hc.RestartPolicy.Name, hc.RestartPolicy.MaximumRetryCount)}
	return kb << 10, ""
}

// busyboxKB runs a measure of a volume — `du -sk` of what it holds, or `df
// -Pk` of the room on its file system — from a busybox container, and answers
// the kilobytes.
func (r *runner) busyboxKB(ctx context.Context, volume string, measure ...string) (int64, error) {
	args := append([]string{"run", "--rm", "--mount", csvField("type=volume", "src="+volume, "dst=/data", "readonly"), busybox}, measure...)
	out, err := r.deps.Docker.Run(ctx, args...)
	if err != nil {
		return 0, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	field := 0
	if measure[0] == "df" {
		// Filesystem 1024-blocks Used Available Capacity Mounted-on
		field = 3
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) <= field {
		return 0, fmt.Errorf("%s said %q", measure[0], strings.TrimSpace(string(out)))
	}
	return strconv.ParseInt(f[field], 10, 64)
}

// loadContainer reads what a way back recreates the container from: its
// inspect and its run, as the upgrade wrote them.
func (j *job) loadContainer() error {
	if j.ctr.Run != nil {
		return nil
	}
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
	b, err = os.ReadFile(filepath.Join(j.dir, "run.json"))
	if err != nil {
		return err
	}
	var rc recreated
	if err := json.Unmarshal(b, &rc); err != nil {
		return err
	}
	if ic.ID != j.st.Container.ID || !slices.Contains(rc.Run, imageSlot) {
		return errors.New("they are not this run's container's")
	}
	j.ctr = Container{Name: j.st.Container.Name, Volume: j.st.Container.Volume, Run: rc.Run, EnvNames: rc.EnvNames, inspect: ic}
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

	args := runArgs(j.ctr, cs.Name, cs.NewRef, filepath.Join(j.dir, "env"), "")
	var out []byte
	err = j.act(stepStarted, "run "+cs.NewRef+" as "+cs.Name, func() (err error) {
		out, err = docker.Run(ctx, args...)
		return err
	})
	if err != nil {
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
	c := r.checkContainer(ctx, cs.URL, p.to, st.CountBefore, cs.NewID, true)
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
	var err error
	r.waitUntil(ctx, func() bool {
		if _, err = r.deps.Docker.Run(ctx, "kill", "--signal", "TERM", name); err == nil {
			return true
		}
		c, ierr := r.inspectOne(ctx, name)
		if ierr == nil && c != nil && !c.State.Running {
			err = nil
			return true
		}
		return false
	})
	return err
}

// setPolicy sets a container's restart policy, asking again for as long as a
// stop may take: docker refuses it now and then while its restart manager
// moves a crash-looping container between two of its states ("cannot update
// a stopped container", the real docker of the integration tests).
func (r *runner) setPolicy(ctx context.Context, ref, policy string) error {
	var err error
	r.waitUntil(ctx, func() bool {
		_, err = r.deps.Docker.Run(ctx, "update", "--restart", policy, ref)
		return err == nil
	})
	return err
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
func (r *runner) checkContainer(ctx context.Context, base, want string, before *int64, id string, fresh bool) Checked {
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
// what a restore takes, -1 when none is ahead; binary is whether the way
// back puts the host's binary back.
func (j *job) containerBackPreconditions(ctx context.Context, restoreBytes int64, binary bool) error {
	r, st := j.r, j.st
	cs := st.Container
	if _, err := r.deps.Docker.Run(ctx, "image", "inspect", cs.OldImage); err != nil {
		return fmt.Errorf("the old image %s is not there to run again (%s)", cs.OldImage, firstLine(err.Error()))
	}
	if restoreBytes >= 0 {
		if _, err := r.deps.Docker.Run(ctx, "image", "inspect", busybox); err != nil {
			if _, err := r.deps.Docker.Run(ctx, "pull", "-q", busybox); err != nil {
				return fmt.Errorf("%s, which restores the archive, is not there and could not be pulled (%s)", busybox, firstLine(err.Error()))
			}
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
		free, err := r.busyboxKB(ctx, cs.Volume, "df", "-Pk", "/data")
		if err != nil {
			return fmt.Errorf("the room for a restore of the volume %s cannot be told (%s)", cs.Volume, firstLine(err.Error()))
		}
		if need := restoreBytes + mib100; free<<10 < need {
			return fmt.Errorf("no room beside the volume %s for the restore a way back may need: %d MiB free, %d MiB needed", cs.Volume, free>>10, need>>20)
		}
	}
	// The host's binary, put back by the way back when the run replaced it:
	// the install path as the run left it, the copy answering, and nothing
	// newer running from it (#35 (a), #38; the eighth review: only when the
	// run replaced it).
	b := st.Binary
	if !binary || b == nil || b.Old == "" {
		return nil
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
		return j.fail("the run's container.json or run.json does not read (" + err.Error() + "); nothing was touched")
	}
	restoreBytes := int64(-1)
	if st.has(stepStarted) && !st.has(stepBackVolume) {
		if st.Archive == nil {
			return j.fail(fmt.Sprintf("tracepad %s ran on the volume %s, and the run records no archive of it to restore; nothing was touched", st.To, cs.Volume))
		}
		restoreBytes = st.Archive.Bytes
	}
	binary := st.has(stepBinaryReplacing) && !st.has(stepBackBinary)
	if err := j.wayBackPreconditions(ctx, restoreBytes, 0, binary); err != nil {
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
	running := false
	if st.has(stepBackStarted) {
		ok, err := r.containerRunning(ctx, cs.BackID, false)
		if err != nil {
			return j.fail(fmt.Sprintf("docker cannot say whether %s runs (%s); nothing was touched", cs.Name, firstLine(err.Error())))
		}
		running = ok
	}
	if !running {
		j.begin()
		ref, err := j.oldRef(ctx)
		if err != nil {
			return j.fail(err.Error() + "; nothing was started")
		}
		if out, ok := j.clearName(ctx, cs.BackID); !ok {
			return out
		}
		// The container an earlier attempt ran is started again; otherwise
		// the old image runs on the restored volume.
		var made []byte
		err = j.act(stepBackStarted, "run "+ref+" as "+cs.Name+" on "+vol, func() error {
			if cs.BackID != "" {
				if c, err := r.inspectOne(ctx, cs.BackID); err != nil {
					return err
				} else if c != nil {
					_, err := docker.Run(ctx, "start", cs.BackID)
					return err
				}
			}
			out, err := docker.Run(ctx, runArgs(j.ctr, cs.Name, ref, filepath.Join(j.dir, "env"), vol)...)
			made, fresh = out, err == nil
			return err
		})
		if err != nil {
			return j.fail(fmt.Sprintf("%s did not start on %s (%s); run --back again", st.From, vol, firstLine(err.Error())))
		}
		if fresh {
			cs.BackID = strings.TrimSpace(string(made))
			j.done("ran %s as %s on %s; the volume %s keeps what the new version left", ref, cs.Name, vol, cs.Volume)
		} else {
			j.done("started %s again on %s", cs.Name, vol)
		}
		j.step(stepBackStarted)
	}
	j.backHostBinary(ctx)
	return j.checkBackContainer(ctx, cs.BackID, fresh)
}

// startOld starts the old container again, under its name, with the restart
// policy the stop set to no put back: a run stopped, or asked to stop, and
// renamed nothing. One whose stop timed out runs still, and is kept.
func (j *job) startOld(ctx context.Context) wentBack {
	r, cs := j.r, j.st.Container
	if j.st.has(stepBackStarted) {
		ok, err := r.containerRunning(ctx, cs.ID, false)
		if err != nil {
			return j.fail(fmt.Sprintf("docker cannot say whether %s runs (%s); nothing was touched", cs.Name, firstLine(err.Error())))
		}
		if ok {
			return j.checkBackContainer(ctx, cs.ID, false)
		}
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
		ok, err := r.containerRunning(ctx, cs.BackID, false)
		if err != nil {
			return j.fail(fmt.Sprintf("docker cannot say whether %s runs (%s); nothing was touched", cs.Name, firstLine(err.Error())))
		}
		if !ok {
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
		// run recorded it was created.
		ref, err := j.oldRef(ctx)
		if err != nil {
			return j.fail(err.Error() + "; nothing was started")
		}
		if out, ok := j.clearName(ctx, ""); !ok {
			return out
		}
		var out []byte
		err = j.act(stepBackStarted, "run "+ref+" as "+cs.Name, func() (err error) {
			out, err = r.deps.Docker.Run(ctx, runArgs(j.ctr, cs.Name, ref, filepath.Join(j.dir, "env"), "")...)
			return err
		})
		if err != nil {
			return j.fail(fmt.Sprintf("%s did not start (%s); run --back again", st.From, firstLine(err.Error())))
		}
		cs.BackID = strings.TrimSpace(string(out))
		j.step(stepBackStarted)
		j.done("%s was gone; ran %s as %s again", j.before(), ref, cs.Name)
		return j.checkBackContainer(ctx, cs.BackID, true)
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
			st.SetAside = removeItem(st.SetAside, "container "+j.before())
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
	if _, err := readBack(archive, *st.Archive); err != nil {
		return j.fail(err.Error() + "; nothing was touched"), false
	}
	if is, err := j.nameIs(ctx, cs.NewID); err != nil {
		return j.fail(fmt.Sprintf("docker cannot say what %s is (%s); nothing was touched", cs.Name, firstLine(err.Error()))), false
	} else if !is {
		if c, err := r.inspectOne(ctx, cs.Name); err != nil || c != nil {
			return j.fail("the container named " + cs.Name + " now is not the one this run started: a later run's, or one made since. Go back from the run that made it first; nothing was touched"), false
		}
	}
	if err := checkArchiveDB(ctx, archive, j.dir); err != nil {
		return j.fail("the archive's database fails its check (" + err.Error() + "); nothing was touched"), false
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
	if b == nil || b.Old == "" || !st.has(stepBinaryReplacing) || st.has(stepBackBinary) {
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
func (j *job) oldRef(ctx context.Context) (string, error) {
	cs := j.st.Container
	out, err := j.r.deps.Docker.Run(ctx, "image", "inspect", "--format", "{{.Id}}", cs.OldRef)
	switch {
	case err == nil && strings.TrimSpace(string(out)) == cs.OldImage:
		return cs.OldRef, nil
	case err == nil, strings.Contains(err.Error(), "No such image"):
		return cs.OldImage, nil
	}
	return "", fmt.Errorf("docker cannot say which image %s names (%s)", cs.OldRef, firstLine(err.Error()))
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

// checkArchiveDB checks a container's archive's database on the host, from a
// copy of its database files the command makes and removes itself.
func checkArchiveDB(ctx context.Context, archive, runDir string) error {
	check := filepath.Join(runDir, "check-"+strconv.Itoa(os.Getpid()))
	if err := os.Mkdir(check, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(check) // ignored: the command's own copy in the run's directory; a leftover is named for this pid and removed by none
	if err := extractDB(archive, check); err != nil {
		return err
	}
	return quickCheck(ctx, filepath.Join(check, dataDBName))
}

// checkBackContainer checks the old version a way back started, or found
// running.
func (j *job) checkBackContainer(ctx context.Context, id string, fresh bool) wentBack {
	if err := j.at(stepBackDone); err != nil {
		return wentBack{ok: true, unconfirmed: true, why: "its check failed: " + err.Error()}
	}
	cs := j.st.Container
	c := j.r.checkContainer(ctx, cs.URL, j.st.From, j.st.CountBefore, id, fresh)
	j.rep.BackCheck = &c
	return checked(j.st.From+" started again but is not healthy", c)
}

func removeItem(list []string, item string) []string {
	out := list[:0]
	for _, s := range list {
		if s != item {
			out = append(out, s)
		}
	}
	return out
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
		return false, fmt.Errorf("the container %s is neither %s nor %s", cs.ID[:12], cs.Name, j.before())
	case stepStarted:
		// Run, and started: the new version may have written the volume.
		// One docker made and could not start is no start.
		if under != nil && under.ID != cs.ID && under.Config.Image == cs.NewRef && everStarted(under) {
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
		case under.ID != cs.ID && under.ID != cs.NewID && under.Image == cs.OldImage && everStarted(under):
			cs.BackID = under.ID
			return true, nil
		}
	}
	return false, nil
}

// errKeep: the intent stays pending, for the act's own path to take up.
var errKeep = errors.New("kept")
