//go:build unix

package skills

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// The setup's start, run: on data it made, no key is a STOP that stops the
// server it started; on data that was there — the earlier install the human
// chose to start — the server keeps running and the key is the human's (the
// review of #231). A stand-in tracepad serves until it is killed.
func TestTheSetupStopsOnlyWhatItMade(t *testing.T) {
	t.Parallel()
	var start string
	for _, b := range shellBlocks(t) {
		if b.file == "references/setup.md" && strings.HasPrefix(b.text, "port=4318; declare=yes;") {
			start = b.text
		}
	}
	if start == "" {
		t.Fatal("no start block in setup.md")
	}
	start = strings.Replace(start, "declare=yes;", "declare=no;", 1)
	bin := t.TempDir()
	stub := "#!/bin/sh\ncase \"$1\" in\nversion) echo 0.1.1 ;;\nhealth) echo '{\"ok\":true}' ;;\nserve) echo $$ > \"$PIDS\"; echo 'listening addr=localhost:4318'; exec sleep 30 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "tracepad"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, earlier := range []bool{false, true} {
		home, data, pids := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "pid")
		if earlier {
			_ = os.WriteFile(filepath.Join(data, "tracepad.db"), nil, 0o600)
		}
		cmd := exec.Command("sh", "-c", start)
		cmd.Dir = t.TempDir()
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home, "TRACEPAD_DATA_DIR=" + data, "PIDS=" + pids}
		out, err := cmd.CombinedOutput()
		b, _ := os.ReadFile(pids)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		alive := pid > 0 && syscall.Kill(pid, 0) == nil
		if alive {
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGTERM) })
		}
		switch {
		case !earlier && (err == nil || !strings.Contains(string(out), "STOP: no key") || alive):
			t.Errorf("fresh data: %v, the server alive %v: %s", err, alive, out)
		case earlier && (err != nil || !strings.Contains(string(out), "KEY: ") || !alive):
			t.Errorf("the data that was there: %v, the server alive %v: %s", err, alive, out)
		}
	}
}
