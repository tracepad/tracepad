package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/store"
)

// Binary is the installed binary: what the install script and this command
// put at <install dir>/tracepad.
type Binary struct {
	Path   string
	Exists bool
	// Kind is what it is, which the plan's table (binaryTodo) turns into
	// what is printed and whether it is the person's to do.
	Kind    binaryKind
	Version string
	// Link is where the install path links to, when it is a link.
	Link string
	// Ours is whether the command may replace it (Kind binOurs); Reason
	// says why not.
	Ours   bool
	Reason string
	// First is the `tracepad` first on PATH when that is another one, and
	// what it says it is.
	First        string
	FirstVersion string
	FirstErr     error
}

// binaryKind is what the binary at the install path is, one of a closed set
// decided in one place, installedBinary, in this order: a package manager's
// before anything else, so a link into its tree is never taken for a
// person's link or a development build (the fifth review of #228).
type binaryKind int

const (
	binNone       binaryKind = iota // nothing there
	binOdd                          // not a file, or not read
	binPackaged                     // a package manager's: a file or a link in its tree
	binLinked                       // a link of the person's
	binSilent                       // a file that does not say its version
	binDev                          // a file that says a version no release has
	binUnwritable                   // a release's file in a directory this user cannot write
	binOurs                         // a release's file the command replaces
)

// Server is a `tracepad serve` process this user runs.
type Server struct {
	Proc    Process
	DataDir string
	Listen  string
	URL     string
	// Version is what /health answers at URL, empty when nothing answered.
	Version string
	Ours    bool
	Reason  string
	// Unchecked says why its version was not asked: it listens on an
	// address of this machine's that is not a loopback one (a single
	// interface's), which the command does not ask. Said as not checked,
	// never as a version it does not say.
	Unchecked string
}

// Findings is what the plan saw.
type Findings struct {
	Binary     Binary
	Servers    []Server
	Containers []Container
	Probe      Probe
	// Notes say what could not be looked at, so that "none found" is never
	// a larger claim than the search.
	Notes []string
	// complete is whether everything was looked at: every candidate process
	// and Docker. processes is whether every candidate process could be
	// read — what a binary's replacement needs (#39).
	complete, processes bool
}

// Probe is what answers at the default address.
type Probe struct {
	URL     string `json:"url"`
	Version string `json:"version"`
	// Whose: "server pid N", "container NAME", "not this install's", or
	// "could not tell whose".
	Whose string `json:"whose"`
}

// serverFlags are a process's server arguments: the words after the binary,
// without the command when it names `serve`. ok is false for any other
// command (`tracepad mcp`, `tracepad tail`), which is not a server.
func serverFlags(argv []string) (flags []string, ok bool) {
	if len(argv) == 0 {
		return nil, false
	}
	args := argv[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		if args[0] != "serve" {
			return nil, false
		}
		args = args[1:]
	}
	return args, true
}

// resolveServer works out a server process's data directory and listen
// address the way the server itself does, and joins a relative data
// directory to its working directory.
func resolveServer(p Process) (dataDir, listen string, err error) {
	dataDir, listen, err = configuredDirs(p)
	if err != nil {
		return "", "", err
	}
	if !filepath.IsAbs(dataDir) {
		if p.Cwd == "" {
			return "", "", fmt.Errorf("its data directory %q is relative, and its working directory could not be read", dataDir)
		}
		dataDir = filepath.Join(p.Cwd, dataDir)
	}
	if real, err := filepath.EvalSymlinks(dataDir); err == nil {
		dataDir = real
	}
	return filepath.Clean(dataDir), listen, nil
}

// configuredDirs is a server's data directory and listen address as its
// configuration gives them, before anything is joined: the flag, else its
// environment, else the default — the order the server reads, in one place
// (the eighth review: a second copy of it drifts).
func configuredDirs(p Process) (dataDir, listen string, err error) {
	args, ok := serverFlags(p.Argv)
	if !ok {
		return "", "", errNotServer
	}
	flags, err := config.ParseFlags(args)
	if err != nil {
		return "", "", fmt.Errorf("its arguments are not a server's: %w", err)
	}
	// As the server reads them (config.FromEnv): a flag given wins, empty
	// too (the tenth review); then the environment's value when it is not
	// empty; then the default.
	or := func(flag, env, def string) string {
		if v, given := flags.Given(flag); given {
			return v
		}
		if v := p.Getenv(env); v != "" {
			return v
		}
		return def
	}
	return or("data-dir", "TRACEPAD_DATA_DIR", config.DefaultDataDirFor(p.Getenv)), or("listen", "TRACEPAD_LISTEN", config.DefaultListen), nil
}

var errNotServer = errors.New("not a server")

// lockedBy is the PID the data directory's lock records: the server writes
// its own after taking the lock (spec 001 #20), so a live process whose PID
// is there holds the database. The protocol is the store's.
func lockedBy(dataDir string) (int, error) {
	return store.RecordedPID(filepath.Join(dataDir, dataDBName))
}

// lockHeld says whether a server holds the data directory's database now.
func lockHeld(dataDir string) (bool, error) {
	return locked(filepath.Join(dataDir, dataDBName))
}

// loopbackURL is the address to ask a server listening on listen, when that
// address is this machine's alone; ok is false for one open beyond it.
func loopbackURL(listen string) (string, bool) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || config.PlainHTTPBeyondLoopback(listen) {
		return "", false
	}
	return loopbackBase(host, port)
}

// healthURL is where a server listening on listen is asked its version:
// its loopback address, and for one listening on every address of the
// machine (0.0.0.0, [::], or no host) the loopback address of the same
// family, which reaches it too (the thirteenth review). /health carries no
// key, so a server open beyond this machine is asked as one that is not;
// the trace count, which does, is read only at loopbackURL.
func healthURL(listen string) (string, bool) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", false
	}
	return loopbackBase(onLoopback(host), port)
}

// onLoopback is the loopback address of the same family as a wildcard host,
// or host itself.
func onLoopback(host string) string {
	switch host {
	case "", "0.0.0.0":
		return "127.0.0.1"
	case "::":
		return "::1"
	}
	return host
}

// loopbackBase is the address to ask at host:port, by its IP. `localhost`
// is asked at 127.0.0.1, the address a Go server listening on `localhost`
// binds: a client that resolved the name could reach [::1] instead, where
// any other user of the machine may listen, and the key the count is read
// with would go there.
func loopbackBase(host, port string) (string, bool) {
	if strings.EqualFold(host, "localhost") {
		host = "127.0.0.1"
	}
	ip := net.ParseIP(host)
	n, err := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || err != nil || n <= 0 || n > 65535 || strconv.Itoa(n) != port {
		return "", false
	}
	return "http://" + net.JoinHostPort(ip.String(), port), true
}

// sameFile says whether two paths name one file, comparing the paths with
// their directories' links resolved: a replaced binary is no longer the same
// inode, but it ran from the same path.
func sameFile(a, b string) bool {
	return a != "" && b != "" && canonicalPath(a) == canonicalPath(b)
}

func canonicalPath(p string) string {
	p = filepath.Clean(p)
	if dir, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
		p = filepath.Join(dir, filepath.Base(p))
	}
	return p
}

// classifyServer decides whether a server process is the command's
// (Decision 4), and if not, says why.
func classifyServer(p Process, install string) (Server, bool) {
	s := Server{Proc: p}
	dataDir, listen, err := resolveServer(p)
	if errors.Is(err, errNotServer) {
		return s, false
	}
	s.DataDir, s.Listen = dataDir, listen
	s.URL, _ = loopbackURL(listen)
	switch {
	case err != nil:
		s.Reason = err.Error()
	case p.Exe == "":
		s.Reason = "its executable could not be read"
	case !sameFile(p.Exe, install):
		s.Reason = fmt.Sprintf("it runs %s, not the installed %s", p.Exe, install)
	case !recordsPID(dataDir, p.PID):
		s.Reason = fmt.Sprintf("the lock of its data directory %s does not record it", dataDir)
	case strings.HasPrefix(p.Manager, "its "):
		s.Reason = p.Manager
	case p.Manager != "":
		s.Reason = "it runs in " + p.Manager + ", which may restart what the command stops"
	case s.URL == "":
		s.Reason = fmt.Sprintf("it listens on %s, beyond this machine", listen)
	case isMountPoint(dataDir):
		s.Reason = fmt.Sprintf("its data directory %s is a mount point, which a way back cannot set aside", dataDir)
	default:
		s.Ours = true
	}
	return s, true
}

func recordsPID(dataDir string, pid int) bool {
	recorded, err := lockedBy(dataDir)
	return err == nil && recorded == pid
}

// health asks a server's /health for its version, for timeout at most and
// with no proxy: the server is on this machine.
func health(ctx context.Context, client *http.Client, base string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body); err != nil {
		return "", fmt.Errorf("its answer is not /health's: %w", err)
	}
	if body.Version == "" {
		return "", errors.New("its answer names no version")
	}
	return body.Version, nil
}

// discover is the plan's look at the machine (Decision 5).
// discoverWait bounds the plan's look at the machine as a whole: the install
// script stops waiting for a plan after fifteen seconds, so a few servers
// that take a connection and never answer must not run it past that (the
// fourth review). The probes run side by side under it.
const discoverWait = 10 * time.Second

func (r *runner) discover(ctx context.Context) Findings {
	wait := r.deps.DiscoverWait
	if wait <= 0 {
		wait = discoverWait
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	f := Findings{complete: true, processes: true}
	var (
		wg         sync.WaitGroup
		containers []Container
		note       string
		probe      Probe
	)
	wg.Add(3)
	go func() { defer wg.Done(); f.Binary = r.installedBinary(ctx) }()
	go func() { defer wg.Done(); containers, note = r.containers(ctx) }()
	go func() { defer wg.Done(); probe = r.ask(ctx) }()
	procs, unread, err := r.deps.Sys.Candidates(ctx)
	switch {
	case err != nil:
		f.complete, f.processes = false, false
		f.Notes = append(f.Notes, "the processes could not be listed ("+err.Error()+"), so no server was found that way")
	case unread > 0:
		f.complete, f.processes = false, false
		f.Notes = append(f.Notes, fmt.Sprintf("%d process(es) named tracepad could not be read", unread))
	}
	wg.Wait()
	for _, p := range procs {
		if s, ok := classifyServer(p, f.Binary.Path); ok {
			f.Servers = append(f.Servers, s)
		}
	}
	var hw sync.WaitGroup
	for i := range f.Servers {
		probe, ok := healthURL(f.Servers[i].Listen)
		if !ok {
			// Only an address read is one not asked: a server whose
			// configuration could not be read keeps that reason, and is
			// named as one that may be behind (the review of #225).
			if f.Servers[i].Listen != "" {
				f.Servers[i].Unchecked = fmt.Sprintf("it listens on %s only, an address the command does not ask", f.Servers[i].Listen)
			}
			continue
		}
		hw.Add(1)
		go func(s *Server) {
			defer hw.Done()
			s.Version, _ = health(ctx, r.deps.HTTP, probe) // ignored: no answer is no version, which is never upgraded
		}(&f.Servers[i])
	}
	hw.Wait()
	for i := range f.Servers {
		s := &f.Servers[i]
		if s.Ours && s.Version == "" {
			s.Ours = false
			s.Reason = "it does not answer at " + s.URL + "/health"
		}
		if s.Ours && !IsRelease(s.Version) {
			s.Ours = false
			s.Reason = fmt.Sprintf("it answers %q, a development build, which has no place in the order between releases", s.Version)
		}
	}
	sort.Slice(f.Servers, func(i, j int) bool { return f.Servers[i].Proc.PID < f.Servers[j].Proc.PID })
	f.Containers = containers
	if note != "" {
		f.Notes = append(f.Notes, note)
		// Docker there and not answering: what answers on 4318 may be a
		// container's, and is not called no one's (the audit of #223).
		f.complete = f.complete && r.deps.Docker == nil
	}
	f.Probe = r.attribute(probe, f)
	return f
}

// installedBinary reads <install dir>/tracepad.
func (r *runner) installedBinary(ctx context.Context) Binary {
	b := Binary{Path: filepath.Join(r.deps.InstallDir, "tracepad")}
	// Lstat: a link there is someone else's install (a package manager's),
	// never replaced with a file (the final review).
	st, err := os.Lstat(b.Path)
	if err == nil {
		b.Exists = true
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		b.Kind, b.Reason = binNone, "no binary is installed at "+b.Path
	case err != nil:
		b.Kind, b.Reason = binOdd, err.Error()
	case packageManager(canonicalPath(b.Path)) != "":
		// A package manager's, a link into its tree or a file: replaced
		// under it, the manager's record and the binary part (the ninth
		// review), whatever version it says.
		b.Version, _ = r.deps.Version(ctx, b.Path) // ignored: a package manager's binary is the person's either way
		b.Kind, b.Reason = binPackaged, b.Path+" is "+packageManager(canonicalPath(b.Path))
	case st.Mode()&os.ModeSymlink != 0:
		target, _ := os.Readlink(b.Path) // ignored: the message's; a link is the person's either way
		if target != "" && !filepath.IsAbs(target) {
			// Named from the link's directory, which the reader is not in
			// (the fourth review of #228).
			target = filepath.Join(filepath.Dir(b.Path), target)
		}
		b.Link = target
		b.Version, _ = r.deps.Version(ctx, b.Path) // ignored: a link is the person's either way
		b.Kind, b.Reason = binLinked, b.Path+" is a symbolic link to "+target+": its owner's to replace"
	case !st.Mode().IsRegular():
		b.Kind, b.Reason = binOdd, b.Path+" is not a regular file"
	default:
		b.Version, err = r.deps.Version(ctx, b.Path)
		switch {
		case err != nil:
			b.Kind, b.Reason = binSilent, b.Path+" does not run here: "+err.Error()
		case !IsRelease(b.Version):
			b.Kind, b.Reason = binDev, fmt.Sprintf("%s says it is %q, a development build", b.Path, b.Version)
		case !writableDir(filepath.Dir(b.Path)):
			b.Kind, b.Reason = binUnwritable, filepath.Dir(b.Path)+" is not writable by this user"
		default:
			b.Kind, b.Ours = binOurs, true
		}
	}
	if first := r.deps.LookPath("tracepad"); first != "" && !sameFile(first, b.Path) {
		// Asked here, under the look at the machine's deadline (the tenth
		// review).
		b.First = first
		b.FirstVersion, b.FirstErr = r.deps.Version(ctx, first)
	}
	return b
}

func writableDir(dir string) bool {
	f, err := os.CreateTemp(dir, ".tracepad-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close() // ignored: an empty probe, removed below
	return os.Remove(name) == nil
}

// ask asks the default address what answers there.
func (r *runner) ask(ctx context.Context) Probe {
	base, _ := loopbackURL(config.DefaultListen)
	pctx, cancel := context.WithTimeout(ctx, r.deps.ProbeWait)
	defer cancel()
	v, err := health(pctx, r.deps.HTTP, base)
	if err != nil {
		return Probe{}
	}
	return Probe{URL: base, Version: v}
}

// attribute says whose what answers at the default address is, only as far
// as the search could tell.
func (r *runner) attribute(p Probe, f Findings) Probe {
	if p.Version == "" {
		return p
	}
	for _, s := range f.Servers {
		if s.URL != "" && listensOnDefault(s.Listen) {
			p.Whose = fmt.Sprintf("server pid %d", s.Proc.PID)
			return p
		}
	}
	for _, c := range f.Containers {
		if c.Default {
			p.Whose = "container " + c.Name
			return p
		}
	}
	if f.complete {
		p.Whose = "not this install's: a container, or another install"
	} else {
		p.Whose = "could not tell whose"
	}
	return p
}

func listensOnDefault(listen string) bool {
	_, port, err := net.SplitHostPort(listen)
	return err == nil && port == "4318"
}

// packageManager says whose a binary at path is when a package manager
// installed it — Homebrew's Cellar, the Nix store, a snap, the system's own
// directories that dpkg and rpm fill — with what upgrades it; empty when it
// is none's.
func packageManager(path string) string {
	switch {
	case strings.Contains(path, "/Cellar/") || strings.HasPrefix(path, "/opt/homebrew/") || strings.HasPrefix(path, "/home/linuxbrew/"):
		return "Homebrew's: brew upgrade tracepad"
	case strings.HasPrefix(path, "/nix/store/"):
		return "in the Nix store: your Nix configuration upgrades it"
	case strings.HasPrefix(path, "/snap/"):
		return "a snap's: snap refresh tracepad"
	}
	for _, dir := range []string{"/usr/bin/", "/usr/sbin/", "/bin/", "/sbin/", "/usr/lib/", "/usr/libexec/", "/usr/share/", "/opt/local/"} {
		if strings.HasPrefix(path, dir) {
			return "the system's package manager's (dpkg, rpm, MacPorts): upgrade it with that manager"
		}
	}
	return ""
}
