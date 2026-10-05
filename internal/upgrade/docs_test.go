package upgrade

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// upgradeLine is an invocation of the command in prose or in a block: what
// follows `tracepad upgrade` up to the end of the code span or the line.
var upgradeLine = regexp.MustCompile("tracepad\"? upgrade([^`\\n]*)")

// TestTheDocsNameOnlyTheCommandsFlags holds every `tracepad upgrade …` in the
// docs, the README and the skill to the flags the command has (spec 054 #16):
// an agent runs them as written.
func TestTheDocsNameOnlyTheCommandsFlags(t *testing.T) {
	root := filepath.Join("..", "..")
	var files []string
	for _, pattern := range []string{"docs/*.md", "README.md", "agent/skills/tracepad/*.md", "agent/skills/tracepad/references/*.md"} {
		matches, err := filepath.Glob(filepath.Join(root, pattern))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, matches...)
	}
	set := FlagSet()
	seen := 0
	for _, file := range files {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range upgradeLine.FindAllStringSubmatch(string(b), -1) {
			seen++
			for _, word := range strings.Fields(m[1]) {
				word = strings.Trim(word, "[]|()")
				name, ok := strings.CutPrefix(word, "--")
				if !ok || name == "" {
					continue
				}
				name, _, _ = strings.Cut(name, "=")
				if set.Lookup(name) == nil {
					t.Errorf("%s: `tracepad upgrade%s` names --%s, which the command does not have", file, m[1], name)
				}
			}
		}
	}
	if seen < 10 {
		t.Fatalf("found %d invocations; the check would pass anything", seen)
	}
}
