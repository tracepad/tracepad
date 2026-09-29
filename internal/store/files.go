package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The files on disk (spec 044 #12, #13). The database holds every prompt and
// completion, the accounts' password hashes and the media-upload signing key,
// so the files are their owner's alone: the directory 0700, the database, its
// `-wal`, `-shm` and pre-migration backups 0600.
//
// Enforced on every start and not only on create, because every install that
// predates this was created 0644 under the usual umask, and the files — not the
// directory someone else may have made — are what the server can vouch for.
// The directory is tightened only while it holds nothing but the database's
// own files (spec 044 #18): one the operator shares with other things —
// `--data-dir .`, a home directory — keeps its mode, with a warning. Never
// fatal: a filesystem without Unix modes, or a directory the process does not
// own, must still start, and says so.

const (
	dataDirMode  os.FileMode = 0o700
	dataFileMode os.FileMode = 0o600
)

// BackupLifetime is how long the newest pre-migration backup is kept (spec 044
// #12): long enough to notice an upgrade went wrong and swap the file back,
// and fixed for the reason the project grace window is (spec 005 #9).
const BackupLifetime = 7 * 24 * time.Hour

// chmod is os.Chmod, a seam for the test that makes it fail.
var chmod = os.Chmod

// secureFiles creates the data directory if it is missing, tightens it and
// every database file already there, and creates a missing database file empty
// at 0600 so that SQLite — which gives `-wal` and `-shm` the mode of the
// database file — opens it rather than creating it under the umask.
func secureFiles(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dataDirMode); err != nil {
		return err
	}
	tightenDir(dir, filepath.Base(path))

	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if err := createOwnerOnly(path); err != nil && !errors.Is(err, fs.ErrExist) {
			logger().Warn("could not create the database file owner-only; SQLite will create it", "path", path, "err", err)
		}
	}
	for _, file := range append([]string{path, path + "-wal", path + "-shm", path + LockSuffix}, backupFiles(path)...) {
		tighten(file, dataFileMode)
	}
	return nil
}

// tighten takes group and other access away from one file or directory. A
// file that is not there is nothing to do; one whose mode cannot be changed is
// a warning naming it and its mode.
func tighten(path string, mode os.FileMode) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Mode().Perm()&0o077 == 0 {
		return
	}
	if err := chmod(path, mode); err != nil {
		logger().Warn("could not make a database file readable by its owner only",
			"path", path, "mode", info.Mode().Perm().String(), "err", err)
	}
}

// tightenDir takes group and other access away from the data directory when
// every entry in it is the database's own — the file, its log, its index, its
// backups. Anything else there means the directory is not the server's alone
// to set, and a mode the operator chose for other files is left as it is.
func tightenDir(dir, db string) {
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm()&0o077 == 0 {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		logger().Warn("could not list the data directory to decide its mode", "dir", dir, "err", err)
		return
	}
	for _, entry := range entries {
		if !databaseFile(db, entry) && !systemEntry(entry.Name()) {
			logger().Warn("the data directory is open to other users and holds more than the database, so its mode is left as it is; "+
				"give the database a directory of its own, or chmod 700 this one",
				"dir", dir, "mode", info.Mode().Perm().String(), "other", entry.Name())
			return
		}
	}
	tighten(dir, dataDirMode)
}

// databaseFile says whether a directory entry is one of the database's own
// files: the database, its `-wal`, `-shm` and `-journal`, or a pre-migration
// backup.
func databaseFile(db string, entry fs.DirEntry) bool {
	switch name := entry.Name(); name {
	case db, db + "-wal", db + "-shm", db + "-journal", db + LockSuffix:
		return true
	default:
		return isBackup(db, entry)
	}
}

// systemEntry says whether a name is one the operating system puts in a
// directory on its own — a filesystem's `lost+found` at the root of a
// dedicated volume, the Finder's `.DS_Store` and `._` files — so that a
// directory holding only those and the database is still the database's own.
func systemEntry(name string) bool {
	return name == "lost+found" || name == ".DS_Store" || strings.HasPrefix(name, "._")
}

// isBackup says whether a directory entry is a pre-migration backup of the
// database named db: a regular file named `<db>.pre-<migration>.bak`. Matched
// by name rather than by a glob over the path, so that a data directory whose
// name holds `[` or `*` names only its own backups — and a link, which a chmod
// or a remove would follow elsewhere or leave behind, is not one.
func isBackup(db string, entry fs.DirEntry) bool {
	name, prefix, suffix := entry.Name(), db+".pre-", ".bak"
	return entry.Type().IsRegular() && len(name) > len(prefix)+len(suffix) &&
		strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix)
}

// backupFiles lists the pre-migration backups beside the database, oldest name
// first. A migration's name starts with its number, so name order — the order
// the directory is read in — is the order they were written in.
func backupFiles(path string) []string {
	dir, db := filepath.Dir(path), filepath.Base(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		logger().Warn("could not list the data directory for pre-migration backups", "dir", dir, "err", err)
		return nil
	}
	var backups []string
	for _, entry := range entries {
		if isBackup(db, entry) {
			backups = append(backups, filepath.Join(dir, entry.Name()))
		}
	}
	return backups
}

// createOwnerOnly creates an empty file at 0600 for SQLite to write into — the
// database before its first open, a backup before its `VACUUM INTO`. SQLite
// accepts an empty existing file and keeps its mode; left to create the file
// itself it would use the umask's 0644. A file already there is an error.
func createOwnerOnly(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, dataFileMode)
	if err != nil {
		return err
	}
	return f.Close()
}

// removeBackups deletes every pre-migration backup but keep, logging each by
// name. It runs once the migrations a backup guards have committed, which is
// the only moment the older ones stop being the way back from anything.
func removeBackups(path, keep string) {
	for _, file := range backupFiles(path) {
		if file == keep {
			continue
		}
		if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logger().Warn("could not remove a superseded pre-migration backup", "backup", file, "err", err)
			continue
		}
		logger().Info("removed a superseded pre-migration backup", "backup", file)
	}
}

// Backup is a pre-migration backup as the erasure answer names it (spec 044
// #12): when it was written, and the moment after which the sweeper's next
// pass removes it — not a promise of that moment, since a pass runs on its
// interval and not while the server is down.
type Backup struct {
	Path        string
	CreatedAt   int64 // Unix ns: the file's modification time
	RemoveAfter int64 // CreatedAt plus BackupLifetime
}

// newestBackup is the most recently written backup beside the database at
// path, nil when there is none. The one lookup both the erasure's answer and
// the recovery hint of a failed start use, so they cannot name different
// files.
func newestBackup(path string) *Backup {
	var newest *Backup
	for _, file := range backupFiles(path) {
		info, err := os.Stat(file)
		if err != nil {
			continue
		}
		created := info.ModTime().UnixNano()
		if newest == nil || created > newest.CreatedAt {
			newest = &Backup{Path: file, CreatedAt: created, RemoveAfter: created + int64(BackupLifetime)}
		}
	}
	return newest
}

// PreMigrationBackup is the newest backup beside the database, nil when there
// is none.
func (s *Store) PreMigrationBackup() *Backup { return newestBackup(s.path) }

// expireBackups deletes the backups written more than BackupLifetime before
// now, each by name in the log. It is the sweeper's; the migration runner
// removes the superseded ones as soon as a run commits.
func (s *Store) expireBackups(now time.Time) {
	for _, file := range backupFiles(s.path) {
		info, err := os.Stat(file)
		if err != nil || now.Sub(info.ModTime()) < BackupLifetime {
			continue
		}
		if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logger().Warn("could not remove an expired pre-migration backup", "backup", file, "err", err)
			continue
		}
		logger().Info("removed a pre-migration backup past its seven days", "backup", file)
	}
}
