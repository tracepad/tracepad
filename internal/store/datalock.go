package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// LockSuffix is what the lock file's name adds to the database's: the file a
// running server holds locked is `<database>.lock` (spec 001 #20), so the guard
// is tied to the file it protects and is one of that file's own.
const LockSuffix = ".lock"

// DataLock is a server's hold on its database, for as long as it runs.
type DataLock struct {
	file *os.File
}

// takeLock is lockFile, a seam for the test that gives it a file system without locks.
var takeLock = lockFile

// LockDatabase takes the exclusive lock of the database at dbPath, creating
// its directory and the lock file if they are missing, and returns at once:
// when another process holds it, it says so, and gives the pid that process
// last recorded. The lock is the operating system's, on the open file, so it is
// gone the moment the holder is — a killed server leaves nothing to clean up,
// which a pid file would. A file system that cannot lock at all is a warning and
// not a refusal to start (spec 001 #20): a deployment that ran on it yesterday
// runs on it today, unguarded, and is told.

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

// The pieces below are the same protocol for a process that is not the
// server: `tracepad upgrade` holds a database while it archives and swaps it,
// asks whether a server holds one, and reads who last did (spec 054 #28). One
// protocol, here, so the two sides cannot drift.

// lockTries and LockRetry are how long a lock found held is asked again
// before it counts as held: a child a process is starting holds a copy of
// every descriptor between its fork and its exec, so a lock let go a moment
// ago can look held for that moment. LockRetry is a variable for tests that
// start no process, where a held lock is held.
const lockTries = 5

var LockRetry = 10 * time.Millisecond

// TryLock takes the lock of the database at dbPath as a server takes it, and
// returns at once: ok is false when another process holds it. Unlike a
// server's, it records no pid. On a file system that cannot lock there is
// nothing to hold, and it says ok with a release that does nothing, as a
// server starts there unguarded.
func TryLock(dbPath string) (release func(), ok bool, err error) {
	file, err := os.OpenFile(dbPath+LockSuffix, os.O_RDWR|os.O_CREATE, dataFileMode)
	if err != nil {
		return nil, false, err
	}
	for try := 0; ; try++ {
		err := takeLock(file)
		switch {
		case err == nil:
			return func() {
				_ = unlockFile(file)
				_ = file.Close()
			}, true, nil
		case isLockUnsupported(err):
			file.Close()
			return func() {}, true, nil
		case isLockHeld(err) && try < lockTries:
			time.Sleep(LockRetry)
		case isLockHeld(err):
			file.Close()
			return nil, false, nil
		default:
			file.Close()
			return nil, false, err
		}
	}
}

// Locked says whether a process holds the lock of the database at dbPath now.
// A lock taken here to ask is let go at once; a missing lock file is no
// holder.
func Locked(dbPath string) bool {
	if _, err := os.Stat(dbPath + LockSuffix); err != nil {
		return false
	}
	release, ok, err := TryLock(dbPath)
	if err != nil {
		return false
	}
	if ok {
		release()
	}
	return !ok
}

// RecordedPID is the pid the last server to hold the database at dbPath
// wrote into its lock: it took the lock first, so a live process with this pid
// holds it.
func RecordedPID(dbPath string) (int, error) {
	b, err := os.ReadFile(dbPath + LockSuffix)
	if err != nil {
		return 0, err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	return strconv.Atoi(strings.TrimSpace(line))
}
