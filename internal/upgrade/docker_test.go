//go:build unix

package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// imageEnv is the image's own environment, as its Dockerfile sets it.
var imageEnv = []string{"PATH=/", "TRACEPAD_DATA_DIR=/data", "TRACEPAD_LISTEN=:4318", "TRACEPAD_IN_CONTAINER=1"}

// fakeDocker is the docker CLI over containers in memory. It records every
// call; a container is the person's in this release (spec 054 #36), and a
// test fails on any call but the two the plan reads with.
type fakeDocker struct {
	t      *testing.T
	calls  [][]string
	byName map[string]*fakeContainer
	// down is a daemon that does not answer; noImage, images that cannot be
	// inspected.
	down, noImage bool
}

type fakeContainer struct {
	inspectContainer
	version string
	// mute does not answer.
	mute bool
}

func newFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()
	return &fakeDocker{t: t, byName: map[string]*fakeContainer{}}
}

// add runs a container of version v, named name, publishing 4318 on host:port,
// with its data in volume.
// The image's own configuration (the Dockerfile's ENTRYPOINT and
// TRACEPAD_LISTEN) comes with each.
func (d *fakeDocker) add(name, v, host, port, volume string, labels map[string]string) *fakeContainer {
	c := &fakeContainer{version: v}
	c.ID = fmt.Sprintf("%064d", len(d.byName)+1)
	c.Name = "/" + name
	c.Config.Image = "ghcr.io/tracepad/tracepad:" + v
	c.Config.Labels = labels
	c.Image = "sha256:image-" + v
	c.Config.Entrypoint = []string{"/tracepad"}
	c.Config.Env = append(slices.Clone(imageEnv), "TRACEPAD_URL=http://localhost:"+port)
	c.HostConfig.PortBindings = map[string][]portBinding{"4318/tcp": {{HostIP: host, HostPort: port}}}
	c.Mounts = []mount{{Type: "volume", Name: volume, Destination: "/data", RW: true}}
	d.byName[name] = c
	return c
}

func (d *fakeDocker) Run(ctx context.Context, args ...string) ([]byte, error) {
	d.calls = append(d.calls, slices.Clone(args))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.down {
		return nil, errors.New("Cannot connect to the Docker daemon")
	}
	switch {
	case strings.Join(args, " ") == "ps -q --no-trunc":
		var ids []string
		for _, c := range d.byName {
			ids = append(ids, c.ID)
		}
		slices.Sort(ids)
		return []byte(strings.Join(ids, "\n")), nil
	case len(args) > 2 && args[0] == "image" && args[1] == "inspect":
		if d.noImage {
			return nil, errors.New("No such image")
		}
		var list []imageConfig
		for _, id := range args[2:] {
			var img imageConfig
			img.ID, img.Config.Env, img.Config.Entrypoint = id, imageEnv, []string{"/tracepad"}
			list = append(list, img)
		}
		return json.Marshal(list)
	case len(args) > 2 && args[0] == "container" && args[1] == "inspect":
		var list []inspectContainer
		for _, ref := range args[2:] {
			for _, c := range d.byName {
				if c.ID == ref {
					list = append(list, c.inspectContainer)
				}
			}
		}
		return json.Marshal(list)
	}
	d.t.Errorf("the command acted on a container: docker %s", strings.Join(args, " "))
	return nil, errors.New("not here")
}

// RoundTrip answers /health for whatever container publishes the asked port.
func (d *fakeDocker) RoundTrip(req *http.Request) (*http.Response, error) {
	for _, c := range d.byName {
		if c.mute {
			continue
		}
		for _, bs := range c.HostConfig.PortBindings {
			for _, b := range bs {
				host := b.HostIP
				if host == "" || host == "0.0.0.0" {
					host = "127.0.0.1"
				}
				if req.URL.Host != host+":"+b.HostPort || req.URL.Path != "/health" {
					continue
				}
				body := fmt.Sprintf(`{"status":"ok","version":%q}`, c.version)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
			}
		}
	}
	return nil, errors.New("connection refused")
}

func containerDeps(t *testing.T, d *fakeDocker) Deps {
	t.Helper()
	home := t.TempDir()
	mirror := t.TempDir()
	for _, v := range []string{"0.1.0", "0.2.0"} {
		mirrorRelease(t, mirror, v, v, v == "0.2.0")
	}
	return Deps{
		Sys:        noProcesses{},
		Docker:     d,
		HTTP:       &http.Client{Transport: d},
		Releases:   fakeReleases(t, mirror),
		InstallDir: filepath.Join(home, ".local", "bin"),
		Backups:    filepath.Join(home, "tracepad-backups"),
		Home:       home,
		Cwd:        home,
		Self:       "/bin/sh",
		Getenv:     func(string) string { return "" },
		LookPath:   func(string) string { return "" },
		Version:    scriptVersion,
		Skills:     func(context.Context, string, string, ...string) (string, error) { return "", nil },
		Now:        time.Now,
		Sleep:      func(context.Context, time.Duration) error { return nil },
		StopWait:   time.Second,
		ProbeWait:  time.Millisecond,
	}
}

// noProcesses is a machine with no server process.
type noProcesses struct{}

func (noProcesses) Candidates(context.Context) ([]Process, int, error) { return nil, 0, nil }
func (noProcesses) Inspect(int) (Process, error)                       { return Process{}, errors.New("none") }
func (noProcesses) Alive(int) bool                                     { return false }
func (noProcesses) Signal(int, syscall.Signal) error                   { return errors.New("none") }
func (noProcesses) Start(StartSpec) (Started, error)                   { return nil, errors.New("none") }

func runReport(t *testing.T, deps Deps, args ...string) (Report, int) {
	t.Helper()
	var out bytes.Buffer
	code := run(context.Background(), Options{Args: append(args, "--json"), Stdout: &out, Stderr: io.Discard}, deps)
	var rep Report
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	return rep, code
}

// A container is the person's (#36): the plan names it with the commands
// that upgrade it, and the upgrade refuses it the same way — exit 4, nothing
// made, no docker call but the plan's two.
func TestAContainerGetsTheCommandsThatUpgradeIt(t *testing.T) {
	t.Parallel()
	d := newFakeDocker(t)
	c := d.add("tracepad-app", "0.1.0", "127.0.0.1", "4318", "tracepad-app", nil)
	const secret = "demo:tp-pk-1:tp-sk-not-to-be-printed"
	c.Config.Env = append(c.Config.Env, "TRACEPAD_PROJECTS="+secret)
	c.Config.Cmd = []string{"serve"}
	c.HostConfig.RestartPolicy.Name = "always"
	c.Mounts = append(c.Mounts, mount{Type: "bind", Source: "/srv/my config", Destination: "/etc/extra", RW: false})
	deps := containerDeps(t, d)
	for _, mode := range [][]string{{"--plan"}, {}} {
		rep, code := runReport(t, deps, mode...)
		if code != exitDecide || rep.Run != nil {
			t.Fatalf("%q: %d %s", mode, code, rep.Summary)
		}
		all := strings.Join(rep.Person, "\n")
		// The run as the container was created (the live run of rc.3): its
		// ports, mounts, restart policy and command, and the variables it was
		// given in a file read from Docker — named, never printed.
		for _, want := range []string{"docker stop tracepad-app", "src=tracepad-app,dst=/data,readonly", "sh -c 'umask 077 && set -C && tar czf - -C /data . > /backup/tracepad-app-0.1.0.tar.gz'",
			"docker pull ghcr.io/tracepad/tracepad:0.2.0", "docker rename tracepad-app tracepad-app-old", "docker/#upgrading",
			"docker rename tracepad-app tracepad-app-old && (umask 077 && set -C && docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' tracepad-app-old | grep -E '^(TRACEPAD_PROJECTS|TRACEPAD_URL)=' > tracepad-app.upgrade.env) && ",
			"docker run -d --name tracepad-app --env-file tracepad-app.upgrade.env -p 127.0.0.1:4318:4318 -v tracepad-app:/data -v '/srv/my config:/etc/extra:ro' --restart always ghcr.io/tracepad/tracepad:0.2.0 serve && rm tracepad-app.upgrade.env. Once the new one is healthy: docker rm tracepad-app-old",
			"Stopped after the rename: docker rm tracepad-app if it was made, then rm -f tracepad-app.upgrade.env; docker rename tracepad-app-old tracepad-app && docker start tracepad-app"} {
			if !strings.Contains(all, want) {
				t.Errorf("%q: the commands miss %q: %s", mode, want, all)
			}
		}
		if out, _ := json.Marshal(rep); strings.Contains(string(out), "tp-sk-not-to-be-printed") {
			t.Errorf("%q: the report prints a variable's value", mode)
		}
		if len(rep.Containers) != 1 || rep.Containers[0].Whose != "person" || rep.Containers[0].Version != "0.1.0" {
			t.Errorf("%q: %+v", mode, rep.Containers)
		}
	}
	if entries, _ := os.ReadDir(deps.Backups); len(entries) > 0 {
		t.Errorf("a run was made: %v", entries)
	}
}

// Compose's container is upgraded through its Compose file; one already at
// the version is not called behind. Each is asked where its server listens
// — TRACEPAD_LISTEN or --listen, published on any address of this machine —
// not at 4318 only (the final review). One that publishes it and does not
// answer may be behind, and gets the commands (the audit of #223); one whose
// address cannot be told is said not checked, never called behind (the
// final review); a daemon that does not answer is said, never taken for
// none.
func TestWhatThePlanSaysOfContainers(t *testing.T) {
	t.Parallel()
	d := newFakeDocker(t)
	d.add("obs-tracepad-1", "0.1.0", "127.0.0.1", "4318", "obs_data", map[string]string{"com.docker.compose.project": "obs"})
	d.add("tracepad-current", "0.2.0", "127.0.0.1", "4319", "current", nil)
	d.add("tracepad-open", "0.1.0", "0.0.0.0", "4320", "open", nil)
	moved := d.add("tracepad-moved", "0.2.0", "127.0.0.1", "18080", "moved", nil)
	moved.Config.Env = []string{"TRACEPAD_LISTEN=:8080"}
	moved.HostConfig.PortBindings = map[string][]portBinding{"8080/tcp": {{HostIP: "127.0.0.1", HostPort: "18080"}}}
	flag := d.add("tracepad-flag", "0.1.0", "127.0.0.1", "18081", "flag", nil)
	flag.Config.Cmd = []string{"serve", "--listen", ":9090"}
	flag.HostConfig.PortBindings = map[string][]portBinding{"9090/tcp": {{HostIP: "127.0.0.1", HostPort: "18081"}}}
	d.add("tracepad-mute", "0.1.0", "127.0.0.1", "4321", "mute", nil).mute = true
	unpublished := d.add("tracepad-inside", "0.1.0", "127.0.0.1", "4322", "inside", nil)
	unpublished.HostConfig.PortBindings = nil
	rep, code := runReport(t, containerDeps(t, d), "--plan")
	all, notes := strings.Join(rep.Person, "\n"), strings.Join(rep.Notes, "\n")
	for _, want := range []string{"docker compose up -d", "the project obs", "container tracepad-open runs 0.1.0",
		"container tracepad-flag runs 0.1.0", "container tracepad-mute does not say its version"} {
		if !strings.Contains(all, want) {
			t.Errorf("the plan misses %q:\n%s", want, all)
		}
	}
	for _, not := range []string{"tracepad-current", "tracepad-moved", "tracepad-inside"} {
		if strings.Contains(all, not) {
			t.Errorf("%s is called behind:\n%s", not, all)
		}
	}
	if code != exitDecide || !strings.Contains(notes, "container tracepad-inside was not checked: its server listens on port 4318 inside, which it does not publish") {
		t.Errorf("%d %s\n%s", code, rep.Summary, notes)
	}

	d.down = true
	rep, code = runReport(t, containerDeps(t, d), "--plan")
	if code != exitOK || !strings.Contains(strings.Join(rep.Notes, " "), "Docker did not answer") {
		t.Errorf("a daemon down: %d %q", code, rep.Notes)
	}
}

// An image that cannot be inspected leaves the container's run to the
// person, said: its options are in docker inspect, never guessed.
func TestAContainerWhoseImageCannotBeReadGetsTheSentence(t *testing.T) {
	t.Parallel()
	d := newFakeDocker(t)
	d.noImage = true
	d.add("tracepad-app", "0.1.0", "127.0.0.1", "4318", "tracepad-app", nil)
	rep, _ := runReport(t, containerDeps(t, d), "--plan")
	if all := strings.Join(rep.Person, "\n"); !strings.Contains(all, "with the options tracepad-app was created with, which the command could not write (its image could not be read (No such image); docker inspect tracepad-app-old has them)") {
		t.Errorf("%s", all)
	}
}

// fakeDockerCLI is a `docker` for a shell: it logs each call, runs a
// backup's own sh -c against a data directory of its own, and answers an
// inspect with two variables.
const fakeDockerCLI = `#!/bin/sh
echo "$*" >> "$LOG"
case "$1 $2" in
"run --rm")
	eval "cmd=\${$#}"
	cmd=$(printf '%s' "$cmd" | sed "s#> /backup/#> $PWD/#; s#-C /data#-C $DATA#")
	exec sh -c "$cmd" ;;
"inspect --format") printf 'TRACEPAD_PROJECTS=p\nTRACEPAD_URL=u\n' ;;
esac
exit 0
`

// The person's commands for a container are one chain (the review of
// #225): a backup refused because one of its name is there, or a file of
// variables left from before, stops every step after it — nothing is
// pulled, renamed or run on a volume the backup did not take.
func TestAContainersStepsStopAtTheFirstThatFails(t *testing.T) {
	t.Parallel()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeDockerCLI), 0o755); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	_ = os.WriteFile(filepath.Join(data, dataDBName), []byte("db"), 0o600)
	c := Container{Name: "tracepad-app", Repo: "ghcr.io/tracepad/tracepad", Version: "0.1.0", DataMount: "type=volume,src=tracepad-app",
		Run: []string{"-p", "127.0.0.1:4318:4318", "-v", "tracepad-app:/data", imageSlot, "serve"}, EnvNames: []string{"TRACEPAD_PROJECTS", "TRACEPAD_URL"}}
	chain, _ := containerSteps(c, "0.2.0")
	run := func(t *testing.T, before func(dir string)) (calls string, err error, dir string) {
		dir = t.TempDir()
		before(dir)
		log := filepath.Join(dir, "calls")
		cmd := exec.Command("sh", "-c", chain)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "LOG=" + log, "DATA=" + data}
		err = cmd.Run()
		b, _ := os.ReadFile(log)
		return string(b), err, dir
	}
	archive, env := "tracepad-app-0.1.0.tar.gz", "tracepad-app.upgrade.env"

	calls, err, dir := run(t, func(string) {})
	if err != nil || !strings.Contains(calls, "run -d --name tracepad-app --env-file "+env) {
		t.Fatalf("a clean run: %v\n%s", err, calls)
	}
	if info, err := os.Stat(filepath.Join(dir, archive)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the backup: %v %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(dir, env)); err == nil {
		t.Error("the file of variables was left")
	}

	calls, err, _ = run(t, func(dir string) { _ = os.WriteFile(filepath.Join(dir, archive), []byte("an earlier backup"), 0o600) })
	if err == nil || strings.Contains(calls, "pull") || strings.Contains(calls, "rename") {
		t.Errorf("a backup refused went on: %v\n%s", err, calls)
	}

	calls, err, dir = run(t, func(dir string) {
		_ = os.WriteFile(filepath.Join(dir, env), []byte("TRACEPAD_PROJECTS=stale\n"), 0o600)
	})
	if err == nil || strings.Contains(calls, "run -d") {
		t.Errorf("a file of variables left from before was used: %v\n%s", err, calls)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, env)); string(b) != "TRACEPAD_PROJECTS=stale\n" {
		t.Errorf("the file there was written over: %q", b)
	}
}

// The run is written as the container was made (the review of #225): -P
// when it published every port, an entrypoint cleared at its creation
// cleared again, and a mount it cannot write refuses rather than becomes
// another.
func TestTheRunIsTheContainersOwn(t *testing.T) {
	t.Parallel()
	var img imageConfig
	img.Config.Entrypoint, img.Config.Env = []string{"/tracepad"}, imageEnv
	var ic inspectContainer
	ic.HostConfig.PublishAllPorts = true
	ic.Config.Cmd = []string{"/tracepad", "serve", "--listen", ":8080"}
	ic.Config.Env = imageEnv
	run, _, err := createdAs(ic, img)
	want := []string{"-P", "--entrypoint", "", imageSlot, "/tracepad", "serve", "--listen", ":8080"}
	if err != nil || !slices.Equal(run, want) {
		t.Errorf("%q %v, want %q", run, err, want)
	}
	c := Container{Name: "t", Repo: "ghcr.io/tracepad/tracepad", Run: run}
	if chain, _ := containerSteps(c, "0.2.0"); !strings.Contains(chain, "docker run -d --name t -P --entrypoint '' ghcr.io/tracepad/tracepad:0.2.0 /tracepad serve --listen :8080") {
		t.Errorf("%s", chain)
	}
	ic.Mounts = []mount{{Type: "npipe", Source: `\\.\pipe\docker_engine`, Destination: "/pipe"}}
	if _, _, err := createdAs(ic, img); err == nil || !strings.Contains(err.Error(), "a mount of type npipe") {
		t.Errorf("an npipe mount: %v", err)
	}
}
