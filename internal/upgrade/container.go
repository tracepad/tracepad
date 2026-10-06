package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// prepareContainer pulls the new image, saves the old container's and
// image's inspect, writes the env file the person's variables go through, and
// measures the volume. It answers the room the backup needs.
func (j *job) prepareContainer(ctx context.Context, p *plan) (int64, string) {
	r, c, st := j.r, p.container, j.st
	newRef := c.Repo + ":" + p.to
	if _, err := r.deps.Docker.Run(ctx, "pull", "-q", newRef); err != nil {
		return 0, "could not pull " + newRef + ": " + firstLine(err.Error())
	}
	j.done("pulled %s", newRef)
	if !r.exists(ctx, "image", busybox) {
		if _, err := r.deps.Docker.Run(ctx, "pull", "-q", busybox); err != nil {
			return 0, "could not pull " + busybox + ", which archives the volume: " + firstLine(err.Error())
		}
	}
	list, err := r.inspectContainers(ctx, c.Name)
	if err != nil || len(list) != 1 {
		return 0, "could not inspect " + c.Name
	}
	img, err := r.inspectImage(ctx, list[0].Image)
	if err != nil {
		return 0, "could not inspect its image: " + firstLine(err.Error())
	}
	if reason := containerRefusal(list[0], img, r.flags.container); reason != "" {
		return 0, c.Name + " changed since the plan: " + reason
	}
	j.inspect, j.image = list[0], img
	if err := writeJSON(filepath.Join(j.dir, "container.json"), j.inspect); err != nil {
		return 0, err.Error()
	}
	if err := writeJSON(filepath.Join(j.dir, "image.json"), j.image); err != nil {
		return 0, err.Error()
	}
	env := strings.Join(personEnv(j.inspect.Config.Env, img.Config.Env), "\n")
	if env != "" {
		env += "\n"
	}
	if err := os.WriteFile(filepath.Join(j.dir, "env"), []byte(env), 0o600); err != nil {
		return 0, err.Error()
	}
	kb, err := r.volumeKB(ctx, c.Volume)
	if err != nil {
		return 0, "could not measure the volume " + c.Volume + ": " + firstLine(err.Error())
	}
	hc := j.inspect.HostConfig
	st.Container = &ContainerState{Name: c.Name, ID: j.inspect.ID, Volume: c.Volume, URL: c.URL,
		OldRef: c.Ref, OldImage: j.inspect.Image, NewRef: newRef,
		Restart: restartArg(hc.RestartPolicy.Name, hc.RestartPolicy.MaximumRetryCount)}
	return kb << 10, ""
}

func (j *job) before() string { return j.st.Container.Name + "-before-" + j.st.Run }
func (j *job) after() string  { return j.st.Container.Name + "-after-" + j.st.Run }
func (j *job) failed() string { return j.st.Container.Name + "-failed-" + j.st.Run }

// swapContainer stops the container, archives its volume, sets it aside and
// runs the new image with what the old one had (Decision 10).
func (j *job) swapContainer(ctx context.Context, p *plan) {
	r, st, rep := j.r, j.st, j.rep
	cs := st.Container
	docker := r.deps.Docker
	if err := j.step(stepStopSent); err != nil {
		j.stuck("could not record the run's state: " + err.Error())
		return
	}
	err := j.at(stepStopSent)
	if err == nil {
		err = r.askToStop(ctx, cs.Name)
	}
	if err != nil {
		if r.containerRunning(ctx, cs.ID, false) {
			// It runs as it did: nothing changed, its restart policy too.
			j.unstep(stepStopSent)
			rep.ExitCode, rep.Summary = exitRefused, "Refused, and nothing changed: "+cs.Name+" could not be stopped: "+firstLine(err.Error())
			if _, perr := docker.Run(ctx, "update", "--restart", cs.Restart, cs.Name); perr != nil {
				rep.ExitCode, rep.Summary = exitStuck, cs.Name+" could not be stopped ("+firstLine(err.Error())+"), and its restart policy, set to no for the stop, could not be put back to "+cs.Restart+": "+firstLine(perr.Error())
				rep.Next = append(rep.Next, "docker update --restart "+shq(cs.Restart)+" "+shq(cs.Name))
			}
			r.discard(j.dir)
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
	_ = j.step(stepStopped)
	j.done("stopped %s", cs.Name)

	script := fmt.Sprintf("umask 077 && tar czf /backup/data.tar.gz -C /data . && chown %d:%d /backup/data.tar.gz", os.Getuid(), os.Getgid())
	archive := filepath.Join(j.dir, "data.tar.gz")
	var rb ReadBack
	err = j.act(stepArchived, "archive the volume "+cs.Volume+" into "+archive, func() error {
		_, err := docker.Run(ctx, "run", "--rm",
			"--mount", csvField("type=volume", "src="+cs.Volume, "dst=/data", "readonly"),
			"--mount", csvField("type=bind", "src="+j.dir, "dst=/backup"),
			busybox, "sh", "-c", script)
		if err == nil {
			rb, err = readBack(archive, Archived{DBSize: -1})
		}
		return err
	})
	if err != nil {
		j.goBack(ctx, "the archive of the volume "+cs.Volume+" failed: "+firstLine(err.Error()))
		return
	}
	// The archive's bytes are pinned as the process archive's are: a file
	// changed between the upgrade and a way back is not restored (the second
	// review).
	st.Archive = &Archived{SHA256: rb.SHA256, DBSize: -1}
	_ = j.step(stepArchived)
	j.done("archived the volume %s into %s and read it back whole", cs.Volume, archive)

	err = j.act(stepRenamedOld, "rename the container "+cs.Name+" to "+j.before(), func() error {
		_, err := docker.Run(ctx, "rename", cs.Name, j.before())
		return err
	})
	if err != nil {
		j.goBack(ctx, "the rename of "+cs.Name+" failed: "+firstLine(err.Error()))
		return
	}
	// Its restart policy is no already, since the stop, and a rename keeps it.
	st.SetAside = append(st.SetAside, "container "+j.before())
	_ = j.step(stepRenamedOld)
	j.done("renamed %s to %s, restart policy no", cs.Name, j.before())

	args := runArgs(j.inspect, j.image, cs.Name, cs.NewRef, filepath.Join(j.dir, "env"), "")
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
	_ = j.step(stepStarted)
	j.done("ran %s as %s", cs.NewRef, cs.Name)

	if err := j.at(stepChecked); err != nil {
		j.goBack(ctx, "not healthy: "+err.Error())
		return
	}
	c := r.check(ctx, cs.URL, p.to, st.CountBefore, func() bool { return r.containerRunning(ctx, cs.NewID, true) })
	c.LogLine = r.containerFirstLog(ctx, cs.NewID)
	j.verdict(ctx, c)
}

// askToStop stops a container the way a server is stopped: SIGTERM, never
// the SIGKILL `docker stop` sends after its timeout (the sixth review). Its
// restart policy is set to no first, or docker would start again what the
// signal stopped; the way back puts it back.
func (r *runner) askToStop(ctx context.Context, name string) error {
	if _, err := r.deps.Docker.Run(ctx, "update", "--restart", "no", name); err != nil {
		return err
	}
	_, err := r.deps.Docker.Run(ctx, "kill", "--signal", "TERM", name)
	return err
}

// waitStopped waits, as a server's stop does, for a container asked to stop.
func (r *runner) waitStopped(ctx context.Context, id string) bool {
	deadline := r.deps.Now().Add(r.deps.StopWait)
	for {
		if list, err := r.inspectContainers(ctx, id); err == nil && len(list) == 1 && !list[0].State.Running {
			return true
		}
		if !r.deps.Now().Before(deadline) || r.deps.Sleep(ctx, 250*time.Millisecond) != nil {
			return false
		}
	}
}

// containerRunning says whether the container with this id runs. Docker
// keeps a crash-looping container Running under a restart policy, with
// Restarting set and its count growing (the review of #1): that is not
// running. fresh is a container this run just made, which has no business
// having restarted at all.
func (r *runner) containerRunning(ctx context.Context, id string, fresh bool) bool {
	if id == "" {
		return false
	}
	list, err := r.inspectContainers(ctx, id)
	if err != nil || len(list) != 1 {
		return false
	}
	c := list[0]
	return c.State.Running && !c.State.Restarting && (!fresh || c.RestartCount == 0)
}

func (r *runner) containerFirstLog(ctx context.Context, id string) string {
	// This start's log, not the container's from its creation: a container
	// the way back starts again has months of it (the final review).
	args := []string{"logs"}
	if list, err := r.inspectContainers(ctx, id); err == nil && len(list) == 1 && list[0].State.StartedAt != "" {
		args = append(args, "--since", list[0].State.StartedAt)
	}
	out, err := r.deps.Docker.Run(ctx, append(args, id)...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(firstLine(string(out)))
}

// readContainer reads container.json and image.json back.
func readContainer(dir string) (inspectContainer, inspectImage, error) {
	var ic inspectContainer
	var img inspectImage
	b, err := os.ReadFile(filepath.Join(dir, "container.json"))
	if err != nil {
		return ic, img, err
	}
	if err := json.Unmarshal(b, &ic); err != nil {
		return ic, img, err
	}
	b, err = os.ReadFile(filepath.Join(dir, "image.json"))
	if err != nil {
		return ic, img, err
	}
	return ic, img, json.Unmarshal(b, &img)
}
