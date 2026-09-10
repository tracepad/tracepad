package mapping

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// docs/ingest.md is the mapping's contract page, and these are the two claims
// on it that a reader acts on directly: the attributes the mapper reads, and
// the block they paste into `~/.claude/settings.json`. Both are checked here
// the way the CLI's usage lines are checked against its flags — parsed out of
// the text, compared with what the code does — because a page nothing reads
// drifts silently and is worse than no page.
const ingestDoc = "../../docs/ingest.md"

func ingestPage(t *testing.T) string {
	t.Helper()
	page, err := os.ReadFile(ingestDoc)
	if err != nil {
		t.Fatal(err)
	}
	return string(page)
}

// Every bare usage key the mapper reads is named in the mapping table
// (spec 030 #2). The set is closed precisely so that it can be written down;
// a key added to rules.go and not to the table is a key nobody can find out
// about, which is the whole objection Decision 2 answers.
func TestBareUsageKeysAreDocumented(t *testing.T) {
	var usageRow string
	for _, line := range strings.Split(ingestPage(t), "\n") {
		if strings.HasPrefix(line, "| usage |") {
			usageRow = line
		}
	}
	if usageRow == "" {
		t.Fatalf("no `| usage |` row in %s: the mapping table is where every attribute the mapper reads is named", ingestDoc)
	}
	for _, key := range bareUsageKeys {
		if !strings.Contains(usageRow, "`"+key+"`") {
			t.Errorf("the usage row does not name %q; add it to the table in %s", key, ingestDoc)
		}
	}
}

// The Claude Code section offers a JSON object to paste into a settings file.
// It has to parse, and it has to carry the variables the section's own shell
// block sets — a `settings.json` that is one comma short, or one variable
// short, fails where nothing reports it: telemetry simply never arrives.
func TestClaudeCodeSettingsBlockIsValid(t *testing.T) {
	page := ingestPage(t)
	section := regexp.MustCompile(`(?s)\n## Claude Code\n(.*?)\n## `).FindStringSubmatch(page)
	if section == nil {
		t.Fatalf("no `## Claude Code` section in %s (spec 030 #3)", ingestDoc)
	}
	body := section[1]

	block := regexp.MustCompile("(?s)```json\n(.*?)```").FindStringSubmatch(body)
	if block == nil {
		t.Fatalf("the Claude Code section carries no JSON block; it must offer the `env` object for ~/.claude/settings.json")
	}
	var settings struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal([]byte(block[1]), &settings); err != nil {
		t.Fatalf("the settings block is not valid JSON: %v\n%s", err, block[1])
	}

	// Every variable the shell block exports is a variable the JSON block
	// sets: the two are the same instructions in two shapes, and a reader
	// picks whichever suits them.
	exported := regexp.MustCompile(`export ([A-Z_]+)=`).FindAllStringSubmatch(body, -1)
	if len(exported) == 0 {
		t.Fatalf("the Claude Code section exports nothing; it must show the variables as a shell block too")
	}
	for _, match := range exported {
		name := match[1]
		// The environment attribute is offered separately, as the way to
		// keep these traces apart, and is deliberately not in the block
		// somebody pastes to turn telemetry on.
		if name == "OTEL_RESOURCE_ATTRIBUTES" {
			continue
		}
		if _, set := settings.Env[name]; !set {
			t.Errorf("the shell block exports %s and the settings block does not set it", name)
		}
	}
	// And the protocol is the one Tracepad serves: gRPC is not implemented,
	// so a page that suggested it would send its reader nowhere.
	if got := settings.Env["OTEL_EXPORTER_OTLP_PROTOCOL"]; !strings.HasPrefix(got, "http/") {
		t.Errorf("OTEL_EXPORTER_OTLP_PROTOCOL = %q, want an HTTP protocol: Tracepad does not serve gRPC", got)
	}
}
