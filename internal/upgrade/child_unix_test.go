//go:build unix

package upgrade

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// A child is in a process group of its own, so a terminal's SIGINT to the
// command's group does not reach it.
func TestAChildIsInAGroupOfItsOwn(t *testing.T) {
	t.Parallel()
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

// A server started with no environment at all (`env -i`) starts again with
// none: a nil environment would hand it the command's own (the final review).
func TestAnEmptyEnvironmentStartsEmpty(t *testing.T) {
	t.Parallel()
	log := filepath.Join(t.TempDir(), "out")
	env, err := exec.LookPath("env")
	if err != nil {
		t.Skip("no env here")
	}
	for _, given := range [][]string{nil, {}} {
		s, err := startDetached(StartSpec{Path: env, Argv: []string{"env"}, Env: given, Dir: "/", Log: log})
		if err != nil {
			t.Fatal(err)
		}
		<-s.Exited()
	}
	if out, _ := os.ReadFile(log); len(out) > 0 {
		t.Errorf("the server was given an environment: %q", out)
	}
}

// The process listing takes its caller's deadline (the final review): one
// cut short is an error, never a shorter list taken for the whole.
func TestAListingCutShortIsAnError(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if procs, _, err := newSystem().Candidates(ctx); err == nil {
		t.Errorf("a cancelled listing answered %d processes and no error", len(procs))
	}
}
