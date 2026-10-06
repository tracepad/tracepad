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
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeDocker is the docker CLI over containers in memory. It records every
// call; a container is the person's in this release (spec 054 #36), and a
// test fails on any call but the two the plan reads with.
type fakeDocker struct {
	t      *testing.T
	calls  [][]string
	byName map[string]*fakeContainer
	// down is a daemon that does not answer.
	down bool
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
	c.Config.Entrypoint = []string{"/tracepad"}
	c.Config.Env = []string{"PATH=/", "TRACEPAD_LISTEN=:4318"}
	c.HostConfig.PortBindings = map[string][]portBinding{"4318/tcp": {{HostIP: host, HostPort: port}}}
	c.Mounts = []mount{{Type: "volume", Name: volume, Destination: "/data"}}
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
	d.add("tracepad-app", "0.1.0", "127.0.0.1", "4318", "tracepad-app", nil)
	deps := containerDeps(t, d)
	for _, mode := range [][]string{{"--plan"}, {}} {
		rep, code := runReport(t, deps, mode...)
		if code != exitDecide || rep.Run != nil {
			t.Fatalf("%q: %d %s", mode, code, rep.Summary)
		}
		all := strings.Join(rep.Person, "\n")
		for _, want := range []string{"docker stop tracepad-app", "src=tracepad-app,dst=/data,readonly", "umask 077",
			"docker pull ghcr.io/tracepad/tracepad:0.2.0", "docker rename tracepad-app tracepad-app-old", "docker/#upgrading"} {
			if !strings.Contains(all, want) {
				t.Errorf("%q: the commands miss %q: %s", mode, want, all)
			}
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
