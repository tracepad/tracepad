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
	"slices"
	"strings"
)

// Exit codes, the CLI's own (spec 037, command contract): a script has to tell
// "that directory is somebody else's" from "you typed the flag wrong".
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2
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
	// the only one read.
	Env func(string) string
	// Getwd is the directory --project resolves against; os.Getwd in
	// production.
	Getwd func() (string, error)
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

	base, err := installBase(opt, *project, *dir)
	if err != nil {
		return err
	}
	target, err := filepath.Abs(filepath.Join(base, Name))
	if err != nil {
		return err
	}
	previous, replaced, err := install(Files(), target, opt.Version, *force)
	if err != nil {
		return err
	}
	switch {
	case replaced && previous == "":
		fmt.Fprintf(opt.Stdout, "installed %s to %s, replacing a directory that was not an installed skill\n",
			opt.Version, target)
	case replaced && previous == opt.Version:
		fmt.Fprintf(opt.Stdout, "reinstalled %s in %s\n", opt.Version, target)
	case replaced:
		fmt.Fprintf(opt.Stdout, "updated %s → %s in %s\n", previous, opt.Version, target)
	default:
		fmt.Fprintf(opt.Stdout, "installed %s to %s\n", opt.Version, target)
	}
	return nil
}

// installBase is the directory the skill's own directory goes into: the
// user's Claude Code skills by default, the project's with --project, or
// anywhere with --dir (#6).
func installBase(opt Options, project bool, dir string) (string, error) {
	switch {
	case dir != "":
		return dir, nil
	case project:
		wd, err := opt.Getwd()
		if err != nil {
			return "", fmt.Errorf("cannot tell the current directory for --project: %w", err)
		}
		return filepath.Join(wd, ".claude", "skills"), nil
	}
	home := opt.Env("HOME")
	if home == "" {
		// A container runs without one, and the fallback would be a write
		// to /.claude that nobody meant (edge cases).
		return "", errors.New("HOME is not set, so there is no default place to install to; " +
			"pass --dir DIR (or --project for ./.claude/skills)")
	}
	return filepath.Join(home, ".claude", "skills"), nil
}

// install writes files to target, whole. previous is the version the marker
// named, and replaced says whether there was a directory to replace at all.
//
// The new copy is written beside the target first and swapped in by rename,
// so a failure half-way leaves the old skill where it was rather than a mix
// of two versions (#6).
func install(files fs.FS, target, version string, force bool) (previous string, replaced bool, err error) {
	if _, err := os.Lstat(target); err == nil {
		replaced = true
		stamp, err := os.ReadFile(filepath.Join(target, marker))
		switch {
		case err == nil:
			previous = strings.TrimSpace(string(stamp))
		case !force:
			return "", false, fmt.Errorf("%s exists and is not a skill this command installed "+
				"(it has no %s); pass --force to replace it", target, marker)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", false, err
	}

	parent := filepath.Dir(target)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", false, err
	}
	staging, err := os.MkdirTemp(parent, ".tracepad-install-")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(staging)
	if err := os.Chmod(staging, 0o755); err != nil {
		return "", false, err
	}
	if err := write(files, staging, version); err != nil {
		return "", false, err
	}

	if replaced {
		old := staging + ".old"
		if err := os.Rename(target, old); err != nil {
			return "", false, err
		}
		if err := os.Rename(staging, target); err != nil {
			// Put the old one back rather than leave nothing.
			os.Rename(old, target)
			return "", false, err
		}
		if err := os.RemoveAll(old); err != nil {
			return "", false, err
		}
		return previous, true, nil
	}
	if err := os.Rename(staging, target); err != nil {
		return "", false, err
	}
	return "", false, nil
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
