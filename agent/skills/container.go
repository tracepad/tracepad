package skills

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/tracepad/tracepad/internal/config"
)

// containerHint is the command that does what `docker exec … skills install`
// cannot (#17), the one docs/agents.md prints.
const containerHint = `docker run --rm --user "$(id -u):$(id -g)" -v "$HOME/.claude/skills:/skills" IMAGE skills install --dir /skills`

// checkContainerTarget refuses an install inside a container whose target is
// not on a mounted volume (#17). The image sets TRACEPAD_IN_CONTAINER; there,
// anything written outside a mount lands in the container's writable layer,
// which `docker rm` takes — the install would print "installed" for a skill
// that is gone with the container, and the image has no shell to look for it
// with. A host, with the variable unset, is not asked anything.
func checkContainerTarget(opt Options, target string) error {
	if !config.IsOn(opt.Env("TRACEPAD_IN_CONTAINER")) {
		return nil
	}
	resolved, err := resolveExisting(target)
	if err != nil {
		return fmt.Errorf("cannot tell where %s is: %w", target, err)
	}
	mounts, err := opt.MountInfo()
	if err != nil {
		return fmt.Errorf("this is a container (TRACEPAD_IN_CONTAINER is set), and the list of its mounts "+
			"cannot be read (%v), so there is no telling whether %s survives it; install from a "+
			"container of its own onto a mounted directory:\n\n  %s\n\nSee docs/agents.md, From the Docker image",
			err, target, containerHint)
	}
	defer mounts.Close()
	mounted, err := onMount(resolved, mounts)
	if err != nil {
		return fmt.Errorf("cannot read the mounts of this container: %w", err)
	}
	if mounted {
		return nil
	}
	return fmt.Errorf("not installing to %s: this is a container, and that path is in its writable layer, "+
		"which is deleted with the container (the image has no shell to remove it by hand either). "+
		"Run the install in a container of its own, onto a directory you mount:\n\n  %s\n\n"+
		"See docs/agents.md, From the Docker image", resolved, containerHint)
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

// onMount reports whether path lies on a mount other than the root of the
// mount namespace: the longest mount point that contains it is the one it is
// on, and in a container the root is the image's writable layer, while a
// volume or a bind mount is a mount point of its own. mountinfo is the format
// of /proc/self/mountinfo (proc(5)): the fifth field is the mount point, with
// space, tab, newline and backslash written as octal escapes.
func onMount(path string, mountinfo io.Reader) (bool, error) {
	best := ""
	scanner := bufio.NewScanner(mountinfo)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		point := unescapeMount(fields[4])
		if (path == point || strings.HasPrefix(path, strings.TrimSuffix(point, "/")+"/")) && len(point) > len(best) {
			best = point
		}
	}
	if err := scanner.Err(); err != nil {
		return false, err
	}
	return best != "" && best != "/", nil
}

// unescapeMount undoes mountinfo's \040-style escapes.
func unescapeMount(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	var out strings.Builder
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+3 < len(field) {
			var n int
			if _, err := fmt.Sscanf(field[i+1:i+4], "%03o", &n); err == nil {
				out.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		out.WriteByte(field[i])
	}
	return out.String()
}
