package upgrade

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	err := j.at("swap.stop")
	if err == nil {
		_, err = docker.Run(ctx, "stop", "--time", strconv.Itoa(int(r.deps.StopWait.Seconds())), cs.Name)
	}
	if err != nil {
		if r.containerRunning(ctx, cs.ID, false) {
			// It runs as it did: nothing changed.
			j.unstep(stepStopSent)
			rep.ExitCode, rep.Summary = exitRefused, "Refused, and nothing changed: "+cs.Name+" could not be stopped: "+firstLine(err.Error())
			r.discard(j.dir)
			rep.Run = nil
			return
		}
		j.goBack(ctx, cs.Name+" did not stop cleanly: "+firstLine(err.Error()))
		return
	}
	_ = j.step(stepStopped)
	j.done("stopped %s", cs.Name)

	if err := j.at("swap.archive"); err != nil {
		j.goBack(ctx, "the archive of the volume "+cs.Volume+" failed: "+err.Error())
		return
	}
	script := fmt.Sprintf("umask 077 && tar czf /backup/data.tar.gz -C /data . && chown %d:%d /backup/data.tar.gz", os.Getuid(), os.Getgid())
	_, err = docker.Run(ctx, "run", "--rm",
		"--mount", csvField("type=volume", "src="+cs.Volume, "dst=/data", "readonly"),
		"--mount", csvField("type=bind", "src="+j.dir, "dst=/backup"),
		busybox, "sh", "-c", script)
	archive := filepath.Join(j.dir, "data.tar.gz")
	var rb ReadBack
	if err == nil {
		rb, err = readBack(archive, Archived{DBSize: -1})
	}
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

	err = j.at("swap.rename")
	if err == nil {
		_, err = docker.Run(ctx, "rename", cs.Name, j.before())
	}
	if err != nil {
		j.goBack(ctx, "the rename of "+cs.Name+" failed: "+firstLine(err.Error()))
		return
	}
	_ = j.step(stepRenamedOld)
	st.SetAside = append(st.SetAside, "container "+j.before())
	if _, err := docker.Run(ctx, "update", "--restart", "no", j.before()); err != nil {
		j.goBack(ctx, "the restart policy of "+j.before()+" could not be set to no: "+firstLine(err.Error()))
		return
	}
	j.done("renamed %s to %s, restart policy no", cs.Name, j.before())

	args := runArgs(j.inspect, j.image, cs.Name, cs.NewRef, filepath.Join(j.dir, "env"), "")
	var out []byte
	err = j.at("swap.run")
	if err == nil {
		out, err = docker.Run(ctx, args...)
	}
	if err != nil {
		j.goBack(ctx, "the new container did not start: "+firstLine(err.Error()))
		return
	}
	cs.NewID = strings.TrimSpace(string(out))
	_ = j.step(stepStarted)
	j.done("ran %s as %s", cs.NewRef, cs.Name)

	if err := j.at("swap.check"); err != nil {
		j.goBack(ctx, "not healthy: "+err.Error())
		return
	}
	c := r.check(ctx, cs.URL, p.to, st.CountBefore, func() bool { return r.containerRunning(ctx, cs.NewID, true) })
	c.LogLine = r.containerFirstLog(ctx, cs.NewID)
	j.verdict(ctx, c)
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
	out, err := r.deps.Docker.Run(ctx, "logs", id)
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
