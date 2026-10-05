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
	for _, c := range []struct {
		version, commit, want string
	}{
		{"0.1.0-rc.1", "b14b11e", "tracepad 0.1.0-rc.1 (b14b11e)"},
		{"0.1.0", "b14b11e2a9c0d4f1e8a7b6c5d4e3f2a1b0c9d8e7", "tracepad 0.1.0 (b14b11e)"},
		{"0.1.0", "b14b", "tracepad 0.1.0 (b14b)"},
		{"dev", "", "tracepad dev"},
		{"dev", "b14b11e", "tracepad dev (b14b11e)"},
	} {
		if got := buildLabel(c.version, c.commit); got != c.want {
			t.Errorf("buildLabel(%q, %q) = %q, want %q", c.version, c.commit, got, c.want)
		}
	}
}
