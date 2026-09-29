//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !windows

package store

import "os"

// A platform with neither flock nor LockFileEx runs without the guard.
func lockFile(*os.File) error      { return nil }
func unlockFile(*os.File) error    { return nil }
func isLockHeld(error) bool        { return false }
func isLockUnsupported(error) bool { return false }
