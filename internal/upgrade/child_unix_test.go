//go:build unix

package upgrade

import (
	"context"
	"syscall"
	"testing"
)

// A child is in a process group of its own, so a terminal's SIGINT to the
// command's group does not reach it.
func TestAChildIsInAGroupOfItsOwn(t *testing.T) {
	cmd := child(context.Background(), "sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if pgid == syscall.Getpgrp() {
		t.Errorf("the child shares the command's process group %d", pgid)
	}
}
