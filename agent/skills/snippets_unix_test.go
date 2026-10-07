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

// setup.md's starts, run with a stand-in tracepad that serves until it is
// killed and prints no key, as a server on data that was there (the reviews
// of #231). The first start, on fresh data: no key is a STOP that stops the
// server, and a key an earlier start left in the log is never taken for this
// one's. The earlier install's start makes and reads no key: the server runs,
// .env gets its URL, and the key is the human's.
func TestTheSetupsStartsStopOnlyWhatTheyMade(t *testing.T) {
	t.Parallel()
	blocks := map[string]string{}
	for _, b := range shellBlocks(t) {
		if b.file == "references/setup.md" && strings.HasPrefix(b.text, "port=4318; ") {
			blocks[strings.Fields(b.text)[1]] = b.text
		}
	}
	first, earlier := blocks["declare=yes;"], blocks["url=\"http://localhost:$port\";"]
	if first == "" || earlier == "" {
		t.Fatalf("setup.md's two starts not found: %q", blocks)
	}
	bin := t.TempDir()
	stub := "#!/bin/sh\ncase \"$1\" in\nversion) echo 0.1.1 ;;\nhealth) echo '{\"ok\":true}' ;;\nserve) echo $$ > \"$PIDS\"; echo 'listening addr=localhost:4318'; exec sleep 30 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "tracepad"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	const oldKey = "  OTEL_EXPORTER_OTLP_HEADERS=\"authorization=Bearer tp-sk-old\"\n  LANGFUSE_PUBLIC_KEY=tp-pk-old\n"
	for _, c := range []struct {
		name, block string
		log         string
		says        string // in the output
		alive       bool
		env         string // in .env
		key         bool   // a key in .env
	}{
		{"fresh, no key printed", strings.Replace(first, "declare=yes;", "declare=no;", 1), "", "STOP: no key", false, "", false},
		{"fresh, an earlier start's key in the log", strings.Replace(first, "declare=yes;", "declare=no;", 1), oldKey, "STOP: no key", false, "", false},
		{"fresh, a key declared", first, "", "", true, "TRACEPAD_API_KEY=tp-sk-", true},
		{"the earlier install", earlier, oldKey, "KEY: ", true, "TRACEPAD_URL=http://localhost:4318", false},
	} {
		home, data, dir, pids := t.TempDir(), t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "pid")
		_ = os.WriteFile(filepath.Join(data, "server.log"), []byte(c.log), 0o600)
		cmd := exec.Command("sh", "-c", c.block)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home, "TRACEPAD_DATA_DIR=" + data, "PIDS=" + pids}
		out, _ := cmd.CombinedOutput()
		b, _ := os.ReadFile(pids)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if pid > 0 {
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGTERM) })
		}
		env, _ := os.ReadFile(filepath.Join(dir, ".env"))
		if alive := running(pid, c.alive); alive != c.alive || !strings.Contains(string(out), c.says) ||
			!strings.Contains(string(env), c.env) || c.key != strings.Contains(string(env), "TRACEPAD_API_KEY=") {
			t.Errorf("%s: the server alive %v, .env %q: %s", c.name, alive, env, out)
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
