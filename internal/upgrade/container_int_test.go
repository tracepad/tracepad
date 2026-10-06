//go:build unix && upgradeint

package upgrade

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// A container's run against a real docker (spec 054 #47): images of the
// binary built here at two versions, and one that does not serve, tagged as
// the release's repository on this machine only; setup's container of the
// old one, with a restart policy, a read-only bind whose path has a space and
// the variables setup gives it. Where no docker answers — a CI runner without
// one, a machine without it — the tests skip, said.

// localImages is docker with the test's images as the registry: a pull of an
// image already here is that image, since the tags exist on this machine
// only. Every other call is docker's.
type localImages struct{ Docker }

func (l localImages) Run(ctx context.Context, args ...string) ([]byte, error) {
	if args[0] == "pull" {
		if _, err := l.Docker.Run(ctx, "image", "inspect", args[len(args)-1]); err == nil {
			return nil, nil
		}
	}
	return l.Docker.Run(ctx, args...)
}

var (
	imagesOnce sync.Once
	imagesErr  error
)

// realDocker is docker, or the test skips.
func realDocker(t *testing.T) Docker {
	t.Helper()
	if testing.Short() {
		t.Skip("builds images")
	}
	d := newDockerCLI()
	if d == nil {
		t.Skip("no docker on PATH")
	}
	if _, err := d.Run(context.Background(), "info", "--format", "{{.ID}}"); err != nil {
		t.Skipf("docker does not answer: %v", err)
	}
	return d
}

// testImages builds the images, once: vOld and vNew of the binary built for
// docker's own platform, and vBroken, whose /tracepad exits. Each is tagged
// ghcr.io/tracepad/tracepad:<v>, removed when the tests end.
func testImages(t *testing.T, d Docker) {
	t.Helper()
	imagesOnce.Do(func() {
		ctx := context.Background()
		out, err := d.Run(ctx, "version", "--format", "{{.Server.Arch}}")
		if err != nil {
			imagesErr = err
			return
		}
		arch := strings.TrimSpace(string(out))
		atExit = append(atExit, func() {
			for _, v := range []string{vOld, vNew, vBroken} {
				_, _ = d.Run(context.Background(), "image", "rm", "ghcr.io/tracepad/tracepad:"+v) // ignored: the test's own tags; one in use stays
			}
		})
		dir, err := os.MkdirTemp("", "tracepad-upgrade-images-")
		if err != nil {
			imagesErr = err
			return
		}
		defer os.RemoveAll(dir)
		for _, v := range []string{vOld, vNew, vBroken} {
			ctxDir := filepath.Join(dir, v)
			_ = os.MkdirAll(filepath.Join(ctxDir, "data"), 0o700)
			bin := filepath.Join(ctxDir, "tracepad")
			if v == vBroken {
				imagesErr = os.WriteFile(bin, []byte("#!/bin/sh\necho 'this release does not serve' >&2\nexit 1\n"), 0o755)
			} else {
				cmd := exec.Command("go", "build", "-ldflags", "-X main.version="+v, "-o", bin, "github.com/tracepad/tracepad/cmd/tracepad")
				cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+arch, "CGO_ENABLED=0")
				if b, err := cmd.CombinedOutput(); err != nil {
					imagesErr = fmt.Errorf("go build %s: %v\n%s", v, err, b)
				}
			}
			if imagesErr != nil {
				return
			}
			// The image's shape, as the Dockerfile makes it: /tracepad the
			// entrypoint, /data the image's user's, the listen address and
			// data directory in its environment.
			dockerfile := "FROM " + busybox + "\nCOPY tracepad /tracepad\nCOPY --chown=65532:65532 data /data\n" +
				"ENV TRACEPAD_DATA_DIR=/data TRACEPAD_LISTEN=:4318 TRACEPAD_IN_CONTAINER=1\nEXPOSE 4318\nVOLUME /data\nUSER 65532:65532\nENTRYPOINT [\"/tracepad\"]\n"
			if err := os.WriteFile(filepath.Join(ctxDir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
				imagesErr = err
				return
			}
			if b, err := d.Run(ctx, "build", "-q", "-t", "ghcr.io/tracepad/tracepad:"+v, ctxDir); err != nil {
				imagesErr = fmt.Errorf("docker build %s: %v\n%s", v, err, b)
				return
			}
		}
	})
	if imagesErr != nil {
		t.Fatal(imagesErr)
	}
}

// dockerWorld is a container test's machine: a home with no installed
// binary, a mirror that names the releases, and setup's container.
type dockerWorld struct {
	t      *testing.T
	d      Docker
	name   string
	volume string
	port   int
	url    string
	home   string
	mirror string
}

func newDockerWorld(t *testing.T) *dockerWorld {
	t.Helper()
	real := realDocker(t)
	testImages(t, real)
	bins := builtBinaries(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b) // ignored: crypto/rand.Read does not fail
	w := &dockerWorld{t: t, d: localImages{real}, name: "tracepad-upgradeint-" + hex.EncodeToString(b), home: filepath.Join(root, "home"), mirror: filepath.Join(root, "mirror")}
	w.volume, w.port = w.name, freePort(t)
	w.url = "http://127.0.0.1:" + strconv.Itoa(w.port)
	for _, v := range []string{vOld, vNew, vBroken} {
		publish(t, w.mirror, v, filepath.Join(bins, v))
	}
	_ = os.MkdirAll(w.home, 0o700)
	t.Cleanup(w.cleanup)
	// A read-only bind whose path has a space, as a person's certificates.
	tls := filepath.Join(root, "My Certs")
	_ = os.MkdirAll(tls, 0o755)
	_ = os.WriteFile(filepath.Join(tls, "ca.pem"), []byte("not a certificate"), 0o644)
	if out, err := real.Run(context.Background(), "run", "-d", "--name", w.name, "--restart", "always",
		"-v", w.volume+":/data", "-p", "127.0.0.1:"+strconv.Itoa(w.port)+":4318",
		"--mount", csvField("type=bind", "src="+tls, "dst=/tls", "readonly"),
		"-e", "TRACEPAD_URL="+w.url, "-e", "TRACEPAD_PROJECTS=app:"+testPK+":"+testSK,
		"ghcr.io/tracepad/tracepad:"+vOld, "serve"); err != nil {
		t.Fatalf("docker run: %v %s", err, out)
	}
	w.waitVersion(vOld)
	return w
}

// cleanup removes what the test made — its containers, volumes and, when the
// last world ends, nothing else: the images stay for the next test.
func (w *dockerWorld) cleanup() {
	ctx := context.Background()
	out, _ := w.d.Run(ctx, "ps", "-a", "-q", "--filter", "name="+w.name)
	for _, id := range strings.Fields(string(out)) {
		_, _ = w.d.Run(ctx, "rm", "-f", id)
	}
	out, _ = w.d.Run(ctx, "volume", "ls", "-q", "--filter", "name="+w.volume)
	for _, v := range strings.Fields(string(out)) {
		_, _ = w.d.Run(ctx, "volume", "rm", v)
	}
}

func (w *dockerWorld) deps() Deps {
	getenv := func(k string) string {
		if k == "TRACEPAD_API_KEY" {
			return testSK
		}
		return ""
	}
	install := filepath.Join(w.home, ".local", "bin")
	_ = os.MkdirAll(install, 0o700)
	return Deps{
		Sys:    newSystem(),
		Docker: w.d,
		HTTP:   &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 5 * time.Second},
		Getenv: getenv,
		Releases: &Releases{Base: "file://" + w.mirror, Mirror: true, OS: runtime.GOOS, Arch: runtime.GOARCH, Version: binaryVersion,
			Attest: func(context.Context, string) (string, error) { return "attestation not checked: a test mirror", nil }},
		InstallDir: install,
		Backups:    filepath.Join(w.home, "tracepad-backups"),
		Home:       w.home,
		Cwd:        w.home,
		Self:       filepath.Join(w.home, "upgrader"),
		LookPath:   func(string) string { return "" },
		Version:    binaryVersion,
		Skills:     runSkills,
		Now:        time.Now,
		Sleep:      sleepCtx,
		StopWait:   30 * time.Second,
		HealthWait: 60 * time.Second,
		ProbeWait:  200 * time.Millisecond,
	}
}

func (w *dockerWorld) run(args ...string) (Report, int) {
	w.t.Helper()
	deps := w.deps()
	if _, err := os.Stat(deps.Self); err != nil {
		_ = os.WriteFile(deps.Self, []byte("#!/bin/sh\n"), 0o755)
	}
	ww := &world{t: w.t}
	return ww.run(deps, args...)
}

func (w *dockerWorld) waitVersion(v string) {
	w.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if got, err := health(context.Background(), http.DefaultClient, w.url); err == nil && got == v {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	w.t.Fatalf("%s does not answer as %s", w.url, v)
}

func (w *dockerWorld) count() int64 {
	return (&world{t: w.t, url: w.url}).count()
}

func (w *dockerWorld) sendTrace(id int) {
	(&world{t: w.t, url: w.url}).sendTrace(id)
}

// inspect is the container under name, read by the command's own reading.
func (w *dockerWorld) inspect(name string) *inspectContainer {
	w.t.Helper()
	c, err := (&runner{deps: Deps{Docker: w.d}}).inspectOne(context.Background(), name)
	if err != nil {
		w.t.Fatal(err)
	}
	return c
}

// setup's container, upgraded by a real docker: healthy, with its policy,
// its read-only bind and its variables, the old one set aside at no; a trace
// after, then --back: the old image on a new volume with the traces the
// archive held, the migrated volume and both containers kept, and a second
// --back that changes nothing.
func TestARealContainerIsUpgradedAndGoneBackFrom(t *testing.T) {
	w := newDockerWorld(t)
	for i := 1; i <= 3; i++ {
		w.sendTrace(i)
	}
	before := w.count()
	if plan, code := w.run("--plan", "--container", w.name, "--to", vNew); code != exitPending {
		t.Fatalf("plan: %d %s\n%q", code, plan.Summary, plan.Person)
	}
	rep, code := w.run("--container", w.name, "--to", vNew)
	if code != exitOK || rep.Check == nil || rep.Check.Verdict != verdictHealthy {
		t.Fatalf("upgrade: %d %s", code, rep.Summary)
	}
	if got := w.count(); got != before {
		t.Errorf("traces after the upgrade: %d, not %d", got, before)
	}
	cur := w.inspect(w.name)
	if cur.Config.Image != "ghcr.io/tracepad/tracepad:"+vNew || cur.HostConfig.RestartPolicy.Name != "always" ||
		!slices.Contains(cur.Config.Env, "TRACEPAD_PROJECTS=app:"+testPK+":"+testSK) || strings.Count(strings.Join(cur.Config.Env, "\n"), "TRACEPAD_LISTEN=") != 1 {
		t.Errorf("the new container: %s %+v %q", cur.Config.Image, cur.HostConfig.RestartPolicy, cur.Config.Env)
	}
	if i := slices.IndexFunc(cur.Mounts, func(m mount) bool { return m.Destination == "/tls" }); i < 0 || cur.Mounts[i].RW {
		t.Errorf("the read-only bind: %+v", cur.Mounts)
	}
	if old := w.inspect(w.name + "-before-" + rep.Run.ID); old == nil || old.State.Running || old.HostConfig.RestartPolicy.Name != "no" {
		t.Errorf("the old container set aside: %+v", old)
	}
	if !strings.Contains(rep.Check.LogLine, "tracepad "+vNew) {
		t.Errorf("the first log line: %q", rep.Check.LogLine)
	}

	w.sendTrace(4)
	back, code := w.run("--back", rep.Run.ID)
	if code != exitOK {
		t.Fatalf("--back: %d %s", code, back.Summary)
	}
	w.waitVersion(vOld)
	if got := w.count(); got != before {
		t.Errorf("traces after the way back: %d, not %d", got, before)
	}
	cur = w.inspect(w.name)
	vol := w.volume + "-" + rep.Run.ID
	if cur.Config.Image != "ghcr.io/tracepad/tracepad:"+vOld || !slices.ContainsFunc(cur.Mounts, func(m mount) bool { return m.Name == vol && m.Destination == "/data" }) ||
		cur.HostConfig.RestartPolicy.Name != "always" {
		t.Errorf("after the way back: %s %+v %+v", cur.Config.Image, cur.Mounts, cur.HostConfig.RestartPolicy)
	}
	if after := w.inspect(w.name + "-after-" + rep.Run.ID); after == nil || after.State.Running || after.HostConfig.RestartPolicy.Name != "no" {
		t.Errorf("the new container set aside: %+v", after)
	}
	if again, code := w.run("--back", rep.Run.ID); code != exitOK {
		t.Errorf("--back again: %d %s", code, again.Summary)
	}
	if now := w.inspect(w.name); now.ID != cur.ID || !now.State.Running {
		t.Errorf("--back again changed what runs: %s, was %s", now.ID[:12], cur.ID[:12])
	}
}

// A release whose server does not start: not healthy, and the way back runs
// the old image again, on a volume restored from the archive.
func TestARealContainerWhoseReleaseDoesNotServeGoesBack(t *testing.T) {
	w := newDockerWorld(t)
	w.sendTrace(1)
	before := w.count()
	rep, code := w.run("--container", w.name, "--to", vBroken)
	if code != exitWentBack {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	w.waitVersion(vOld)
	if got := w.count(); got != before {
		t.Errorf("traces after the way back: %d, not %d", got, before)
	}
	if cur := w.inspect(w.name); cur.Config.Image != "ghcr.io/tracepad/tracepad:"+vOld || cur.HostConfig.RestartPolicy.Name != "always" {
		t.Errorf("after the way back: %s %+v", cur.Config.Image, cur.HostConfig.RestartPolicy)
	}
}
