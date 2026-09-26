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
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// licence names a file that carries a licence or the notices it asks for.
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
	npm, err := os.ReadFile(uiList)
	if err != nil {
		return fmt.Errorf("%w (run `make ui` first)", err)
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
		len(modules), strings.Count(string(npm), rule+"\n"))
	b.WriteString(golang)
	b.WriteString("\n")
	b.Write(npm)
	if out == "" {
		_, err = os.Stdout.Write(b.Bytes())
		return err
	}
	return os.WriteFile(out, b.Bytes(), 0o644)
}

// module is one dependency the build compiles in.
type module struct{ path, version, dir string }

// platforms is the release matrix of .goreleaser.yaml: a module one of them
// compiles in (golang.org/x/sys's Windows half, say) is in every archive's
// notices, since the file is one for all of them.
var platforms = []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64"}

// goModules lists the modules that provide a package of the binary on any
// release platform, built as the release builds it (the `ui` tag, no cgo);
// the main module is not one of them.
func goModules(pkg string) ([]module, error) {
	var raw []byte
	for _, platform := range platforms {
		goos, goarch, _ := strings.Cut(platform, "/")
		cmd := exec.Command("go", "list", "-deps", "-e", "-tags", "ui",
			"-f", "{{with .Module}}{{if not .Main}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{end}}{{end}}", pkg)
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list (%s): %w", platform, err)
		}
		raw = append(raw, out...)
	}
	var modules []module
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			continue
		}
		m := module{fields[0], fields[1], fields[2]}
		if m.dir == "" {
			return nil, fmt.Errorf("%s %s is not in the module cache (run `go mod download`)", m.path, m.version)
		}
		if !slices.Contains(modules, m) {
			modules = append(modules, m)
		}
	}
	slices.SortFunc(modules, func(a, b module) int { return strings.Compare(a.path, b.path) })
	return modules, nil
}

// goSections is the Go distribution's section, then one per module.
func goSections(modules []module) (string, error) {
	root, err := exec.Command("go", "env", "GOROOT", "GOVERSION").Output()
	if err != nil {
		return "", fmt.Errorf("go env: %w", err)
	}
	env := strings.Fields(string(root))
	all := append([]module{{"The Go distribution (runtime and standard library)", env[1], env[0]}}, modules...)
	var b strings.Builder
	for _, m := range all {
		text, err := licences(m.dir)
		if err != nil {
			return "", fmt.Errorf("%s %s: %w", m.path, m.version, err)
		}
		fmt.Fprintf(&b, "%s\n%s %s\n%s\n%s\n\n", rule, m.path, m.version, strings.Repeat("-", len(rule)), text)
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// licences is every licence file at the top of dir, in name order.
func licences(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var texts []string
	for _, e := range entries {
		if e.Type().IsRegular() && licence.MatchString(e.Name()) {
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return "", err
			}
			texts = append(texts, strings.TrimSpace(string(raw)))
		}
	}
	if len(texts) == 0 {
		return "", fmt.Errorf("no licence file in %s", dir)
	}
	return strings.Join(texts, "\n\n"), nil
}
