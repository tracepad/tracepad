package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LockSuffix is what the lock file's name adds to the database's: the file a
// running server holds locked is `<database>.lock` (spec 001 #20), so the guard
// is tied to the file it protects and is one of that file's own.
const LockSuffix = ".lock"

// DataLock is a server's hold on its database, for as long as it runs.
type DataLock struct {
	file *os.File
}

// LockDatabase takes the exclusive lock of the database at dbPath, creating
// its directory and the lock file if they are missing, and returns at once:
// when another process holds it, it says so, and gives the pid that process
// last recorded. The lock is the operating system's, on the open file, so it is
// gone the moment the holder is — a killed server leaves nothing to clean up,
// which a pid file would. A file system that cannot lock at all is a warning and
// not a refusal to start (spec 001 #20): a deployment that ran on it yesterday
// runs on it today, unguarded, and is told.
// takeLock is lockFile, a seam for the test that gives it a file system without locks.
var takeLock = lockFile

func LockDatabase(dbPath string) (*DataLock, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, dataDirMode); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	path := dbPath + LockSuffix
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, dataFileMode)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := takeLock(file); err != nil {
		switch {
		case isLockHeld(err):
			hint := recordedPid(file)
			file.Close()
			return nil, fmt.Errorf("another tracepad is already running on the database %s%s: "+
				"stop it, or give this one a directory of its own with --data-dir", dbPath, hint)
		case isLockUnsupported(err):
			file.Close()
			logger().Warn("this file system cannot lock files, so nothing stops a second server on the same data directory; "+
				"do not run two", "path", path, "err", err)
			return &DataLock{}, nil
		default:
			file.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
	}
	// Who holds it, for the next process that finds it held: a hint, and said
	// as one. Written after the lock is taken, so it can be a crashed holder's
	// number, a reused pid, or a container's pid 1.
	if err := recordPid(file); err != nil {
		logger().Warn("could not record this server's pid in its lock file; a refused second server will not be able to name it",
			"path", path, "err", err)
	}
	return &DataLock{file: file}, nil
}

// Close releases the lock. A process that exits releases it without this.
func (l *DataLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := unlockFile(l.file)
	if cerr := l.file.Close(); err == nil {
		err = cerr
	}
	l.file = nil
	return err
}

// recordPid writes the pid where a refused process can read it: past the byte
// range the lock takes on a platform whose locks are mandatory (see lockFile).
func recordPid(file *os.File) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	_, err := file.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return err
}

// recordedPid is what the last holder wrote, as " (the process that last
// recorded itself there had pid N, which may be stale)", or nothing.
func recordedPid(file *os.File) string {
	buf := make([]byte, 32)
	n, _ := file.ReadAt(buf, 0)
	pid := strings.TrimSpace(string(buf[:n]))
	if _, err := strconv.Atoi(pid); err != nil {
		return ""
	}
	return " (the process that last recorded itself there had pid " + pid +
		", which may be stale or another container's)"
}
