package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/store"
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
	// held are the database locks the command holds, by data directory:
	// from the stop until a server starts on the data, and through a way
	// back (spec 054 #26, #28, #32).
	held map[string]func()
}

// hold takes the lock of dataDir's database, as a server takes it, and
// keeps it until release: a server a shell loop or a supervisor brings back
// while the command archives or swaps the data cannot take the database and
// write into it. ok is false when another process holds it. Holding it
// already is ok.
func (j *job) hold(dataDir string) (ok bool, err error) {
	if j.holds(dataDir) {
		return true, nil
	}
	release, ok, err := store.TryLock(filepath.Join(dataDir, dataDBName))
	if err != nil || !ok {
		return ok, err
	}
	if j.held == nil {
		j.held = map[string]func(){}
	}
	j.held[dataDir] = release
	return true, nil
}

func (j *job) holds(dataDir string) bool {
	_, ok := j.held[dataDir]
	return ok
}

// letGo lets go of one directory's lock.
func (j *job) letGo(dataDir string) {
	if release, ok := j.held[dataDir]; ok {
		release()
		delete(j.held, dataDir)
	}
}

// release lets go of every lock the command holds, when it ends.
func (j *job) release() {
	for dir := range j.held {
		j.letGo(dir)
	}
}

// at marks where the act a step records happens, for the fault matrix: the
// error a test injects there is the act's failure. Its points are the
// table's steps (#31).
func (j *job) at(step string) error {
	if j.r.deps.Fault == nil {
		return nil
	}
	return j.r.deps.Fault(step)
}

// unstep takes back a step recorded ahead of an act that then did not happen.
func (j *job) unstep(name string) {
	steps := j.st.Steps[:0]
	for _, s := range j.st.Steps {
		if s.Name != name {
			steps = append(steps, s)
		}
	}
	j.st.Steps = steps
	_ = j.st.save(j.dir)
}

// step records a step, along the run's table, and clears the intent it
// records the act of. A step off the table is never written (#34): the run
// stops there, stuck, and says what it was about to record.
func (j *job) step(name string) error {
	if last := j.st.last(); !allows(j.st.Kind, last, name) {
		badStep(j.st.Kind, last, name)
		panic(offTable{kind: j.st.Kind, last: last, next: name})
	}
	// The matrix's kill: the act done, its record not yet written.
	if j.r.deps.Fault != nil {
		_ = j.r.deps.Fault(name + recordPoint)
	}
	if p := j.st.Pending; p != nil && p.Step == name {
		j.st.Pending = nil
	}
	j.st.Steps = append(j.st.Steps, Step{Name: name, At: j.r.deps.Now().UTC()})
	return j.st.save(j.dir)
}

// recordPoint marks, for the fault matrix, the moment between an act and
// the write of the step that records it.
const recordPoint = "/record"

// intend writes what is about to happen before it happens (#34): a run cut
// short between an act and its step is settled from it on its next load.
func (j *job) intend(step, format string, args ...any) error {
	j.st.Pending = &Intent{Step: step, What: fmt.Sprintf(format, args...)}
	return j.st.save(j.dir)
}

// act is one act of a step (#34): what it will do written first, then its
// point in the fault matrix, then the act; an act that failed takes its
// intent back, and what it may have left is the next path's to meet.
func (j *job) act(step, what string, do func() error) error {
	if err := j.intend(step, "%s", what); err != nil {
		return err
	}
	err := j.at(step)
	if err == nil {
		err = do()
	}
	if err != nil {
		j.drop()
	}
	return err
}

// drop takes back an intent whose act did not happen.
func (j *job) drop() {
	if j.st.Pending != nil {
		j.st.Pending = nil
		_ = j.st.save(j.dir)
	}
}

// offTable is a step the command was about to record off its run's table:
// a fault in the command, which ends the run stuck rather than write it.
type offTable struct{ kind, last, next string }

func (e offTable) Error() string {
	return fmt.Sprintf("the command was about to record %q after %q, which a %s run's steps do not allow; nothing was recorded, and the run stops here. Report this; --back and --check still read the run as it was", e.next, e.last, e.kind)
}

// stopOffTable turns an off-table step into the run's end: exit 5, said.
func stopOffTable(rep *Report) {
	if v := recover(); v != nil {
		e, ok := v.(offTable)
		if !ok {
			panic(v)
		}
		rep.ExitCode, rep.Summary = exitStuck, "Stuck: "+e.Error()
	}
}

func (j *job) done(format string, args ...any) {
	j.rep.Done = append(j.rep.Done, fmt.Sprintf(format, args...))
}

// upgrade is the default mode.
func (r *runner) upgrade(ctx context.Context) (rep *Report) {
	rep = &Report{Mode: "upgrade"}
	defer stopOffTable(rep)
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
		code, summary := verdictOf(p)
		rep.ExitCode, rep.Summary = code, "Nothing upgraded by this run. "+summary
		if bin.Ours {
			r.reinstallSkill(ctx, rep, bin.Path, p.to)
		}
		rep.Next = append(rep.Next, laterOnly(next)...)
		return rep
	}

	release, ok := r.lockRuns(rep)
	if !ok {
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
	// From here a run changes what runs; an interrupt must not leave a
	// server or a container down half way (the second review), so every swap
	// and its way back finish on a context of their own. Everything before
	// this — downloads, pulls, the archive's room — stays interruptible.
	ctx, cancel := afterStop(ctx)
	defer cancel()
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
// a refusal with nothing changed, and upgrade removes the run directory it
// made (discard): before the stop it holds only what this run fetched and
// wrote, the server's environment among it.
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
		old := filepath.Join(dir, "tracepad-"+bin.Version)
		if err := r.keepCopy(ctx, bin.Path, old, bin.Version); err != nil {
			return nil, fmt.Sprintf("no copy of the installed %s to go back to: %v", bin.Version, err)
		}
		st.Binary.Old = old
		// Before anything stops (the sixth review): a server running the
		// installed binary at a later version refuses the run here, not
		// after its server is down. The replacement checks again.
		var own []int
		if p.server != nil {
			own = append(own, p.server.Proc.PID)
		}
		if err := r.nothingNewerRuns(ctx, bin.Path, p.to, own...); err != nil {
			return nil, err.Error()
		}
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

// lockRuns takes the lock every mode that changes or reads a run holds, and
// writes its one refusal (the fourth review).
func (r *runner) lockRuns(rep *Report) (release func(), ok bool) {
	if err := os.MkdirAll(r.deps.Backups, 0o700); err != nil {
		rep.ExitCode, rep.Summary = exitRefused, "Refused: could not make "+r.deps.Backups+": "+err.Error()
		return nil, false
	}
	if err := private(r.deps.Backups); err != nil {
		rep.ExitCode, rep.Summary = exitRefused, "Refused: "+err.Error()
		return nil, false
	}
	release, ok, err := store.TryLock(filepath.Join(r.deps.Backups, "runs"))
	switch {
	case err != nil:
		rep.ExitCode, rep.Summary = exitRefused, "Refused: could not lock "+r.deps.Backups+": "+err.Error()
		return nil, false
	case !ok:
		rep.ExitCode, rep.Summary = exitRefused, "Refused: another tracepad upgrade is running."
		return nil, false
	}
	return release, true
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

// afterStop is the context a swap takes once it may stop a server: an
// interrupt (Ctrl-C, an agent's timeout) must not leave it down half way. It
// has no deadline of its own — an archive takes as long as the data does, and
// its steps that wait (a stop, a check) have theirs — and the way back never
// runs on what it leaves: wayBackContext gives it a budget of its own (the
// third review).
func afterStop(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(context.WithoutCancel(ctx))
}

// wayBackBudget is the time a way back has, fresh, whatever the swap before
// it took: a restore of the archive, a stop, a start and a check.
const wayBackBudget = 30 * time.Minute

// wayBackContext is a way back's own context: not cancelled by an interrupt,
// and with its own budget.
func wayBackContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), wayBackBudget)
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
	if err == nil {
		err = syncFile(out)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// replaceBinary puts version at the install path, after the one check every
// replacement makes, an upgrade's or a way back's (spec 054 #26): nothing
// may run from that binary at a version later than the one put there —
// a server's next restart would be an older binary over a database a newer
// one migrated, the downgrade the command refuses everywhere else. The run's
// own server, which it stops and starts itself, is not counted.
func (j *job) replaceBinary(ctx context.Context, src, version string) error {
	if err := j.r.nothingNewerRuns(ctx, j.st.Binary.Path, version, j.ownPIDs()...); err != nil {
		return err
	}
	return j.r.putInPlace(ctx, src, j.st.Binary.Path, version)
}

func (j *job) ownPIDs() []int {
	if ps := j.st.Process; ps != nil {
		return []int{ps.PID, ps.NewPID, ps.BackPID}
	}
	return nil
}

// nothingNewerRuns is that check: every server running the binary at path,
// found as the plan finds them, answers a version no later than version. One
// that does not answer cannot be shown to be safe, and refuses too.
func (r *runner) nothingNewerRuns(ctx context.Context, path, version string, own ...int) error {
	procs, _, err := r.deps.Sys.Candidates()
	if err != nil {
		return fmt.Errorf("the processes could not be listed to check that nothing newer runs from %s: %w", path, err)
	}
	for _, p := range procs {
		if slices.Contains(own, p.PID) || !sameFile(p.Exe, path) {
			continue
		}
		dataDir, listen, err := resolveServer(p)
		if errors.Is(err, errNotServer) {
			continue
		}
		v := ""
		if url, ok := loopbackURL(listen); ok && err == nil {
			hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			v, _ = health(hctx, r.deps.HTTP, url)
			cancel()
		}
		order, known := Compare(v, version)
		switch {
		case !known:
			return fmt.Errorf("server pid %d runs %s on %s and does not say a release's version, so putting %s there cannot be shown to be safe; stop it, or upgrade it first", p.PID, path, dataDir, version)
		case order > 0:
			return fmt.Errorf("server pid %d runs %s at %s on %s: its next restart would be %s over a database %s may have migrated, which an older binary does not open. Stop it first, or keep %s", p.PID, path, v, dataDir, version, v, v)
		}
	}
	return nil
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
		if err != nil {
			return fmt.Errorf("the binary does not run at %s (%v)", filepath.Dir(dst), err)
		}
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
	if info, err := os.Stat(j.spec.Dir); j.spec.Dir == "" || err != nil || !info.IsDir() {
		// Its working directory is gone (a temporary directory, a worktree
		// removed since): it would not start there again. It starts in its
		// data directory — unless an argument is relative to the directory
		// that is gone.
		if relativeDataDir(s.Proc) {
			return fmt.Sprintf("server pid %d's working directory %s is gone, and its arguments name paths relative to it: restart it yourself", s.Proc.PID, s.Proc.Cwd)
		}
		if s.Proc.Cwd != "" {
			j.rep.Notes = append(j.rep.Notes, fmt.Sprintf("server pid %d's working directory %s is gone; it starts again in its data directory, %s", s.Proc.PID, s.Proc.Cwd, s.DataDir))
		}
		j.spec.Dir = s.DataDir
	}
	if err := writeJSON(filepath.Join(j.dir, "server.json"), j.spec); err != nil {
		return err.Error()
	}
	return ""
}

// relativeDataDir says whether a server's data directory, from whichever
// source it came — --data-dir, TRACEPAD_DATA_DIR, XDG_DATA_HOME, HOME — is a
// relative path: one that means something only in the directory it was
// started in (the final review).
func relativeDataDir(p Process) bool {
	args, ok := serverFlags(p.Argv)
	if !ok {
		return false
	}
	flags, err := config.ParseFlags(args)
	if err != nil {
		return true
	}
	if d, given := flags.Given("data-dir"); given && d != "" {
		return !filepath.IsAbs(d)
	}
	for _, key := range []string{"TRACEPAD_DATA_DIR", "XDG_DATA_HOME", "HOME"} {
		if v := p.Getenv(key); v != "" {
			return !filepath.IsAbs(v)
		}
	}
	return true
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
	return r.signal(pid) == nil && r.waitGone(ctx, pid)
}

// signal asks a server to stop; one already gone counts as asked.
func (r *runner) signal(pid int) error {
	if err := r.deps.Sys.Signal(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
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
	if err := j.step(stepStopSent); err != nil {
		j.stuck("could not record the run's state: " + err.Error())
		return
	}
	err := j.at(stepStopSent)
	if err == nil {
		err = r.signal(ps.PID)
	}
	if err != nil {
		// Not asked to stop after all: nothing changed, and nothing waits.
		j.unstep(stepStopSent)
		rep.ExitCode, rep.Summary = exitRefused, "Refused, and nothing changed: server pid "+strconv.Itoa(ps.PID)+" could not be asked to stop: "+err.Error()
		r.discard(j.dir)
		rep.Run = nil
		return
	}
	if err := j.at(stepStopped); err != nil || !r.waitGone(ctx, ps.PID) {
		rep.ExitCode = exitStuck
		rep.Summary = fmt.Sprintf("Server pid %d was asked to stop and has not stopped in %s; it may still. Nothing else changed.", ps.PID, r.deps.StopWait)
		rep.Next = append(rep.Next, "when it has stopped, start it again as it was: "+j.upgradeCmd("--back "+st.Run))
		return
	}
	_ = j.step(stepStopped)
	j.done("stopped server pid %d", ps.PID)
	// From the stop the command holds the database, so a server brought
	// back by something else cannot write into what it archives (the fourth
	// review); it lets go only to start a server on it (#32).
	defer j.release()
	if ok, err := j.hold(ps.DataDir); !ok {
		why := "the data directory's lock could not be taken"
		if err == nil {
			holder, _ := lockedBy(ps.DataDir)
			why = fmt.Sprintf("pid %d took %s after the stop (a supervisor, a shell loop?)", holder, ps.DataDir)
		} else {
			why += ": " + err.Error()
		}
		j.goBack(ctx, why)
		return
	}

	archive := filepath.Join(j.dir, "data.tar.gz")
	var a Archived
	err = j.act(stepArchived, "archive "+ps.DataDir+" into "+archive, func() (err error) {
		a, err = writeArchive(ps.DataDir, archive)
		if err == nil {
			afterArchive(archive)
			err = verifyArchive(archive, a)
		}
		return err
	})
	if err != nil {
		j.goBack(ctx, "the archive of "+ps.DataDir+" failed: "+err.Error())
		return
	}
	st.Archive = &a
	_ = j.step(stepArchived)
	j.done("archived %s into %s and read it back whole", ps.DataDir, archive)

	if p.replaceBinary {
		if err := j.putNew(ctx); err != nil {
			j.goBack(ctx, "tracepad "+p.to+" could not be put in place: "+err.Error())
			return
		}
	}

	var started Started
	err = j.act(stepStarted, "start tracepad "+p.to+" on "+ps.DataDir, func() (err error) {
		started, err = j.launch(ctx, true)
		return err
	})
	if err != nil {
		j.goBack(ctx, "tracepad "+p.to+" did not start: "+err.Error())
		return
	}
	ps.NewPID = started.PID()
	_ = j.step(stepStarted)
	j.done("started tracepad %s as pid %d, with the old server's arguments and environment", p.to, ps.NewPID)

	if err := j.at(stepChecked); err != nil {
		j.goBack(ctx, "not healthy: "+err.Error())
		return
	}
	c := r.check(ctx, ps.URL, p.to, st.CountBefore, running(started))
	c.LogLine = firstLogLine(ps.Log, ps.LogOffset)
	// What answered must have opened this data directory: the lock of it
	// records the new server. A server that resolved its data elsewhere — a
	// relative path in its environment, say — answers healthy on an empty
	// database (the final review).
	if c.Verdict != verdictNotHealthy && c.Health != "" && !recordsPID(ps.DataDir, ps.NewPID) {
		c.Verdict, c.Why = verdictNotHealthy, fmt.Sprintf("the new server answers, but did not open %s", ps.DataDir)
	}
	j.verdict(ctx, c)
}

// putNew puts the run's new binary at the install path, recording first that
// it may be there: a way back that meets a server on the data reads from the
// steps alone which binary that server may have started from (#31).
func (j *job) putNew(ctx context.Context) error {
	st := j.st
	if st.last() != stepBinaryReplacing {
		if err := j.step(stepBinaryReplacing); err != nil {
			return err
		}
	}
	err := j.at(stepBinaryReplacing)
	if err == nil {
		err = j.replaceBinary(ctx, filepath.Join(j.dir, "tracepad-"+st.To), st.To)
	}
	if err != nil {
		return err
	}
	_ = j.step(stepBinaryReplaced)
	j.done("put tracepad %s at %s", st.To, st.Binary.Path)
	return nil
}

// launch is the one way the command starts a server on the data (#32). The
// command holds the data's lock — it takes it here when it does not — so
// nothing else is on the data; it lets go of it only for the start itself,
// because a server takes its database's lock and refuses one held, and an
// flock cannot be handed to a child that opens the file itself. A start
// that fails takes the lock back for the way back; a child that exits takes
// it back too; and the lock must then be the child's, or what took it in
// that moment is named. newVersion is the run's new version, which may
// write into the data once it has the lock: that is recorded before the
// lock is let go.
func (j *job) launch(ctx context.Context, newVersion bool) (Started, error) {
	ps := j.st.Process
	if ok, err := j.hold(ps.DataDir); !ok {
		if err != nil {
			return nil, fmt.Errorf("the lock of %s could not be taken: %w", ps.DataDir, err)
		}
		holder, _ := lockedBy(ps.DataDir)
		return nil, fmt.Errorf("pid %d holds %s", holder, ps.DataDir)
	}
	if info, err := os.Stat(ps.Log); err == nil {
		ps.LogOffset = info.Size()
	} else {
		ps.LogOffset = 0
	}
	if newVersion {
		ps.WroteAfterSwap = true
	}
	if err := j.st.save(j.dir); err != nil {
		return nil, err
	}
	j.letGo(ps.DataDir)
	started, err := j.r.deps.Sys.Start(StartSpec{Path: j.spec.Exe, Argv: j.spec.Argv, Env: j.spec.Env, Dir: j.spec.Dir, Log: ps.Log})
	if err != nil {
		_, _ = j.hold(ps.DataDir)
		return nil, err
	}
	j.newProc = started
	if newVersion {
		ps.NewPID = started.PID()
	} else {
		ps.BackPID = started.PID()
	}
	_ = j.st.save(j.dir)
	// The child holds the data now, or it does not and the command holds it
	// again: one that exited, or opened another directory, leaves the data
	// to the way back and its check says why. Only something else that took
	// the lock in that moment stops here.
	j.r.waitRecorded(ctx, started.PID(), ps.DataDir)
	if !(j.r.deps.Sys.Alive(started.PID()) && lockHeld(ps.DataDir) && recordsPID(ps.DataDir, started.PID())) {
		if ok, _ := j.hold(ps.DataDir); !ok {
			holder, _ := lockedBy(ps.DataDir)
			return nil, fmt.Errorf("pid %d took %s in the moment it was free for the start", holder, ps.DataDir)
		}
	}
	pidFile := filepath.Join(ps.DataDir, "server.pid")
	if b, err := os.ReadFile(pidFile); err == nil {
		named := strings.TrimSpace(string(b))
		for _, pid := range []int{ps.PID, ps.NewPID, ps.BackPID} {
			if pid > 0 && named == strconv.Itoa(pid) {
				_ = writeFileAtomic(pidFile, []byte(strconv.Itoa(started.PID())+"\n"))
				break
			}
		}
	}
	return started, nil
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
		j.finishHealthy(ctx)
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

// finishHealthy is what a healthy new version is followed by, whichever check
// found it healthy — the upgrade's own, or a later --check after a decide
// (the fourth review): the host's binary for a container run, and the skill.
func (j *job) finishHealthy(ctx context.Context) {
	st := j.st
	if st.has(stepSkill) {
		return
	}
	if st.Kind == kindContainer && !st.has(stepBinaryReplaced) {
		j.replaceHostBinary(ctx)
	}
	if st.Binary != nil && st.Binary.Path != "" {
		j.r.reinstallSkill(ctx, j.rep, st.Binary.Path, st.To)
		_ = j.step(stepSkill)
	}
}

// goBack takes the way back from inside the upgrade, on a failure after the
// stop or a check that says not healthy.
func (j *job) goBack(ctx context.Context, why string) {
	j.done("%s; the way back runs", why)
	// The data stays held: the way back lets go only right before it starts
	// the old version, so nothing — a supervisor, a shell loop — runs the
	// new binary on data it has not restored (the final review).
	ctx, cancel := wayBackContext(ctx)
	defer cancel()
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

// upgradeCmd is how to run the command later on this run: its own copy of
// the binary that ran it, by its absolute path (the second review). A bare
// `tracepad` could be another binary first on PATH, or after a way back a
// release from before the command; the copy is the code that wrote the
// state, and stays with it.
func (j *job) upgradeCmd(args string) string {
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
		// What runs after the run: the old version a way back started, else
		// the new one (Decision 19 (d), the third review).
		switch {
		case j.st.Process.BackPID > 0:
			s.PID = j.st.Process.BackPID
		case j.st.Process.NewPID > 0:
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
	if err := j.putNew(ctx); err != nil {
		rep.ExitCode, rep.Summary = exitRefused, "Refused, and nothing changed: "+err.Error()
		r.discard(j.dir)
		rep.Run = nil
		return
	}
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
	if err := j.putNew(ctx); err != nil {
		j.rep.Notes = append(j.rep.Notes, "the binary at "+st.Binary.Path+" was not replaced: "+err.Error())
	}
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

// loadSpec reads the run's server.json, which must start the installed
// binary: a run's files name nothing else to run (#30).
func (j *job) loadSpec() (ServerSpec, error) {
	spec, err := readSpec(j.dir)
	if err != nil {
		return spec, err
	}
	if j.st.Binary == nil || !sameFile(spec.Exe, j.st.Binary.Path) {
		return spec, errors.New("server.json starts another binary than the installed one")
	}
	return spec, nil
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
