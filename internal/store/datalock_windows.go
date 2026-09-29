//go:build windows

package store

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// The lock is one byte far past the pid: Windows locks are mandatory, and a
// lock on byte 0 would refuse the reads by which a second server finds the
// first one's pid.
const lockOffset = 0x7fffffff

func lockFile(f *os.File) error {
	overlapped := windows.Overlapped{Offset: lockOffset}
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
}

func unlockFile(f *os.File) error {
	overlapped := windows.Overlapped{Offset: lockOffset}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlapped)
}

func isLockHeld(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

func isLockUnsupported(err error) bool {
	return errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_CALL_NOT_IMPLEMENTED)
}
