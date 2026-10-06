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
	"path/filepath"
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
	Hostname     string              `json:"Hostname"`
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

type portBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

type mount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Driver      string `json:"Driver"`
	Mode        string `json:"Mode"`
	RW          bool   `json:"RW"`
	Propagation string `json:"Propagation"`
}

// inspectContainer is the part of `docker inspect` the command reads. The
// fields under "refused" are settings a recreate does not reproduce, which
// make a container the person's when set; raw is the whole inspect, for every
// other setting (unreproduced).
type inspectContainer struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
	// Image is the image's ID, which `docker image inspect` reads.
	Image string `json:"Image"`
	State struct {
		Running    bool   `json:"Running"`
		Restarting bool   `json:"Restarting"`
		StartedAt  string `json:"StartedAt"`
	} `json:"State"`
	RestartCount int           `json:"RestartCount"`
	Config       inspectConfig `json:"Config"`
	HostConfig   struct {
		PortBindings  map[string][]portBinding `json:"PortBindings"`
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
		NetworkMode     string `json:"NetworkMode"`
		PublishAllPorts bool   `json:"PublishAllPorts"`
		LogConfig       struct {
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
		UsernsMode     string            `json:"UsernsMode"`
		CgroupnsMode   string            `json:"CgroupnsMode"`
		Init           *bool             `json:"Init"`
	} `json:"HostConfig"`
	// NetworkSettings.Ports are the bindings as Docker made them: a port
	// asked for as any (`-p 127.0.0.1::4318`) has its number only here.
	NetworkSettings struct {
		Ports map[string][]portBinding `json:"Ports"`
	} `json:"NetworkSettings"`
	Mounts []mount `json:"Mounts"`
	raw    map[string]json.RawMessage
}

// imageConfig is the part of `docker image inspect` the command reads.
type imageConfig struct {
	ID     string        `json:"Id"`
	Config inspectConfig `json:"Config"`
}

// dockerInfo is what the daemon says of itself that a recreate depends on:
// whether an archive written in a container is the host user's to read
// (remapped says why not), and the log driver a container gets unless told.
type dockerInfo struct {
	remapped  string
	logDriver string
}

// Container is a container running Tracepad's image: the command's when
// Ours (Decision 4, spec 054 #47), the person's otherwise, with the reason
// and the commands that upgrade it.
type Container struct {
	Name string
	// Ref is the image reference it was created from; Repo is that without
	// the tag.
	Ref, Repo string
	Volume    string
	URL       string
	Version   string
	Ours      bool
	// Compose is its Compose project, when Compose made it; Service is its
	// service there, ComposeFiles the files Compose read for it and
	// ComposeDir its project directory, all as Compose labelled it (the live
	// run of 0.1.0: the person's commands said neither the file nor where to
	// run them from).
	Compose      string
	Service      string
	ComposeFiles []string
	ComposeDir   string
	Reason       string
	// Default is whether it publishes the default port, 4318, on the host.
	Default bool
	// Unchecked says why the plan could not tell where to ask it, when it
	// could not: a container not checked is said, not called behind (the
	// final review).
	Unchecked string
	// DataMount is its /data as busybox's --mount takes it, empty when it
	// has none.
	DataMount string
	// Run is how it was created, as `docker run` takes it — the options,
	// the image's place (imageSlot), its command — and EnvNames the
	// variables it was given, whose values stay in Docker: the plan names
	// them and never prints one. Run is nil when the image's own
	// configuration could not be read. One function writes it, for the
	// person's commands and the command's own recreate alike (#47).
	Run      []string
	EnvNames []string
	// RunWhy says why Run could not be written, when it could not.
	RunWhy string
	// Unreproduced are the settings it has that Run does not carry.
	Unreproduced []string
	// freeKB is the room on its volume's file system, as the preparation's
	// look found it: for that preparation's own check alone.
	freeKB  int64
	inspect inspectContainer
	image   imageConfig
}

// imageSlot stands for the image in Container.Run.
const imageSlot = "\x00image"

// createdAs is how a container was created, read back from its inspect and its
// image's (the live run of rc.3: "the options it was created with" left a
// person to find them): its published ports, its mounts, its restart
// policy, its network, its labels, its log driver (not its options), its
// user, and the command and variables it was given beyond the image's own. logDriver is the
// daemon's default, "" when it could not be read. The person's commands and
// the command's own recreate are both written from it (#47).
func createdAs(ic inspectContainer, img imageConfig, logDriver string) (run, envNames []string, err error) {
	hc := ic.HostConfig
	if hc.PublishAllPorts {
		run = append(run, "-P")
	}
	keys := make([]string, 0, len(hc.PortBindings))
	for k := range hc.PortBindings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		port, proto, _ := strings.Cut(k, "/")
		for _, b := range hc.PortBindings[k] {
			spec := b.HostPort + ":" + port
			if b.HostIP != "" {
				host := b.HostIP
				if strings.Contains(host, ":") {
					host = "[" + host + "]"
				}
				spec = host + ":" + spec
			}
			if proto != "" && proto != "tcp" {
				spec += "/" + proto
			}
			run = append(run, "-p", spec)
		}
	}
	for _, m := range ic.Mounts {
		switch m.Type {
		case "volume":
			run = append(run, "--mount", mountArg(m, m.Name))
		case "bind":
			// --mount, not -v: a path with a colon or a comma comes through
			// as it is, and a source that is gone is refused, not made.
			run = append(run, "--mount", mountArg(m, m.Source))
		case "tmpfs":
			run = append(run, "--tmpfs", m.Destination)
		default:
			// Never another mount in its place (the review of #225).
			return nil, nil, fmt.Errorf("the command cannot write a mount of type %s (on %s) into a docker run", m.Type, m.Destination)
		}
	}
	if rp := hc.RestartPolicy; rp.Name != "" && rp.Name != "no" {
		run = append(run, "--restart", restartArg(rp.Name, rp.MaximumRetryCount))
	}
	if n := hc.NetworkMode; n != "" && n != "default" && n != "bridge" {
		run = append(run, "--network", n)
	}
	// The namespaces' modes as they are, whatever the daemon's defaults:
	// the daemon does not say its IPC default, and a mode written out is
	// the same on any (the audit of #226: --cgroupns host and --ipc
	// shareable were dropped as "the daemon's own"). Sharing another's, or
	// the host's IPC, is refused by name.
	if m := hc.CgroupnsMode; m == "host" || m == "private" {
		run = append(run, "--cgroupns", m)
	}
	if m := hc.IpcMode; m == "private" || m == "shareable" || m == "none" {
		run = append(run, "--ipc", m)
	}
	for _, l := range personLabels(ic.Config.Labels, img.Config.Labels) {
		run = append(run, "--label", l)
	}
	// A log driver of its own is carried, and of its options those that
	// hold no credential (logOpts): a token would go on a command line and
	// into the plan (the security review of #47), and a container with such
	// an option is the person's, named. The daemon merges its own default
	// options into every container of its default driver (moby's
	// mergeAndVerifyLogConfig), so a host whose daemon.json sets max-size
	// shows them on every container: they are carried as they are, and the
	// recreate is the same on any daemon (the review of #226).
	if lc := hc.LogConfig; lc.Type != "" && lc.Type != logDriver {
		run = append(run, "--log-driver", lc.Type)
	}
	opts := make([]string, 0, len(hc.LogConfig.Config))
	for k, v := range hc.LogConfig.Config {
		if logOpts[k] {
			opts = append(opts, k+"="+v)
		}
	}
	sort.Strings(opts)
	for _, o := range opts {
		run = append(run, "--log-opt", o)
	}
	if ic.Config.User != img.Config.User {
		run = append(run, "--user", ic.Config.User)
	}
	args := ic.Config.Cmd
	switch {
	case slices.Equal(ic.Config.Entrypoint, img.Config.Entrypoint):
		if slices.Equal(args, img.Config.Cmd) {
			args = nil
		}
	case len(ic.Config.Entrypoint) == 0:
		// Cleared at its creation (--entrypoint ""), and cleared again.
		run = append(run, "--entrypoint", "")
	default:
		run = append(run, "--entrypoint", ic.Config.Entrypoint[0])
		args = append(slices.Clone(ic.Config.Entrypoint[1:]), args...)
	}
	run = append(run, imageSlot)
	run = append(run, args...)
	for _, kv := range ic.Config.Env {
		if name, _, ok := strings.Cut(kv, "="); ok && !slices.Contains(img.Config.Env, kv) && !slices.Contains(envNames, name) {
			envNames = append(envNames, name)
		}
	}
	sort.Strings(envNames)
	return run, envNames, nil
}

// csvField writes a --mount field the way docker reads it: as CSV, quoted
// when it holds a comma or a quote.
func csvField(fields ...string) string {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	_ = w.Write(fields) // ignored: a bytes.Buffer takes every write; Flush reports none either
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

// personLabels are the labels set on the container, not inherited from its
// image.
func personLabels(container, image map[string]string) []string {
	var out []string
	for k, v := range container {
		if iv, ok := image[k]; (ok && iv == v) || strings.HasPrefix(k, runLabel+".") || k == runLabel {
			continue
		}
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// runLabel marks a container the command made: the run that made it, and
// (runLabel.step) the step its start records. A container under the run's
// name is the run's own only with them (the review of #226): one the person
// made, of the same image, is theirs, and refused with nothing touched. A
// recreate does not carry them over (personLabels).
const runLabel = "org.tracepad.upgrade.run"

// runArgs is the command's own `docker run` for a container, from Run (the
// plan's, createdAs): its name, its variables through envFile, the image ref,
// and — when volume is given — that volume at /data instead of its own; the
// run and step that make it, as labels. Never through a shell: an argument
// vector has no word splitting.
func runArgs(c Container, name, ref, envFile, volume, run, step string) []string {
	args := []string{"run", "-d", "--name", name, "--env-file", envFile,
		"--label", runLabel + "=" + run, "--label", runLabel + ".step=" + step}
	from, to := "", ""
	for _, m := range c.inspect.Mounts {
		if m.Destination == "/data" && m.Type == "volume" && volume != "" {
			from, to = mountArg(m, m.Name), mountArg(m, volume)
		}
	}
	for i, a := range c.Run {
		switch {
		case a == imageSlot:
			a = ref
		case from != "" && a == from && i > 0 && c.Run[i-1] == "--mount":
			a = to
		}
		args = append(args, a)
	}
	return args
}

// How a recreate treats each field of a container's inspect (the review of
// #226): every field the command reads, or the daemon sets, is in exactly one
// of these, and a test holds the command's types to them. A field in none is
// one the recreate does not know: set, it makes the container the person's,
// named (unreproduced).
var (
	// hostReproduced are carried by createdAs: the bindings, the mounts
	// (what a mount has beyond them is named, mountLosses), the restart
	// policy, the network, the log driver (its options are named), the
	// cgroup and IPC namespaces' modes.
	hostReproduced = setOf("RestartPolicy", "PortBindings", "NetworkMode", "LogConfig", "Binds", "Mounts", "CgroupnsMode", "IpcMode")
	// hostRefused make a container the person's by name (refusedSettings
	// and containerRefusal).
	hostRefused = setOf("AutoRemove", "Privileged", "ReadonlyRootfs", "CapAdd", "CapDrop", "Devices", "DeviceRequests",
		"ExtraHosts", "Links", "VolumesFrom", "GroupAdd", "Dns", "DnsSearch", "SecurityOpt", "Tmpfs", "Sysctls", "Ulimits",
		"Memory", "NanoCpus", "CpuShares", "CpuQuota", "PidMode", "UTSMode", "UsernsMode", "Init", "PublishAllPorts")
	// hostDaemon are what the daemon gives every container, each with the
	// check that the value is the daemon's own and not the person's.
	hostDaemon = map[string]func(raw json.RawMessage) bool{
		// The paths the daemon masks: emptied only by a security option,
		// which is refused by name.
		"MaskedPaths":   func(raw json.RawMessage) bool { return !unset(raw) },
		"ReadonlyPaths": func(raw json.RawMessage) bool { return !unset(raw) },
		// The terminal of the client that made it: a run with -t is named
		// by Config.Tty.
		"ConsoleSize":      func(json.RawMessage) bool { return true },
		"Isolation":        func(raw json.RawMessage) bool { return unset(raw) || string(compactJSON(raw)) == `"default"` },
		"ContainerIDFile":  unset,
		"ShmSize":          func(raw json.RawMessage) bool { return unset(raw) || string(compactJSON(raw)) == "67108864" },
		"Runtime":          func(raw json.RawMessage) bool { return unset(raw) || string(compactJSON(raw)) == `"runc"` },
		"MemorySwappiness": func(raw json.RawMessage) bool { return unset(raw) || string(compactJSON(raw)) == "-1" },
	}
	// configReproduced are carried by createdAs, or checked to be what a
	// recreate makes again (the hostname docker derives, the ports the image
	// exposes).
	configReproduced = setOf("Env", "Cmd", "Image", "User", "Labels", "Entrypoint", "ExposedPorts", "Hostname")
	// configRefused make it the person's by name.
	configRefused = setOf("WorkingDir", "Healthcheck", "StopSignal")
	// configDaemon are the image's, or how a client attached when it was
	// made, not how it runs.
	configDaemon = setOf("Volumes", "ArgsEscaped", "AttachStdout", "AttachStderr")
)

// logOpts are the log options a recreate carries: the rotation, compression
// and tagging of the json-file and local drivers, which hold no credential.
var logOpts = setOf("max-size", "max-file", "compress", "mode", "max-buffer-size", "tag", "labels", "labels-regex", "env", "env-regex")

func setOf(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

func unset(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", "false", "0", `""`, "[]", "{}":
		return true
	}
	return false
}

// unreproduced names every field of a container's inspect that is set and
// that its run (createdAs) does not carry: a container with any is the
// person's, and the plan names them.
func unreproduced(ic inspectContainer, img imageConfig) []string {
	var out []string
	section := func(key string) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(ic.raw[key], &m) // ignored: a section that does not read names nothing here; the typed fields above it are read all the same
		return m
	}
	for k, v := range section("HostConfig") {
		switch daemons, ok := hostDaemon[k]; {
		case hostReproduced[k], hostRefused[k]:
		case ok:
			if !daemons(v) {
				out = append(out, "HostConfig."+k)
			}
		case !unset(v):
			out = append(out, "HostConfig."+k)
		}
	}
	for k, v := range section("Config") {
		if configReproduced[k] || configRefused[k] || configDaemon[k] || unset(v) {
			continue
		}
		out = append(out, "Config."+k)
	}
	for k := range ic.HostConfig.LogConfig.Config {
		if !logOpts[k] {
			out = append(out, "HostConfig.LogConfig.Config["+k+"]")
		}
	}
	out = append(out, mountLosses(ic)...)
	if h := ic.Config.Hostname; h != "" && (len(ic.ID) < 12 || h != ic.ID[:12]) {
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

// mountLosses names what a mount has that --mount type,src,dst[,readonly]
// does not carry (the review of #226): a bind's relabel (z, Z) or
// propagation, a volume's mode beyond docker's own "z", and every option of
// a --mount the container was created with — nocopy, a subpath, a bind's
// recursion, a tmpfs's size. As docker shows them: a volume's mode is "z"
// and a bind's propagation "rprivate" unless told otherwise.
func mountLosses(ic inspectContainer) []string {
	var out []string
	for _, m := range ic.Mounts {
		for _, opt := range strings.Split(m.Mode, ",") {
			switch {
			case opt == "", opt == "rw", opt == "ro":
			case opt == "z" && m.Type == "volume":
			default:
				out = append(out, fmt.Sprintf("Mounts[%s].Mode=%s", m.Destination, m.Mode))
			}
		}
		if p := m.Propagation; p != "" && p != "rprivate" {
			out = append(out, fmt.Sprintf("Mounts[%s].Propagation=%s", m.Destination, p))
		}
	}
	var hc struct {
		Binds  []string                     `json:"Binds"`
		Mounts []map[string]json.RawMessage `json:"Mounts"`
	}
	_ = json.Unmarshal(ic.raw["HostConfig"], &hc) // ignored: a section that does not read names nothing here; Mounts above are read all the same
	// A -v's options, as it was given them: src:dst[:opts].
	for _, b := range hc.Binds {
		parts := strings.Split(b, ":")
		if len(parts) < 3 {
			continue
		}
		for _, opt := range strings.Split(parts[len(parts)-1], ",") {
			if opt != "ro" && opt != "rw" {
				out = append(out, fmt.Sprintf("HostConfig.Binds[%s]=%s", parts[len(parts)-2], parts[len(parts)-1]))
				break
			}
		}
	}
	for _, m := range hc.Mounts {
		var target string
		_ = json.Unmarshal(m["Target"], &target) // ignored: the name's only; an empty one still names the option
		for _, k := range []string{"BindOptions", "VolumeOptions", "TmpfsOptions", "ImageOptions", "ClusterOptions"} {
			if v, ok := m[k]; ok && !unset(v) {
				out = append(out, fmt.Sprintf("HostConfig.Mounts[%s].%s", target, k))
			}
		}
		if v, ok := m["Consistency"]; ok && !unset(v) && string(compactJSON(v)) != `"default"` {
			out = append(out, fmt.Sprintf("HostConfig.Mounts[%s].Consistency", target))
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
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

// containerRefusal says why a container of the image is not the command's to
// recreate (Decision 4, #47), empty when it is: the recreate is exactly the
// container again only under these conditions, and outside them a recreate
// is a guess about someone's deployment. named is the --container the person
// gave. A container under Compose is said apart (Compose).
func containerRefusal(c Container, named string, info dockerInfo) string {
	ic, hc := c.inspect, c.inspect.HostConfig
	for key := range ic.Config.Labels {
		switch {
		case strings.HasPrefix(key, "com.docker.swarm."):
			return "a Swarm service runs it"
		case strings.HasPrefix(key, "io.kubernetes."):
			return "Kubernetes runs it"
		}
	}
	switch {
	case c.Compose != "":
		return "Compose runs it (project " + c.Compose + "): its file is yours"
	case named != c.Name && !strings.HasPrefix(c.Name, "tracepad-"):
		return "its name is not tracepad-<project>, as setup names one: name it with --container to upgrade it"
	case !ic.State.Running || ic.State.Restarting:
		return "it is not running"
	case c.Run == nil:
		return "its docker run could not be written: " + c.RunWhy
	case info.remapped != "":
		return info.remapped + ", and the command cannot read back an archive written in a container there: back up its volume and recreate it yourself"
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
	case data.Driver != "" && data.Driver != "local":
		return "its volume " + data.Name + " has the driver " + data.Driver + ", and a way back's restore would go into a local volume"
	}
	for _, m := range ic.Mounts {
		if m.Type == "tmpfs" {
			return "it has a tmpfs mount at " + m.Destination + ", whose options a recreate would not carry"
		}
	}
	for port, binds := range hc.PortBindings {
		for _, b := range binds {
			if b.HostPort == "" || b.HostPort == "0" {
				// A recreate, or a start of it again, would publish it on
				// another port than the one it answers on now (the review of
				// #226).
				return fmt.Sprintf("it publishes %s on a port docker chooses at each start, which a recreate would move: publish it on a fixed port", port)
			}
			if _, ok := loopbackBase(b.HostIP, "1"); !ok {
				ip := b.HostIP
				if ip == "" {
					ip = "every address"
				}
				return fmt.Sprintf("it publishes %s on %s, beyond this machine", port, ip)
			}
		}
	}
	if hc.PublishAllPorts {
		return "it publishes every port it exposes (-P), on every address"
	}
	if c.URL == "" {
		return "where its server answers on this machine cannot be told: " + c.Unchecked
	}
	if n := hc.NetworkMode; n != "" && n != "default" && n != "bridge" {
		return "it is on the network " + n
	}
	if rs := refusedSettings(ic, c.image); len(rs) > 0 {
		return rs[0].what
	}
	if !slices.Equal(ic.Config.Entrypoint, c.image.Config.Entrypoint) {
		return "its entrypoint is not the image's, and a recreate on the next image would pin this one's"
	}
	if len(c.Unreproduced) > 0 {
		return "it has settings the command does not reproduce (" + strings.Join(c.Unreproduced, ", ") + "): back up its volume and recreate it yourself"
	}
	for _, kv := range ic.Config.Env {
		if strings.ContainsAny(kv, "\r\n") {
			k, _, _ := strings.Cut(kv, "=")
			return "its variable " + k + " holds a line break, which an env file cannot carry"
		}
	}
	return ""
}

// setting is a setting a container has that its run does not carry: the
// field of the inspect, and why that makes it the person's.
type setting struct{ field, what string }

// refusedSettings are the settings a container has that its run does not
// carry and that make it the person's by name: the command refuses them,
// and the person's commands name them (the review of #226).
func refusedSettings(ic inspectContainer, img imageConfig) []setting {
	hc := ic.HostConfig
	all := []struct {
		set bool
		setting
	}{
		{hc.AutoRemove, setting{"HostConfig.AutoRemove", "it was started with --rm, so stopping it removes it"}},
		{hc.Privileged, setting{"HostConfig.Privileged", "it is privileged"}},
		{hc.ReadonlyRootfs, setting{"HostConfig.ReadonlyRootfs", "its root file system is read-only"}},
		{len(hc.CapAdd)+len(hc.CapDrop) > 0, setting{"HostConfig.CapAdd/CapDrop", "it has capabilities of its own"}},
		{len(hc.Devices)+len(hc.DeviceRequests) > 0, setting{"HostConfig.Devices", "it has devices"}},
		{len(hc.ExtraHosts) > 0, setting{"HostConfig.ExtraHosts", "it has extra hosts"}},
		{len(hc.Links)+len(hc.VolumesFrom) > 0, setting{"HostConfig.Links/VolumesFrom", "it is linked to other containers"}},
		{len(hc.GroupAdd) > 0, setting{"HostConfig.GroupAdd", "it has groups of its own"}},
		{len(hc.DNS)+len(hc.DNSSearch) > 0, setting{"HostConfig.Dns", "it has DNS settings of its own"}},
		{len(hc.SecurityOpt) > 0, setting{"HostConfig.SecurityOpt", "it has security options"}},
		{len(hc.Tmpfs) > 0, setting{"HostConfig.Tmpfs", "it has tmpfs mounts"}},
		{len(hc.Sysctls)+len(hc.Ulimits) > 0, setting{"HostConfig.Sysctls/Ulimits", "it has kernel settings or limits of its own"}},
		{hc.Memory+hc.NanoCpus+hc.CPUShares+hc.CPUQuota > 0, setting{"HostConfig.Memory/NanoCpus/CpuShares/CpuQuota", "it has resource limits"}},
		{strings.HasPrefix(hc.PidMode, "host") || strings.HasPrefix(hc.PidMode, "container:"), setting{"HostConfig.PidMode", "it shares a PID namespace"}},
		{strings.HasPrefix(hc.IpcMode, "host") || strings.HasPrefix(hc.IpcMode, "container:"), setting{"HostConfig.IpcMode", "it shares an IPC namespace"}},
		{hc.UTSMode != "", setting{"HostConfig.UTSMode", "it shares a UTS namespace"}},
		{hc.UsernsMode != "", setting{"HostConfig.UsernsMode", "it has a user namespace of its own"}},
		{hc.Init != nil && *hc.Init, setting{"HostConfig.Init", "it runs an init"}},
		{ic.Config.WorkingDir != img.Config.WorkingDir, setting{"Config.WorkingDir", "its working directory is not the image's"}},
		{!bytes.Equal(compactJSON(ic.Config.Healthcheck), compactJSON(img.Config.Healthcheck)), setting{"Config.Healthcheck", "its health check is not the image's"}},
		{ic.Config.StopSignal != img.Config.StopSignal, setting{"Config.StopSignal", "its stop signal is not the image's"}},
	}
	var out []setting
	for _, s := range all {
		if s.set {
			out = append(out, s.setting)
		}
	}
	return out
}

// containerAddress is where a container's server answers on this machine:
// the port it listens on inside, read as the server reads its listen
// address (its arguments, then TRACEPAD_LISTEN, then the default), published
// on the host. A binding on every address is asked on loopback. why says
// what kept the address from being told.
func containerAddress(ic inspectContainer) (url string, onDefault bool, why string) {
	p := Process{Argv: append(slices.Clone(ic.Config.Entrypoint), ic.Config.Cmd...), Env: ic.Config.Env}
	_, listen, err := configuredDirs(p)
	if err != nil {
		return "", false, "what it runs is not a server's command line Tracepad can read (" + strings.Join(p.Argv, " ") + ")"
	}
	_, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", false, fmt.Sprintf("its listen address %q has no port the plan can read", listen)
	}
	bindings := ic.NetworkSettings.Ports[port+"/tcp"]
	if len(bindings) == 0 {
		bindings = ic.HostConfig.PortBindings[port+"/tcp"]
	}
	for _, b := range bindings {
		onDefault = onDefault || b.HostPort == "4318"
		if u, ok := loopbackBase(onLoopback(b.HostIP), b.HostPort); ok && url == "" {
			url = u
		}
	}
	if url == "" {
		return "", onDefault, fmt.Sprintf("its server listens on port %s inside, which it does not publish on this machine's loopback", port)
	}
	return url, onDefault, ""
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

// info asks the daemon what a recreate depends on (dockerInfo). Not asked is
// never "not remapped" (the eighth review): every container is then the
// person's, said.
func (r *runner) info(ctx context.Context) dockerInfo {
	out, err := r.deps.Docker.Run(ctx, "info", "--format", "{{json .SecurityOptions}} {{json .LoggingDriver}}")
	if err != nil {
		return dockerInfo{remapped: "Docker did not say whether it runs rootless (" + firstLine(err.Error()) + ")"}
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	var opts []string
	var driver string
	if dec.Decode(&opts) != nil || dec.Decode(&driver) != nil {
		return dockerInfo{remapped: "Docker's account of itself did not read"}
	}
	// Rootless Docker, or user namespaces remapped: root in a container is
	// not the host's, and an archive written there is not the host user's to
	// read back. The command does not try; the person does it (the third
	// review).
	info := dockerInfo{logDriver: driver}
	for _, o := range opts {
		switch {
		case strings.Contains(o, "name=rootless"):
			info.remapped = "Docker runs rootless"
		case strings.Contains(o, "name=userns"):
			info.remapped = "Docker remaps user namespaces"
		}
	}
	return info
}

// containers lists the running containers of Tracepad's image, each with its
// version when it answers on this machine, and whether it is the command's.
// note says when Docker could not be asked; an error is never taken for "no
// containers".
func (r *runner) containers(ctx context.Context) ([]Container, string) {
	if r.deps.Docker == nil {
		// Said, not taken for "no containers" (the audit of #223).
		return nil, "docker is not on PATH, so containers were not looked for"
	}
	out, err := r.deps.Docker.Run(ctx, "ps", "-q", "--no-trunc")
	if err != nil {
		return nil, "Docker did not answer, so containers were not checked (" + firstLine(err.Error()) + ")"
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil, ""
	}
	list, err := r.inspectContainers(ctx, ids...)
	if err != nil {
		return nil, "the containers could not be inspected, so they were not checked (" + firstLine(err.Error()) + ")"
	}
	var cs []Container
	for _, ic := range list {
		if c, ok := asContainer(ic); ok {
			cs = append(cs, c)
		}
	}
	if len(cs) == 0 {
		return nil, ""
	}
	info := r.info(ctx)
	// Its image's own configuration tells what each was given beyond it:
	// every image asked once, in one call, and none for Compose's, whose
	// advice is its Compose file (the review of #225). One that cannot be
	// read leaves the run to the person, said.
	var imageIDs []string
	for _, c := range cs {
		if c.Compose == "" && !slices.Contains(imageIDs, c.inspect.Image) {
			imageIDs = append(imageIDs, c.inspect.Image)
		}
	}
	images, imgErr := r.inspectImages(ctx, imageIDs)
	for i := range cs {
		if cs[i].Compose == "" {
			r.recreate(&cs[i], images, imgErr, info)
		}
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
			if v, err := health(ctx, r.deps.HTTP, c.URL); err == nil {
				c.Version = v
			}
		}(&cs[i])
	}
	wg.Wait()
	for i := range cs {
		c := &cs[i]
		c.Reason = containerRefusal(*c, r.flags.container, info)
		switch {
		case c.Reason != "":
		case c.Version == "":
			c.Reason = "it does not answer at " + c.URL + "/health"
		case !IsRelease(c.Version):
			c.Reason = fmt.Sprintf("it answers %q, not a release", c.Version)
		default:
			c.Ours = true
		}
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
	return cs, ""
}

// asContainer reads a container of the image from its inspect; ok is false
// for any other image's.
func asContainer(ic inspectContainer) (Container, bool) {
	repo, ok := imageRepo(ic.Config.Image)
	if !ok {
		return Container{}, false
	}
	labels := ic.Config.Labels
	c := Container{Name: strings.TrimPrefix(ic.Name, "/"), Ref: ic.Config.Image, Repo: repo,
		Compose: labels["com.docker.compose.project"], Service: labels["com.docker.compose.service"],
		ComposeDir: labels["com.docker.compose.project.working_dir"], inspect: ic}
	// Compose writes the files it read joined by commas.
	if files := labels["com.docker.compose.project.config_files"]; files != "" {
		c.ComposeFiles = strings.Split(files, ",")
	}
	for _, m := range ic.Mounts {
		switch {
		case m.Destination != "/data":
		case m.Type == "volume":
			c.Volume, c.DataMount = m.Name, "type=volume,src="+m.Name
		case m.Type == "bind":
			c.DataMount = "type=bind,src=" + m.Source
		}
	}
	c.URL, c.Default, c.Unchecked = containerAddress(ic)
	return c, true
}

// recreate writes a container's run from its image's configuration, or says
// why it cannot.
func (r *runner) recreate(c *Container, images map[string]imageConfig, imgErr error, info dockerInfo) {
	img, ok := images[c.inspect.Image]
	if !ok {
		c.RunWhy = "its image could not be read"
		if imgErr != nil {
			c.RunWhy += " (" + firstLine(imgErr.Error()) + ")"
		}
		return
	}
	c.image = img
	var err error
	if c.Run, c.EnvNames, err = createdAs(c.inspect, img, info.logDriver); err != nil {
		c.Run, c.RunWhy = nil, err.Error()
		return
	}
	c.Unreproduced = unreproduced(c.inspect, img)
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
	if err := json.Unmarshal(out, &raws); err != nil || len(raws) != len(list) {
		return nil, fmt.Errorf("docker inspect: its answer does not read whole: %v", err)
	}
	for i := range list {
		list[i].raw = raws[i]
	}
	return list, nil
}

// inspectOne is one container, by name or id. Not there is (nil, nil); a
// daemon that does not answer is an error, never "not there" (the eighth
// review: a failed inspect was read as "the new container never started").
func (r *runner) inspectOne(ctx context.Context, ref string) (*inspectContainer, error) {
	list, err := r.inspectContainers(ctx, ref)
	switch {
	case err != nil && strings.Contains(err.Error(), "No such container"):
		return nil, nil
	case err != nil:
		return nil, err
	case len(list) != 1:
		return nil, fmt.Errorf("docker inspect %s answered %d containers", ref, len(list))
	}
	return &list[0], nil
}

// inspectImages reads the images' own configurations, by ID, in one call.
func (r *runner) inspectImages(ctx context.Context, ids []string) (map[string]imageConfig, error) {
	images := map[string]imageConfig{}
	if len(ids) == 0 {
		return images, nil
	}
	out, err := r.deps.Docker.Run(ctx, append([]string{"image", "inspect"}, ids...)...)
	if err != nil {
		return images, err
	}
	var list []imageConfig
	if err := json.Unmarshal(out, &list); err != nil {
		return images, fmt.Errorf("docker image inspect: %w", err)
	}
	for _, img := range list {
		images[img.ID] = img
	}
	return images, nil
}

// containerAdvice is what upgrades a container, as docker.md does it: stop
// it, back its volume up, pull the release, and run it again with the
// options it was created with, the old one kept under another name until the
// new one has proved itself.
func containerAdvice(c Container, to string) string {
	image := c.Repo + ":" + to
	if c.Compose != "" {
		return composeAdvice(c, to)
	}
	name, old := shq(c.Name), shq(c.Name+"-old")
	chain, after := containerSteps(c, to)
	if c.Run == nil {
		return fmt.Sprintf("Upgrade it yourself (%s), each step only once the one before it worked: %s. Then run %s as %s with the options %s was created with, which the command could not write (%s; docker inspect %s has them), and remove %s once it is healthy. %s",
			docsDocker, chain, image, name, name, c.RunWhy, old, old, strings.Join(after, ". "))
	}
	advice := fmt.Sprintf("Upgrade it yourself (%s), as one command — a step that fails stops the ones after it: %s. %s",
		docsDocker, chain, strings.Join(after, ". "))
	if lost := notCarried(c); len(lost) > 0 {
		advice += fmt.Sprintf(". It was also given what this docker run does not carry — %s — which docker inspect %s shows: add it to the run", strings.Join(lost, ", "), old)
	}
	return advice
}

// composeAdvice is how a container Compose made is upgraded: through its
// Compose file, which is the person's, after a backup of the volume it
// really has (the live run of 0.1.0: "back up its volume as docs/docker.md
// shows" was copied with docker.md's volume name, `tracepad`, where Compose
// had named it `<project>_<volume>`, and `docker run` made an empty volume
// of that name and archived it). The commands name the project and its file,
// so they work from any directory and never start another project; an image
// pinned by digest is said, since a new tag before the old digest changes
// nothing.
func composeAdvice(c Container, to string) string {
	image := c.Repo + ":" + to
	compose := composeCommand(c)
	service := "<its service>"
	if c.Service != "" {
		service = shq(c.Service)
	}
	where := "the project " + shq(c.Compose) + "'s Compose file"
	switch len(c.ComposeFiles) {
	case 0:
	case 1:
		where = c.ComposeFiles[0]
	default:
		where = "whichever of " + strings.Join(c.ComposeFiles, ", ") + " sets it"
	}
	set := fmt.Sprintf("set the image of the service %s in %s to %s", service, where, image)
	if _, digest, ok := strings.Cut(c.Ref, "@"); ok {
		set += fmt.Sprintf(" — and take its digest off: it is pinned to %s, which Docker runs whatever the tag says. To pin the new one, docker pull %s prints its digest: %s@<that digest>", digest, image, image)
	}
	version := c.Version
	if version == "" {
		version = "backup"
	}
	data := c.DataMount
	if data == "" {
		data = "type=volume,src=<its /data volume>"
	}
	chain := strings.Join([]string{
		compose + " stop " + service,
		fmt.Sprintf("docker run --rm --mount %s,dst=/data,readonly -v \"$PWD:/backup\" %s sh -c 'umask 077 && set -C && tar czf - -C /data . > /backup/%s-%s.tar.gz'", shq(data), busybox, c.Name, version),
		"docker pull " + image,
	}, " && ")
	return fmt.Sprintf("Upgrade it with Compose (%s). First, as one command — a step that fails stops the ones after it: %s. Then %s, and: %s up -d %s. Stopped before the image was set: %s start %s",
		docsDocker, chain, set, compose, service, compose, service)
}

// composeCommand is `docker compose` for a container's project, from any
// directory: its name, its files, and its directory when that is not the
// first file's.
func composeCommand(c Container) string {
	cmd := "docker compose -p " + shq(c.Compose)
	if c.ComposeDir != "" && (len(c.ComposeFiles) == 0 || c.ComposeDir != filepath.Dir(c.ComposeFiles[0])) {
		cmd += " --project-directory " + shq(c.ComposeDir)
	}
	for _, f := range c.ComposeFiles {
		cmd += " -f " + shq(f)
	}
	return cmd
}

// notCarried names every setting a container has that its run does not
// carry: those refused by name, and those unreproduced names (the review of
// #226: the person's commands said nothing of a privilege or a limit).
func notCarried(c Container) []string {
	var lost []string
	for _, s := range refusedSettings(c.inspect, c.image) {
		lost = append(lost, s.field)
	}
	return append(lost, c.Unreproduced...)
}

// containerSteps are a container's upgrade as the person runs it: one
// chain, each step only once the one before it worked (the review of #225:
// a backup refused, or a file of variables left from before, stopped
// nothing after it), and the sentences for after it.
func containerSteps(c Container, to string) (chain string, after []string) {
	image := c.Repo + ":" + to
	version := c.Version
	if version == "" {
		version = "backup"
	}
	data := c.DataMount
	if data == "" {
		data = "type=volume,src=<its /data volume>"
	}
	name, old := shq(c.Name), shq(c.Name+"-old")
	steps := []string{
		"docker stop " + name,
		// set -C: an earlier backup of the same name is never written over.
		fmt.Sprintf("docker run --rm --mount %s,dst=/data,readonly -v \"$PWD:/backup\" %s sh -c 'umask 077 && set -C && tar czf - -C /data . > /backup/%s-%s.tar.gz'", shq(data), busybox, c.Name, version),
		"docker pull " + image,
		fmt.Sprintf("docker rename %s %s", name, old),
	}
	if c.Run == nil {
		return strings.Join(steps, " && "), []string{"Stopped before the rename: docker start " + name}
	}
	// The variables it was given go into a file of their own, read from
	// Docker and never printed (the live run of rc.3): they hold its keys.
	// A name of the upgrade's own, never overwritten (set -C): docs/docker.md
	// keeps an admin token in tracepad.env, which this file must not take.
	run := []string{"docker run -d --name", name}
	env := shq(c.Name + ".upgrade.env")
	if len(c.EnvNames) > 0 {
		steps = append(steps, fmt.Sprintf("(umask 077 && set -C && docker inspect --format '{{range .Config.Env}}{{println .}}{{end}}' %s | grep -E '^(%s)=' > %s)", old, strings.Join(c.EnvNames, "|"), env))
		run = append(run, "--env-file", env)
	}
	for _, a := range c.Run {
		if a == imageSlot {
			a = image
		}
		run = append(run, shq(a))
	}
	steps = append(steps, strings.Join(run, " "))
	if len(c.EnvNames) > 0 {
		steps = append(steps, "rm "+env)
	}
	after = append(after, fmt.Sprintf("Once the new one is healthy: docker rm %s", old))
	if slices.Contains(c.Run, "-P") {
		after = append(after, fmt.Sprintf("-P publishes new random host ports: clients that used the old port need the new one (docker port %s)", name))
	}
	after = append(after, "Stopped before the rename: docker start "+name)
	back := fmt.Sprintf("docker rename %s %s && docker start %s", old, name, name)
	if len(c.EnvNames) > 0 {
		back = "rm -f " + env + "; " + back
	}
	after = append(after, fmt.Sprintf("Stopped after the rename: docker rm %s if it was made, then %s", name, back))
	return strings.Join(steps, " && "), after
}

// personEnv is the environment the person gave a container, as an env file
// takes it: the variables createdAs names, with their values, so the next
// image's defaults are the next image's (the second review).
func personEnv(ic inspectContainer, names []string) []string {
	var out []string
	for _, kv := range ic.Config.Env {
		if name, _, ok := strings.Cut(kv, "="); ok && slices.Contains(names, name) {
			out = append(out, kv)
		}
	}
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// errNoDocker is a run's container step with no docker to ask.
var errNoDocker = errors.New("docker is not on PATH")
