//go:build unix

package config

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A named pipe given as TRACEPAD_ADMIN_TOKEN_FILE is refused before it is
// opened: opening one waits for a writer, and the start would hang without a
// word (spec 001 #18).
func TestAdminTokenFileRefusesANamedPipe(t *testing.T) {
	t.Setenv("TRACEPAD_ADMIN_TOKEN", "")
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRACEPAD_ADMIN_TOKEN_FILE", fifo)
	refused := make(chan error, 1)
	go func() { _, err := Load(nil); refused <- err }()
	select {
	case err := <-refused:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("err = %v, want a named pipe refused", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the start is waiting on a named pipe")
	}
}
