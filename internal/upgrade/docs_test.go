package upgrade

import (
	"os"
	"path/filepath"
	"reflect"
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

// TestTheDocsNameEveryFieldOfTheReport holds cli.md's list of `--json`'s
// fields to the report's (the sixth review found `back_check` missing).
func TestTheDocsNameEveryFieldOfTheReport(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "docs", "cli.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, list, ok := strings.Cut(string(b), "prints the report as one object:")
	if !ok {
		t.Fatal("cli.md no longer lists the report's fields")
	}
	list, _, _ = strings.Cut(list, "\n\n")
	typ := reflect.TypeFor[Report]()
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" && !strings.Contains(list, "`"+name+"`") {
			t.Errorf("cli.md does not name the report's field %q", name)
		}
	}
}
