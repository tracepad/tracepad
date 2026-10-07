// Package skillmark is spec 037 #16's rule for an installed copy of the
// skill: its marker and a SKILL.md naming it. A package of its own so that
// `skills install` and `tracepad upgrade` read a copy by the one rule
// (spec 054 #64), which neither could import from the other's package.
package skillmark

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Name is the skill's name, which its SKILL.md says and it is installed as.
const Name = "tracepad"

// File is the marker that says a directory is an installed copy of the
// skill, and which version it is (spec 037 #6, #7).
const File = ".version"

// Read reports whether dir is an installed copy of the skill, and the version
// its marker names ("" when the marker is empty). It takes both halves (#16):
// SKILL.md naming this skill, and the marker. The marker alone is one file
// with a generic name, which any repository can commit — and a link followed
// on its word alone replaced whatever held one.
func Read(dir string) (bool, string, error) {
	stamp, err := os.ReadFile(filepath.Join(dir, File))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, "", nil
	case err != nil:
		// A marker that cannot be read is not the same as no marker,
		// and calling it "somebody else's directory" would hide why.
		return false, "", fmt.Errorf("cannot read %s: %w", filepath.Join(dir, File), err)
	}
	skill, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, "", nil
	case err != nil:
		// The same rule as the marker's: unreadable is not absent, and a
		// real install that cannot be read must not be replaced as
		// somebody else's directory.
		return false, "", fmt.Errorf("cannot read %s: %w", filepath.Join(dir, "SKILL.md"), err)
	case !namesThisSkill(skill):
		return false, "", nil
	}
	return true, strings.TrimSpace(string(stamp)), nil
}

// namesThisSkill reports whether a SKILL.md's frontmatter says `name:
// tracepad`, the line every install writes.
func namesThisSkill(skill []byte) bool {
	lines := strings.Split(string(skill), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return false
	}
	for _, line := range lines[1:] {
		line = strings.TrimRight(line, "\r")
		if line == "---" {
			return false
		}
		if value, found := strings.CutPrefix(line, "name:"); found {
			return strings.Trim(strings.TrimSpace(value), `"'`) == Name
		}
	}
	return false
}
