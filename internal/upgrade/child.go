package upgrade

import (
	"context"
	"os/exec"
	"time"
)

// child is every process the command starts but a server: docker, busybox
// through it, a tracepad asked its version, lsof, gh.
// Each goes into a process group of its own (spec 054 #26): a Ctrl-C at the
// terminal is sent to the foreground group, and must reach the command alone,
// which decides — a docker run half way through a restore, or a `tracepad
// version` a way back depends on, must not die of it. The command ends a
// child through ctx, never through the terminal. Nothing in this package
// calls exec.Command itself; TestEveryChildGoesThroughOneDoor holds it to that.
// busyTries and busyWait are how often, and how far apart, a run of a file
// this process has just written is tried again while Linux says it is busy:
// a child of another goroutine holds the file's write descriptor between its
// fork and its exec, for a moment (spec 054 #28; the Go toolchain retries
// the same way).
const (
	busyTries = 50
	busyWait  = 20 * time.Millisecond
)

// retryBusy runs start until it does not fail with ETXTBSY, a second at most.
func retryBusy(start func() error) error {
	for try := 0; ; try++ {
		err := start()
		if !textBusy(err) || try == busyTries {
			return err
		}
		time.Sleep(busyWait)
	}
}

func child(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	ownGroup(cmd)
	return cmd
}
