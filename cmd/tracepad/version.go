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

// currentVersion answers where the version is needed rather than rewriting the
// stamped variable at start-up: `version` stays what the linker put there, and
// nothing that runs before `main` can read a half-resolved one.
func currentVersion() string {
	info, _ := debug.ReadBuildInfo()
	return resolveVersion(version, info)
}
