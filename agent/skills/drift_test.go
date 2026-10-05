package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tracepad/tracepad/internal/cli"
	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/mcpserver"
	"github.com/tracepad/tracepad/internal/server"
	"github.com/tracepad/tracepad/internal/store"
	"github.com/tracepad/tracepad/internal/storetest"
)

// The drift test (spec 037 #8). A skill that tells an agent to run a command
// that does not exist is worse than no skill: the agent trusts it, fails, and
// improvises. So every name the skill uses is asked of the binary itself — the
// CLI is run, the MCP server is listed, the route map is fetched — and a
// rename anywhere turns red in the PR that made it.
//
// It checks names, never meaning: that `traces rm` still deletes what the
// skill says it deletes is the author's rule in AGENTS.md, not this file's.

// The line budgets of #1: the loaded part is paid for in every conversation
// the skill triggers in.
var shipped = budgets{skill: 200, total: 900}

type budgets struct{ skill, total int }

// TestTheSkillNamesOnlyWhatTheBinaryHas is the rule, over the files the binary
// embeds — the ones an agent will read.
func TestTheSkillNamesOnlyWhatTheBinaryHas(t *testing.T) {
	surface := askTheBinary(t)
	for _, problem := range check(Files(), surface, shipped) {
		t.Error(problem)
	}
}

// TestHeadingSlugsFollowTheDocAnchorsRule: the absolute links the drift test
// checks land on GitHub, so their anchors have to be GitHub's.
func TestHeadingSlugsFollowTheDocAnchorsRule(t *testing.T) {
	content := "# Foo — bar\n## `traces ls`\n## Setup ##\n```sh\n# not a heading\n```\n## Setup\n   ## Setup  \n" +
		"## a\n## a\n## a-1\n"
	want := []string{"foo--bar", "traces-ls", "setup", "setup-1", "setup-2", "a", "a-1", "a-1-1"}
	if got := headingSlugs(content); !slices.Equal(got, want) {
		t.Errorf("slugs = %q, want %q", got, want)
	}
}

// TestTheDriftCheckCatchesDrift is the self-test: a fixture skill with one of
// each defect, and the check has to report every one of them and nothing it
// was not meant to. A check that passes the real skill because it can no
// longer see anything would otherwise look exactly like a clean skill.
func TestTheDriftCheckCatchesDrift(t *testing.T) {
	surface := askTheBinary(t)
	fixture := os.DirFS(filepath.Join(surface.root, "agent", "skills", "testdata", "drift"))

	got := check(fixture, surface, shipped)
	want := []string{
		"`tracepad tracez`",                     // an unknown command
		"`tracepad traces lst`",                 // an unknown subcommand
		"--sinse",                               // an unknown flag
		"--global",                              // an unknown flag of the binary's own command
		"`tracepad serve` has no --port",        // an unknown flag of the server
		"`tracepad serve`: unexpected argument", // a positional word after it
		"`tracepad mcp`",                        // a word this test cannot check
		"`get_trcae`",                           // an unknown MCP tool
		"/api/v1/tracez",                        // an unknown route
		"DELETE /api/v1/system",                 // a route under the wrong method
		"../../../docs/cli.md",                  // a link into docs/
		"references/nowhere.md",                 // a relative link that does not resolve
		"docs/nowhere.md",                       // a repository link to no file
		"docs/cli.md#no-such-heading",           // a repository link to no heading
		"--fulll",                               // an inline command, wrapped, checked like a fenced one
		"`tracepad skills show debuging.md`",    // a file the skill does not have
		"`tracepad traces lsx`",                 // behind a prompt
		"`tracepad trace`",                      // behind sudo, a path to the binary
		"`tracepad skills instal`",              // behind `docker run` and the image
	}
	for _, needle := range want {
		found := 0
		for _, problem := range got {
			if strings.Contains(problem, needle) {
				found++
			}
		}
		if found != 1 {
			t.Errorf("want exactly one problem naming %s, got %d", needle, found)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %d problems, want %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}

	tight := check(fixture, surface, budgets{skill: 5, total: 10})
	budget := 0
	for _, problem := range tight {
		if strings.Contains(problem, "lines, over the budget") {
			budget++
		}
	}
	if budget != 2 {
		t.Errorf("a skill over both budgets should be reported twice, got %d", budget)
	}
}

// surface is what the binary has, asked of the binary.
type surface struct {
	t *testing.T
	// root is the repository, for the absolute links that name one of its
	// files.
	root string
	// probes caches what the CLI answered, per question.
	probes map[string]probe
	tools  map[string]bool
	// verbs are the first words of the tool names (`get`, `list`, …): a
	// backticked snake_case word starting with one of them is a tool name,
	// and `ttft_ms` is not.
	verbs  map[string]bool
	routes []route
}

type probe struct {
	code   int
	stderr string
}

type route struct {
	method   string
	segments []string
}

func askTheBinary(t *testing.T) *surface {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// A probe runs the real command with a flag it was handed, and a
	// command that writes to disk (`export --dir`) must not write here.
	t.Chdir(t.TempDir())

	s := &surface{t: t, root: root, probes: map[string]probe{}, tools: map[string]bool{},
		verbs: map[string]bool{}}
	s.listTools()
	s.fetchRoutes()
	return s
}

// listTools asks the MCP server for its tool list, over the SDK's in-memory
// transport. Listing calls no tool, so there is no API behind it.
func (s *surface) listTools() {
	s.t.Helper()
	serverSide, clientSide := mcp.NewInMemoryTransports()
	registry := mcpserver.New("drift", nil)
	ctx, cancel := context.WithCancel(s.t.Context())
	defer cancel()
	go registry.Run(ctx, serverSide)
	session, err := mcp.NewClient(&mcp.Implementation{Name: "drift", Version: "1"}, nil).
		Connect(ctx, clientSide, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	defer session.Close()
	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	for _, tool := range listed.Tools {
		s.tools[tool.Name] = true
		verb, _, _ := strings.Cut(tool.Name, "_")
		s.verbs[verb] = true
	}
	if len(s.tools) == 0 {
		s.t.Fatal("the MCP server listed no tools; the check would pass anything")
	}
}

// fetchRoutes reads `GET /api/v1` from a real server — the route map the skill
// sends agents to, so the test reads what they will read.
func (s *surface) fetchRoutes() {
	s.t.Helper()
	st := storetest.Open(s.t)
	writer, err := st.NewWriter(storetest.Writes)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { writer.Close() })
	cfg := &config.Config{Listen: ":0", MaxBodyBytes: config.DefaultMaxBodyBytes}
	handler := server.New(cfg, "drift", st, writer, st.NewSweeper(writer, store.SweepOptions{})).Handler()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1", nil))
	var index struct {
		Endpoints []struct{ Method, Path string }
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &index); err != nil {
		s.t.Fatalf("GET /api/v1 answered %d: %v", recorder.Code, err)
	}
	for _, endpoint := range index.Endpoints {
		s.routes = append(s.routes, route{endpoint.Method, strings.Split(endpoint.Path, "/")})
	}
	if len(s.routes) == 0 {
		s.t.Fatal("the route map is empty; the check would pass anything")
	}
}

// run asks the CLI one question, and remembers the answer. The context is
// already over and the URL goes nowhere: flags are parsed and subcommands
// dispatched before anything is asked of a server, which is all a probe needs.
func (s *surface) run(args ...string) probe {
	key := strings.Join(args, "\x00")
	if answer, ok := s.probes[key]; ok {
		return answer
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer
	code := cli.Run(ctx, cli.Options{
		Args:    args,
		Version: "drift",
		Stdout:  &stdout,
		Stderr:  &stderr,
		Stdin:   strings.NewReader(""),
		Env: func(name string) string {
			return map[string]string{"TRACEPAD_URL": "http://127.0.0.1:9", "TRACEPAD_API_KEY": "tp-sk-drift"}[name]
		},
	})
	answer := probe{code: code, stderr: stderr.String()}
	s.probes[key] = answer
	return answer
}

// command checks one `tracepad …` invocation, given the words after
// `tracepad`, and returns what is wrong with it.
func (s *surface) command(words []string) []string {
	if len(words) == 0 || strings.HasPrefix(words[0], "-") {
		return []string{"`tracepad` with no command runs the server"}
	}
	name, rest := words[0], words[1:]
	switch name {
	case "version", "help":
		if len(rest) > 0 {
			return []string{fmt.Sprintf("`tracepad %s` takes nothing after it", name)}
		}
		return nil
	case "skills":
		if len(rest) == 0 {
			return []string{"`tracepad skills` needs install or show"}
		}
		set, ok := FlagSets()[rest[0]]
		if !ok {
			return []string{fmt.Sprintf("`tracepad skills %s` is not a subcommand", rest[0])}
		}
		var problems []string
		for _, flag := range flags(rest[1:]) {
			if set.Lookup(flag) == nil {
				problems = append(problems, fmt.Sprintf("`tracepad skills %s` has no --%s", rest[0], flag))
			}
		}
		// `show` takes a file of the skill the binary carries; a
		// placeholder (`<file>`, `[FILE]`) stands for one.
		if rest[0] == "show" {
			for _, word := range rest[1:] {
				if strings.HasPrefix(word, "-") || strings.ContainsAny(word, "<>[]") {
					continue
				}
				if _, found := lookup(Files(), word); !found {
					problems = append(problems, fmt.Sprintf("`tracepad skills show %s` names no file of the skill", word))
				}
			}
		}
		return problems
	case "serve":
		// The setup reference starts a server (spec 053 #13, #19): its words
		// go through config.ParseFlags, which is how `serve` reads its
		// arguments — the flags of the one table, and no positional word. A
		// redirection (`>>log`, `2>`) is the shell's, not the command's.
		var words []string
		for _, word := range rest {
			if !strings.ContainsAny(word, "<>") {
				words = append(words, word)
			}
		}
		if _, err := config.ParseFlags(words); err != nil {
			if name, ok := strings.CutPrefix(err.Error(), "flag provided but not defined: -"); ok {
				return []string{fmt.Sprintf("`tracepad serve` has no --%s", name)}
			}
			return []string{fmt.Sprintf("`tracepad serve`: %v", err)}
		}
		return nil
	case "mcp":
		// Its flags are parsed in package main, out of this test's reach.
		// The skill has no reason to run it; if it grows one, this test grows
		// the check first.
		return []string{fmt.Sprintf("`tracepad %s` cannot be checked by the drift test", name)}
	}
	if !slices.Contains(cli.Commands(), name) {
		return []string{fmt.Sprintf("`tracepad %s` is not a command", name)}
	}

	// A command with subcommands refuses one it does not know with a
	// usage error; a command without them takes `--help` after any word.
	prefix := []string{name}
	if s.run(name, "zz-drift-probe", "--help").code == cli.ExitUsage {
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return []string{fmt.Sprintf("`tracepad %s` needs a subcommand", name)}
		}
		if s.run(name, rest[0], "--help").code != cli.ExitOK {
			return []string{fmt.Sprintf("`tracepad %s %s` is not a subcommand", name, rest[0])}
		}
		prefix = append(prefix, rest[0])
		rest = rest[1:]
	}
	var problems []string
	for _, flag := range flags(rest) {
		if flag == "help" {
			continue
		}
		answer := s.run(append(slices.Clone(prefix), "--"+flag+"=1")...)
		if strings.Contains(answer.stderr, "flag provided but not defined") {
			problems = append(problems, fmt.Sprintf("`tracepad %s` has no --%s",
				strings.Join(prefix, " "), flag))
		}
	}
	return problems
}

// flags are the names of the `--flags` among a command's words.
func flags(words []string) []string {
	var out []string
	for _, word := range words {
		if name, ok := strings.CutPrefix(word, "--"); ok && name != "" {
			name, _, _ = strings.Cut(name, "=")
			out = append(out, name)
		}
	}
	return out
}

var (
	fence      = regexp.MustCompile("^\\s*```")
	inlineCode = regexp.MustCompile("`([^`]+)`")
	toolName   = regexp.MustCompile(`^[a-z]+(?:_[a-z]+)*$`)
	link       = regexp.MustCompile(`\[[^\]]*\]\(([^)\s]+)\)`)
	apiPath    = regexp.MustCompile(`(?:\b(GET|POST|PUT|PATCH|DELETE)\s+)?(/api/v1(?:/[^\s?#"'` + "`" + `)\]]*)?)`)
	repoLink   = regexp.MustCompile(`^https://github\.com/tracepad/tracepad/blob/main/([^#]+)(?:#(.+))?$`)
)

// check reads every file of a skill and returns what is wrong with it.
func check(files fs.FS, s *surface, limits budgets) []string {
	var problems []string
	total := 0
	fs.WalkDir(files, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			problems = append(problems, err.Error())
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		content, err := fs.ReadFile(files, name)
		if err != nil {
			problems = append(problems, err.Error())
			return nil
		}
		lines := strings.Count(string(content), "\n")
		total += lines
		if name == "SKILL.md" && lines > limits.skill {
			problems = append(problems, fmt.Sprintf("SKILL.md has %d lines, over the budget of %d", lines, limits.skill))
		}
		for _, problem := range checkFile(files, name, string(content), s) {
			problems = append(problems, name+": "+problem)
		}
		return nil
	})
	if total > limits.total {
		problems = append(problems, fmt.Sprintf("the skill has %d lines, over the budget of %d", total, limits.total))
	}
	return problems
}

func checkFile(files fs.FS, name, content string, s *surface) []string {
	var problems []string
	report := func(line int, problem string) {
		problems = append(problems, fmt.Sprintf("line %d: %s", line, problem))
	}
	lines := strings.Split(content, "\n")

	// The fenced lines, one shell line at a time. The prose is kept aside,
	// with a blank where a fence was, so it can be read a paragraph at a
	// time below.
	prose := make([]string, len(lines))
	inFence := false
	pending := ""
	for i, line := range lines {
		for _, problem := range s.paths(line) {
			report(i+1, problem)
		}
		if fence.MatchString(line) {
			// A block that ends on a trailing backslash has nothing to
			// continue into; carrying it over would glue it to the next
			// block's first command.
			inFence, pending = !inFence, ""
			continue
		}
		if !inFence {
			prose[i] = line
			continue
		}
		// A trailing backslash continues the command on the next line.
		if joined, more := strings.CutSuffix(line, `\`); more {
			pending += joined + " "
			continue
		}
		for _, words := range invocations(pending + line) {
			for _, problem := range s.command(words) {
				report(i+1, problem)
			}
		}
		pending = ""
	}

	// Markdown lets a code span and a link wrap onto the next line, so the
	// prose is matched a paragraph at a time; a span is reported on the line
	// it starts on.
	for first := 0; first < len(prose); {
		if strings.TrimSpace(prose[first]) == "" {
			first++
			continue
		}
		last := first
		for last < len(prose) && strings.TrimSpace(prose[last]) != "" {
			last++
		}
		paragraph := strings.Join(prose[first:last], "\n")
		lineOf := func(offset int) int { return first + 1 + strings.Count(paragraph[:offset], "\n") }
		for _, match := range inlineCode.FindAllStringSubmatchIndex(paragraph, -1) {
			span := strings.Join(strings.Fields(paragraph[match[2]:match[3]]), " ")
			if strings.Contains(span, "tracepad ") {
				for _, words := range invocations(span) {
					for _, problem := range s.command(words) {
						report(lineOf(match[0]), problem)
					}
				}
				continue
			}
			verb, _, _ := strings.Cut(span, "_")
			if toolName.MatchString(span) && s.verbs[verb] && !s.tools[span] {
				report(lineOf(match[0]), fmt.Sprintf("`%s` is not an MCP tool", span))
			}
		}
		for _, match := range link.FindAllStringSubmatchIndex(paragraph, -1) {
			if problem := s.link(files, name, paragraph[match[2]:match[3]]); problem != "" {
				report(lineOf(match[0]), problem)
			}
		}
		first = last
	}
	return problems
}

// invocations finds the `tracepad …` commands in one shell line: split on
// pipes, `&&`, `;` and command substitution, skip `VAR=value` prefixes, and
// keep the words after `tracepad`. Quotes group words and hide separators.
func invocations(line string) [][]string {
	var segments [][]string
	var words []string
	var word strings.Builder
	quote := rune(0)
	flush := func() {
		if word.Len() > 0 {
			words = append(words, word.String())
			word.Reset()
		}
	}
	boundary := func() {
		flush()
		segments = append(segments, words)
		words = nil
	}
	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '#' && word.Len() == 0:
			boundary()
			return commands(segments)
		case strings.ContainsRune("|;&()", r):
			boundary()
		case r == ' ' || r == '\t':
			flush()
		default:
			word.WriteRune(r)
		}
	}
	boundary()
	return commands(segments)
}

func commands(segments [][]string) [][]string {
	var out [][]string
	for _, words := range segments {
		// What stands in front of the binary without being it: a prompt,
		// `sudo`, `nohup`, `VAR=value`.
		for len(words) > 0 && (words[0] == "$" || words[0] == "sudo" || words[0] == "nohup" ||
			strings.Contains(words[0], "=") && !strings.HasPrefix(words[0], "-")) {
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		switch {
		case isBinary(words[0]):
			out = append(out, words[1:])
		case words[0] == "docker" && len(words) > 1 && words[1] == "run":
			// The command is what follows the image.
			for i, word := range words {
				if isImage(word) {
					out = append(out, words[i+1:])
					break
				}
			}
		}
	}
	return out
}

// isBinary is `tracepad` or a path to it (`./bin/tracepad`); `tracepad.init()`
// and `pip install tracepad` are not a command line starting with it.
func isBinary(word string) bool {
	return word == "tracepad" || strings.HasSuffix(word, "/tracepad") && !strings.Contains(word, ":")
}

// isImage is the published image, with or without a tag.
func isImage(word string) bool {
	image, _, _ := strings.Cut(word, ":")
	return strings.HasSuffix(image, "tracepad/tracepad")
}

// paths checks every `/api/v1…` path on a line against the route map, and
// against the method when one is written in front of it.
func (s *surface) paths(line string) []string {
	var problems []string
	for _, match := range apiPath.FindAllStringSubmatch(line, -1) {
		method, written := match[1], strings.TrimRight(match[2], ".,:;")
		segments := strings.Split(strings.TrimRight(written, "/"), "/")
		known, allowed := false, false
		for _, r := range s.routes {
			if !samePath(r.segments, segments) {
				continue
			}
			known = true
			allowed = allowed || method == "" || method == r.method
		}
		switch {
		case !known:
			problems = append(problems, fmt.Sprintf("%s is not a route", written))
		case !allowed:
			problems = append(problems, fmt.Sprintf("%s %s is not a route", method, written))
		}
	}
	return problems
}

// samePath matches a written path to a route's: a `{name}` in the route takes
// any segment, and a placeholder in the skill (`<id>`, `{id}`) matches only a
// `{name}` — `/traces/<id>` is not `/traces/last`.
func samePath(route, written []string) bool {
	if len(route) != len(written) {
		return false
	}
	for i := range route {
		variable := strings.HasPrefix(route[i], "{")
		placeholder := strings.ContainsAny(written[i], "<>{}…")
		switch {
		case variable && written[i] != "":
		case !placeholder && route[i] == written[i]:
		default:
			return false
		}
	}
	return true
}

// link checks one link target. Inside the skill a relative link has to land
// on a file of the skill; nothing may point into `docs/`, which an installed
// skill has no copy of; and a link to the repository on GitHub has to name a
// file — and a heading — that exists here.
func (s *surface) link(files fs.FS, from, target string) string {
	if match := repoLink.FindStringSubmatch(target); match != nil {
		content, err := os.ReadFile(filepath.Join(s.root, filepath.FromSlash(match[1])))
		if err != nil {
			return fmt.Sprintf("%s names %s, which is not in the repository", target, match[1])
		}
		if match[2] != "" && !hasHeading(string(content), match[2]) {
			return fmt.Sprintf("%s names a heading %s#%s does not have", target, match[1], match[2])
		}
		return ""
	}
	if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
		return ""
	}
	file, _, _ := strings.Cut(target, "#")
	if slices.Contains(strings.Split(file, "/"), "docs") {
		return fmt.Sprintf("%s points into docs/, which an installed skill does not have", target)
	}
	resolved := path.Join(path.Dir(from), file)
	if strings.HasPrefix(resolved, "..") {
		return fmt.Sprintf("%s leaves the skill's directory", target)
	}
	if _, err := fs.Stat(files, resolved); err != nil {
		return fmt.Sprintf("%s does not resolve inside the skill", target)
	}
	return ""
}

// hasHeading reports whether a Markdown file has a heading with this anchor,
// under GitHub's rule (github-slugger), which `scripts/doc-anchors.sh` states
// too: lower case; everything but ASCII letters, digits, `_`, `-` and spaces
// removed; spaces to hyphens; closing `#`s and trailing blanks dropped; a slug
// already taken gets `-1`, `-2`, … until it is free, so `a`, `a`, `a-1` become
// `a`, `a-1`, `a-1-1`. Setext headings are not read, as the script does not
// read them: the documents the skill links to have none.
func hasHeading(content, anchor string) bool {
	return slices.Contains(headingSlugs(content), anchor)
}

var (
	headingLine    = regexp.MustCompile(`^ {0,3}#{1,6}([ \t]|$)`)
	closingHashes  = regexp.MustCompile(`[ \t]+#+[ \t]*$`)
	openingHashes  = regexp.MustCompile(`^ {0,3}#+[ \t]*`)
	notSlugPattern = regexp.MustCompile(`[^-_ a-z0-9]`)
)

func headingSlugs(content string) []string {
	var out []string
	taken := map[string]int{}
	inFence := false
	for _, line := range strings.Split(content, "\n") {
		if fence.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence || !headingLine.MatchString(line) {
			continue
		}
		text := strings.TrimSpace(closingHashes.ReplaceAllString(openingHashes.ReplaceAllString(line, ""), ""))
		base := strings.ReplaceAll(notSlugPattern.ReplaceAllString(strings.ToLower(text), ""), " ", "-")
		slug := base
		for {
			if _, used := taken[slug]; !used {
				break
			}
			taken[base]++
			slug = fmt.Sprintf("%s-%d", base, taken[base])
		}
		taken[slug] = 0
		out = append(out, slug)
	}
	return out
}
