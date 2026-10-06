package store

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// The protocol a process that is not the server uses (spec 054 #28): a hold
// a server cannot take, a question that answers who holds it, and the pid a
// server recorded.
func TestTheLockAsAnotherProcessSeesIt(t *testing.T) {
	db := filepath.Join(t.TempDir(), "tracepad.db")
	if Locked(db) {
		t.Fatal("no lock file, and locked")
	}
	server, err := LockDatabase(db)
	if err != nil {
		t.Fatal(err)
	}
	if !Locked(db) {
		t.Error("a server holds it, and it is not locked")
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
	if Locked(db) {
		t.Error("let go, and still locked")
	}
	_ = os.WriteFile(db+LockSuffix, []byte("not a pid\n"), 0o600)
	if _, err := RecordedPID(db); err == nil {
		t.Error("a record that is not a pid")
	}
	_ = strconv.Itoa
}
