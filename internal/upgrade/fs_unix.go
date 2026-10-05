//go:build unix

package upgrade

import (
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
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

// lockFile takes an exclusive lock on path without waiting; ok is false when
// another process holds it.
func lockFile(path string) (release func(), ok bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() { f.Close() }, true, nil
}
