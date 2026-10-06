package upgrade

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
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
	if err := j.step(it.Step); err != nil {
		return fmt.Errorf("could not record the run's state: %w", err)
	}
	j.done("found that the run had done this before it was cut short, and recorded it: %s", it.What)
	return nil
}

// happened says whether the act of a step is done, from what is on disk.
// A step whose act is done again safely — an archive, a restore beside the
// data, a binary put in place by its version — answers no, and is done
// again.
func (j *job) happened(ctx context.Context, step string) (bool, error) {
	st := j.st
	exists := func(p string) bool { _, err := os.Lstat(p); return err == nil }
	switch {
	case step == stepBackBinary:
		v, err := j.r.deps.Version(ctx, st.Binary.Path)
		want := st.Binary.From
		if st.Kind == kindProcess {
			want = st.From
		}
		return err == nil && v == want, nil
	case st.Kind == kindProcess:
		ps := st.Process
		after, restore := ps.DataDir+".after-"+st.Run, ps.DataDir+".restore-"+st.Run
		switch step {
		case stepBackAside:
			switch {
			case exists(after) && !exists(ps.DataDir):
				if !slices.Contains(st.SetAside, after) {
					st.SetAside = append(st.SetAside, after)
				}
				return true, nil
			case exists(ps.DataDir) && !exists(after):
				return false, nil
			}
			return false, fmt.Errorf("%s and %s are both there, or neither is", ps.DataDir, after)
		case stepBackMoved:
			switch {
			case exists(ps.DataDir) && !exists(restore):
				return true, nil
			case exists(restore) && !exists(ps.DataDir):
				return false, nil
			}
			return false, fmt.Errorf("%s and %s are both there, or neither is", restore, ps.DataDir)
		case stepBackCleared:
			if !exists(st.Binary.Path) {
				return true, nil
			}
			v, err := j.r.deps.Version(ctx, st.Binary.Path)
			return err == nil && v == st.From, nil
		case stepStarted, stepBackStarted:
			// The server it started holds the data, as the run recorded it.
			if j.spec.Exe == "" {
				spec, err := j.loadSpec()
				if err != nil {
					return false, err
				}
				j.spec = spec
			}
			if !lockHeld(ps.DataDir) {
				return false, nil
			}
			holder, _ := lockedBy(ps.DataDir)
			if ok, _ := j.r.isServer(holder, ps.DataDir, j.spec); !ok {
				return false, nil
			}
			if step == stepStarted {
				ps.NewPID = holder
			} else {
				ps.BackPID = holder
			}
			return true, nil
		}
	case st.Kind == kindContainer:
		cs := st.Container
		if j.r.deps.Docker == nil {
			return false, errors.New("docker is not on PATH")
		}
		named, _ := j.r.inspectContainers(ctx, cs.Name)
		under := func() *inspectContainer {
			if len(named) == 1 {
				return &named[0]
			}
			return nil
		}()
		switch step {
		case stepRenamedOld:
			before, _ := j.r.inspectContainers(ctx, j.before())
			switch {
			case len(before) == 1 && before[0].ID == cs.ID:
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
			if under != nil && under.ID != cs.ID && everStarted(under) {
				cs.NewID = under.ID
				return true, nil
			}
			return false, nil
		case stepBackVolume:
			// Kept: the way back fills a volume it began again, and knows it
			// for the run's by this intent.
			return false, errKeep
		case stepBackStarted:
			// The old image run on the restored volume, and started; one
			// docker made and could not start is set aside by the next try.
			if under != nil && under.ID != cs.ID && under.ID != cs.NewID && under.Image == cs.OldImage && everStarted(under) {
				cs.BackID = under.ID
				return true, nil
			}
			return false, nil
		}
	}
	return false, nil
}

// errKeep: the intent stays pending, for the act's own path to take up.
var errKeep = errors.New("kept")

// everStarted: a container docker has started at least once.
func everStarted(c *inspectContainer) bool {
	return c.State.Running || (c.State.StartedAt != "" && !strings.HasPrefix(c.State.StartedAt, "0001-01-01"))
}
