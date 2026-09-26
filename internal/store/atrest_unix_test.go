//go:build unix

package store

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// underUmask022 runs the test under the umask most systems ship, the one that
// made every database file 0644 before the server set the modes itself.
func underUmask022(t *testing.T) {
	t.Helper()
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// A fresh store is its owner's alone: the directory 0700, the database, its
// log and its index 0600 — made so, not left to the umask (spec 044 #13).
func TestAFreshStoreIsOwnerOnly(t *testing.T) {
	underUmask022(t)
	path := filepath.Join(t.TempDir(), "data", "tracepad.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// A write, so the log and its index exist.
	if _, err := s.Bootstrap(nil); err != nil {
		t.Fatal(err)
	}

	if mode := modeOf(t, filepath.Dir(path)); mode != 0o700 {
		t.Errorf("data directory is %v, want 0700", mode)
	}
	for _, file := range []string{path, path + "-wal", path + "-shm"} {
		if mode := modeOf(t, file); mode != 0o600 {
			t.Errorf("%s is %v, want 0600", filepath.Base(file), mode)
		}
	}
}

// Every install before this one was created 0644 in a directory someone else
// may have made 0755; the next start tightens all of it, backups included.
func TestAStartTightensAnExistingInstall(t *testing.T) {
	underUmask022(t)
	s, path := openTemp(t)
	if _, err := s.Bootstrap(nil); err != nil {
		t.Fatal(err)
	}
	backup := path + ".pre-0022_media_holders.bak"
	if err := os.WriteFile(backup, []byte("an old copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	loose := []string{path, path + "-wal", path + "-shm", backup}
	for _, file := range loose {
		if err := os.Chmod(file, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if mode := modeOf(t, filepath.Dir(path)); mode != 0o700 {
		t.Errorf("data directory is %v, want 0700", mode)
	}
	for _, file := range loose {
		if _, err := os.Stat(file); errors.Is(err, os.ErrNotExist) {
			continue // Close checkpointed the log away
		}
		if mode := modeOf(t, file); mode != 0o600 {
			t.Errorf("%s is %v, want 0600", filepath.Base(file), mode)
		}
	}
}

// The backup the migration runner writes is 0600 from its first byte: it is
// the whole database.
func TestAnUpgradeWritesItsBackupOwnerOnly(t *testing.T) {
	underUmask022(t)
	s, path := openTemp(t)
	pendingAgain(t, s, false)
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if mode := modeOf(t, path+".pre-0024_compaction.bak"); mode != 0o600 {
		t.Errorf("the backup is %v, want 0600", mode)
	}
}

// A mode that cannot be changed — a filesystem without them, a directory the
// process does not own — is a warning naming the file, never a refusal.
func TestAModeThatCannotBeChangedIsAWarning(t *testing.T) {
	underUmask022(t)
	s, path := openTemp(t)
	s.Close()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	oldLogger, oldChmod := logger, chmod
	logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&logged, nil)) }
	chmod = func(string, os.FileMode) error { return errors.New("operation not permitted") }
	t.Cleanup(func() { logger, chmod = oldLogger, oldChmod })

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("a chmod that fails stopped the start: %v", err)
	}
	s2.Close()
	if !strings.Contains(logged.String(), "level=WARN") || !strings.Contains(logged.String(), path) ||
		!strings.Contains(logged.String(), "-rw-r--r--") {
		t.Errorf("want a WARN naming %s and its mode, got:\n%s", path, logged.String())
	}
}

// A directory that holds more than the database is not the server's alone to
// set (#18): it keeps the mode its owner gave it, with a warning naming it and
// what else is there. The database's own files are 0600 all the same.
func TestADirectoryHoldingOtherFilesKeepsItsMode(t *testing.T) {
	underUmask022(t)
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notes, []byte("someone else's"), 0o644); err != nil {
		t.Fatal(err)
	}

	var logged bytes.Buffer
	oldLogger := logger
	logger = func() *slog.Logger { return slog.New(slog.NewTextHandler(&logged, nil)) }
	t.Cleanup(func() { logger = oldLogger })

	path := filepath.Join(dir, "tracepad.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if mode := modeOf(t, dir); mode != 0o755 {
		t.Errorf("the shared directory is %v, want it left 0755", mode)
	}
	if mode := modeOf(t, notes); mode != 0o644 {
		t.Errorf("the other file is %v, want it untouched", mode)
	}
	if mode := modeOf(t, path); mode != 0o600 {
		t.Errorf("the database is %v, want 0600", mode)
	}
	if !strings.Contains(logged.String(), "level=WARN") || !strings.Contains(logged.String(), dir) ||
		!strings.Contains(logged.String(), "notes.txt") {
		t.Errorf("want a WARN naming %s and notes.txt, got:\n%s", dir, logged.String())
	}
}
