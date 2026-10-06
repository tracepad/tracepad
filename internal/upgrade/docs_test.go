package upgrade

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
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

// TestTheSkillReadsTheExitTable holds the skill's reading of each exit status
// to cli.md's one table, and the table to the command's codes (the seventh
// review found the skill reading 4 and 10 as the command never returns
// them): every status the plan returns is in the skill's plan, every one the
// upgrade returns has its own line in the skill's upgrade section.
func TestTheSkillReadsTheExitTable(t *testing.T) {
	read := func(parts ...string) string {
		b, err := os.ReadFile(filepath.Join(append([]string{"..", ".."}, parts...)...))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	cli, skill := read("docs", "cli.md"), read("agent", "skills", "tracepad", "references", "upgrade.md")
	_, table, ok := strings.Cut(cli, "| Exit | `--plan` | the upgrade |")
	if !ok {
		t.Fatal("cli.md has no exit table")
	}
	row := regexp.MustCompile("(?m)^\\| `(\\d+)` \\| ([^|]*) \\| ([^|]*) \\|")
	plan, upgrade, codes := []string{}, []string{}, []string{}
	for _, m := range row.FindAllStringSubmatch(table, -1) {
		codes = append(codes, m[1])
		if strings.TrimSpace(m[2]) != "—" {
			plan = append(plan, m[1])
		}
		if strings.TrimSpace(m[3]) != "—" {
			upgrade = append(upgrade, m[1])
		}
	}
	var want []string
	for _, c := range []int{exitOK, exitRefused, exitUsage, exitWentBack, exitDecide, exitStuck, exitPending} {
		want = append(want, strconv.Itoa(c))
	}
	if strings.Join(codes, " ") != strings.Join(want, " ") {
		t.Fatalf("the table's codes %q, the command's %q", codes, want)
	}
	_, sec1, _ := strings.Cut(skill, "## 1.")
	sec1, sec2, _ := strings.Cut(sec1, "## 2.")
	sec2, _, _ = strings.Cut(sec2, "## 3.")
	for _, c := range plan {
		if c != "2" && !strings.Contains(sec1, "`"+c+"`") {
			t.Errorf("the skill's plan does not read exit %s", c)
		}
	}
	for _, c := range upgrade {
		if c != "2" && !strings.Contains(sec2, "- `"+c+"`") {
			t.Errorf("the skill's upgrade has no line for exit %s", c)
		}
	}
}
