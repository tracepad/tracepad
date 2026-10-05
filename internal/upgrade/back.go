package upgrade

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tracepad/tracepad/internal/store"
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
	// A lock the way back took is let go however it ends.
	defer j.release()
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
	if err := j.replaceBinary(ctx, copyPath, want); err != nil {
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
		if err := j.at("back.move"); err != nil {
			return j.fail(fmt.Sprintf("the restore in %s could not take the place of %s (%v); run --back again", restore, ps.DataDir, err))
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
			return j.checkBack(ctx, r.watch(holder, ps.DataDir, j.spec), holder, false)
		}
	}

	if !st.has(stepBackBinary) {
		j.begin()
		err := j.at("back.binary")
		if err == nil {
			err = j.restoreBinary(ctx, ps.Old, st.From)
		}
		if err != nil {
			return j.fail(fmt.Sprintf("%s could not be put back at %s (%v); nothing was started; run --back again", st.From, st.Binary.Path, err))
		}
		_ = j.step(stepBackBinary)
	}

	// Started already, by an earlier attempt: that server is checked rather
	// than a second one started — once it has taken the database, which a
	// server just started may not have yet (the fault matrix).
	if st.has(stepBackStarted) && r.deps.Sys.Alive(ps.BackPID) {
		r.waitRecorded(ctx, ps.BackPID, ps.DataDir)
	}
	// Whoever holds the data now — this command since the restore (j.held),
	// an earlier attempt's server, or another — decides: this directory's
	// server as the run recorded it is checked and kept; anything else
	// stops the way back with nothing started.
	if j.held == nil && lockHeld(ps.DataDir) {
		holder, _ := lockedBy(ps.DataDir)
		if ok, err := r.isServer(holder, ps.DataDir, j.spec); ok {
			if ps.BackPID != holder {
				ps.BackPID = holder
				_ = j.step(stepBackStarted)
			}
			return j.checkBack(ctx, r.watch(holder, ps.DataDir, j.spec), holder, false)
		} else if err != nil {
			return j.fail(fmt.Sprintf("pid %d took %s meanwhile (%v); nothing was started", holder, ps.DataDir, err))
		}
		return j.fail(fmt.Sprintf("pid %d took %s meanwhile; nothing was started", holder, ps.DataDir))
	}
	j.begin()
	if err := j.at("back.start"); err != nil {
		return j.fail(fmt.Sprintf("%s did not start (%v); run --back again", st.From, err))
	}
	started, err := j.start()
	if err != nil {
		return j.fail(fmt.Sprintf("%s did not start (%v); run --back again", st.From, err))
	}
	ps.BackPID = started.PID()
	_ = j.step(stepBackStarted)
	j.done("started %s again as pid %d, with its arguments and environment", st.From, ps.BackPID)
	return j.checkBack(ctx, running(started), ps.BackPID, true)
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
	rb, err := readBack(archive, *st.Archive)
	if err != nil {
		return j.fail(err.Error() + "; nothing was touched"), false
	}
	// The restore goes beside the data, on the data's file system, which
	// may be another and fuller one than the archive's (the fourth review).
	if free, err := freeBytes(filepath.Dir(ps.DataDir)); err == nil && free < rb.Bytes+mib100 {
		return j.fail(fmt.Sprintf("no room beside %s for the restore: %d MiB free, %d MiB needed; nothing was touched", ps.DataDir, free>>20, (rb.Bytes+mib100)>>20)), false
	}
	if _, err := os.Lstat(after); err == nil {
		return j.fail(after + " exists already, and this run did not put it there; nothing was touched"), false
	}
	// A restore the way back finished and checked before is taken up as it
	// is; one it began and did not finish (no back_restored) holds nothing
	// but part of the archive, which stays, and is begun again (the fault
	// matrix: a way back cut short after its restore never got past it).
	if !st.has(stepBackRestored) {
		if _, err := os.Lstat(restore); err == nil {
			if err := os.RemoveAll(restore); err != nil {
				return j.fail(fmt.Sprintf("a restore begun before and not finished is in %s and could not be begun again (%v); nothing was touched", restore, err)), false
			}
		}
		info, err := os.Stat(ps.DataDir)
		if err != nil {
			return j.fail(err.Error()), false
		}
		err = j.at("back.restore")
		if err == nil {
			err = extractArchive(archive, restore, info.Mode().Perm())
		}
		if err != nil {
			return j.fail(fmt.Sprintf("the restore into %s failed (%v); the server and %s are as they were; run --back again", restore, err, ps.DataDir)), false
		}
		if err := quickCheck(ctx, filepath.Join(restore, dataDBName)); err != nil {
			return j.fail(fmt.Sprintf("the restored database fails its check (%v); it is in %s, and the server and %s are as they were", err, restore, ps.DataDir)), false
		}
		_ = j.step(stepBackRestored)
		j.done("restored the archive into %s, and its database passes quick_check", restore)
	} else if _, err := os.Stat(filepath.Join(restore, dataDBName)); err != nil {
		return j.fail(fmt.Sprintf("the restore this way back made and checked is not in %s any more (%v); nothing was touched", restore, err)), false
	}
	// The restore's database is held from here until the old version starts
	// on it: the lock follows the file through the renames below.
	if ok, err := j.hold(filepath.Join(restore, dataDBName+store.LockSuffix)); !ok {
		return j.fail(fmt.Sprintf("the restore's lock could not be taken (%v); the server and %s are as they were, and the restore waits in %s", err, ps.DataDir, restore)), false
	}

	// Only now is the server stopped: a bad archive is found while it still
	// runs.
	j.begin()
	if err := j.at("back.stop"); err != nil {
		return j.fail(fmt.Sprintf("server pid %d was not stopped (%v); the restore waits in %s; run --back again", holder, err, restore)), false
	}
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
	err = j.at("back.aside")
	if err == nil {
		err = renameDir(ps.DataDir, after)
	}
	if err != nil {
		return j.fail(fmt.Sprintf("%s could not be set aside (%v); nothing was started, and the restore waits in %s", ps.DataDir, err, restore)), false
	}
	st.SetAside = append(st.SetAside, after)
	_ = j.step(stepBackAside)
	err = j.at("back.move")
	if err == nil {
		err = renameDir(restore, ps.DataDir)
	}
	if err != nil {
		return j.fail(fmt.Sprintf("the restore could not take the place of %s (%v); %s is set aside, and the restore waits in %s; run --back again to finish", ps.DataDir, err, after, restore)), false
	}
	_ = j.step(stepBackMoved)
	j.done("set %s aside as %s, and put the restore in its place", ps.DataDir, after)
	return wentBack{}, true
}

// waitRecorded waits, as a stop does, for a server just started to take its
// database: its PID in the lock.
func (r *runner) waitRecorded(ctx context.Context, pid int, dataDir string) {
	deadline := r.deps.Now().Add(r.deps.StopWait)
	for r.deps.Sys.Alive(pid) && !recordsPID(dataDir, pid) {
		if !r.deps.Now().Before(deadline) || r.deps.Sleep(ctx, 250*time.Millisecond) != nil {
			return
		}
	}
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

// watch is a check's liveness for a server this invocation did not start:
// its full identity — executable, arguments, the lock's record, which on
// macOS asks lsof — is read once, and each poll after that is kill(pid, 0)
// and the lock's record alone (the third review).
func (r *runner) watch(pid int, dataDir string, spec ServerSpec) func() bool {
	if ok, _ := r.isServer(pid, dataDir, spec); !ok {
		return func() bool { return false }
	}
	return func() bool { return r.deps.Sys.Alive(pid) && recordsPID(dataDir, pid) }
}

// confirm reads a server's full identity once more after a check found it
// healthy: what answered must still be the server the run recorded.
func (r *runner) confirm(c Checked, pid int, dataDir string, spec ServerSpec) Checked {
	if c.Verdict != verdictHealthy || pid <= 0 {
		return c
	}
	if ok, err := r.isServer(pid, dataDir, spec); !ok {
		why := "it is gone"
		if err != nil {
			why = err.Error()
		}
		c.Verdict, c.Why = verdictNotHealthy, fmt.Sprintf("pid %d answered, but %s", pid, why)
	}
	return c
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
func (j *job) checkBack(ctx context.Context, alive func() bool, pid int, fresh bool) wentBack {
	ps := j.st.Process
	if err := j.at("back.check"); err != nil {
		return wentBack{ok: true, unconfirmed: true, why: "its check failed: " + err.Error()}
	}
	c := j.r.check(ctx, ps.URL, j.st.From, j.st.CountBefore, alive)
	if !fresh {
		// One this command started is watched through its own exit; one it
		// found running is read once more.
		c = j.r.confirm(c, pid, ps.DataDir, j.spec)
	}
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
		err := j.at("back.run")
		if err == nil {
			_, err = docker.Run(ctx, "start", cs.Name)
		}
		if err != nil {
			return j.fail(cs.Name + " did not start again: " + firstLine(err.Error()))
		}
		j.done("started %s again", cs.Name)
		return j.checkContainer(ctx, cs.ID, false)
	case !st.has(stepStarted) && !r.exists(ctx, "container", j.before()) && !st.has(stepBackStarted) && !j.nameIs(ctx, cs.ID):
		// The old container was removed since (by the person): the old
		// image runs again, on the volume the new version never ran on.
		j.begin()
		if out, ok := j.clearName(ctx, ""); !ok {
			return out
		}
		ref := j.oldRef(ctx)
		out, err := docker.Run(ctx, runArgs(j.inspect, j.image, cs.Name, ref, filepath.Join(j.dir, "env"), "")...)
		if err != nil {
			return j.fail(st.From + " did not start: " + firstLine(err.Error()) + "; run --back again")
		}
		cs.BackID = strings.TrimSpace(string(out))
		_ = j.step(stepBackStarted)
		j.done("%s was gone; ran %s as %s again", j.before(), ref, cs.Name)
		return j.checkContainer(ctx, cs.BackID, true)
	case !st.has(stepStarted) && st.has(stepBackStarted):
		return j.checkContainer(ctx, cs.BackID, false)
	case !st.has(stepStarted):
		j.begin()
		if err := j.at("back.run"); err != nil {
			return j.fail(j.before() + " was not renamed back (" + err.Error() + "); run --back again")
		}
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
		}
		if list, err := r.inspectContainers(ctx, cs.Name); err == nil && len(list) == 1 && list[0].ID != cs.NewID {
			return j.fail("the container named " + cs.Name + " now is not the one this run started: a later run's, or one made since. " +
				"Go back from the run that made it first; nothing was touched")
		}
		if err := checkArchiveDB(ctx, archive, j.dir); err != nil {
			return j.fail("the archive's database fails its check (" + err.Error() + "); nothing was touched")
		}
		if err := j.at("back.volume"); err != nil {
			return j.fail("the volume " + vol + " was not made (" + err.Error() + "); nothing was touched")
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
		if err := j.at("back.aside"); err != nil {
			return j.fail("the new container was not set aside (" + err.Error() + "); run --back again")
		}
		if list, err := r.inspectContainers(ctx, cs.Name); err == nil && len(list) == 1 && list[0].ID == cs.NewID {
			if _, err := docker.Run(ctx, "stop", "--time", strconv.Itoa(int(r.deps.StopWait.Seconds())), cs.Name); err != nil {
				return j.fail(cs.Name + " did not stop: " + firstLine(err.Error()) + "; run --back again")
			}
			if _, err := docker.Run(ctx, "rename", cs.Name, j.after()); err != nil {
				return j.fail(cs.Name + " could not be set aside: " + firstLine(err.Error()) + "; run --back again")
			}
		}
		if r.exists(ctx, "container", j.after()) {
			if _, err := docker.Run(ctx, "update", "--restart", "no", j.after()); err != nil {
				return j.fail("the restart policy of " + j.after() + " could not be set to no: " + firstLine(err.Error()) + "; run --back again")
			}
			st.SetAside = append(st.SetAside, "container "+j.after())
			j.done("stopped the new container and set it aside as %s, restart policy no", j.after())
		} else {
			// Removed by the person since: a fact, not a failure (the third
			// review). There is nothing to set aside.
			j.done("the new container is gone already (removed since the upgrade); nothing to set aside")
		}
		_ = j.step(stepBackAside)
	}

	fresh := false
	if !st.has(stepBackStarted) || !r.containerRunning(ctx, cs.BackID, false) {
		j.begin()
		if err := j.at("back.run"); err != nil {
			return j.fail(st.From + " did not start on " + vol + " (" + err.Error() + "); run --back again")
		}
		if out, ok := j.clearName(ctx, cs.BackID); !ok {
			return out
		}
		if cs.BackID != "" && r.exists(ctx, "container", cs.BackID) {
			if _, err := docker.Run(ctx, "start", cs.BackID); err != nil {
				return j.fail(st.From + " did not start on " + vol + ": " + firstLine(err.Error()) + "; run --back again")
			}
		} else {
			ref := j.oldRef(ctx)
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
		err := j.at("back.binary")
		if err == nil {
			err = j.restoreBinary(ctx, st.Binary.Old, st.Binary.From)
		}
		if err != nil {
			j.rep.Notes = append(j.rep.Notes, "the binary at "+st.Binary.Path+" was not put back: "+err.Error())
		} else {
			_ = j.step(stepBackBinary)
		}
	}
	return j.checkContainer(ctx, cs.BackID, fresh)
}

// oldRef is how the old image is named to run it again: its tag while that
// still names the old image, else the image's id.
func (j *job) oldRef(ctx context.Context) string {
	cs := j.st.Container
	if img, err := j.r.inspectImage(ctx, cs.OldRef); err == nil && img.ID == cs.OldImage {
		return cs.OldRef
	}
	return cs.OldImage
}

// nameIs says whether the container under the run's name is the one with
// this id.
func (j *job) nameIs(ctx context.Context, id string) bool {
	list, err := j.r.inspectContainers(ctx, j.st.Container.Name)
	return err == nil && len(list) == 1 && list[0].ID == id
}

// clearName sets aside, as <name>-failed-<run> with restart policy no, a
// container left under the name the way back is about to run: one a docker
// run made and could not start (a port still taken), which would otherwise
// make every later attempt fail on the name (the third review). A container
// under the name that runs, and is not the run's own (keep), stops the way
// back with nothing touched.
func (j *job) clearName(ctx context.Context, keep string) (wentBack, bool) {
	r, cs := j.r, j.st.Container
	list, err := r.inspectContainers(ctx, cs.Name)
	if err != nil || len(list) != 1 || (keep != "" && list[0].ID == keep) {
		return wentBack{}, true
	}
	c := list[0]
	if c.State.Running {
		return j.fail("a container named " + cs.Name + " runs, and it is not this run's; nothing was started"), false
	}
	name := j.failed()
	for i := 2; r.exists(ctx, "container", name); i++ {
		name = j.failed() + "-" + strconv.Itoa(i)
	}
	if _, err := r.deps.Docker.Run(ctx, "rename", c.ID, name); err != nil {
		return j.fail("the container left as " + cs.Name + " could not be set aside: " + firstLine(err.Error())), false
	}
	_, _ = r.deps.Docker.Run(ctx, "update", "--restart", "no", name)
	j.st.SetAside = append(j.st.SetAside, "container "+name)
	_ = j.st.save(j.dir)
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
	defer os.RemoveAll(check)
	if err := extractDB(archive, check); err != nil {
		return err
	}
	return quickCheck(ctx, filepath.Join(check, dataDBName))
}

func (j *job) checkContainer(ctx context.Context, id string, fresh bool) wentBack {
	cs := j.st.Container
	if err := j.at("back.check"); err != nil {
		return wentBack{ok: true, unconfirmed: true, why: "its check failed: " + err.Error()}
	}
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
	release, ok := r.lockRuns(rep)
	if !ok {
		return rep
	}
	defer release()
	// Once it has begun, an interrupt must not leave the server down half way.
	ctx, cancel := wayBackContext(ctx)
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
	release, ok := r.lockRuns(rep)
	if !ok {
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
		c = r.check(ctx, ps.URL, st.To, st.CountBefore, r.watch(ps.NewPID, ps.DataDir, spec))
		c = r.confirm(c, ps.NewPID, ps.DataDir, spec)
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
		j.finishHealthy(ctx)
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
