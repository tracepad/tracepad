//go:build unix

package upgrade

import (
	"errors"
	"os/exec"
	"syscall"
)

// textBusy is execve's ETXTBSY: the file is open for writing somewhere — in
// a child of this process between its fork and its exec, for a binary this
// process has just written.
func textBusy(err error) bool { return errors.Is(err, syscall.ETXTBSY) }

func ownGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
