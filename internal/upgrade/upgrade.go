// Package upgrade is `tracepad upgrade` (spec 054): it plans an upgrade of
// the installed binary and of one server or container this user started,
// backs up before it stops anything, swaps, checks, and goes back when the new
// version does not answer, deleting nothing on the way.
package upgrade

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Options is how cmd/tracepad calls the command.
type Options struct {
	// Args are the words after `upgrade`.
	Args []string
	// Version is this binary's.
	Version        string
	Stdout, Stderr io.Writer
	Getenv         func(string) string
}

// Deps is everything the command asks of the machine. Run builds the real
// ones; tests give fakes.
type Deps struct {
	Sys    System
	Docker Docker
	// HTTP asks servers on this machine: no proxy.
	HTTP     *http.Client
	Releases *Releases
	// InstallDir is TRACEPAD_INSTALL_DIR, or ~/.local/bin.
	InstallDir string
	// Backups is ~/tracepad-backups.
	Backups string
	Home    string
	Cwd     string
	// Self is this binary, kept in each run directory for its way back.
	Self     string
	Getenv   func(string) string
	LookPath func(string) string
	Version  func(ctx context.Context, path string) (string, error)
	// Skills runs `bin skills install args…` in dir.
	Skills func(ctx context.Context, bin, dir string, args ...string) (string, error)
	Now    func() time.Time
	Sleep  func(ctx context.Context, d time.Duration) error
	// StopWait is how long a stopped server may take to exit; HealthWait how
	// long a started one may take to answer; ProbeWait the default address's.
	StopWait, HealthWait, ProbeWait time.Duration
}

type flags struct {
	plan      bool
	to        string
	check     string
	back      string
	dataDir   string
	container string
	json      bool
}

type runner struct {
	deps    Deps
	flags   flags
	version string
}

const usage = `Usage:
  tracepad upgrade --plan [--to X] [--data-dir DIR | --container NAME] [--json]
  tracepad upgrade [--to X] [--data-dir DIR | --container NAME] [--json]
  tracepad upgrade --check RUN [--json]
  tracepad upgrade --back RUN [--json]

Upgrades the installed binary and one server or container this user started,
with a backup first and a way back. --plan changes nothing. The latest stable
release unless --to names one; never an older one.

Exit status: 0 done or nothing to do, 1 refused (nothing changed), 2 usage,
3 not healthy and the way back ran, 4 yours to decide, 5 stuck (see the
report); --plan: 10 when the upgrade would change something.
`

// Run is `tracepad upgrade`.
func Run(ctx context.Context, opt Options) int {
	deps, err := realDeps(opt.Getenv)
	if err != nil {
		fmt.Fprintf(opt.Stderr, "tracepad upgrade: %v\n", err)
		return exitRefused
	}
	return run(ctx, opt, deps)
}

func run(ctx context.Context, opt Options, deps Deps) int {
	f, err := parseFlags(opt.Args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(opt.Stdout, usage)
		return exitOK
	}
	if err != nil {
		fmt.Fprintf(opt.Stderr, "tracepad upgrade: %v\n\n%s", err, usage)
		return exitUsage
	}
	r := &runner{deps: deps, flags: f, version: opt.Version}
	var rep *Report
	switch {
	case f.plan:
		rep = r.planMode(ctx)
	case f.check != "":
		rep = r.checkMode(ctx)
	case f.back != "":
		rep = r.backMode(ctx)
	default:
		rep = r.upgrade(ctx)
	}
	rep.write(opt.Stdout, f.json)
	return rep.ExitCode
}

func newFlagSet(f *flags) *flag.FlagSet {
	fs := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&f.plan, "plan", false, "")
	fs.StringVar(&f.to, "to", "", "")
	fs.StringVar(&f.check, "check", "", "")
	fs.StringVar(&f.back, "back", "", "")
	fs.StringVar(&f.dataDir, "data-dir", "", "")
	fs.StringVar(&f.container, "container", "", "")
	fs.BoolVar(&f.json, "json", false, "")
	return fs
}

// FlagSet is the command's flags, for the skill's drift test.
func FlagSet() *flag.FlagSet { return newFlagSet(&flags{}) }

func parseFlags(args []string) (flags, error) {
	var f flags
	fs := newFlagSet(&f)
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	if fs.NArg() > 0 {
		return f, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	modes := 0
	for _, on := range []bool{f.plan, f.check != "", f.back != ""} {
		if on {
			modes++
		}
	}
	switch {
	case modes > 1:
		return f, errors.New("--plan, --check and --back are three modes; give one")
	case (f.check != "" || f.back != "") && (f.to != "" || f.dataDir != "" || f.container != ""):
		return f, errors.New("--check and --back take a run, which already says what it upgraded")
	case f.dataDir != "" && f.container != "":
		return f, errors.New("--data-dir names a server and --container a container; one run upgrades one of them")
	}
	if f.to != "" {
		f.to = normalizeVersion(f.to)
	}
	return f, nil
}

func realDeps(getenv func(string) string) (Deps, error) {
	home := getenv("HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return Deps{}, fmt.Errorf("no home directory: %w", err)
		}
		home = h
	}
	installDir := strings.TrimRight(getenv("TRACEPAD_INSTALL_DIR"), "/")
	if installDir == "" {
		installDir = filepath.Join(home, ".local", "bin")
	}
	if abs, err := filepath.Abs(installDir); err == nil {
		installDir = abs
	}
	self, err := os.Executable()
	if err != nil {
		return Deps{}, err
	}
	cwd, _ := os.Getwd()
	base, mirror := getenv("TRACEPAD_DOWNLOAD_URL"), true
	if base == "" {
		base, mirror = githubReleases, false
	}
	goos, arch := runtime.GOOS, nativeArch()
	// A server that takes a connection and never answers must not hold the
	// plan: every local request has its own deadline.
	local := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext}}
	network := &http.Client{Timeout: 10 * time.Minute, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	return Deps{
		Sys:    newSystem(),
		Docker: newDockerCLI(),
		HTTP:   local,
		Releases: &Releases{Base: base, Mirror: mirror, API: "https://api.github.com/repos/" + repo,
			HTTP: network, OS: goos, Arch: arch, Attest: ghAttest, Version: binaryVersion},
		InstallDir: installDir,
		Backups:    filepath.Join(home, "tracepad-backups"),
		Home:       home,
		Cwd:        cwd,
		Self:       self,
		Getenv:     getenv,
		LookPath: func(name string) string {
			p, _ := exec.LookPath(name)
			return p
		},
		Version:    binaryVersion,
		Skills:     runSkills,
		Now:        time.Now,
		Sleep:      sleepCtx,
		StopWait:   60 * time.Second,
		HealthWait: 120 * time.Second,
		ProbeWait:  3 * time.Second,
	}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func runSkills(ctx context.Context, bin, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, append([]string{"skills", "install"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
