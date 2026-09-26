// Command notices writes THIRD_PARTY_NOTICES: the licence of every Go module
// compiled into the server binary, the Go distribution's own, then the npm
// packages of the web interface's bundle, which the interface's build lists
// in `third-party-notices.txt` beside it (spec 020 #17).
//
// It reads only what is on disk — the module cache and the bundle's list —
// so its answer is the same offline and on every run over the same tree. A
// module with no licence file is an error, not a gap in the file.
//
//	go run ./scripts/notices -ui internal/ui/dist/third-party-notices.txt -o THIRD_PARTY_NOTICES
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// licence names a file that carries a licence or the notices it asks for;
// `ui/notices.ts` matches the same names.
var licence = regexp.MustCompile(`(?i)^(licen[cs]e|copying|notice|patents)`)

const rule = "================================================================================"

func main() {
	ui := flag.String("ui", "", "the web interface's list, written by its build (required)")
	out := flag.String("o", "", "where to write; standard output when empty")
	flag.Parse()
	if err := run(*ui, *out); err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
}

func run(uiList, out string) error {
	if uiList == "" {
		return fmt.Errorf("-ui is required: the binary carries the interface, so its notices do too")
	}
	raw, err := os.ReadFile(uiList)
	if err != nil {
		return fmt.Errorf("%w (run `make ui` first)", err)
	}
	// The list's first line is its own count: a licence text may hold a line
	// of '=' too, so the sections are not counted here.
	var packages int
	first, npm, _ := strings.Cut(string(raw), "\n")
	if _, err := fmt.Sscanf(first, "npm packages: %d", &packages); err != nil {
		return fmt.Errorf("%s: the first line is not its count (%q)", uiList, first)
	}
	modules, err := goModules("./cmd/tracepad")
	if err != nil {
		return err
	}
	golang, err := goSections(modules)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "Third-party software in Tracepad\n\n"+
		"The tracepad binary is compiled from the Go modules below and carries a web\n"+
		"interface bundled from the npm packages after them. Each section is the\n"+
		"licence text the module or package ships with. The fonts and icons named in\n"+
		"NOTICE are licensed in third_party/.\n\n"+
		"Go modules: %d, and the Go distribution. npm packages: %d.\n\n",
		len(modules), packages)
	b.WriteString(golang)
	b.WriteString(npm)
	if out == "" {
		_, err = os.Stdout.Write(b.Bytes())
		return err
	}
	return os.WriteFile(out, b.Bytes(), 0o644)
}

// module is one dependency the build compiles in, with the directories of
// the packages it provides: a licence can sit beside a package, below the
// module's own (a vendored hash, say).
type module struct {
	path, version, dir string
	packages           []string
}

// platforms is the release matrix of .goreleaser.yaml: a module one of them
// compiles in (golang.org/x/sys's Windows half, say) is in every archive's
// notices, since the file is one for all of them.
var platforms = []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64"}

// listed is what `go list -json` says of one package.
type listed struct {
	ImportPath string
	Dir        string
	Standard   bool
	Module     *struct {
		Path, Version, Dir string
		Main               bool
	}
	Error *struct{ Err string }
}

// goModules lists the modules that provide a package of the binary on any
// release platform, built as the release builds it (the `ui` tag, no cgo);
// the main module is not one of them.
func goModules(pkg string) ([]module, error) {
	answers := make([][]listed, len(platforms))
	errs := make([]error, len(platforms))
	var wg sync.WaitGroup
	for i, platform := range platforms {
		wg.Go(func() { answers[i], errs[i] = list(pkg, platform) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	byPath := map[string]*module{}
	for _, answer := range answers {
		for _, p := range answer {
			if p.Standard || p.Module.Main {
				continue
			}
			m := byPath[p.Module.Path]
			if m == nil {
				if p.Module.Dir == "" {
					return nil, fmt.Errorf("%s %s is not in the module cache (run `go mod download`)", p.Module.Path, p.Module.Version)
				}
				m = &module{path: p.Module.Path, version: p.Module.Version, dir: p.Module.Dir}
				byPath[p.Module.Path] = m
			}
			if !slices.Contains(m.packages, p.Dir) {
				m.packages = append(m.packages, p.Dir)
			}
		}
	}
	var modules []module
	for _, m := range byPath {
		modules = append(modules, *m)
	}
	slices.SortFunc(modules, func(a, b module) int { return strings.Compare(a.path, b.path) })
	return modules, nil
}

// list is `go list -deps` for one platform. `-e` because the main module's
// `ui` package does not compile until `make ui` has staged the bundle, which
// changes no dependency; every other error is one.
func list(pkg, platform string) ([]listed, error) {
	goos, goarch, _ := strings.Cut(platform, "/")
	cmd := exec.Command("go", "list", "-deps", "-e", "-tags", "ui",
		"-json=ImportPath,Dir,Standard,Module,Error", pkg)
	cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	cmd.Stderr = os.Stderr
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list (%s): %w", platform, err)
	}
	var packages []listed
	for decoder := json.NewDecoder(bytes.NewReader(raw)); ; {
		var p listed
		if err := decoder.Decode(&p); err == io.EOF {
			return packages, nil
		} else if err != nil {
			return nil, fmt.Errorf("go list (%s): %w", platform, err)
		}
		if err := check(p); err != nil {
			return nil, fmt.Errorf("go list (%s): %w", platform, err)
		}
		packages = append(packages, p)
	}
}

// check refuses a package whose module is unknown or that failed to load:
// either would leave a module out of the notices without a word.
func check(p listed) error {
	switch {
	case p.Error != nil && p.Module != nil && p.Module.Main &&
		strings.HasSuffix(p.ImportPath, "/internal/ui") && strings.Contains(p.Error.Err, "all:dist"):
		return nil // the bundle is not staged; see list
	case p.Error != nil:
		return fmt.Errorf("%s: %s", p.ImportPath, p.Error.Err)
	case !p.Standard && p.Module == nil:
		return fmt.Errorf("%s belongs to no module", p.ImportPath)
	}
	return nil
}

// goSections is the Go distribution's section, then one per module.
func goSections(modules []module) (string, error) {
	raw, err := exec.Command("go", "env", "-json", "GOROOT", "GOVERSION").Output()
	if err != nil {
		return "", fmt.Errorf("go env: %w", err)
	}
	var env struct{ GOROOT, GOVERSION string }
	if err := json.Unmarshal(raw, &env); err != nil || env.GOROOT == "" {
		return "", fmt.Errorf("go env: %q: %v", raw, err)
	}
	all := append([]module{{path: "The Go distribution (runtime and standard library)", version: env.GOVERSION, dir: env.GOROOT}}, modules...)
	var b strings.Builder
	for _, m := range all {
		text, err := licences(m)
		if err != nil {
			return "", fmt.Errorf("%s %s: %w", m.path, m.version, err)
		}
		fmt.Fprintf(&b, "%s\n%s %s\n%s\n%s\n\n", rule, m.path, m.version, strings.Repeat("-", len(rule)), text)
	}
	return b.String(), nil
}

// licences is every licence file of a module: at its root, then in each
// directory between the root and a package it provides, by path. A file
// below the root is headed with where it sits.
func licences(m module) (string, error) {
	dirs := []string{m.dir}
	for _, pkg := range m.packages {
		for dir := pkg; dir != m.dir && strings.HasPrefix(dir, m.dir+string(filepath.Separator)); dir = filepath.Dir(dir) {
			if !slices.Contains(dirs, dir) {
				dirs = append(dirs, dir)
			}
		}
	}
	slices.Sort(dirs[1:])
	var texts []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return "", err
		}
		for _, e := range entries {
			if !e.Type().IsRegular() || !licence.MatchString(e.Name()) {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return "", err
			}
			text := strings.TrimSpace(string(raw))
			if dir != m.dir {
				rel, _ := filepath.Rel(m.dir, filepath.Join(dir, e.Name()))
				text = "(" + filepath.ToSlash(rel) + ")\n" + text
			}
			texts = append(texts, text)
		}
	}
	if len(texts) == 0 {
		return "", fmt.Errorf("no licence file in %s", m.dir)
	}
	return strings.Join(texts, "\n\n"), nil
}
