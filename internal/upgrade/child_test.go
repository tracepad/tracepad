package upgrade

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryChildGoesThroughOneDoor: every process the command starts goes
// through child (its own process group) or startDetached (a session of its
// own); a direct exec.Command anywhere else lets a terminal's Ctrl-C reach it
// (spec 054 #26).
func TestEveryChildGoesThroughOneDoor(t *testing.T) {
	t.Parallel()
	direct := regexp.MustCompile(`exec\.Command(Context)?\(|&exec\.Cmd\{|\bos\.StartProcess\(|syscall\.ForkExec\(`)
	allowed := map[string]bool{"child.go": true, "procs_unix.go": true}
	files, _ := filepath.Glob("*.go")
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if direct.MatchString(line) {
				seen++
				if !allowed[f] {
					t.Errorf("%s:%d starts a process past child(): %s", f, i+1, strings.TrimSpace(line))
				}
			}
		}
	}
	if seen < 2 {
		t.Fatalf("found %d process starts; the check would pass anything", seen)
	}
}
