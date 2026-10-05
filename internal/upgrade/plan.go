package upgrade

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

const docsUpgrading = "https://tracepad.github.io/tracepad/install/#upgrading"
const docsDocker = "https://tracepad.github.io/tracepad/docker/#upgrading-and-backing-up-first"

// plan is what an upgrade would do, worked out from the findings.
type plan struct {
	f  Findings
	to string
	// replaceBinary: the installed binary is the command's and older.
	replaceBinary bool
	server        *Server
	container     *Container
	// from is the target's running version, or the binary's.
	from string
	// choose is set when more than one server or container is the
	// command's and none was named.
	choose []string
	// later are the command's own servers or containers, older than to,
	// that this run does not take (another run will).
	later []string
	// person are the person's, older than to, each with what to do.
	person []string
}

func (p *plan) pending() bool {
	return p.replaceBinary || p.server != nil || p.container != nil || len(p.choose) > 0
}

// makePlan resolves the target version and works out the plan. A refusal is
// its sentence; the plan is nil then.
func (r *runner) makePlan(ctx context.Context, rep *Report) (*plan, string) {
	to := r.flags.to
	switch {
	case to == "":
		latest, err := r.deps.Releases.Latest(ctx)
		if err != nil {
			return nil, err.Error()
		}
		to = latest
	case !IsRelease(to):
		return nil, fmt.Sprintf("%q is not a release's version (X.Y.Z, or X.Y.Z-rc.N)", to)
	default:
		if err := r.deps.Releases.Exists(ctx, to); err != nil {
			return nil, err.Error()
		}
	}
	rep.To = to
	p := &plan{f: r.discover(ctx), to: to}
	rep.fill(p.f)

	if err := r.pickTarget(p); err != nil {
		return nil, err.Error()
	}
	bin := p.f.Binary
	if bin.Ours {
		order, _ := Compare(to, bin.Version)
		if order < 0 {
			return nil, downgrade(to, "the installed binary", bin.Version)
		}
		p.replaceBinary = order > 0
	}
	p.from = bin.Version
	switch {
	case p.server != nil:
		if !bin.Ours {
			return nil, fmt.Sprintf("server pid %d runs %s, which the command cannot replace: %s", p.server.Proc.PID, bin.Path, bin.Reason)
		}
		order, _ := Compare(to, p.server.Version)
		if order < 0 {
			return nil, downgrade(to, fmt.Sprintf("server pid %d", p.server.Proc.PID), p.server.Version)
		}
		if order == 0 {
			p.server = nil
		} else {
			p.from = p.server.Version
		}
	case p.container != nil:
		order, _ := Compare(to, p.container.Version)
		if order < 0 {
			return nil, downgrade(to, "container "+p.container.Name, p.container.Version)
		}
		if order == 0 {
			p.container = nil
		} else {
			p.from = p.container.Version
		}
	}
	r.othersBehind(p)
	rep.From = p.from
	return p, ""
}

func downgrade(to, what, running string) string {
	return fmt.Sprintf("%s is older than %s, which %s runs: migrations run forward only, and an older binary does not open a database a newer one migrated. "+
		"The way back from an upgrade is that upgrade's: tracepad upgrade --back <run>", to, running, what)
}

// pickTarget chooses the server or container this run upgrades: the one the
// flags name, or the only one that is the command's.
func (r *runner) pickTarget(p *plan) error {
	var ours []string
	for i, s := range p.f.Servers {
		if s.Ours {
			ours = append(ours, "--data-dir "+s.DataDir)
			if r.flags.dataDir == "" && r.flags.container == "" {
				p.server = &p.f.Servers[i]
			}
		}
	}
	for i, c := range p.f.Containers {
		if c.Ours {
			ours = append(ours, "--container "+c.Name)
			if r.flags.dataDir == "" && r.flags.container == "" {
				p.container = &p.f.Containers[i]
			}
		}
	}
	switch {
	case r.flags.dataDir != "":
		want, err := filepath.Abs(r.flags.dataDir)
		if err != nil {
			return err
		}
		want = canonicalDir(want)
		for i, s := range p.f.Servers {
			if s.DataDir != want {
				continue
			}
			if !s.Ours {
				return fmt.Errorf("the server on %s is yours: %s", want, s.Reason)
			}
			p.server = &p.f.Servers[i]
			return nil
		}
		return fmt.Errorf("no server runs on %s", want)
	case r.flags.container != "":
		for i, c := range p.f.Containers {
			if c.Name != r.flags.container {
				continue
			}
			if !c.Ours {
				return fmt.Errorf("the container %s is yours: %s", c.Name, c.Reason)
			}
			p.container = &p.f.Containers[i]
			return nil
		}
		return fmt.Errorf("no running container of Tracepad's image is named %s", r.flags.container)
	case len(ours) > 1:
		p.server, p.container = nil, nil
		p.choose = ours
	}
	return nil
}

func canonicalDir(dir string) string {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Clean(real)
	}
	return filepath.Clean(dir)
}

// othersBehind lists what runs older than the target version and this run
// does not upgrade: the command's own (for a later run) and the person's.
func (r *runner) othersBehind(p *plan) {
	older := func(v string) bool {
		order, ok := Compare(p.to, v)
		return ok && order > 0
	}
	for i := range p.f.Servers {
		s := &p.f.Servers[i]
		switch {
		case s == p.server || !older(s.Version):
		case s.Ours:
			p.later = append(p.later, fmt.Sprintf("server pid %d (%s): tracepad upgrade --data-dir %s", s.Proc.PID, s.Version, s.DataDir))
		default:
			p.person = append(p.person, fmt.Sprintf("server pid %d runs %s; %s. %s", s.Proc.PID, s.Version, s.Reason, serverAdvice(*s)))
		}
	}
	for i := range p.f.Containers {
		c := &p.f.Containers[i]
		switch {
		case c == p.container || !older(c.Version):
		case c.Ours:
			p.later = append(p.later, fmt.Sprintf("container %s (%s): tracepad upgrade --container %s", c.Name, c.Version, c.Name))
		default:
			p.person = append(p.person, fmt.Sprintf("container %s runs %s; %s. %s", c.Name, c.Version, c.Reason, containerAdvice(*c, p.to)))
		}
	}
	if b := p.f.Binary; !b.Ours && b.Exists && older(b.Version) {
		p.person = append(p.person, fmt.Sprintf("%s is %s; %s", b.Path, b.Version, b.Reason))
	}
	if b := p.f.Binary; b.First != "" {
		if v, err := r.deps.Version(context.Background(), b.First); err == nil && older(v) {
			advice := "its package manager upgrades it"
			if strings.Contains(b.First, "/homebrew/") || strings.Contains(b.First, "/Cellar/") || strings.HasPrefix(b.First, "/usr/local/bin/") {
				advice = "brew upgrade tracepad"
			}
			p.person = append(p.person, fmt.Sprintf("%s, first on PATH, is %s: %s", b.First, v, advice))
		}
	}
}

func containerAdvice(c Container, to string) string {
	if project := c.inspect.Config.Labels["com.docker.compose.project"]; project != "" {
		return fmt.Sprintf("Back up its volume, set the image to %s:%s in its Compose file, then: docker compose up -d (%s)", c.Repo, to, docsDocker)
	}
	return fmt.Sprintf("Back up its volume and recreate it on %s:%s (%s)", c.Repo, to, docsDocker)
}

func serverAdvice(s Server) string {
	switch {
	case strings.HasPrefix(s.Proc.Manager, "the systemd unit "):
		unit := strings.TrimPrefix(s.Proc.Manager, "the systemd unit ")
		return fmt.Sprintf("Back up its data directory, install %s, then: sudo systemctl restart %s (%s)", "the new binary", unit, docsUpgrading)
	case strings.HasPrefix(s.Proc.Manager, "the launchd job "):
		job := strings.TrimPrefix(s.Proc.Manager, "the launchd job ")
		return fmt.Sprintf("Back up its data directory, install the new binary, then: launchctl kickstart -k gui/$(id -u)/%s (%s)", job, docsUpgrading)
	}
	return "Back it up and restart it yourself (" + docsUpgrading + ")"
}

// planMode is `--plan`: what would happen, and nothing done.
func (r *runner) planMode(ctx context.Context) *Report {
	rep := &Report{Mode: "plan"}
	p, refusal := r.makePlan(ctx, rep)
	if p == nil {
		rep.ExitCode, rep.Summary = exitRefused, "Refused: "+refusal
		return rep
	}
	r.describe(p, rep)
	switch {
	case p.pending():
		rep.ExitCode = exitPending
		rep.Summary = fmt.Sprintf("An upgrade to %s is pending.", p.to)
		if len(p.choose) > 0 {
			rep.Summary = fmt.Sprintf("An upgrade to %s is pending; more than one server or container is the command's, so name one.", p.to)
		}
	case len(p.later) > 0:
		rep.ExitCode = exitPending
		rep.Summary = fmt.Sprintf("An upgrade to %s is pending.", p.to)
	case len(p.person) > 0:
		rep.ExitCode = exitDecide
		rep.Summary = fmt.Sprintf("Nothing of the command's is behind %s; what is, is yours.", p.to)
	default:
		rep.ExitCode = exitOK
		rep.Summary = fmt.Sprintf("Everything this command looks after runs %s already.", p.to)
	}
	return rep
}

// describe writes the plan's steps, what is the person's, and the next
// command into the report.
func (r *runner) describe(p *plan, rep *Report) {
	for i := range rep.Servers {
		rep.Servers[i].Target = p.server != nil && rep.Servers[i].PID == p.server.Proc.PID
	}
	for i := range rep.Containers {
		rep.Containers[i].Target = p.container != nil && rep.Containers[i].Name == p.container.Name
	}
	rep.Person = append(rep.Person, p.person...)
	if len(p.choose) > 0 {
		rep.Plan = append(rep.Plan, "more than one is the command's; one run upgrades one of them: "+strings.Join(p.choose, ", or "))
		for _, c := range p.choose {
			rep.Next = append(rep.Next, "tracepad upgrade "+c)
		}
		return
	}
	bin := p.f.Binary
	steps := []string{}
	if p.replaceBinary || p.server != nil || p.container != nil {
		steps = append(steps, fmt.Sprintf("download tracepad %s and check it against the release's checksums.txt (and its attestation when gh is logged in)", p.to))
	}
	count := "read the trace count with TRACEPAD_API_KEY, to compare after"
	if r.deps.Getenv("TRACEPAD_API_KEY") == "" {
		count = "no TRACEPAD_API_KEY in the environment: the trace counts will not be compared"
	}
	switch {
	case p.server != nil:
		s := p.server
		steps = append(steps,
			fmt.Sprintf("make a run directory under %s, keep a copy of %s there to go back to, and check there is room for a backup of %s", r.deps.Backups, p.from, s.DataDir),
			count,
			fmt.Sprintf("stop server pid %d (SIGTERM, up to %d seconds; never killed)", s.Proc.PID, int(r.deps.StopWait.Seconds())),
			fmt.Sprintf("archive %s and read the archive back whole", s.DataDir),
			fmt.Sprintf("put tracepad %s at %s", p.to, bin.Path),
			"start it with the same arguments, environment and working directory",
			fmt.Sprintf("check that %s answers as %s, and the trace count", s.URL, p.to),
			"no answer as "+p.to+": go back at once; a lower or unreadable count: stop and leave it to you",
		)
	case p.container != nil:
		c := p.container
		steps = append(steps,
			fmt.Sprintf("pull %s:%s, make a run directory under %s, and check there is room for a backup of the volume %s", c.Repo, p.to, r.deps.Backups, c.Volume),
			count,
			fmt.Sprintf("stop %s, archive its volume with %s, and read the archive back whole", c.Name, busybox),
			fmt.Sprintf("rename it %s-before-<run> (restart policy no), and run %s:%s as %s with its mounts, ports, restart policy, labels and the variables you set (through an env file)", c.Name, c.Repo, p.to, c.Name),
			fmt.Sprintf("check that %s answers as %s, and the trace count", c.URL, p.to),
			"no answer as "+p.to+": go back at once; a lower or unreadable count: stop and leave it to you",
		)
		if p.replaceBinary {
			steps = append(steps, fmt.Sprintf("then put tracepad %s at %s", p.to, bin.Path))
		}
	case p.replaceBinary:
		steps = append(steps, fmt.Sprintf("keep a copy of %s in a run directory under %s, and put tracepad %s at %s; no server of the command's runs it", bin.Version, r.deps.Backups, p.to, bin.Path))
	}
	if len(steps) > 0 {
		steps = append(steps, "install the skill again wherever a copy is marked with its .version")
		rep.Plan = steps
		next := "tracepad upgrade"
		if r.flags.to != "" {
			next += " --to " + p.to
		}
		if p.server != nil && r.flags.dataDir != "" {
			next += " --data-dir " + p.server.DataDir
		}
		if p.container != nil && r.flags.container != "" {
			next += " --container " + p.container.Name
		}
		rep.Next = append(rep.Next, next)
	}
	for _, l := range p.later {
		rep.Next = append(rep.Next, "then "+l)
	}
}
