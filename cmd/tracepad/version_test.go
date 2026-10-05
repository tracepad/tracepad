package main

import (
	"runtime/debug"
	"testing"
)

func TestResolveVersion(t *testing.T) {
	module := func(v string, settings ...debug.BuildSetting) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Version: v}, Settings: settings}
	}
	vcs := debug.BuildSetting{Key: "vcs.revision", Value: "712c47b"}
	cgo := debug.BuildSetting{Key: "CGO_ENABLED", Value: "0"}

	for _, c := range []struct {
		name    string
		stamped string
		info    *debug.BuildInfo
		want    string
	}{
		{"a release build's stamp wins", "0.1.0", module("v9.9.9"), "0.1.0"},
		{"make's own dev stays dev", "dev", module("v9.9.9"), "dev"},
		{"go install pkg@version reports the tag without its v", "", module("v0.1.0", cgo), "0.1.0"},
		{"a pre-release keeps its suffix", "", module("v0.1.0-rc.1"), "0.1.0-rc.1"},
		{"a checkout's pseudo-version is not a version", "", module("v0.0.0-20261002-712c47b+dirty", vcs, cgo), "dev"},
		{"go install @main is a pseudo-version with no vcs stamp", "", module("v0.0.0-20261002120000-abcdef123456", cgo), "dev"},
		{"a pseudo-version after a tag is not that tag", "", module("v0.1.1-0.20261002120000-abcdef123456"), "dev"},
		{"a leading zero is not a release", "", module("v0.01.0"), "dev"},
		{"build metadata is not a release", "", module("v1.0.0+incompatible"), "dev"},
		{"a pre-release outside the canon is not a release", "", module("v0.1.0-rc1"), "dev"},
		{"a checkout with no tag says (devel)", "", module("(devel)", cgo), "dev"},
		{"no build info", "", nil, "dev"},
		{"no version in the info", "", module(""), "dev"},
	} {
		if got := resolveVersion(c.stamped, c.info); got != c.want {
			t.Errorf("%s: resolveVersion(%q) = %q, want %q", c.name, c.stamped, got, c.want)
		}
	}
}

func TestBuildLabel(t *testing.T) {
	built := func(settings ...debug.BuildSetting) *debug.BuildInfo { return &debug.BuildInfo{Settings: settings} }
	rev := debug.BuildSetting{Key: "vcs.revision", Value: "712c47b9a0e1d2c3b4a5968778695a4b3c2d1e0f"}
	dirty := debug.BuildSetting{Key: "vcs.modified", Value: "true"}
	clean := debug.BuildSetting{Key: "vcs.modified", Value: "false"}

	for _, c := range []struct {
		name         string
		ver, stamped string
		info         *debug.BuildInfo
		want         string
	}{
		{"a release is its version and its stamp", "0.1.0-rc.1", "b14b11e", nil, "tracepad 0.1.0-rc.1 (b14b11e)"},
		{"a long stamp is cut", "0.1.0", "b14b11e2a9c0d4f1e8a7b6c5d4e3f2a1b0c9d8e7", nil, "tracepad 0.1.0 (b14b11e)"},
		{"a short stamp is kept", "0.1.0", "b14b", nil, "tracepad 0.1.0 (b14b)"},
		{"a dev build with nothing says dev", "dev", "", nil, "tracepad dev"},
		{"a stamp on a dev build", "dev", "b14b11e", built(rev), "tracepad dev (b14b11e)"},
		{"a dev build made in a checkout says its revision", "dev", "", built(rev, clean), "tracepad dev (712c47b)"},
		{"and says when the tree was not clean", "dev", "", built(rev, dirty), "tracepad dev (712c47b, dirty)"},
		{"a release whose stamp was dropped does not borrow the checkout's", "0.1.0", "", built(rev, clean), "tracepad 0.1.0"},
	} {
		if got := buildLabel(c.ver, c.stamped, c.info); got != c.want {
			t.Errorf("%s: buildLabel(%q, %q) = %q, want %q", c.name, c.ver, c.stamped, got, c.want)
		}
	}
}
