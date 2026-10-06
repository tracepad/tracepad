package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The protocol a process that is not the server uses (spec 054 #28): a hold
// a server cannot take, a question that answers who holds it, and the pid a
// server recorded.
func TestTheLockAsAnotherProcessSeesIt(t *testing.T) {
	db := filepath.Join(t.TempDir(), "tracepad.db")
	if held, err := Locked(db); held || err != nil {
		t.Fatal("no lock file, and locked", err)
	}
	server, err := LockDatabase(db)
	if err != nil {
		t.Fatal(err)
	}
	if held, err := Locked(db); !held || err != nil {
		t.Error("a server holds it, and it is not locked", err)
	}
	if pid, err := RecordedPID(db); err != nil || pid != os.Getpid() {
		t.Errorf("recorded %d, %v", pid, err)
	}
	if _, ok, err := TryLock(db); ok || err != nil {
		t.Errorf("taken from under a server: %v %v", ok, err)
	}
	server.Close()
	release, ok, err := TryLock(db)
	if !ok || err != nil {
		t.Fatalf("free, and not taken: %v %v", ok, err)
	}
	if _, err := LockDatabase(db); err == nil {
		t.Error("a server started while the lock was held")
	}
	release()
	if held, err := Locked(db); held || err != nil {
		t.Error("let go, and still locked", err)
	}
	_ = os.WriteFile(db+LockSuffix, []byte("not a pid\n"), 0o600)
	if _, err := RecordedPID(db); err == nil {
		t.Error("a record that is not a pid")
	}
}

// A server that starts while another process holds the lock only to ask
// whether it is held starts all the same (spec 054 #37): it asks again for a
// moment before it refuses.
func TestAServerStartsThroughAProbe(t *testing.T) {
	db := filepath.Join(t.TempDir(), "tracepad.db")
	release, ok, err := TryLock(db)
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	go func() {
		time.Sleep(LockRetry)
		release()
	}()
	server, err := LockDatabase(db)
	if err != nil {
		t.Fatalf("a probe kept the server from starting: %v", err)
	}
	server.Close()
}

// A lock that cannot be asked is not free (spec 054 #37).
func TestALockThatCannotBeAskedIsAnError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root reads any directory")
	}
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "tracepad.db")
	if err := os.WriteFile(db+LockSuffix, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if held, err := Locked(db); err == nil {
		t.Errorf("an unreadable lock answered %v, with no error", held)
	}
}
