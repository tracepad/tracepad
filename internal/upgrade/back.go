package upgrade

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// renameDir is os.Rename; a test's seam, to cut a way back short between
// its two moves.
var renameDir = os.Rename

// wentBack is how a way back ended.
type wentBack struct {
	ok bool
	// unconfirmed: the steps are done, but the old version has not shown it
	// is healthy (its check said decide); why says what it showed.
	unconfirmed bool
	why         string
}

// wayBack undoes what the run did, from the steps its state records, in
// reverse, and deletes nothing: what the new version left is set aside and
// the archive restored into a new place (Decision 11). It is the one path the
// upgrade takes on a failure after the stop and on not healthy, and the one
// `--back` takes.
func (j *job) wayBack(ctx context.Context) wentBack {
	st := j.st
	if st.has(stepBackDone) {
		return wentBack{ok: false, why: "this run's way back already ran"}
	}
	var out wentBack
	switch st.Kind {
	case kindProcess:
		out = j.backProcess(ctx)
	case kindContainer:
		out = j.backContainer(ctx)
	default:
		out = j.backBinary(ctx)
	}
	// Done only when the old version is seen healthy: a way back that could
	// not confirm it is taken up again by the next --back, which checks.
	if out.ok && !out.unconfirmed {
		_ = j.step(stepBackDone)
	}
	return out
}

func (j *job) fail(why string) wentBack { return wentBack{ok: false, why: why} }

// begin records that the way back is about to change what runs. A way back
// refused before this point touched nothing, and `--check` still works for
// the run (the review of #1).
func (j *job) begin() {
	if !j.st.has(stepBackBegun) {
		_ = j.step(stepBackBegun)
	}
}

// checked is the end of a way back that started the old version: done, and
// confirmed only when its check says healthy; failed, with prefix, when the
// old version is not healthy either.
func checked(prefix string, c Checked) wentBack {
	if c.Verdict == verdictNotHealthy {
		return wentBack{ok: false, why: prefix + ": " + c.Why}
	}
	return wentBack{ok: true, unconfirmed: c.Verdict != verdictHealthy, why: c.Why}
}

// restoreBinary puts the version want back at the install path when it is
// not there, from the copy in the run directory.
func (j *job) restoreBinary(ctx context.Context, copyPath, want string) error {
	path := j.st.Binary.Path
	if v, err := j.r.deps.Version(ctx, path); err == nil && v == want {
		return nil
	}
	if err := j.r.putInPlace(ctx, copyPath, path, want); err != nil {
		return err
	}
	j.done("put tracepad %s back at %s", want, path)
	return nil
}

func (j *job) backBinary(ctx context.Context) wentBack {
	b := j.st.Binary
	if !j.st.has(stepBinaryReplaced) {
		return wentBack{ok: true}
	}
	j.begin()
	if err := j.restoreBinary(ctx, b.Old, b.From); err != nil {
		return j.fail(fmt.Sprintf("%s could not be put back at %s: %v", b.From, b.Path, err))
	}
	return wentBack{ok: true}
}

func (j *job) backProcess(ctx context.Context) wentBack {
	r, st := j.r, j.st
	ps := st.Process
	if j.spec.Exe == "" {
		spec, err := readSpec(j.dir)
		if err != nil {
			return j.fail("the run's server.json does not read: " + err.Error())
		}
		j.spec = spec
	}
	if v, err := r.deps.Version(ctx, ps.Old); err != nil || v != st.From {
		return j.fail(fmt.Sprintf("the copy %s does not answer %s; nothing was touched", ps.Old, st.From))
	}
	if !st.has(stepStopSent) {
		return wentBack{ok: true}
	}
	after, restore := ps.DataDir+".after-"+st.Run, ps.DataDir+".restore-"+st.Run

	// A way back that stopped half way is taken up where it stopped, from
	// the steps it recorded (the second review): never refused for good, and
	// never with advice to remove what the new version wrote.
	if st.has(stepBackAside) && !st.has(stepBackMoved) {
		if _, err := os.Lstat(ps.DataDir); err == nil {
			return j.fail(fmt.Sprintf("%s is set aside as %s, and something is at %s again; nothing was touched", ps.DataDir, after, ps.DataDir))
		}
		if err := renameDir(restore, ps.DataDir); err != nil {
			return j.fail(fmt.Sprintf("the restore in %s could not take the place of %s (%v); run --back again once it can", restore, ps.DataDir, err))
		}
		_ = j.step(stepBackMoved)
		j.done("put the restore in the place of %s", ps.DataDir)
	}

	if !st.has(stepBackMoved) {
		// The old server may still be on its way out: its stop timed out.
		// It is waited for, and started again once it has gone.
		if alive, _ := r.isServer(ps.PID, ps.DataDir, j.spec); alive {
			if !r.waitGone(ctx, ps.PID) {
				return j.fail(fmt.Sprintf("server pid %d is still shutting down from its stop; run --back again once it has exited. Nothing was touched", ps.PID))
			}
		}
		// Whatever holds the data directory now — whoever started it, this
		// run, a later run's way back, the person — may be stopped only when
		// it is this directory's server as the run recorded it. Anything else
		// stops the way back before it touches a thing.
		holder := 0
		if lockHeld(ps.DataDir) {
			holder, _ = lockedBy(ps.DataDir)
			ok, err := r.isServer(holder, ps.DataDir, j.spec)
			if !ok {
				why := fmt.Sprintf("pid %d", holder)
				if err != nil {
					why = err.Error()
				}
				return j.fail(fmt.Sprintf("pid %d holds %s, and it is not this directory's server as the run recorded it: %s. Stop it first; nothing was touched", holder, ps.DataDir, why))
			}
		}
		if st.has(stepStarted) {
			if out, ok := j.swapBack(ctx, holder, after, restore); !ok {
				return out
			}
		} else if holder > 0 {
			// Nothing ran on the data, and its server runs: check it.
			ps.BackPID = holder
			_ = st.save(j.dir)
			return j.checkBack(ctx, j.isServerFn(holder), false)
		}
	}

	if !st.has(stepBackBinary) {
		j.begin()
		if err := j.restoreBinary(ctx, ps.Old, st.From); err != nil {
			return j.fail(fmt.Sprintf("%s could not be put back at %s (%v); nothing was started; run --back again", st.From, st.Binary.Path, err))
		}
		_ = j.step(stepBackBinary)
	}

	// Started already, by an earlier attempt: check it rather than start a
	// second one.
	if st.has(stepBackStarted) {
		if ok, _ := r.isServer(ps.BackPID, ps.DataDir, j.spec); ok {
			return j.checkBack(ctx, j.isServerFn(ps.BackPID), false)
		}
	}
	if lockHeld(ps.DataDir) {
		holder, _ := lockedBy(ps.DataDir)
		return j.fail(fmt.Sprintf("pid %d took %s meanwhile; nothing was started", holder, ps.DataDir))
	}
	j.begin()
	started, err := j.start()
	if err != nil {
		return j.fail(fmt.Sprintf("%s did not start (%v); run --back again", st.From, err))
	}
	ps.BackPID = started.PID()
	_ = j.step(stepBackStarted)
	j.done("started %s again as pid %d, with its arguments and environment", st.From, ps.BackPID)
	return j.checkBack(ctx, running(started), true)
}

// swapBack restores the archive beside the data and checks it, stops the
// server that holds the data, and swaps the two directories, recording each
// move so that a way back cut short is taken up where it stopped.
func (j *job) swapBack(ctx context.Context, holder int, after, restore string) (wentBack, bool) {
	r, st := j.r, j.st
	ps := st.Process
	if st.Archive == nil {
		return j.fail("the run records no archive; nothing was touched"), false
	}
	archive := filepath.Join(j.dir, "data.tar.gz")
	if err := verifyArchive(archive, *st.Archive); err != nil {
		return j.fail(err.Error() + "; nothing was touched"), false
	}
	if _, err := os.Lstat(after); err == nil {
		return j.fail(after + " exists already, and this run did not put it there; nothing was touched"), false
	}
	if _, err := os.Lstat(restore); err == nil {
		return j.fail(restore + " exists already: a way back stopped half way and left it before it touched the server (then remove it: rm -r " + shq(restore) + "), or something else is there; nothing was touched"), false
	}
	info, err := os.Stat(ps.DataDir)
	if err != nil {
		return j.fail(err.Error()), false
	}
	if err := extractArchive(archive, restore, info.Mode().Perm()); err != nil {
		return j.fail(fmt.Sprintf("the restore into %s failed (%v); the server and %s are as they were. Remove what it left before going back again: rm -r %s", restore, err, ps.DataDir, shq(restore))), false
	}
	if err := quickCheck(ctx, filepath.Join(restore, dataDBName)); err != nil {
		return j.fail(fmt.Sprintf("the restored database fails its check (%v); it is in %s, and the server and %s are as they were", err, restore, ps.DataDir)), false
	}
	j.done("restored the archive into %s, and its database passes quick_check", restore)

	// Only now is the server stopped: a bad archive is found while it still
	// runs.
	j.begin()
	if holder > 0 {
		if ok, err := r.isServer(holder, ps.DataDir, j.spec); !ok {
			why := "it is gone"
			if err != nil {
				why = err.Error()
			}
			if r.deps.Sys.Alive(holder) || lockHeld(ps.DataDir) {
				return j.fail(why + "; nothing was stopped, and the restore waits in " + restore), false
			}
		} else if !r.stop(ctx, holder) {
			return j.fail(fmt.Sprintf("server pid %d was asked to stop and has not; the restore waits in %s; run --back again once it has exited", holder, restore)), false
		} else {
			j.done("stopped server pid %d", holder)
		}
	}
	if lockHeld(ps.DataDir) {
		return j.fail("something took " + ps.DataDir + " meanwhile; nothing was set aside, and the restore waits in " + restore), false
	}
	if err := renameDir(ps.DataDir, after); err != nil {
		return j.fail(fmt.Sprintf("%s could not be set aside (%v); nothing was started, and the restore waits in %s", ps.DataDir, err, restore)), false
	}
	st.SetAside = append(st.SetAside, after)
	_ = j.step(stepBackAside)
	if err := renameDir(restore, ps.DataDir); err != nil {
		return j.fail(fmt.Sprintf("the restore could not take the place of %s (%v); %s is set aside, and the restore waits in %s; run --back again to finish", ps.DataDir, err, after, restore)), false
	}
	_ = j.step(stepBackMoved)
	j.done("set %s aside as %s, and put the restore in its place", ps.DataDir, after)
	return wentBack{}, true
}

// waitGone waits, as a stop does, for a process already asked to stop.
func (r *runner) waitGone(ctx context.Context, pid int) bool {
	deadline := r.deps.Now().Add(r.deps.StopWait)
	for r.deps.Sys.Alive(pid) {
		if !r.deps.Now().Before(deadline) || r.deps.Sleep(ctx, 250*time.Millisecond) != nil {
			return false
		}
	}
	return true
}

func (j *job) isServerFn(pid int) func() bool {
	return func() bool {
		ok, _ := j.r.isServer(pid, j.st.Process.DataDir, j.spec)
		return ok
	}
}

// running is a started process's liveness, for a check.
func running(s Started) func() bool {
	exited := s.Exited()
	return func() bool {
		select {
		case <-exited:
			return false
		default:
			return true
		}
	}
}

// checkBack checks the old version a way back started.
func (j *job) checkBack(ctx context.Context, alive func() bool, fresh bool) wentBack {
	ps := j.st.Process
	c := j.r.check(ctx, ps.URL, j.st.From, j.st.CountBefore, alive)
	if fresh {
		c.LogLine = firstLogLine(ps.Log, ps.LogOffset)
	}
	j.rep.BackCheck = &c
	return checked(j.st.From+" started again but is not healthy", c)
}

func (j *job) backContainer(ctx context.Context) wentBack {
	r, st := j.r, j.st
	cs := st.Container
	docker := r.deps.Docker
	if docker == nil {
		return j.fail("docker is not on PATH")
	}
	if j.inspect.ID == "" {
		ic, img, err := readContainer(j.dir)
		if err != nil {
			return j.fail("the run's container.json does not read: " + err.Error())
		}
		j.inspect, j.image = ic, img
	}
	if !st.has(stepStopSent) {
		return wentBack{ok: true}
	}
	restorePolicy := func(name string) error {
		_, err := docker.Run(ctx, "update", "--restart", cs.Restart, name)
		return err
	}
	switch {
	case !st.has(stepRenamedOld):
		j.begin()
		if _, err := docker.Run(ctx, "start", cs.Name); err != nil {
			return j.fail(cs.Name + " did not start again: " + firstLine(err.Error()))
		}
		j.done("started %s again", cs.Name)
		return j.checkContainer(ctx, cs.ID, false)
	case !st.has(stepStarted):
		j.begin()
		if r.exists(ctx, "container", j.before()) {
			if r.exists(ctx, "container", cs.Name) {
				if _, err := docker.Run(ctx, "rename", cs.Name, j.failed()); err != nil {
					return j.fail("the container the failed run left as " + cs.Name + " could not be set aside: " + firstLine(err.Error()))
				}
				_, _ = docker.Run(ctx, "update", "--restart", "no", j.failed())
				st.SetAside = append(st.SetAside, "container "+j.failed())
				j.done("set the container the failed run made aside as %s, restart policy no", j.failed())
			}
			if _, err := docker.Run(ctx, "rename", j.before(), cs.Name); err != nil {
				return j.fail(j.before() + " could not be renamed back: " + firstLine(err.Error()))
			}
			st.SetAside = removeItem(st.SetAside, "container "+j.before())
			_ = st.save(j.dir)
		}
		if err := restorePolicy(cs.Name); err != nil {
			return j.fail("the restart policy of " + cs.Name + " could not be put back: " + firstLine(err.Error()) + "; run --back again")
		}
		if _, err := docker.Run(ctx, "start", cs.Name); err != nil {
			return j.fail(cs.Name + " did not start again: " + firstLine(err.Error()) + "; run --back again")
		}
		j.done("renamed %s back to %s, with its restart policy, and started it", j.before(), cs.Name)
		return j.checkContainer(ctx, cs.ID, false)
	}

	// The new version ran on the volume: restore the archive into a new one.
	// Each move is recorded, so a way back cut short is taken up where it
	// stopped (the second review).
	vol := cs.Volume + "-" + st.Run
	if !st.has(stepBackVolume) {
		if st.Archive == nil {
			return j.fail("the run records no archive; nothing was touched")
		}
		archive := filepath.Join(j.dir, "data.tar.gz")
		if err := verifyArchive(archive, *st.Archive); err != nil {
			return j.fail(err.Error() + "; nothing was touched")
		}
		switch {
		case r.exists(ctx, "volume", vol):
			return j.fail("the volume " + vol + " exists already, and this run did not fill it: a way back failed half way and left it (then remove it: docker volume rm " + shq(vol) + "), or something else is there; nothing was touched")
		case !st.has(stepBackAside) && r.exists(ctx, "container", j.after()):
			return j.fail("the container " + j.after() + " exists already; nothing was touched")
		case !r.exists(ctx, "container", j.before()):
			return j.fail("there is no " + j.before() + " to go back to; nothing was touched")
		}
		if list, err := r.inspectContainers(ctx, cs.Name); err == nil && len(list) == 1 && list[0].ID != cs.NewID {
			return j.fail("the container named " + cs.Name + " now is not the one this run started: a later run's, or one made since. " +
				"Go back from the run that made it first; nothing was touched")
		}
		if err := checkArchiveDB(ctx, archive, j.dir); err != nil {
			return j.fail("the archive's database fails its check (" + err.Error() + "); nothing was touched")
		}
		if _, err := docker.Run(ctx, "volume", "create", vol); err != nil {
			return j.fail("the volume " + vol + " could not be made: " + firstLine(err.Error()))
		}
		_, err := docker.Run(ctx, "run", "--rm",
			"--mount", csvField("type=volume", "src="+vol, "dst=/data"),
			"--mount", csvField("type=volume", "src="+cs.Volume, "dst=/old", "readonly"),
			"--mount", csvField("type=bind", "src="+j.dir, "dst=/backup", "readonly"),
			busybox, "sh", "-c", `tar xzf /backup/data.tar.gz -C /data && chown -R "$(stat -c %u:%g /old)" /data && test -s /data/tracepad.db`)
		if err != nil {
			return j.fail("the restore into the new volume " + vol + " failed (" + firstLine(err.Error()) + "); the new container still runs on " + cs.Volume + ", untouched. " +
				"Remove the half-made volume before going back again: docker volume rm " + shq(vol))
		}
		// Only now does the old volume become what the way back sets aside:
		// a failed restore leaves it the live one.
		st.SetAside = append(st.SetAside, "volume "+cs.Volume)
		_ = j.step(stepBackVolume)
		j.done("restored the archive into a new volume, %s, owned as %s is", vol, cs.Volume)
	}

	if !st.has(stepBackAside) {
		j.begin()
		if list, err := r.inspectContainers(ctx, cs.Name); err == nil && len(list) == 1 {
			if list[0].ID != cs.NewID {
				return j.fail("the container named " + cs.Name + " now is not the one this run started; the restored volume " + vol + " waits")
			}
			if _, err := docker.Run(ctx, "stop", "--time", strconv.Itoa(int(r.deps.StopWait.Seconds())), cs.Name); err != nil {
				return j.fail(cs.Name + " did not stop: " + firstLine(err.Error()) + "; run --back again")
			}
			if _, err := docker.Run(ctx, "rename", cs.Name, j.after()); err != nil {
				return j.fail(cs.Name + " could not be set aside: " + firstLine(err.Error()) + "; run --back again")
			}
		}
		if _, err := docker.Run(ctx, "update", "--restart", "no", j.after()); err != nil {
			return j.fail("the restart policy of " + j.after() + " could not be set to no: " + firstLine(err.Error()) + "; run --back again")
		}
		st.SetAside = append(st.SetAside, "container "+j.after())
		_ = j.step(stepBackAside)
		j.done("stopped the new container and set it aside as %s, restart policy no", j.after())
	}

	fresh := false
	if !st.has(stepBackStarted) || !r.containerRunning(ctx, cs.BackID, false) {
		j.begin()
		if cs.BackID != "" && r.exists(ctx, "container", cs.BackID) {
			if _, err := docker.Run(ctx, "start", cs.BackID); err != nil {
				return j.fail(st.From + " did not start on " + vol + ": " + firstLine(err.Error()) + "; run --back again")
			}
		} else {
			ref := cs.OldRef
			if img, err := r.inspectImage(ctx, ref); err != nil || img.ID != cs.OldImage {
				ref = cs.OldImage
			}
			out, err := docker.Run(ctx, runArgs(j.inspect, j.image, cs.Name, ref, filepath.Join(j.dir, "env"), vol)...)
			if err != nil {
				return j.fail(st.From + " did not start on " + vol + ": " + firstLine(err.Error()) + "; run --back again")
			}
			cs.BackID = strings.TrimSpace(string(out))
			fresh = true
			j.done("ran %s as %s on %s; the volume %s keeps what the new version left", ref, cs.Name, vol, cs.Volume)
		}
		_ = j.step(stepBackStarted)
	}
	if st.has(stepBinaryReplaced) && st.Binary.Old != "" && !st.has(stepBackBinary) {
		if err := j.restoreBinary(ctx, st.Binary.Old, st.Binary.From); err != nil {
			j.rep.Notes = append(j.rep.Notes, "the binary at "+st.Binary.Path+" was not put back: "+err.Error())
		} else {
			_ = j.step(stepBackBinary)
		}
	}
	return j.checkContainer(ctx, cs.BackID, fresh)
}

// checkArchiveDB checks a container's archive's database on the host, from a
// copy of its database files the command makes and removes itself.
func checkArchiveDB(ctx context.Context, archive, runDir string) error {
	check := filepath.Join(runDir, "check-"+strconv.Itoa(os.Getpid()))
	if err := os.Mkdir(check, 0o700); err != nil {
		return err
	}
	defer os.RemoveAll(check)
	if err := extractDB(archive, check); err != nil {
		return err
	}
	return quickCheck(ctx, filepath.Join(check, dataDBName))
}

func (j *job) checkContainer(ctx context.Context, id string, fresh bool) wentBack {
	cs := j.st.Container
	c := j.r.check(ctx, cs.URL, j.st.From, j.st.CountBefore, func() bool { return j.r.containerRunning(ctx, id, fresh) })
	c.LogLine = j.r.containerFirstLog(ctx, id)
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

// openRun loads a run for --check or --back.
func (r *runner) openRun(arg string, rep *Report) (*job, error) {
	dir, err := resolveRun(r.deps.Backups, arg)
	if err != nil {
		return nil, err
	}
	st, err := loadState(dir)
	if err != nil {
		return nil, err
	}
	rep.Run = &RunRef{ID: st.Run, Dir: dir}
	rep.From, rep.To = st.From, st.To
	return &job{r: r, rep: rep, st: st, dir: dir}, nil
}

// backMode is `--back RUN`.
func (r *runner) backMode(ctx context.Context) *Report {
	rep := &Report{Mode: "back"}
	j, err := r.openRun(r.flags.back, rep)
	if err != nil {
		rep.ExitCode, rep.Summary = exitRefused, "Refused: "+err.Error()
		return rep
	}
	if j.st.has(stepBackDone) {
		rep.ExitCode, rep.Summary = exitRefused, "Refused: this run's way back already ran; nothing changed."
		j.privacy()
		return rep
	}
	release, ok, err := lockFile(filepath.Join(r.deps.Backups, ".lock"))
	if err != nil || !ok {
		rep.ExitCode, rep.Summary = exitRefused, "Refused: another tracepad upgrade is running, or "+r.deps.Backups+" cannot be locked."
		return rep
	}
	defer release()
	// Once it has begun, an interrupt must not leave the server down half way.
	ctx, cancel := afterStop(ctx)
	defer cancel()
	out := j.wayBack(ctx)
	switch {
	case out.ok && !out.unconfirmed:
		rep.ExitCode = exitOK
		rep.Summary = fmt.Sprintf("Went back: %s runs again, healthy.", j.st.From)
	case out.ok:
		rep.ExitCode = exitStuck
		rep.Summary = fmt.Sprintf("Went back and started %s again, but it has not shown it is healthy: %s.", j.st.From, out.why)
	default:
		rep.ExitCode = exitStuck
		rep.Summary = "The way back did not finish: " + out.why
	}
	if j.st.Kind == kindProcess && j.st.Process != nil {
		if v, err := health(ctx, r.deps.HTTP, j.st.Process.URL); err == nil {
			rep.Notes = append(rep.Notes, j.st.Process.URL+" answers as "+v)
		}
	}
	j.privacy()
	return rep
}

// checkMode is `--check RUN`: the verdict again, for a server that was still
// starting. It never takes the way back itself.
func (r *runner) checkMode(ctx context.Context) *Report {
	rep := &Report{Mode: "check"}
	j, err := r.openRun(r.flags.check, rep)
	if err != nil {
		rep.ExitCode, rep.Summary = exitRefused, "Refused: "+err.Error()
		return rep
	}
	release, ok, err := lockFile(filepath.Join(r.deps.Backups, ".lock"))
	if err != nil || !ok {
		rep.ExitCode, rep.Summary = exitRefused, "Refused: another tracepad upgrade is running, or "+r.deps.Backups+" cannot be locked."
		return rep
	}
	defer release()
	st := j.st
	if !st.has(stepStarted) || st.has(stepBackBegun) {
		rep.ExitCode, rep.Summary = exitRefused, "Refused: this run has no new version running to check."
		return rep
	}
	var c Checked
	switch st.Kind {
	case kindProcess:
		ps := st.Process
		spec, err := readSpec(j.dir)
		if err != nil {
			rep.ExitCode, rep.Summary = exitRefused, "Refused: the run's server.json does not read: "+err.Error()
			return rep
		}
		c = r.check(ctx, ps.URL, st.To, st.CountBefore, func() bool {
			ok, _ := r.isServer(ps.NewPID, ps.DataDir, spec)
			return ok
		})
		c.LogLine = firstLogLine(ps.Log, ps.LogOffset)
	case kindContainer:
		cs := st.Container
		c = r.check(ctx, cs.URL, st.To, st.CountBefore, func() bool { return r.containerRunning(ctx, cs.NewID, false) })
		c.LogLine = r.containerFirstLog(ctx, cs.NewID)
	default:
		rep.ExitCode, rep.Summary = exitRefused, "Refused: a run of the binary alone has no server to check."
		return rep
	}
	rep.Check = &c
	st.Verdict = c.Verdict
	_ = st.save(j.dir)
	if c.Verdict == verdictHealthy {
		rep.ExitCode, rep.Summary = exitOK, fmt.Sprintf("%s is healthy.", st.To)
		return rep
	}
	rep.ExitCode = exitDecide
	rep.Summary = fmt.Sprintf("%s is not healthy: %s.", st.To, c.Why)
	if c.Verdict == verdictDecide {
		rep.Summary = fmt.Sprintf("%s runs, but %s.", st.To, c.Why)
	}
	rep.Person = append(rep.Person, "go back to "+st.From+", which drops what arrived since: "+j.upgradeCmd("--back "+st.Run))
	return rep
}
