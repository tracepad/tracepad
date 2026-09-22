// Package skills carries the agent skill that teaches a coding agent how to
// work with Tracepad, and the `tracepad skills` command that installs it
// (spec 037).
//
// The files under `tracepad/` are the source and the thing shipped: the binary
// embeds them as they are, so the skill an agent reads is always the one of
// the binary that installed it (#5). This file lives beside the directory
// rather than under `internal/` because `go:embed` cannot reach a parent.
package skills

import (
	"embed"
	"io/fs"
)

// Name is the skill's name and the directory it is installed as.
const Name = "tracepad"

//go:embed all:tracepad
var embedded embed.FS

// Files is the skill as the binary carries it, rooted at the skill's own
// directory: `SKILL.md`, `references/…`.
func Files() fs.FS {
	sub, err := fs.Sub(embedded, Name)
	if err != nil {
		// fs.Sub fails only on an invalid path, and Name is a constant.
		panic(err)
	}
	return sub
}
