package upgrade

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Docker is the docker CLI. The real one runs `docker`; tests give a fake
// that keeps containers and volumes in memory and records every call.
type Docker interface {
	// Run runs `docker args…` and answers its standard output. A non-zero
	// exit is an error carrying its standard error.
	Run(ctx context.Context, args ...string) ([]byte, error)
}

type dockerCLI struct{ path string }

// newDockerCLI is the `docker` on PATH, or nil when there is none.
func newDockerCLI() Docker {
	path, err := exec.LookPath("docker")
	if err != nil {
		return nil
	}
	return dockerCLI{path: path}
}

func (d dockerCLI) Run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := child(ctx, d.path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if args[0] == "logs" {
		// A server logs to stderr; `docker logs` replays it there.
		cmd.Stderr = &stdout
	}
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return stdout.Bytes(), nil
}

// busybox archives and restores a volume, as docker.md's backup does; the
// newest stable tag on Docker Hub on 2026-10-05 (spec 054 #10).
const busybox = "busybox:1.38"

// imageRepos are the repositories of the image: GitHub's registry and the
// Docker Hub copy (spec 020 #33).
var imageRepos = []string{"ghcr.io/tracepad/tracepad", "docker.io/tracepad/tracepad", "tracepad/tracepad"}

// inspectConfig is the part of a container's or an image's Config the
// command reads.
type inspectConfig struct {
	HostnameV    string              `json:"Hostname"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts"`
	Image        string              `json:"Image"`
	Env          []string            `json:"Env"`
	Cmd          []string            `json:"Cmd"`
	Entrypoint   []string            `json:"Entrypoint"`
	User         string              `json:"User"`
	WorkingDir   string              `json:"WorkingDir"`
	Labels       map[string]string   `json:"Labels"`
	Healthcheck  json.RawMessage     `json:"Healthcheck"`
	StopSignal   string              `json:"StopSignal"`
}

// Hostname is the container's hostname, as inspect gives it.
func (c inspectConfig) Hostname() string { return c.HostnameV }

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type mount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

// inspectContainer is the part of `docker inspect` the command reads. The
// fields under "refused" are the ones a recreate would have to reproduce and
// this command does not: any of them set makes the container the person's.
type inspectContainer struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	Image string `json:"Image"`
	State struct {
		Running    bool   `json:"Running"`
		Restarting bool   `json:"Restarting"`
		StartedAt  string `json:"StartedAt"`
	} `json:"State"`
	RestartCount int           `json:"RestartCount"`
	Config       inspectConfig `json:"Config"`
	HostConfig   struct {
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
		PortBindings map[string][]portBinding `json:"PortBindings"`
		NetworkMode  string                   `json:"NetworkMode"`
		LogConfig    struct {
			Type   string            `json:"Type"`
			Config map[string]string `json:"Config"`
		} `json:"LogConfig"`
		// refused
		AutoRemove     bool              `json:"AutoRemove"`
		Privileged     bool              `json:"Privileged"`
		ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
		CapAdd         []string          `json:"CapAdd"`
		CapDrop        []string          `json:"CapDrop"`
		Devices        []json.RawMessage `json:"Devices"`
		DeviceRequests []json.RawMessage `json:"DeviceRequests"`
		ExtraHosts     []string          `json:"ExtraHosts"`
		Links          []string          `json:"Links"`
		VolumesFrom    []string          `json:"VolumesFrom"`
		GroupAdd       []string          `json:"GroupAdd"`
		DNS            []string          `json:"Dns"`
		DNSSearch      []string          `json:"DnsSearch"`
		SecurityOpt    []string          `json:"SecurityOpt"`
		Tmpfs          map[string]string `json:"Tmpfs"`
		Sysctls        map[string]string `json:"Sysctls"`
		Ulimits        []json.RawMessage `json:"Ulimits"`
		Memory         int64             `json:"Memory"`
		NanoCpus       int64             `json:"NanoCpus"`
		CPUShares      int64             `json:"CpuShares"`
		CPUQuota       int64             `json:"CpuQuota"`
		PidMode        string            `json:"PidMode"`
		IpcMode        string            `json:"IpcMode"`
		UTSMode        string            `json:"UTSMode"`
		Init           *bool             `json:"Init"`
	} `json:"HostConfig"`
	Mounts []mount `json:"Mounts"`
	// raw is the whole of the inspect, for the settings the command does not
	// reproduce: whatever is set there, it does not claim to keep.
	raw map[string]json.RawMessage
}

type inspectImage struct {
	ID     string        `json:"Id"`
	Config inspectConfig `json:"Config"`
}

// Container is a container running Tracepad's image.
type Container struct {
	Name string
	// Ref is the image reference it was created from; Repo is that without
	// the tag.
	Ref, Repo string
	Volume    string
	URL       string
	Version   string
	Ours      bool
	Reason    string
	inspect   inspectContainer
}

func (c Container) publishesDefault() bool {
	for _, b := range c.inspect.HostConfig.PortBindings["4318/tcp"] {
		if b.HostPort == "4318" {
			return true
		}
	}
	return false
}

// imageRepo is the repository of ref when it is one of the image's, without
// its tag or digest.
func imageRepo(ref string) (string, bool) {
	repo := ref
	if i := strings.Index(repo, "@"); i >= 0 {
		repo = repo[:i]
	}
	if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
		repo = repo[:i]
	}
	for _, r := range imageRepos {
		if repo == r {
			return repo, true
		}
	}
	return "", false
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// classifyContainer decides whether a container is the command's (Decision
// 4), and if not, says why. named is the --container the person gave.
func classifyContainer(ic inspectContainer, img inspectImage, named string) Container {
	c := Container{Name: strings.TrimPrefix(ic.Name, "/"), Ref: ic.Config.Image, inspect: ic}
	c.Repo, _ = imageRepo(ic.Config.Image)
	for _, m := range ic.Mounts {
		if m.Destination == "/data" && m.Type == "volume" {
			c.Volume = m.Name
		}
	}
	for _, b := range ic.HostConfig.PortBindings["4318/tcp"] {
		if isLoopbackHost(b.HostIP) {
			c.URL, _ = loopbackBase(b.HostIP, b.HostPort)
		}
	}
	c.Reason = containerRefusal(ic, img, named)
	c.Ours = c.Reason == ""
	return c
}

func containerRefusal(ic inspectContainer, img inspectImage, named string) string {
	name := strings.TrimPrefix(ic.Name, "/")
	hc := ic.HostConfig
	for key := range ic.Config.Labels {
		switch {
		case strings.HasPrefix(key, "com.docker.compose."):
			return "Compose runs it (project " + ic.Config.Labels["com.docker.compose.project"] + "): its file is yours"
		case strings.HasPrefix(key, "com.docker.swarm."):
			return "a Swarm service runs it"
		case strings.HasPrefix(key, "io.kubernetes."):
			return "Kubernetes runs it"
		}
	}
	if named == "" && !strings.HasPrefix(name, "tracepad-") {
		return "its name is not tracepad-<project>, as setup names one: name it with --container to upgrade it"
	}
	if named != "" && named != name {
		return "it is not the container --container names"
	}
	if !ic.State.Running {
		return "it is not running"
	}
	var data *mount
	for i, m := range ic.Mounts {
		if m.Destination == "/data" {
			data = &ic.Mounts[i]
		}
	}
	switch {
	case data == nil:
		return "nothing is mounted at /data, so its data would go with it"
	case data.Type != "volume":
		return "/data is a " + data.Type + " mount of " + data.Source + ", not a volume"
	}
	for _, m := range ic.Mounts {
		if m.Type != "volume" && m.Type != "bind" {
			return "it has a " + m.Type + " mount at " + m.Destination
		}
	}
	defaultPort := 0
	for port, binds := range hc.PortBindings {
		for _, b := range binds {
			if !isLoopbackHost(b.HostIP) {
				ip := b.HostIP
				if ip == "" {
					ip = "every address"
				}
				return fmt.Sprintf("it publishes %s on %s, beyond this machine", port, ip)
			}
			if port == "4318/tcp" {
				defaultPort++
			}
		}
	}
	if defaultPort != 1 {
		return fmt.Sprintf("it publishes 4318/tcp %d times, not once", defaultPort)
	}
	if hc.NetworkMode != "" && hc.NetworkMode != "default" && hc.NetworkMode != "bridge" {
		return "it is on the network " + hc.NetworkMode
	}
	refused := []struct {
		set  bool
		what string
	}{
		{hc.AutoRemove, "it was started with --rm, so stopping it removes it"},
		{hc.Privileged, "it is privileged"},
		{hc.ReadonlyRootfs, "its root file system is read-only"},
		{len(hc.CapAdd)+len(hc.CapDrop) > 0, "it has capabilities of its own"},
		{len(hc.Devices)+len(hc.DeviceRequests) > 0, "it has devices"},
		{len(hc.ExtraHosts) > 0, "it has extra hosts"},
		{len(hc.Links)+len(hc.VolumesFrom) > 0, "it is linked to other containers"},
		{len(hc.GroupAdd) > 0, "it has groups of its own"},
		{len(hc.DNS)+len(hc.DNSSearch) > 0, "it has DNS settings of its own"},
		{len(hc.SecurityOpt) > 0, "it has security options"},
		{len(hc.Tmpfs) > 0, "it has tmpfs mounts"},
		{len(hc.Sysctls)+len(hc.Ulimits) > 0, "it has kernel settings or limits of its own"},
		{hc.Memory+hc.NanoCpus+hc.CPUShares+hc.CPUQuota > 0, "it has resource limits"},
		{strings.HasPrefix(hc.PidMode, "host") || strings.HasPrefix(hc.PidMode, "container:"), "it shares a PID namespace"},
		{strings.HasPrefix(hc.IpcMode, "host") || strings.HasPrefix(hc.IpcMode, "container:"), "it shares an IPC namespace"},
		{hc.UTSMode != "", "it shares a UTS namespace"},
		{hc.Init != nil && *hc.Init, "it runs an init"},
		{!reflect.DeepEqual(ic.Config.Entrypoint, img.Config.Entrypoint), "its entrypoint is not the image's"},
		{ic.Config.WorkingDir != img.Config.WorkingDir, "its working directory is not the image's"},
		{!bytes.Equal(compactJSON(ic.Config.Healthcheck), compactJSON(img.Config.Healthcheck)), "its health check is not the image's"},
		{ic.Config.StopSignal != img.Config.StopSignal, "its stop signal is not the image's"},
	}
	for _, r := range refused {
		if r.set {
			return r.what
		}
	}
	if fields := unreproduced(ic, img); len(fields) > 0 {
		return "it has settings the command does not reproduce (" + strings.Join(fields, ", ") + "): back up its volume and recreate it yourself"
	}
	for _, kv := range personEnv(ic.Config.Env, img.Config.Env) {
		if strings.ContainsAny(kv, "\r\n") {
			k, _, _ := strings.Cut(kv, "=")
			return "its variable " + k + " holds a line break, which an env file cannot carry"
		}
	}
	return ""
}

// hostKept are the HostConfig fields a recreate reproduces, refuses above by
// name, or that the daemon sets the same way again; anything else that is set
// is a setting the command would silently drop (the third review).
var hostKept = map[string]bool{
	// reproduced
	"RestartPolicy": true, "PortBindings": true, "NetworkMode": true, "LogConfig": true, "Binds": true, "Mounts": true,
	// refused above, by name
	"AutoRemove": true, "Privileged": true, "ReadonlyRootfs": true, "CapAdd": true, "CapDrop": true, "Devices": true,
	"DeviceRequests": true, "ExtraHosts": true, "Links": true, "VolumesFrom": true, "GroupAdd": true, "Dns": true,
	"DnsSearch": true, "SecurityOpt": true, "Tmpfs": true, "Sysctls": true, "Ulimits": true, "Memory": true,
	"NanoCpus": true, "CpuShares": true, "CpuQuota": true, "PidMode": true, "IpcMode": true, "UTSMode": true, "Init": true,
	// the daemon's own, the same on a recreate
	"MaskedPaths": true, "ReadonlyPaths": true, "ConsoleSize": true, "Isolation": true, "ContainerIDFile": true, "CgroupnsMode": true,
}

// hostDefaults are values the daemon gives a field nobody set.
var hostDefaults = map[string][]string{
	"ShmSize":          {"67108864"},
	"Runtime":          {`"runc"`},
	"MemorySwappiness": {"-1"},
}

// configKept are the Config fields a recreate reproduces or checks above.
var configKept = map[string]bool{
	"Env": true, "Cmd": true, "Image": true, "User": true, "Labels": true, "WorkingDir": true, "Entrypoint": true,
	"Healthcheck": true, "StopSignal": true, "Volumes": true, "ArgsEscaped": true, "ExposedPorts": true, "Hostname": true,
	// how a client attached when it was created, not how it runs
	"AttachStdout": true, "AttachStderr": true,
}

func unset(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", "false", "0", `""`, "[]", "{}":
		return true
	}
	return false
}

// unreproduced names every field of a container's inspect that is set and
// that a recreate would not carry: a container with any is the person's,
// and the plan names them.
func unreproduced(ic inspectContainer, img inspectImage) []string {
	var out []string
	sections := func(key string) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(ic.raw[key], &m)
		return m
	}
	for k, v := range sections("HostConfig") {
		if hostKept[k] || unset(v) || slices.Contains(hostDefaults[k], string(compactJSON(v))) {
			continue
		}
		out = append(out, "HostConfig."+k)
	}
	for k, v := range sections("Config") {
		if configKept[k] || unset(v) {
			continue
		}
		out = append(out, "Config."+k)
	}
	if h := ic.Config.Hostname(); h != "" && (len(ic.ID) < 12 || h != ic.ID[:12]) {
		out = append(out, "Config.Hostname")
	}
	for port := range ic.Config.ExposedPorts {
		if _, ok := img.Config.ExposedPorts[port]; ok {
			continue
		}
		if _, ok := ic.HostConfig.PortBindings[port]; ok {
			continue
		}
		out = append(out, "Config.ExposedPorts["+port+"]")
	}
	sort.Strings(out)
	return out
}

func compactJSON(raw json.RawMessage) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return raw
	}
	return buf.Bytes()
}

// personEnv is the environment the person set on a container: its own minus
// the image's, so the next image's defaults are the next image's (the second
// review).
func personEnv(container, image []string) []string {
	own := make(map[string]bool, len(image))
	for _, kv := range image {
		own[kv] = true
	}
	var out []string
	for _, kv := range container {
		if !own[kv] {
			out = append(out, kv)
		}
	}
	return out
}

// personLabels are the labels set on the container, not inherited from its
// image.
func personLabels(container, image map[string]string) []string {
	var out []string
	for k, v := range container {
		if iv, ok := image[k]; ok && iv == v {
			continue
		}
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// csvField writes a --mount field the way docker reads it: as CSV, quoted
// when it holds a comma or a quote.
func csvField(fields ...string) string {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write(fields)
	w.Flush()
	return strings.TrimSuffix(buf.String(), "\n")
}

func mountArg(m mount, src string) string {
	fields := []string{"type=" + m.Type, "src=" + src, "dst=" + m.Destination}
	if !m.RW {
		fields = append(fields, "readonly")
	}
	return csvField(fields...)
}

// restartArg is the policy as `--restart` takes it: an empty one is `no`, and
// on-failure keeps its count (the third review).
func restartArg(name string, retries int) string {
	switch {
	case name == "":
		return "no"
	case name == "on-failure" && retries > 0:
		return name + ":" + strconv.Itoa(retries)
	}
	return name
}

// runArgs is `docker run` for the container again, from its inspect: never
// through a shell, so a path with a space or a read-only flag comes through as
// it was (Decision 10). volume, when not empty, replaces the volume at /data.
func runArgs(ic inspectContainer, img inspectImage, name, ref, envFile, volume string) []string {
	hc := ic.HostConfig
	args := []string{"run", "-d", "--name", name,
		"--restart", restartArg(hc.RestartPolicy.Name, hc.RestartPolicy.MaximumRetryCount)}
	for _, m := range ic.Mounts {
		src := m.Source
		if m.Type == "volume" {
			src = m.Name
		}
		if m.Destination == "/data" && volume != "" {
			src = volume
		}
		args = append(args, "--mount", mountArg(m, src))
	}
	ports := make([]string, 0, len(hc.PortBindings))
	for port := range hc.PortBindings {
		ports = append(ports, port)
	}
	sort.Strings(ports)
	for _, port := range ports {
		for _, b := range hc.PortBindings[port] {
			host := b.HostIP
			if strings.Contains(host, ":") {
				host = "[" + host + "]"
			}
			args = append(args, "-p", host+":"+b.HostPort+":"+port)
		}
	}
	for _, l := range personLabels(ic.Config.Labels, img.Config.Labels) {
		args = append(args, "--label", l)
	}
	if hc.LogConfig.Type != "" {
		args = append(args, "--log-driver", hc.LogConfig.Type)
		opts := make([]string, 0, len(hc.LogConfig.Config))
		for k, v := range hc.LogConfig.Config {
			opts = append(opts, k+"="+v)
		}
		sort.Strings(opts)
		for _, o := range opts {
			args = append(args, "--log-opt", o)
		}
	}
	if ic.Config.User != img.Config.User {
		args = append(args, "--user", ic.Config.User)
	}
	args = append(args, "--env-file", envFile, ref)
	if !reflect.DeepEqual(nilIfEmpty(ic.Config.Cmd), nilIfEmpty(img.Config.Cmd)) {
		args = append(args, ic.Config.Cmd...)
	}
	return args
}

func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// containers lists the running containers of Tracepad's image and classifies
// each (Decision 4). note says when Docker could not be asked.
func (r *runner) containers(ctx context.Context) ([]Container, string) {
	if r.deps.Docker == nil {
		return nil, ""
	}
	out, err := r.deps.Docker.Run(ctx, "ps", "-q", "--no-trunc")
	if err != nil {
		return nil, "Docker did not answer, so containers were not checked (" + firstLine(err.Error()) + ")"
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil, ""
	}
	// Rootless Docker, or user namespaces remapped: root in a container is
	// not the host's, and an archive written there is not the host user's to
	// read back. The command does not try; the person does it (the third
	// review).
	remapped := ""
	if info, err := r.deps.Docker.Run(ctx, "info", "--format", "{{json .SecurityOptions}}"); err == nil {
		switch s := string(info); {
		case strings.Contains(s, "name=rootless"):
			remapped = "Docker runs rootless"
		case strings.Contains(s, "name=userns"):
			remapped = "Docker remaps user namespaces"
		}
	}
	list, err := r.inspectContainers(ctx, ids...)
	if err != nil {
		return nil, "the containers could not be inspected (" + firstLine(err.Error()) + ")"
	}
	var cs []Container
	for _, ic := range list {
		if _, ok := imageRepo(ic.Config.Image); !ok {
			continue
		}
		img, err := r.inspectImage(ctx, ic.Image)
		if err != nil {
			c := Container{Name: strings.TrimPrefix(ic.Name, "/"), Ref: ic.Config.Image, inspect: ic,
				Reason: "its image could not be inspected"}
			cs = append(cs, c)
			continue
		}
		c := classifyContainer(ic, img, r.flags.container)
		if remapped != "" && c.Ours {
			c.Ours = false
			c.Reason = remapped + ", and the command cannot read back an archive written in a container there: back up its volume and recreate it yourself"
		}
		cs = append(cs, c)
	}
	// Every container's address is asked at once, under the plan's deadline.
	var wg sync.WaitGroup
	for i := range cs {
		if cs[i].URL == "" {
			continue
		}
		wg.Add(1)
		go func(c *Container) {
			defer wg.Done()
			c.Version, _ = health(ctx, r.deps.HTTP, c.URL)
		}(&cs[i])
	}
	wg.Wait()
	for i := range cs {
		c := &cs[i]
		if c.Ours && !IsRelease(c.Version) {
			c.Ours = false
			if c.Version == "" {
				c.Reason = "it does not answer at " + c.URL + "/health"
			} else {
				c.Reason = fmt.Sprintf("it answers %q, not a release", c.Version)
			}
		}
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
	return cs, ""
}

func (r *runner) inspectContainers(ctx context.Context, refs ...string) ([]inspectContainer, error) {
	out, err := r.deps.Docker.Run(ctx, append([]string{"container", "inspect"}, refs...)...)
	if err != nil {
		return nil, err
	}
	var list []inspectContainer
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("docker inspect: %w", err)
	}
	var raws []map[string]json.RawMessage
	if json.Unmarshal(out, &raws) == nil && len(raws) == len(list) {
		for i := range list {
			list[i].raw = raws[i]
		}
	}
	return list, nil
}

func (r *runner) inspectImage(ctx context.Context, ref string) (inspectImage, error) {
	out, err := r.deps.Docker.Run(ctx, "image", "inspect", ref)
	if err != nil {
		return inspectImage{}, err
	}
	var list []inspectImage
	if err := json.Unmarshal(out, &list); err != nil || len(list) != 1 {
		return inspectImage{}, errors.New("docker image inspect: not one image")
	}
	return list[0], nil
}

// exists says whether docker knows a container or a volume by that name.
func (r *runner) exists(ctx context.Context, kind, name string) bool {
	_, err := r.deps.Docker.Run(ctx, kind, "inspect", name)
	return err == nil
}

// volumeKB is how much a volume holds, measured from a busybox container.
func (r *runner) volumeKB(ctx context.Context, volume string) (int64, error) {
	out, err := r.deps.Docker.Run(ctx, "run", "--rm", "--mount", csvField("type=volume", "src="+volume, "dst=/data", "readonly"),
		busybox, "du", "-sk", "/data")
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return 0, errors.New("du said nothing")
	}
	return strconv.ParseInt(fields[0], 10, 64)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
