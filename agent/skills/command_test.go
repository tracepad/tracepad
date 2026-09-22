package skills

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// result is one run's whole observable behaviour.
type result struct {
	stdout, stderr string
	code           int
}

// runSkills runs the command with HOME and the working directory the test
// chooses, so nothing reaches the real home of whoever runs the suite.
func runSkills(t *testing.T, home, wd, version string, args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(Options{
		Args:    args,
		Version: version,
		Stdout:  &stdout,
		Stderr:  &stderr,
		Env: func(key string) string {
			if key == "HOME" {
				return home
			}
			return ""
		},
		Getwd: func() (string, error) { return wd, nil },
	})
	return result{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// TestInstallWritesEveryFileAndStampsTheVersion: the whole embedded set lands,
// SKILL.md carries the binary's version, the marker names it, and the command
// says what it did (#6, #7).
func TestInstallWritesEveryFileAndStampsTheVersion(t *testing.T) {
	dir := t.TempDir()
	got := runSkills(t, "", "", "0.4.0", "install", "--dir", dir)
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	target := filepath.Join(dir, Name)
	if want := "installed 0.4.0 to " + target + "\n"; got.stdout != want {
		t.Errorf("stdout = %q, want %q", got.stdout, want)
	}

	embedded := names(Files())
	if len(embedded) < 5 {
		t.Fatalf("the binary carries %d files, want SKILL.md and its references", len(embedded))
	}
	for _, name := range embedded {
		written, err := os.ReadFile(filepath.Join(target, filepath.FromSlash(name)))
		if err != nil {
			t.Errorf("%s was not written: %v", name, err)
			continue
		}
		source, _ := fs.ReadFile(Files(), name)
		if name == "SKILL.md" {
			if !strings.Contains(string(written), "\n  version: \"0.4.0\"\n") {
				t.Errorf("SKILL.md is not stamped with the version:\n%s", head(written))
			}
			continue
		}
		if !bytes.Equal(written, source) {
			t.Errorf("%s differs from the embedded file", name)
		}
	}
	marked, err := os.ReadFile(filepath.Join(target, marker))
	if err != nil || string(marked) != "0.4.0\n" {
		t.Errorf("marker = %q, %v; want the version", marked, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, ".tracepad-install-*")); len(leftovers) > 0 {
		t.Errorf("the staging directory was left behind: %v", leftovers)
	}
}

// TestSecondInstallUpdatesAndDropsWhatTheNewVersionLacks: replace-whole is what
// makes an update an update rather than a merge of two versions (#6).
func TestSecondInstallUpdatesAndDropsWhatTheNewVersionLacks(t *testing.T) {
	dir := t.TempDir()
	if got := runSkills(t, "", "", "0.3.1", "install", "--dir", dir); got.code != exitOK {
		t.Fatalf("first install: exit %d, %s", got.code, got.stderr)
	}
	target := filepath.Join(dir, Name)
	stale := filepath.Join(target, "references", "retired.md")
	if err := os.WriteFile(stale, []byte("# a reference 0.3.1 had\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := runSkills(t, "", "", "0.4.0", "install", "--dir", dir)
	if got.code != exitOK {
		t.Fatalf("second install: exit %d, %s", got.code, got.stderr)
	}
	if want := "updated 0.3.1 → 0.4.0 in " + target + "\n"; got.stdout != want {
		t.Errorf("stdout = %q, want %q", got.stdout, want)
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a file the new version does not have survived the update: %v", err)
	}
	if marked, _ := os.ReadFile(filepath.Join(target, marker)); string(marked) != "0.4.0\n" {
		t.Errorf("marker = %q after the update", marked)
	}

	again := runSkills(t, "", "", "0.4.0", "install", "--dir", dir)
	if want := "reinstalled 0.4.0 in " + target + "\n"; again.code != exitOK || again.stdout != want {
		t.Errorf("same version again: exit %d, stdout %q, want %q", again.code, again.stdout, want)
	}
}

// TestAnUnmarkedDirectoryIsRefused: a `tracepad` directory without the marker
// is somebody else's, so the default path is safe to run on a machine the
// command has never seen; --force is the one way past it (#6).
func TestAnUnmarkedDirectoryIsRefused(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, Name)
	theirs := filepath.Join(target, "notes.md")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(theirs, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := runSkills(t, "", "", "0.4.0", "install", "--dir", dir)
	if got.code != exitFailure {
		t.Fatalf("exit = %d, want %d; stderr %s", got.code, exitFailure, got.stderr)
	}
	if !strings.Contains(got.stderr, target) || !strings.Contains(got.stderr, "--force") {
		t.Errorf("the refusal should name the directory and the flag: %s", got.stderr)
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Errorf("the refused directory was touched: %v", err)
	}

	forced := runSkills(t, "", "", "0.4.0", "install", "--dir", dir, "--force")
	if forced.code != exitOK {
		t.Fatalf("--force: exit %d, %s", forced.code, forced.stderr)
	}
	if !strings.Contains(forced.stdout, "replacing a directory that was not an installed skill") {
		t.Errorf("--force should say what it replaced: %q", forced.stdout)
	}
	if _, err := os.Stat(theirs); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("--force kept the old contents: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "SKILL.md")); err != nil {
		t.Errorf("--force did not install: %v", err)
	}
}

// TestTheThreeTargets: the default is the user's Claude Code skills, --project
// the working directory's, --dir anywhere (#6).
func TestTheThreeTargets(t *testing.T) {
	home, wd, dir := t.TempDir(), t.TempDir(), t.TempDir()
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"install"}, filepath.Join(home, ".claude", "skills", Name)},
		{[]string{"install", "--project"}, filepath.Join(wd, ".claude", "skills", Name)},
		{[]string{"install", "--dir", filepath.Join(dir, ".agents", "skills")}, filepath.Join(dir, ".agents", "skills", Name)},
	}
	for _, c := range cases {
		got := runSkills(t, home, wd, "dev", c.args...)
		if got.code != exitOK {
			t.Errorf("%v: exit %d, %s", c.args, got.code, got.stderr)
			continue
		}
		if _, err := os.Stat(filepath.Join(c.want, "SKILL.md")); err != nil {
			t.Errorf("%v: nothing at %s: %v", c.args, c.want, err)
		}
		if !strings.Contains(got.stdout, c.want) {
			t.Errorf("%v: stdout %q does not name %s", c.args, got.stdout, c.want)
		}
	}
}

// TestNoHomeIsAnErrorNamingDir: in a container without HOME the default would
// be /.claude; the command says what to pass instead (edge cases).
func TestNoHomeIsAnErrorNamingDir(t *testing.T) {
	wd := t.TempDir()
	got := runSkills(t, "", wd, "dev", "install")
	if got.code != exitFailure || !strings.Contains(got.stderr, "--dir") {
		t.Errorf("exit %d, stderr %q; want 1 and a pointer to --dir", got.code, got.stderr)
	}
	if entries, _ := os.ReadDir(wd); len(entries) > 0 {
		t.Errorf("something was written without a target: %v", entries)
	}
}

// TestUsageErrorsExitTwo: the command contract's third code.
func TestUsageErrorsExitTwo(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{
		nil,
		{"uninstall"},
		{"install", "--project", "--dir", dir},
		{"install", "--dir", ""},
		{"install", "--global"},
		{"install", "somewhere"},
		{"show", "SKILL.md", "debugging.md"},
		{"show", "nothing-like-it.md"},
	} {
		got := runSkills(t, dir, dir, "dev", args...)
		if got.code != exitUsage {
			t.Errorf("%v: exit %d, want %d; stderr %s", args, got.code, exitUsage, got.stderr)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 0 {
		t.Errorf("a usage error wrote something: %v", entries)
	}
	help := runSkills(t, dir, dir, "dev", "--help")
	if help.code != exitOK || !strings.Contains(help.stdout, "tracepad skills install") {
		t.Errorf("--help: exit %d, stdout %q", help.code, help.stdout)
	}
}

// TestShowPrintsAndLists: SKILL.md stamped like an install would, a reference
// by any of the names a person would try, and the list when the name is wrong.
func TestShowPrintsAndLists(t *testing.T) {
	got := runSkills(t, "", "", "0.4.0", "show")
	if got.code != exitOK || !strings.HasPrefix(got.stdout, "---\nname: tracepad\n") {
		t.Fatalf("show: exit %d, stdout starts %q", got.code, head([]byte(got.stdout)))
	}
	if !strings.Contains(got.stdout, "\n  version: \"0.4.0\"\n") {
		t.Errorf("show printed an unstamped SKILL.md")
	}

	want, err := fs.ReadFile(Files(), "references/debugging.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"debugging.md", "debugging", "references/debugging.md"} {
		got := runSkills(t, "", "", "dev", "show", name)
		if got.code != exitOK || got.stdout != string(want) {
			t.Errorf("show %s: exit %d, %d bytes, want the reference's %d", name, got.code,
				len(got.stdout), len(want))
		}
	}

	unknown := runSkills(t, "", "", "dev", "show", "nothing-like-it.md")
	for _, name := range names(Files()) {
		if !strings.Contains(unknown.stderr, name) {
			t.Errorf("the refusal does not list %s: %s", name, unknown.stderr)
		}
	}
}

// TestStamp: the one line changes and nothing else does; a frontmatter without
// the line is an error rather than a skill with no version to check (#7).
func TestStamp(t *testing.T) {
	source := "---\nname: x\nmetadata:\n  version: dev\n---\n\n# body\n  version: not this one\n"
	got, err := Stamp([]byte(source), "1.2.0")
	if err != nil {
		t.Fatal(err)
	}
	want := "---\nname: x\nmetadata:\n  version: \"1.2.0\"\n---\n\n# body\n  version: not this one\n"
	if string(got) != want {
		t.Errorf("stamped:\n%s\nwant:\n%s", got, want)
	}
	for _, broken := range []string{
		"# no frontmatter\n",
		"---\nname: x\n---\n",
		"---\nname: x\nversion: dev\n---\n",
		"---\nname: x\nmetadata:\n  version: dev\n",
	} {
		if _, err := Stamp([]byte(broken), "1.2.0"); err == nil {
			t.Errorf("Stamp accepted %q", broken)
		}
	}
	if _, err := Stamp(mustRead(t, "SKILL.md"), "dev"); err != nil {
		t.Errorf("the shipped SKILL.md cannot be stamped: %v", err)
	}
}

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	content, err := fs.ReadFile(Files(), name)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func head(content []byte) string {
	if len(content) > 200 {
		return string(content[:200])
	}
	return string(content)
}

// TestWindowsReadsUserProfile: Windows keeps the home directory in USERPROFILE
// and usually has no HOME, which is os.UserHomeDir's rule too.
func TestWindowsReadsUserProfile(t *testing.T) {
	profile := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run(Options{
		Args: []string{"install"}, Version: "dev", Stdout: &stdout, Stderr: &stderr,
		Env: func(key string) string {
			if key == "USERPROFILE" {
				return profile
			}
			return ""
		},
		GOOS: "windows",
	})
	if code != exitOK {
		t.Fatalf("exit %d, %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(profile, ".claude", "skills", Name, "SKILL.md")); err != nil {
		t.Errorf("nothing under USERPROFILE: %v", err)
	}
}

// TestASymlinkedTargetIsFollowed: a skill kept in a dotfiles repository and
// linked into place is the copy that gets updated; the link stays a link.
func TestASymlinkedTargetIsFollowed(t *testing.T) {
	kept, dir := t.TempDir(), t.TempDir()
	if got := runSkills(t, "", "", "0.3.1", "install", "--dir", kept); got.code != exitOK {
		t.Fatalf("first install: %s", got.stderr)
	}
	link := filepath.Join(dir, Name)
	if err := os.Symlink(filepath.Join(kept, Name), link); err != nil {
		t.Fatal(err)
	}
	got := runSkills(t, "", "", "0.4.0", "install", "--dir", dir)
	if got.code != exitOK || !strings.Contains(got.stdout, "updated 0.3.1 → 0.4.0") {
		t.Fatalf("exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("the link was replaced: %v", err)
	}
	if marked, _ := os.ReadFile(filepath.Join(kept, Name, marker)); string(marked) != "0.4.0\n" {
		t.Errorf("the linked copy was not updated: marker %q", marked)
	}
}

// TestTheMarkerIsReadHonestly: an empty marker is still a marker, and one that
// cannot be read is an error saying so, not "somebody else's directory".
func TestTheMarkerIsReadHonestly(t *testing.T) {
	dir := t.TempDir()
	if got := runSkills(t, "", "", "0.3.1", "install", "--dir", dir); got.code != exitOK {
		t.Fatal(got.stderr)
	}
	stamp := filepath.Join(dir, Name, marker)
	if err := os.WriteFile(stamp, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := runSkills(t, "", "", "0.4.0", "install", "--dir", dir)
	if got.code != exitOK || !strings.Contains(got.stdout, "updated an unversioned install → 0.4.0") {
		t.Errorf("empty marker: exit %d, stdout %q", got.code, got.stdout)
	}

	if os.Getuid() == 0 {
		t.Skip("root reads a file whatever its mode")
	}
	if err := os.Chmod(stamp, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(stamp, 0o644) })
	unreadable := runSkills(t, "", "", "0.5.0", "install", "--dir", dir)
	if unreadable.code != exitFailure || !strings.Contains(unreadable.stderr, "cannot read") ||
		strings.Contains(unreadable.stderr, "not a skill") {
		t.Errorf("unreadable marker: exit %d, stderr %q", unreadable.code, unreadable.stderr)
	}
}

// TestAStagingDirectoryLeftByAKilledInstallIsRemoved: the prefix is this
// command's own, so the next install clears what a killed one left behind.
func TestAStagingDirectoryLeftByAKilledInstallIsRemoved(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, stagingPrefix+"123.old")
	if err := os.MkdirAll(filepath.Join(stale, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := runSkills(t, "", "", "dev", "install", "--dir", dir); got.code != exitOK {
		t.Fatal(got.stderr)
	}
	if _, err := os.Stat(stale); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the stale staging directory survived: %v", err)
	}
}
