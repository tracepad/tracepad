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
	// notes are what the plan could not check, said without a verdict.
	notes []string
	// ahead: what the command looks after runs past the latest stable
	// release, and no version was named.
	ahead bool
	// latest is the latest stable release when no version was named and the
	// installed binary is past it — a release candidate — which is then the
	// target (raised): the plan never takes a server back from the binary it
	// runs, nor forward to a candidate nobody named (the final review).
	latest string
	raised bool
	// held are the command's servers behind a raised target: the person's
	// to take there, by naming it.
	held []string
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
	p := &plan{f: r.discover(ctx), to: to}
	rep.fill(p.f)
	// With no version named, the target is the later of the latest stable
	// release and the installed binary (the final review): a candidate
	// installed is not gone back from, and a server behind it is not taken
	// to it unasked.
	if b := p.f.Binary; r.flags.to == "" && b.Ours {
		if order, ok := Compare(b.Version, to); ok && order > 0 {
			p.latest, p.raised = to, true
			p.to, to = b.Version, b.Version
		}
	}
	rep.To = to

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
			// Only a version named gets here: one not named was raised to
			// the installed binary's above.
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
		if order < 0 && r.flags.to != "" {
			return nil, downgrade(to, fmt.Sprintf("server pid %d", p.server.Proc.PID), p.server.Version)
		}
		if order <= 0 {
			p.ahead = p.ahead || order < 0
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
			if r.flags.to == "" {
				p.ahead = true
				continue
			}
			return nil, downgrade(to, fmt.Sprintf("server pid %d", s.Proc.PID), s.Version) + " Leave it running"
		}
	}
	// The plan refuses what the upgrade would (#38): a binary put under
	// another server of it, whatever version that one runs.
	// Two of the command's, or more, on one binary that needs replacing
	// refuse up front too (the tenth review): naming one with --data-dir
	// would put the binary under the other, which #38 refuses.
	if p.replaceBinary {
		if !p.f.processes {
			return nil, fmt.Sprintf("processes of this user could not be read, and any of them may run from %s; nothing can be put there until they can (the report's notes say how many)", bin.Path)
		}
		var on []Server
		for _, s := range p.f.Servers {
			if (p.server == nil || s.Proc.PID != p.server.Proc.PID) && (s.Proc.Exe == "" || sameFile(s.Proc.Exe, bin.Path)) {
				on = append(on, s)
			}
		}
		switch {
		case len(on) > 1 || (len(on) == 1 && len(p.choose) > 0):
			var names []string
			for _, s := range on {
				names = append(names, fmt.Sprintf("pid %d on %s", s.Proc.PID, s.DataDir))
			}
			return nil, fmt.Sprintf("servers %s all run %s, and its replacement would go under each of them but one: "+
				"stop all of them but one, backing their data directories up first, upgrade the one left with --data-dir, then start the others again, which migrates them on %s (%s)",
				strings.Join(names, " and "), bin.Path, to, docsUpgrading)
		case len(on) == 1:
			return nil, underAnother(on[0].Proc.PID, bin.Path, on[0].Version, on[0].DataDir, to)
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
	// Behind a raised target, a server is the person's to take there by
	// naming it: neither picked nor a choice.
	if p.raised {
		behind = func(string) bool { return false }
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
			if order, ok := Compare(p.to, s.Version); p.raised && ok && order > 0 {
				return nil // named, and still the person's to take to a candidate
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
		case s == p.server:
		case s.Version == "":
			// Unknown is never current (the audit of #223): it may be behind.
			p.person = append(p.person, fmt.Sprintf("server pid %d does not say its version, so whether it is behind %s cannot be told; %s. %s", s.Proc.PID, p.to, s.Reason, serverAdvice(*s)))
		case !older(s.Version):
		case s.Ours && p.raised:
			p.held = append(p.held, fmt.Sprintf("server pid %d runs %s, behind the installed %s, a release candidate past the latest stable release %s; the command takes a server to a candidate only when it is named: %s --to %s --data-dir %s",
				s.Proc.PID, s.Version, p.to, p.latest, r.self(), p.to, shq(s.DataDir)))
		case s.Ours:
			p.later = append(p.later, fmt.Sprintf("server pid %d (%s): %s --data-dir %s", s.Proc.PID, s.Version, r.self(), shq(s.DataDir)))
		default:
			p.person = append(p.person, fmt.Sprintf("server pid %d runs %s; %s. %s", s.Proc.PID, s.Version, s.Reason, serverAdvice(*s)))
		}
	}
	// A container is the person's (#36): one behind, or one that does not
	// say its version on this machine and may be, gets the commands that
	// upgrade it.
	for _, c := range p.f.Containers {
		switch {
		case c.Unchecked != "":
			// Where it answers cannot be told: not checked, and said so —
			// not called behind (the final review).
			p.notes = append(p.notes, fmt.Sprintf("container %s was not checked: %s. Whether it is behind %s is yours to look at: docker exec %s /tracepad version", c.Name, c.Unchecked, p.to, shq(c.Name)))
		case c.Version == "":
			p.person = append(p.person, fmt.Sprintf("container %s does not say its version on this machine, so whether it is behind %s cannot be told; %s. %s", c.Name, p.to, c.Reason, containerAdvice(c, p.to)))
		case older(c.Version):
			p.person = append(p.person, fmt.Sprintf("container %s runs %s; %s. %s", c.Name, c.Version, c.Reason, containerAdvice(c, p.to)))
		}
	}
	// A binary runs nothing (#37 (e)): one behind, or one that does not say
	// its version, is named, and not counted.
	if b := p.f.Binary; !b.Ours && b.Exists && (b.Version == "" || older(b.Version)) {
		p.binaries = append(p.binaries, fmt.Sprintf("%s is %s; %s", b.Path, orNone(b.Version), b.Reason))
	}
	if b := p.f.Binary; b.First != "" {
		v, err := b.FirstVersion, b.FirstErr
		if err != nil {
			p.binaries = append(p.binaries, fmt.Sprintf("%s, first on PATH, does not say its version (%v)", b.First, err))
		} else if older(v) {
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
	if interrupted(ctx, rep) {
		return rep
	}
	if p == nil {
		rep.ExitCode, rep.Summary = exitRefused, "Refused: "+refusal
		return rep
	}
	r.describe(p, rep)
	rep.ExitCode, rep.Summary = verdictOf(p)
	return rep
}

// interrupted ends a plan an interrupt cut short — SIGTERM from the install
// script's watchdog, Ctrl-C — with no verdict (the final review): what it
// looked at in part is no finding, and the install script reads exit 1 as
// "could not check", never as "something runs older".
func interrupted(ctx context.Context, rep *Report) bool {
	if ctx.Err() == nil {
		return false
	}
	*rep = Report{Mode: rep.Mode, ExitCode: exitRefused,
		Summary: "Interrupted: the look at the machine was cut short, so this says nothing of what runs; nothing was changed."}
	return true
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
	case len(p.held) > 0:
		return exitDecide, fmt.Sprintf("The installed binary is %s, a release candidate past the latest stable release %s, and a server runs behind it: yours to take there by naming it, as said below.", p.to, p.latest)
	case len(p.person) > 0:
		return exitDecide, fmt.Sprintf("Nothing of the command's is behind %s; what is, or may be, is yours.", p.to)
	}
	switch {
	case p.raised:
		return exitOK, fmt.Sprintf("Nothing to do: what this command looks after runs %s, the installed release candidate, past the latest stable release %s; --to names another.", p.to, p.latest)
	case p.ahead:
		return exitOK, fmt.Sprintf("Nothing to do: what this command looks after runs %s or a later release than that, the latest stable one; --to names another.", p.to)
	}
	return exitOK, fmt.Sprintf("Everything this command looks after runs %s already.", p.to)
}

// describe writes the plan's steps, what is the person's, and the next
// command into the report.
func (r *runner) describe(p *plan, rep *Report) {
	for i := range rep.Servers {
		rep.Servers[i].Target = p.server != nil && rep.Servers[i].PID == p.server.Proc.PID
	}
	rep.Person = append(append(append(rep.Person, p.held...), p.person...), p.binaries...)
	rep.Notes = append(rep.Notes, p.notes...)
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
