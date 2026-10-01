package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/config"
)

// The environment the server reads is written down in three places that have
// nothing but this test to keep them in step: the code (config.KnownEnv, which
// is also what the start-up typo warning checks against), `tracepad help`, and
// docs/configuration.md. A variable added to the first and left out of the
// others is a variable nobody finds (spec 001 #24).

// notInHelp: the image sets this one; a person has no reason to.
var notInHelp = []string{"TRACEPAD_IN_CONTAINER"}

// packageEnv is read by the Python, Node and Go packages and never by the
// server, so the server's list does not have it and the documentation does.
var packageEnv = []string{
	"TRACEPAD_ENVIRONMENT", "TRACEPAD_RELEASE", "TRACEPAD_EXPORT_TIMEOUT",
}

var envName = regexp.MustCompile(`TRACEPAD_[A-Z][A-Z_]*[A-Z]`)

// tableVars returns the variable named in the first cell of each table row of
// a Markdown file, keyed by the file's line number.
func tableVars(t *testing.T, path string) map[int]string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	found := map[int]string{}
	for i, line := range strings.Split(string(body), "\n") {
		cell, ok := strings.CutPrefix(line, "| `")
		if !ok {
			continue
		}
		if name := envName.FindString(cell); name != "" && strings.HasPrefix(cell, name+"`") {
			found[i+1] = name
		}
	}
	return found
}

func TestHelpNamesEveryServerVariable(t *testing.T) {
	known := config.KnownEnv()
	for _, name := range known {
		if slices.Contains(notInHelp, name) {
			continue
		}
		if !strings.Contains(helpText, name) {
			t.Errorf("`tracepad help` does not mention %s", name)
		}
	}
	for _, name := range envName.FindAllString(helpText, -1) {
		if !slices.Contains(known, name) {
			t.Errorf("`tracepad help` names %s, which the server does not read", name)
		}
	}
}

func TestConfigurationDocListsEveryVariable(t *testing.T) {
	known := config.KnownEnv()
	listed := map[string]bool{}
	for _, name := range tableVars(t, "../../docs/configuration.md") {
		listed[name] = true
		if !slices.Contains(known, name) && !slices.Contains(packageEnv, name) {
			t.Errorf("docs/configuration.md lists %s, which neither the server nor the packages read", name)
		}
	}
	for _, name := range append(slices.Clone(known), packageEnv...) {
		if !listed[name] {
			t.Errorf("docs/configuration.md has no row for %s", name)
		}
	}
}

// TestDocTablesNameRealVariables: the topic pages keep a table of the
// variables that bear on them, with the detail of that topic. A name in one of
// those that nothing reads is a typo or a variable that was renamed.
func TestDocTablesNameRealVariables(t *testing.T) {
	known := config.KnownEnv()
	files, err := os.ReadDir("../../docs")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".md") {
			continue
		}
		for line, name := range tableVars(t, "../../docs/"+f.Name()) {
			if !slices.Contains(known, name) && !slices.Contains(packageEnv, name) {
				t.Errorf("docs/%s:%d names %s, which nothing reads", f.Name(), line, name)
			}
		}
	}
}
