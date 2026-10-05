package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/config"
)

// The environment the binaries and the packages read is written down in
// config.Env, once. What is derived from it (the start-up warning, the variable
// list of `tracepad help`) cannot disagree with it; what is written by hand
// (docs/configuration.md, the pages that name a variable, the packages' own
// sources) is held to it here (spec 001 #24).

var envName = regexp.MustCompile(`TRACEPAD_[A-Z][A-Z_]*[A-Z]`)

// names returns the variable names in text, each once.
func names(text string) []string {
	return slices.Compact(slices.Sorted(slices.Values(envName.FindAllString(text, -1))))
}

func ofKind(kinds ...config.EnvKind) []string {
	var out []string
	for _, v := range config.Env {
		if slices.Contains(kinds, v.Kind) {
			out = append(out, v.Name)
		}
	}
	slices.Sort(out)
	return out
}

func known(name string) bool {
	return config.IsKnownEnv(name) || config.IsDeprecatedEnv(name)
}

func read(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// markdown lists the Markdown files under dir.
func markdown(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".md") {
			files = append(files, path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// cells returns, for each line of a Markdown table, the cells of it.
func rows(text string) map[int][]string {
	out := map[int][]string{}
	for i, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "|") {
			out[i+1] = strings.Split(line, "|")[1:]
		}
	}
	return out
}

// TestHelpNamesExactlyTheVariablesAPersonSets: every variable of the server and
// of the CLI is in `tracepad help`, and nothing else is. Whole names, not
// substrings: TRACEPAD_ADMIN_TOKEN is a prefix of TRACEPAD_ADMIN_TOKEN_FILE.
func TestHelpNamesExactlyTheVariablesAPersonSets(t *testing.T) {
	want := ofKind(config.EnvServer, config.EnvClient)
	got := names(helpText())
	for _, name := range want {
		if !slices.Contains(got, name) {
			t.Errorf("`tracepad help` does not mention %s", name)
		}
	}
	for _, name := range got {
		if !slices.Contains(want, name) {
			t.Errorf("`tracepad help` names %s, which is not a variable a person sets on the server or the CLI", name)
		}
	}
}

// TestConfigurationPageHasARowForEveryVariable, and for nothing else.
func TestConfigurationPageHasARowForEveryVariable(t *testing.T) {
	listed := map[string]bool{}
	for line, cells := range rows(read(t, "../../docs/configuration.md")) {
		for _, name := range names(cells[0]) {
			listed[name] = true
			if !config.IsKnownEnv(name) {
				t.Errorf("docs/configuration.md:%d has a row for %s, which nothing reads", line, name)
			}
		}
	}
	for _, v := range config.Env {
		if !listed[v.Name] {
			t.Errorf("docs/configuration.md has no row for %s", v.Name)
		}
	}
}

// TestPagesNameRealVariables: a name in a table cell of any page of the
// documentation or of the README that nothing reads is a typo, or a variable
// that was renamed. The topic pages keep a table of the variables that bear on
// them — the detail of that topic is why they are read — so the cells are
// where a stale one hides.
func TestPagesNameRealVariables(t *testing.T) {
	files := append(markdown(t, "../../docs"), "../../README.md")
	for _, path := range files {
		for line, cells := range rows(read(t, path)) {
			for _, name := range names(strings.Join(cells, "|")) {
				if !known(name) {
					t.Errorf("%s:%d names %s, which nothing reads", strings.TrimPrefix(path, "../../"), line, name)
				}
			}
		}
	}
}

// TestPackageREADMEsNameOnlyWhatPackagesRead: the packages' READMEs are the
// packages' own, and a server-only variable in one is a recipe that does
// nothing.
func TestPackageREADMEsNameOnlyWhatPackagesRead(t *testing.T) {
	allowed := append(ofKind(config.EnvPackage), "TRACEPAD_URL", "TRACEPAD_API_KEY")
	for _, path := range []string{"python", "js", "go"} {
		path = "../../sdk/" + path + "/README.md"
		for _, name := range names(read(t, path)) {
			if !slices.Contains(allowed, name) && !config.IsDeprecatedEnv(name) {
				t.Errorf("%s names %s, which the packages do not read", strings.TrimPrefix(path, "../../"), name)
			}
		}
	}
}

// TestEveryPackageReadsTheVariablesItIsDocumentedFor: the packages' variables
// are read by all three, and so are the two they share with the CLI. The
// sources are searched, tests and vendored code excluded; a name that no
// longer appears there is a variable the documentation still promises.
func TestEveryPackageReadsTheVariablesItIsDocumentedFor(t *testing.T) {
	packages := map[string][]string{
		"python": {"../../sdk/python/src"},
		"js":     {"../../sdk/js/src"},
		"go":     {"../../sdk/go"},
	}
	for pkg, roots := range packages {
		var source strings.Builder
		for _, root := range roots {
			err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					if d.Name() == "node_modules" || d.Name() == "testdata" {
						return fs.SkipDir
					}
					return nil
				}
				switch filepath.Ext(path) {
				case ".py", ".ts", ".go":
					if !strings.HasSuffix(path, "_test.go") {
						source.WriteString(read(t, path))
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		if source.Len() == 0 {
			t.Fatalf("no source found for the %s package", pkg)
		}
		got := names(source.String())
		for _, name := range append(ofKind(config.EnvPackage), "TRACEPAD_URL", "TRACEPAD_API_KEY") {
			if !slices.Contains(got, name) {
				t.Errorf("the %s package does not read %s, which config.Env says it does", pkg, name)
			}
		}
	}
}

// TestInstallScriptReadsExactlyItsVariables: scripts/install.sh reads the
// installer's variables and no other TRACEPAD_* name, so a variable it gains
// or drops is a change to config.Env and to docs/configuration.md as well
// (spec 053 #10).
func TestInstallScriptReadsExactlyItsVariables(t *testing.T) {
	want := ofKind(config.EnvInstaller)
	got := names(read(t, "../../scripts/install.sh"))
	if !slices.Equal(got, want) {
		t.Errorf("scripts/install.sh names %v; config.Env's installer variables are %v", got, want)
	}
}
