package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Docker is the docker CLI. The real one runs `docker`; tests give a fake
// that keeps containers in memory and records every call.
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
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("docker %s: %s", args[0], msg)
	}
	return stdout.Bytes(), nil
}

// busybox is the image docker.md's backup of a volume runs.
const busybox = "busybox:1.38"

// imageRepos are the repositories of the image: GitHub's registry and the
// Docker Hub copy (spec 020 #33).
var imageRepos = []string{"ghcr.io/tracepad/tracepad", "docker.io/tracepad/tracepad", "tracepad/tracepad"}

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

// inspectContainer is the part of `docker inspect` the plan reads: enough to
// name a container, its image, its volume and its address.
type inspectContainer struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
	// Image is the image's ID, which `docker image inspect` reads.
	Image  string `json:"Image"`
	Config struct {
		Image      string            `json:"Image"`
		Labels     map[string]string `json:"Labels"`
		Env        []string          `json:"Env"`
		Entrypoint []string          `json:"Entrypoint"`
		Cmd        []string          `json:"Cmd"`
	} `json:"Config"`
	HostConfig struct {
		PortBindings  map[string][]portBinding `json:"PortBindings"`
		RestartPolicy struct {
			Name              string `json:"Name"`
			MaximumRetryCount int    `json:"MaximumRetryCount"`
		} `json:"RestartPolicy"`
		NetworkMode string `json:"NetworkMode"`
	} `json:"HostConfig"`
	// NetworkSettings.Ports are the bindings as Docker made them: a port
	// asked for as any (`-p 127.0.0.1::4318`) has its number only here.
	NetworkSettings struct {
		Ports map[string][]portBinding `json:"Ports"`
	} `json:"NetworkSettings"`
	Mounts []mount `json:"Mounts"`
}

// Container is a container running Tracepad's image. Every one is the
// person's in this release (#36): the plan names it, its version, and the
// commands that upgrade it, and the command changes nothing of it.
type Container struct {
	Name string
	// Ref is the image reference it was created from; Repo is that without
	// the tag.
	Ref, Repo string
	Volume    string
	URL       string
	Version   string
	// Compose is its Compose project, when Compose made it.
	Compose string
	Reason  string
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
	// configuration could not be read.
	Run      []string
	EnvNames []string
}

// imageSlot stands for the image in Container.Run.
const imageSlot = "\x00image"

// imageConfig is the part of `docker image inspect` the plan reads.
type imageConfig struct {
	Config struct {
		Env        []string `json:"Env"`
		Entrypoint []string `json:"Entrypoint"`
		Cmd        []string `json:"Cmd"`
	} `json:"Config"`
}

// createdAs is how a container was created, read back from its inspect and its
// image's (the live run of rc.3: "the options it was created with" left a
// person to find them): its published ports, its mounts, its restart
// policy, its network, and the command and variables it was given beyond
// the image's own.
func createdAs(ic inspectContainer, img imageConfig) (run, envNames []string) {
	keys := make([]string, 0, len(ic.HostConfig.PortBindings))
	for k := range ic.HostConfig.PortBindings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		port, proto, _ := strings.Cut(k, "/")
		for _, b := range ic.HostConfig.PortBindings[k] {
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
		src := m.Name
		switch m.Type {
		case "volume":
		case "bind":
			src = m.Source
		default:
			run = append(run, "--tmpfs", m.Destination)
			continue
		}
		spec := src + ":" + m.Destination
		if !m.RW {
			spec += ":ro"
		}
		run = append(run, "-v", spec)
	}
	if rp := ic.HostConfig.RestartPolicy; rp.Name != "" && rp.Name != "no" {
		policy := rp.Name
		if rp.Name == "on-failure" && rp.MaximumRetryCount > 0 {
			policy += ":" + strconv.Itoa(rp.MaximumRetryCount)
		}
		run = append(run, "--restart", policy)
	}
	if n := ic.HostConfig.NetworkMode; n != "" && n != "default" && n != "bridge" {
		run = append(run, "--network", n)
	}
	args := ic.Config.Cmd
	if !slices.Equal(ic.Config.Entrypoint, img.Config.Entrypoint) && len(ic.Config.Entrypoint) > 0 {
		run = append(run, "--entrypoint", ic.Config.Entrypoint[0])
		args = append(slices.Clone(ic.Config.Entrypoint[1:]), args...)
	} else if slices.Equal(args, img.Config.Cmd) {
		args = nil
	}
	run = append(run, imageSlot)
	run = append(run, args...)
	for _, kv := range ic.Config.Env {
		if name, _, ok := strings.Cut(kv, "="); ok && !slices.Contains(img.Config.Env, kv) && !slices.Contains(envNames, name) {
			envNames = append(envNames, name)
		}
	}
	sort.Strings(envNames)
	return run, envNames
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

// containerReason is why a container is the person's: all are, in this
// release.
const containerReason = "the command does not stop, recreate or go back from a container in this release"

// containers lists the running containers of Tracepad's image, each with its
// version when it answers on this machine. note says when Docker could not
// be asked; an error is never taken for "no containers".
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
		repo, ok := imageRepo(ic.Config.Image)
		if !ok {
			continue
		}
		c := Container{Name: strings.TrimPrefix(ic.Name, "/"), Ref: ic.Config.Image, Repo: repo,
			Compose: ic.Config.Labels["com.docker.compose.project"], Reason: containerReason}
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
		// Its image's own configuration tells what it was given beyond it;
		// one that cannot be read leaves the run to the person, said.
		if img, err := r.inspectImage(ctx, ic.Image); err == nil {
			c.Run, c.EnvNames = createdAs(ic, img)
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
			if v, err := health(ctx, r.deps.HTTP, c.URL); err == nil {
				c.Version = v
			}
		}(&cs[i])
	}
	wg.Wait()
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
	return list, nil
}

// inspectImage reads an image's own configuration.
func (r *runner) inspectImage(ctx context.Context, id string) (imageConfig, error) {
	out, err := r.deps.Docker.Run(ctx, "image", "inspect", id)
	if err != nil {
		return imageConfig{}, err
	}
	var list []imageConfig
	if err := json.Unmarshal(out, &list); err != nil || len(list) != 1 {
		return imageConfig{}, fmt.Errorf("docker image inspect %s: %v", id, err)
	}
	return list[0], nil
}

// containerAdvice is what upgrades a container, as docker.md does it: stop
// it, back its volume up, pull the release, and run it again with the
// options it was created with, the old one kept under another name until the
// new one has proved itself.
func containerAdvice(c Container, to string) string {
	image := c.Repo + ":" + to
	if c.Compose != "" {
		return fmt.Sprintf("Upgrade it with Compose (%s): back up its volume as docs/docker.md shows, set the image to %s in the project %s's Compose file, then: docker compose up -d",
			docsDocker, image, shq(c.Compose))
	}
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
		steps = append(steps, fmt.Sprintf("then run %s as %s with the options %s was created with (docker inspect %s), and remove %s once it is healthy", image, name, name, old, old))
		return fmt.Sprintf("Upgrade it yourself (%s): %s", docsDocker, strings.Join(steps, "; "))
	}
	// The variables it was given go into a file of their own, read from
	// Docker and never printed (the live run of rc.3): they hold its keys.
	run := []string{"docker run -d --name", name}
	// A name of the upgrade's own, never overwritten (set -C): docs/docker.md
	// keeps an admin token in tracepad.env, which this file must not take.
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
	steps = append(steps, fmt.Sprintf("docker rm %s once the new one is healthy", old))
	return fmt.Sprintf("Upgrade it yourself (%s): %s. Any other option it was given — a user, limits, labels — is in docker inspect %s", docsDocker, strings.Join(steps, "; "), old)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
