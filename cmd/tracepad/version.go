package main

import (
	"runtime/debug"
	"strings"

	"github.com/tracepad/tracepad/internal/upgrade"
)

// canonicalVersion is the one spelling a release has (spec 054 keeps the rule
// in internal/upgrade, where the order between versions lives too). Anything
// else Go puts in a build's version (`0.0.0-20261002120000-abcdef123456` for
// `go install …@main`, `+dirty`, a `+incompatible`) is not a release and must
// not be reported as one: it would sort below every real version in whatever
// compares them.
var canonicalVersion = upgrade.IsRelease

// resolveVersion is what `tracepad version` and the server report. A stamped
// version (the release build's `-X main.version=`, `make`'s `dev`) is taken as
// it is. An unstamped binary reports the module's version, without its `v`,
// when `go install pkg@vX.Y.Z` built it from the module cache at a real tag —
// the build info carries that tag and no VCS stamp — and `dev` in every other
// case: a checkout (Go writes a pseudo-version there too, and the `vcs.*`
// settings say it is one), `go install pkg@main`, or no build info at all.
func resolveVersion(stamped string, info *debug.BuildInfo) string {
	if stamped != "" {
		return stamped
	}
	if info == nil {
		return "dev"
	}
	for _, s := range info.Settings {
		if strings.HasPrefix(s.Key, "vcs.") {
			return "dev"
		}
	}
	if v := strings.TrimPrefix(info.Main.Version, "v"); canonicalVersion(v) {
		return v
	}
	return "dev"
}

// shortCommitLen is how much of a commit the start's first line shows: the
// length `git rev-parse --short` settles on for a repository this size.
const shortCommitLen = 7

// buildLabel is the first line a server writes: `tracepad 0.1.0-rc.1 (b14b11e)`,
// or `tracepad dev` when nothing says which commit the build is. The commit is
// what the release build stamped (`-X main.commit=`), cut to its short form.
// A `dev` build without a stamp says the revision Go itself recorded for it
// (`vcs.revision`, with `, dirty` when the tree had changes), which is what a
// local `go build` or `make build` has. A release never falls back to it: an
// archive is built in a checkout and would keep saying a commit after the
// stamp was dropped, which is the mistake the archive check exists to catch;
// and an image build has no checkout at all (spec 020 #12).
func buildLabel(ver, stamped string, info *debug.BuildInfo) string {
	rev, dirty := stamped, false
	if rev == "" && ver == "dev" && info != nil {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	label := "tracepad " + ver
	if len(rev) > shortCommitLen {
		rev = rev[:shortCommitLen]
	}
	switch {
	case rev == "":
	case dirty:
		label += " (" + rev + ", dirty)"
	default:
		label += " (" + rev + ")"
	}
	return label
}

// label is buildLabel for this binary.
func label() string {
	info, _ := debug.ReadBuildInfo()
	return buildLabel(resolveVersion(version, info), commit, info)
}

// currentVersion answers where the version is needed rather than rewriting the
// stamped variable at start-up: `version` stays what the linker put there, and
// nothing that runs before `main` can read a half-resolved one.
func currentVersion() string {
	info, _ := debug.ReadBuildInfo()
	return resolveVersion(version, info)
}
