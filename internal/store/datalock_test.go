package store

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// A second server on a database refuses at once and says why, giving the pid
// the first recorded as a hint; the first keeps the database, and once it lets
// go the database is free again (spec 001 #20).
func TestASecondServerOnADatabaseIsRefused(t *testing.T) {
	db := filepath.Join(t.TempDir(), "data", "tracepad.db")
	first, err := LockDatabase(db)
	if err != nil {
		t.Fatal(err)
	}

	_, err = LockDatabase(db)
	if err == nil {
		t.Fatal("a second lock on the same database was granted")
	}
	for _, want := range []string{"another tracepad is already running", db, "pid " + strconv.Itoa(os.Getpid()), "may be stale", "--data-dir"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := LockDatabase(db)
	if err != nil {
		t.Fatalf("the database is not free after the first server let go: %v", err)
	}
	again.Close()
}

// A pid file that is empty or holds something else names no one: the refusal
// stands and says nothing it does not know.
func TestARefusalNamesNoPidItCannotRead(t *testing.T) {
	db := filepath.Join(t.TempDir(), "tracepad.db")
	first, err := LockDatabase(db)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	for _, content := range []string{"", "not a number\n"} {
		if err := os.WriteFile(db+LockSuffix, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := LockDatabase(db)
		if err == nil || !strings.Contains(err.Error(), "another tracepad is already running") {
			t.Fatalf("with %q in the lock file: %v, want the refusal", content, err)
		}
		if strings.Contains(err.Error(), "pid") {
			t.Errorf("with %q in the lock file the refusal names a pid: %v", content, err)
		}
	}
}

// The lock takes one file, which is the database's own: owner-only, and not a
// reason for the directory to count as shared.
func TestTheLockFileIsTheDatabasesOwn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	lock, err := LockDatabase(filepath.Join(dir, "tracepad.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "tracepad.db"+LockSuffix {
		t.Fatalf("the data directory holds %v (%v), want the lock file alone", entries, err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(filepath.Join(dir, entries[0].Name()))
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("the lock file is %v, want owner-only", info.Mode().Perm())
		}
	}
	if !databaseFile("tracepad.db", entries[0]) {
		t.Error("the lock file makes the data directory look shared with something else")
	}
	// A file of the same name beside another database is not this one's.
	if databaseFile("other.db", entries[0]) {
		t.Error("a lock file of tracepad.db counts as other.db's own")
	}
}

// Closing twice, or a lock that was never taken, is not an error.
func TestClosingALockTwice(t *testing.T) {
	lock, err := LockDatabase(filepath.Join(t.TempDir(), "tracepad.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	var none *DataLock
	if err := none.Close(); err != nil {
		t.Errorf("Close on nil: %v", err)
	}
}

// The test binary, run again as the server that holds the lock: what the
// feature is for is another process, and the lock going when that process does.
const lockHolderEnv = "TRACEPAD_TEST_LOCK_HOLDER"

func TestLockHolderProcess(t *testing.T) {
	db := os.Getenv(lockHolderEnv)
	if db == "" {
		t.Skip("run by TestALockHeldByAnotherProcessGoesWithIt")
	}
	if _, err := LockDatabase(db); err != nil {
		t.Fatal(err)
	}
	os.Stdout.WriteString("locked\n")
	bufio.NewReader(os.Stdin).ReadByte() // until the parent lets go of the pipe, or kills this
}

func TestALockHeldByAnotherProcessGoesWithIt(t *testing.T) {
	db := filepath.Join(t.TempDir(), "tracepad.db")
	child := exec.Command(os.Args[0], "-test.run=^TestLockHolderProcess$")
	child.Env = append(os.Environ(), lockHolderEnv+"="+db)
	stdin, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := false
	t.Cleanup(func() {
		if !reaped {
			child.Process.Kill()
			child.Wait()
		}
	})
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "locked\n" {
		t.Fatalf("the holder said %q, %v, want it locked", line, err)
	}

	_, err = LockDatabase(db)
	if err == nil {
		t.Fatal("a lock held by another process was granted")
	}
	if want := "pid " + strconv.Itoa(child.Process.Pid); !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal %q does not give the holder's %q", err, want)
	}

	// Killed, not asked to leave: nothing is cleaned up, and the database is free.
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	child.Wait()
	reaped = true
	freed, err := LockDatabase(db)
	if err != nil {
		t.Fatalf("the database is still locked after its holder was killed: %v", err)
	}
	freed.Close()
}
