package main

import (
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/termsafe"
)

// The install script prints the command lines of other processes, and it has
// no Go to call: its `termsafe` is internal/termsafe's rule written again in
// awk (spec 053 #21 (j)). This holds the two to the same answers — termsafe's
// own cases, the ones a terminal acts on, and random bytes — run through the
// function as the script defines it, under the `sh` and `awk` of this machine
// (dash and mawk on CI).
func TestInstallScriptEscapesAsTermsafeDoes(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "scripts", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	fn := regexp.MustCompile(`(?ms)^termsafe\(\) \{\n.*?^\}\n`).Find(src)
	if fn == nil {
		t.Fatal("scripts/install.sh defines no termsafe() { … }")
	}

	lines := []string{
		"", "support-chat", "héllo 日本語 ✓",
		"\x1b]52;c;ZWNobyBwd25lZA==\x07", "\x1b[2J\x1b[H", "a\tb\rd", "\x7f",
		"\u0080\u009b\u009f ", "‪a‮b⁦c⁩", "‎‏",
		"ok\xffok\xc2", "\xc0\xaf", "\xe0\x80\x80", "\xed\xa0\x80", "\xf4\x90\x80\x80",
		"\xf0\x9f\x98\x80", "\xe2\x80", "\xe2\x80A", "tracepad serve --data-dir /d\u009b2J",
	}
	// Random bytes, weighted toward the ones that matter: controls, the lead
	// and continuation bytes of UTF-8, and the bytes of U+009B and U+202E. A
	// line holds neither NUL, which no awk reads, nor a newline, which ends it.
	pool := []byte("ab ~\x01\x1b\x7f\x80\x9b\xaa\xae\xbf\xc0\xc2\xe2\x81\xa6\xed\xf0\xf4\xf5\xff")
	r := rand.New(rand.NewPCG(53, 21))
	for range 2000 {
		b := make([]byte, 1+r.IntN(12))
		for i := range b {
			b[i] = pool[r.IntN(len(pool))]
		}
		lines = append(lines, string(b))
	}

	cmd := exec.Command("sh", "-c", string(fn)+"termsafe")
	cmd.Stdin = strings.NewReader(strings.Join(lines, "\n") + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("termsafe in sh: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(got) != len(lines) {
		t.Fatalf("%d lines in, %d out", len(lines), len(got))
	}
	for i, line := range lines {
		if want := termsafe.String(line); got[i] != want {
			t.Errorf("termsafe(%q) = %q, termsafe.String = %q", line, got[i], want)
		}
	}
}
