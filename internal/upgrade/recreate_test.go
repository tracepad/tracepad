//go:build unix

package upgrade

// What a recreate carries (spec 054 #47, #48; the audit of #226). Each cell
// makes setup's container with one more setting, on a fake docker that
// allocates a port asked for as any at every create and start, as docker
// does, answers on the ports actually bound, and says the cell's default log
// driver. Then:
//   reproduced: the upgrade is healthy at the address the run recorded, the
//     setting is on the new container (or in the docker run that made it),
//     --back is healthy at the same address, and the setting is on the old
//     version's container;
//   refused: the upgrade touches nothing — no kill, rename, run or update,
//     no stop_sent — and the reason it gives names the setting;
//   round trip: values as docker gives them are recorded, and load back.
// A test beside it holds every field of the inspect the command reads to one
// class: reproduced, refused by name, or the daemon's own with a check.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type recreateDocker struct {
	*fakeDocker
	pmu      sync.Mutex
	nextPort int
	driver   string // the daemon's default log driver; "" is json-file
	rootless bool
}

func (m *recreateDocker) Run(ctx context.Context, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "info" {
		m.fakeDocker.mu.Lock()
		m.fakeDocker.calls = append(m.fakeDocker.calls, slices.Clone(args))
		m.fakeDocker.mu.Unlock()
		opts := `["name=seccomp,profile=builtin","name=cgroupns"]`
		if m.rootless {
			opts = `["name=seccomp,profile=builtin","name=rootless"]`
		}
		d := m.driver
		if d == "" {
			d = "json-file"
		}
		b, _ := json.Marshal(d)
		return []byte(opts + " " + string(b) + "\n"), nil
	}
	out, err := m.fakeDocker.Run(ctx, args...)
	if len(args) >= 2 && ((args[0] == "run" && args[1] == "-d") || args[0] == "start") {
		ref := strings.TrimSpace(string(out))
		if args[0] == "start" {
			ref = args[1]
		}
		m.allocate(ref, args)
	}
	return out, err
}

// allocate binds what was asked for as any port, as docker does on every
// start: a fresh port each time.
func (m *recreateDocker) allocate(ref string, args []string) {
	d := m.fakeDocker
	d.mu.Lock()
	defer d.mu.Unlock()
	c := d.find(ref)
	if c == nil && len(args) > 0 && args[0] == "run" {
		for i, a := range args {
			if a == "--name" && i+1 < len(args) {
				c = d.byName[args[i+1]]
			}
		}
	}
	if c == nil {
		return
	}
	m.pmu.Lock()
	defer m.pmu.Unlock()
	ports := map[string][]portBinding{}
	for p, bs := range c.HostConfig.PortBindings {
		for _, b := range bs {
			if b.HostPort == "" {
				m.nextPort++
				b.HostPort = fmt.Sprint(m.nextPort)
			}
			ports[p] = append(ports[p], b)
		}
	}
	if c.HostConfig.PublishAllPorts {
		for p := range c.Config.ExposedPorts {
			if _, ok := ports[p]; !ok {
				m.nextPort++
				ports[p] = []portBinding{{HostIP: "0.0.0.0", HostPort: fmt.Sprint(m.nextPort)}}
			}
		}
	}
	c.NetworkSettings.Ports = ports
}

// RoundTrip answers on the ports actually bound (NetworkSettings.Ports).
func (m *recreateDocker) RoundTrip(req *http.Request) (*http.Response, error) {
	d := m.fakeDocker
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, c := range d.byName {
		if !c.State.Running || c.State.Restarting || c.mute {
			continue
		}
		ports := c.NetworkSettings.Ports
		if len(ports) == 0 {
			ports = c.HostConfig.PortBindings
		}
		for _, bs := range ports {
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
					for _, mt := range c.Mounts {
						if mt.Destination == "/data" {
							vol = mt.Name
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

type recreateCase struct {
	name string
	// claim: "reproduced" or "refused".
	claim string
	// name, image ref and volume of the person's container, when not setup's.
	ctrName, ref, volume string
	// ports replace setup's -p 127.0.0.1:4318:4318 when set.
	ports []string
	// args are more docker run options, before the image.
	args []string
	// cmd replaces setup's "serve".
	cmd []string
	// says is what a refusal's reason must name.
	says string
	// mutate sets what docker's inspect shows beyond what the fake's run
	// makes (fields the fake does not take as options).
	mutate func(c *fakeContainer)
	driver string
	// carried says, of the container the run made (or the way back), and of
	// the docker run that made it, what of the setting is missing; "" when
	// it is all there.
	carried func(c *fakeContainer, run []string) string
}

type recreateCell struct {
	t        *testing.T
	m        *recreateDocker
	deps     Deps
	w        *fakeWorld
	name     string
	problems []string
	log      []string
	notes    []string
}

func (c *recreateCell) problem(format string, args ...any) {
	c.problems = append(c.problems, fmt.Sprintf(format, args...))
}

func (c *recreateCell) cmd(args ...string) (Report, int) {
	rep, code, off := cellRun(c.deps, args...)
	c.log = append(c.log, fmt.Sprintf("%s -> %d %s", strings.Join(args, " "), code, oneLine(rep.Summary)))
	if off != "" {
		c.problem("offTable on `%s`: %s", strings.Join(args, " "), oneLine(off))
	}
	return rep, code
}

func newRecreateCell(t *testing.T, cs recreateCase) *recreateCell {
	w := newBareWorld(t)
	d := newFakeDocker(t)
	for _, v := range []string{fOld, fNew, fBroken} {
		d.image("ghcr.io/tracepad/tracepad:"+v, v)
		d.image("docker.io/tracepad/tracepad:"+v, v)
		d.image("tracepad/tracepad:"+v, v)
	}
	d.brokenRef = "ghcr.io/tracepad/tracepad:" + fBroken
	m := &recreateDocker{fakeDocker: d, nextPort: 41000, driver: cs.driver}
	deps := w.deps()
	deps.StopWait = 50 * time.Millisecond
	deps.Sleep = func(ctx context.Context, dd time.Duration) error { return sleepCtx(ctx, min(dd, 5*time.Millisecond)) }
	deps.HealthWait = 100 * time.Millisecond
	deps.Docker = m
	deps.HTTP = &http.Client{Transport: m, Timeout: 2 * time.Second}
	c := &recreateCell{t: t, m: m, deps: deps, w: w, name: cs.name}

	name, ref, vol := cs.ctrName, cs.ref, cs.volume
	if name == "" {
		name = "tracepad-app"
	}
	if ref == "" {
		ref = "ghcr.io/tracepad/tracepad:" + fOld
	}
	if _, ok := d.images[ref]; !ok {
		// A tag or digest of the old release, by another name.
		d.images[ref] = d.images["ghcr.io/tracepad/tracepad:"+fOld]
	}
	if vol == "" {
		vol = "tracepad-app"
	}
	d.volumes[vol] = 2
	env := filepath.Join(w.home, "setup.env")
	_ = os.WriteFile(env, []byte("TRACEPAD_URL=http://localhost:4318\nTRACEPAD_PROJECTS=app:tp-pk-a:tp-sk-b\n"), 0o600)
	args := []string{"run", "-d", "--name", name, "--restart", "always", "--mount", "type=volume,src=" + vol + ",dst=/data", "--env-file", env}
	ports := cs.ports
	if ports == nil {
		ports = []string{"127.0.0.1:4318:4318"}
	}
	for _, p := range ports {
		args = append(args, "-p", p)
	}
	args = append(args, cs.args...)
	args = append(args, ref)
	cmd := cs.cmd
	if cmd == nil {
		cmd = []string{"serve"}
	}
	args = append(args, cmd...)
	if _, err := m.Run(context.Background(), args...); err != nil {
		t.Fatalf("the person's container: %v", err)
	}
	d.mu.Lock()
	ctr := d.byName[name]
	if cs.mutate != nil {
		if ctr.extra == nil {
			ctr.extra = map[string]any{}
		}
		cs.mutate(ctr)
	}
	d.calls = nil
	d.mu.Unlock()
	return c
}

func (c *recreateCell) calls(verb string) [][]string {
	c.m.fakeDocker.mu.Lock()
	defer c.m.fakeDocker.mu.Unlock()
	var out [][]string
	for _, call := range c.m.fakeDocker.calls {
		if len(call) > 0 && call[0] == verb && (verb != "run" || (len(call) > 1 && call[1] == "-d")) {
			out = append(out, call)
		}
	}
	return out
}

func (c *recreateCell) container(name string) *fakeContainer {
	c.m.fakeDocker.mu.Lock()
	defer c.m.fakeDocker.mu.Unlock()
	ctr := c.m.fakeDocker.byName[name]
	if ctr == nil {
		return nil
	}
	cp := *ctr
	return &cp
}

func (c *recreateCell) answersAt(url string) string {
	v, _ := health(context.Background(), &http.Client{Transport: c.m, Timeout: time.Second}, url)
	return v
}

func (c *recreateCell) runState() (*State, string) {
	states, _ := filepath.Glob(filepath.Join(c.deps.Backups, "*", stateFile))
	if len(states) == 0 {
		return nil, ""
	}
	dir := filepath.Dir(states[len(states)-1])
	st, err := loadState(dir)
	if err != nil {
		c.problem("the run's state does not load: %v", err)
		return nil, dir
	}
	return st, dir
}

func (c *recreateCell) reasonOf(rep Report, name string) string {
	for _, cr := range rep.Containers {
		if cr.Name == name {
			return cr.Whose + ": " + cr.Reason
		}
	}
	return "(not listed)"
}

func (c *recreateCell) runRefused(cs recreateCase) {
	name := cs.ctrName
	if name == "" {
		name = "tracepad-app"
	}
	before := c.container(name)
	rep, code := c.cmd("--to", fNew)
	c.notes = append(c.notes, "reason: "+c.reasonOf(rep, name))
	if n := len(c.calls("kill")) + len(c.calls("rename")) + len(c.calls("run")) + len(c.calls("update")); n > 0 {
		c.problem("claimed refused, but the upgrade acted on it (%d kill/rename/run/update calls), exit %d: %s", n, code, oneLine(rep.Summary))
	}
	if after := c.container(name); after == nil || after.State.Running != before.State.Running || after.ID != before.ID {
		c.problem("claimed refused, but the container is not as it was")
	}
	if st, _ := c.runState(); st != nil && st.has(stepStopSent) {
		c.problem("claimed refused, but the run recorded stop_sent")
	}
	if code == exitOK && strings.Contains(c.reasonOf(rep, name), "command's") {
		c.problem("claimed refused, but the plan took it as the command's")
	}
	if cs.says != "" && !strings.Contains(c.reasonOf(rep, name), cs.says) {
		c.problem("refused, but its reason does not name %q: %s", cs.says, c.reasonOf(rep, name))
	}
}

func (c *recreateCell) runReproduced(cs recreateCase) {
	name := cs.ctrName
	if name == "" {
		name = "tracepad-app"
	}
	rep, code := c.cmd("--to", fNew)
	if code != exitOK {
		c.problem("claimed reproduced, but the upgrade ended %d (%s; %s)", code, oneLine(rep.Summary), c.reasonOf(rep, name))
		if st, _ := c.runState(); st != nil && rep.Run != nil {
			if _, code := c.cmd("--back", rep.Run.ID); code != exitOK {
				c.problem("and --back ended %d", code)
			}
		}
		return
	}
	st, dir := c.runState()
	if st == nil || st.Container == nil {
		c.problem("no container run recorded")
		if dir != "" {
			_, code := c.cmd("--back", filepath.Base(dir))
			c.problem("--back of that run ended %d", code)
		}
		return
	}
	url := st.Container.URL
	if v := c.answersAt(url); v != fNew {
		c.problem("after the upgrade, the run's address %s answers %q, not %s", url, v, fNew)
	}
	runs := c.calls("run")
	var made []string
	if len(runs) > 0 {
		made = runs[len(runs)-1]
		c.notes = append(c.notes, "upgrade's run: "+strings.Join(made[2:], " "))
	}
	if cs.carried != nil {
		if miss := cs.carried(c.container(name), made); miss != "" {
			c.problem("the new container lacks it: %s", miss)
		}
	}
	nRuns := len(runs)
	_, code = c.cmd("--back", st.Run)
	if code != exitOK {
		c.problem("--back ended %d", code)
		_, code = c.cmd("--back", st.Run)
		if code != exitOK {
			c.problem("and again %d", code)
		}
		return
	}
	if v := c.answersAt(url); v != fOld {
		c.problem("after --back, the run's address %s answers %q, not %s", url, v, fOld)
	}
	runs = c.calls("run")
	made = nil
	if len(runs) > nRuns {
		made = runs[len(runs)-1]
	}
	if cs.carried != nil {
		if miss := cs.carried(c.container(name), made); miss != "" {
			c.problem("the way back's container lacks it: %s", miss)
		}
	}
}

func (c *recreateCell) report() {
	verdict := "PASS"
	if len(c.problems) > 0 {
		verdict = "FAIL"
	}
	c.t.Logf("RESULT\t%s\t%s\t%s", c.name, verdict, strings.Join(c.problems, " | "))
	c.t.Logf("NOTE\t%s\t%s", c.name, strings.Join(c.notes, " | "))
	c.t.Logf("LOG\t%s\t%s", c.name, strings.Join(c.log, " || "))
	if verdict == "FAIL" {
		c.t.Fail()
	}
}

// argsHave: an option of the docker run (never the env file's path or the
// image) carries sub.
func argsHave(run []string, sub string) bool {
	for i, a := range run {
		if i > 0 && run[i-1] == "--env-file" {
			continue
		}
		if (a == "-v" || a == "--volume" || a == "--mount") && i+1 < len(run) && strings.Contains(run[i+1], sub) {
			return true
		}
	}
	return false
}

func setExtra(c *fakeContainer, key string, v any) { c.extra[key] = v }

// bindAt is a bind mount the fake makes for a cell.
func bindAt(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), "tls")
	_ = os.MkdirAll(dir, 0o700)
	return dir
}

func recreateCases(t *testing.T) []recreateCase {
	tls := bindAt(t)
	bind := "type=bind,src=" + tls + ",dst=/tls,readonly"
	refused := func(name string, mutate func(c *fakeContainer)) recreateCase {
		return recreateCase{name: "refused/" + name, claim: "refused", mutate: mutate}
	}
	return []recreateCase{
		// Reproduced, as the command claims.
		{name: "repro/baseline", claim: "reproduced"},
		{name: "repro/restart-unless-stopped", claim: "reproduced", args: nil, mutate: func(c *fakeContainer) { c.HostConfig.RestartPolicy.Name = "unless-stopped" },
			carried: func(n *fakeContainer, _ []string) string {
				if n == nil || n.HostConfig.RestartPolicy.Name != "unless-stopped" {
					return "restart policy unless-stopped"
				}
				return ""
			}},
		{name: "repro/restart-on-failure-5", claim: "reproduced", mutate: func(c *fakeContainer) {
			c.HostConfig.RestartPolicy.Name, c.HostConfig.RestartPolicy.MaximumRetryCount = "on-failure", 5
		}, carried: func(n *fakeContainer, _ []string) string {
			if n == nil || n.HostConfig.RestartPolicy.Name != "on-failure" || n.HostConfig.RestartPolicy.MaximumRetryCount != 5 {
				return "restart policy on-failure:5"
			}
			return ""
		}},
		{name: "repro/second-port-loopback", claim: "reproduced", ports: []string{"127.0.0.1:4318:4318", "127.0.0.1:9090:9090"},
			carried: func(n *fakeContainer, _ []string) string {
				if n == nil || len(n.HostConfig.PortBindings["9090/tcp"]) == 0 {
					return "-p 127.0.0.1:9090:9090"
				}
				return ""
			}},
		// A port docker chooses moves at every start (#48 (a)).
		{name: "refused/ephemeral-loopback-port", claim: "refused", ports: []string{"127.0.0.1::4318"}, says: "a port docker chooses"},
		{name: "repro/bind-readonly", claim: "reproduced", args: []string{"--mount", bind},
			carried: func(n *fakeContainer, _ []string) string {
				for _, m := range n.Mounts {
					if m.Destination == "/tls" && m.Type == "bind" && !m.RW {
						return ""
					}
				}
				return "the read-only bind at /tls"
			}},
		// What a mount has beyond type, source, destination and read-only is
		// named, never dropped (#48 (d)).
		{name: "refused/bind-selinux-Z", claim: "refused", args: []string{"--mount", bind}, says: "HostConfig.Binds[/tls]=ro,Z",
			mutate: func(c *fakeContainer) { setExtra(c, "HostConfig.Binds", []string{tls + ":/tls:ro,Z"}) }},
		{name: "refused/bind-propagation-rshared", claim: "refused", args: []string{"--mount", bind}, says: "HostConfig.Mounts[/tls].BindOptions",
			mutate: func(c *fakeContainer) {
				setExtra(c, "HostConfig.Mounts", []map[string]any{{"Type": "bind", "Source": tls, "Target": "/tls", "ReadOnly": true, "BindOptions": map[string]any{"Propagation": "rshared"}}})
			}},
		{name: "refused/volume-nocopy", claim: "refused", says: "HostConfig.Mounts[/data].VolumeOptions",
			mutate: func(c *fakeContainer) {
				setExtra(c, "HostConfig.Mounts", []map[string]any{{"Type": "volume", "Source": "tracepad-app", "Target": "/data", "VolumeOptions": map[string]any{"NoCopy": true}}})
			}},
		{name: "refused/volume-subpath", claim: "refused", says: "HostConfig.Mounts[/data].VolumeOptions",
			mutate: func(c *fakeContainer) {
				setExtra(c, "HostConfig.Mounts", []map[string]any{{"Type": "volume", "Source": "tracepad-app", "Target": "/data", "VolumeOptions": map[string]any{"Subpath": "sub"}}})
			}},
		{name: "repro/label", claim: "reproduced", args: []string{"--label", "com.example.team=obs"},
			carried: func(n *fakeContainer, _ []string) string {
				if n == nil || n.Config.Labels["com.example.team"] != "obs" {
					return "label com.example.team=obs"
				}
				return ""
			}},
		{name: "repro/user", claim: "reproduced", args: []string{"--user", "1000:1000"},
			carried: func(n *fakeContainer, _ []string) string {
				if n == nil || n.Config.User != "1000:1000" {
					return "--user 1000:1000"
				}
				return ""
			}},
		{name: "repro/command-args", claim: "reproduced", cmd: []string{"serve", "--listen", ":4318"},
			carried: func(n *fakeContainer, _ []string) string {
				if n == nil || !slices.Equal(n.Config.Cmd, []string{"serve", "--listen", ":4318"}) {
					return "its command serve --listen :4318"
				}
				return ""
			}},
		{name: "repro/env", claim: "reproduced",
			carried: func(n *fakeContainer, _ []string) string {
				if n == nil || !slices.Contains(n.Config.Env, "TRACEPAD_PROJECTS=app:tp-pk-a:tp-sk-b") {
					return "TRACEPAD_PROJECTS"
				}
				return ""
			}},
		{name: "repro/log-driver-local", claim: "reproduced", args: []string{"--log-driver", "local"},
			carried: func(n *fakeContainer, _ []string) string {
				if n == nil || n.HostConfig.LogConfig.Type != "local" {
					return "--log-driver local"
				}
				return ""
			}},
		{name: "repro/log-driver-plugin-own", claim: "reproduced", args: []string{"--log-driver", "grafana/loki-docker-driver:latest"},
			carried: func(n *fakeContainer, _ []string) string {
				if n == nil || n.HostConfig.LogConfig.Type != "grafana/loki-docker-driver:latest" {
					return "--log-driver grafana/loki-docker-driver:latest"
				}
				return ""
			}},
		// The state's round trip: values docker gives, written and read back.
		{name: "roundtrip/daemon-log-driver-plugin", claim: "reproduced", driver: "grafana/loki-docker-driver:latest"},
		{name: "roundtrip/daemon-log-driver-journald", claim: "reproduced", driver: "journald"},
		{name: "roundtrip/name-dots-underscores", claim: "reproduced", ctrName: "tracepad-my.app_1"},
		{name: "roundtrip/volume-dots", claim: "reproduced", volume: "tracepad.app_v1"},
		{name: "roundtrip/image-dockerhub", claim: "reproduced", ref: "docker.io/tracepad/tracepad:" + fOld},
		{name: "roundtrip/image-digest", claim: "reproduced", ref: "ghcr.io/tracepad/tracepad@sha256:" + strings.Repeat("ab", 32)},
		{name: "roundtrip/image-tag-uppercase", claim: "reproduced", ref: "ghcr.io/tracepad/tracepad:Stable"},
		{name: "roundtrip/image-implicit-latest", claim: "reproduced", ref: "tracepad/tracepad"},
		// Refused, as the command claims.
		{name: "refused/publish-all-P", claim: "refused", ports: []string{}, args: []string{"-P"}},
		{name: "refused/port-every-address", claim: "refused", ports: []string{":4318:4318"}},
		{name: "refused/port-0.0.0.0", claim: "refused", ports: []string{"0.0.0.0:4318:4318"}},
		{name: "refused/tmpfs", claim: "refused", args: []string{"--tmpfs", "/tmp"}},
		{name: "refused/network", claim: "refused", args: []string{"--network", "tpnet"}},
		{name: "refused/log-opt", claim: "refused", args: []string{"--log-opt", "max-size=10m"}},
		{name: "refused/entrypoint-cleared", claim: "refused", args: []string{"--entrypoint", ""}},
		refused("env-newline", func(c *fakeContainer) { c.Config.Env = append(c.Config.Env, "TRACEPAD_X=a\nb") }),
		refused("privileged", func(c *fakeContainer) { c.HostConfig.Privileged = true }),
		refused("cap-add", func(c *fakeContainer) { c.HostConfig.CapAdd = []string{"NET_ADMIN"} }),
		refused("memory", func(c *fakeContainer) { c.HostConfig.Memory = 1 << 30 }),
		refused("readonly-rootfs", func(c *fakeContainer) { c.HostConfig.ReadonlyRootfs = true }),
		refused("extra-hosts", func(c *fakeContainer) { c.HostConfig.ExtraHosts = []string{"db:10.0.0.2"} }),
		refused("dns", func(c *fakeContainer) { c.HostConfig.DNS = []string{"1.1.1.1"} }),
		refused("security-opt", func(c *fakeContainer) { c.HostConfig.SecurityOpt = []string{"no-new-privileges"} }),
		refused("sysctls", func(c *fakeContainer) { c.HostConfig.Sysctls = map[string]string{"net.core.somaxconn": "1024"} }),
		refused("pid-host", func(c *fakeContainer) { c.HostConfig.PidMode = "host" }),
		refused("init", func(c *fakeContainer) { b := true; c.HostConfig.Init = &b }),
		refused("autoremove", func(c *fakeContainer) { c.HostConfig.AutoRemove = true }),
		refused("group-add", func(c *fakeContainer) { c.HostConfig.GroupAdd = []string{"video"} }),
		refused("workdir", func(c *fakeContainer) { c.Config.WorkingDir = "/srv" }),
		refused("stop-signal", func(c *fakeContainer) { c.Config.StopSignal = "SIGINT" }),
		refused("healthcheck", func(c *fakeContainer) { c.Config.Healthcheck = json.RawMessage(`{"Test":["NONE"]}`) }),
		refused("data-on-bind", func(c *fakeContainer) {
			for i := range c.Mounts {
				if c.Mounts[i].Destination == "/data" {
					c.Mounts[i].Type, c.Mounts[i].Source, c.Mounts[i].Name = "bind", "/srv/tp", ""
				}
			}
		}),
		refused("volume-driver-nfs", func(c *fakeContainer) {
			for i := range c.Mounts {
				if c.Mounts[i].Destination == "/data" {
					c.Mounts[i].Driver = "nfs"
				}
			}
		}),
		refused("compose", func(c *fakeContainer) { c.Config.Labels["com.docker.compose.project"] = "tp" }),
		refused("swarm", func(c *fakeContainer) { c.Config.Labels["com.docker.swarm.service.id"] = "x" }),
		refused("not-running", func(c *fakeContainer) { c.State.Running = false }),
		// Settings the inspect has that the command's types do not name:
		// unreproduced, so refused by its generic rule.
		refused("pids-limit", func(c *fakeContainer) { setExtra(c, "HostConfig.PidsLimit", 100) }),
		refused("shm-size", func(c *fakeContainer) { setExtra(c, "HostConfig.ShmSize", 134217728) }),
		refused("runtime-runsc", func(c *fakeContainer) { setExtra(c, "HostConfig.Runtime", "runsc") }),
		refused("cgroup-parent", func(c *fakeContainer) { setExtra(c, "HostConfig.CgroupParent", "/tp") }),
		refused("cpuset", func(c *fakeContainer) { setExtra(c, "HostConfig.CpusetCpus", "0-1") }),
		refused("memory-reservation", func(c *fakeContainer) { setExtra(c, "HostConfig.MemoryReservation", 1<<28) }),
		refused("oom-score-adj", func(c *fakeContainer) { setExtra(c, "HostConfig.OomScoreAdj", 500) }),
		refused("blkio-weight", func(c *fakeContainer) { setExtra(c, "HostConfig.BlkioWeight", 300) }),
		refused("storage-opt", func(c *fakeContainer) { setExtra(c, "HostConfig.StorageOpt", map[string]string{"size": "10G"}) }),
		refused("annotations", func(c *fakeContainer) { setExtra(c, "HostConfig.Annotations", map[string]string{"a": "b"}) }),
		refused("domainname", func(c *fakeContainer) { setExtra(c, "Config.Domainname", "example.org") }),
		refused("hostname", func(c *fakeContainer) { c.Config.Hostname = "tracepad" }),
		refused("mac-address", func(c *fakeContainer) { setExtra(c, "Config.MacAddress", "02:42:ac:11:00:02") }),
		refused("tty", func(c *fakeContainer) { setExtra(c, "Config.Tty", true) }),
		refused("cpu-period", func(c *fakeContainer) { setExtra(c, "HostConfig.CpuPeriod", 100000) }),
		refused("memory-swap", func(c *fakeContainer) { setExtra(c, "HostConfig.MemorySwap", 2<<30) }),
		refused("oom-kill-disable", func(c *fakeContainer) { setExtra(c, "HostConfig.OomKillDisable", true) }),
		refused("volume-driver", func(c *fakeContainer) { setExtra(c, "HostConfig.VolumeDriver", "nfs") }),
		refused("ipc-host", func(c *fakeContainer) { c.HostConfig.IpcMode = "host" }),
		// The namespaces' modes are written out as they are (#48, the audit's
		// cgroupns and ipc).
		{name: "repro/ipc-shareable", claim: "reproduced", args: []string{"--ipc", "shareable"},
			carried: func(n *fakeContainer, _ []string) string {
				if n == nil || n.HostConfig.IpcMode != "shareable" {
					return "--ipc shareable"
				}
				return ""
			}},
		{name: "repro/cgroupns-host", claim: "reproduced", args: []string{"--cgroupns", "host"},
			carried: func(n *fakeContainer, _ []string) string {
				if n == nil || n.HostConfig.CgroupnsMode != "host" {
					return "--cgroupns host"
				}
				return ""
			}},
	}
}

// theRecreateMatrix runs the cells, under the matrices' seams.
func theRecreateMatrix(t *testing.T) {
	for _, cs := range recreateCases(t) {
		t.Run(cs.name, func(t *testing.T) {
			t.Parallel()
			c := newRecreateCell(t, cs)
			if cs.claim == "refused" {
				c.runRefused(cs)
			} else {
				c.runReproduced(cs)
			}
			c.report()
		})
	}
	// Rootless docker: refused, before anything.
	t.Run("refused/rootless", func(t *testing.T) {
		t.Parallel()
		c := newRecreateCell(t, recreateCase{name: "refused/rootless"})
		c.m.rootless = true
		c.runRefused(recreateCase{})
		c.report()
	})
	t.Run("two/named-one", func(t *testing.T) {
		t.Parallel()
		twoContainers(t)
	})
}

// Two containers of the command's; one named with --container: the other
// stays the command's (with a line to upgrade it next), as --data-dir leaves
// other servers (the review of #226, finding 3).
func twoContainers(t *testing.T) {
	c := newRecreateCell(t, recreateCase{name: "two/named-one"})
	d := c.m.fakeDocker
	d.mu.Lock()
	d.volumes["tracepad-b"] = 2
	d.mu.Unlock()
	env := filepath.Join(c.w.home, "b.env")
	_ = os.WriteFile(env, []byte("TRACEPAD_URL=http://localhost:4319\n"), 0o600)
	if _, err := c.m.Run(context.Background(), "run", "-d", "--name", "tracepad-b", "--restart", "always",
		"--mount", "type=volume,src=tracepad-b,dst=/data", "--env-file", env, "-p", "127.0.0.1:4319:4318", "ghcr.io/tracepad/tracepad:"+fOld, "serve"); err != nil {
		t.Fatal(err)
	}
	all, code := c.cmd("--plan", "--to", fNew)
	c.notes = append(c.notes, fmt.Sprintf("plan without --container: exit %d; a=%s; b=%s", code, c.reasonOf(all, "tracepad-app"), c.reasonOf(all, "tracepad-b")))
	named, code := c.cmd("--plan", "--to", fNew, "--container", "tracepad-app")
	c.notes = append(c.notes, fmt.Sprintf("plan --container tracepad-app: exit %d; b=%s; next=%q", code, c.reasonOf(named, "tracepad-b"), named.Next))
	if strings.HasPrefix(c.reasonOf(named, "tracepad-b"), "person") {
		c.problem("naming tracepad-app makes tracepad-b the person's: %s", c.reasonOf(named, "tracepad-b"))
	}
	if !strings.Contains(strings.Join(named.Next, "\n"), "--container tracepad-b") {
		c.problem("no later run's line for tracepad-b: %q", named.Next)
	}
	c.report()
}

// Every field of the inspect the command reads is in exactly one class:
// carried by the recreate, refused by name, or the daemon's own with a check
// (the review of #226). A field added to the command's types without a class
// fails here, as a refusal without a row does.
func TestEveryFieldARecreateReadsHasOneClass(t *testing.T) {
	t.Parallel()
	classes := func(name string, sets ...map[string]bool) int {
		n := 0
		for _, set := range sets {
			if set[name] {
				n++
			}
		}
		return n
	}
	daemonHost := map[string]bool{}
	for k := range hostDaemon {
		daemonHost[k] = true
	}
	for section, typ := range map[string]reflect.Type{
		"HostConfig": reflect.TypeOf(inspectContainer{}.HostConfig),
		"Config":     reflect.TypeOf(inspectConfig{}),
	} {
		sets := []map[string]bool{hostReproduced, hostRefused, daemonHost}
		if section == "Config" {
			sets = []map[string]bool{configReproduced, configRefused, configDaemon}
		}
		for i := 0; i < typ.NumField(); i++ {
			name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			if n := classes(name, sets...); n != 1 {
				t.Errorf("%s.%s is in %d classes, not one: carried, refused by name, or the daemon's own", section, name, n)
			}
		}
		// And no field is in two, typed or not.
		all := map[string]int{}
		for _, set := range sets {
			for k := range set {
				all[k]++
			}
		}
		for k, n := range all {
			if n > 1 {
				t.Errorf("%s.%s is in %d classes", section, k, n)
			}
		}
	}
}
