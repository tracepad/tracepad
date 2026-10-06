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
func statFree(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}

// ownedByMe says whether a file is this user's.
func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// sameDevice says whether two paths are on one file system; one that cannot
// be looked at is taken to be, which counts its room twice, never once.
func sameDevice(a, b string) bool {
	var x, y syscall.Stat_t
	if syscall.Stat(a, &x) != nil || syscall.Stat(b, &y) != nil {
		return true
	}
	return x.Dev == y.Dev
}
