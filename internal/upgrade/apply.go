package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// afterArchive is a test's seam: what happens to an archive between its
// writing and its read-back (a disk that cut it short).
var afterArchive = func(string) {}

// mib100 is the margin the room check keeps beyond the backup.
const mib100 = 100 << 20

// job is one run in progress: its state, its directory and its report.
type job struct {
	r   *runner
	rep *Report
	st  *State
	dir string
	// spec is how the old server was started (a process run).
	spec ServerSpec
	// newProc is the server this run started, while the command runs.
	newProc Started
	// inspect and image are the old container's (a container run).
	inspect inspectContainer
	image   inspectImage
}

func (j *job) step(name string) error {
	j.st.Steps = append(j.st.Steps, Step{Name: name, At: j.r.deps.Now().UTC()})
	return j.st.save(j.dir)
}

func (j *job) done(format string, args ...any) {
	j.rep.Done = append(j.rep.Done, fmt.Sprintf(format, args...))
}

// upgrade is the default mode.
func (r *runner) upgrade(ctx context.Context) *Report {
	rep := &Report{Mode: "upgrade"}
	p, refusal := r.makePlan(ctx, rep)
	if p == nil {
		rep.ExitCode, rep.Summary = exitRefused, "Refused: "+refusal
		return rep
	}
	r.describe(p, rep)
	rep.Plan = nil
	next := rep.Next
	rep.Next = nil
	if len(p.choose) > 0 {
		rep.ExitCode = exitRefused
		rep.Summary = "Refused: more than one server or container is the command's; name the one this run upgrades."
		rep.Next = next
		return rep
	}
	bin := p.f.Binary
	if !p.replaceBinary && p.server == nil && p.container == nil {
		rep.ExitCode = exitOK
		rep.Summary = fmt.Sprintf("Nothing to upgrade: everything this command looks after runs %s already.", p.to)
		if bin.Ours {
			r.reinstallSkill(ctx, rep, bin.Path, p.to)
		}
		rep.Next = append(rep.Next, laterOnly(next)...)
		return rep
	}

	if err := os.MkdirAll(r.deps.Backups, 0o700); err != nil {
		rep.ExitCode, rep.Summary = exitRefused, "Refused: "+err.Error()
		return rep
	}
	release, ok, err := lockFile(filepath.Join(r.deps.Backups, ".lock"))
	switch {
	case err != nil:
		rep.ExitCode, rep.Summary = exitRefused, "Refused: could not lock "+r.deps.Backups+": "+err.Error()
		return rep
	case !ok:
		rep.ExitCode, rep.Summary = exitRefused, "Refused: another tracepad upgrade is running."
		return rep
	}
	defer release()

	j, refusal := r.prepare(ctx, p, rep)
	if j == nil {
		if rep.Run != nil {
			r.discard(rep.Run.Dir)
			rep.Run = nil
		}
		rep.ExitCode, rep.Summary = exitRefused, "Refused, and nothing changed: "+refusal
		return rep
	}
	switch {
	case p.server != nil:
		j.swapProcess(ctx, p)
	case p.container != nil:
		j.swapContainer(ctx, p)
	default:
		j.swapBinaryOnly(ctx)
	}
	if rep.ExitCode == exitOK || rep.ExitCode == exitDecide {
		rep.Next = append(rep.Next, laterOnly(next)...)
	}
	j.refresh(ctx)
	j.privacy()
	return rep
}

func laterOnly(next []string) []string {
	var out []string
	for _, n := range next {
		if strings.HasPrefix(n, "then ") {
			out = append(out, strings.TrimPrefix(n, "then "))
		}
	}
	return out
}

// prepare is everything before anything stops (Decision 7). Every failure is
// a refusal with nothing changed; the run directory it made stays, with what
// it holds, as a record.
func (r *runner) prepare(ctx context.Context, p *plan, rep *Report) (*job, string) {
	now := r.deps.Now()
	st := &State{Kind: kindBinary, Created: now.UTC(), From: p.from, To: p.to}
	switch {
	case p.server != nil:
		st.Kind = kindProcess
	case p.container != nil:
		st.Kind = kindContainer
	}
	st.Run = newRunID(now, p.from)
	dir := filepath.Join(r.deps.Backups, st.Run)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err.Error()
	}
	j := &job{r: r, rep: rep, st: st, dir: dir}
	rep.Run = &RunRef{ID: st.Run, Dir: dir}
	if err := st.save(dir); err != nil {
		return nil, err.Error()
	}
	if err := linkOrCopy(r.deps.Self, filepath.Join(dir, "upgrader")); err != nil {
		return nil, "could not keep a copy of this command for its way back: " + err.Error()
	}

	bin := p.f.Binary
	st.Binary = &BinaryState{Path: bin.Path}
	if IsRelease(bin.Version) {
		// A development build's `dev` is no version a state may hold; such a
		// binary is the person's and this run never replaces it.
		st.Binary.From = bin.Version
	}
	var newBin Fetched
	if p.replaceBinary {
		// The installed binary is already the target otherwise, and is what
		// the server starts on: nothing to download (the review of #1).
		var err error
		newBin, err = r.deps.Releases.Fetch(ctx, p.to, dir, "tracepad-"+p.to)
		if err != nil {
			return nil, err.Error()
		}
		j.done("downloaded tracepad %s: %s", p.to, newBin.Verified)
	}
	if p.replaceBinary {
		old := filepath.Join(dir, "tracepad-"+bin.Version)
		if err := r.keepCopy(ctx, bin.Path, old, bin.Version); err != nil {
			return nil, fmt.Sprintf("no copy of the installed %s to go back to: %v", bin.Version, err)
		}
		st.Binary.Old = old
	}

	var need int64 = mib100
	switch {
	case p.server != nil:
		if refusal := j.prepareProcess(ctx, p); refusal != "" {
			return nil, refusal
		}
		size, err := survey(p.server.DataDir)
		if err != nil {
			return nil, err.Error()
		}
		need += size
	case p.container != nil:
		size, refusal := j.prepareContainer(ctx, p)
		if refusal != "" {
			return nil, refusal
		}
		need += size
	}
	if st.Kind != kindBinary {
		free, err := freeBytes(dir)
		if err != nil {
			return nil, "could not tell the room in " + dir + ": " + err.Error()
		}
		if free < need {
			return nil, fmt.Sprintf("no room for a backup in %s: %d MiB free, %d MiB needed", dir, free>>20, need>>20)
		}
		base := ""
		if st.Process != nil {
			base = st.Process.URL
		} else {
			base = st.Container.URL
		}
		count, note := r.traceCount(ctx, base)
		st.CountBefore = count
		if count != nil {
			j.done("read the trace count: %d", *count)
		} else {
			rep.Notes = append(rep.Notes, "the trace counts are not compared: "+note)
		}
	}
	if err := j.step(stepPrepared); err != nil {
		return nil, err.Error()
	}
	return j, ""
}

// discard removes a run directory a refused run made and filled only with
// what it fetched and wrote itself — copies of binaries, its own state — before
// anything was stopped: left, it would hold secrets for nothing and eat into
// the next run's room (the review of #1). Only a directory of a run's name,
// directly under the backups directory, is ever removed.
func (r *runner) discard(dir string) {
	if filepath.Dir(dir) != r.deps.Backups || !runID.MatchString(filepath.Base(dir)) {
		return
	}
	_ = os.RemoveAll(dir)
}

// afterStop is the context the rest of a run takes once it has asked a server
// to stop: an interrupt (Ctrl-C, an agent's timeout) must not leave it down
// half way, so the swap and its way back finish on a deadline of their own.
func afterStop(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
}

// keepCopy puts a verified copy of the binary at src into dst: a hard link
// when it can, which the rename of a new binary over src does not touch.
func (r *runner) keepCopy(ctx context.Context, src, dst, version string) error {
	if err := linkOrCopy(src, dst); err != nil {
		return err
	}
	if v, err := r.deps.Version(ctx, dst); err != nil || v != version {
		return fmt.Errorf("the copy says %q, not %s", v, version)
	}
	return nil
}

func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst, 0o755)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// putInPlace puts a binary at dst the install script's way: copied beside
// it, run there, then renamed over it, so a running process keeps its file
// and a binary that does not run here changes nothing.
func (r *runner) putInPlace(ctx context.Context, src, dst, version string) error {
	tmp := filepath.Join(filepath.Dir(dst), ".tracepad."+strconv.Itoa(os.Getpid())+".new")
	_ = os.Remove(tmp)
	if err := copyFile(src, tmp, 0o755); err != nil {
		return err
	}
	if v, err := r.deps.Version(ctx, tmp); err != nil || v != version {
		os.Remove(tmp)
		return fmt.Errorf("the binary says %q at %s, not %s", v, filepath.Dir(dst), version)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// prepareProcess records how the server was started and keeps a copy of the
// version it runs.
func (j *job) prepareProcess(ctx context.Context, p *plan) string {
	r, s, st := j.r, p.server, j.st
	old := filepath.Join(j.dir, "tracepad-"+s.Version)
	switch {
	case st.Binary.Old != "" && st.Binary.From == s.Version:
		old = st.Binary.Old
	case r.keepCopy(ctx, "/proc/"+strconv.Itoa(s.Proc.PID)+"/exe", old, s.Version) == nil:
		// Linux keeps the running executable readable even once replaced.
	default:
		os.Remove(old)
		if _, err := r.deps.Releases.Fetch(ctx, s.Version, j.dir, "tracepad-"+s.Version); err != nil {
			return fmt.Sprintf("no copy of the running %s to go back to: %v", s.Version, err)
		}
	}
	log := s.Proc.Stdout
	if log == "" {
		log = filepath.Join(s.DataDir, "server.log")
	}
	st.Process = &ProcessState{PID: s.Proc.PID, DataDir: s.DataDir, Listen: s.Listen, URL: s.URL, Log: log, Old: old}
	j.spec = ServerSpec{Exe: st.Binary.Path, Argv: s.Proc.Argv, Env: s.Proc.Env, Dir: s.Proc.Cwd}
	if j.spec.Dir == "" {
		j.spec.Dir = s.DataDir
	}
	if err := writeJSON(filepath.Join(j.dir, "server.json"), j.spec); err != nil {
		return err.Error()
	}
	return ""
}

// isServer checks, right before a signal, that pid is still the server of
// dataDir started as spec: its executable, its arguments and the lock's
// record (Decision 8).
func (r *runner) isServer(pid int, dataDir string, spec ServerSpec) (bool, error) {
	if !r.deps.Sys.Alive(pid) {
		return false, nil
	}
	p, err := r.deps.Sys.Inspect(pid)
	if err != nil {
		return false, fmt.Errorf("pid %d cannot be read: %w", pid, err)
	}
	if !sameFile(p.Exe, spec.Exe) || !slices.Equal(p.Argv, spec.Argv) || !recordsPID(dataDir, pid) {
		return false, fmt.Errorf("pid %d is no longer this server", pid)
	}
	return true, nil
}

// stop sends SIGTERM and waits for the exit; it never kills.
func (r *runner) stop(ctx context.Context, pid int) bool {
	if err := r.deps.Sys.Signal(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return false
	}
	deadline := r.deps.Now().Add(r.deps.StopWait)
	for r.deps.Sys.Alive(pid) {
		if !r.deps.Now().Before(deadline) {
			return false
		}
		if r.deps.Sleep(ctx, 250*time.Millisecond) != nil {
			return false
		}
	}
	return true
}

// swapProcess stops the server, archives its data, replaces the binary and
// starts it again, then checks it (Decisions 8 and 9).
func (j *job) swapProcess(ctx context.Context, p *plan) {
	r, st, rep := j.r, j.st, j.rep
	ps := st.Process
	if ok, err := r.isServer(ps.PID, ps.DataDir, j.spec); !ok {
		why := "it is no longer running"
		if err != nil {
			why = err.Error()
		}
		rep.ExitCode, rep.Summary = exitRefused, "Refused, and nothing changed: server pid "+strconv.Itoa(ps.PID)+" changed since the plan: "+why
		r.discard(j.dir)
		rep.Run = nil
		return
	}
	ctx, cancel := afterStop(ctx)
	defer cancel()
	if err := j.step(stepStopSent); err != nil {
		j.stuck("could not record the run's state: " + err.Error())
		return
	}
	if !r.stop(ctx, ps.PID) {
		rep.ExitCode = exitStuck
		rep.Summary = fmt.Sprintf("Server pid %d was asked to stop and has not stopped in %s; it may still. Nothing else changed.", ps.PID, r.deps.StopWait)
		rep.Next = append(rep.Next, "when it has stopped, start it again as it was: "+j.upgradeCmd("--back "+st.Run))
		return
	}
	_ = j.step(stepStopped)
	j.done("stopped server pid %d", ps.PID)

	archive := filepath.Join(j.dir, "data.tar.gz")
	a, err := writeArchive(ps.DataDir, archive)
	if err == nil {
		afterArchive(archive)
		err = verifyArchive(archive, a)
	}
	if err != nil {
		j.goBack(ctx, "the archive of "+ps.DataDir+" failed: "+err.Error())
		return
	}
	st.Archive = &a
	_ = j.step(stepArchived)
	j.done("archived %s into %s and read it back whole", ps.DataDir, archive)

	if p.replaceBinary {
		if err := r.putInPlace(ctx, filepath.Join(j.dir, "tracepad-"+p.to), st.Binary.Path, p.to); err != nil {
			j.goBack(ctx, "tracepad "+p.to+" could not be put in place: "+err.Error())
			return
		}
		_ = j.step(stepBinaryReplaced)
		j.done("put tracepad %s at %s", p.to, st.Binary.Path)
	}

	if err := j.startServer(); err != nil {
		j.goBack(ctx, "tracepad "+p.to+" did not start: "+err.Error())
		return
	}
	_ = j.step(stepStarted)
	j.done("started tracepad %s as pid %d, with the old server's arguments and environment", p.to, ps.NewPID)

	exited := j.newProc.Exited()
	c := r.check(ctx, ps.URL, p.to, st.CountBefore, func() bool {
		select {
		case <-exited:
			return false
		default:
			return true
		}
	})
	c.LogLine = firstLogLine(ps.Log, ps.LogOffset)
	j.verdict(ctx, c)
}

// startServer starts the installed binary with the old server's arguments,
// environment and working directory, its output appended to the old log.
func (j *job) startServer() error {
	ps := j.st.Process
	if info, err := os.Stat(ps.Log); err == nil {
		ps.LogOffset = info.Size()
	} else {
		ps.LogOffset = 0
	}
	started, err := j.r.deps.Sys.Start(StartSpec{Path: j.spec.Exe, Argv: j.spec.Argv, Env: j.spec.Env, Dir: j.spec.Dir, Log: ps.Log})
	if err != nil {
		return err
	}
	j.newProc = started
	oldPID := ps.PID
	ps.NewPID = started.PID()
	pidFile := filepath.Join(ps.DataDir, "server.pid")
	if b, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(b)) == strconv.Itoa(oldPID) {
		if writeFileAtomic(pidFile, []byte(strconv.Itoa(ps.NewPID)+"\n")) == nil {
			ps.PIDFile = true
		}
	}
	return nil
}

// verdict acts on a check of the new version (Decision 9).
func (j *job) verdict(ctx context.Context, c Checked) {
	st, rep := j.st, j.rep
	rep.Check = &c
	st.Verdict = c.Verdict
	_ = j.step(stepChecked)
	switch c.Verdict {
	case verdictHealthy:
		rep.ExitCode = exitOK
		rep.Summary = fmt.Sprintf("Upgraded to %s, healthy.", st.To)
		if st.Kind == kindContainer {
			j.replaceHostBinary(ctx)
		}
		if st.Binary != nil && st.Binary.Path != "" {
			j.r.reinstallSkill(ctx, rep, st.Binary.Path, st.To)
			_ = j.step(stepSkill)
		}
		rep.Next = append(rep.Next, "to go back to "+st.From+", dropping what arrived since: "+j.upgradeCmd("--back "+st.Run))
	case verdictDecide:
		rep.ExitCode = exitDecide
		rep.Summary = fmt.Sprintf("Upgraded to %s; %s. Nothing was reverted: keeping it or going back is yours to decide.", st.To, c.Why)
		rep.Person = append(rep.Person, "keep "+st.To+" (check again later: "+j.upgradeCmd("--check "+st.Run)+
			"), or go back to "+st.From+", which drops what arrived since: "+j.upgradeCmd("--back "+st.Run))
	default:
		j.goBack(ctx, "not healthy: "+c.Why)
	}
}

// goBack takes the way back from inside the upgrade, on a failure after the
// stop or a check that says not healthy.
func (j *job) goBack(ctx context.Context, why string) {
	j.done("%s; the way back runs", why)
	outcome := j.wayBack(ctx)
	switch {
	case outcome.ok && !outcome.unconfirmed:
		j.rep.ExitCode = exitWentBack
		j.rep.Summary = fmt.Sprintf("Not upgraded: %s. The way back ran, and %s runs again, healthy.", why, j.st.From)
		return
	case outcome.ok:
		j.rep.ExitCode = exitStuck
		j.rep.Summary = fmt.Sprintf("Not upgraded: %s. The way back ran and started %s again, but it has not shown it is healthy: %s.", why, j.st.From, outcome.why)
		return
	}
	j.rep.ExitCode = exitStuck
	j.rep.Summary = fmt.Sprintf("Not upgraded: %s. The way back did not finish: %s", why, outcome.why)
}

// upgradeCmd is how to run the command later: the installed binary when it
// has the command, else the copy of this one the run kept — after a way back
// to a release from before the command, the installed binary does not know
// the word.
func (j *job) upgradeCmd(args string) string {
	if b := j.st.Binary; b != nil && b.Path != "" {
		if exec.Command(b.Path, "upgrade", "--help").Run() == nil {
			return "tracepad upgrade " + args
		}
	}
	return shq(filepath.Join(j.dir, "upgrader")) + " upgrade " + args
}

// refresh brings the report's lines for the binary and the target to what
// they are after the run.
func (j *job) refresh(ctx context.Context) {
	rep, r := j.rep, j.r
	if rep.Binary != nil {
		if v, err := r.deps.Version(ctx, rep.Binary.Path); err == nil {
			rep.Binary.Version = v
		}
	}
	for i := range rep.Servers {
		s := &rep.Servers[i]
		if !s.Target || j.st.Process == nil {
			continue
		}
		if j.st.Process.NewPID > 0 {
			s.PID = j.st.Process.NewPID
		}
		s.Version, _ = health(ctx, r.deps.HTTP, j.st.Process.URL)
	}
	for i := range rep.Containers {
		c := &rep.Containers[i]
		if c.Target && j.st.Container != nil {
			c.Version, _ = health(ctx, r.deps.HTTP, j.st.Container.URL)
		}
	}
}

func (j *job) stuck(why string) {
	j.rep.ExitCode = exitStuck
	j.rep.Summary = "Stuck: " + why
}

// swapBinaryOnly replaces the installed binary when no server of the
// command's runs it.
func (j *job) swapBinaryOnly(ctx context.Context) {
	r, st, rep := j.r, j.st, j.rep
	if err := r.putInPlace(ctx, filepath.Join(j.dir, "tracepad-"+st.To), st.Binary.Path, st.To); err != nil {
		rep.ExitCode, rep.Summary = exitRefused, "Refused, and nothing changed: "+err.Error()
		r.discard(j.dir)
		rep.Run = nil
		return
	}
	_ = j.step(stepBinaryReplaced)
	j.done("put tracepad %s at %s", st.To, st.Binary.Path)
	r.reinstallSkill(ctx, rep, st.Binary.Path, st.To)
	_ = j.step(stepSkill)
	rep.ExitCode = exitOK
	rep.Summary = fmt.Sprintf("Upgraded the binary to %s; no server of the command's runs it.", st.To)
	rep.Next = append(rep.Next, "to put "+st.Binary.From+" back: "+j.upgradeCmd("--back "+st.Run))
}

// replaceHostBinary brings the host's CLI to the container's version after a
// healthy container upgrade.
func (j *job) replaceHostBinary(ctx context.Context) {
	st := j.st
	if st.Binary == nil || st.Binary.Old == "" {
		return
	}
	if err := j.r.putInPlace(ctx, filepath.Join(j.dir, "tracepad-"+st.To), st.Binary.Path, st.To); err != nil {
		j.rep.Notes = append(j.rep.Notes, "the binary at "+st.Binary.Path+" was not replaced: "+err.Error())
		return
	}
	_ = j.step(stepBinaryReplaced)
	j.done("put tracepad %s at %s", st.To, st.Binary.Path)
}

// reinstallSkill installs the skill again, with the binary at bin when it is
// version to, wherever a copy carries its marker (Decision 12).
func (r *runner) reinstallSkill(ctx context.Context, rep *Report, bin, to string) {
	if v, err := r.deps.Version(ctx, bin); err != nil || v != to {
		return
	}
	targets := []struct {
		marker string
		args   []string
	}{
		{filepath.Join(r.deps.Home, ".claude", "skills", "tracepad", ".version"), []string{"--dir", filepath.Join(r.deps.Home, ".claude", "skills")}},
		{filepath.Join(r.deps.Home, ".agents", "skills", "tracepad", ".version"), []string{"--dir", filepath.Join(r.deps.Home, ".agents", "skills")}},
		{filepath.Join(r.deps.Cwd, ".claude", "skills", "tracepad", ".version"), []string{"--project"}},
	}
	for _, t := range targets {
		if _, err := os.Stat(t.marker); err != nil {
			continue
		}
		out, err := r.deps.Skills(ctx, bin, r.deps.Cwd, t.args...)
		if err != nil {
			rep.Notes = append(rep.Notes, fmt.Sprintf("the skill at %s was not installed again: %s", filepath.Dir(filepath.Dir(t.marker)), firstLine(out)))
			continue
		}
		rep.Done = append(rep.Done, "skill: "+firstLine(out))
	}
}

// privacy says what a backup is (Decision 14), for every run that wrote one.
func (j *job) privacy() {
	if j.st.Kind == kindBinary {
		return
	}
	if _, err := os.Stat(filepath.Join(j.dir, "data.tar.gz")); err != nil {
		return
	}
	j.rep.SetAside = append([]string{j.dir}, j.st.SetAside...)
	j.rep.Person = append(j.rep.Person, backupsSentence(j.dir, j.st))
}

func backupsSentence(dir string, st *State) string {
	// Containers before volumes: docker refuses to remove a volume that any
	// container, a stopped one too, still mounts (the review of #1).
	dirs, containers, volumes := []string{"rm -r " + shq(dir)}, []string{}, []string{}
	for _, s := range st.SetAside {
		switch {
		case strings.HasPrefix(s, "container "):
			containers = append(containers, "docker rm "+shq(strings.TrimPrefix(s, "container ")))
		case strings.HasPrefix(s, "volume "):
			volumes = append(volumes, "docker volume rm "+shq(strings.TrimPrefix(s, "volume ")))
		default:
			dirs = append(dirs, "rm -r "+shq(s))
		}
	}
	cmds := append(append(dirs, containers...), volumes...)
	return dir + " is a full copy of the database — every prompt and completion — and of the server's environment, secrets included. " +
		"It stays until someone deletes it, and erasing traces or a user reaches neither it nor what a way back set aside. " +
		"Once the new version has run for a while, remove it and what is set aside: " + strings.Join(cmds, "; ")
}

// shq quotes a word for a POSIX shell when it needs it: a command the report
// hands on is pasted as written, and an unquoted path with a space (macOS's
// "Application Support") would run against other paths.
func shq(word string) string {
	if word != "" && strings.Trim(word, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./:@%+=,-") == "" {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// readSpec reads server.json.
func readSpec(dir string) (ServerSpec, error) {
	var s ServerSpec
	b, err := os.ReadFile(filepath.Join(dir, "server.json"))
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, err
	}
	if !filepath.IsAbs(s.Exe) || len(s.Argv) == 0 || !filepath.IsAbs(s.Dir) {
		return s, errors.New("server.json is not one a run writes")
	}
	return s, nil
}
