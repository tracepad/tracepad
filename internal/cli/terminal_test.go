package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// What a trace carries is written by whoever holds the project's key, or by
// the end user whose text the application logged, and the human mode prints
// it to a terminal (spec 004 #35). These tests put the sequences a terminal
// acts on into every field the server stores, run every command that shows
// them, and read the output byte by byte.

// payload is the attack: an OSC 52 clipboard write, a screen wipe and a
// cursor home, a C1 CSI, a bidi override, a carriage return and a newline —
// inside the value rather than at its end, where the facets would trim them
// and then leave the value out as one no filter can spell.
const payload = "\x1b]52;c;ZWNobyBwd25lZA==\x07\x1b[2J\x1b[H\u009b31m\u202eevil\r\nend"

// hostile is the payload behind a word naming the field, so the output can be
// searched for each field on its own.
func hostile(field string) string { return field + payload }

// shown is what a one-line field reads as once it is safe: every byte of the
// attack still there, as text.
func shown(field string) string {
	return field + `\x1b]52;c;ZWNobyBwd25lZA==\x07\x1b[2J\x1b[H\u009b31m\u202eevil\x0d\x0aend`
}

// shownText is the start of what a block of text reads as: the same, up to
// the newline it is allowed to keep.
func shownText(field string) string {
	return field + `\x1b]52;c;ZWNobyBwd25lZA==\x07\x1b[2J\x1b[H\u009b31m\u202eevil\x0d`
}

// assertInert fails on anything a terminal would act on. The one escape the
// human mode writes itself is the faint style around a search snippet, and
// only as those two exact sequences.
func assertInert(t *testing.T, what, out string) {
	t.Helper()
	out = strings.NewReplacer("\x1b[2m", "", "\x1b[0m", "").Replace(out)
	if !utf8.ValidString(out) {
		t.Errorf("%s: output is not UTF-8:\n%q", what, out)
	}
	for i, r := range out {
		unsafe := (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f ||
			(r >= 0x80 && r <= 0x9f) ||
			(r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
		if unsafe {
			t.Errorf("%s: raw %U at byte %d:\n%q", what, r, i, out)
			return
		}
	}
}

func seedHostile(t *testing.T, h *harness) {
	t.Helper()
	start := seedBase - 600*1000*ms
	h.seed(t, &model.Trace{ID: traceHex(9), Name: hostile("name"), UserID: hostile("user"),
		SessionID: hostile("session"), Environment: hostile("env"), Release: hostile("release"),
		Version: hostile("version"), Tags: []string{hostile("tag")},
		Metadata: map[string]any{"note": hostile("meta")}},
		&model.Observation{TraceID: traceHex(9), ID: spanHex(9), Type: model.TypeGeneration,
			Name: hostile("obs"), Model: hostile("model"), Level: model.LevelError,
			StatusMessage: hostile("status"), PromptName: hostile("prompt"),
			StartTime: start, EndTime: start + 500*ms,
			Input:  "input " + payload + " markerword",
			Output: map[string]any{"text": hostile("output")}})
	score := &store.Score{ID: traceHex(9), TraceID: traceHex(9), Name: hostile("score"),
		DataType: store.ScoreCategorical, StringValue: ptr(hostile("value")),
		Comment: hostile("comment"), Timestamp: start, CreatedAt: start}
	if err := h.writer.Submit(t.Context(),
		&store.ScoreWrite{ProjectID: h.projectID(t), Scores: []*store.Score{score}}); err != nil {
		t.Fatal(err)
	}
	// The users listing answers from the rollup alone (spec 023 #4).
	rollUsers(t, h, start/1e9/3600*3600)
}

func ptr[T any](value T) *T { return &value }

// jsonString is a Go string as a JSON literal, for the files the write
// commands read.
func jsonString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestHumanOutputIsInertOnATerminal(t *testing.T) {
	h := newHarness(t)
	seedHostile(t, h)
	ctx := t.Context()
	dir := t.TempDir()

	// The eval and review halves, which carry text of their own: a
	// dataset's description and cases, a run's name and failure, a prompt's
	// text and commit message, a queue's description.
	cases := filepath.Join(dir, "cases.jsonl")
	writeFile(t, cases, `{"id": "`+cliItemID(1)+`", "input": `+jsonString(t, hostile("case"))+`}`+"\n")
	for _, args := range [][]string{
		{"datasets", "push", "golden", "--file", cases, "--description", hostile("description")},
		{"runs", "create", "golden", "--id", cliRunID(1), "--name", hostile("run")},
		{"runs", "finish", cliRunID(1), "--failed", hostile("failed")},
		{"score-configs", "push", "quality", "--file",
			writeTemp(t, `{"data_type": "numeric", "direction": "higher", "description": `+jsonString(t, hostile("config"))+`}`)},
		{"queues", "put", "desk", "--config", "quality", "--description", hostile("queue")},
	} {
		if got := h.run(ctx, true, args...); got.code != ExitOK {
			t.Fatalf("%v = %+v", args, got)
		}
	}
	prompt := filepath.Join(dir, "prompt.json")
	for _, text := range []string{hostile("first"), hostile("second")} {
		writeFile(t, prompt, `{"type": "text", "prompt": `+jsonString(t, text)+`}`)
		if got := h.run(ctx, true, "prompts", "push", "support", "--file", prompt,
			"--message", hostile("message")); got.code != ExitOK {
			t.Fatalf("prompts push = %+v", got)
		}
	}

	for _, check := range []struct {
		args []string
		// want are the fields the command shows, each of which has to
		// arrive — escaped, but there: a field that vanished would pass
		// the byte check and still be a bug.
		want []string
		// text are the fields shown as a block, whose newline stays.
		text []string
		// fragments are said exactly, for the one field the server cuts:
		// a search snippet is words around the hit.
		fragments []string
	}{
		{args: []string{"traces", "ls"}, want: []string{"name", "env"}},
		{args: []string{"traces", "ls", "--search", "markerword"},
			want:      []string{"name"},
			fragments: []string{`input: input \x1b]52;c;ZWNobyBwd25lZA==\x07\x1b[2J\x1b[H\u009b31m\u202eevil`}},
		{args: []string{"traces", "show", traceHex(9), "--full"},
			want: []string{"name", "env", "release", "version", "user", "session", "tag",
				"obs", "model", "prompt"},
			text: []string{"status"}},
		{args: []string{"traces", "last"}, want: []string{"name", "env", "release"}},
		{args: []string{"sessions", "ls"}, want: []string{"session"}},
		{args: []string{"sessions", "show", hostile("session")}, want: []string{"session", "name"}},
		{args: []string{"users", "ls"}, want: []string{"user"}},
		{args: []string{"users", "show", hostile("user")}, want: []string{"user"}},
		{args: []string{"scores", "ls"}, want: []string{"score", "value", "comment"}},
		{args: []string{"scores", "trend", "--since", "24h"}, want: []string{"score"}},
		{args: []string{"stats", "--group-by", "environment", "--since", "24h"}, want: []string{"env"}},
		{args: []string{"stats", "--group-by", "model", "--since", "24h"}, want: []string{"model"}},
		{args: []string{"stats", "--group-by", "release", "--since", "24h"}, want: []string{"release"}},
		{args: []string{"facets"}, want: []string{"env", "release", "name"}},
		{args: []string{"datasets", "ls"}, want: []string{"description"}},
		{args: []string{"datasets", "show", "golden"}, text: []string{"description"}},
		{args: []string{"runs", "ls"}, want: []string{"run"}},
		{args: []string{"runs", "show", cliRunID(1)}, want: []string{"run"}, text: []string{"failed"}},
		{args: []string{"score-configs", "ls"}, want: []string{"config"}},
		{args: []string{"score-configs", "show", "quality"}, text: []string{"config"}},
		{args: []string{"queues", "ls"}, want: []string{"queue"}},
		{args: []string{"prompts", "get", "support"}, want: []string{"message"}},
		{args: []string{"prompts", "diff", "support", "--from", "1", "--to", "2"}},
	} {
		// Quoted: the arguments carry the attack too, and a failure
		// message is printed to the terminal running the tests.
		what := fmt.Sprintf("%q", check.args)
		got := h.run(ctx, true, check.args...)
		if got.code != ExitOK {
			t.Errorf("%s: exit = %d, stderr = %s", what, got.code, got.stderr)
			continue
		}
		assertInert(t, what, got.stdout)
		assertInert(t, what+" (stderr)", got.stderr)
		for _, field := range check.want {
			if !strings.Contains(got.stdout, shown(field)) {
				t.Errorf("%s: %s is missing or not escaped as %q:\n%s", what, field, shown(field), got.stdout)
			}
		}
		for _, fragment := range check.fragments {
			if !strings.Contains(got.stdout, fragment) {
				t.Errorf("%s: output is missing %q:\n%s", what, fragment, got.stdout)
			}
		}
		for _, field := range check.text {
			if !strings.Contains(got.stdout, shownText(field)) {
				t.Errorf("%s: %s is missing or not escaped as %q:\n%s", what, field, shownText(field), got.stdout)
			}
		}
	}
}

// The payloads are JSON on the screen, and JSON escapes C0 already — but not
// C1, which reaches the terminal as two bytes of UTF-8 unless it is escaped
// here too.
func TestPayloadsAreInertOnATerminal(t *testing.T) {
	h := newHarness(t)
	seedHostile(t, h)

	got := h.run(t.Context(), true, "traces", "show", traceHex(9), "--full")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	assertInert(t, "traces show --full", got.stdout)
	for _, fragment := range []string{
		`input: "input \u001b]52;c;ZWNobyBwd25lZA==\u0007\u001b[2J\u001b[H\u009b31m`,
		`output: {"text":"output\u001b]52;c;`,
	} {
		if !strings.Contains(got.stdout, fragment) {
			t.Errorf("output is missing %q:\n%s", fragment, got.stdout)
		}
	}
}

// A follow prints the same fields a listing does, one line at a time.
func TestTailIsInertOnATerminal(t *testing.T) {
	h := newHarness(t)
	seedHostile(t, h)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	follow := h.follow(ctx, true, "tail", "--interval", "20ms")
	follow.await(t, shown("name"), "the hostile trace, escaped")
	cancel()
	got := follow.wait()
	assertInert(t, "tail", got.stdout)
	if !strings.Contains(got.stdout, shown("env")) {
		t.Errorf("tail is missing the environment:\n%s", got.stdout)
	}
}

// The JSON mode is the API's bytes and stays so: the C1 character JSON does
// not escape arrives as it was sent, because a pipe is something a program
// parses and the escaping is the program's to do (#1).
func TestJSONModeKeepsTheAPIsBytes(t *testing.T) {
	h := newHarness(t)
	seedHostile(t, h)

	for _, check := range []struct {
		args []string
		path string
	}{
		{[]string{"traces", "show", traceHex(9), "--full"},
			"/api/v1/traces/" + traceHex(9) + fmt.Sprintf("?expand=io&budget=%d", fullBudget)},
		{[]string{"traces", "ls"}, "/api/v1/traces"},
	} {
		direct := httpGet(t, h.url+check.path)
		// A pipe, and `--json` on a terminal: the terminal's escaping
		// writer is not the one the JSON goes through.
		for _, tty := range []bool{false, true} {
			got := h.run(t.Context(), tty, append(check.args, "--json")...)
			if got.code != ExitOK {
				t.Fatalf("%v: exit = %d, stderr = %s", check.args, got.code, got.stderr)
			}
			if strings.TrimRight(got.stdout, "\n") != strings.TrimRight(direct, "\n") {
				t.Errorf("%v, tty %v: the CLI and the endpoint disagree:\ncli:  %q\ncurl: %q",
					check.args, tty, got.stdout, direct)
			}
			if !strings.Contains(got.stdout, "\u009b") {
				t.Errorf("%v, tty %v: the C1 character did not arrive as sent:\n%q", check.args, tty, got.stdout)
			}
		}
	}
}

// The backstop: a renderer that forgets termsafe and prints a server value
// with a bare Fprintf still cannot put a control sequence on the terminal,
// because a terminal run's stdout and stderr escape what goes through them.
// The faint-style pair dim writes is the one sequence let through, and the
// JSON mode's writer is left alone (#35).
func TestATerminalRunsWritersEscapeWhatARendererForgot(t *testing.T) {
	var out, errOut bytes.Buffer
	r := newRun(Options{Stdout: &out, Stderr: &errOut, TTY: true,
		Env: func(string) string { return "" }})

	fmt.Fprintf(r.opt.Stdout, "name %s\n", hostile("raw"))
	fmt.Fprintln(r.opt.Stdout, dim("snippet", true))
	fmt.Fprintf(r.opt.Stderr, "tracepad: %s\n", hostile("err"))
	for what, got := range map[string]string{"stdout": out.String(), "stderr": errOut.String()} {
		assertInert(t, what, got)
		if strings.Contains(got, "\x1b") && !strings.Contains(got, "\x1b[2msnippet\x1b[0m") {
			t.Errorf("%s: an escape other than the faint pair: %q", what, got)
		}
	}
	if !strings.Contains(out.String(), shownText("raw")) || !strings.Contains(out.String(), "\x1b[2msnippet\x1b[0m") {
		t.Errorf("stdout = %q", out.String())
	}
	if !strings.Contains(errOut.String(), shownText("err")) {
		t.Errorf("stderr = %q", errOut.String())
	}

	out.Reset()
	fmt.Fprintf(r.stdout, "%s\n", `{"name":"\u001b"}`+"\u009b")
	if out.String() != `{"name":"\u001b"}`+"\u009b\n" {
		t.Errorf("the JSON writer changed the bytes: %q", out.String())
	}
}

// A table's headers can come from the server too: `stats` and `scores trend`
// name their first column after the answer's own `group_by`. The real server
// only ever answers with one of its enums, so this is a server that does not.
func TestTableHeadersFromTheServerAreEscaped(t *testing.T) {
	bad := jsonString(t, hostile("group"))
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/stats":
			fmt.Fprintf(w, `{"group_by":%s,"unit":%s,"buckets":[{"key":"k","count":1,"error_count":0}]}`, bad, bad)
		case "/api/v1/stats/scores":
			fmt.Fprintf(w, `{"group_by":%s,"targets":"traces","series":[{"name":"s","data_type":"NUMERIC",`+
				`"buckets":[{"key":"k","count":1,"mean":1}]}]}`, bad)
		default:
			http.NotFound(w, r)
		}
	}))
	defer fake.Close()
	h := &harness{url: fake.URL, env: map[string]string{"TRACEPAD_URL": fake.URL, "TRACEPAD_API_KEY": testKey}}

	for _, args := range [][]string{{"stats"}, {"scores", "trend"}} {
		got := h.run(t.Context(), true, args...)
		if got.code != ExitOK {
			t.Fatalf("%v: exit = %d, stderr = %s", args, got.code, got.stderr)
		}
		assertInert(t, fmt.Sprintf("%q", args), got.stdout)
		// Upper-cased by the renderer, then escaped as a one-line field:
		// the escaping writer alone would keep the newline, and the
		// header would break the table in two.
		if !strings.Contains(got.stdout, `GROUP\x1b]52;C;`) || !strings.Contains(got.stdout, `EVIL\x0d\x0aEND`) {
			t.Errorf("%v: the header is missing or not escaped:\n%s", args, got.stdout)
		}
	}
}

// An error message is text by nature — often a traceback — and keeps its
// lines, each indented under the label, with what is inside them escaped.
func TestAnErrorMessageKeepsItsLines(t *testing.T) {
	h := newHarness(t)
	start := seedBase - 600*1000*ms
	h.seed(t, &model.Trace{ID: traceHex(8), Name: "failing"},
		&model.Observation{TraceID: traceHex(8), ID: spanHex(8), Type: model.TypeSpan,
			Name: "call", Level: model.LevelError, StartTime: start, EndTime: start + ms,
			StatusMessage: "Traceback (most recent call last):\n\tFile \"app.py\", line 3\nValueError: \x1b[2Jboom"})

	got := h.run(t.Context(), true, "traces", "show", traceHex(8))
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	assertInert(t, "traces show", got.stdout)
	want := "    status: Traceback (most recent call last):\n" +
		"            \tFile \"app.py\", line 3\n" +
		`            ValueError: \x1b[2Jboom` + "\n"
	if !strings.Contains(got.stdout, want) {
		t.Errorf("output is missing\n%s\nin\n%s", want, got.stdout)
	}
}
