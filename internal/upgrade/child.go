package upgrade

import (
	"context"
	"os/exec"
)

// child is every process the command starts but a server: docker, busybox
// through it, a tracepad asked its version, lsof, systemctl, launchctl, gh.
// Each goes into a process group of its own (spec 054 #26): a Ctrl-C at the
// terminal is sent to the foreground group, and must reach the command alone,
// which decides — a docker run half way through a restore, or a `tracepad
// version` a way back depends on, must not die of it. The command ends a
// child through ctx, never through the terminal. Nothing in this package
// calls exec.Command itself; TestEveryChildGoesThroughOneDoor holds it to that.
func child(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	ownGroup(cmd)
	return cmd
}
