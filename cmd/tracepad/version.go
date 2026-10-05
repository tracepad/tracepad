package main

import (
	"regexp"
	"runtime/debug"
	"strings"
)

// canonicalVersion is the one spelling a release has, `release-tag.sh`'s own
// without the `v`: three numbers without leading zeros, and a pre-release only
// as -alpha.N, -beta.N or -rc.N. Anything else Go puts in a build's version
// (`0.0.0-20261002120000-abcdef123456` for `go install …@main`, `+dirty`, a
// `+incompatible`) is not a release and must not be reported as one: it would
// sort below every real version in whatever compares them.
var canonicalVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|beta|rc)\.(0|[1-9][0-9]*))?$`)

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
	if v := strings.TrimPrefix(info.Main.Version, "v"); canonicalVersion.MatchString(v) {
		return v
	}
	return "dev"
}

// shortCommitLen is how much of a commit the start's first line shows: the
// length `git rev-parse --short` settles on for a repository this size.
const shortCommitLen = 7

// buildLabel is the first line a server writes: `tracepad 0.1.0-rc.1 (b14b11e)`,
// or `tracepad dev` when the build was given no commit. The commit is whatever
// the release build stamped (`-X main.commit=`), cut to its short form; a build
// that stamped none says only the version, because a build context carries no
// commit and only the workflow that has one should claim it (spec 020 #12).
func buildLabel(version, commit string) string {
	label := "tracepad " + version
	if len(commit) > shortCommitLen {
		commit = commit[:shortCommitLen]
	}
	if commit != "" {
		label += " (" + commit + ")"
	}
	return label
}

// currentVersion answers where the version is needed rather than rewriting the
// stamped variable at start-up: `version` stays what the linker put there, and
// nothing that runs before `main` can read a half-resolved one.
func currentVersion() string {
	info, _ := debug.ReadBuildInfo()
	return resolveVersion(version, info)
}
