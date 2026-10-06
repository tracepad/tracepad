package upgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	// A way back done already runs again to the same end (#31): the old
	// version, checked, on the same data — started again if it was stopped
	// since, and nothing else changed. A lock the way back took is let go
	// however it ends.
	defer j.release()
	if st.Kind != kindBinary && !st.has(stepStopSent) {
		// Nothing was stopped, nothing to undo: a run refused, or cut short
		// before its stop (#34).
		if st.last() != stepBackDone {
			j.step(stepBackDone)
		}
		return wentBack{ok: true}
	}
	var out wentBack
	switch st.Kind {
	case kindProcess:
		out = j.backProcess(ctx)
	default:
		out = j.backBinary(ctx)
	}
	// Done only when the old version is seen healthy: a way back that could
	// not confirm it is taken up again by the next --back, which checks.
	if out.ok && !out.unconfirmed && st.last() != stepBackDone {
		j.step(stepBackDone)
	}
	return out
}

func (j *job) fail(why string) wentBack { return wentBack{ok: false, why: why} }

// begin records that the way back is about to change what runs. A way back
// refused before this point touched nothing, and `--check` still works for
// the run (the review of #1).
func (j *job) begin() {
	if !j.st.has(stepBackBegun) {
		j.step(stepBackBegun)
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
	if err := j.replaceBinary(ctx, copyPath, want, false); err != nil {
		return err
	}
	j.done("put tracepad %s back at %s", want, path)
	return nil
}

func (j *job) backBinary(ctx context.Context) wentBack {
	b := j.st.Binary
	if !j.st.has(stepBinaryReplacing) {
		return wentBack{ok: true}
	}
	// What is at the path must be what the run left there (#31 (b)): a
	// later run's version is not this run's to take back.
	if err := j.pathAsRecorded(ctx); err != nil {
		return j.fail(err.Error() + "; nothing was touched")
	}
	if j.st.has(stepBackBinary) {
		return wentBack{ok: true}
	}
	j.begin()
	err := j.act(stepBackBinary, "put tracepad "+b.From+" back at "+b.Path, func() error {
		return j.restoreBinary(ctx, b.Old, b.From)
	})
	if err != nil {
		return j.fail(fmt.Sprintf("%s could not be put back at %s: %v", b.From, b.Path, err))
	}
	j.step(stepBackBinary)
	return wentBack{ok: true}
}

func (j *job) backProcess(ctx context.Context) wentBack {
	r, st := j.r, j.st
	ps := st.Process
	if j.spec.Exe == "" {
		spec, err := j.loadSpec()
		if err != nil {
			return j.fail("the run's server.json does not read: " + err.Error())
		}
		j.spec = spec
	}
	// What the machine must give the way back, checked before its first
	// act, as the upgrade checked it before its stop (the final review).
	restoreBytes := int64(-1)
	if st.Archive != nil && !st.has(stepBackRestored) && !st.has(stepBackMoved) && (ps.WroteAfterSwap || j.pathMayBeNew()) {
		restoreBytes = st.Archive.Bytes
	}
	if err := j.wayBackPreconditions(ctx, restoreBytes, 0); err != nil {
		return j.fail(err.Error() + "; nothing was touched")
	}
	if !st.has(stepStopSent) {
		return wentBack{ok: true}
	}
	if err := j.pathAsRecorded(ctx); err != nil {
		return j.fail(err.Error() + "; nothing was touched")
	}
	// Every check the way back makes comes before its first act (the eighth
	// review), as the upgrade's come before its stop: the old binary is put
	// back only where nothing runs it at a later version — the server of
	// this run's data aside, which the way back stops itself. Put back at
	// the end, it checks again, a second line against one started meanwhile.
	if !st.has(stepBackBinary) {
		// The run's own new server is not counted only when it is the one the
		// way back stops: one alive and unread is counted (the audit of #223).
		own := []int{ps.PID, ps.BackPID}
		if ok, _ := r.startedAs(ps.NewPID, ps.NewStart, j.spec); ok { // ignored: one that cannot be read is counted, which refuses
			own = append(own, ps.NewPID)
		}
		if held, _ := lockHeld(ps.DataDir); held { // ignored: an unread lock leaves the holder counted, which refuses
			if holder, err := lockedBy(ps.DataDir); err == nil {
				if ok, _ := r.isServer(holder, 0, ps.DataDir, j.spec); ok { // ignored: one that cannot be read is not the run's, and is counted
					own = append(own, holder)
				}
			}
		}
		if err := r.serversOn(ctx, st.Binary.Path, st.From, false, own...); err != nil {
			return j.fail(err.Error() + "; nothing was touched")
		}
	}
	after, restore := ps.DataDir+".after-"+st.Run, ps.DataDir+".restore-"+st.Run

	// A way back that stopped half way is taken up where it stopped, from
	// the steps it recorded (the second review): never refused for good, and
	// never with advice to remove what the new version wrote.
	if st.has(stepBackAside) && !st.has(stepBackMoved) {
		if _, err := os.Lstat(ps.DataDir); err == nil {
			return j.fail(fmt.Sprintf("%s is set aside as %s, and something is at %s again; nothing was touched", ps.DataDir, after, ps.DataDir))
		} else if !errors.Is(err, os.ErrNotExist) {
			return j.fail(fmt.Sprintf("%s cannot be looked at (%v); nothing was touched", ps.DataDir, err))
		}
		if out, ok := j.moveRestore(restore, after); !ok {
			return out
		}
	}

	// The run's own server, never seen stopped — the stop's intent written
	// and its signal lost to a kill, or a stop that timed out — and still
	// running: nothing after the stop was done, so it is the old version on
	// its own data, untouched, and it is kept and checked (the eighth
	// review's kill cells). One on its way out is waited for first.
	if !st.has(stepStopped) && !st.has(stepBackCleared) {
		alive, err := r.isServer(ps.PID, ps.PIDStart, ps.DataDir, j.spec)
		if err != nil && r.deps.Sys.Alive(ps.PID) {
			return j.fail(fmt.Sprintf("pid %d, the server the run asked to stop, may still run and cannot be read (%v); nothing was touched", ps.PID, err))
		}
		kept := st.has(stepBackStarted) && ps.BackPID == ps.PID
		if alive && (kept || !r.waitGone(ctx, ps.PID)) {
			if !kept {
				j.begin()
				ps.BackPID, ps.BackStart = ps.PID, ps.PIDStart
				j.step(stepBackStarted)
				j.done("server pid %d was asked to stop and still runs, as it was before the run: it is kept", ps.PID)
			}
			return j.checkBack(ctx, nil, ps.PID, ps.PIDStart)
		}
	}

	// The decision, unless it is made and acted on: a way back cut short
	// between the path's clearing and the restore's move takes it up again,
	// with nothing able to start from the path meanwhile.
	if !st.has(stepBackMoved) && (!st.has(stepBackCleared) || ps.WroteAfterSwap) {
		holder, out, ok := j.decide(ctx)
		if !ok {
			return out
		}
		if ps.WroteAfterSwap {
			if out, ok := j.swapBack(ctx, holder, after, restore); !ok {
				return out
			}
		} else {
			// Nothing but the old version had the data: it stays as it is.
			j.begin()
			if out, ok := j.clearPath(ctx); !ok {
				return out
			}
		}
	}

	if !st.has(stepBackBinary) {
		j.begin()
		err := j.act(stepBackBinary, "put tracepad "+st.From+" back at "+st.Binary.Path, func() error {
			return j.restoreBinary(ctx, ps.Old, st.From)
		})
		if err != nil {
			return j.fail(fmt.Sprintf("%s could not be put back at %s (%v); nothing was started, and nothing runs from %s until it is: run --back again", st.From, st.Binary.Path, err, st.Binary.Path))
		}
		j.step(stepBackBinary)
	}

	// From here the install path has held the old version or nothing since
	// the steps say so, and the command held the data until then: a server
	// on the data now is the old version (#31). It is kept when it is this
	// directory's server as the run recorded it — an earlier attempt's, or
	// one the person or a shell loop started — once it has taken the
	// database; anything else stops the way back with nothing started.
	if st.has(stepBackStarted) && r.deps.Sys.Alive(ps.BackPID) {
		r.waitRecorded(ctx, ps.BackPID, ps.DataDir)
	}
	held, err := lockHeld(ps.DataDir)
	if err != nil && !j.holds(ps.DataDir) {
		return j.fail(fmt.Sprintf("the lock of %s cannot be read (%v); nothing was started", ps.DataDir, err))
	}
	if !j.holds(ps.DataDir) && held {
		holder, _ := lockedBy(ps.DataDir) // ignored: the pid is the message's; one that cannot be read is no server, and is refused
		if ok, err := r.isServer(holder, 0, ps.DataDir, j.spec); !ok {
			why := fmt.Sprintf("pid %d took %s meanwhile", holder, ps.DataDir)
			if err != nil {
				why += " (" + err.Error() + ")"
			}
			return j.fail(why + "; nothing was started")
		}
		if ps.BackPID != holder || st.last() == stepBackBinary {
			ps.BackPID, ps.BackStart = holder, r.startOf(holder)
			j.step(stepBackStarted)
		}
		return j.checkBack(ctx, nil, holder, ps.BackStart)
	}
	j.begin()
	var started Started
	err = j.act(stepBackStarted, "start tracepad "+st.From+" on "+ps.DataDir, func() (err error) {
		started, err = j.launch(ctx, false)
		return err
	})
	if err != nil {
		return j.fail(fmt.Sprintf("%s did not start (%v); run --back again", st.From, err))
	}
	ps.BackPID = started.PID()
	j.step(stepBackStarted)
	j.done("started %s again as pid %d, with its arguments and environment", st.From, ps.BackPID)
	return j.checkBack(ctx, started, ps.BackPID, ps.BackStart)
}

// wayBackPreconditions are what a way back of a process run needs from the
// machine rather than from the run: the copy of the old version in the run's
// directory, and — when the archive may be restored — room for the restore
// beside the data, and a parent directory the restore can be made in and
// the data renamed aside through. The upgrade checks them before its stop,
// and --back before its first act (the final review): a way back found
// impossible only after the stop leaves a server down.
//
// restoreBytes is what a restore takes, or -1 when none is ahead; archiveBytes
// is what an archive not yet written takes on the run directory's file
// system, counted too when that is the data's.
func (j *job) wayBackPreconditions(ctx context.Context, restoreBytes, archiveBytes int64) error {
	st, ps := j.st, j.st.Process
	if v, err := j.r.deps.Version(ctx, ps.Old); err != nil || v != st.From {
		return fmt.Errorf("the copy %s does not answer %s", ps.Old, st.From)
	}
	if restoreBytes < 0 {
		return nil
	}
	if restoreBytes == 0 {
		// An archive recorded without its size: the data's own is the
		// nearest measure.
		size, err := survey(ps.DataDir)
		if err != nil {
			return fmt.Errorf("the room a restore of %s needs cannot be told: %v", ps.DataDir, err)
		}
		restoreBytes = size
	}
	parent := filepath.Dir(ps.DataDir)
	if !st.has(stepBackAside) {
		after := ps.DataDir + ".after-" + st.Run
		if _, err := os.Lstat(after); err == nil {
			return fmt.Errorf("%s exists already, and this run did not put it there", after)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s cannot be looked at (%v)", after, err)
		}
		if info, err := os.Stat(parent); err != nil {
			return fmt.Errorf("%s cannot be looked at (%v)", parent, err)
		} else if info.Mode()&os.ModeSticky != 0 && !ownedByMe(info) {
			// Only a file's owner renames it in a sticky directory of
			// another's.
			if d, err := os.Lstat(ps.DataDir); err != nil || !ownedByMe(d) {
				return fmt.Errorf("%s is in %s, another user's sticky directory, and a way back could not rename it aside", ps.DataDir, parent)
			}
		}
	}
	// The way back makes the restore in the parent and renames through it:
	// a directory made, renamed and removed there shows it can.
	probe, err := os.MkdirTemp(parent, ".tracepad-probe-")
	if err != nil {
		return fmt.Errorf("a restore cannot be made beside %s (%v)", ps.DataDir, err)
	}
	moved := probe + "-moved"
	if err := os.Rename(probe, moved); err != nil {
		os.Remove(probe) // ignored: an empty directory of the command's own; its name says whose
		return fmt.Errorf("nothing can be renamed in %s (%v), as a way back renames %s aside", parent, err, ps.DataDir)
	}
	if err := os.Remove(moved); err != nil {
		return fmt.Errorf("%s does not let the way back remove what it makes there (%v)", parent, err)
	}
	free, err := freeBytes(parent)
	if err != nil {
		return fmt.Errorf("the room beside %s for a restore cannot be told (%v)", ps.DataDir, err)
	}
	need := restoreBytes + mib100
	if sameDevice(parent, j.dir) {
		need += archiveBytes
	}
	if free < need {
		return fmt.Errorf("no room beside %s for the restore a way back may need: %d MiB free, %d MiB needed", ps.DataDir, free>>20, need>>20)
	}
	return nil
}

// pathMayBeNew says, from the steps alone, whether the install path may have
// held another version than the old one since the stop: what started from it
// in that time may have been that version (#31).
func (j *job) pathMayBeNew() bool {
	st := j.st
	return !st.has(stepBackCleared) && (st.has(stepBinaryReplacing) || st.Binary.From != st.From)
}

// pathAsRecorded checks that the install path holds what the steps say it
// may: a later run or the person may have put another version there since,
// and a way back from this run would then undo theirs. Its version is the
// binary's own answer, not a server's.
func (j *job) pathAsRecorded(ctx context.Context) error {
	st, b := j.st, j.st.Binary
	if b.From == "" {
		return nil
	}
	if _, err := os.Lstat(b.Path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	want := []string{b.From}
	if st.has(stepBinaryReplacing) && !st.has(stepBackBinary) {
		want = append(want, st.To)
	}
	if st.Kind == kindProcess && st.has(stepBackCleared) {
		want = []string{st.From}
	}
	v, err := j.r.deps.Version(ctx, b.Path)
	if err != nil || !slices.Contains(want, v) {
		return fmt.Errorf("%s is now %q, which this run did not put there (it may hold %s): a later upgrade, or the person's; go back from the run that put it there first", b.Path, v, strings.Join(want, " or "))
	}
	return nil
}

// decide is the way back's one decision — restore the archive or not — made
// from what the state records, never from how a server answers or how fast
// (#31): the archive is restored when anything but the old version may have
// had the data since the stop. A server on the data is this directory's
// server as the run recorded it, or the way back stops with nothing
// touched; when it started while the steps say the install path may have
// held another version, that is recorded too. Without one, the data is held
// from here. It answers the server that holds the data, if any.
func (j *job) decide(ctx context.Context) (int, wentBack, bool) {
	r, ps := j.r, j.st.Process
	// The old server may still be on its way out: its stop timed out. It is
	// waited for, and started again once it has gone.
	alive, err := r.isServer(ps.PID, ps.PIDStart, ps.DataDir, j.spec)
	if err != nil && r.deps.Sys.Alive(ps.PID) {
		return 0, j.fail(fmt.Sprintf("pid %d, the server the run stopped, may still run and cannot be read (%v); nothing was touched", ps.PID, err)), false
	}
	if alive && !r.waitGone(ctx, ps.PID) {
		return 0, j.fail(fmt.Sprintf("server pid %d is still shutting down from its stop; run --back again once it has exited. Nothing was touched", ps.PID)), false
	}
	for range 3 {
		if j.holds(ps.DataDir) {
			return 0, wentBack{}, true
		}
		held, err := lockHeld(ps.DataDir)
		if err != nil {
			return 0, j.fail(fmt.Sprintf("the lock of %s cannot be read (%v); nothing was touched", ps.DataDir, err)), false
		}
		if !held {
			ok, err := j.hold(ps.DataDir)
			if err != nil {
				return 0, j.fail(fmt.Sprintf("the lock of %s could not be taken (%v); nothing was touched", ps.DataDir, err)), false
			}
			if ok {
				return 0, wentBack{}, true
			}
			continue
		}
		holder, _ := lockedBy(ps.DataDir) // ignored: the pid is the message's; one that cannot be read is no server, and is refused
		if ok, err := r.isServer(holder, 0, ps.DataDir, j.spec); !ok {
			why := fmt.Sprintf("pid %d", holder)
			if err != nil {
				why = err.Error()
			}
			return 0, j.fail(fmt.Sprintf("pid %d holds %s, and it is not this directory's server as the run recorded it: %s. Stop it first; nothing was touched", holder, ps.DataDir, why)), false
		}
		if !ps.WroteAfterSwap && j.pathMayBeNew() {
			ps.WroteAfterSwap = true
			j.persist()
			j.done("server pid %d holds %s, and started while %s may have held tracepad %s: what it wrote is set aside, and the archive restored", holder, ps.DataDir, j.st.Binary.Path, j.st.To)
		}
		return holder, wentBack{}, true
	}
	return 0, j.fail("the lock of " + ps.DataDir + " kept changing hands; nothing was touched. Run --back again"), false
}

// clearPath takes another version than the old one off the install path,
// while the command holds the data: from here, what starts from the path is
// the old version or nothing, until the old version is put back (#31). The
// binary taken off is kept in the run's directory; nothing is deleted.
func (j *job) clearPath(ctx context.Context) (wentBack, bool) {
	st := j.st
	path := st.Binary.Path
	if st.has(stepBackCleared) {
		return wentBack{}, true
	}
	err := j.act(stepBackCleared, "take any other version than "+st.From+" off "+path+", keeping it in "+j.dir, func() error {
		// Only what is not there is absent (the audit of #223): a path that
		// cannot be looked at is not cleared.
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return fmt.Errorf("%s cannot be looked at (%v); nothing was started; run --back again", path, err)
		}
		v, err := j.r.deps.Version(ctx, path)
		if err == nil && v == st.From {
			return nil
		}
		if !j.holds(st.Process.DataDir) {
			return fmt.Errorf("a server holds %s and %s is %q; nothing was touched", st.Process.DataDir, path, v)
		}
		kept := ""
		for i := 1; kept == ""; i++ {
			kept = filepath.Join(j.dir, "installed-"+strconv.Itoa(i))
			if _, err := os.Lstat(kept); err == nil {
				kept = ""
			}
		}
		if err := linkOrCopy(path, kept); err != nil {
			return fmt.Errorf("%s could not be kept in %s (%v); nothing was started; run --back again", path, j.dir, err)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("%s could not be taken off (%v); nothing was started; run --back again", path, err)
		}
		j.done("took %s off %s until %s is back; it is kept as %s", firstNonEmpty(v, "the binary"), path, st.From, kept)
		return nil
	})
	if err != nil {
		return j.fail(err.Error()), false
	}
	j.step(stepBackCleared)
	return wentBack{}, true
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// moveRestore puts the restore in the data's place, the lock held on it
// following it there.
func (j *job) moveRestore(restore, after string) (wentBack, bool) {
	ps := j.st.Process
	if ok, err := j.hold(restore); !ok {
		return j.fail(fmt.Sprintf("the restore's lock could not be taken (%v); the restore waits in %s; run --back again", err, restore)), false
	}
	err := j.act(stepBackMoved, "rename "+restore+" to "+ps.DataDir, func() error {
		return renameDir(restore, ps.DataDir)
	})
	if err != nil {
		return j.fail(fmt.Sprintf("the restore could not take the place of %s (%v); %s is set aside, and the restore waits in %s; run --back again to finish", ps.DataDir, err, after, restore)), false
	}
	// The data's lock was the directory set aside; the restore's is the
	// data's now.
	j.letGo(ps.DataDir)
	j.held[ps.DataDir] = j.held[restore]
	delete(j.held, restore)
	j.step(stepBackMoved)
	j.done("put the restore in the place of %s", ps.DataDir)
	return wentBack{}, true
}

// swapBack restores the archive beside the data and checks it, stops the
// server that holds the data, and swaps the two directories, recording each
// move so that a way back cut short is taken up where it stopped.
func (j *job) swapBack(ctx context.Context, holder int, after, restore string) (wentBack, bool) {
	r, st := j.r, j.st
	ps := st.Process
	if st.Archive == nil {
		return j.fail(fmt.Sprintf("tracepad %s may have had %s, and the run took no archive of it to restore: going back would start %s on data %s may have migrated. Keep %s, or restore a backup of your own; nothing was touched", st.To, ps.DataDir, st.From, st.To, st.To)), false
	}
	archive := filepath.Join(j.dir, "data.tar.gz")
	rb, err := readBack(archive, *st.Archive)
	if err != nil {
		return j.fail(err.Error() + "; nothing was touched"), false
	}
	// The restore goes beside the data, on the data's file system, which
	// may be another and fuller one than the archive's (the fourth review).
	// Room the way back cannot tell is no room (the audit of #223), as in
	// the upgrade's own check.
	free, err := freeBytes(filepath.Dir(ps.DataDir))
	switch {
	case err != nil:
		return j.fail(fmt.Sprintf("could not tell the room beside %s for the restore (%v); nothing was touched", ps.DataDir, err)), false
	case free < rb.Bytes+mib100:
		return j.fail(fmt.Sprintf("no room beside %s for the restore: %d MiB free, %d MiB needed; nothing was touched", ps.DataDir, free>>20, (rb.Bytes+mib100)>>20)), false
	}
	if _, err := os.Lstat(after); err == nil {
		return j.fail(after + " exists already, and this run did not put it there; nothing was touched"), false
	} else if !errors.Is(err, os.ErrNotExist) {
		return j.fail(fmt.Sprintf("%s cannot be looked at (%v); nothing was touched", after, err)), false
	}
	j.begin()
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
		err = j.act(stepBackRestored, "restore "+archive+" into "+restore, func() error {
			return extractArchive(archive, restore, info.Mode().Perm())
		})
		if err != nil {
			return j.fail(fmt.Sprintf("the restore into %s failed (%v); the server and %s are as they were; run --back again", restore, err, ps.DataDir)), false
		}
		if err := quickCheck(ctx, filepath.Join(restore, dataDBName)); err != nil {
			return j.fail(fmt.Sprintf("the restored database fails its check (%v); it is in %s, and the server and %s are as they were", err, restore, ps.DataDir)), false
		}
		j.step(stepBackRestored)
		j.done("restored the archive into %s, and its database passes quick_check", restore)
	} else if _, err := os.Stat(filepath.Join(restore, dataDBName)); err != nil {
		return j.fail(fmt.Sprintf("the restore this way back made and checked is not in %s any more (%v); nothing was touched", restore, err)), false
	}
	// The restore's database is held from here until the old version starts
	// on it: the lock follows the file through the renames below.
	if ok, err := j.hold(restore); !ok {
		return j.fail(fmt.Sprintf("the restore's lock could not be taken (%v); the server and %s are as they were, and the restore waits in %s", err, ps.DataDir, restore)), false
	}

	// The run's own new server, if it runs on another directory, is
	// stopped too below; one that runs and cannot be read stops the way back
	// before anything is (the audit of #223).
	newRuns, err := r.startedAs(ps.NewPID, ps.NewStart, j.spec)
	if err != nil {
		return j.fail("the run's server " + err.Error() + "; nothing was stopped, and the restore waits in " + restore), false
	}
	// Only now is the server stopped: a bad archive is found while it still
	// runs.
	if holder > 0 {
		if ok, err := r.isServer(holder, 0, ps.DataDir, j.spec); !ok {
			why := "it is gone"
			if err != nil {
				why = err.Error()
			}
			if held, err := lockHeld(ps.DataDir); r.deps.Sys.Alive(holder) || held || err != nil {
				return j.fail(why + "; nothing was stopped, and the restore waits in " + restore), false
			}
		} else if !r.stop(ctx, holder) {
			return j.fail(fmt.Sprintf("server pid %d was asked to stop and has not; the restore waits in %s; run --back again once it has exited", holder, restore)), false
		} else {
			j.done("stopped server pid %d", holder)
		}
	}
	// The server this run started, when it holds something else than this
	// directory (it opened another one): it is the run's own, by its binary
	// and arguments, and it holds the address the old version needs.
	if ps.NewPID > 0 && ps.NewPID != holder && newRuns {
		if !r.stop(ctx, ps.NewPID) {
			return j.fail(fmt.Sprintf("the run's server pid %d was asked to stop and has not; the restore waits in %s; run --back again once it has exited", ps.NewPID, restore)), false
		}
		j.done("stopped the run's server pid %d", ps.NewPID)
	}
	// The data held from here too, until the old version starts: nothing
	// runs on it between the stop and the swap.
	if ok, err := j.hold(ps.DataDir); err != nil || !ok {
		why := "something took it meanwhile"
		if err != nil {
			why = "its lock could not be taken: " + err.Error()
		}
		return j.fail(ps.DataDir + ": " + why + "; nothing was set aside, and the restore waits in " + restore), false
	}
	if out, ok := j.clearPath(ctx); !ok {
		return out, false
	}
	err = j.act(stepBackAside, "rename "+ps.DataDir+" to "+after, func() error {
		return renameDir(ps.DataDir, after)
	})
	if err != nil {
		return j.fail(fmt.Sprintf("%s could not be set aside (%v); nothing was started, and the restore waits in %s", ps.DataDir, err, restore)), false
	}
	st.SetAside = append(st.SetAside, after)
	j.step(stepBackAside)
	j.done("set %s aside as %s", ps.DataDir, after)
	return j.moveRestore(restore, after)
}

// waitUntil polls done, as a stop waits, for StopWait at most: the one wait
// of the package (the eighth review), so its deadline and its interrupt are
// handled in one place. It answers whether done came true.
func (r *runner) waitUntil(ctx context.Context, done func() bool) bool {
	deadline := r.deps.Now().Add(r.deps.StopWait)
	for !done() {
		if !r.deps.Now().Before(deadline) || r.deps.Sleep(ctx, 250*time.Millisecond) != nil {
			return false
		}
	}
	return true
}

// waitRecorded waits for a server just started to take its database — its
// PID in the lock — or to exit.
func (r *runner) waitRecorded(ctx context.Context, pid int, dataDir string) {
	r.waitUntil(ctx, func() bool { return !r.deps.Sys.Alive(pid) || recordsPID(dataDir, pid) })
}

// waitGone waits for a process already asked to stop.
func (r *runner) waitGone(ctx context.Context, pid int) bool {
	return r.waitUntil(ctx, func() bool { return !r.deps.Sys.Alive(pid) })
}

// startedAs says whether pid runs as spec started it: its binary and its
// arguments. It is how a way back knows the server its own run started when
// that server holds no lock of the run's directory.
func (r *runner) startedAs(pid int, start int64, spec ServerSpec) (bool, error) {
	if pid <= 0 || !r.deps.Sys.Alive(pid) {
		return false, nil
	}
	p, err := r.deps.Sys.Inspect(pid)
	switch {
	case errors.Is(err, errNotMine):
		return false, nil // another user's: never the run's
	case err != nil:
		return false, fmt.Errorf("pid %d runs and cannot be read: %w", pid, err)
	case start != 0 && p.Start != 0 && p.Start != start:
		return false, nil // the PID reused by another process
	}
	return sameFile(p.Exe, spec.Exe) && slices.Equal(p.Argv, spec.Argv), nil
}

// watch is a check's liveness for a server this invocation did not start:
// its full identity — executable, arguments, the lock's record, which on
// macOS asks lsof — is read once, and each poll after that is kill(pid, 0)
// and the lock's record alone (the third review).
// A server that cannot be read is no answer: watch says why (the audit of
// #223), and checkWatched makes it the verdict decide, never "exited".
func (r *runner) watch(pid int, start int64, dataDir string, spec ServerSpec) (func() bool, error) {
	ok, err := r.isServer(pid, start, dataDir, spec)
	if err != nil {
		return nil, err
	}
	if !ok {
		return func() bool { return false }, nil
	}
	return func() bool { return r.deps.Sys.Alive(pid) && recordsPID(dataDir, pid) }, nil
}

// checkWatched is a check of a server this invocation did not start.
func (r *runner) checkWatched(ctx context.Context, base, want string, before *int64, pid int, start int64, dataDir string, spec ServerSpec) Checked {
	alive, err := r.watch(pid, start, dataDir, spec)
	if err != nil {
		return Checked{Before: before, Verdict: verdictDecide, Why: err.Error()}
	}
	return r.confirm(r.check(ctx, base, want, before, alive), pid, start, dataDir, spec)
}

// confirm reads a server's full identity once more after a check found it
// healthy: what answered must still be the server the run recorded.
func (r *runner) confirm(c Checked, pid int, start int64, dataDir string, spec ServerSpec) Checked {
	if c.Verdict != verdictHealthy || pid <= 0 {
		return c
	}
	if ok, err := r.isServer(pid, start, dataDir, spec); !ok {
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

// checkBack checks the old version a way back started (fresh, watched
// through its own exit) or found running (read through checkWatched).
func (j *job) checkBack(ctx context.Context, started Started, pid int, start int64) wentBack {
	ps := j.st.Process
	if err := j.at(stepBackDone); err != nil {
		return wentBack{ok: true, unconfirmed: true, why: "its check failed: " + err.Error()}
	}
	var c Checked
	if started != nil {
		c = j.r.check(ctx, ps.URL, j.st.From, j.st.CountBefore, running(started))
		c.LogLine = firstLogLine(ps.Log, ps.LogOffset)
	} else {
		c = j.r.checkWatched(ctx, ps.URL, j.st.From, j.st.CountBefore, pid, start, ps.DataDir, j.spec)
	}
	j.rep.BackCheck = &c
	return checked(j.st.From+" started again but is not healthy", c)
}

// openRun loads a run for --check or --back.
func (r *runner) openRun(arg string, rep *Report) (*job, error) {
	dir, err := resolveRun(r.deps.Backups, arg)
	if err != nil {
		return nil, err
	}
	// A run's state names files the way back runs and starts: it is taken
	// only from a directory of the person's own, nobody else can write (the
	// sixth review).
	if err := private(dir); err != nil {
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
func (r *runner) backMode(ctx context.Context) (rep *Report) {
	rep = &Report{Mode: "back"}
	defer stopOffTable(rep)
	// The lock first, then the state: a state read before it could be one
	// another invocation is about to change (the sixth review).
	release, ok := r.lockRuns(rep)
	if !ok {
		return rep
	}
	defer release()
	j, err := r.openRun(r.flags.back, rep)
	if err == nil {
		err = j.settle(ctx)
	}
	if err != nil {
		rep.ExitCode, rep.Summary = exitRefused, "Refused: "+err.Error()
		return rep
	}
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
func (r *runner) checkMode(ctx context.Context) (rep *Report) {
	rep = &Report{Mode: "check"}
	defer stopOffTable(rep)
	release, ok := r.lockRuns(rep)
	if !ok {
		return rep
	}
	defer release()
	j, err := r.openRun(r.flags.check, rep)
	if err == nil {
		err = j.settle(ctx)
	}
	if err != nil {
		rep.ExitCode, rep.Summary = exitRefused, "Refused: "+err.Error()
		return rep
	}
	st := j.st
	if !st.has(stepStarted) || st.has(stepBackBegun) {
		rep.ExitCode, rep.Summary = exitRefused, "Refused: this run has no new version running to check."
		return rep
	}
	var c Checked
	switch st.Kind {
	case kindProcess:
		ps := st.Process
		spec, err := j.loadSpec()
		if err != nil {
			rep.ExitCode, rep.Summary = exitRefused, "Refused: the run's server.json does not read: "+err.Error()
			return rep
		}
		// The server is whichever holds the data as the run recorded it — the
		// one it started, or the same one the person started again since
		// (the seventh review) — and its version is the check's to say.
		// A lock, or a holder, that cannot be read refuses the check, with
		// nothing recorded (the audit of #223).
		pid, start := ps.NewPID, ps.NewStart
		held, err := lockHeld(ps.DataDir)
		if err != nil {
			rep.ExitCode, rep.Summary = exitRefused, fmt.Sprintf("Refused: the lock of %s cannot be read (%v); nothing was recorded.", ps.DataDir, err)
			return rep
		}
		if held {
			holder, err := lockedBy(ps.DataDir)
			if err != nil {
				rep.ExitCode, rep.Summary = exitRefused, fmt.Sprintf("Refused: %s is locked, and its lock does not say by whom (%v); nothing was recorded.", ps.DataDir, err)
				return rep
			}
			ok, err := r.isServer(holder, 0, ps.DataDir, spec)
			if err != nil && r.deps.Sys.Alive(holder) {
				rep.ExitCode, rep.Summary = exitRefused, fmt.Sprintf("Refused: the server on %s cannot be read (%v); nothing was recorded.", ps.DataDir, err)
				return rep
			}
			if ok && holder != pid {
				pid, start = holder, 0
			}
		}
		c = r.checkWatched(ctx, ps.URL, st.To, st.CountBefore, pid, start, ps.DataDir, spec)
		c.LogLine = firstLogLine(ps.Log, ps.LogOffset)
	default:
		rep.ExitCode, rep.Summary = exitRefused, "Refused: a run of the binary alone has no server to check."
		return rep
	}
	rep.Check = &c
	st.Verdict = c.Verdict
	// A check after the upgrade's own was cut short is the run's check: it
	// is recorded as one, and what follows a healthy one follows it on the
	// table (the seventh review).
	if st.last() == stepStarted {
		j.step(stepChecked)
	} else {
		j.persist()
	}
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
