//go:build unix

package upgrade

import (
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tracepad/tracepad/internal/store"
)

// isMountPoint says whether dir is on another device than its parent: a
// mount point, which cannot be renamed aside.
func isMountPoint(dir string) bool {
	var self, parent syscall.Stat_t
	if syscall.Stat(dir, &self) != nil || syscall.Stat(filepath.Dir(dir), &parent) != nil {
		return false
	}
	return self.Dev != parent.Dev
}

// freeBytes is the room for this user on the file system that holds dir.
func freeBytes(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// lockHeld says whether a server holds the data directory's database now: its
// lock file is locked (spec 001 #20). A lock taken here to ask is released at
// once.
func lockHeld(dataDir string) bool {
	f, err := os.OpenFile(filepath.Join(dataDir, dataDBName+store.LockSuffix), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	for try := 0; ; try++ {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			return false
		}
		if err != syscall.EWOULDBLOCK || try == lockTries {
			return err == syscall.EWOULDBLOCK
		}
		time.Sleep(lockRetry)
	}
}

// lockTries and lockRetry are how long a lock found held is asked again
// before it counts as held: a child this process is starting holds a copy of
// every descriptor between its fork and its exec, so a lock let go a moment
// ago can look held for that moment.
const (
	lockTries = 10
	lockRetry = 20 * time.Millisecond
)

// lockFile takes an exclusive lock on path without waiting; ok is false when
// another process holds it.
func lockFile(path string) (release func(), ok bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	for try := 0; ; try++ {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, true, nil
		}
		if err != syscall.EWOULDBLOCK || try == lockTries {
			f.Close()
			if err == syscall.EWOULDBLOCK {
				return nil, false, nil
			}
			return nil, false, err
		}
		time.Sleep(lockRetry)
	}
}
