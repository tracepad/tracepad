//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package store

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A file system with no locks to give — NFS without a lock daemon, some FUSE
// volumes — is a warning and not a refusal to start: a deployment that ran on
// it yesterday runs on it today (spec 001 #20).
func TestAFileSystemWithoutLocksStartsWithAWarning(t *testing.T) {
	logged := captureLog(t)
	old := takeLock
	takeLock = func(*os.File) error { return syscall.ENOLCK }
	t.Cleanup(func() { takeLock = old })

	lock, err := LockDatabase(filepath.Join(t.TempDir(), "tracepad.db"))
	if err != nil {
		t.Fatalf("no locks on the file system refused the start: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if !strings.Contains(logged(), "cannot lock files") {
		t.Errorf("the start did not say it was unguarded:\n%s", logged())
	}

	takeLock = func(*os.File) error { return syscall.EACCES }
	if _, err := LockDatabase(filepath.Join(t.TempDir(), "tracepad.db")); err == nil {
		t.Error("an error that is not about support was swallowed")
	}
}
