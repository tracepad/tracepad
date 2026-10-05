package skills

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runInContainer runs the command as the image runs it: TRACEPAD_IN_CONTAINER
// set, and the mounts the test says the container has.
func runInContainer(t *testing.T, env map[string]string, mountinfo func() (io.ReadCloser, error), args ...string) result {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(Options{
		Args:      args,
		Version:   "0.4.0",
		Stdout:    &stdout,
		Stderr:    &stderr,
		Env:       func(key string) string { return env[key] },
		Getwd:     func() (string, error) { return "/", nil },
		MountInfo: mountinfo,
	})
	return result{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// mountsOf is a /proc/self/mountinfo the way a container sees it: an overlay
// root, the pseudo file systems, and the given mount points.
func mountsOf(points ...string) func() (io.ReadCloser, error) {
	lines := []string{
		"1 0 0:50 / / rw,relatime - overlay overlay rw,lowerdir=/l,upperdir=/u,workdir=/w",
		"2 1 0:51 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw",
		"3 1 0:52 / /dev rw,nosuid - tmpfs tmpfs rw,size=65536k,mode=755",
	}
	for i, point := range points {
		lines = append(lines, strings.Join([]string{"10" + string(rune('0'+i)), "1", "8:1", "/skills", point, "rw,relatime", "-", "ext4", "/dev/vda1", "rw"}, " "))
	}
	return func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(strings.Join(lines, "\n") + "\n")), nil
	}
}

// resolved is dir as the command sees it: macOS's temporary directories are
// behind a link, and a mount point is the real path.
func resolved(t *testing.T, dir string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

func inContainer() map[string]string { return map[string]string{"TRACEPAD_IN_CONTAINER": "1"} }

// `docker exec … skills install` writes to the container's own layer and, until
// #17, said "installed": the skill was gone with the container. Not on a
// mount, the install is refused, says what to run instead, and writes nothing.
func TestAnInstallOffAMountIsRefusedInAContainer(t *testing.T) {
	dir := t.TempDir()
	got := runInContainer(t, inContainer(), mountsOf(), "install", "--dir", dir)
	if got.code != exitFailure {
		t.Fatalf("exit = %d, want %d; stdout %q stderr %q", got.code, exitFailure, got.stdout, got.stderr)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want nothing: no line may claim an install", got.stdout)
	}
	for _, want := range []string{"container", "writable layer", "docker run", "-v ", "skills install --dir /skills", "docs/agents.md"} {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, got.stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, Name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused install wrote %s (stat err %v)", filepath.Join(dir, Name), err)
	}
}

// The path docs/agents.md gives — a mounted directory — works as it always did.
func TestAnInstallOnAMountWorksInAContainer(t *testing.T) {
	dir := t.TempDir()
	for name, point := range map[string]string{
		"the directory is the mount": resolved(t, dir),
		"a parent is the mount":      filepath.Dir(resolved(t, dir)),
	} {
		t.Run(name, func(t *testing.T) {
			got := runInContainer(t, inContainer(), mountsOf(point), "install", "--dir", dir)
			if got.code != exitOK {
				t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
			}
			if !strings.Contains(got.stdout, "installed 0.4.0 to ") {
				t.Errorf("stdout = %q", got.stdout)
			}
			if _, err := os.Stat(filepath.Join(dir, Name, "SKILL.md")); err != nil {
				t.Error(err)
			}
			os.RemoveAll(filepath.Join(dir, Name))
		})
	}
}

// The default place is the nonroot user's home in the layer; --project is the
// working directory's. Neither is a volume.
func TestTheDefaultPlacesAreRefusedInAContainer(t *testing.T) {
	home := t.TempDir()
	env := inContainer()
	env["HOME"] = home
	for _, args := range [][]string{{"install"}, {"install", "--project"}} {
		got := runInContainer(t, env, mountsOf(), args...)
		if got.code != exitFailure || got.stdout != "" || !strings.Contains(got.stderr, "docker run") {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want the refusal", args, got.code, got.stdout, got.stderr)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); err == nil {
		t.Error("the refused install made ~/.claude")
	}
}

// A host sets nothing and is asked nothing, however its mounts look; so does a
// variable that says no.
func TestAHostIsNotAskedAboutMounts(t *testing.T) {
	for _, value := range []string{"", "0", "off"} {
		dir := t.TempDir()
		env := map[string]string{"TRACEPAD_IN_CONTAINER": value}
		failing := func() (io.ReadCloser, error) { return nil, errors.New("mounts must not be read here") }
		got := runInContainer(t, env, failing, "install", "--dir", dir)
		if got.code != exitOK {
			t.Errorf("TRACEPAD_IN_CONTAINER=%q: exit = %d, stderr = %s", value, got.code, got.stderr)
		}
	}
}

// Not knowing is not a pass: an install that cannot tell whether its target
// survives the container is refused, saying why.
func TestUnreadableMountsRefuseTheInstall(t *testing.T) {
	dir := t.TempDir()
	failing := func() (io.ReadCloser, error) { return nil, errors.New("no /proc here") }
	got := runInContainer(t, inContainer(), failing, "install", "--dir", dir)
	if got.code != exitFailure || got.stdout != "" {
		t.Fatalf("exit %d, stdout %q; want the refusal", got.code, got.stdout)
	}
	if !strings.Contains(got.stderr, "no /proc here") || !strings.Contains(got.stderr, "docker run") {
		t.Errorf("stderr = %s", got.stderr)
	}
}

func TestOnMount(t *testing.T) {
	info := strings.Join([]string{
		`1 0 0:50 / / rw - overlay overlay rw`,
		`4 1 8:1 /v /data rw - ext4 /dev/vda1 rw`,
		`5 1 8:1 /w /with\040space rw - ext4 /dev/vda1 rw`,
		`6 4 8:1 /x /data/nested rw - ext4 /dev/vda1 rw`,
		`7 1 8:1 /etc /etc/hosts rw - ext4 /dev/vda1 rw`,
	}, "\n")
	for _, c := range []struct {
		path string
		want bool
	}{
		{"/data", true},
		{"/data/skills/tracepad", true},
		{"/data/nested/x", true},
		{"/data2/skills", false}, // shares a prefix with /data, is not under it
		{"/home/nonroot/.claude/skills/tracepad", false},
		{"/with space/skills", true},
		{"/etc/hosts", true},
		{"/etc/skills", false},
		{"/", false},
	} {
		got, err := onMount(c.path, strings.NewReader(info))
		if err != nil || got != c.want {
			t.Errorf("onMount(%q) = %v, %v; want %v", c.path, got, err, c.want)
		}
	}
}
