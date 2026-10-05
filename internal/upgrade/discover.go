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

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/store"
)

// Binary is the installed binary: what the install script and this command
// put at <install dir>/tracepad.
type Binary struct {
	Path    string
	Exists  bool
	Version string
	// Ours is whether the command may replace it; Reason says why not.
	Ours   bool
	Reason string
	// First is the `tracepad` first on PATH when that is another one.
	First string
}

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
	// complete is whether every candidate process could be read.
	complete bool
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
// address the way the server itself does: the flag, else its environment,
// else the default, in its environment and relative to its working directory.
func resolveServer(p Process) (dataDir, listen string, err error) {
	args, ok := serverFlags(p.Argv)
	if !ok {
		return "", "", errNotServer
	}
	flags, err := config.ParseFlags(args)
	if err != nil {
		return "", "", fmt.Errorf("its arguments are not a server's: %w", err)
	}
	dataDir, given := flags.Given("data-dir")
	if !given || dataDir == "" {
		dataDir = p.Getenv("TRACEPAD_DATA_DIR")
	}
	if dataDir == "" {
		dataDir = config.DefaultDataDirFor(p.Getenv)
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
	listen, given = flags.Given("listen")
	if !given || listen == "" {
		listen = p.Getenv("TRACEPAD_LISTEN")
	}
	if listen == "" {
		listen = config.DefaultListen
	}
	return filepath.Clean(dataDir), listen, nil
}

var errNotServer = errors.New("not a server")

// lockedBy is the PID the data directory's lock file records: the server
// writes its own after taking the lock (spec 001 #20), so a live process whose
// PID is there holds the database.
func lockedBy(dataDir string) (int, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, "tracepad.db"+store.LockSuffix))
	if err != nil {
		return 0, err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strconv.Atoi(strings.TrimSpace(line))
}

// loopbackURL is the address to ask a server listening on listen, when that
// address is this machine's alone; ok is false for one open beyond it.
func loopbackURL(listen string) (string, bool) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || config.PlainHTTPBeyondLoopback(listen) {
		return "", false
	}
	return "http://" + net.JoinHostPort(host, port), true
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
	case p.Manager != "":
		s.Reason = "it runs as " + p.Manager + ", which restarts it"
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
func (r *runner) discover(ctx context.Context) Findings {
	f := Findings{Binary: r.installedBinary(ctx), complete: true}
	procs, unread, err := r.deps.Sys.Candidates()
	switch {
	case err != nil:
		f.complete = false
		f.Notes = append(f.Notes, "the processes could not be listed ("+err.Error()+"), so no server was found that way")
	case unread > 0:
		f.complete = false
		f.Notes = append(f.Notes, fmt.Sprintf("%d process(es) named tracepad could not be read", unread))
	}
	for _, p := range procs {
		s, ok := classifyServer(p, f.Binary.Path)
		if !ok {
			continue
		}
		if s.URL != "" {
			s.Version, _ = health(ctx, r.deps.HTTP, s.URL)
		}
		if s.Ours && s.Version == "" {
			s.Ours = false
			s.Reason = "it does not answer at " + s.URL + "/health"
		}
		if s.Ours && !IsRelease(s.Version) {
			s.Ours = false
			s.Reason = fmt.Sprintf("it answers %q, a development build, which has no place in the order between releases", s.Version)
		}
		f.Servers = append(f.Servers, s)
	}
	sort.Slice(f.Servers, func(i, j int) bool { return f.Servers[i].Proc.PID < f.Servers[j].Proc.PID })

	containers, note := r.containers(ctx)
	f.Containers = containers
	if note != "" {
		f.Notes = append(f.Notes, note)
	}
	f.Probe = r.probe(ctx, f)
	return f
}

// installedBinary reads <install dir>/tracepad.
func (r *runner) installedBinary(ctx context.Context) Binary {
	b := Binary{Path: filepath.Join(r.deps.InstallDir, "tracepad")}
	st, err := os.Stat(b.Path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		b.Reason = "no binary is installed at " + b.Path
	case err != nil:
		b.Reason = err.Error()
	case !st.Mode().IsRegular():
		b.Exists = true
		b.Reason = b.Path + " is not a regular file"
	default:
		b.Exists = true
		b.Version, err = r.deps.Version(ctx, b.Path)
		switch {
		case err != nil:
			b.Reason = b.Path + " does not run here: " + err.Error()
		case !IsRelease(b.Version):
			b.Reason = fmt.Sprintf("%s says it is %q, a development build", b.Path, b.Version)
		case !writableDir(filepath.Dir(b.Path)):
			b.Reason = filepath.Dir(b.Path) + " is not writable by this user"
		default:
			b.Ours = true
		}
	}
	if first := r.deps.LookPath("tracepad"); first != "" && !sameFile(first, b.Path) {
		b.First = first
	}
	return b
}

func writableDir(dir string) bool {
	f, err := os.CreateTemp(dir, ".tracepad-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	return os.Remove(name) == nil
}

// probe asks the default address what answers there, and says whose it is
// only as far as the search could tell.
func (r *runner) probe(ctx context.Context, f Findings) Probe {
	p := Probe{URL: "http://" + config.DefaultListen}
	pctx, cancel := context.WithTimeout(ctx, r.deps.ProbeWait)
	defer cancel()
	v, err := health(pctx, r.deps.HTTP, p.URL)
	if err != nil {
		return Probe{}
	}
	p.Version = v
	for _, s := range f.Servers {
		if s.URL != "" && listensOnDefault(s.Listen) {
			p.Whose = fmt.Sprintf("server pid %d", s.Proc.PID)
			return p
		}
	}
	for _, c := range f.Containers {
		if c.publishesDefault() {
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
