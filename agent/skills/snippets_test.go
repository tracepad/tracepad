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
)

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
		for _, m := range assignment.FindAllStringSubmatch(b.text, -1) {
			if slices.Contains(keeps, m[1]) {
				t.Errorf("%s:%d assigns %s, which zsh keeps for itself", b.file, b.line, m[1])
			}
		}
	}
	if bad := assignment.FindAllStringSubmatch("x=1; path=package", -1); len(bad) != 2 || bad[1][1] != "path" {
		t.Fatalf("the check does not see an assignment: %q", bad)
	}
}

// The bridge of upgrade.md passes `--to` only when a version was named, as
// two words, in every shell.
func TestTheUpgradeBridgeNamesTheVersionInEveryShell(t *testing.T) {
	t.Parallel()
	var line string
	for _, b := range shellBlocks(t) {
		if b.file == "references/upgrade.md" && strings.Contains(b.text, "tracepad/tmp.release") {
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
		for v, want := range map[string]string{"": "upgrade\n--plan\n", "0.2.0": "upgrade\n--plan\n--to\n0.2.0\n"} {
			args := filepath.Join(t.TempDir(), "args")
			cmd := exec.Command(sh, "-c", strings.Replace(line, "v=;", "v="+v+";", 1))
			cmd.Env = []string{"PATH=" + stub + ":/usr/bin:/bin", "ARGS=" + args, "HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir()}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%s, v=%q: %v %s", sh, v, err, out)
			}
			if got, _ := os.ReadFile(args); string(got) != want {
				t.Errorf("%s, v=%q: the plan was run with %q, want %q", sh, v, got, want)
			}
		}
		// A download that fails runs no bridge an earlier run left (the
		// review of #228): curl fails, sh reads nothing and exits 0.
		home := t.TempDir()
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
