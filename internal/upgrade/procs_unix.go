//go:build unix

package upgrade

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// alive is kill(pid, 0): a process that exists, whether or not this user may
// signal it.
func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	// A zombie answers kill(pid, 0) and runs nothing: it is gone, whatever
	// reaps it (the final review).
	return (err == nil || errors.Is(err, syscall.EPERM)) && !isZombie(pid)
}

// started is a process this command started and reaps.
type started struct {
	pid  int
	done chan struct{}
}

func (s *started) PID() int                { return s.pid }
func (s *started) Exited() <-chan struct{} { return s.done }

// startDetached starts a server in a session of its own, so it outlives this
// command and the terminal it ran in, and reaps it if it exits while the
// command still runs (a zombie would answer kill(pid, 0)).
func startDetached(spec StartSpec) (Started, error) {
	log, err := os.OpenFile(spec.Log, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	// A server is the one process the command starts outside child(): a
	// session of its own, so it outlives the command. A binary just put in
	// place may be busy for a moment (ETXTBSY); each try is a fresh command.
	var cmd *exec.Cmd
	if err := retryBusy(func() error {
		cmd = &exec.Cmd{
			Path:        spec.Path,
			Args:        spec.Argv,
			Env:         spec.Env,
			Dir:         spec.Dir,
			Stdout:      log,
			Stderr:      log,
			SysProcAttr: &syscall.SysProcAttr{Setsid: true},
		}
		return cmd.Start()
	}); err != nil {
		return nil, err
	}
	s := &started{pid: cmd.Process.Pid, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait() // ignored: the reaping of a server that exited; its exit is what Exited says
		close(s.done)
	}()
	return s, nil
}
