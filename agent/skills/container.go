package skills

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tracepad/tracepad/internal/config"
)

// containerHint is the command that does what `docker exec … skills install`
// cannot (#17): docs/agents.md's, character for character, which a test holds
// it to — the refusal quotes the doc rather than keeping a second copy of the
// advice that could age on its own.
const containerHint = `docker run --rm --user "$(id -u):$(id -g)" \
  -v "$HOME/.claude/skills:/skills" \
  ghcr.io/tracepad/tracepad skills install --dir /skills`

// checkContainerTarget refuses an install inside a container whose target is
// not on a mounted volume (#17). The image sets TRACEPAD_IN_CONTAINER; there,
// anything written outside a volume lands in the container's writable layer or
// in memory, which `docker rm` takes — the install would print "installed" for
// a skill that is gone with the container, and the image has no shell to look
// for it with. A host, with the variable unset, is not asked anything.
func checkContainerTarget(opt Options, target string) error {
	if !config.IsOn(opt.Env("TRACEPAD_IN_CONTAINER")) {
		return nil
	}
	resolved, err := resolveExisting(target)
	if err != nil {
		return refuseInContainer(target, target, fmt.Sprintf("cannot tell where it is (%v)", err))
	}
	mounts, err := opt.MountInfo()
	if err != nil {
		return refuseInContainer(target, resolved, fmt.Sprintf("the list of this container's mounts cannot be read (%v), "+
			"so there is no telling whether it survives the container", err))
	}
	defer mounts.Close()
	on, err := mountOf(resolved, mounts)
	if err != nil {
		return refuseInContainer(target, resolved, fmt.Sprintf("the list of this container's mounts cannot be read through (%v), "+
			"so there is no telling whether it survives the container", err))
	}
	switch {
	case on.point == "" || on.point == "/":
		return refuseInContainer(target, resolved, "it is in the container's writable layer, which is deleted with the container")
	case volatileFS[on.fstype]:
		return refuseInContainer(target, resolved, fmt.Sprintf("it is on %s (%s), which is memory or the container's own and goes with it", on.fstype, on.point))
	}
	return nil
}

// refuseInContainer is every refusal of #17, so each carries the remedy: one
// path — the resolved one, with the requested one beside it when they differ —
// why it was refused, and the command that works.
func refuseInContainer(requested, resolved, why string) error {
	path := resolved
	if requested != resolved {
		path += " (given as " + requested + ")"
	}
	return fmt.Errorf("not installing to %s: this is a container, and %s. "+
		"Run the install in a container of its own, onto a directory you mount:\n\n%s\n\n"+
		"See docs/agents.md, From the Docker image", path, why, containerHint)
}

// resolveExisting is path with its symbolic links resolved as far as it
// exists: the target of an install usually does not yet, and the mount it will
// be created on is its nearest existing ancestor's.
func resolveExisting(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	for {
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		parent := filepath.Dir(path)
		if parent == path || !os.IsNotExist(err) {
			return "", err
		}
		rest = filepath.Join(filepath.Base(path), rest)
		path = parent
	}
}

// volatileFS are the file systems that are not a volume, whatever mounts them:
// memory (tmpfs and ramfs — `--tmpfs`, /dev/shm, a pod's emptyDir with
// `medium: Memory`), the image's own layers (overlay, aufs), and the kernel's
// (proc, sysfs, devtmpfs, cgroups, …). A file written there is gone with the
// container as surely as one in the root.
var volatileFS = map[string]bool{
	"tmpfs": true, "ramfs": true, "overlay": true, "aufs": true,
	"devtmpfs": true, "devpts": true, "mqueue": true, "proc": true, "sysfs": true,
	"cgroup": true, "cgroup2": true,
}

// mount is one line of mountinfo that matters here.
type mount struct{ point, fstype string }

// mountOf is the mount path lies on, from /proc/self/mountinfo (proc(5)): the
// last entry in the list whose mount point contains it. The list is in the
// order the mounts were made, so a later mount over a directory hides an
// earlier one at or below it — a volume at /a/b with a tmpfs mounted over /a
// afterwards is not where /a/b/x is written. An empty result is no mount at all.
//
// An entry reads `36 35 98:0 /root /mnt rw,noatime master:1 - ext3 /dev/root rw`:
// the fifth field is the mount point (with space, tab, newline and backslash as
// octal escapes), and the file system type follows the lone `-`.
func mountOf(path string, mountinfo io.Reader) (mount, error) {
	var on mount
	scanner := bufio.NewScanner(mountinfo)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		point := unescapeMount(fields[4])
		if !(path == point || point == "/" || strings.HasPrefix(path, strings.TrimSuffix(point, "/")+"/")) {
			continue
		}
		on = mount{point: point}
		for i, f := range fields[5:] {
			if f == "-" && 5+i+1 < len(fields) {
				on.fstype = fields[5+i+1]
				break
			}
		}
	}
	return on, scanner.Err()
}

// unescapeMount undoes mountinfo's \040-style escapes: a backslash and exactly
// three octal digits. Anything else is left as it is.
func unescapeMount(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var out strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+3 < len(field) {
			if n, err := strconv.ParseUint(field[i+1:i+4], 8, 8); err == nil {
				out.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		out.WriteByte(field[i])
	}
	return out.String()
}
