package skills

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The skill's shell is run by an agent's tool, which is sh, bash or zsh
// depending on the machine — zsh on a Mac (spec 037 #19). zsh does not split
// an unquoted expansion into words, and keeps names of its own: `${v:+--to
// "$v"}` stayed one word there, and `path=package` emptied PATH, so every
// command after it was not found. Every block is parsed by each shell, may
// not assign a name zsh keeps, and the one line that builds arguments from a
// variable is run in each.

// A shell block of the skill: its file, its first line, its text.
type block struct {
	file string
	line int
	text string
}

func shellBlocks(t *testing.T) []block {
	t.Helper()
	var blocks []block
	err := fs.WalkDir(os.DirFS("tracepad"), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(name, ".md") {
			return err
		}
		b, err := os.ReadFile(filepath.Join("tracepad", name))
		if err != nil {
			return err
		}
		// drift_test's fence, indented or not (a block in a list is
		// indented); a block that is not shell is passed over whole.
		var cur *block
		open, indent := false, ""
		for i, l := range strings.Split(string(b), "\n") {
			switch {
			case fence.MatchString(l) && !open:
				open, indent = true, l[:len(l)-len(strings.TrimLeft(l, " \t"))]
				if lang := strings.TrimPrefix(strings.TrimSpace(l), "```"); lang == "sh" || lang == "bash" || lang == "shell" {
					cur = &block{file: name, line: i + 2}
				}
			case fence.MatchString(l):
				if cur != nil {
					blocks = append(blocks, *cur)
				}
				open, cur = false, nil
			case cur != nil:
				cur.text += strings.TrimPrefix(l, indent) + "\n"
			}
		}
		if open {
			return fmt.Errorf("%s: a fence is never closed", name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) < 20 {
		t.Fatalf("found %d shell blocks; the check would pass anything", len(blocks))
	}
	return blocks
}

// shells are the shells to run a block in. zsh is required where CI runs:
// a check that skips there checks nothing.
func shells(t *testing.T) []string {
	t.Helper()
	var found []string
	for _, sh := range []string{"sh", "bash", "zsh"} {
		path, err := exec.LookPath(sh)
		switch {
		case err == nil:
			found = append(found, path)
		case os.Getenv("CI") != "":
			t.Fatalf("%s is not installed, and the skill's shell is run in it", sh)
		default:
			t.Logf("%s is not installed here; not checked", sh)
		}
	}
	return found
}

// placeholderWord is what a person fills in, `<trace-id>`: a word to the
// shell once filled, so a word here.
var placeholderWord = regexp.MustCompile(`<[A-Za-z][A-Za-z0-9_.-]*>`)

func TestTheSkillsShellParsesInEveryShell(t *testing.T) {
	t.Parallel()
	blocks := shellBlocks(t)
	for _, sh := range shells(t) {
		for _, b := range blocks {
			text := placeholderWord.ReplaceAllString(b.text, "filled")
			if out, err := exec.Command(sh, "-n", "-c", text).CombinedOutput(); err != nil {
				t.Errorf("%s:%d does not parse in %s: %v %s", b.file, b.line, sh, err, out)
			}
		}
	}
}

// zshKeeps are the lower-case names zsh gives a meaning of its own: one
// assigned is not the snippet's variable (`path` is PATH, `status` is
// read-only). Upper-case names are the environment's, and the skill sets
// some on purpose.
var zshKeeps = []string{"argv", "cdpath", "commands", "fignore", "fpath", "functions", "histchars", "history",
	"mailpath", "manpath", "module_path", "options", "parameters", "path", "pipestatus", "prompt", "psvar",
	"signals", "status", "watch", "zsh_eval_context", "aliases", "dirstack", "jobdirs", "jobstates", "jobtexts",
	"modules", "nameddirs", "reswords", "userdirs"}

var (
	assignment = regexp.MustCompile(`(?:^|[\s;&|({])([a-z_][a-z0-9_]*)=`)
	lowerName  = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	// What else sets a name with no `=` (the eighth review of #228): a
	// loop's or a menu's variable, getopts', and the words after read or
	// a declaration.
	loopName    = regexp.MustCompile(`(?:^|[\s;&|({])(?:for|select)\s+([a-z_][a-z0-9_]*)\b`)
	getoptsName = regexp.MustCompile(`(?:^|[\s;&|({])getopts\s+\S+\s+([a-z_][a-z0-9_]*)\b`)
	declaration = regexp.MustCompile(`(?:^|[\s;&|({])(?:read|typeset|local|declare|export|readonly|integer|float)\b([^;&|\n)]*)`)
)

// bound are the names a block of shell sets.
func bound(text string) []string {
	var names []string
	for _, re := range []*regexp.Regexp{assignment, loopName, getoptsName} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			names = append(names, m[1])
		}
	}
	for _, m := range declaration.FindAllStringSubmatch(text, -1) {
		for _, word := range strings.Fields(m[1]) {
			if name, _, _ := strings.Cut(word, "="); lowerName.MatchString(name) {
				names = append(names, name)
			}
		}
	}
	return names
}

func TestTheSkillAssignsNoNameZshKeeps(t *testing.T) {
	t.Parallel()
	keeps := slices.Clone(zshKeeps)
	// What zsh itself says it has, where it runs: the list above is the
	// floor, never the ceiling.
	if out, err := exec.Command("zsh", "-f", "-c", `print -l ${(k)parameters}`).Output(); err == nil {
		for _, name := range strings.Fields(string(out)) {
			if lowerName.MatchString(name) && name != "_" {
				keeps = append(keeps, name)
			}
		}
	}
	for _, b := range shellBlocks(t) {
		for _, name := range bound(b.text) {
			if slices.Contains(keeps, name) {
				t.Errorf("%s:%d sets %s, which zsh keeps for itself", b.file, b.line, name)
			}
		}
	}
	for _, c := range []string{"x=1; path=package", "for path in a b; do :; done", "read -r status", "select path in a b; do break; done",
		"getopts ab path", "typeset -a path", "local status=1", "while read -r a status; do :; done"} {
		if !slices.ContainsFunc(bound(c), func(n string) bool { return n == "path" || n == "status" }) {
			t.Errorf("the check does not see what %q sets: %q", c, bound(c))
		}
	}
}

// releaseTemp is the rule every release since 0.1.0 tells the bridge's
// directory by (internal/upgrade's mktempName): a binary run from it plans
// for the installed one, never for itself. Frozen: the releases out there
// keep it whatever this tree says.
var releaseTemp = regexp.MustCompile(`^tmp\.[A-Za-z0-9]{6,}$`)

// The bridge of upgrade.md passes `--to` only when a version was named, as
// two words, in every shell; each run fetches into a directory of its own
// (the live run of 0.1.1: one shared directory let a session's rm take the
// binary another had fetched), which the releases out there know.
func TestTheUpgradeBridgeNamesTheVersionInEveryShell(t *testing.T) {
	t.Parallel()
	var line string
	for _, b := range shellBlocks(t) {
		if b.file == "references/upgrade.md" && strings.Contains(b.text, "mktemp -d") {
			line = strings.TrimSpace(b.text)
		}
	}
	if !strings.HasPrefix(line, "v=;") {
		t.Fatalf("no bridge in upgrade.md that starts with v=;: %q", line)
	}
	// curl answers an install script that puts a tracepad recording its
	// arguments into TRACEPAD_INSTALL_DIR.
	stub := t.TempDir()
	installer := "#!/bin/sh\ncat <<'EOF'\nprintf '#!/bin/sh\\nprintf \"%%s\\\\n\" \"$@\" > \"$ARGS\"\\n' > \"$TRACEPAD_INSTALL_DIR/tracepad\"\nchmod +x \"$TRACEPAD_INSTALL_DIR/tracepad\"\nEOF\n"
	if err := os.WriteFile(filepath.Join(stub, "curl"), []byte(installer), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, sh := range shells(t) {
		home := t.TempDir()
		for v, want := range map[string]string{"": "upgrade\n--plan\n", "0.2.0": "upgrade\n--plan\n--to\n0.2.0\n"} {
			args := filepath.Join(t.TempDir(), "args")
			cmd := exec.Command(sh, "-c", strings.Replace(line, "v=;", "v="+v+";", 1))
			cmd.Env = []string{"PATH=" + stub + ":/usr/bin:/bin", "ARGS=" + args, "HOME=" + home, "TMPDIR=" + t.TempDir()}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s, v=%q: %v %s", sh, v, err, out)
			}
			if got, _ := os.ReadFile(args); string(got) != want {
				t.Errorf("%s, v=%q: the plan was run with %q, want %q", sh, v, got, want)
			}
		}
		// Two runs, two directories, each one a release tells.
		dirs, err := os.ReadDir(filepath.Join(home, ".cache", "tracepad"))
		if err != nil || len(dirs) != 2 {
			t.Fatalf("%s: two runs made %d directories (%v), want one each", sh, len(dirs), err)
		}
		for _, d := range dirs {
			if !releaseTemp.MatchString(d.Name()) {
				t.Errorf("%s: the bridge's directory %s is not one a release tells for a temporary one", sh, d.Name())
			}
		}
		// A download that fails runs no bridge an earlier run left (the
		// review of #228): curl fails, sh reads nothing and exits 0.
		home = t.TempDir()
		stale := filepath.Join(home, ".cache", "tracepad", "tmp.release", "tracepad")
		ran := filepath.Join(t.TempDir(), "ran")
		if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(stale, []byte("#!/bin/sh\n: > \"$RAN\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		failing := t.TempDir()
		if err := os.WriteFile(filepath.Join(failing, "curl"), []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(sh, "-c", strings.Replace(line, "v=;", "v=0.2.0;", 1))
		cmd.Env = []string{"PATH=" + failing + ":/usr/bin:/bin", "RAN=" + ran, "HOME=" + home}
		if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "STOP: the release did not download") {
			t.Errorf("%s: a failed download: %v %s", sh, err, out)
		}
		if _, err := os.Stat(ran); err == nil {
			t.Errorf("%s: a failed download ran the bridge an earlier run left", sh)
		}
	}
}

// A STOP after the skill has started something stops it first (the live run
// of 0.1.1): a server left running, or a container on the volume it made,
// meets the next try as "a container named … is there already". A block
// that defines stop() starts something; from its first start on, every exit
// comes right after stop.
func TestEveryExitAfterAStartStopsWhatStarted(t *testing.T) {
	t.Parallel()
	start := regexp.MustCompile(`docker run |[^&]&\s*$`)
	exit := regexp.MustCompile(`(\S+)\s*;?\s*exit\b`)
	unstopped := func(text string) []string {
		var bad []string
		started := false
		for _, l := range strings.Split(text, "\n") {
			started = started || start.MatchString(l)
			for _, m := range exit.FindAllStringSubmatch(l, -1) {
				if started && m[1] != "stop;" && m[1] != "stop" {
					bad = append(bad, strings.TrimSpace(l))
				}
			}
		}
		return bad
	}
	if unstopped("stop() { :; }\ndocker run -d x || { stop; exit 1; }\n[ -n \"$sk\" ] || { echo \"STOP: no key\"; exit 1; }") == nil {
		t.Fatal("the check does not see an exit after a start that does not stop it")
	}
	n := 0
	for _, b := range shellBlocks(t) {
		if !strings.Contains(b.text, "stop()") {
			continue
		}
		n++
		for _, l := range unstopped(b.text) {
			t.Errorf("%s:%d exits after a start without stopping it: %s", b.file, b.line, l)
		}
	}
	if n == 0 {
		t.Fatal("no block defines stop(); the check would pass anything")
	}
}

// The skill names no pre-release: a pin it gives is copied as written, and a
// candidate's goes stale with the next release (the live run of 0.1.0 found
// three). A spelling is written with letters (`X.Y.Z-rc.N`).
func TestTheSkillNamesNoPreRelease(t *testing.T) {
	t.Parallel()
	pre := regexp.MustCompile(`\d+\.\d+\.\d+(-rc\.?\d+|rc\d+|-alpha|-beta)`)
	if !pre.MatchString("tracepad==0.1.0rc2") || !pre.MatchString("@v0.1.0-rc.2") || pre.MatchString("0.1.0 X.Y.Z-rc.N") {
		t.Fatal("the pattern does not tell a pre-release")
	}
	err := fs.WalkDir(os.DirFS("tracepad"), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(filepath.Join("tracepad", name))
		if err != nil {
			return err
		}
		for i, l := range strings.Split(string(b), "\n") {
			if m := pre.FindString(l); m != "" {
				t.Errorf("%s:%d names the pre-release %s", name, i+1, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
