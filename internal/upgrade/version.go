package upgrade

import (
	"regexp"
	"strconv"
	"strings"
)

// releaseVersion is the one spelling a release has, `release-tag.sh`'s own
// without the `v`: three numbers without leading zeros, and a pre-release only
// as -alpha.N, -beta.N or -rc.N.
var releaseVersion = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|beta|rc)\.(0|[1-9][0-9]*))?$`)

// IsRelease reports whether v is a release's version. Anything else a binary
// can report (`dev`, a pseudo-version) has no place in the order between
// releases.
func IsRelease(v string) bool { return releaseVersion.MatchString(v) }

// version is a release's version taken apart for comparing.
type version struct {
	nums [3]int
	// pre is 0 for a release, and 1, 2, 3 for alpha, beta, rc: a release is
	// later than its candidates.
	pre, preN int
}

var preRank = map[string]int{"alpha": 1, "beta": 2, "rc": 3}

func parseVersion(v string) (version, bool) {
	m := releaseVersion.FindStringSubmatch(v)
	if m == nil {
		return version{}, false
	}
	// A number too large to parse is no release's (the audit of #223), never 0.
	var out version
	var err error
	for i := range 3 {
		if out.nums[i], err = strconv.Atoi(m[i+1]); err != nil {
			return version{}, false
		}
	}
	if m[5] != "" {
		out.pre = preRank[m[5]]
		if out.preN, err = strconv.Atoi(m[6]); err != nil {
			return version{}, false
		}
	}
	return out, true
}

// Compare orders two releases by semver: -1 when a is earlier than b, 0 when
// they are the same, 1 when a is later. ok is false when either is not a
// release's version, and then the order says nothing.
func Compare(a, b string) (order int, ok bool) {
	x, okA := parseVersion(a)
	y, okB := parseVersion(b)
	if !okA || !okB {
		return 0, false
	}
	for i := range 3 {
		if x.nums[i] != y.nums[i] {
			return sign(x.nums[i] - y.nums[i]), true
		}
	}
	switch {
	case x.pre == y.pre:
		return sign(x.preN - y.preN), true
	case x.pre == 0:
		return 1, true
	case y.pre == 0:
		return -1, true
	}
	return sign(x.pre - y.pre), true
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// normalizeVersion takes `--to` as a person writes it: with or without the
// tag's `v`.
func normalizeVersion(v string) string { return strings.TrimPrefix(strings.TrimSpace(v), "v") }
