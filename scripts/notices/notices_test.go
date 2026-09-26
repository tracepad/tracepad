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
	// Asked again, apart from the code under test: every module that
	// provides a package of the binary on this platform is in the notices.
	raw, err := exec.Command("go", "list", "-deps", "-tags", "ui", "-e",
		"-f", "{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}{{end}}{{end}}",
		"github.com/tracepad/tracepad/cmd/tracepad").Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, header := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if header != "" && !strings.Contains(text, "\n"+header+"\n") {
			t.Errorf("%s is compiled in and not in the notices", header)
		}
	}
	if len(modules) == 0 || !strings.Contains(text, "The Go distribution") {
		t.Errorf("the notices name %d modules and the distribution %v", len(modules), strings.Contains(text, "The Go distribution"))
	}
}

func TestAModuleWithoutALicenceIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("no licence here"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := goSections([]module{{"example.com/unlicensed", "v1.0.0", dir}})

	if err == nil || !strings.Contains(err.Error(), "example.com/unlicensed v1.0.0: no licence file") {
		t.Fatalf("got %v", err)
	}
}
