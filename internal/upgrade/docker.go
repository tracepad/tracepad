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
	Destination string `json:"Destination"`
}

// inspectContainer is the part of `docker inspect` the plan reads: enough to
// name a container, its image, its volume and its address.
type inspectContainer struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Config struct {
		Image      string            `json:"Image"`
		Labels     map[string]string `json:"Labels"`
		Env        []string          `json:"Env"`
		Entrypoint []string          `json:"Entrypoint"`
		Cmd        []string          `json:"Cmd"`
	} `json:"Config"`
	HostConfig struct {
		PortBindings map[string][]portBinding `json:"PortBindings"`
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
		host := b.HostIP
		switch host {
		case "", "0.0.0.0":
			host = "127.0.0.1"
		case "::":
			host = "::1"
		}
		if u, ok := loopbackBase(host, b.HostPort); ok && url == "" {
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
			if m.Destination == "/data" && m.Type == "volume" {
				c.Volume = m.Name
			}
		}
		c.URL, c.Default, c.Unchecked = containerAddress(ic)
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
	vol := c.Volume
	if vol == "" {
		vol = "<its /data volume>"
	}
	return fmt.Sprintf("Upgrade it yourself (%s): docker stop %s; "+
		"docker run --rm --mount type=volume,src=%s,dst=/data,readonly -v \"$PWD:/backup\" %s sh -c 'umask 077 && tar czf /backup/%s-%s.tar.gz -C /data .'; "+
		"docker pull %s; docker rename %s %s-old; then run %s as %s with the options %s was created with, and remove %s-old once it is healthy",
		docsDocker, shq(c.Name), shq(vol), busybox, c.Name, version, image, shq(c.Name), c.Name, image, shq(c.Name), shq(c.Name), shq(c.Name))
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
