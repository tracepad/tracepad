package skills

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tracepad/tracepad/internal/cli"
)

// Exit codes are the CLI's own (spec 037, command contract): a script has to
// tell "that directory is somebody else's" from "you typed the flag wrong", and
// both halves of the binary have to mean the same thing by each number.
const (
	exitOK      = cli.ExitOK
	exitFailure = cli.ExitFailure
	exitUsage   = cli.ExitUsage
)

// marker is the file that says a directory is an installed copy of this skill
// and which version it is (#6, #7). Without it, a directory called `tracepad`
// is somebody else's and is not overwritten.
const marker = ".version"

// Usage is the command's help text.
const Usage = `Usage:
  tracepad skills install [--project | --dir DIR] [--force]
  tracepad skills show [FILE]

install writes the agent skill this binary carries to ~/.claude/skills/tracepad,
to ./.claude/skills/tracepad with --project, or to DIR/tracepad with --dir. It
replaces the directory whole, so an update leaves nothing of the old version
behind, and it refuses a tracepad directory it did not install unless --force.
Run it again after upgrading the binary.

show prints SKILL.md, or one of its references (show debugging.md), to stdout.

Neither talks to a server.
`

// Options is everything the command resolves from its process.
type Options struct {
	// Args are the arguments after `skills`.
	Args    []string
	Version string
	Stdout  io.Writer
	Stderr  io.Writer
	// Env resolves environment variables; os.Getenv in production. HOME is
	// the one read, and USERPROFILE on Windows.
	Env func(string) string
	// Getwd is the directory --project resolves against; os.Getwd in
	// production.
	Getwd func() (string, error)
	// GOOS decides where the home directory is read from; runtime.GOOS
	// when empty.
	GOOS string
}

// usageError is a mistake in how the command was typed: exit 2, with the usage
// text after the message.
type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }

func usageErrorf(format string, args ...any) error {
	return &usageError{message: fmt.Sprintf(format, args...)}
}

// errHelp is a request for the usage text, which is not a mistake.
var errHelp = errors.New("help requested")

// Run executes `tracepad skills …` and returns the process exit code.
func Run(opt Options) int {
	if opt.Env == nil {
		opt.Env = func(string) string { return "" }
	}
	if opt.Getwd == nil {
		opt.Getwd = os.Getwd
	}
	if opt.GOOS == "" {
		opt.GOOS = runtime.GOOS
	}
	err := run(opt)
	var usage *usageError
	switch {
	case err == nil:
		return exitOK
	case errors.Is(err, errHelp):
		fmt.Fprint(opt.Stdout, Usage)
		return exitOK
	case errors.As(err, &usage):
		fmt.Fprintf(opt.Stderr, "tracepad skills: %s\n\n%s", err, Usage)
		return exitUsage
	default:
		fmt.Fprintf(opt.Stderr, "tracepad skills: %s\n", err)
		return exitFailure
	}
}

func run(opt Options) error {
	if len(opt.Args) == 0 {
		return usageErrorf("skills takes install or show")
	}
	sub, args := opt.Args[0], opt.Args[1:]
	switch sub {
	case "install":
		return runInstall(opt, args)
	case "show":
		return runShow(opt, args)
	case "help", "-h", "-help", "--help":
		return errHelp
	}
	return usageErrorf("skills takes install or show, got %q", sub)
}

// installFlags is `install`'s flag set. A function rather than inline so the
// drift test asks the command itself which flags exist (#8).
func installFlags() (*flag.FlagSet, *bool, *string, *bool) {
	set := flag.NewFlagSet("skills install", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	project := set.Bool("project", false, "")
	dir := set.String("dir", "", "")
	force := set.Bool("force", false, "")
	return set, project, dir, force
}

// showFlags is `show`'s: none of its own, but `--help` has to parse.
func showFlags() *flag.FlagSet {
	set := flag.NewFlagSet("skills show", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	return set
}

// FlagSets are the flag sets of the two subcommands, for the drift test: it
// checks a `--flag` in the skill against what the command defines, never
// against a list written out a second time.
func FlagSets() map[string]*flag.FlagSet {
	install, _, _, _ := installFlags()
	return map[string]*flag.FlagSet{"install": install, "show": showFlags()}
}

func parse(set *flag.FlagSet, args []string) error {
	if err := set.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return errHelp
		}
		return usageErrorf("%s: %s", set.Name(), err)
	}
	return nil
}

func runInstall(opt Options, args []string) error {
	set, project, dir, force := installFlags()
	if err := parse(set, args); err != nil {
		return err
	}
	if set.NArg() > 0 {
		return usageErrorf("install takes no arguments, got %q", set.Arg(0))
	}
	given := map[string]bool{}
	set.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if *project && given["dir"] {
		return usageErrorf("--project and --dir name two different places; pass one")
	}
	if given["dir"] && *dir == "" {
		// What an unset shell variable expands to. Reading it as "the
		// default" would write to a place the caller did not name.
		return usageErrorf("--dir needs a directory; it was passed empty")
	}

	root, base, err := installBase(opt, *project, *dir)
	if err != nil {
		return err
	}
	target := filepath.Join(base, Name)
	done, err := install(Files(), root, target, opt.Version, *force, *project)
	if err != nil {
		return err
	}
	switch {
	case done.empty:
		// Nothing was there to replace: the directory a link set up for
		// the skill, waiting for it (#16).
		fmt.Fprintf(opt.Stdout, "installed %s to %s\n", opt.Version, done.target)
	case done.replaced && !done.marked:
		fmt.Fprintf(opt.Stdout, "installed %s to %s, replacing a directory that was not an installed skill\n",
			opt.Version, done.target)
	case done.replaced && done.previous == opt.Version:
		fmt.Fprintf(opt.Stdout, "reinstalled %s in %s\n", opt.Version, done.target)
	case done.replaced && done.previous == "":
		fmt.Fprintf(opt.Stdout, "updated an unversioned install → %s in %s\n", opt.Version, done.target)
	case done.replaced:
		fmt.Fprintf(opt.Stdout, "updated %s → %s in %s\n", done.previous, opt.Version, done.target)
	default:
		fmt.Fprintf(opt.Stdout, "installed %s to %s\n", opt.Version, done.target)
	}
	for _, path := range done.kept {
		fmt.Fprintf(opt.Stderr, "tracepad skills: an interrupted install left %s, which may hold "+
			"what it replaced; delete it once it is not needed\n", path)
	}
	if done.leftover != "" {
		fmt.Fprintf(opt.Stderr, "tracepad skills: the previous copy could not be removed; delete %s\n",
			done.leftover)
	}
	return nil
}

// installBase is the directory the skill's own directory goes into: the
// user's Claude Code skills by default, the project's with --project, or
// anywhere with --dir (#6). root is where the path the command chose begins —
// the home, the working tree, --dir itself — below which it follows no link
// (#16).
func installBase(opt Options, project bool, dir string) (root, base string, err error) {
	if dir != "" && filepath.IsAbs(dir) {
		return filepath.Clean(dir), filepath.Clean(dir), nil
	}
	if dir != "" || project {
		// Both resolve against the one working directory the command was
		// given, so a relative --dir and --project cannot disagree.
		wd, err := opt.Getwd()
		if err != nil {
			return "", "", fmt.Errorf("cannot tell the current directory: %w", err)
		}
		if dir != "" {
			return filepath.Join(wd, dir), filepath.Join(wd, dir), nil
		}
		return wd, filepath.Join(wd, ".claude", "skills"), nil
	}
	// os.UserHomeDir's rule, over the injected environment: USERPROFILE on
	// Windows — where Claude Code looks, and where a HOME set by Git Bash or
	// a roaming profile is not — and HOME everywhere else.
	variable := "HOME"
	if opt.GOOS == "windows" {
		variable = "USERPROFILE"
	}
	home := opt.Env(variable)
	if home == "" {
		// A container runs without one, and the fallback would be a write
		// to /.claude that nobody meant (edge cases).
		return "", "", fmt.Errorf("%s is not set, so there is no default place to install to; "+
			"pass --dir DIR (or --project for ./.claude/skills)", variable)
	}
	return home, filepath.Join(home, ".claude", "skills"), nil
}

// outcome is what an install found and did, for the lines it prints.
type outcome struct {
	// target is where the skill went: the directory a symlink at the
	// requested path points to, when it is one.
	target string
	// replaced says there was something to replace; marked, that it was a
	// directory carrying the marker, whose version is previous ("" when the
	// marker was empty).
	replaced, marked bool
	previous         string
	// empty says the target was an empty directory reached through a
	// link, which --force installs into (#16): nothing was replaced.
	empty bool
	// leftover is the previous copy when it could not be removed after the
	// new one was in place: the install succeeded, and the caller says where
	// the debris is.
	leftover string
	// kept are previous copies an interrupted install set aside and never
	// got to remove. One may be the only copy of what it replaced — a
	// directory `--force` took over — so they are named, never deleted.
	kept []string
}

// holding is the directory, beside the skill, where an install writes its new
// copy and sets the old one aside before swapping them. Nested one level down
// on purpose: a skills directory is read one level deep, so nothing in here —
// a copy a killed install left, a previous version that could not be removed
// — is ever taken for a second `tracepad` skill.
const holding = ".tracepad-install"

// abandonedAfter is how old a staging copy has to be before an install takes
// it for one a killed install left, rather than one another install is
// writing right now.
const abandonedAfter = time.Hour

// install writes files to target, whole.
//
// The new copy is written in the holding directory first and swapped in by
// rename, so a failure half-way leaves the old skill where it was rather than
// a mix of two versions (#6). A target that is a symlink — a copy kept in a
// dotfiles repository — is followed, so the copy the user maintains is the
// one updated, not replaced by a directory of its own (#14). Since #16 that
// is the only link an install follows: the last step of a path the user chose
// (never one inside a project, which is somebody's checkout), and only into
// this command's own skill, or with --force into an empty directory. A link
// is a path to anywhere, and replacing whatever it names would delete a
// directory the user never pointed the command at.
func install(files fs.FS, root, target, version string, force, project bool) (outcome, error) {
	done := outcome{target: target}
	if err := noLinkBelow(root, filepath.Dir(target)); err != nil {
		return done, err
	}
	linked := false
	info, err := os.Lstat(target)
	switch {
	case err == nil && info.Mode()&fs.ModeSymlink != 0:
		if project {
			return done, fmt.Errorf("%s is a symlink, and an install into a project follows none: "+
				"a checkout's links are its authors', not yours; remove it and install again", target)
		}
		resolved, err := filepath.EvalSymlinks(target)
		if err != nil {
			return done, fmt.Errorf("%s is a symlink to nothing that can be installed into: %w", target, err)
		}
		if info, err = os.Stat(resolved); err != nil {
			return done, err
		}
		done.target, done.replaced, linked = resolved, true, true
	case err == nil:
		done.replaced = true
	case !errors.Is(err, fs.ErrNotExist):
		return done, err
	}
	if done.replaced && info.IsDir() {
		if done.marked, done.previous, err = ours(done.target); err != nil {
			return done, err
		}
	}
	if done.replaced && !done.marked && linked {
		// An empty directory behind a link is the one exception, with
		// --force: it is how a copy kept in a dotfiles repository is set
		// up in the first place, and replacing it loses nothing.
		if !force || !emptyDir(done.target) {
			return done, fmt.Errorf("%s is a symlink to %s, which is not a skill this command installed "+
				"(%s); an install follows a link only into its own skill, or with --force "+
				"into an empty directory: remove the link and install again", target, done.target, ownMark)
		}
		done.empty = true
	}
	if done.replaced && !done.marked && !force {
		return done, fmt.Errorf("%s exists and is not a skill this command installed "+
			"(%s); pass --force to replace it", done.target, ownMark)
	}

	work := filepath.Join(filepath.Dir(done.target), holding)
	if err := holdingDir(work); err != nil {
		return done, err
	}
	// Deferred first so it runs last: the holding directory goes when an
	// install leaves nothing in it, which is every install but an
	// interrupted one.
	defer os.Remove(work)
	done.kept = sweep(work)
	staging, err := stagingDir(work)
	if err != nil {
		return done, err
	}
	defer os.RemoveAll(staging)
	if err := write(files, staging, version); err != nil {
		return done, err
	}

	if !done.replaced {
		return done, os.Rename(staging, done.target)
	}
	old := staging + ".old"
	if err := os.Rename(done.target, old); err != nil {
		return done, err
	}
	if err := os.Rename(staging, done.target); err != nil {
		// Put the old one back rather than leave nothing, and say so when
		// even that fails: the previous skill is then in the holding
		// directory, which no later install deletes.
		if back := os.Rename(old, done.target); back != nil {
			return done, fmt.Errorf("%w; putting the previous skill back failed too (%v), and it is at %s",
				err, back, old)
		}
		return done, err
	}
	if err := os.RemoveAll(old); err != nil {
		done.leftover = old
	}
	return done, nil
}

// ownMark says what makes a directory this command's own skill, for the
// refusals that name its absence.
const ownMark = "no SKILL.md named " + Name + " beside a " + marker

// ours reports whether dir is a skill this command installed, and the version
// its marker names ("" when the marker is empty). It takes both halves (#16):
// SKILL.md naming this skill, and the marker. The marker alone is one file
// with a generic name, which any repository can commit — and a link followed
// on its word alone replaced whatever held one.
func ours(dir string) (bool, string, error) {
	stamp, err := os.ReadFile(filepath.Join(dir, marker))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, "", nil
	case err != nil:
		// A marker that cannot be read is not the same as no marker,
		// and calling it "somebody else's directory" would hide why.
		return false, "", fmt.Errorf("cannot read %s: %w", filepath.Join(dir, marker), err)
	}
	skill, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil || !namesThisSkill(skill) {
		return false, "", nil
	}
	return true, strings.TrimSpace(string(stamp)), nil
}

// namesThisSkill reports whether a SKILL.md's frontmatter says `name:
// tracepad`, the line every install writes.
func namesThisSkill(skill []byte) bool {
	lines := strings.Split(string(skill), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return false
	}
	for _, line := range lines[1:] {
		line = strings.TrimRight(line, "\r")
		if line == "---" {
			return false
		}
		if value, found := strings.CutPrefix(line, "name:"); found {
			return strings.Trim(strings.TrimSpace(value), `"'`) == Name
		}
	}
	return false
}

// noLinkBelow refuses a path that goes through a link between root and dir:
// `.claude` or `.claude/skills` as a symlink would carry the install — and
// the deletion of what it replaces — anywhere (#16). What does not exist yet
// is made by the install, as a directory.
func noLinkBelow(root, dir string) error {
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil
	}
	path := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink, and an install follows none on the way to the skill "+
				"directory; remove it, or install with --dir into the directory it points at", path)
		}
	}
	return nil
}

// emptyDir reports whether path is a directory with nothing in it.
func emptyDir(path string) bool {
	entries, err := os.ReadDir(path)
	return err == nil && len(entries) == 0
}

// holdingDir makes the holding directory, or checks that the one already
// there is a directory rather than a link to one: an install deletes what it
// finds in it, and through a link that would be the contents of wherever the
// link points (#16).
func holdingDir(work string) error {
	if err := os.MkdirAll(filepath.Dir(work), 0o755); err != nil {
		return err
	}
	err := os.Mkdir(work, 0o755)
	if err == nil || !errors.Is(err, fs.ErrExist) {
		return err
	}
	info, err := os.Lstat(work)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		// Lstat does not follow: a link to a directory lands here too.
		return fmt.Errorf("%s is where an install stages its copy, and it is not a directory "+
			"(a symlink, or a file); remove it and install again", work)
	}
	return nil
}

// sweep removes the staging copies an install killed half-way left behind —
// copies of a binary's own skill, which any install writes again — and returns
// the previous copies set aside by one, which it does not touch.
func sweep(work string) (kept []string) {
	entries, err := os.ReadDir(work)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		path := filepath.Join(work, entry.Name())
		if strings.HasSuffix(entry.Name(), ".old") {
			kept = append(kept, path)
			continue
		}
		if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > abandonedAfter {
			os.RemoveAll(path)
		}
	}
	return kept
}

// stagingDir makes a fresh directory in the holding directory. os.Mkdir
// rather than os.MkdirTemp, whose 0700 would have to be widened by a chmod
// that ignores the umask the files inside it are written under.
func stagingDir(work string) (string, error) {
	for range 10 {
		name := filepath.Join(work, strconv.FormatInt(time.Now().UnixNano(), 36))
		err := os.Mkdir(name, 0o755)
		if !errors.Is(err, fs.ErrExist) {
			return name, err
		}
	}
	return "", fmt.Errorf("cannot make a staging directory in %s", work)
}

// write copies every file of the skill into dir, stamping SKILL.md with the
// version and writing the marker.
func write(files fs.FS, dir, version string) error {
	err := fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		destination := filepath.Join(dir, filepath.FromSlash(name))
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		content, err := fs.ReadFile(files, name)
		if err != nil {
			return err
		}
		if name == "SKILL.md" {
			if content, err = Stamp(content, version); err != nil {
				return err
			}
		}
		return os.WriteFile(destination, content, 0o644)
	})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, marker), []byte(version+"\n"), 0o644)
}

// Stamp sets `metadata.version` in SKILL.md's frontmatter (#7). The source
// carries `version: dev`, which is also what a development build stamps; a
// frontmatter without the line is an error rather than a skill installed
// without the check the agent is told to make.
func Stamp(skill []byte, version string) ([]byte, error) {
	lines := bytes.SplitAfter(skill, []byte("\n"))
	if len(lines) == 0 || strings.TrimSpace(string(lines[0])) != "---" {
		return nil, errors.New("SKILL.md does not start with a frontmatter block")
	}
	inMetadata, stamped := false, false
	for i := 1; i < len(lines); i++ {
		line := strings.TrimRight(string(lines[i]), "\r\n")
		switch {
		case line == "---":
			if !stamped {
				return nil, errors.New("SKILL.md's frontmatter has no metadata.version to stamp")
			}
			return bytes.Join(lines, nil), nil
		case line == "metadata:":
			inMetadata = true
		case inMetadata && !stamped && strings.HasPrefix(line, "  version:"):
			lines[i] = []byte(fmt.Sprintf("  version: %q\n", version))
			stamped = true
		case strings.TrimSpace(line) == "", strings.HasPrefix(line, "#"):
			// A blank line or a comment does not end a YAML mapping.
		case !strings.HasPrefix(line, " "):
			inMetadata = false
		}
	}
	return nil, errors.New("SKILL.md's frontmatter is not closed")
}

func runShow(opt Options, args []string) error {
	set := showFlags()
	if err := parse(set, args); err != nil {
		return err
	}
	if set.NArg() > 1 {
		return usageErrorf("show takes one file, got %d", set.NArg())
	}
	files := Files()
	name := "SKILL.md"
	if set.NArg() == 1 {
		found, ok := lookup(files, set.Arg(0))
		if !ok {
			return usageErrorf("the skill has no file %q; it has %s",
				set.Arg(0), strings.Join(names(files), ", "))
		}
		name = found
	}
	content, err := fs.ReadFile(files, name)
	if err != nil {
		return err
	}
	if name == "SKILL.md" {
		if content, err = Stamp(content, opt.Version); err != nil {
			return err
		}
	}
	_, err = opt.Stdout.Write(content)
	return err
}

// lookup resolves what `show` was given: a file's path in the skill, or a
// reference by its bare name, with or without `.md`.
func lookup(files fs.FS, arg string) (string, bool) {
	all := names(files)
	for _, candidate := range []string{arg, path.Join("references", arg), path.Join("references", arg+".md")} {
		if slices.Contains(all, candidate) {
			return candidate, true
		}
	}
	return "", false
}

// names is every file of the skill, SKILL.md first.
func names(files fs.FS) []string {
	var out []string
	fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			out = append(out, name)
		}
		return nil
	})
	slices.SortFunc(out, func(a, b string) int {
		switch {
		case a == "SKILL.md":
			return -1
		case b == "SKILL.md":
			return 1
		}
		return strings.Compare(a, b)
	})
	return out
}
