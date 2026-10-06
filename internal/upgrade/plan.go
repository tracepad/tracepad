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
	// from is the target's running version, or the binary's.
	from string
	// choose is set when more than one server is the command's and none
	// was named.
	choose []string
	// later are the command's own servers, older than to, that this run
	// does not take (another run will).
	later []string
	// person are the person's servers and containers, older than to, each
	// with what to do; binaries, the person's binaries older than to — what
	// runs older is the first, and only it makes the plan's exit 4 (the
	// eighth review: an older tracepad first on PATH runs nothing).
	person   []string
	binaries []string
}

func (p *plan) pending() bool {
	return p.replaceBinary || p.server != nil || len(p.choose) > 0
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
		order, ok := Compare(to, bin.Version)
		if !ok {
			return nil, fmt.Sprintf("the installed binary says %q, which cannot be ordered against %s", bin.Version, to)
		}
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
		// A server named that does not say its version — still starting, or
		// hung — is not taken for current (the eighth review).
		order, ok := Compare(to, p.server.Version)
		if !ok {
			return nil, fmt.Sprintf("server pid %d does not say its version at %s, so whether it is behind %s cannot be told; run the plan again once it answers", p.server.Proc.PID, p.server.URL, to)
		}
		if order < 0 {
			return nil, downgrade(to, fmt.Sprintf("server pid %d", p.server.Proc.PID), p.server.Version)
		}
		if order == 0 {
			p.server = nil
		} else {
			p.from = p.server.Version
		}
	}
	// A server running later than the target, from the installed binary or
	// the command's own, is a downgrade too: after an
	// install script put an older binary in place (TRACEPAD_VERSION), the
	// plan refuses, and says to leave what runs as it is (#15; the final
	// review: the plan used to say everything was current).
	for _, s := range p.f.Servers {
		if order, ok := Compare(s.Version, to); ok && order > 0 && (s.Ours || sameFile(s.Proc.Exe, bin.Path)) {
			return nil, downgrade(to, fmt.Sprintf("server pid %d", s.Proc.PID), s.Version) + " Leave it running"
		}
	}
	// The plan refuses what the upgrade would (#38): a binary put under
	// another server of it, whatever version that one runs.
	if p.replaceBinary && len(p.choose) == 0 {
		for _, s := range p.f.Servers {
			if (p.server == nil || s.Proc.PID != p.server.Proc.PID) && sameFile(s.Proc.Exe, bin.Path) {
				return nil, underAnother(s.Proc.PID, bin.Path, s.Version, s.DataDir, to)
			}
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

// pickTarget chooses the server this run upgrades: the one --data-dir
// names, or the only one that is the command's.
func (r *runner) pickTarget(p *plan) error {
	// Only what runs older than the target is a choice: two servers already
	// at it would otherwise keep a plan pending that the upgrade refuses (the
	// review of #1). A flag still names any of the command's.
	behind := func(v string) bool {
		order, ok := Compare(p.to, v)
		return ok && order > 0
	}
	var ours []string
	for i, s := range p.f.Servers {
		if s.Ours && behind(s.Version) {
			ours = append(ours, "--data-dir "+shq(s.DataDir))
			if r.flags.dataDir == "" {
				p.server = &p.f.Servers[i]
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
	case len(ours) > 1:
		p.server = nil
		p.choose = ours
	}
	return nil
}

// self is this command, as the person runs it again: the binary running it,
// by its absolute path, never a bare `tracepad` another binary on PATH may
// answer to (the second review).
func (r *runner) self() string {
	return shq(r.deps.Self) + " upgrade"
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
			p.later = append(p.later, fmt.Sprintf("server pid %d (%s): %s --data-dir %s", s.Proc.PID, s.Version, r.self(), shq(s.DataDir)))
		default:
			p.person = append(p.person, fmt.Sprintf("server pid %d runs %s; %s. %s", s.Proc.PID, s.Version, s.Reason, serverAdvice(*s)))
		}
	}
	// A container is the person's (#36): one behind gets the commands that
	// upgrade it. One that does not say its version on this machine is in
	// the report's list of containers, and is not called behind.
	for _, c := range p.f.Containers {
		if older(c.Version) {
			p.person = append(p.person, fmt.Sprintf("container %s runs %s; %s. %s", c.Name, c.Version, c.Reason, containerAdvice(c, p.to)))
		}
	}
	if b := p.f.Binary; !b.Ours && b.Exists && older(b.Version) {
		p.binaries = append(p.binaries, fmt.Sprintf("%s is %s; %s", b.Path, b.Version, b.Reason))
	}
	if b := p.f.Binary; b.First != "" {
		if v, err := r.deps.Version(context.Background(), b.First); err == nil && older(v) {
			advice := "its package manager upgrades it"
			if pm := packageManager(canonicalPath(b.First)); pm != "" {
				advice = pm
			} else if strings.HasPrefix(b.First, "/usr/local/bin/") {
				advice = "brew upgrade tracepad, if Homebrew installed it"
			}
			p.binaries = append(p.binaries, fmt.Sprintf("%s, first on PATH, is %s: %s", b.First, v, advice))
		}
	}
}

func serverAdvice(s Server) string {
	m := s.Proc.Manager
	switch {
	case strings.HasPrefix(m, "the user systemd unit "):
		return fmt.Sprintf("Back up its data directory, install the new binary, then: systemctl --user restart %s (%s)", strings.TrimPrefix(m, "the user systemd unit "), docsUpgrading)
	case strings.HasPrefix(m, "the systemd unit "):
		return fmt.Sprintf("Back up its data directory, install the new binary, then: sudo systemctl restart %s (%s)", strings.TrimPrefix(m, "the systemd unit "), docsUpgrading)
	case strings.HasPrefix(m, "the launchd job "):
		return fmt.Sprintf("Back up its data directory, install the new binary, then: launchctl kickstart -k gui/$(id -u)/%s (%s)", strings.TrimPrefix(m, "the launchd job "), docsUpgrading)
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
	rep.ExitCode, rep.Summary = verdictOf(p)
	return rep
}

// verdictOf is what a plan finds, for --plan and for an upgrade that finds
// nothing of its own to do: the same state, the same verdict (the final
// review).
func verdictOf(p *plan) (int, string) {
	switch {
	case len(p.choose) > 0:
		return exitPending, fmt.Sprintf("An upgrade to %s is pending; more than one server is the command's, so name one.", p.to)
	case p.pending():
		return exitPending, fmt.Sprintf("An upgrade to %s is pending.", p.to)
	case len(p.later) > 0:
		return exitPending, fmt.Sprintf("An upgrade to %s is pending for another of the command's: %s.", p.to, strings.Join(p.later, "; "))
	case len(p.person) > 0:
		return exitDecide, fmt.Sprintf("Nothing of the command's is behind %s; what is, is yours.", p.to)
	}
	return exitOK, fmt.Sprintf("Everything this command looks after runs %s already.", p.to)
}

// describe writes the plan's steps, what is the person's, and the next
// command into the report.
func (r *runner) describe(p *plan, rep *Report) {
	for i := range rep.Servers {
		rep.Servers[i].Target = p.server != nil && rep.Servers[i].PID == p.server.Proc.PID
	}
	rep.Person = append(append(rep.Person, p.person...), p.binaries...)
	if len(p.choose) > 0 {
		rep.Plan = append(rep.Plan, "more than one is the command's; one run upgrades one of them: "+strings.Join(p.choose, ", or "))
		for _, c := range p.choose {
			rep.Next = append(rep.Next, r.self()+" "+c)
		}
		return
	}
	bin := p.f.Binary
	steps := []string{}
	if p.replaceBinary || p.server != nil {
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
	case p.replaceBinary:
		steps = append(steps, fmt.Sprintf("keep a copy of %s in a run directory under %s, and put tracepad %s at %s; no server of the command's runs it", bin.Version, r.deps.Backups, p.to, bin.Path))
	}
	if len(steps) > 0 {
		steps = append(steps, "install the skill again wherever a copy is marked with its .version")
		rep.Plan = steps
		next := r.self()
		if r.flags.to != "" {
			next += " --to " + p.to
		}
		if p.server != nil && r.flags.dataDir != "" {
			next += " --data-dir " + shq(p.server.DataDir)
		}
		rep.Next = append(rep.Next, next)
	}
	for _, l := range p.later {
		rep.Next = append(rep.Next, "then "+l)
	}
}
