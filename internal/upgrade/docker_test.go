//go:build unix

package upgrade

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
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

// fakeDocker is the docker CLI over containers and volumes in memory. It
// records every call, and a test fails if any of them removes something.
type fakeDocker struct {
	t       *testing.T
	calls   [][]string
	byName  map[string]*inspectContainer
	images  map[string]inspectImage
	volumes map[string]int64 // a volume and the traces it holds
	// archives holds what each archive it wrote was taken from.
	archives map[string]int64
	nextID   int
	// failRun makes `docker run -d` create its container and fail to start
	// it, as a busy port does; brokenRef is an image whose server exits.
	failRun   bool
	brokenRef string
	// silentRef is an image whose server, on a volume a way back restored,
	// runs and never answers;
	// failRestore makes the way back's busybox restore fail.
	silentRef   string
	failRestore bool
	rootless    bool
	// stubborn is a container that does not stop on SIGTERM; slow, how long
	// every container takes to answer while set.
	stubborn bool
	slow     time.Duration
	// onCall runs before each call: a test's interrupt.
	onCall func(args []string)
	dbFile []byte
}

func newFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()
	return &fakeDocker{t: t, byName: map[string]*inspectContainer{}, images: map[string]inspectImage{},
		volumes: map[string]int64{}, archives: map[string]int64{}, dbFile: sqliteFile(t)}
}

// sqliteFile is a real, empty SQLite database, for the way back's check.
func sqliteFile(t *testing.T) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "x.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE traces (id TEXT)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (d *fakeDocker) image(ref, version string) {
	img := inspectImage{ID: "sha256:" + strings.Repeat(version[len(version)-1:], 64), Config: inspectConfig{
		Env:         []string{"PATH=/usr/bin", "TRACEPAD_DATA_DIR=/data", "TRACEPAD_LISTEN=:4318", "TRACEPAD_IN_CONTAINER=1"},
		Entrypoint:  []string{"/tracepad"},
		User:        "nonroot:nonroot",
		WorkingDir:  "/home/nonroot",
		Labels:      map[string]string{"org.opencontainers.image.version": version, "org.opencontainers.image.title": "Tracepad"},
		Healthcheck: json.RawMessage(`{"Test":["CMD","/tracepad","health"]}`),
	}}
	d.images[ref] = img
	d.images[img.ID] = img
}

func (d *fakeDocker) find(ref string) *inspectContainer {
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

func mountValue(m string) map[string]string {
	fields, _ := csv.NewReader(strings.NewReader(m)).Read()
	out := map[string]string{}
	for _, f := range fields {
		k, v, _ := strings.Cut(f, "=")
		out[k] = v
	}
	return out
}

func (d *fakeDocker) Run(ctx context.Context, args ...string) ([]byte, error) {
	d.calls = append(d.calls, slices.Clone(args))
	// As exec.CommandContext does: a cancelled context runs nothing.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.onCall != nil {
		d.onCall(args)
	}
	joined := strings.Join(args, " ")
	for _, removal := range []string{"rm ", "rmi ", "volume rm", "prune", "container rm", "image rm", "-delete"} {
		if strings.HasPrefix(joined, removal) || strings.Contains(joined, " "+removal) {
			d.t.Errorf("a removal: docker %s", joined)
		}
	}
	switch {
	case args[0] == "info":
		if d.rootless {
			return []byte(`["name=seccomp,profile=builtin","name=rootless","name=cgroupns"]`), nil
		}
		return []byte(`["name=seccomp,profile=builtin","name=cgroupns"]`), nil
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
		var list []inspectContainer
		for _, ref := range args[2:] {
			c := d.find(ref)
			if c == nil {
				return nil, errors.New("No such container: " + ref)
			}
			list = append(list, *c)
		}
		return json.Marshal(list)
	case args[0] == "image" && args[1] == "inspect":
		img, ok := d.images[args[2]]
		if !ok {
			return nil, errors.New("No such image: " + args[2])
		}
		return json.Marshal([]inspectImage{img})
	case args[0] == "volume" && args[1] == "inspect":
		if _, ok := d.volumes[args[2]]; !ok {
			return nil, errors.New("No such volume")
		}
		return []byte("[{}]"), nil
	case args[0] == "volume" && args[1] == "create":
		if _, ok := d.volumes[args[2]]; !ok {
			d.volumes[args[2]] = 0
		}
		return []byte(args[2]), nil
	case args[0] == "pull":
		if _, ok := d.images[args[len(args)-1]]; !ok {
			return nil, errors.New("manifest unknown")
		}
		return nil, nil
	case args[0] == "stop":
		d.t.Errorf("docker stop kills after its timeout: %s", joined)
		return nil, errors.New("not here")
	case args[0] == "kill":
		c := d.find(args[len(args)-1])
		if c == nil {
			return nil, errors.New("No such container")
		}
		if args[1] != "--signal" || args[2] != "TERM" {
			d.t.Errorf("a kill that is not SIGTERM: %s", joined)
		}
		if d.stubborn {
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
			return nil, errors.New("No such container")
		}
		d.boot(c)
		return nil, nil
	case args[0] == "rename":
		c := d.find(args[1])
		if c == nil || d.byName[args[2]] != nil {
			return nil, errors.New("rename refused")
		}
		delete(d.byName, strings.TrimPrefix(c.Name, "/"))
		c.Name = "/" + args[2]
		d.byName[args[2]] = c
		return nil, nil
	case args[0] == "update":
		c := d.find(args[3])
		if c == nil {
			return nil, errors.New("No such container")
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
		return []byte(`time=x level=INFO msg="tracepad ` + d.images[c.Config.Image].Config.Labels["org.opencontainers.image.version"] + `"` + "\n"), nil
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
	case slices.Contains(args, "du"):
		return []byte("2048\t/data\n"), nil
	case strings.Contains(script, "tar czf"):
		path := filepath.Join(mounts["/backup"]["src"], "data.tar.gz")
		body := archiveOf(d.t, map[string][]byte{"./tracepad.db": d.dbFile})
		if err := os.WriteFile(path, body, 0o600); err != nil {
			return nil, err
		}
		d.archives[path] = d.volumes[mounts["/data"]["src"]]
		return nil, nil
	case strings.Contains(script, "tar xzf"):
		if d.failRestore {
			return nil, errors.New("tar: write error: No space left on device")
		}
		path := filepath.Join(mounts["/backup"]["src"], "data.tar.gz")
		d.volumes[mounts["/data"]["src"]] = d.archives[path]
		return nil, nil
	}
	return nil, errors.New("the fake does not know this busybox")
}

// create is `docker run -d`: a container from the arguments as docker reads
// them, the env file included.
func (d *fakeDocker) create(args []string) ([]byte, error) {
	d.nextID++
	c := &inspectContainer{ID: fmt.Sprintf("%064d", d.nextID)}
	c.HostConfig.PortBindings = map[string][]portBinding{}
	c.HostConfig.NetworkMode = "bridge"
	c.Config.Labels = map[string]string{}
	i := 2
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i += 2 {
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
				mt.Name = m["src"]
			} else {
				mt.Source = m["src"]
			}
			c.Mounts = append(c.Mounts, mt)
		case "-p":
			last := strings.LastIndex(v, ":")
			hostPart, port := v[:last], v[last+1:]
			hl := strings.LastIndex(hostPart, ":")
			ip := strings.Trim(hostPart[:hl], "[]")
			c.HostConfig.PortBindings[port] = append(c.HostConfig.PortBindings[port], portBinding{HostIP: ip, HostPort: hostPart[hl+1:]})
		case "--label":
			k, val, _ := strings.Cut(v, "=")
			c.Config.Labels[k] = val
		case "--log-driver":
			c.HostConfig.LogConfig.Type = v
		case "--user":
			c.Config.User = v
		case "--env-file":
			b, err := os.ReadFile(v)
			if err != nil {
				return nil, err
			}
			for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				if line != "" {
					c.Config.Env = append(c.Config.Env, line)
				}
			}
		}
	}
	ref := args[i]
	img, ok := d.images[ref]
	if !ok {
		return nil, errors.New("Unable to find image " + ref)
	}
	c.Image = img.ID
	c.Config.Image = ref
	c.Config.Cmd = args[i+1:]
	c.Config.Env = append(slices.Clone(img.Config.Env), c.Config.Env...)
	c.Config.Entrypoint, c.Config.WorkingDir, c.Config.Healthcheck = img.Config.Entrypoint, img.Config.WorkingDir, img.Config.Healthcheck
	if c.Config.User == "" {
		c.Config.User = img.Config.User
	}
	for k, v := range img.Config.Labels {
		if _, ok := c.Config.Labels[k]; !ok {
			c.Config.Labels[k] = v
		}
	}
	name := strings.TrimPrefix(c.Name, "/")
	if d.byName[name] != nil {
		return nil, errors.New("Conflict. The container name is already in use")
	}
	d.byName[name] = c
	if d.failRun {
		return nil, errors.New("Bind for 127.0.0.1:4318 failed: port is already allocated")
	}
	d.boot(c)
	return []byte(c.ID + "\n"), nil
}

// boot starts a container as docker does: a broken image exits at once, and
// under a restart policy docker starts it again and again, keeping it
// Running with Restarting set (the review of #1).
func (d *fakeDocker) boot(c *inspectContainer) {
	if c.Config.Image != d.brokenRef {
		c.State.Running, c.State.Restarting = true, false
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
	if d.slow > 0 {
		select {
		case <-time.After(d.slow):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	for _, c := range d.byName {
		if !c.State.Running || c.State.Restarting || c.Config.Image == d.silentRef && onRestoredVolume(c) {
			continue
		}
		for _, b := range c.HostConfig.PortBindings["4318/tcp"] {
			if req.URL.Host != b.HostIP+":"+b.HostPort {
				continue
			}
			body := ""
			switch req.URL.Path {
			case "/health":
				body = fmt.Sprintf(`{"status":"ok","version":%q}`, d.images[c.Config.Image].Config.Labels["org.opencontainers.image.version"])
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
	return nil, errors.New("connection refused")
}

// onRestoredVolume: a way back's container, on a volume it restored.
func onRestoredVolume(c *inspectContainer) bool {
	for _, m := range c.Mounts {
		if m.Destination == "/data" && strings.HasPrefix(m.Name, "tracepad-app-") {
			return true
		}
	}
	return false
}

// setupContainer is the container setup.md starts, made as docker makes it,
// with a restart policy and a read-only bind of the person's on top.
func (d *fakeDocker) setupContainer(t *testing.T, bind string) {
	t.Helper()
	d.image("ghcr.io/tracepad/tracepad:0.1.0", "0.1.0")
	d.image("ghcr.io/tracepad/tracepad:0.2.0", "0.2.0")
	d.image("ghcr.io/tracepad/tracepad:0.2.1", "0.2.1")
	d.image(busybox, "1")
	d.volumes["tracepad-app"] = 7
	env := filepath.Join(t.TempDir(), "env")
	_ = os.WriteFile(env, []byte("TRACEPAD_URL=http://localhost:4318\nTRACEPAD_PROJECTS=app:tp-pk-a:tp-sk-b\n"), 0o600)
	if _, err := d.Run(context.Background(), "run", "-d", "--name", "tracepad-app", "--restart", "always",
		"--mount", "type=volume,src=tracepad-app,dst=/data",
		"--mount", csvField("type=bind", "src="+bind, "dst=/tls", "readonly"),
		"-p", "127.0.0.1:4318:4318/tcp", "--env-file", env, "ghcr.io/tracepad/tracepad:0.1.0", "serve"); err != nil {
		t.Fatal(err)
	}
	d.calls = nil
}

func containerDeps(t *testing.T, d *fakeDocker) Deps {
	t.Helper()
	home := t.TempDir()
	mirror := t.TempDir()
	for _, v := range []string{"0.1.0", "0.2.0", "0.2.1"} {
		mirrorRelease(t, mirror, v, v, v == "0.2.0")
	}
	return Deps{
		Sys:        noProcesses{},
		Docker:     d,
		HTTP:       &http.Client{Transport: d},
		Releases:   testReleases(t, mirror),
		InstallDir: filepath.Join(home, ".local", "bin"),
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
		LookPath:   func(string) string { return "" },
		Version:    binaryVersion,
		Skills:     func(context.Context, string, string, ...string) (string, error) { return "", nil },
		Now:        time.Now,
		Sleep:      func(context.Context, time.Duration) error { return nil },
		StopWait:   time.Second,
		HealthWait: 0,
		ProbeWait:  time.Millisecond,
	}
}

// noProcesses is a machine with no server process.
type noProcesses struct{}

func (noProcesses) Candidates() ([]Process, int, error) { return nil, 0, nil }
func (noProcesses) Inspect(int) (Process, error)        { return Process{}, errors.New("none") }
func (noProcesses) Alive(int) bool                      { return false }
func (noProcesses) Signal(int, syscall.Signal) error    { return errors.New("none") }
func (noProcesses) Start(StartSpec) (Started, error)    { return nil, errors.New("none") }

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

func TestTheRecreateKeepsWhatTheContainerHad(t *testing.T) {
	img := inspectImage{Config: inspectConfig{
		Env:    []string{"PATH=/bin", "TRACEPAD_LISTEN=:4318"},
		User:   "nonroot:nonroot",
		Labels: map[string]string{"org.opencontainers.image.version": "0.1.0"},
	}}
	var ic inspectContainer
	ic.Config = inspectConfig{
		Env:    []string{"PATH=/bin", "TRACEPAD_LISTEN=:4318", "TRACEPAD_PROJECTS=a:b:c"},
		Cmd:    []string{"serve"},
		User:   "nonroot:nonroot",
		Labels: map[string]string{"org.opencontainers.image.version": "0.1.0", "team": "obs"},
	}
	ic.HostConfig.RestartPolicy.Name = "on-failure"
	ic.HostConfig.RestartPolicy.MaximumRetryCount = 3
	ic.HostConfig.PortBindings = map[string][]portBinding{"4318/tcp": {{HostIP: "::1", HostPort: "4318"}}, "4317/tcp": {{HostIP: "127.0.0.1", HostPort: "4317"}}}
	ic.HostConfig.LogConfig.Type = "json-file"
	ic.Mounts = []mount{
		{Type: "volume", Name: "tp", Destination: "/data", RW: true},
		{Type: "bind", Source: "/Users/me/My Data, certs", Destination: "/tls", RW: false},
	}
	got := runArgs(ic, img, "tracepad-app", "ghcr.io/tracepad/tracepad:0.2.0", "/run/env", "")
	want := []string{"run", "-d", "--name", "tracepad-app", "--restart", "on-failure:3",
		"--mount", "type=volume,src=tp,dst=/data",
		"--mount", `type=bind,"src=/Users/me/My Data, certs",dst=/tls,readonly`,
		"-p", "127.0.0.1:4317:4317/tcp", "-p", "[::1]:4318:4318/tcp",
		"--label", "team=obs", "--log-driver", "json-file",
		"--env-file", "/run/env", "ghcr.io/tracepad/tracepad:0.2.0", "serve"}
	if !slices.Equal(got, want) {
		t.Errorf("run args\n got %q\nwant %q", got, want)
	}
	if env := personEnv(ic.Config.Env, img.Config.Env); !slices.Equal(env, []string{"TRACEPAD_PROJECTS=a:b:c"}) {
		t.Errorf("the person's environment: %q", env)
	}
	ic.HostConfig.RestartPolicy.Name = ""
	if got := runArgs(ic, img, "n", "r", "e", "tp-run")[5]; got != "no" {
		t.Errorf("an empty policy is %q", got)
	}
	if got := runArgs(ic, img, "n", "r", "e", "tp-run")[7]; got != "type=volume,src=tp-run,dst=/data" {
		t.Errorf("the volume swap: %q", got)
	}
}

func TestWhatContainerIsTheCommands(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	base := *d.byName["tracepad-app"]
	img := d.images["ghcr.io/tracepad/tracepad:0.1.0"]
	if c := classifyContainer(base, img, ""); !c.Ours || c.Volume != "tracepad-app" || c.URL != "http://127.0.0.1:4318" {
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
		{"two bindings", func(c *inspectContainer) {
			c.HostConfig.PortBindings = map[string][]portBinding{"4318/tcp": {{HostIP: "127.0.0.1", HostPort: "4318"}, {HostIP: "::1", HostPort: "4318"}}}
		}, "2 times"},
		{"a bind at /data", func(c *inspectContainer) {
			c.Mounts = []mount{{Type: "bind", Source: "/srv", Destination: "/data", RW: true}}
		}, "not a volume"},
		{"--rm", func(c *inspectContainer) { c.HostConfig.AutoRemove = true }, "--rm"},
		{"privileged", func(c *inspectContainer) { c.HostConfig.Privileged = true }, "privileged"},
		{"a network", func(c *inspectContainer) { c.HostConfig.NetworkMode = "obs" }, "network obs"},
		{"an entrypoint", func(c *inspectContainer) { c.Config.Entrypoint = []string{"/bin/sh"} }, "entrypoint"},
		{"limits", func(c *inspectContainer) { c.HostConfig.Memory = 1 << 30 }, "resource limits"},
		{"a line break", func(c *inspectContainer) { c.Config.Env = append(c.Config.Env, "TRACEPAD_X=a\nb") }, "line break"},
		{"stopped", func(c *inspectContainer) { c.State.Running = false }, "not running"},
		{"a setting a recreate drops", func(c *inspectContainer) {
			c.raw = map[string]json.RawMessage{"HostConfig": json.RawMessage(`{"PidsLimit":100}`)}
		}, "does not reproduce (HostConfig.PidsLimit)"},
	} {
		c := base
		c.Config.Env = slices.Clone(base.Config.Env)
		tc.change(&c)
		got := classifyContainer(c, img, "")
		if got.Ours || !strings.Contains(got.Reason, tc.reason) {
			t.Errorf("%s: %+v", tc.name, got.Reason)
		}
	}
	named := base
	named.Name = "/tp"
	if c := classifyContainer(named, img, "tp"); !c.Ours {
		t.Errorf("a container named with --container: %s", c.Reason)
	}
}

func TestAContainerUpgradeKeepsMountsPolicyAndVariables(t *testing.T) {
	d := newFakeDocker(t)
	bind := filepath.Join(t.TempDir(), "My Data")
	d.setupContainer(t, bind)
	deps := containerDeps(t, d)

	plan, code := runReport(t, deps, "--plan")
	if code != exitPending || plan.To != "0.2.0" || len(plan.Containers) != 1 || !plan.Containers[0].Target {
		t.Fatalf("plan: %d %+v", code, plan)
	}
	if len(plan.Next) == 0 || !strings.HasPrefix(plan.Next[0], deps.Self+" upgrade") {
		t.Errorf("the plan hands on a bare command: %q", plan.Next)
	}
	if len(d.calls) == 0 || slices.ContainsFunc(d.calls, func(c []string) bool { return c[0] == "kill" || c[0] == "run" }) {
		t.Fatalf("the plan acted: %q", d.calls)
	}

	rep, code := runReport(t, deps)
	if code != exitOK || rep.Check == nil || rep.Check.Verdict != verdictHealthy {
		t.Fatalf("upgrade: %d %+v", code, rep)
	}
	newC := d.byName["tracepad-app"]
	if newC.Config.Image != "ghcr.io/tracepad/tracepad:0.2.0" || newC.HostConfig.RestartPolicy.Name != "always" {
		t.Errorf("new container: %+v", newC)
	}
	if !slices.Contains(newC.Config.Env, "TRACEPAD_PROJECTS=app:tp-pk-a:tp-sk-b") || strings.Count(strings.Join(newC.Config.Env, "\n"), "TRACEPAD_LISTEN=") != 1 {
		t.Errorf("environment: %q", newC.Config.Env)
	}
	if i := slices.IndexFunc(newC.Mounts, func(m mount) bool { return m.Destination == "/tls" }); i < 0 || newC.Mounts[i].RW || newC.Mounts[i].Source != bind {
		t.Errorf("the read-only bind: %+v", newC.Mounts)
	}
	if want := filepath.Join(rep.Run.Dir, "upgrader") + " upgrade --back " + rep.Run.ID; !strings.Contains(strings.Join(rep.Next, "\n"), want) {
		t.Errorf("the way back is not handed on by this binary's copy: %q", rep.Next)
	}
	before := d.byName["tracepad-app-before-"+rep.Run.ID]
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

	// The way back after a healthy upgrade: a new volume from the archive,
	// the new container set aside, the old image on the new volume.
	d.volumes["tracepad-app"] = 9
	back, code := runReport(t, deps, "--back", rep.Run.ID)
	if code != exitOK {
		t.Fatalf("back: %d %+v", code, back)
	}
	cur := d.byName["tracepad-app"]
	vol := "tracepad-app-" + rep.Run.ID
	if cur.Config.Image != "ghcr.io/tracepad/tracepad:0.1.0" || !slices.ContainsFunc(cur.Mounts, func(m mount) bool { return m.Name == vol && m.Destination == "/data" }) {
		t.Errorf("after the way back: %+v", cur)
	}
	if d.volumes[vol] != 7 || d.volumes["tracepad-app"] != 9 {
		t.Errorf("volumes: %v", d.volumes)
	}
	after := d.byName["tracepad-app-after-"+rep.Run.ID]
	if after == nil || after.HostConfig.RestartPolicy.Name != "no" {
		t.Errorf("the new container set aside: %+v", after)
	}
	// Run again, it ends where the first did and changes nothing (#31).
	calls := len(d.calls)
	if again, code := runReport(t, deps, "--back", rep.Run.ID); code != exitOK {
		t.Errorf("second way back: %d %s", code, again.Summary)
	}
	for _, c := range d.calls[calls:] {
		if c[0] != "container" && c[0] != "image" && c[0] != "logs" {
			t.Errorf("the second way back acted: docker %q", c)
		}
	}
}

func TestABrokenImageGoesBackIntoANewVolume(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	d.brokenRef = "ghcr.io/tracepad/tracepad:0.2.1"
	deps := containerDeps(t, d)
	rep, code := runReport(t, deps, "--to", "0.2.1")
	if code != exitWentBack {
		t.Fatalf("%d %+v", code, rep)
	}
	cur := d.byName["tracepad-app"]
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

func TestAFailedRunPutsTheOldContainerBack(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	d.failRun = true
	deps := containerDeps(t, d)
	rep, code := runReport(t, deps)
	if code != exitWentBack {
		t.Fatalf("%d %+v", code, rep)
	}
	cur := d.byName["tracepad-app"]
	if !cur.State.Running || cur.Config.Image != "ghcr.io/tracepad/tracepad:0.1.0" || cur.HostConfig.RestartPolicy.Name != "always" {
		t.Errorf("the old container: %+v", cur)
	}
	failed := d.byName["tracepad-app-failed-"+rep.Run.ID]
	if failed == nil || failed.HostConfig.RestartPolicy.Name != "no" {
		t.Errorf("the half-made container: %+v", failed)
	}
}

func TestAWayBackWhoseVolumeExistsTouchesNothing(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d)
	rep, code := runReport(t, deps)
	if code != exitOK {
		t.Fatalf("%d %+v", code, rep)
	}
	d.volumes["tracepad-app-"+rep.Run.ID] = 1
	d.calls = nil
	back, code := runReport(t, deps, "--back", rep.Run.ID)
	if code != exitStuck || !strings.Contains(back.Summary, "exists already") {
		t.Fatalf("%d %s", code, back.Summary)
	}
	if slices.ContainsFunc(d.calls, func(c []string) bool { return c[0] == "kill" || c[0] == "rename" || c[0] == "run" }) {
		t.Errorf("it acted: %q", d.calls)
	}
}

func TestAMissingImageStopsNothing(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d)
	delete(d.images, "ghcr.io/tracepad/tracepad:0.2.0")
	rep, code := runReport(t, deps)
	if code != exitRefused || !strings.Contains(rep.Summary, "could not pull") {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	if !d.byName["tracepad-app"].State.Running {
		t.Error("the container was stopped")
	}
}

func TestAWayBackLeavesALaterRunsContainerAlone(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d)
	rep, code := runReport(t, deps)
	if code != exitOK {
		t.Fatalf("%d %+v", code, rep)
	}
	d.byName["tracepad-app"].ID = strings.Repeat("9", 64)
	d.calls = nil
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
func TestAWayBackThatIsNotConfirmedSaysSo(t *testing.T) {
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
func TestAFailedRestoreNamesWhatItLeft(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d)
	rep, code := runReport(t, deps)
	if code != exitOK {
		t.Fatalf("%d %+v", code, rep)
	}
	d.failRestore = true
	vol := "tracepad-app-" + rep.Run.ID
	back, code := runReport(t, deps, "--back", rep.Run.ID)
	if code != exitStuck || !strings.Contains(back.Summary, "docker volume rm "+vol) {
		t.Fatalf("%d %s", code, back.Summary)
	}
	all := strings.Join(append(back.Person, back.SetAside...), "\n")
	if strings.Contains(all, "docker volume rm tracepad-app;") || strings.Contains(all, "docker volume rm tracepad-app\n") || strings.HasSuffix(all, "docker volume rm tracepad-app") || slices.Contains(back.SetAside, "volume tracepad-app") {
		t.Errorf("the live volume is offered for removal: %q", all)
	}
	if !d.byName["tracepad-app"].State.Running {
		t.Error("the new container was stopped")
	}
	// Nothing that runs was changed, so the run can still be checked.
	if chk, code := runReport(t, deps, "--check", rep.Run.ID); code != exitOK {
		t.Errorf("--check after a refused way back: %d %s", code, chk.Summary)
	}
	again, code := runReport(t, deps, "--back", rep.Run.ID)
	if code != exitStuck || !strings.Contains(again.Summary, "docker volume rm "+vol) {
		t.Errorf("second way back: %d %s", code, again.Summary)
	}
}

func TestTheRemovalsRemoveContainersBeforeVolumes(t *testing.T) {
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

// An interrupt after docker stop must not leave the container down: the
// rest of the run, and its way back, finish on a context of their own (the
// second review).
func TestAnInterruptAfterTheContainerStopsStillBringsItBack(t *testing.T) {
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
	cur := d.byName["tracepad-app"]
	if cur == nil || !cur.State.Running || cur.HostConfig.RestartPolicy.Name != "always" || cur.Config.Image != "ghcr.io/tracepad/tracepad:0.1.0" {
		t.Errorf("the old container: %+v", cur)
	}
}

// The container's archive is pinned by its bytes: one changed since is not
// restored.
func TestAContainerArchiveChangedSinceIsNotRestored(t *testing.T) {
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
func TestAFailedRunInTheWayBackDoesNotBlockTheNext(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d)
	rep, code := runReport(t, deps)
	if code != exitOK {
		t.Fatalf("%d %+v", code, rep)
	}
	d.failRun = true
	first, code := runReport(t, deps, "--back", rep.Run.ID)
	if code != exitStuck {
		t.Fatalf("first: %d %s", code, first.Summary)
	}
	d.failRun = false
	second, code := runReport(t, deps, "--back", rep.Run.ID)
	if code != exitOK {
		t.Fatalf("second: %d %s", code, second.Summary)
	}
	left := d.byName["tracepad-app-failed-"+rep.Run.ID]
	if left == nil || left.HostConfig.RestartPolicy.Name != "no" {
		t.Errorf("the container the failed run left: %+v", left)
	}
	if cur := d.byName["tracepad-app"]; cur == nil || !cur.State.Running || cur.Config.Image != "ghcr.io/tracepad/tracepad:0.1.0" {
		t.Errorf("the old image does not run: %+v", cur)
	}
}

// The person removed the new container after an upgrade: the way back has
// nothing to set aside, and goes on.
func TestAWayBackAfterTheNewContainerWasRemoved(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	deps := containerDeps(t, d)
	rep, code := runReport(t, deps)
	if code != exitOK {
		t.Fatalf("%d %+v", code, rep)
	}
	delete(d.byName, "tracepad-app")
	back, code := runReport(t, deps, "--back", rep.Run.ID)
	if code != exitOK {
		t.Fatalf("%d %s", code, back.Summary)
	}
	if cur := d.byName["tracepad-app"]; cur == nil || !cur.State.Running || cur.Config.Image != "ghcr.io/tracepad/tracepad:0.1.0" {
		t.Errorf("the old image does not run: %+v", cur)
	}
}

func TestRootlessDockerIsThePersons(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	d.rootless = true
	deps := containerDeps(t, d)
	plan, code := runReport(t, deps, "--plan")
	if code != exitDecide || len(plan.Containers) != 1 || plan.Containers[0].Whose != "person" || !strings.Contains(plan.Containers[0].Reason, "rootless") {
		t.Fatalf("%d %+v", code, plan.Containers)
	}
}

// What a recreate does not carry is named, and makes the container the
// person's; what the daemon sets by itself is not counted.
func TestSettingsARecreateWouldDropAreNamed(t *testing.T) {
	img := inspectImage{Config: inspectConfig{ExposedPorts: map[string]struct{}{"4318/tcp": {}}}}
	ic := inspectContainer{ID: "0123456789abcdef"}
	ic.Config.HostnameV = "0123456789ab"
	ic.Config.ExposedPorts = map[string]struct{}{"4318/tcp": {}}
	ic.HostConfig.PortBindings = map[string][]portBinding{"4318/tcp": {{HostIP: "127.0.0.1", HostPort: "4318"}}}
	defaults := `{"HostConfig":{"ShmSize":67108864,"Runtime":"runc","CgroupnsMode":"private","MaskedPaths":["/proc/kcore"],"ConsoleSize":[0,0],
		"PidsLimit":null,"MemorySwappiness":null,"OomScoreAdj":0,"CpusetCpus":"","BlkioWeight":0,"RestartPolicy":{"Name":"always"}},
		"Config":{"Hostname":"0123456789ab","AttachStdout":true,"Tty":false,"Env":["A=b"]}}`
	_ = json.Unmarshal([]byte(defaults), &ic.raw)
	if got := unreproduced(ic, img); len(got) != 0 {
		t.Errorf("defaults counted: %q", got)
	}
	set := `{"HostConfig":{"PidsLimit":100,"CpusetCpus":"0-1","MemorySwap":1073741824,"Runtime":"runsc","OomScoreAdj":500,"ShmSize":134217728},
		"Config":{"Hostname":"mine","Tty":true,"StopTimeout":30}}`
	_ = json.Unmarshal([]byte(set), &ic.raw)
	ic.Config.HostnameV = "mine"
	ic.Config.ExposedPorts["9000/tcp"] = struct{}{}
	want := []string{"Config.ExposedPorts[9000/tcp]", "Config.Hostname", "Config.StopTimeout", "Config.Tty",
		"HostConfig.CpusetCpus", "HostConfig.MemorySwap", "HostConfig.OomScoreAdj", "HostConfig.PidsLimit", "HostConfig.Runtime", "HostConfig.ShmSize"}
	if got := unreproduced(ic, img); !slices.Equal(got, want) {
		t.Errorf("named %q,\nwant %q", got, want)
	}
}

// A deadline the caller set that runs out during the swap is not the way
// back's: it has a budget of its own (the third review).
func TestADeadlineSpentByTheSwapIsNotTheWayBacks(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	d.failRun = true
	deps := containerDeps(t, d)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
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
	if cur := d.byName["tracepad-app"]; cur == nil || !cur.State.Running {
		t.Errorf("the old container: %+v", cur)
	}
}

// Interrupted after the stop, a healthy swap goes on to the end: the upgrade
// is finished, not undone.
func TestAnInterruptedHealthySwapFinishes(t *testing.T) {
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
	if cur := d.byName["tracepad-app"]; cur == nil || cur.Config.Image != "ghcr.io/tracepad/tracepad:0.2.0" {
		t.Errorf("the new container: %+v", cur)
	}
}

// A container is stopped as a server is (the sixth review): SIGTERM, its
// restart policy set to no so docker does not start it again, and no
// SIGKILL after the wait — one that has not stopped leaves the run stuck,
// and --back puts its policy back and starts it.
func TestAContainerThatDoesNotStopIsNeverKilled(t *testing.T) {
	d := newFakeDocker(t)
	d.setupContainer(t, t.TempDir())
	d.stubborn = true
	deps := containerDeps(t, d)
	deps.StopWait = 50 * time.Millisecond
	rep, code := runReport(t, deps)
	if code != exitStuck || !strings.Contains(rep.Summary, "has not stopped") {
		t.Fatalf("%d %s", code, rep.Summary)
	}
	c := d.byName["tracepad-app"]
	if !c.State.Running || c.HostConfig.RestartPolicy.Name != "no" {
		t.Errorf("the container after the stop's wait: running %v, policy %s", c.State.Running, c.HostConfig.RestartPolicy.Name)
	}
	d.stubborn = false
	c.State.Running = false // it stopped, late
	if back, code := runReport(t, deps, "--back", rep.Run.ID); code != exitOK {
		t.Fatalf("--back: %d %s", code, back.Summary)
	}
	if !c.State.Running || c.HostConfig.RestartPolicy.Name != "always" {
		t.Errorf("after --back: running %v, policy %s", c.State.Running, c.HostConfig.RestartPolicy.Name)
	}
}
