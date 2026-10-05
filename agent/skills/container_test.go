package skills

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
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

// mnt is one mount of a /proc/self/mountinfo.
type mnt struct{ point, fstype string }

// mountsOf is a /proc/self/mountinfo the way a container sees it: an overlay
// root, the pseudo file systems, and then the given mounts, in the order given
// (the order they were made in).
func mountsOf(mounts ...mnt) func() (io.ReadCloser, error) {
	lines := []string{
		"1 0 0:50 / / rw,relatime - overlay overlay rw,lowerdir=/l,upperdir=/u,workdir=/w",
		"2 1 0:51 / /proc rw,nosuid,nodev,noexec,relatime - proc proc rw",
		"3 1 0:52 / /dev rw,nosuid - tmpfs tmpfs rw,size=65536k,mode=755",
	}
	for i, m := range mounts {
		lines = append(lines, strings.Join([]string{"10" + string(rune('0'+i)), "1", "8:1", "/skills", m.point, "rw,relatime", "-", m.fstype, "/dev/vda1", "rw"}, " "))
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

// refused asserts the shape every refusal of #17 has: exit 1, nothing on
// stdout, the remedy quoted whole, and no more than the one path.
func refused(t *testing.T, got result, mention ...string) {
	t.Helper()
	if got.code != exitFailure {
		t.Fatalf("exit = %d, want %d; stdout %q stderr %q", got.code, exitFailure, got.stdout, got.stderr)
	}
	if got.stdout != "" {
		t.Errorf("stdout = %q, want nothing: no line may claim an install", got.stdout)
	}
	for _, want := range append([]string{"this is a container", containerHint, "docs/agents.md"}, mention...) {
		if !strings.Contains(got.stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, got.stderr)
		}
	}
}

// `docker exec … skills install` writes to the container's own layer and, until
// #17, said "installed": the skill was gone with the container. Not on a
// mount, the install is refused, says what to run instead, and writes nothing.
func TestAnInstallOffAMountIsRefusedInAContainer(t *testing.T) {
	dir := t.TempDir()
	got := runInContainer(t, inContainer(), mountsOf(), "install", "--dir", dir)
	refused(t, got, "writable layer")
	if _, err := os.Stat(filepath.Join(dir, Name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused install wrote %s (stat err %v)", filepath.Join(dir, Name), err)
	}
	// --force is about what may be replaced, not where.
	forced := runInContainer(t, inContainer(), mountsOf(), "install", "--force", "--dir", dir)
	refused(t, forced, "writable layer")
	if _, err := os.Stat(filepath.Join(dir, Name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the forced install wrote %s", filepath.Join(dir, Name))
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
			got := runInContainer(t, inContainer(), mountsOf(mnt{point, "ext4"}), "install", "--dir", dir)
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

// A tmpfs is a mount and not a volume: `--tmpfs`, /dev/shm (Docker always
// mounts one), a pod's emptyDir with medium Memory. The install would print
// "installed" for files that go with the container.
func TestAMemoryMountIsNotAVolume(t *testing.T) {
	dir := t.TempDir()
	for _, fstype := range []string{"tmpfs", "ramfs", "overlay"} {
		got := runInContainer(t, inContainer(), mountsOf(mnt{resolved(t, dir), fstype}), "install", "--dir", dir)
		refused(t, got, fstype)
	}
	// /dev/shm as Docker has it; the directory need not exist for the answer.
	shm := runInContainer(t, inContainer(), mountsOf(mnt{"/dev/shm", "tmpfs"}), "install", "--dir", "/dev/shm/skills")
	refused(t, shm, "tmpfs", "/dev/shm")
}

// The list is in mount order, and the last mount that contains the path is
// where it is written: a tmpfs mounted over a volume's parent hides the volume,
// and a volume mounted into a tmpfs is the volume.
func TestTheLastMountThatContainsThePathWins(t *testing.T) {
	dir := t.TempDir()
	real, parent := resolved(t, dir), filepath.Dir(resolved(t, dir))

	hidden := runInContainer(t, inContainer(), mountsOf(mnt{real, "ext4"}, mnt{parent, "tmpfs"}), "install", "--dir", dir)
	refused(t, hidden, "tmpfs")
	if _, err := os.Stat(filepath.Join(dir, Name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an install under a hiding tmpfs wrote %s", filepath.Join(dir, Name))
	}

	shown := runInContainer(t, inContainer(), mountsOf(mnt{parent, "tmpfs"}, mnt{real, "ext4"}), "install", "--dir", dir)
	if shown.code != exitOK {
		t.Fatalf("a volume mounted into a tmpfs was refused: %s", shown.stderr)
	}
}

// The default place is the nonroot user's home in the layer; --project is the
// working directory's. Neither is a volume.
func TestTheDefaultPlacesAreRefusedInAContainer(t *testing.T) {
	home := t.TempDir()
	env := inContainer()
	env["HOME"] = home
	for _, args := range [][]string{{"install"}, {"install", "--project"}} {
		refused(t, runInContainer(t, env, mountsOf(), args...))
	}
	if _, err := os.Stat(filepath.Join(home, ".claude")); err == nil {
		t.Error("the refused install made ~/.claude")
	}
}

// A symbolic link is resolved before the mount is looked for: a link in the
// layer to a mounted directory is on the mount, and a link on the mount to a
// directory in the layer is not.
func TestALinkIsResolvedBeforeTheMountIsLookedFor(t *testing.T) {
	layer, mounted := t.TempDir(), t.TempDir()
	if err := os.Symlink(mounted, filepath.Join(layer, "into")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(layer, filepath.Join(mounted, "out")); err != nil {
		t.Fatal(err)
	}
	mounts := mountsOf(mnt{resolved(t, mounted), "ext4"})

	in := runInContainer(t, inContainer(), mounts, "install", "--dir", filepath.Join(layer, "into"))
	if in.code != exitOK {
		t.Fatalf("a link to a mounted directory was refused: %s", in.stderr)
	}
	if _, err := os.Stat(filepath.Join(mounted, Name, "SKILL.md")); err != nil {
		t.Error(err)
	}

	out := runInContainer(t, inContainer(), mounts, "install", "--dir", filepath.Join(mounted, "out"))
	refused(t, out, resolved(t, layer), "given as "+filepath.Join(mounted, "out"))
	if _, err := os.Stat(filepath.Join(layer, Name)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an install through a link out of the mount wrote %s", filepath.Join(layer, Name))
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

// Not knowing is not a pass, and a refusal for not knowing carries the remedy
// as every other does: the list cannot be opened, cannot be read through, or the
// target cannot be resolved.
func TestNotKnowingRefusesWithTheRemedy(t *testing.T) {
	dir := t.TempDir()
	unopenable := func() (io.ReadCloser, error) { return nil, errors.New("no /proc here") }
	refused(t, runInContainer(t, inContainer(), unopenable, "install", "--dir", dir), "no /proc here")

	broken := func() (io.ReadCloser, error) { return io.NopCloser(iotest.ErrReader(errors.New("read failed"))), nil }
	refused(t, runInContainer(t, inContainer(), broken, "install", "--dir", dir), "read failed")

	tooLong := func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(strings.Repeat("x", 2<<20) + "\n")), nil
	}
	refused(t, runInContainer(t, inContainer(), tooLong, "install", "--dir", dir), "token too long")

	// A path that cannot be resolved at all (an embedded NUL fails in the
	// kernel's lookup, not as "does not exist").
	refused(t, runInContainer(t, inContainer(), mountsOf(), "install", "--dir", "/a\x00b"), "cannot tell where it is")
}

// The remedy is docs/agents.md's own command, not a second copy of it.
func TestTheHintIsTheDocumentedCommand(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "agents.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), containerHint) {
		t.Errorf("docs/agents.md does not contain the command the refusal quotes:\n%s", containerHint)
	}
}

func TestMountOf(t *testing.T) {
	info := strings.Join([]string{
		`1 0 0:50 / / rw - overlay overlay rw`,
		`4 1 8:1 /v /data rw shared:1 - ext4 /dev/vda1 rw`,
		`5 1 8:1 /w /with\040space rw - ext4 /dev/vda1 rw`,
		`6 4 8:1 /x /data/nested rw - xfs /dev/vda2 rw`,
		`7 1 8:1 /etc /etc/hosts rw - ext4 /dev/vda1 rw`,
		`8 1 0:60 / /run/shm rw - tmpfs shm rw`,
	}, "\n")
	later := info + "\n" + `9 1 8:1 /z /data rw - btrfs /dev/vda3 rw`
	for _, c := range []struct {
		name, info, path, point, fstype string
	}{
		{"a volume", info, "/data", "/data", "ext4"},
		{"under a volume", info, "/data/skills/tracepad", "/data", "ext4"},
		{"a nested mount is the one it is under", info, "/data/nested/x", "/data/nested", "xfs"},
		{"a later mount over the parent hides it", later, "/data/nested/x", "/data", "btrfs"},
		{"a prefix that is not a parent", info, "/data2/skills", "/", "overlay"},
		{"the layer", info, "/home/nonroot/.claude/skills/tracepad", "/", "overlay"},
		{"an escaped space", info, "/with space/skills", "/with space", "ext4"},
		{"a file mounted", info, "/etc/hosts", "/etc/hosts", "ext4"},
		{"beside a file mounted", info, "/etc/skills", "/", "overlay"},
		{"memory", info, "/run/shm/x", "/run/shm", "tmpfs"},
	} {
		got, err := mountOf(c.path, strings.NewReader(c.info))
		if err != nil || got != (mount{c.point, c.fstype}) {
			t.Errorf("%s: mountOf(%q) = %+v, %v; want %s on %s", c.name, c.path, got, err, c.fstype, c.point)
		}
	}
}

// Escapes are a backslash and exactly three octal digits (proc(5)); anything
// that is not one is the text it is.
func TestUnescapeMount(t *testing.T) {
	for in, want := range map[string]string{
		`/a\040b`:     "/a b",
		`/a\134b`:     `/a\b`,
		`/a\0400`:     "/a 0",
		`/a\12x`:      `/a\12x`,  // two digits and a letter: not an escape
		`/a\1`:        `/a\1`,    // cut short
		`/a\999b`:     `/a\999b`, // not octal
		`/a\777b`:     `/a\777b`, // does not fit a byte
		`/a\+12b`:     `/a\+12b`, // a sign is not a digit
		`/plain`:      "/plain",
		`/a\011b\012`: "/a\tb\n",
	} {
		if got := unescapeMount(in); got != want {
			t.Errorf("unescapeMount(%q) = %q, want %q", in, got, want)
		}
	}
}
