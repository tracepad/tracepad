package upgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
)

// settle finishes, or takes back, the act a run was cut short in (#34). Each
// act writes what it is about to do before it does it; a run loaded with
// that intent still pending was stopped between the act and its step — a
// kill, a crash, a disk that would not take the step — and what is on disk
// says which side of the act it is on. An act that happened is recorded as
// it would have been; one that did not is taken back, and the path that
// runs next does it again. When the disk says neither, nothing is touched,
// and the refusal says what to look at.
func (j *job) settle(ctx context.Context) error {
	it := j.st.Pending
	if it == nil {
		return nil
	}
	done, err := j.happened(ctx, it.Step)
	switch {
	case errors.Is(err, errKeep):
		return nil
	case err != nil:
		return fmt.Errorf("the run was cut short as it was to %s, and what is on disk does not say whether that happened: %v. Nothing was touched", it.What, err)
	case !done:
		j.drop()
		return nil
	case !allows(j.st.Kind, j.st.last(), it.Step):
		return fmt.Errorf("the run was cut short after it %s, and its steps cannot record that after %q; nothing was touched", it.What, j.st.last())
	}
	j.step(it.Step)
	j.done("found that the run had done this before it was cut short, and recorded it: %s", it.What)
	return nil
}

// happened says whether the act of a step is done, from what is on disk.
// A step whose act is done again safely — an archive, a restore beside the
// data, a binary put in place by its version — answers no, and is done
// again.
func (j *job) happened(ctx context.Context, step string) (bool, error) {
	st := j.st
	// What cannot be looked at is neither there nor not (the eighth review):
	// the first such error stops the settling, and nothing is touched.
	var unread error
	exists := func(p string) bool {
		_, err := os.Lstat(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) && unread == nil {
			unread = err
		}
		return err == nil
	}
	version := func() (string, error) {
		v, err := j.r.deps.Version(ctx, st.Binary.Path)
		if err != nil {
			return "", fmt.Errorf("%s does not say its version: %w", st.Binary.Path, err)
		}
		return v, nil
	}
	switch {
	case step == stepBackBinary:
		if !exists(st.Binary.Path) {
			return false, unread
		}
		v, err := version()
		want := st.Binary.From
		if st.Kind == kindProcess {
			want = st.From
		}
		return v == want, err
	case st.Kind == kindContainer:
		return j.settleContainer(ctx, step)
	case st.Kind == kindProcess:
		ps := st.Process
		after, restore := ps.DataDir+".after-"+st.Run, ps.DataDir+".restore-"+st.Run
		switch step {
		case stepBackAside:
			a, d := exists(after), exists(ps.DataDir)
			switch {
			case unread != nil:
				return false, unread
			case a && !d:
				if !slices.Contains(st.SetAside, after) {
					st.SetAside = append(st.SetAside, after)
				}
				return true, nil
			case d && !a:
				return false, nil
			}
			return false, fmt.Errorf("%s and %s are both there, or neither is", ps.DataDir, after)
		case stepBackMoved:
			d, r := exists(ps.DataDir), exists(restore)
			switch {
			case unread != nil:
				return false, unread
			case d && !r:
				return true, nil
			case r && !d:
				return false, nil
			}
			return false, fmt.Errorf("%s and %s are both there, or neither is", restore, ps.DataDir)
		case stepBackCleared:
			if !exists(st.Binary.Path) {
				return unread == nil, unread
			}
			v, err := version()
			return v == st.From, err
		case stepStarted, stepBackStarted:
			// The server it started holds the data, as the run recorded it.
			if j.spec.Exe == "" {
				spec, err := j.loadSpec()
				if err != nil {
					return false, err
				}
				j.spec = spec
			}
			held, err := lockHeld(ps.DataDir)
			if err != nil {
				return false, err
			}
			if !held {
				return false, nil
			}
			holder, err := lockedBy(ps.DataDir)
			if err != nil {
				return false, fmt.Errorf("%s is locked, and its lock does not say by whom: %w", ps.DataDir, err)
			}
			ok, err := j.r.isServer(holder, 0, ps.DataDir, j.spec)
			if err != nil && j.r.deps.Sys.Alive(holder) {
				return false, err
			}
			if !ok {
				return false, nil
			}
			if step == stepStarted {
				ps.NewPID, ps.NewStart = holder, j.r.startOf(holder)
			} else {
				ps.BackPID, ps.BackStart = holder, j.r.startOf(holder)
			}
			return true, nil
		}
	}
	return false, nil
}
