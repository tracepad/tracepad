//go:build unix

package store

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// On a file system that cannot lock, a process that is not the server is
// told so (spec 054 #39): TryLock holds nothing and says why, and Locked
// cannot say "free". The server itself still starts, unguarded (#20).
func TestALockThatCannotBeTakenIsNeverFree(t *testing.T) {
	saved := takeLock
	takeLock = func(*os.File) error { return syscall.ENOLCK }
	t.Cleanup(func() { takeLock = saved })
	db := filepath.Join(t.TempDir(), "tracepad.db")
	if _, ok, err := TryLock(db); ok || !errors.Is(err, ErrLocksUnsupported) {
		t.Errorf("TryLock: ok %v, %v", ok, err)
	}
	if held, err := Locked(db); held || !errors.Is(err, ErrLocksUnsupported) {
		t.Errorf("Locked: held %v, %v", held, err)
	}
	server, err := LockDatabase(db)
	if err != nil {
		t.Fatalf("the server did not start unguarded: %v", err)
	}
	server.Close()
}
