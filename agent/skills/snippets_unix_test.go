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
	"time"
)

// setup.md's start, run with a stand-in tracepad that serves until it is
// killed and prints no key (the reviews of #231): no key is a STOP that
// stops the server it started, and a key an earlier start left in the log
// is never taken for this one's; a key declared reaches .env. A STOP takes
// with it the database the start's server made, its pid in the lock, so the
// next try finds the data fresh, and leaves one that was there before or
// that another server holds (spec 037 #21).
func TestTheSetupsStartStopsWhatItStarted(t *testing.T) {
	t.Parallel()
	var first string
	for _, b := range shellBlocks(t) {
		if b.file == "references/setup.md" && strings.HasPrefix(b.text, "port=4318; declare=yes;") {
			first = b.text
		}
	}
	if first == "" {
		t.Fatal("no start block in setup.md")
	}
	bin := t.TempDir()
	stub := "#!/bin/sh\ncase \"$1\" in\nversion) echo 0.1.1 ;;\nhealth) echo '{\"ok\":true}' ;;\nserve) echo $$ > \"$PIDS\"; [ -z \"$FOREIGN\" ] || { for f in tracepad.db tracepad.db-wal tracepad.db-shm; do echo theirs > \"$5/$f\"; done; echo 1 > \"$5/tracepad.db.lock\"; echo 'another tracepad is already running'; exit 1; }; [ -e \"$5/tracepad.db\" ] || : > \"$5/tracepad.db\"; echo $$ > \"$5/tracepad.db.lock\"; echo 'listening addr=localhost:4318'; exec sleep 30 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "tracepad"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	const oldKey = "  OTEL_EXPORTER_OTLP_HEADERS=\"authorization=Bearer tp-sk-old\"\n  LANGFUSE_PUBLIC_KEY=tp-pk-old\n"
	cases := []struct {
		name, block string
		db          bool // a database there before the start
		foreign     bool // another server makes one and holds it while this one starts
		log         string
		says        string // in the output
		alive       bool
		env         string // in .env
		key         bool   // a key in .env
		fresh       bool   // the data fresh afterwards
	}{
		{"fresh, no key printed", strings.Replace(first, "declare=yes;", "declare=no;", 1), false, false, "", "STOP: no key", false, "", false, true},
		{"fresh, an earlier start's key in the log", strings.Replace(first, "declare=yes;", "declare=no;", 1), false, false, oldKey, "STOP: no key", false, "", false, true},
		{"fresh, a key declared", first, false, false, "", "", true, "TRACEPAD_API_KEY=tp-sk-", true, false},
		{"a database there before, no key", strings.Replace(first, "declare=yes;", "declare=no;", 1), true, false, "", "STOP: no key", false, "", false, false},
		{"fresh, another server's database made meanwhile", first, false, true, "", "another tracepad", false, "", false, false},
	}
	for _, sh := range shells(t) {
		for _, c := range cases {
			t.Run(filepath.Base(sh)+": "+c.name, func(t *testing.T) {
				t.Parallel()
				home, data, dir, pids := t.TempDir(), t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "pid")
				_ = os.WriteFile(filepath.Join(data, "server.log"), []byte(c.log), 0o600)
				files := []string{"tracepad.db", "tracepad.db-wal", "tracepad.db-shm", "tracepad.db.lock"}
				if c.db {
					for _, f := range files {
						_ = os.WriteFile(filepath.Join(data, f), []byte("theirs"), 0o600)
					}
				}
				cmd := exec.Command(sh, "-c", c.block)
				cmd.Dir = dir
				cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home, "TRACEPAD_DATA_DIR=" + data, "PIDS=" + pids}
				if c.foreign {
					cmd.Env = append(cmd.Env, "FOREIGN=1")
				}
				out, _ := cmd.CombinedOutput()
				b, _ := os.ReadFile(pids)
				pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
				if pid > 0 {
					t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGTERM) })
				}
				env, _ := os.ReadFile(filepath.Join(dir, ".env"))
				if alive := running(pid, c.alive); alive != c.alive || !strings.Contains(string(out), c.says) ||
					!strings.Contains(string(env), c.env) || c.key != strings.Contains(string(env), "TRACEPAD_API_KEY=") {
					t.Errorf("the server alive %v, .env %q: %s", alive, env, out)
				}
				// What the setup's own check reads (NOT FRESH), and what was
				// there before kept as it was.
				left, _ := filepath.Glob(filepath.Join(data, "tracepad.db*"))
				if fresh := len(left) == 0; fresh != c.fresh {
					t.Errorf("the data fresh %v afterwards: %q", fresh, left)
				}
				for _, f := range files[:3] {
					if b, err := os.ReadFile(filepath.Join(data, f)); (c.db || c.foreign) && (err != nil || strings.TrimSpace(string(b)) != "theirs") {
						t.Errorf("%s, not this start's, is %q, %v", f, b, err)
					}
				}
			})
		}
	}
}

// running is whether pid runs, not a zombie its parent has not reaped yet:
// one expected gone is polled for briefly before it is called alive.
func running(pid int, expect bool) bool {
	for i := 0; ; i++ {
		out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		state := strings.TrimSpace(string(out))
		alive := pid > 0 && state != "" && !strings.HasPrefix(state, "Z")
		if alive == expect || i == 50 {
			return alive
		}
		time.Sleep(50 * time.Millisecond)
	}
}
