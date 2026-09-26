package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Go half is what the gate can afford: a module the binary gained
// without a licence file fails here, not on the release day. The interface's
// half needs its build and is checked where that runs (e2e, the image).
func TestEveryModuleOfTheBinaryHasItsLicence(t *testing.T) {
	modules, err := goModules("github.com/tracepad/tracepad/cmd/tracepad")
	if err != nil {
		t.Fatal(err)
	}
	text, err := goSections(modules)
	if err != nil {
		t.Fatal(err)
	}
	// Asked again, apart from the code under test and as the release builds:
	// every module that provides a package of the binary is in the notices.
	for _, platform := range platforms {
		goos, goarch, _ := strings.Cut(platform, "/")
		cmd := exec.Command("go", "list", "-deps", "-tags", "ui", "-e",
			"-f", "{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}{{end}}{{end}}",
			"github.com/tracepad/tracepad/cmd/tracepad")
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
		raw, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		for _, header := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if header != "" && !strings.Contains(text, "\n"+header+"\n") {
				t.Errorf("%s: %s is compiled in and not in the notices", platform, header)
			}
		}
	}
	if !strings.Contains(text, "The Go distribution") {
		t.Error("the Go distribution is not in the notices")
	}
	// A licence beside a package, below its module's own: zstd's vendored
	// xxhash is MIT under its own author.
	if !strings.Contains(text, "(zstd/internal/xxhash/LICENSE.txt)\nCopyright (c) 2016 Caleb Spare") {
		t.Error("zstd/internal/xxhash/LICENSE.txt is compiled in and not in the notices")
	}
}

func TestAModuleWithoutALicenceIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("no licence here"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := goSections([]module{{path: "example.com/unlicensed", version: "v1.0.0", dir: dir}})

	if err == nil || !strings.Contains(err.Error(), "example.com/unlicensed v1.0.0: no licence file") {
		t.Fatalf("got %v", err)
	}
}

func TestAPackageThatFailedToLoadIsAnError(t *testing.T) {
	type mod = struct {
		Path, Version, Dir string
		Main               bool
	}
	failed := &struct{ Err string }{"cannot find module providing package example.com/gone"}
	embed := &struct{ Err string }{"pattern all:dist: no matching files found"}
	for _, c := range []struct {
		name string
		p    listed
		ok   bool
	}{
		{"a dependency that did not load", listed{ImportPath: "example.com/gone", Module: &mod{Path: "example.com/gone"}, Error: failed}, false},
		{"a package of no module", listed{ImportPath: "example.com/orphan"}, false},
		{"the unstaged bundle", listed{ImportPath: "github.com/tracepad/tracepad/internal/ui", Module: &mod{Main: true}, Error: embed}, true},
		{"another error of the main module", listed{ImportPath: "github.com/tracepad/tracepad/internal/cli", Module: &mod{Main: true}, Error: failed}, false},
		{"the standard library", listed{ImportPath: "net/http", Standard: true}, true},
	} {
		if err := check(c.p); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}
