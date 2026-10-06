//go:build unix

package upgrade

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// imageEnv is the image's own environment, as its Dockerfile sets it.
var imageEnv = []string{"PATH=/", "TRACEPAD_DATA_DIR=/data", "TRACEPAD_LISTEN=:4318", "TRACEPAD_IN_CONTAINER=1"}

// fakeDocker is the docker CLI over containers and volumes in memory, made
// and changed as docker makes and changes them. It records every call, and a
// test fails if any of them removes something.
type fakeDocker struct {
	t  *testing.T
	mu sync.Mutex
	// calls are every call, in order.
	calls  [][]string
	byName map[string]*fakeContainer
	images map[string]imageConfig
	// volumes are the volumes and the traces each holds; options, a
	// volume's options.
	volumes map[string]int64
	options map[string]string
	// archives holds what each archive it wrote was taken from.
	archives map[string]int64
	nextID   int
	// down is a daemon that does not answer; noImage, images that cannot be
	// inspected; rootless, a daemon that runs so.
	down, noImage, rootless bool
	// failRun makes `docker run -d` create its container and fail to start
	// it, as a busy port does; brokenRef is an image whose server exits.
	failRun   bool
	brokenRef string
	// silentRef is an image whose server, on a volume a way back restored,
	// runs and never answers; failRestore makes the way back's busybox
	// restore fail; room is the kilobytes busybox's df gives (0: plenty).
	silentRef   string
	failRestore bool
	room        int64
	failDf      bool
	// failAfterStart makes `docker run -d` of the release start its
	// container and answer an error.
	failAfterStart bool
	// startsAndErrs is an image whose next `docker run -d` starts its
	// container and answers an error.
	startsAndErrs string
	// links is what busybox's find of links and special files says;
	// digests, an image's repository digests by its ID.
	links   string
	digests map[string][]string
	// flakyUpdate is how many updates of a restart policy docker refuses
	// before it takes one.
	flakyUpdate int
	// stubborn is a container that does not stop on SIGTERM until
	// stopLate, termed those asked to; slow, how long every container
	// takes to answer while set.
	stubborn bool
	termed   map[string]bool
	slow     time.Duration
	// ranOn is every "<volume> <image>" a container ran: what may have
	// written a volume.
	ranOn map[string]bool
	// onCall runs before each call: a test's interrupt, or a kill.
	onCall func(args []string)
	dbFile []byte
}

// fakeContainer is a container as docker keeps it; extra are fields of its
// inspect the command does not type ("HostConfig.PidsLimit": 100).
type fakeContainer struct {
	inspectContainer
	extra map[string]any
	// mute does not answer.
	mute bool
}

func newFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()
	d := &fakeDocker{t: t, byName: map[string]*fakeContainer{}, images: map[string]imageConfig{},
		volumes: map[string]int64{}, options: map[string]string{}, archives: map[string]int64{}, ranOn: map[string]bool{}, dbFile: templateDB(t, 0)}
	for _, v := range []string{"0.1.0", "0.2.0", "0.2.1"} {
		d.image("ghcr.io/tracepad/tracepad:"+v, v)
	}
	d.images[busybox] = imageConfig{ID: "sha256:" + strings.Repeat("b", 64)}
	return d
}

// image is a release of the image, configured as its Dockerfile does.
func (d *fakeDocker) image(ref, version string) {
	img := imageConfig{ID: fmt.Sprintf("sha256:%064x", len(d.images)+1)}
	img.Config = inspectConfig{
		Env:          slices.Clone(imageEnv),
		Entrypoint:   []string{"/tracepad"},
		User:         "nonroot:nonroot",
		ExposedPorts: map[string]struct{}{"4318/tcp": {}},
		Labels:       map[string]string{"org.opencontainers.image.version": version, "org.opencontainers.image.title": "Tracepad"},
		Healthcheck:  json.RawMessage(`{"Test":["CMD","/tracepad","health"]}`),
	}
	d.images[ref] = img
	d.images[img.ID] = img
}

func (d *fakeDocker) find(ref string) *fakeContainer {
	if c, ok := d.byName[ref]; ok {
		return c
	}
	for _, c := range d.byName {
		if c.ID == ref {
			return c
		}
	}
	return nil
}

// container is a container by name, for a test to look at.
func (d *fakeDocker) container(name string) *fakeContainer {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.byName[name]
}

func (d *fakeDocker) set(f func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f()
}

func (d *fakeDocker) versionOf(c *fakeContainer) string {
	return d.images[c.Config.Image].Config.Labels["org.opencontainers.image.version"]
}

func mountValue(m string) map[string]string {
	fields, _ := csv.NewReader(strings.NewReader(m)).Read()
	out := map[string]string{}
	for _, f := range fields {
		k, v, _ := strings.Cut(f, "=")
		out[k] = v
	}
	return out
}

func (c *fakeContainer) marshal() map[string]any {
	b, _ := json.Marshal(c.inspectContainer)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for k, v := range c.extra {
		section, field, _ := strings.Cut(k, ".")
		m[section].(map[string]any)[field] = v
	}
	return m
}

func (d *fakeDocker) Run(ctx context.Context, args ...string) ([]byte, error) {
	d.mu.Lock()
	d.calls = append(d.calls, slices.Clone(args))
	hook := d.onCall
	d.mu.Unlock()
	// As exec.CommandContext does: a cancelled context runs nothing.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if hook != nil {
		hook(args)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.down {
		return nil, errors.New("docker " + args[0] + ": Cannot connect to the Docker daemon at unix:///var/run/docker.sock")
	}
	joined := strings.Join(args, " ")
	for _, removal := range []string{"rm ", "rmi ", "volume rm", "prune", "container rm", "image rm", "-delete"} {
		if strings.HasPrefix(joined, removal) || strings.Contains(joined, " "+removal) {
			d.t.Errorf("a removal: docker %s", joined)
		}
	}
	switch {
	case args[0] == "info":
		opts := `["name=seccomp,profile=builtin","name=cgroupns"]`
		if d.rootless {
			opts = `["name=seccomp,profile=builtin","name=rootless","name=cgroupns"]`
		}
		return []byte(opts + ` "json-file"` + "\n"), nil
	case joined == "ps -q --no-trunc":
		var ids []string
		for _, c := range d.byName {
			if c.State.Running {
				ids = append(ids, c.ID)
			}
		}
		slices.Sort(ids)
		return []byte(strings.Join(ids, "\n")), nil
	case args[0] == "container" && args[1] == "inspect":
		var list []map[string]any
		for _, ref := range args[2:] {
			c := d.find(ref)
			if c == nil {
				return nil, errors.New("docker container: Error: No such container: " + ref)
			}
			list = append(list, c.marshal())
		}
		return json.Marshal(list)
	case args[0] == "image" && args[1] == "inspect":
		if d.noImage {
			return nil, errors.New("docker image: Error: No such image: " + args[len(args)-1])
		}
		if args[2] == "--format" {
			img, ok := d.images[args[4]]
			if !ok {
				return nil, errors.New("docker image: Error: No such image: " + args[4])
			}
			if strings.Contains(args[3], "RepoDigests") {
				b, _ := json.Marshal(d.digests[img.ID])
				return b, nil
			}
			return []byte(img.ID + "\n"), nil
		}
		var list []imageConfig
		for _, ref := range args[2:] {
			img, ok := d.images[ref]
			if !ok {
				return nil, errors.New("docker image: Error: No such image: " + ref)
			}
			list = append(list, img)
		}
		return json.Marshal(list)
	case args[0] == "volume" && args[1] == "inspect":
		name := args[len(args)-1]
		if _, ok := d.volumes[name]; !ok {
			return nil, errors.New("docker volume: Error response from daemon: get " + name + ": no such volume")
		}
		if args[2] == "--format" {
			opts := d.options[name]
			if opts == "" {
				opts = "null"
			}
			return []byte("local " + opts + "\n"), nil
		}
		return []byte("[{}]"), nil
	case args[0] == "volume" && args[1] == "create":
		if _, ok := d.volumes[args[2]]; !ok {
			d.volumes[args[2]] = 0
		}
		return []byte(args[2]), nil
	case args[0] == "tag":
		img, ok := d.images[args[1]]
		if !ok {
			return nil, errors.New("docker tag: No such image: " + args[1])
		}
		d.images[args[2]] = img
		return nil, nil
	case args[0] == "pull":
		if _, ok := d.images[args[len(args)-1]]; !ok {
			return nil, errors.New("docker pull: manifest unknown")
		}
		return nil, nil
	case args[0] == "stop":
		d.t.Errorf("docker stop kills after its timeout: %s", joined)
		return nil, errors.New("not here")
	case args[0] == "kill":
		c := d.find(args[len(args)-1])
		if c == nil {
			return nil, errors.New("docker kill: No such container")
		}
		if args[1] != "--signal" || args[2] != "TERM" {
			d.t.Errorf("a kill that is not SIGTERM: %s", joined)
		}
		if d.stubborn {
			if d.termed == nil {
				d.termed = map[string]bool{}
			}
			d.termed[c.ID] = true
			return nil, nil
		}
		// As docker does: a container that exits under a restart policy is
		// started again.
		if p := c.HostConfig.RestartPolicy.Name; p != "" && p != "no" {
			c.RestartCount++
			return nil, nil
		}
		c.State.Running = false
		return nil, nil
	case args[0] == "start":
		c := d.find(args[1])
		if c == nil {
			return nil, errors.New("docker start: No such container")
		}
		d.boot(c)
		return nil, nil
	case args[0] == "rename":
		c := d.find(args[1])
		if c == nil || d.byName[args[2]] != nil {
			return nil, errors.New("docker rename: rename refused")
		}
		delete(d.byName, strings.TrimPrefix(c.Name, "/"))
		c.Name = "/" + args[2]
		d.byName[args[2]] = c
		return nil, nil
	case args[0] == "update":
		if d.flakyUpdate > 0 {
			// As docker's restart manager answers now and then.
			d.flakyUpdate--
			return nil, errors.New("docker update: Error response from daemon: Cannot update container: cannot update a stopped container")
		}
		c := d.find(args[3])
		if c == nil {
			return nil, errors.New("docker update: No such container")
		}
		name, retries, _ := strings.Cut(args[2], ":")
		c.HostConfig.RestartPolicy.Name = name
		c.HostConfig.RestartPolicy.MaximumRetryCount = 0
		if retries != "" {
			fmt.Sscan(retries, &c.HostConfig.RestartPolicy.MaximumRetryCount)
		}
		return nil, nil
	case args[0] == "logs":
		c := d.find(args[len(args)-1])
		if c == nil {
			return nil, errors.New("docker logs: No such container")
		}
		return []byte(`time=x level=INFO msg="tracepad ` + d.versionOf(c) + `"` + "\n"), nil
	case args[0] == "run" && args[1] == "--rm":
		return d.busybox(args)
	case args[0] == "run" && args[1] == "-d":
		return d.create(args)
	}
	return nil, errors.New("the fake does not know: docker " + joined)
}

func (d *fakeDocker) busybox(args []string) ([]byte, error) {
	mounts := map[string]map[string]string{}
	for i, a := range args {
		if a == "--mount" {
			m := mountValue(args[i+1])
			mounts[m["dst"]] = m
		}
	}
	script := args[len(args)-1]
	switch {
	case strings.Contains(script, "find /data"):
		if d.failDf {
			return nil, errors.New("docker run: df: /data: Input/output error")
		}
		room := d.room
		if room == 0 {
			room = 99999999
		}
		return []byte(fmt.Sprintf("%s===\n2048\t/data\nFilesystem 1024-blocks Used Available Capacity Mounted on\noverlay 100000000 1 %d 1%% /data\n", d.links, room)), nil
	case strings.Contains(script, "tar czf"):
		path := filepath.Join(mounts["/backup"]["src"], "data.tar.gz")
		if _, err := os.Stat(path); err == nil && strings.Contains(script, "set -C") {
			return nil, errors.New("docker run: sh: can't create /backup/data.tar.gz: File exists")
		}
		body := archiveOf(d.t, map[string][]byte{"./tracepad.db": d.dbFile})
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return nil, err
		}
		d.archives[path] = d.volumes[mounts["/data"]["src"]]
		return []byte(fmt.Sprintf("%d\n", len(d.dbFile))), nil
	case strings.Contains(script, "tar xzf"):
		if d.failRestore {
			return nil, errors.New("docker run: tar: write error: No space left on device")
		}
		path := filepath.Join(mounts["/backup"]["src"], "data.tar.gz")
		d.volumes[mounts["/data"]["src"]] = d.archives[path]
		return nil, nil
	}
	return nil, errors.New("the fake does not know this busybox")
}

// create is `docker run -d`: a container from the arguments as docker reads
// them, the env file included, configured from its image as docker does.
func (d *fakeDocker) create(args []string) ([]byte, error) {
	c := &fakeContainer{}
	c.HostConfig.PortBindings = map[string][]portBinding{}
	c.HostConfig.NetworkMode = "bridge"
	c.HostConfig.LogConfig.Type = "json-file"
	c.Config.Labels = map[string]string{}
	var env []string
	user, entrypoint := "", []string(nil)
	setEntrypoint := false
	i := 2
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i += 2 {
		if args[i] == "-P" {
			c.HostConfig.PublishAllPorts = true
			i--
			continue
		}
		v := args[i+1]
		switch args[i] {
		case "--name":
			c.Name = "/" + v
		case "--restart":
			name, retries, _ := strings.Cut(v, ":")
			c.HostConfig.RestartPolicy.Name = name
			if retries != "" {
				fmt.Sscan(retries, &c.HostConfig.RestartPolicy.MaximumRetryCount)
			}
		case "--mount":
			m := mountValue(v)
			mt := mount{Type: m["type"], Destination: m["dst"], RW: true}
			if _, ro := m["readonly"]; ro {
				mt.RW = false
			}
			if mt.Type == "volume" {
				mt.Name, mt.Driver = m["src"], "local"
				if _, ok := d.volumes[mt.Name]; !ok {
					d.volumes[mt.Name] = 0
				}
			} else {
				mt.Source = m["src"]
			}
			c.Mounts = append(c.Mounts, mt)
		case "--tmpfs":
			c.Mounts = append(c.Mounts, mount{Type: "tmpfs", Destination: v, RW: true})
		case "-p":
			last := strings.LastIndex(v, ":")
			hostPart, port := v[:last], v[last+1:]
			if !strings.Contains(port, "/") {
				port += "/tcp"
			}
			hl := strings.LastIndex(hostPart, ":")
			ip := strings.Trim(hostPart[:hl], "[]")
			c.HostConfig.PortBindings[port] = append(c.HostConfig.PortBindings[port], portBinding{HostIP: ip, HostPort: hostPart[hl+1:]})
		case "--label":
			k, val, _ := strings.Cut(v, "=")
			c.Config.Labels[k] = val
		case "--log-driver":
			c.HostConfig.LogConfig.Type = v
		case "--log-opt":
			k, val, _ := strings.Cut(v, "=")
			if c.HostConfig.LogConfig.Config == nil {
				c.HostConfig.LogConfig.Config = map[string]string{}
			}
			c.HostConfig.LogConfig.Config[k] = val
		case "--user":
			user = v
		case "--network":
			c.HostConfig.NetworkMode = v
		case "--cgroupns":
			c.HostConfig.CgroupnsMode = v
		case "--ipc":
			c.HostConfig.IpcMode = v
		case "--entrypoint":
			setEntrypoint = true
			if v != "" {
				entrypoint = []string{v}
			}
		case "-e":
			env = append(env, v)
		case "--env-file":
			b, err := os.ReadFile(v)
			if err != nil {
				return nil, err
			}
			for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				if line != "" {
					env = append(env, line)
				}
			}
		default:
			d.t.Errorf("the fake does not know the option %s", args[i])
		}
	}
	ref := args[i]
	img, ok := d.images[ref]
	if !ok {
		return nil, errors.New("docker run: Unable to find image " + ref)
	}
	name := strings.TrimPrefix(c.Name, "/")
	if d.byName[name] != nil {
		return nil, errors.New("docker run: Conflict. The container name is already in use")
	}
	d.nextID++
	c.ID = fmt.Sprintf("%064x", 0xc0000+d.nextID)
	c.Image = img.ID
	c.Config.Image = ref
	c.Config.Hostname = c.ID[:12]
	c.Config.ExposedPorts = img.Config.ExposedPorts
	c.Config.Cmd = args[i+1:]
	if len(c.Config.Cmd) == 0 {
		c.Config.Cmd = img.Config.Cmd
	}
	c.Config.Env = append(slices.Clone(img.Config.Env), env...)
	c.Config.Entrypoint, c.Config.WorkingDir, c.Config.Healthcheck = img.Config.Entrypoint, img.Config.WorkingDir, img.Config.Healthcheck
	if setEntrypoint {
		c.Config.Entrypoint = entrypoint
	}
	c.Config.User = img.Config.User
	if user != "" {
		c.Config.User = user
	}
	for k, v := range img.Config.Labels {
		if _, ok := c.Config.Labels[k]; !ok {
			c.Config.Labels[k] = v
		}
	}
	c.NetworkSettings.Ports = c.HostConfig.PortBindings
	d.byName[name] = c
	if d.startsAndErrs != "" && ref == d.startsAndErrs {
		// Started, and running, and the CLI answered an error all the same.
		d.startsAndErrs = ""
		d.boot(c)
		return nil, errors.New("docker run: error waiting for container: unexpected EOF")
	}
	if d.failAfterStart && c.Config.Image != d.brokenRef && strings.HasSuffix(ref, ":0.2.0") {
		// The container started, ran on its volume and exited, and the CLI
		// answered an error all the same.
		d.boot(c)
		c.State.Running = false
		return nil, errors.New("docker run: error waiting for container: context canceled")
	}
	if d.failRun {
		return nil, errors.New("docker run: Bind for 127.0.0.1:4318 failed: port is already allocated")
	}
	d.boot(c)
	return []byte(c.ID + "\n"), nil
}

// boot starts a container as docker does: a broken image exits at once, and
// under a restart policy docker starts it again and again, keeping it
// Running with Restarting set (the review of #1).
func (d *fakeDocker) boot(c *fakeContainer) {
	c.State.StartedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if c.Config.Image != d.brokenRef {
		c.State.Running, c.State.Restarting = true, false
		for _, m := range c.Mounts {
			if m.Destination == "/data" {
				d.ranOn[m.Name+" "+c.Config.Image] = true
			}
		}
		return
	}
	if p := c.HostConfig.RestartPolicy.Name; p != "" && p != "no" {
		c.State.Running, c.State.Restarting = true, true
		c.RestartCount++
		return
	}
	c.State.Running = false
}

// RoundTrip answers for whatever container publishes the asked port: its
// image's version on /health, its volume's traces on /api/v1/system.
func (d *fakeDocker) RoundTrip(req *http.Request) (*http.Response, error) {
	d.mu.Lock()
	slow := d.slow
	d.mu.Unlock()
	if slow > 0 {
		select {
		case <-time.After(slow):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.byName {
		if !c.State.Running || c.State.Restarting || c.mute || c.Config.Image == d.silentRef && onRestoredVolume(c) {
			continue
		}
		for _, bs := range c.HostConfig.PortBindings {
			for _, b := range bs {
				if req.URL.Host != onLoopback(b.HostIP)+":"+b.HostPort {
					continue
				}
				body := ""
				switch req.URL.Path {
				case "/health":
					body = fmt.Sprintf(`{"status":"ok","version":%q}`, d.versionOf(c))
				case "/api/v1/system":
					vol := ""
					for _, m := range c.Mounts {
						if m.Destination == "/data" {
							vol = m.Name
						}
					}
					body = fmt.Sprintf(`{"database":{"rows":{"traces":%d}}}`, d.volumes[vol])
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
			}
		}
	}
	return nil, errors.New("connection refused")
}

// onRestoredVolume: a way back's container, on a volume it restored.
func onRestoredVolume(c *fakeContainer) bool {
	for _, m := range c.Mounts {
		if m.Destination == "/data" && strings.Count(m.Name, "-") > 1 {
			return true
		}
	}
	return false
}

// run runs a container as a person's `docker run` would, for a test's
// setup; the call is not counted.
func (d *fakeDocker) run(t *testing.T, args ...string) *fakeContainer {
	t.Helper()
	out, err := d.Run(context.Background(), append([]string{"run", "-d"}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = nil
	return d.find(strings.TrimSpace(string(out)))
}

// add runs a container of version v, named name, publishing 4318 on host:port,
// with its data in volume, and its labels.
func (d *fakeDocker) add(name, v, host, port, volume string, labels map[string]string) *fakeContainer {
	args := []string{"--name", name, "-p", host + ":" + port + ":4318", "--mount", "type=volume,src=" + volume + ",dst=/data"}
	for k, l := range labels {
		args = append(args, "--label", k+"="+l)
	}
	env := filepath.Join(d.t.TempDir(), "env")
	_ = os.WriteFile(env, []byte("TRACEPAD_URL=http://localhost:"+port+"\n"), 0o600)
	return d.run(d.t, append(args, "--env-file", env, "ghcr.io/tracepad/tracepad:"+v)...)
}

// setupContainer is the container setup.md starts, made as docker makes it,
// with a restart policy and a read-only bind of the person's on top; its
// volume holds 7 traces.
func (d *fakeDocker) setupContainer(t *testing.T, bind string) *fakeContainer {
	t.Helper()
	env := filepath.Join(t.TempDir(), "env")
	_ = os.WriteFile(env, []byte("TRACEPAD_URL=http://localhost:4318\nTRACEPAD_PROJECTS=app:tp-pk-a:tp-sk-b\n"), 0o600)
	d.volumes["tracepad-app"] = 7
	return d.run(t, "--name", "tracepad-app", "--restart", "always",
		"--mount", "type=volume,src=tracepad-app,dst=/data",
		"--mount", csvField("type=bind", "src="+bind, "dst=/tls", "readonly"),
		"-p", "127.0.0.1:4318:4318", "--env-file", env, "ghcr.io/tracepad/tracepad:0.1.0", "serve")
}

// containerDeps is a machine with docker and no server process; installed,
// when given, is the version of the installed binary.
func containerDeps(t *testing.T, d *fakeDocker, installed ...string) Deps {
	t.Helper()
	home := t.TempDir()
	mirror := t.TempDir()
	for _, v := range []string{"0.1.0", "0.2.0", "0.2.1"} {
		mirrorRelease(t, mirror, v, v, v == "0.2.0")
	}
	install := filepath.Join(home, ".local", "bin")
	_ = os.MkdirAll(install, 0o700)
	if len(installed) > 0 {
		scriptBinary(t, filepath.Join(install, "tracepad"), installed[0])
	}
	return Deps{
		Sys:        noProcesses{},
		Docker:     d,
		HTTP:       &http.Client{Transport: d},
		Releases:   fakeReleases(t, mirror),
		InstallDir: install,
		Backups:    filepath.Join(home, "tracepad-backups"),
		Home:       home,
		Cwd:        home,
		Self:       "/bin/sh",
		Getenv: func(k string) string {
			if k == "TRACEPAD_API_KEY" {
				return "tp-sk-b"
			}
			return ""
		},
		LookPath:     func(string) string { return "" },
		Version:      scriptVersion,
		Skills:       func(context.Context, string, string, ...string) (string, error) { return "", nil },
		Now:          time.Now,
		Sleep:        func(context.Context, time.Duration) error { return nil },
		StopWait:     time.Second,
		HealthWait:   0,
		ProbeWait:    time.Millisecond,
		DiscoverWait: 2 * time.Second,
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

// A container that is the person's gets the commands that upgrade it, from
// the plan and the upgrade alike: exit 4, nothing made, the run as it was
// created — its ports, mounts, restart policy and command — and the
// variables it was given in a file read from Docker, named and never printed.
func TestAContainerGetsTheCommandsThatUpgradeIt(t *testing.T) {
	t.Parallel()
	d := newFakeDocker(t)
	const secret = "demo:tp-pk-1:tp-sk-not-to-be-printed"
	env := filepath.Join(t.TempDir(), "env")
	_ = os.WriteFile(env, []byte("TRACEPAD_URL=http://localhost:4318\nTRACEPAD_PROJECTS="+secret+"\n"), 0o600)
	// Named as setup does not name one: the person's.
	d.run(t, "--name", "myapp", "--restart", "always", "-p", "127.0.0.1:4318:4318", "--mount", "type=volume,src=myapp,dst=/data",
		"--mount", "type=bind,src=/srv/my config,dst=/etc/extra,readonly", "--env-file", env, "ghcr.io/tracepad/tracepad:0.1.0", "serve")
	deps := containerDeps(t, d)
	for _, mode := range [][]string{{"--plan"}, {}} {
		rep, code := runReport(t, deps, mode...)
		if code != exitDecide || rep.Run != nil {
			t.Fatalf("%q: %d %s", mode, code, rep.Summary)
		}
		all := strings.Join(rep.Person, "\n")
		for _, want := range []string{"name it with --container to upgrade it", "docker stop myapp", "src=myapp,dst=/data,readonly", "sh -c 'umask 077 && set -C && tar czf - -C /data . > \"/backup/$1\"' sh myapp-0.1.0.tar.gz",
			"docker pull ghcr.io/tracepad/tracepad:0.2.0", "docker rename myapp myapp-old", "docker/#upgrading",
			"docker rename myapp myapp-old && (umask 077 && set -C && docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' myapp-old | grep -E '^(TRACEPAD_PROJECTS|TRACEPAD_URL)=' > myapp.upgrade.env) && ",
			"docker run -d --name myapp --env-file myapp.upgrade.env -p 127.0.0.1:4318:4318 --mount type=volume,src=myapp,dst=/data --mount 'type=bind,src=/srv/my config,dst=/etc/extra,readonly' --restart always ghcr.io/tracepad/tracepad:0.2.0 serve && rm myapp.upgrade.env. Once the new one is healthy: docker rm myapp-old",
			"Stopped before the rename: docker start myapp",
			"Stopped after the rename: docker rm myapp if it was made, then rm -f myapp.upgrade.env; docker rename myapp-old myapp && docker start myapp"} {
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
	for _, c := range d.calls {
		if c[0] != "ps" && c[0] != "info" && c[1] != "inspect" {
			t.Errorf("the command acted on the person's container: docker %q", c)
		}
	}
}

// A Compose container's commands are whole, run from anywhere (the live run
// of 0.1.0): the service stopped through its project and file, the volume
// Compose really named archived — `<project>_<volume>`, never docker.md's
// `tracepad`, which `docker run` would make empty and archive — the file
// named, and an image pinned by digest said, since a new tag in front of
// the old digest leaves Docker on the old image.
func TestComposeAdvice(t *testing.T) {
	t.Parallel()
	d := newFakeDocker(t)
	pinned := "ghcr.io/tracepad/tracepad:0.1.0@sha256:" + strings.Repeat("d", 64)
	d.image(pinned, "0.1.0")
	labels := map[string]string{
		"com.docker.compose.project":                  "tracepad",
		"com.docker.compose.service":                  "tracepad",
		"com.docker.compose.project.working_dir":      "/srv/my app",
		"com.docker.compose.project.config_files":     "/srv/my app/compose.yml,/srv/my app/compose.override.yml",
		"com.docker.compose.project.environment_file": "/srv/my app/.env.prod",
	}
	args := []string{"--name", "tracepad-tracepad-1", "-p", "127.0.0.1:4318:4318", "--mount", "type=volume,src=tracepad_tracepad_data,dst=/data"}
	for k, l := range labels {
		args = append(args, "--label", k+"="+l)
	}
	d.run(t, append(args, pinned)...)
	rep, code := runReport(t, containerDeps(t, d), "--plan")
	all := strings.Join(rep.Person, "\n")
	compose := "docker compose -p tracepad --env-file '/srv/my app/.env.prod' -f '/srv/my app/compose.yml' -f '/srv/my app/compose.override.yml'"
	for _, want := range []string{
		compose + " stop tracepad && docker run --rm --mount type=volume,src=tracepad_tracepad_data,dst=/data,readonly -v \"$PWD:/backup\" " + busybox + " sh -c 'umask 077 && set -C && tar czf - -C /data . > \"/backup/$1\"' sh tracepad-tracepad-1-0.1.0.tar.gz && docker pull ghcr.io/tracepad/tracepad:0.2.0. ",
		"set the image of the service tracepad in whichever of /srv/my app/compose.yml, /srv/my app/compose.override.yml sets it to ghcr.io/tracepad/tracepad:0.2.0",
		"take its digest off: it is pinned to sha256:" + strings.Repeat("d", 64),
		"docker pull ghcr.io/tracepad/tracepad:0.2.0 prints its digest",
		"and: " + compose + " up -d tracepad.",
		"Stopped before the image was set: " + compose + " start tracepad",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the commands miss %q:\n%s", want, all)
		}
	}
	if code != exitDecide || strings.Contains(all, "src=tracepad,") {
		t.Errorf("%d: %s", code, all)
	}

	// One file in another directory than the project's is named with it;
	// an image not pinned says nothing of a digest.
	c := Container{Name: "obs-tracepad-1", Repo: "ghcr.io/tracepad/tracepad", Ref: "ghcr.io/tracepad/tracepad:0.1", Version: "0.1.0",
		Compose: "obs", Service: "tp", ComposeDir: "/srv/obs", ComposeFiles: []string{"/etc/obs/compose.yml"}, DataMount: dataMount("volume", "obs_data")}
	got := composeAdvice(c, "0.2.0")
	for _, want := range []string{"docker compose -p obs --project-directory /srv/obs -f /etc/obs/compose.yml stop tp", "in /etc/obs/compose.yml to ghcr.io/tracepad/tracepad:0.2.0, and"} {
		if !strings.Contains(got, want) {
			t.Errorf("one file elsewhere: %q not in %s", want, got)
		}
	}
	if strings.Contains(got, "digest") {
		t.Errorf("an image with no digest: %s", got)
	}
	// The env files the project was started with are passed again, in their
	// order (the review of #228): a ${VAR} in the file may name the volume.
	c.ComposeEnv = []string{"/srv/obs/base.env", "/srv/obs/my local.env"}
	if got := composeAdvice(c, "0.2.0"); !strings.Contains(got, "docker compose -p obs --project-directory /srv/obs --env-file /srv/obs/base.env --env-file '/srv/obs/my local.env' -f /etc/obs/compose.yml stop tp") ||
		!strings.Contains(got, "--env-file '/srv/obs/my local.env' -f /etc/obs/compose.yml up -d tp") {
		t.Errorf("env files: %s", got)
	}
	c.ComposeEnv = nil
	// An env file Compose wrote relative is read from the project's
	// directory (the second review of #228).
	ic := inspectContainer{Name: "/obs-tracepad-1"}
	ic.Config.Image = "ghcr.io/tracepad/tracepad:0.1.0"
	ic.Config.Labels = map[string]string{"com.docker.compose.project": "obs", "com.docker.compose.project.working_dir": "/srv/obs",
		"com.docker.compose.project.environment_file": "prod.env,/etc/obs/base.env"}
	if got, _ := asContainer(ic); !slices.Equal(got.ComposeEnv, []string{"/srv/obs/prod.env", "/etc/obs/base.env"}) {
		t.Errorf("relative env files: %q", got.ComposeEnv)
	}
	// Compose's labels missing: its project and directory, and what to look up.
	c.ComposeFiles, c.Service = nil, ""
	if got := composeAdvice(c, "0.2.0"); !strings.Contains(got, "docker compose -p obs --project-directory /srv/obs stop '<its service>'") {
		t.Errorf("no file label: %s", got)
	}
}

// What is the person's and needs nothing is apart from what needs them (the
// live run of 0.1.0): in JSON it says nothing_to_do, and the text lists it
// under a heading of its own, out of the inventory above "Yours, to do".
func TestThePersonsIdleApart(t *testing.T) {
	t.Parallel()
	d := newFakeDocker(t)
	d.add("other-current", "0.2.0", "0.0.0.0", "4330", "current", nil)
	d.add("other-behind", "0.1.0", "0.0.0.0", "4331", "behind", nil)
	// One rule, needsNothing: a development build is never marked, as the
	// binary's is not (the fourth review of #228).
	d.image("ghcr.io/tracepad/tracepad:dev", "dev")
	d.add("other-dev", "dev", "0.0.0.0", "4332", "dev", nil)
	rep, code := runReport(t, containerDeps(t, d), "--plan")
	idle := map[string]bool{}
	for _, c := range rep.Containers {
		idle[c.Name] = c.Idle
	}
	if code != exitDecide || !idle["other-current"] || idle["other-behind"] || idle["other-dev"] {
		t.Fatalf("%d %v", code, idle)
	}
	var out bytes.Buffer
	run(context.Background(), Options{Args: []string{"--plan"}, Stdout: &out, Stderr: io.Discard}, containerDeps(t, d))
	text := out.String()
	head, rest, _ := strings.Cut(text, "\nYours, to do:")
	_, apart, _ := strings.Cut(rest, "\nYours, nothing to do (a release at 0.2.0 or past it):\n")
	if !strings.Contains(head, "container other-behind") || strings.Contains(head, "other-current") ||
		!strings.HasPrefix(apart, "  container other-current, ghcr.io/tracepad/tracepad:0.2.0, 0.2.0 — yours") {
		t.Errorf("%s", text)
	}
}

// Compose's container is upgraded through its Compose file; one already at
// the version is not called behind. Each is asked where its server listens
// — TRACEPAD_LISTEN or --listen, published on any address of this machine —
// not at 4318 only (the final review). One that publishes it and does not
// answer may be behind, and gets the commands (the audit of #223); one whose
// address cannot be told is said not checked, never called behind (the
// final review); a daemon that does not answer is said, never taken for
// none. The one of the command's that is behind is the run's.
func TestWhatThePlanSaysOfContainers(t *testing.T) {
	t.Parallel()
	d := newFakeDocker(t)
	d.add("obs-tracepad-1", "0.1.0", "127.0.0.1", "4318", "obs_data", map[string]string{"com.docker.compose.project": "obs"})
	d.add("tracepad-current", "0.2.0", "127.0.0.1", "4319", "current", nil)
	d.add("tracepad-open", "0.1.0", "0.0.0.0", "4320", "open", nil)
	d.run(t, "--name", "tracepad-moved", "-e", "TRACEPAD_LISTEN=:8080", "-p", "127.0.0.1:18080:8080", "ghcr.io/tracepad/tracepad:0.2.0")
	d.run(t, "--name", "tracepad-flag", "-p", "127.0.0.1:18081:9090", "--mount", "type=volume,src=flag,dst=/data", "ghcr.io/tracepad/tracepad:0.1.0", "serve", "--listen", ":9090")
	d.add("tracepad-mute", "0.1.0", "127.0.0.1", "4321", "mute", nil).mute = true
	d.run(t, "--name", "tracepad-inside", "ghcr.io/tracepad/tracepad:0.1.0")
	rep, code := runReport(t, containerDeps(t, d), "--plan")
	all, notes := strings.Join(rep.Person, "\n"), strings.Join(rep.Notes, "\n")
	for _, want := range []string{"docker compose -p obs up -d", "the project obs", "container tracepad-open runs 0.1.0; it publishes 4318/tcp on 0.0.0.0, beyond this machine",
		"container tracepad-mute does not say its version"} {
		if !strings.Contains(all, want) {
			t.Errorf("the plan misses %q:\n%s", want, all)
		}
	}
	for _, not := range []string{"tracepad-current", "tracepad-moved", "tracepad-inside", "tracepad-flag"} {
		if strings.Contains(all, not) {
			t.Errorf("%s is called the person's and behind:\n%s", not, all)
		}
	}
	if !strings.Contains(notes, "container tracepad-inside was not checked: its server listens on port 4318 inside, which it does not publish") {
		t.Errorf("%s", notes)
	}
	target := ""
	for _, c := range rep.Containers {
		if c.Target {
			target = c.Name
		}
	}
	if code != exitPending || target != "tracepad-flag" {
		t.Errorf("%d %s; the run's: %q", code, rep.Summary, target)
	}

	// Naming one of the command's leaves the others the command's: a later
	// run's, as --data-dir leaves the other servers (the review of #226).
	d.run(t, "--name", "tracepad-second", "-p", "127.0.0.1:18082:4318", "--mount", "type=volume,src=second,dst=/data", "ghcr.io/tracepad/tracepad:0.1.0")
	rep, code = runReport(t, containerDeps(t, d), "--plan")
	if code != exitPending || !strings.Contains(rep.Summary, "name one") {
		t.Errorf("two of the command's: %d %s", code, rep.Summary)
	}
	rep, code = runReport(t, containerDeps(t, d), "--plan", "--container", "tracepad-second")
	next := strings.Join(rep.Next, "\n")
	if code != exitPending || !strings.Contains(next, "--container tracepad-flag") || strings.Contains(strings.Join(rep.Person, "\n"), "tracepad-flag") {
		t.Errorf("--container tracepad-second: %d %s\nnext %s\nperson %q", code, rep.Summary, next, rep.Person)
	}
	for _, c := range rep.Containers {
		if c.Name == "tracepad-flag" && c.Whose != "command" {
			t.Errorf("the other one is %s's: %s", c.Whose, c.Reason)
		}
	}

	d.set(func() { d.down = true })
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
	d.add("tracepad-app", "0.1.0", "127.0.0.1", "4318", "tracepad-app", nil)
	d.noImage = true
	rep, _ := runReport(t, containerDeps(t, d), "--plan")
	if all := strings.Join(rep.Person, "\n"); !strings.Contains(all, "with the options tracepad-app was created with, which the command could not write (its image could not be read (") ||
		!strings.Contains(all, "docker inspect tracepad-app-old has them)") {
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
	# … busybox sh -c SCRIPT sh FILE: the archive's name is the script's $1.
	eval "cmd=\${$(($# - 2))}"
	eval "file=\${$#}"
	cmd=$(printf '%s' "$cmd" | sed "s#\"/backup/#\"$PWD/#; s#-C /data#-C $DATA#")
	exec sh -c "$cmd" sh "$file" ;;
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
	c := Container{Name: "tracepad-app", Repo: "ghcr.io/tracepad/tracepad", Version: "0.1.0", DataMount: dataMount("volume", "tracepad-app"),
		Run: []string{"-p", "127.0.0.1:4318:4318", "--mount", "type=volume,src=tracepad-app,dst=/data", imageSlot, "serve"}, EnvNames: []string{"TRACEPAD_PROJECTS", "TRACEPAD_URL"}}
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
	run, _, err := createdAs(ic, img, "json-file")
	want := []string{"-P", "--entrypoint", "", imageSlot, "/tracepad", "serve", "--listen", ":8080"}
	if err != nil || !slices.Equal(run, want) {
		t.Errorf("%q %v, want %q", run, err, want)
	}
	c := Container{Name: "t", Repo: "ghcr.io/tracepad/tracepad", Run: run}
	chain, after := containerSteps(c, "0.2.0")
	if !strings.Contains(chain, "docker run -d --name t -P --entrypoint '' ghcr.io/tracepad/tracepad:0.2.0 /tracepad serve --listen :8080") {
		t.Errorf("%s", chain)
	}
	if !slices.Contains(after, "-P publishes new random host ports: clients that used the old port need the new one (docker port t)") {
		t.Errorf("%q", after)
	}
	ic.Mounts = []mount{{Type: "npipe", Source: `\\.\pipe\docker_engine`, Destination: "/pipe"}}
	if _, _, err := createdAs(ic, img, "json-file"); err == nil || !strings.Contains(err.Error(), "a mount of type npipe") {
		t.Errorf("an npipe mount: %v", err)
	}
}

// The recreate is the container again (Decision 10, #47): its restart policy
// with its count, its mounts as --mount takes them (a path with a comma and a
// space, a read-only flag), its bindings on IPv4 and IPv6, its labels, a log
// driver of its own, its user, its command; the variables the
// person set and none of the image's; and on a way back, the restored
// volume in place of its own.
func TestTheRecreateKeepsWhatTheContainerHad(t *testing.T) {
	t.Parallel()
	var img imageConfig
	img.Config = inspectConfig{Env: []string{"PATH=/bin", "TRACEPAD_LISTEN=:4318"}, User: "nonroot:nonroot", Entrypoint: []string{"/tracepad"},
		Labels: map[string]string{"org.opencontainers.image.version": "0.1.0"}}
	var ic inspectContainer
	ic.Config = inspectConfig{Env: []string{"PATH=/bin", "TRACEPAD_LISTEN=:4318", "TRACEPAD_PROJECTS=a:b:c"}, Cmd: []string{"serve"},
		User: "1000:1000", Entrypoint: []string{"/tracepad"}, Labels: map[string]string{"org.opencontainers.image.version": "0.1.0", "team": "obs"}}
	ic.HostConfig.RestartPolicy.Name = "on-failure"
	ic.HostConfig.RestartPolicy.MaximumRetryCount = 3
	ic.HostConfig.PortBindings = map[string][]portBinding{"4318/tcp": {{HostIP: "::1", HostPort: "4318"}}, "4317/tcp": {{HostIP: "127.0.0.1", HostPort: "4317"}}}
	ic.HostConfig.LogConfig.Type = "local"
	ic.HostConfig.LogConfig.Config = map[string]string{"max-size": "10m"}
	ic.Mounts = []mount{
		{Type: "volume", Name: "tp", Destination: "/data", RW: true},
		{Type: "bind", Source: "/Users/me/My Data, certs", Destination: "/tls", RW: false},
	}
	// What an earlier run marked it with is the command's, not the person's:
	// never carried into a recreate.
	ic.Config.Labels[runLabel], ic.Config.Labels[runLabel+".step"] = "an-earlier-run", "started"
	run, names, err := createdAs(ic, img, "json-file")
	if err != nil {
		t.Fatal(err)
	}
	c := Container{Run: run, EnvNames: names, inspect: ic}
	got := runArgs(c, "tracepad-app", "ghcr.io/tracepad/tracepad:0.2.0", "/run/env", "", "R", stepStarted)
	want := []string{"run", "-d", "--name", "tracepad-app", "--env-file", "/run/env", "--label", runLabel + "=R", "--label", runLabel + ".step=started",
		"-p", "127.0.0.1:4317:4317", "-p", "[::1]:4318:4318",
		"--mount", "type=volume,src=tp,dst=/data",
		"--mount", `type=bind,"src=/Users/me/My Data, certs",dst=/tls,readonly`,
		"--restart", "on-failure:3", "--label", "team=obs", "--log-driver", "local", "--log-opt", "max-size=10m", "--user", "1000:1000",
		"ghcr.io/tracepad/tracepad:0.2.0", "serve"}
	if !slices.Equal(got, want) {
		t.Errorf("run args\n got %q\nwant %q", got, want)
	}
	// What a mount has beyond type, source, destination and read-only is
	// named, not dropped (the review of #226): a bind relabelled or shared,
	// a volume's nocopy; docker's own "z" of a volume and "rprivate" of a
	// bind are no setting.
	mic := inspectContainer{Mounts: []mount{
		{Type: "volume", Name: "v", Destination: "/a", Mode: "z", RW: true},
		{Type: "bind", Source: "/s", Destination: "/b", Mode: "", RW: true, Propagation: "rprivate"},
		{Type: "bind", Source: "/s", Destination: "/c", Mode: "ro", Propagation: "rprivate"},
		{Type: "bind", Source: "/s", Destination: "/d", Mode: "Z", RW: true, Propagation: "rprivate"},
		{Type: "bind", Source: "/s", Destination: "/e", Mode: "rshared", RW: true, Propagation: "rshared"},
	}}
	_ = json.Unmarshal([]byte(`{"HostConfig":{"Mounts":[{"Type":"volume","Source":"w","Target":"/f","VolumeOptions":{"NoCopy":true}},{"Type":"bind","Source":"/s","Target":"/g"}]}}`), &mic.raw)
	wantLost := []string{"HostConfig.Mounts[/f].VolumeOptions", "Mounts[/d].Mode=Z", "Mounts[/e].Mode=rshared", "Mounts[/e].Propagation=rshared"}
	if got := mountLosses(mic); !slices.Equal(got, wantLost) {
		t.Errorf("mount losses %q, want %q", got, wantLost)
	}
	// Its log options are not carried — they can hold a credential — and
	// are named as what the run does not carry.
	ic.HostConfig.LogConfig.Config["splunk-token"] = "t0ken"
	if got := unreproduced(ic, img); !slices.Equal(got, []string{"HostConfig.LogConfig.Config[splunk-token]"}) {
		t.Errorf("a log option that can hold a credential: %q", got)
	}
	delete(ic.HostConfig.LogConfig.Config, "splunk-token")
	if env := personEnv(ic, names); !slices.Equal(env, []string{"TRACEPAD_PROJECTS=a:b:c"}) {
		t.Errorf("the person's environment: %q", env)
	}
	if got := runArgs(c, "n", "r", "e", "tp-run", "R", stepBackStarted); !slices.Contains(got, "type=volume,src=tp-run,dst=/data") || slices.Contains(got, "type=volume,src=tp,dst=/data") {
		t.Errorf("the volume swap: %q", got)
	}
	// The daemon's own log driver, with no options, is the daemon's again.
	ic.HostConfig.LogConfig.Type, ic.HostConfig.LogConfig.Config = "json-file", nil
	ic.HostConfig.RestartPolicy.Name = ""
	run, _, _ = createdAs(ic, img, "json-file")
	if slices.Contains(run, "--log-driver") || slices.Contains(run, "--restart") {
		t.Errorf("defaults written: %q", run)
	}
}

// What is the command's to recreate (Decision 4, #47), and why everything
// else is the person's.
func TestWhatContainerIsTheCommands(t *testing.T) {
	t.Parallel()
	d := newFakeDocker(t)
	base := d.setupContainer(t, t.TempDir()).inspectContainer
	img := d.images["ghcr.io/tracepad/tracepad:0.1.0"]
	info := dockerInfo{logDriver: "json-file"}
	classify := func(ic inspectContainer, named string) Container {
		raw, _ := json.Marshal(ic)
		_ = json.Unmarshal(raw, &ic.raw)
		c, _ := asContainer(ic)
		(&runner{}).recreate(&c, map[string]imageConfig{ic.Image: img}, nil, info)
		c.Version = "0.1.0"
		c.Reason = containerRefusal(c, named, info)
		return c
	}
	if c := classify(base, ""); c.Reason != "" || c.Volume != "tracepad-app" || c.URL != "http://127.0.0.1:4318" {
		t.Fatalf("setup's container: %+v", c)
	}
	for _, tc := range []struct {
		name   string
		change func(*inspectContainer)
		reason string
	}{
		{"compose", func(c *inspectContainer) {
			c.Config.Labels = map[string]string{"com.docker.compose.project": "obs"}
		}, "Compose runs it"},
		{"swarm", func(c *inspectContainer) { c.Config.Labels = map[string]string{"com.docker.swarm.service.id": "x"} }, "Swarm"},
		{"kubernetes", func(c *inspectContainer) { c.Config.Labels = map[string]string{"io.kubernetes.pod.name": "x"} }, "Kubernetes"},
		{"another name", func(c *inspectContainer) { c.Name = "/tp" }, "name it with --container"},
		{"open beyond", func(c *inspectContainer) {
			c.HostConfig.PortBindings = map[string][]portBinding{"4318/tcp": {{HostIP: "0.0.0.0", HostPort: "4318"}}}
		}, "beyond this machine"},
		{"every address", func(c *inspectContainer) {
			c.HostConfig.PortBindings = map[string][]portBinding{"4318/tcp": {{HostIP: "", HostPort: "4318"}}}
		}, "every address"},
		{"another port open", func(c *inspectContainer) {
			c.HostConfig.PortBindings = map[string][]portBinding{"4318/tcp": {{HostIP: "127.0.0.1", HostPort: "4318"}}, "4317/tcp": {{HostIP: "0.0.0.0", HostPort: "4317"}}}
		}, "4317/tcp on 0.0.0.0"},
		{"every port", func(c *inspectContainer) { c.HostConfig.PublishAllPorts = true }, "-P"},
		{"a port docker chooses", func(c *inspectContainer) {
			c.HostConfig.PortBindings = map[string][]portBinding{"4318/tcp": {{HostIP: "127.0.0.1", HostPort: ""}}}
			c.NetworkSettings.Ports = map[string][]portBinding{"4318/tcp": {{HostIP: "127.0.0.1", HostPort: "55012"}}}
		}, "a port docker chooses at each start"},
		{"not published", func(c *inspectContainer) {
			c.HostConfig.PortBindings, c.NetworkSettings.Ports = nil, nil
		}, "does not publish"},
		{"a bind at /data", func(c *inspectContainer) {
			c.Mounts = []mount{{Type: "bind", Source: "/srv", Destination: "/data", RW: true}}
		}, "not a volume"},
		{"a volume driver", func(c *inspectContainer) { c.Mounts[0].Driver = "nfs" }, "driver nfs"},
		{"a tmpfs", func(c *inspectContainer) {
			c.Mounts = append(c.Mounts, mount{Type: "tmpfs", Destination: "/tmp"})
		}, "tmpfs"},
		{"--rm", func(c *inspectContainer) { c.HostConfig.AutoRemove = true }, "--rm"},
		{"privileged", func(c *inspectContainer) { c.HostConfig.Privileged = true }, "privileged"},
		{"a network", func(c *inspectContainer) { c.HostConfig.NetworkMode = "obs" }, "network obs"},
		{"an entrypoint", func(c *inspectContainer) { c.Config.Entrypoint = []string{"/bin/sh"} }, "entrypoint"},
		{"limits", func(c *inspectContainer) { c.HostConfig.Memory = 1 << 30 }, "resource limits"},
		{"a line break", func(c *inspectContainer) { c.Config.Env = append(c.Config.Env, "TRACEPAD_X=a\nb") }, "line break"},
		{"stopped", func(c *inspectContainer) { c.State.Running = false }, "not running"},
		{"restarting", func(c *inspectContainer) { c.State.Restarting = true }, "not running"},
		{"an unknown mount", func(c *inspectContainer) {
			c.Mounts = append(c.Mounts, mount{Type: "npipe", Destination: "/pipe"})
		}, "a mount of type npipe"},
	} {
		c := base
		c.Config.Env = slices.Clone(base.Config.Env)
		c.Mounts = slices.Clone(base.Mounts)
		tc.change(&c)
		if got := classify(c, ""); !strings.Contains(got.Reason, tc.reason) {
			t.Errorf("%s: %q", tc.name, got.Reason)
		}
	}
	// What a run cannot carry is decided in one place, createdAs, and leaves
	// no run for the command or for the person: the advice prints no
	// docker run that would drop or cut it (the third review of #228). A
	// container that is the person's for whose it is — a name, an address
	// — still gets its run.
	for name, change := range map[string]func(*inspectContainer){
		"a value with a line break": func(c *inspectContainer) { c.Config.Env = append(c.Config.Env, "PEM=-----BEGIN\nKEY") },
		"a value with a return":     func(c *inspectContainer) { c.Config.Env = append(c.Config.Env, "NOTE=a\rb") },
		"a name with a space":       func(c *inspectContainer) { c.Config.Env = append(c.Config.Env, "A B=x") },
		"a mount it cannot write":   func(c *inspectContainer) { c.Mounts = append(c.Mounts, mount{Type: "npipe", Destination: "/pipe"}) },
	} {
		ic := base
		ic.Config.Env = slices.Clone(base.Config.Env)
		ic.Mounts = slices.Clone(base.Mounts)
		change(&ic)
		c := classify(ic, "")
		advice := containerAdvice(c, "0.2.0")
		if c.Run != nil || !strings.Contains(c.Reason, "its docker run could not be written: ") || strings.Contains(advice, "docker run -d") || !strings.Contains(advice, "which the command could not write") {
			t.Errorf("%s: run %q, reason %q, advice %s", name, c.Run, c.Reason, advice)
		}
	}
	// The image's own variables are in what the printed grep reads too: a
	// line break there would write a carried name's line nobody set (the
	// fifth review of #228).
	{
		bad := "BANNER=a\nTRACEPAD_PROJECTS=x"
		img.Config.Env = append(slices.Clone(img.Config.Env), bad)
		ic := base
		ic.Config.Env = append(slices.Clone(base.Config.Env), bad)
		if c := classify(ic, ""); c.Run != nil || !strings.Contains(c.Reason, "BANNER holds a line break") {
			t.Errorf("an image's variable with a line break: run %q, reason %q", c.Run, c.Reason)
		}
		img = d.images["ghcr.io/tracepad/tracepad:0.1.0"]
	}
	for name, change := range map[string]func(*inspectContainer){
		"another name": func(c *inspectContainer) { c.Name = "/myapp" },
		"open beyond": func(c *inspectContainer) {
			c.HostConfig.PortBindings = map[string][]portBinding{"4318/tcp": {{HostIP: "0.0.0.0", HostPort: "4318"}}}
		},
		"a name only docker takes": func(c *inspectContainer) { c.Config.Env = append(c.Config.Env, "my.var=x"); c.Name = "/myapp" },
	} {
		ic := base
		ic.Config.Env = slices.Clone(base.Config.Env)
		change(&ic)
		c := classify(ic, "")
		if advice := containerAdvice(c, "0.2.0"); c.Reason == "" || !strings.Contains(advice, "docker run -d") {
			t.Errorf("%s: %q, %s", name, c.Reason, advice)
		}
	}
	// A name the shell would not take is carried, quoted for grep and sh.
	ic := base
	ic.Config.Env = append(slices.Clone(base.Config.Env), "my.var=x")
	ic.Name = "/myapp"
	if advice := containerAdvice(classify(ic, ""), "0.2.0"); !strings.Contains(advice, `grep -E '^(TRACEPAD_PROJECTS|TRACEPAD_URL|my\.var)='`) {
		t.Errorf("my.var: %s", advice)
	}
	// A setting a recreate would drop: the person's, named.
	c := classify(base, "")
	c.Unreproduced = []string{"HostConfig.PidsLimit"}
	if r := containerRefusal(c, "", info); !strings.Contains(r, "does not reproduce (HostConfig.PidsLimit)") {
		t.Errorf("a setting a recreate drops: %q", r)
	}
	if r := containerRefusal(classify(base, ""), "", dockerInfo{remapped: "Docker runs rootless"}); !strings.Contains(r, "rootless") {
		t.Errorf("rootless: %q", r)
	}
	named := base
	named.Name = "/tp"
	if c := classify(named, "tp"); c.Reason != "" {
		t.Errorf("a container named with --container: %s", c.Reason)
	}
}

// What a recreate does not carry is named, and makes the container the
// person's; what the daemon sets by itself is not counted.
func TestSettingsARecreateWouldDropAreNamed(t *testing.T) {
	t.Parallel()
	var img imageConfig
	img.Config.ExposedPorts = map[string]struct{}{"4318/tcp": {}}
	ic := inspectContainer{ID: "0123456789abcdef"}
	ic.Config.Hostname = "0123456789ab"
	ic.Config.ExposedPorts = map[string]struct{}{"4318/tcp": {}}
	ic.HostConfig.PortBindings = map[string][]portBinding{"4318/tcp": {{HostIP: "127.0.0.1", HostPort: "4318"}}}
	defaults := `{"HostConfig":{"ShmSize":67108864,"Runtime":"runc","CgroupnsMode":"private","MaskedPaths":["/proc/kcore"],"ConsoleSize":[0,0],
		"PidsLimit":null,"MemorySwappiness":null,"OomScoreAdj":0,"CpusetCpus":"","BlkioWeight":0,"RestartPolicy":{"Name":"always"},"IpcMode":"private"},
		"Config":{"Hostname":"0123456789ab","AttachStdout":true,"Tty":false,"Env":["A=b"]}}`
	_ = json.Unmarshal([]byte(defaults), &ic.raw)
	if got := unreproduced(ic, img); len(got) != 0 {
		t.Errorf("defaults counted: %q", got)
	}
	set := `{"HostConfig":{"PidsLimit":100,"CpusetCpus":"0-1","MemorySwap":1073741824,"Runtime":"runsc","OomScoreAdj":500,"ShmSize":134217728},
		"Config":{"Hostname":"mine","Tty":true,"StopTimeout":30}}`
	_ = json.Unmarshal([]byte(set), &ic.raw)
	ic.Config.Hostname = "mine"
	ic.Config.ExposedPorts["9000/tcp"] = struct{}{}
	want := []string{"Config.ExposedPorts[9000/tcp]", "Config.Hostname", "Config.StopTimeout", "Config.Tty",
		"HostConfig.CpusetCpus", "HostConfig.MemorySwap", "HostConfig.OomScoreAdj", "HostConfig.PidsLimit", "HostConfig.Runtime", "HostConfig.ShmSize"}
	if got := unreproduced(ic, img); !slices.Equal(got, want) {
		t.Errorf("named %q,\nwant %q", got, want)
	}
}

// The person's container with a setting this run does not carry gets its
// commands with that setting named, not "a user, limits, labels".
func TestThePersonsCommandsNameWhatTheyDoNotCarry(t *testing.T) {
	t.Parallel()
	d := newFakeDocker(t)
	c := d.add("tracepad-app", "0.1.0", "127.0.0.1", "4318", "tracepad-app", nil)
	d.set(func() { c.extra = map[string]any{"HostConfig.PidsLimit": 100} })
	rep, code := runReport(t, containerDeps(t, d), "--plan")
	all := strings.Join(rep.Person, "\n")
	if code != exitDecide || !strings.Contains(all, "settings the command does not reproduce (HostConfig.PidsLimit)") ||
		!strings.Contains(all, "It was also given what this docker run does not carry — HostConfig.PidsLimit — which docker inspect tracepad-app-old shows") {
		t.Errorf("%d %s", code, all)
	}
	// One that is the person's because it is privileged, with limits, says
	// so in its commands too (the review of #226).
	d.set(func() { c.extra = nil; c.HostConfig.Privileged, c.HostConfig.Memory = true, 1<<30 })
	rep, _ = runReport(t, containerDeps(t, d), "--plan")
	if all = strings.Join(rep.Person, "\n"); !strings.Contains(all, "does not carry — HostConfig.Privileged, HostConfig.Memory/NanoCpus/CpuShares/CpuQuota —") {
		t.Errorf("%s", all)
	}
}

// setup's container, upgraded: healthy, recreated with its mounts, policy
// and variables; the old one set aside at restart policy no; the host's
// binary brought along; then its way back: a new volume from the archive,
// the new container set aside, the old image on the new volume — and run
// again, it ends where the first did and changes nothing (#31).
func testAContainerUpgradeKeepsMountsPolicyAndVariables(t *testing.T) {
	d := newFakeDocker(t)
	bind := filepath.Join(t.TempDir(), "My Data")
	d.setupContainer(t, bind)
	deps := containerDeps(t, d, "0.1.0")
	bin := filepath.Join(deps.InstallDir, "tracepad")

	plan, code := runReport(t, deps, "--plan")
	if code != exitPending || plan.To != "0.2.0" || len(plan.Containers) != 1 || !plan.Containers[0].Target {
		t.Fatalf("plan: %d %+v", code, plan)
	}
	if steps := strings.Join(plan.Plan, "\n"); !strings.Contains(steps, "with what it was created with: -p 127.0.0.1:4318:4318; --mount type=volume,src=tracepad-app,dst=/data;") ||
		!strings.Contains(steps, "the variables TRACEPAD_PROJECTS, TRACEPAD_URL") || strings.Contains(steps, "tp-sk-b") || !strings.Contains(steps, "then put tracepad 0.2.0 at "+bin) {
		t.Errorf("the plan's steps: %s", steps)
	}
	if len(plan.Next) == 0 || !strings.HasPrefix(plan.Next[0], deps.Self+" upgrade") {
		t.Errorf("the plan hands on a bare command: %q", plan.Next)
	}
	if slices.ContainsFunc(d.calls, func(c []string) bool { return c[0] == "kill" || c[0] == "run" || c[0] == "pull" }) {
		t.Fatalf("the plan acted: %q", d.calls)
	}

	rep, code := runReport(t, deps)
	if code != exitOK || rep.Check == nil || rep.Check.Verdict != verdictHealthy {
		t.Fatalf("upgrade: %d %+v", code, rep)
	}
	newC := d.container("tracepad-app")
	if newC.Config.Image != "ghcr.io/tracepad/tracepad:0.2.0" || newC.HostConfig.RestartPolicy.Name != "always" || !slices.Equal(newC.Config.Cmd, []string{"serve"}) {
		t.Errorf("new container: %+v", newC)
	}
	if !slices.Contains(newC.Config.Env, "TRACEPAD_PROJECTS=app:tp-pk-a:tp-sk-b") || strings.Count(strings.Join(newC.Config.Env, "\n"), "TRACEPAD_LISTEN=") != 1 {
		t.Errorf("environment: %q", newC.Config.Env)
	}
	if i := slices.IndexFunc(newC.Mounts, func(m mount) bool { return m.Destination == "/tls" }); i < 0 || newC.Mounts[i].RW || newC.Mounts[i].Source != bind {
		t.Errorf("the read-only bind: %+v", newC.Mounts)
	}
	if v, _ := scriptVersion(context.Background(), bin); v != "0.2.0" {
		t.Errorf("the host's binary is %s", v)
	}
	if want := filepath.Join(rep.Run.Dir, "upgrader") + " upgrade --back " + rep.Run.ID; !strings.Contains(strings.Join(rep.Next, "\n"), want) {
		t.Errorf("the way back is not handed on by this binary's copy: %q", rep.Next)
	}
	before := d.container("tracepad-app-before-" + rep.Run.ID)
	if before == nil || before.HostConfig.RestartPolicy.Name != "no" || before.State.Running {
		t.Errorf("the old container set aside: %+v", before)
	}
	for _, c := range d.calls {
		for _, a := range c {
			if strings.Contains(a, "tp-sk-b") {
				t.Errorf("a secret on a command line: %q", c)
			}
		}
	}
	if fi, err := os.Stat(filepath.Join(rep.Run.Dir, "env")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("env file: %v %v", fi, err)
	}

	d.set(func() { d.volumes["tracepad-app"] = 9 })
	back, code := runReport(t, deps, "--back", rep.Run.ID)
	if code != exitOK {
		t.Fatalf("back: %d %+v", code, back)
	}
	cur := d.container("tracepad-app")
	vol := "tracepad-app-" + rep.Run.ID
	if cur.Config.Image != "ghcr.io/tracepad/tracepad:0.1.0" || cur.HostConfig.RestartPolicy.Name != "always" ||
		!slices.ContainsFunc(cur.Mounts, func(m mount) bool { return m.Name == vol && m.Destination == "/data" }) {
		t.Errorf("after the way back: %+v", cur)
	}
	if d.volumes[vol] != 7 || d.volumes["tracepad-app"] != 9 {
		t.Errorf("volumes: %v", d.volumes)
	}
	after := d.container("tracepad-app-after-" + rep.Run.ID)
	if after == nil || after.HostConfig.RestartPolicy.Name != "no" || after.State.Running {
		t.Errorf("the new container set aside: %+v", after)
	}
	if v, _ := scriptVersion(context.Background(), bin); v != "0.1.0" {
		t.Errorf("the host's binary after the way back is %s", v)
	}
	calls := len(d.calls)
	if again, code := runReport(t, deps, "--back", rep.Run.ID); code != exitOK {
		t.Errorf("second way back: %d %s", code, again.Summary)
	}
	for _, c := range d.calls[calls:] {
		if c[0] != "container" && c[0] != "image" && c[0] != "logs" && c[0] != "volume" && !(c[0] == "run" && slices.Contains(c, "df")) {
			t.Errorf("the second way back acted: docker %q", c)
		}
	}
}

func testABrokenImageGoesBackIntoANewVolume(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	d.brokenRef = "ghcr.io/tracepad/tracepad:0.2.1"
	deps := containerDeps(t, d)
	rep, code := runReport(t, deps, "--to", "0.2.1")
	if code != exitWentBack {
		t.Fatalf("%d %+v", code, rep)
	}
	cur := d.container("tracepad-app")
	if !cur.State.Running || cur.Config.Image != "ghcr.io/tracepad/tracepad:0.1.0" || cur.HostConfig.RestartPolicy.Name != "always" {
		t.Errorf("after the way back: %+v", cur)
	}
	if _, ok := d.volumes["tracepad-app-"+rep.Run.ID]; !ok {
		t.Error("no new volume")
	}
	if !slices.Contains(rep.SetAside, "volume tracepad-app") {
		t.Errorf("set aside: %q", rep.SetAside)
	}
}

func testAFailedRunPutsTheOldContainerBack(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	d.failRun = true
	deps := containerDeps(t, d)
	rep, code := runReport(t, deps)
	if code != exitWentBack {
		t.Fatalf("%d %+v", code, rep)
	}
	cur := d.container("tracepad-app")
	if !cur.State.Running || cur.Config.Image != "ghcr.io/tracepad/tracepad:0.1.0" || cur.HostConfig.RestartPolicy.Name != "always" {
		t.Errorf("the old container: %+v", cur)
	}
	failed := d.container("tracepad-app-failed-" + rep.Run.ID)
	if failed == nil || failed.HostConfig.RestartPolicy.Name != "no" {
		t.Errorf("the half-made container: %+v", failed)
	}
}

func testAWayBackWhoseVolumeExistsTouchesNothing(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d)
	rep, code := runReport(t, deps)
	if code != exitOK {
		t.Fatalf("%d %+v", code, rep)
	}
	d.set(func() { d.volumes["tracepad-app-"+rep.Run.ID] = 1; d.calls = nil })
	back, code := runReport(t, deps, "--back", rep.Run.ID)
	if code != exitStuck || !strings.Contains(back.Summary, "exists already") {
		t.Fatalf("%d %s", code, back.Summary)
	}
	if slices.ContainsFunc(d.calls, func(c []string) bool { return c[0] == "kill" || c[0] == "rename" || (c[0] == "run" && c[1] == "-d") }) {
		t.Errorf("it acted: %q", d.calls)
	}
}

func testAMissingImageStopsNothing(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d)
	delete(d.images, "ghcr.io/tracepad/tracepad:0.2.0")
	rep, code := runReport(t, deps)
	if code != exitRefused || !strings.Contains(rep.Summary, "could not pull") {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	if !d.container("tracepad-app").State.Running {
		t.Error("the container was stopped")
	}
	if entries, _ := os.ReadDir(deps.Backups); len(entries) > 1 {
		t.Errorf("the refused run's directory was left: %v", entries)
	}
}

func testAWayBackLeavesALaterRunsContainerAlone(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d)
	rep, code := runReport(t, deps)
	if code != exitOK {
		t.Fatalf("%d %+v", code, rep)
	}
	d.set(func() { d.byName["tracepad-app"].ID = strings.Repeat("9", 64); d.calls = nil })
	back, code := runReport(t, deps, "--back", rep.Run.ID)
	if code != exitStuck || !strings.Contains(back.Summary, "not the one this run started") {
		t.Fatalf("%d %s", code, back.Summary)
	}
	if slices.ContainsFunc(d.calls, func(c []string) bool {
		return c[0] == "kill" || c[0] == "rename" || (c[0] == "volume" && c[1] == "create")
	}) {
		t.Errorf("it acted: %q", d.calls)
	}
}

// A way back whose old version never answers is not reported as healthy.
func testAWayBackThatIsNotConfirmedSaysSo(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	d.brokenRef = "ghcr.io/tracepad/tracepad:0.2.1"
	deps := containerDeps(t, d)
	d.silentRef = "ghcr.io/tracepad/tracepad:0.1.0"
	rep, code := runReport(t, deps, "--to", "0.2.1")
	if code != exitStuck || !strings.Contains(rep.Summary, "has not shown it is healthy") || strings.Contains(rep.Summary, "runs again, healthy") {
		t.Fatalf("%d %s", code, rep.Summary)
	}
}

// A restore that fails leaves the live volume the live one, says which
// half-made volume to remove, and a second way back says so too.
func testAFailedRestoreNamesWhatItLeft(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d)
	rep, code := runReport(t, deps)
	if code != exitOK {
		t.Fatalf("%d %+v", code, rep)
	}
	d.set(func() { d.failRestore = true })
	vol := "tracepad-app-" + rep.Run.ID
	back, code := runReport(t, deps, "--back", rep.Run.ID)
	if code != exitStuck || !strings.Contains(back.Summary, vol) || !strings.Contains(back.Summary, "run --back again") {
		t.Fatalf("%d %s", code, back.Summary)
	}
	all := strings.Join(append(back.Person, back.SetAside...), "\n")
	if strings.Contains(all, "docker volume rm tracepad-app;") || strings.Contains(all, "docker volume rm tracepad-app\n") || strings.HasSuffix(all, "docker volume rm tracepad-app") || slices.Contains(back.SetAside, "volume tracepad-app") {
		t.Errorf("the live volume is offered for removal: %q", all)
	}
	if !d.container("tracepad-app").State.Running {
		t.Error("the new container was stopped")
	}
	// Its restore is a way back's: --back finishes it, --check refuses.
	if chk, code := runReport(t, deps, "--check", rep.Run.ID); code != exitRefused || !strings.Contains(chk.Summary, "way back has begun") {
		t.Errorf("--check after a restore begun: %d %s", code, chk.Summary)
	}
	// The half-filled volume is the run's own (#34): the next way back fills
	// it again, and nothing is left for the person to remove first.
	d.set(func() { d.failRestore = false })
	again, code := runReport(t, deps, "--back", rep.Run.ID)
	if code != exitOK || d.volumes[vol] != 7 {
		t.Errorf("second way back: %d %s; %s holds %d", code, again.Summary, vol, d.volumes[vol])
	}
}

func testTheRemovalsRemoveContainersBeforeVolumes(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d)
	rep, code := runReport(t, deps)
	if code != exitOK {
		t.Fatalf("%d %+v", code, rep)
	}
	back, code := runReport(t, deps, "--back", rep.Run.ID)
	if code != exitOK {
		t.Fatalf("%d %s", code, back.Summary)
	}
	sentence := back.Person[len(back.Person)-1]
	last := strings.LastIndex(sentence, "docker rm ")
	first := strings.Index(sentence, "docker volume rm ")
	if last < 0 || first < 0 || last > first {
		t.Errorf("a volume is removed before a container that mounts it: %s", sentence)
	}
}

// An interrupt after the stop must not leave the container down: the rest
// of the run, and its way back, finish on a context of their own (the
// second review).
func testAnInterruptAfterTheContainerStopsStillBringsItBack(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	d.failRun = true
	deps := containerDeps(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.onCall = func(args []string) {
		if args[0] == "kill" {
			cancel()
		}
	}
	var out bytes.Buffer
	code := run(ctx, Options{Args: []string{"--json"}, Stdout: &out, Stderr: io.Discard}, deps)
	var rep Report
	_ = json.Unmarshal(out.Bytes(), &rep)
	if code != exitWentBack {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	cur := d.container("tracepad-app")
	if cur == nil || !cur.State.Running || cur.HostConfig.RestartPolicy.Name != "always" || cur.Config.Image != "ghcr.io/tracepad/tracepad:0.1.0" {
		t.Errorf("the old container: %+v", cur)
	}
}

// The container's archive is pinned by its bytes: one changed since is not
// restored.
func testAContainerArchiveChangedSinceIsNotRestored(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d)
	rep, code := runReport(t, deps)
	if code != exitOK {
		t.Fatalf("%d %+v", code, rep)
	}
	archive := filepath.Join(rep.Run.Dir, "data.tar.gz")
	if err := os.WriteFile(archive, archiveOf(t, map[string][]byte{"./tracepad.db": d.dbFile, "./extra": []byte("x")}), 0o600); err != nil {
		t.Fatal(err)
	}
	back, code := runReport(t, deps, "--back", rep.Run.ID)
	if code != exitStuck || !strings.Contains(back.Summary, "checksum changed") {
		t.Fatalf("%d %s", code, back.Summary)
	}
	if _, ok := d.volumes["tracepad-app-"+rep.Run.ID]; ok {
		t.Error("a volume was made from it")
	}
}

// A docker run the way back made and could not start (a port still taken)
// leaves a container under the name; the next --back sets it aside and goes
// on (the third review).
func testAFailedRunInTheWayBackDoesNotBlockTheNext(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d)
	rep, code := runReport(t, deps)
	if code != exitOK {
		t.Fatalf("%d %+v", code, rep)
	}
	d.set(func() { d.failRun = true })
	first, code := runReport(t, deps, "--back", rep.Run.ID)
	if code != exitStuck {
		t.Fatalf("first: %d %s", code, first.Summary)
	}
	d.set(func() { d.failRun = false })
	second, code := runReport(t, deps, "--back", rep.Run.ID)
	if code != exitOK {
		t.Fatalf("second: %d %s", code, second.Summary)
	}
	left := d.container("tracepad-app-failed-" + rep.Run.ID)
	if left == nil || left.HostConfig.RestartPolicy.Name != "no" {
		t.Errorf("the container the failed run left: %+v", left)
	}
	if cur := d.container("tracepad-app"); cur == nil || !cur.State.Running || cur.Config.Image != "ghcr.io/tracepad/tracepad:0.1.0" {
		t.Errorf("the old image does not run: %+v", cur)
	}
}

// The person removed the new container after an upgrade: the way back has
// nothing to set aside, and goes on.
func testAWayBackAfterTheNewContainerWasRemoved(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d)
	rep, code := runReport(t, deps)
	if code != exitOK {
		t.Fatalf("%d %+v", code, rep)
	}
	d.set(func() { delete(d.byName, "tracepad-app") })
	back, code := runReport(t, deps, "--back", rep.Run.ID)
	if code != exitOK {
		t.Fatalf("%d %s", code, back.Summary)
	}
	if cur := d.container("tracepad-app"); cur == nil || !cur.State.Running || cur.Config.Image != "ghcr.io/tracepad/tracepad:0.1.0" {
		t.Errorf("the old image does not run: %+v", cur)
	}
}

func TestRootlessDockerIsThePersons(t *testing.T) {
	t.Parallel()
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	d.rootless = true
	deps := containerDeps(t, d)
	plan, code := runReport(t, deps, "--plan")
	if code != exitDecide || len(plan.Containers) != 1 || plan.Containers[0].Whose != "person" || !strings.Contains(plan.Containers[0].Reason, "rootless") {
		t.Fatalf("%d %+v", code, plan.Containers)
	}
}

// A deadline the caller set that runs out during the swap is not the way
// back's: it has a budget of its own (the third review).
func testADeadlineSpentByTheSwapIsNotTheWayBacks(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	d.failRun = true
	deps := containerDeps(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	d.onCall = func(args []string) {
		if args[0] == "kill" {
			<-ctx.Done()
		}
	}
	var out bytes.Buffer
	code := run(ctx, Options{Args: []string{"--json"}, Stdout: &out, Stderr: io.Discard}, deps)
	var rep Report
	_ = json.Unmarshal(out.Bytes(), &rep)
	if code != exitWentBack {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	if cur := d.container("tracepad-app"); cur == nil || !cur.State.Running {
		t.Errorf("the old container: %+v", cur)
	}
}

// Interrupted after the stop, a healthy swap goes on to the end: the upgrade
// is finished, not undone.
func testAnInterruptedHealthySwapFinishes(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.onCall = func(args []string) {
		if args[0] == "kill" {
			cancel()
		}
	}
	var out bytes.Buffer
	code := run(ctx, Options{Args: []string{"--json"}, Stdout: &out, Stderr: io.Discard}, deps)
	var rep Report
	_ = json.Unmarshal(out.Bytes(), &rep)
	if code != exitOK {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	if cur := d.container("tracepad-app"); cur == nil || cur.Config.Image != "ghcr.io/tracepad/tracepad:0.2.0" {
		t.Errorf("the new container: %+v", cur)
	}
}

// The real docker refuses a restart policy's update now and then while a
// crash-looping container is between two of its states; the way back from a
// release that does not start asks again, and goes back.
func testAWayBackAsksDockerAgainForAPolicy(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	d.brokenRef = "ghcr.io/tracepad/tracepad:0.2.1"
	deps := containerDeps(t, d)
	d.onCall = func(args []string) {
		if args[0] == "run" && args[1] == "-d" && slices.Contains(args, "ghcr.io/tracepad/tracepad:0.2.1") {
			d.set(func() { d.flakyUpdate = 3 })
		}
	}
	rep, code := runReport(t, deps, "--to", "0.2.1")
	if code != exitWentBack {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	if cur := d.container("tracepad-app"); !cur.State.Running || cur.Config.Image != "ghcr.io/tracepad/tracepad:0.1.0" {
		t.Errorf("after the way back: %+v", cur)
	}
}

// A container is stopped as a server is (the sixth review): SIGTERM, its
// restart policy set to no so docker does not start it again, and no
// SIGKILL after the wait — one that has not stopped leaves the run stuck,
// and --back puts its policy back and starts it.
func testAContainerThatDoesNotStopIsNeverKilled(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	d.stubborn = true
	deps := containerDeps(t, d)
	deps.StopWait = 50 * time.Millisecond
	rep, code := runReport(t, deps)
	if code != exitStuck || !strings.Contains(rep.Summary, "has not stopped") {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	c := d.container("tracepad-app")
	if !c.State.Running || c.HostConfig.RestartPolicy.Name != "no" {
		t.Errorf("the container after the stop's wait: running %v, policy %s", c.State.Running, c.HostConfig.RestartPolicy.Name)
	}
	d.set(func() { d.stubborn = false; c.State.Running = false }) // it stopped, late
	if back, code := runReport(t, deps, "--back", rep.Run.ID); code != exitOK {
		t.Fatalf("--back: %d %s", code, back.Summary)
	}
	if !c.State.Running || c.HostConfig.RestartPolicy.Name != "always" {
		t.Errorf("after --back: running %v, policy %s", c.State.Running, c.HostConfig.RestartPolicy.Name)
	}
}

// The eighth review: a container's way back whose check could not confirm
// the old version, after the host's binary was put back, is taken up again
// when the old container has stopped since — never wedged off its table.
func testAContainerWayBackStartsAgainAfterTheBinaryWasPutBack(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d, "0.1.0")
	rep, code := runReport(t, deps)
	if code != exitOK {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	d.set(func() { d.silentRef = "ghcr.io/tracepad/tracepad:0.1.0" })
	back, code := runReport(t, deps, "--back", rep.Run.ID)
	if code != exitStuck || !strings.Contains(back.Summary, "has not shown it is healthy") {
		t.Fatalf("first --back: %d %s", code, back.Summary)
	}
	if st, _ := loadState(rep.Run.Dir); st.last() != stepBackBinary {
		t.Fatalf("the first way back ends at %q", st.last())
	}
	d.set(func() { d.silentRef = ""; d.byName["tracepad-app"].State.Running = false })
	if back, code = runReport(t, deps, "--back", rep.Run.ID); code != exitOK {
		t.Fatalf("second --back: %d %s", code, back.Summary)
	}
	if cur := d.container("tracepad-app"); !cur.State.Running || cur.Config.Image != "ghcr.io/tracepad/tracepad:0.1.0" {
		t.Errorf("%+v", cur)
	}
}

// The eighth review: a docker that does not answer while a run is settled
// says nothing either way — the run is refused, its intent kept — and once it
// answers the run is settled from what it says.
func testADockerThatDoesNotAnswerSettlesNothing(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d)
	deps.Fault = func(point string) error {
		if point == stepStarted+recordPoint {
			panic(killed{})
		}
		return nil
	}
	if _, code := runIn(t, context.Background(), deps); code != codeKilled {
		t.Fatalf("not killed: %d", code)
	}
	deps.Fault = nil
	id, dir := runOf(t, deps)
	d.set(func() { d.down = true })
	back, code := runReport(t, deps, "--back", id)
	if st, _ := loadState(dir); code != exitRefused || st.Pending == nil || st.Pending.Step != stepStarted {
		t.Fatalf("a docker that does not answer: %d %s; %+v", code, back.Summary, st.Pending)
	}
	d.set(func() { d.down = false })
	if back, code = runReport(t, deps, "--back", id); code != exitOK {
		t.Fatalf("--back: %d %s", code, back.Summary)
	}
	if st, _ := loadState(dir); !st.has(stepStarted) || !st.has(stepBackVolume) {
		t.Errorf("the new version ran on the volume, and the way back did not restore it: %+v", st.Steps)
	}
}

// The eighth review: a container's way back is not refused for a binary the
// run never replaced — the installed one already the target, changed since
// by the person.
func testAContainerWayBackIsNotRefusedForABinaryItNeverReplaced(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d, "0.2.0")
	rep, code := runReport(t, deps)
	if code != exitOK {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	if st, _ := loadState(rep.Run.Dir); st.has(stepBinaryReplacing) {
		t.Fatal("the binary at the target was replaced")
	}
	scriptBinary(t, filepath.Join(deps.InstallDir, "tracepad"), "0.2.1")
	if back, code := runReport(t, deps, "--back", rep.Run.ID); code != exitOK {
		t.Fatalf("--back: %d %s", code, back.Summary)
	}
	if v, _ := scriptVersion(context.Background(), filepath.Join(deps.InstallDir, "tracepad")); v != "0.2.1" {
		t.Errorf("the person's binary was changed: %s", v)
	}
}

// A volume with options of its own is the person's to upgrade: a way back's
// new volume would not have them. Refused before anything stops.
func testAVolumeWithOptionsIsRefusedBeforeTheStop(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	d.options["tracepad-app"] = `{"type":"nfs"}`
	rep, code := runReport(t, containerDeps(t, d))
	if code != exitRefused || !strings.Contains(rep.Summary, "has options of its own") || !d.container("tracepad-app").State.Running {
		t.Errorf("%d %s", code, rep.Summary)
	}
}

// newContainerWorld is the matrices' docker: the releases of the fake world
// as images, fBroken's a server that exits, and setup's container of fOld,
// restart policy always, on a volume of 2 traces, published on 4318.
func newContainerWorld(t *testing.T, dir string) *fakeDocker {
	d := newFakeDocker(t)
	for _, v := range []string{fOld, fNew, fBroken} {
		d.image("ghcr.io/tracepad/tracepad:"+v, v)
	}
	d.brokenRef = "ghcr.io/tracepad/tracepad:" + fBroken
	d.volumes["tracepad-app"] = 2
	env := filepath.Join(dir, "setup.env")
	_ = os.WriteFile(env, []byte("TRACEPAD_URL=http://localhost:4318\nTRACEPAD_PROJECTS=app:tp-pk-a:tp-sk-b\n"), 0o600)
	d.run(t, "--name", "tracepad-app", "--restart", "always", "--mount", "type=volume,src=tracepad-app,dst=/data",
		"-p", "127.0.0.1:4318:4318", "--env-file", env, "ghcr.io/tracepad/tracepad:"+fOld, "serve")
	return d
}

// answers is what the setup's address answers.
func (d *fakeDocker) answers() string {
	v, _ := health(context.Background(), &http.Client{Transport: d}, "http://127.0.0.1:4318")
	return v
}

// traces are what the volume of the container under name holds.
func (d *fakeDocker) traces(name string) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.byName[name]
	if c == nil {
		return -1
	}
	for _, m := range c.Mounts {
		if m.Destination == "/data" {
			return d.volumes[m.Name]
		}
	}
	return -1
}

// snapshot is what --back again must not change: what answers, which
// container runs under the name and on which volume, and the names and
// volumes there are.
func (d *fakeDocker) snapshot() string {
	answers := d.answers()
	d.mu.Lock()
	defer d.mu.Unlock()
	var names []string
	for n, c := range d.byName {
		names = append(names, fmt.Sprintf("%s=%s/%v/%s", n, c.ID[:6], c.State.Running, c.HostConfig.RestartPolicy.Name))
	}
	var vols []string
	for v, n := range d.volumes {
		vols = append(vols, fmt.Sprintf("%s=%d", v, n))
	}
	slices.Sort(names)
	slices.Sort(vols)
	return fmt.Sprintf("answers %s; containers %q; volumes %q", answers, names, vols)
}

// stopLate completes a stop a stubborn container ignored: it exits, as one
// asked to stop does in the end.
func (d *fakeDocker) stopLate() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.stubborn {
		return
	}
	d.stubborn = false
	for _, c := range d.byName {
		if d.termed[c.ID] {
			c.State.Running = false
		}
	}
}

// A docker call is asked again only while docker answers its restart
// manager's race, and at most twenty times; a container that is gone, or a
// daemon that does not answer, is said at once (the review of #226).
func TestOnlyDockersRaceIsAskedAgain(t *testing.T) {
	t.Parallel()
	r := &runner{deps: Deps{Sleep: func(context.Context, time.Duration) error { return nil }}}
	for _, tc := range []struct {
		answers []string
		calls   int
		ok      bool
	}{
		{[]string{"Error: No such container: x"}, 1, false},
		{[]string{"Cannot connect to the Docker daemon"}, 1, false},
		{[]string{"cannot update a stopped container", "cannot update a stopped container", ""}, 3, true},
		{[]string{"Container x is restarting, wait until the container is running", ""}, 2, true},
		{[]string{"cannot update a stopped container"}, 21, false},
	} {
		calls := 0
		err := r.transient(context.Background(), func() error {
			a := tc.answers[min(calls, len(tc.answers)-1)]
			calls++
			if a == "" {
				return nil
			}
			return errors.New(a)
		})
		if calls != tc.calls || (err == nil) != tc.ok {
			t.Errorf("%q: %d calls, %v", tc.answers, calls, err)
		}
	}
}

// A process that may run from the host's binary, found only by the run's
// own look — not by the plan's — keeps the binary, said, and the container
// is upgraded all the same (the review of #226); the skill, which follows the
// binary, is not installed and nothing is said of it.
func testAProcessOnTheBinaryKeepsItAndTheContainerGoesOn(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d, "0.1.0")
	install := filepath.Join(deps.InstallDir, "tracepad")
	deps.Sys = &lateProcess{p: Process{PID: 4242, Exe: install, Argv: []string{"tracepad", "serve", "--data-dir"}}}
	rep, code := runReport(t, deps)
	if code != exitOK {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	notes := strings.Join(rep.Notes, "\n")
	if !strings.Contains(notes, install+" stays 0.1.0 after the container's upgrade: server pid 4242") || strings.Contains(notes, "skill") {
		t.Errorf("notes: %s", notes)
	}
	if v, _ := scriptVersion(context.Background(), install); v != "0.1.0" {
		t.Errorf("the binary is %s", v)
	}
	if cur := d.container("tracepad-app"); cur.Config.Image != "ghcr.io/tracepad/tracepad:0.2.0" {
		t.Errorf("the container: %s", cur.Config.Image)
	}
}

// lateProcess is a machine whose one process — a tracepad whose
// configuration does not read — shows only from the second look on.
type lateProcess struct {
	noProcesses
	mu    sync.Mutex
	looks int
	p     Process
}

func (l *lateProcess) Candidates(context.Context) ([]Process, int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.looks++
	if l.looks == 1 {
		return nil, 0, nil
	}
	return []Process{l.p}, 0, nil
}

// The installed binary's plan of its own version looks no release up (the
// live run of 0.1.0): the install script asks the binary it has just put in
// place, whose first connection a firewall may hold past the script's
// fifteen seconds. Any other version, a binary run from elsewhere, or a run
// still looks (the second review of #228): a version stamped on a build
// need not be a release.
func TestThePlanOfItsOwnVersionLooksNothingUp(t *testing.T) {
	t.Parallel()
	d := newFakeDocker(t)
	ask := func(version string, self bool, args ...string) Report {
		deps := containerDeps(t, d, version)
		if self {
			deps.Self = filepath.Join(deps.InstallDir, "tracepad")
		}
		var out bytes.Buffer
		run(context.Background(), Options{Args: append(args, "--to", "0.9.9", "--json"), Version: version, Stdout: &out, Stderr: io.Discard}, deps)
		var rep Report
		if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
			t.Fatalf("%v: %s", err, out.String())
		}
		return rep
	}
	if rep := ask("0.9.9", true, "--plan"); strings.Contains(rep.Summary, "no release") {
		t.Errorf("the installed binary's own version was looked up: %s", rep.Summary)
	}
	for name, rep := range map[string]Report{
		"another version":             ask("0.2.0", true, "--plan"),
		"a binary run from elsewhere": ask("0.9.9", false, "--plan"),
		"a run":                       ask("0.9.9", true),
	} {
		if !strings.Contains(rep.Summary, "there is no release v0.9.9") {
			t.Errorf("%s was not looked up: %s", name, rep.Summary)
		}
	}
}

// The binary at the install path is one of a closed set of kinds, sorted in
// one place, and the plan's table says for each what is printed and what the
// plan exits (spec 054 #56): every kind has a cell here, and each cell holds
// the classification, the line, "nothing to do" and the exit status. A
// development build is the person's to replace, exit 4, never 0; a package
// manager's is its manager's, whatever it says, never the install script's
// line over its link (the fifth review of #228). A tracepad first on PATH
// that is behind is named and not counted.
func TestEveryKindOfBinaryIsSortedOnce(t *testing.T) {
	t.Parallel()
	type want struct {
		kind  binaryKind
		code  int
		idle  bool
		line  string // in the person's list, "" for none of the binary's
		never string // never in the person's list
		note  string // in the notes, when the case is about one
	}
	cellar := func(t *testing.T, deps *Deps) string {
		deps.InstallDir = filepath.Join(t.TempDir(), "Cellar", "tracepad", "HEAD", "bin")
		_ = os.MkdirAll(deps.InstallDir, 0o755)
		return filepath.Join(deps.InstallDir, "tracepad")
	}
	readOnly := func(t *testing.T, deps *Deps, version string) {
		if os.Geteuid() == 0 {
			t.Skip("root writes into any directory")
		}
		scriptBinary(t, filepath.Join(deps.InstallDir, "tracepad"), version)
		_ = os.Chmod(deps.InstallDir, 0o500)
		t.Cleanup(func() { _ = os.Chmod(deps.InstallDir, 0o700) })
	}
	bin := func(deps *Deps) string { return filepath.Join(deps.InstallDir, "tracepad") }
	type cell struct {
		setup func(t *testing.T, deps *Deps)
		want  want
	}
	cases := map[string]cell{
		"nothing there": {func(*testing.T, *Deps) {}, want{kind: binNone, code: exitOK}},
		"the command's, current": {func(t *testing.T, deps *Deps) { scriptBinary(t, bin(deps), "0.2.0") },
			want{kind: binOurs, code: exitOK, idle: true}},
		"the command's, behind": {func(t *testing.T, deps *Deps) { scriptBinary(t, bin(deps), "0.1.0") },
			want{kind: binOurs, code: exitPending}},
		// The container is upgraded and the binary kept back: still behind,
		// whatever the run does with it (the third review of #228).
		"the command's, behind, kept by a container's run": {func(t *testing.T, deps *Deps) {
			scriptBinary(t, bin(deps), "0.1.0")
			deps.Sys = unreadableProcesses{}
			deps.Docker.(*fakeDocker).add("tracepad-app", "0.1.0", "127.0.0.1", "4318", "app", nil)
		}, want{kind: binOurs, code: exitPending, note: "stays 0.1.0 after the container's upgrade"}},
		"a development build": {func(t *testing.T, deps *Deps) { scriptBinary(t, bin(deps), "97d6b79") },
			want{kind: binDev, code: exitDecide, line: `says it is "97d6b79", a development build, which the command does not replace; to put 0.2.0 in its place: curl`}},
		"a development build, linked": {func(t *testing.T, deps *Deps) { linked(t, bin(deps), "dev") },
			want{kind: binLinked, code: exitDecide, line: `which says it is "dev", a development build; the command replaces no link. The install script puts 0.2.0 in place of the link, which is then a file (`}},
		"a release, linked, current": {func(t *testing.T, deps *Deps) { linked(t, bin(deps), "0.2.0") },
			want{kind: binLinked, code: exitOK, idle: true}},
		"a release, linked, past it": {func(t *testing.T, deps *Deps) { linked(t, bin(deps), "0.3.0") },
			want{kind: binLinked, code: exitOK, idle: true}},
		"a release, linked, behind": {func(t *testing.T, deps *Deps) { linked(t, bin(deps), "0.1.0") },
			want{kind: binLinked, code: exitDecide, line: "is 0.1.0; ", never: "install.sh"}},
		"one that does not run": {func(t *testing.T, deps *Deps) { _ = os.WriteFile(bin(deps), []byte("not a program"), 0o755) },
			want{kind: binSilent, code: exitDecide, line: "is no answer; "}},
		// A path that cannot be read is not checked, and said so: no step
		// of the person's, no exit 4 (the sixth review of #228).
		"a directory this user cannot search": {func(t *testing.T, deps *Deps) {
			if os.Geteuid() == 0 {
				t.Skip("root searches any directory")
			}
			scriptBinary(t, bin(deps), "0.1.0")
			_ = os.Chmod(deps.InstallDir, 0o600)
			t.Cleanup(func() { _ = os.Chmod(deps.InstallDir, 0o700) })
		}, want{kind: binUnread, code: exitOK, note: "was not checked: "}},
		"not a file": {func(t *testing.T, deps *Deps) { _ = os.Mkdir(bin(deps), 0o700) },
			want{kind: binOdd, code: exitDecide, line: "is not a regular file"}},
		"a release where this user cannot write, behind": {func(t *testing.T, deps *Deps) { readOnly(t, deps, "0.1.0") },
			want{kind: binUnwritable, code: exitDecide, line: "is not writable by this user"}},
		"a release where this user cannot write, current": {func(t *testing.T, deps *Deps) { readOnly(t, deps, "0.2.0") },
			want{kind: binUnwritable, code: exitOK, idle: true}},
		"Homebrew's, behind": {func(t *testing.T, deps *Deps) { scriptBinary(t, cellar(t, deps), "0.1.0") },
			want{kind: binPackaged, code: exitDecide, line: "is Homebrew's: brew upgrade tracepad", never: "install.sh"}},
		"Homebrew's, current": {func(t *testing.T, deps *Deps) { scriptBinary(t, cellar(t, deps), "0.2.0") },
			want{kind: binPackaged, code: exitOK, idle: true}},
		"Homebrew's link to a build of its HEAD": {func(t *testing.T, deps *Deps) {
			path := cellar(t, deps)
			target := filepath.Join(filepath.Dir(filepath.Dir(path)), "libexec", "tracepad")
			_ = os.MkdirAll(filepath.Dir(target), 0o755)
			scriptBinary(t, target, "HEAD-abc1234")
			if err := os.Symlink("../libexec/tracepad", path); err != nil {
				t.Fatal(err)
			}
		}, want{kind: binPackaged, code: exitDecide, line: "is Homebrew's: brew upgrade tracepad", never: "install.sh"}},
		"current, and one behind first on PATH": {func(t *testing.T, deps *Deps) {
			linked(t, bin(deps), "0.2.0")
			first := filepath.Join(t.TempDir(), "tracepad")
			scriptBinary(t, first, "0.1.0")
			deps.LookPath = func(string) string { return first }
		}, want{kind: binLinked, code: exitOK, line: ", first on PATH, is 0.1.0: "}},
	}
	for kind := binNone; kind <= binOurs; kind++ {
		if !slices.ContainsFunc(slices.Collect(maps.Values(cases)), func(c cell) bool { return c.want.kind == kind }) {
			t.Errorf("no cell for the kind %d", kind)
		}
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			deps := containerDeps(t, newFakeDocker(t))
			c.setup(t, &deps)
			if got := (&runner{deps: deps}).installedBinary(context.Background()).Kind; got != c.want.kind {
				t.Errorf("kind %d, want %d", got, c.want.kind)
			}
			rep, code := runReport(t, deps, "--plan")
			person := strings.Join(rep.Person, "\n")
			if code != c.want.code || rep.Binary.Idle != c.want.idle || (c.want.line == "") != (person == "") || !strings.Contains(person, c.want.line) ||
				(c.want.never != "" && strings.Contains(person, c.want.never)) || !strings.Contains(strings.Join(rep.Notes, "\n"), c.want.note) {
				t.Errorf("exit %d, idle %v, person %q; want %+v (%s)", code, rep.Binary.Idle, person, c.want, rep.Summary)
			}
		})
	}
}

// unreadableProcesses is a machine whose processes cannot be listed.
type unreadableProcesses struct{ noProcesses }

func (unreadableProcesses) Candidates(context.Context) ([]Process, int, error) {
	return nil, 0, errors.New("the listing is not allowed")
}

// linked puts a stand-in that says version elsewhere and links it at bin,
// as a build linked from its checkout is.
func linked(t *testing.T, bin, version string) {
	t.Helper()
	target := filepath.Join(t.TempDir(), "tracepad")
	scriptBinary(t, target, version)
	if err := os.Symlink(target, bin); err != nil {
		t.Fatal(err)
	}
}

// A development build at the install path is its builder's: the plan and the
// upgrade leave it, and give the install script's line that puts the
// release in its place (the live run of 0.1.0). It is no "nothing to do".
func TestADevelopmentBuildIsGivenItsReplacement(t *testing.T) {
	t.Parallel()
	d := newFakeDocker(t)
	deps := containerDeps(t, d, "97d6b79")
	bin := filepath.Join(deps.InstallDir, "tracepad")
	for _, mode := range [][]string{{"--plan"}, {}} {
		rep, _ := runReport(t, deps, mode...)
		want := fmt.Sprintf("%s says it is \"97d6b79\", a development build, which the command does not replace; to put 0.2.0 in its place: curl -fsSL https://tracepad.github.io/tracepad/install.sh | TRACEPAD_VERSION=0.2.0 sh", bin)
		if !slices.Contains(rep.Person, want) || rep.Binary == nil || rep.Binary.Idle {
			t.Errorf("%q: %q %+v", mode, rep.Person, rep.Binary)
		}
		if v, _ := scriptVersion(context.Background(), bin); v != "97d6b79" {
			t.Errorf("%q: the build was replaced: %s", mode, v)
		}
	}
}

// A link given relatively names its target from the link's directory, not
// the reader's (the fourth review of #228).
func TestARelativeLinkIsNamedWhole(t *testing.T) {
	t.Parallel()
	deps := containerDeps(t, newFakeDocker(t))
	bin := filepath.Join(deps.InstallDir, "tracepad")
	checkout := filepath.Join(filepath.Dir(deps.InstallDir), "checkout")
	if err := os.MkdirAll(checkout, 0o700); err != nil {
		t.Fatal(err)
	}
	scriptBinary(t, filepath.Join(checkout, "tracepad"), "dev")
	if err := os.Symlink("../checkout/tracepad", bin); err != nil {
		t.Fatal(err)
	}
	rep, _ := runReport(t, deps, "--plan")
	want := fmt.Sprintf("%s is a link to %s, which says", bin, filepath.Join(checkout, "tracepad"))
	if person := strings.Join(rep.Person, "\n"); !strings.Contains(person, want) {
		t.Errorf("%s", person)
	}
}

// Compose's path labels are comma-joined absolute paths: a part that is not
// absolute after another is that one's rest (the sixth review of #228), and
// a first one that is not is the project directory's.
func TestComposePathsKeepACommaInAPath(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		label string
		want  []string
	}{
		{"", nil},
		{"/srv/obs/compose.yml", []string{"/srv/obs/compose.yml"}},
		{"/srv/obs/a.yml,/srv/obs/b.yml", []string{"/srv/obs/a.yml", "/srv/obs/b.yml"}},
		{"/srv/a,b/compose.yaml", []string{"/srv/a,b/compose.yaml"}},
		{"/srv/a,b/one.yml,/srv/a,b/two.yml", []string{"/srv/a,b/one.yml", "/srv/a,b/two.yml"}},
		{"prod.env", []string{"/srv/obs/prod.env"}},
	} {
		if got := composePaths(c.label, "/srv/obs"); !slices.Equal(got, c.want) {
			t.Errorf("%q: %q, want %q", c.label, got, c.want)
		}
	}
}
