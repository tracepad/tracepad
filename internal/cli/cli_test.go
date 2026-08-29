package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/config"
	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/server"
	"github.com/tracepad/tracepad/internal/store"
)

// The CLI's tests (spec 004, Testing #3) run the real commands against a real
// server over real HTTP. Nothing is mocked, because the thing under test is
// exactly "does the API answer what the command needs" — a stub would answer
// yes by construction (#1).

const (
	testKey     = "tp-sk-test-secret"
	testVersion = "test"
)

// seedBase is a fixed instant (2026-09-01T00:00:00Z) so that rendered output
// never depends on when the test ran.
const seedBase int64 = 1788220800_000_000_000

const ms = int64(1_000_000)

type harness struct {
	url    string
	store  *store.Store
	writer *store.Writer
	env    map[string]string
	// stdin is what an interactive confirmation reads (spec 005 #13).
	stdin string
}

func newHarness(t *testing.T) *harness { return newHarnessWithToken(t, "") }

// newHarnessWithToken is the same server with a cross-project admin token
// configured, which the administrative tests need and nothing else does.
func newHarnessWithToken(t *testing.T, token string) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tracepad.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.CreateProject("test", store.KeyPair{PublicKey: "tp-pk-test", Secret: testKey}); err != nil {
		t.Fatal(err)
	}
	writer, err := st.NewWriter(store.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })

	cfg := &config.Config{Listen: ":0", StoreRaw: true, MaxBodyBytes: config.DefaultMaxBodyBytes,
		AdminToken: token}
	httpServer := httptest.NewServer(server.New(cfg, testVersion, st, writer,
		st.NewSweeper(writer, store.SweepOptions{})).Handler())
	t.Cleanup(httpServer.Close)

	return &harness{
		url:    httpServer.URL,
		store:  st,
		writer: writer,
		env:    map[string]string{"TRACEPAD_URL": httpServer.URL, "TRACEPAD_API_KEY": testKey},
	}
}

// result is one command's whole observable behaviour.
type result struct {
	stdout string
	stderr string
	code   int
}

// run executes a command in one of the two output modes. `tty` is what the
// real binary derives from stdout; simulating it is how both modes are tested
// without a pseudo-terminal (#12).
func (h *harness) run(ctx context.Context, tty bool, args ...string) result {
	var out, errOut bytes.Buffer
	code := Run(ctx, Options{
		Args:    args,
		Version: testVersion,
		Stdout:  &out,
		Stderr:  &errOut,
		Stdin:   strings.NewReader(h.stdin),
		TTY:     tty,
		Env:     func(key string) string { return h.env[key] },
		Now:     func() time.Time { return time.Unix(0, seedBase).UTC() },
	})
	return result{stdout: out.String(), stderr: errOut.String(), code: code}
}

func (h *harness) seed(t *testing.T, trace *model.Trace, observations ...*model.Observation) {
	t.Helper()
	err := h.writer.Submit(t.Context(), &store.IngestBatch{
		ProjectID:    h.projectID(t),
		Traces:       []*model.Trace{trace},
		Observations: observations,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (h *harness) projectID(t *testing.T) string {
	t.Helper()
	project, err := h.store.ProjectByName("test")
	if err != nil || project == nil {
		t.Fatalf("project = %v, err = %v", project, err)
	}
	return project.ID
}

func traceHex(n int) string { return fmt.Sprintf("%032x", n) }
func spanHex(n int) string  { return fmt.Sprintf("%016x", n) }

func seedCorpus(t *testing.T, h *harness) {
	t.Helper()
	h.seed(t, &model.Trace{ID: traceHex(1), Name: "support-chat", UserID: "u1",
		SessionID: "s1", Environment: "production"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeSpan,
			Name: "handle-request", Level: model.LevelDefault,
			StartTime: seedBase - 3600*1000*ms, EndTime: seedBase - 3600*1000*ms + 820*ms},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(2), ParentObservationID: spanHex(1),
			Type: model.TypeGeneration, Name: "chat-completion", Model: "claude-sonnet-5",
			Level: model.LevelDefault, StartTime: seedBase - 3600*1000*ms + 40*ms,
			EndTime:     seedBase - 3600*1000*ms + 780*ms,
			Usage:       map[string]any{"total": 169},
			CostDetails: map[string]any{"total": 0.001},
			Input:       []any{map[string]any{"role": "user", "content": "how do I reset my password?"}}})
	h.seed(t, &model.Trace{ID: traceHex(2), Name: "nightly-eval", Environment: "staging"},
		&model.Observation{TraceID: traceHex(2), ID: spanHex(3), Type: model.TypeGeneration,
			Name: "judge", Level: model.LevelError, StatusMessage: "rate limited",
			StartTime: seedBase - 600*1000*ms, EndTime: seedBase - 600*1000*ms + 1500*ms})
}

// TestTracesListBothModes is the golden pair: the same question answered as a
// table on a terminal and as the API's own bytes in a pipe.
func TestTracesListBothModes(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)
	ctx := t.Context()

	t.Run("terminal", func(t *testing.T) {
		got := h.run(ctx, true, "traces", "ls")
		if got.code != ExitOK {
			t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
		}
		want := strings.Join([]string{
			"TIME                 ID                                NAME          ENV         OBS  ERR  LATENCY  COST",
			"2026-08-31 23:50:00  " + traceHex(2) + "  nightly-eval  staging     1    1    1.5s     -",
			"2026-08-31 23:00:00  " + traceHex(1) + "  support-chat  production  2    0    820ms    $0.001000",
			"",
		}, "\n")
		if got.stdout != want {
			t.Fatalf("table output:\n%s\nwant:\n%s", got.stdout, want)
		}
	})

	t.Run("pipe", func(t *testing.T) {
		got := h.run(ctx, false, "traces", "ls")
		if got.code != ExitOK {
			t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
		}
		// A pipe gets machine JSON with no flags at all, and it is the
		// API's bytes rather than a re-rendering of them.
		var body struct {
			Traces []struct {
				ID string `json:"id"`
			} `json:"traces"`
			NextCursor *string `json:"next_cursor"`
		}
		if err := json.Unmarshal([]byte(got.stdout), &body); err != nil {
			t.Fatalf("pipe output is not JSON: %v (%s)", err, got.stdout)
		}
		if len(body.Traces) != 2 || body.Traces[0].ID != traceHex(2) {
			t.Fatalf("traces = %+v, want both, newest first", body.Traces)
		}
	})

	t.Run("--json forces JSON on a terminal", func(t *testing.T) {
		got := h.run(ctx, true, "traces", "ls", "--json")
		if !strings.HasPrefix(got.stdout, `{"traces":`) {
			t.Fatalf("output = %s, want JSON", got.stdout)
		}
	})
}

// TestWalkingBothWays: `--oldest` lands at the far end, and the listing has to
// say how to come back — a jump with no way out is a dead end the docs would
// be advertising (PR #11 review).
func TestWalkingBothWays(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)
	ctx := t.Context()

	newest := h.run(ctx, true, "traces", "ls", "--limit", "1")
	if !strings.Contains(newest.stdout, "older: --cursor ") {
		t.Errorf("the newest page does not say how to go on:\n%s", newest.stdout)
	}
	if strings.Contains(newest.stdout, "newer:") {
		t.Errorf("the newest page claims a page above it:\n%s", newest.stdout)
	}

	oldest := h.run(ctx, true, "traces", "ls", "--limit", "1", "--oldest")
	if strings.Contains(oldest.stdout, "older:") {
		t.Errorf("the oldest page claims a page below it:\n%s", oldest.stdout)
	}
	if !strings.Contains(oldest.stdout, "newer: --newer --cursor ") {
		t.Errorf("the oldest page is a dead end:\n%s", oldest.stdout)
	}

	// `--oldest` with a cursor would quietly stop being a jump: the cursor
	// wins and the far end is never reached. Refused rather than surprising.
	both := h.run(ctx, true, "traces", "ls", "--oldest", "--cursor", "whatever")
	if both.code != ExitUsage {
		t.Errorf("--oldest --cursor exited %d, want a usage error", both.code)
	}

	// And the way back is a command that runs.
	fields := strings.Fields(oldest.stdout[strings.Index(oldest.stdout, "newer:"):])
	back := h.run(ctx, true, "traces", "ls", "--limit", "1", fields[1], fields[2], fields[3])
	if back.code != ExitOK {
		t.Fatalf("walking back exited %d: %s", back.code, back.stderr)
	}
	if !strings.Contains(back.stdout, traceHex(2)) {
		t.Errorf("walking back from the oldest page did not reach the newer trace:\n%s", back.stdout)
	}
}

// TestCLIMatchesTheAPIByte checks the claim the whole design rests on: the CLI
// answers with the API's bytes, not with its own rendering of them (#1).
func TestCLIMatchesTheAPIByte(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)

	got := h.run(t.Context(), false, "traces", "show", traceHex(1), "--full")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	direct := httpGet(t, h.url+"/api/v1/traces/"+traceHex(1)+
		fmt.Sprintf("?expand=io&budget=%d", fullBudget))
	if strings.TrimRight(got.stdout, "\n") != strings.TrimRight(direct, "\n") {
		t.Fatalf("the CLI and the endpoint disagree:\ncli:  %s\ncurl: %s", got.stdout, direct)
	}
}

func TestTracesShowTree(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)

	got := h.run(t.Context(), true, "traces", "show", traceHex(1), "--full")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	for _, fragment := range []string{
		"trace " + traceHex(1),
		"name        support-chat",
		"session     s1",
		"· span  handle-request",
		"  · generation  chat-completion  claude-sonnet-5",
		"169 tokens",
		"reset my password",
	} {
		if !strings.Contains(got.stdout, fragment) {
			t.Errorf("output is missing %q:\n%s", fragment, got.stdout)
		}
	}
}

// TestRenderSaysWhenPayloadsWereNotExpanded: when the server refuses an
// expansion because the budget could not carry a marker for every payload,
// the human output has to say so, or a reader of `--full` is left wondering
// where the payloads went (Decision 31).
//
// A renderer test rather than an end-to-end one on purpose: `--full` asks for
// the largest budget the API allows, so provoking the refusal through the
// command would need a trace of tens of thousands of payloads. What is under
// test here is the rendering, and the server side has its own test.
func TestRenderSaysWhenPayloadsWereNotExpanded(t *testing.T) {
	var out bytes.Buffer
	trace := traceDetail{traceRow: traceRow{ID: traceHex(1), Environment: "production"}}
	trace.Expansion = &struct {
		Payloads     int    `json:"payloads"`
		BudgetNeeded int    `json:"budget_needed"`
		Reason       string `json:"reason"`
	}{Payloads: 600, BudgetNeeded: 178000, Reason: "a budget of 51200 bytes cannot carry 600 markers"}

	renderTraceDetail(&out, trace)
	for _, fragment := range []string{"600 payloads were not expanded", "cannot carry 600 markers"} {
		if !strings.Contains(out.String(), fragment) {
			t.Errorf("output is missing %q:\n%s", fragment, out.String())
		}
	}
}

func TestTracesLast(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)

	got := h.run(t.Context(), true, "traces", "last", "--error")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "trace "+traceHex(2)) {
		t.Fatalf("output = %s, want the one failed trace", got.stdout)
	}
	if !strings.Contains(got.stdout, "status: rate limited") {
		t.Errorf("output = %s, want the failure's message", got.stdout)
	}
}

// TestExitCodes is the contract a script depends on: 2 for a typo, 1 for a
// server saying no, 0 otherwise (#12).
func TestExitCodes(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	for _, tc := range []struct {
		name string
		args []string
		want int
		says string
	}{
		{"unknown command", []string{"trace"}, ExitUsage, "unknown command"},
		{"unknown subcommand", []string{"traces", "list"}, ExitUsage, "traces takes ls"},
		{"unknown flag", []string{"traces", "ls", "--nope"}, ExitUsage, "flag provided but not defined"},
		{"a missing argument", []string{"traces", "show"}, ExitUsage, "needs 1 argument"},
		{"a limit out of range", []string{"traces", "ls", "--limit", "5000"}, ExitUsage, "--limit must be"},
		{"an unparseable --since", []string{"traces", "ls", "--since", "yesterday"}, ExitUsage, "--since takes"},
		{"nothing found", []string{"traces", "last"}, ExitFailure, "no trace matches"},
		{"no such trace", []string{"traces", "show", traceHex(9)}, ExitFailure, "not found"},
		{"a listing that works", []string{"traces", "ls"}, ExitOK, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := h.run(ctx, false, tc.args...)
			if got.code != tc.want {
				t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, tc.want, got.stderr)
			}
			if tc.says != "" && !strings.Contains(got.stderr, tc.says) {
				t.Errorf("stderr = %q, want it to mention %q", got.stderr, tc.says)
			}
		})
	}
}

// TestARefusalNamesTheFlagItParsed: one parser reads both ends of a window, so
// a hardcoded flag name in its refusals is a message that sends the reader to
// the wrong half of their own command line.
func TestARefusalNamesTheFlagItParsed(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	for _, tc := range []struct {
		name string
		args []string
		says string
	}{
		{"sessions ls", []string{"sessions", "ls", "--until", "yesterday"}, "--until takes"},
		{"traces ls", []string{"traces", "ls", "--until", "yesterday"}, "--until takes"},
		{"stats", []string{"stats", "--until", "yesterday"}, "--until takes"},
		{"a window that runs forwards", []string{"sessions", "ls", "--until=-1h"}, "--until must be"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := h.run(ctx, false, tc.args...)
			if got.code != ExitUsage {
				t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, ExitUsage, got.stderr)
			}
			// The refusal itself, not the usage text under it — that lists
			// every flag the binary has, `--since` among them.
			reason, _, _ := strings.Cut(got.stderr, "\n")
			if !strings.Contains(reason, tc.says) {
				t.Errorf("stderr = %q, want it to mention %q", reason, tc.says)
			}
			if strings.Contains(reason, "--since") {
				t.Errorf("stderr = %q, names a flag that was never given", reason)
			}
		})
	}
}

// TestBadCredentialsAreARequestError: a wrong key is the server saying no, not
// a typo in the command, so it exits 1.
func TestBadCredentialsAreARequestError(t *testing.T) {
	h := newHarness(t)
	got := h.run(t.Context(), false, "traces", "ls", "--key", "tp-sk-wrong")
	if got.code != ExitFailure {
		t.Fatalf("exit = %d, want a request failure", got.code)
	}
	if !strings.Contains(got.stderr, "unauthorized") {
		t.Errorf("stderr = %q, want the server's own word", got.stderr)
	}
}

// TestMissingKeyIsAUsageError: nothing was asked of the server yet, so this is
// the caller's mistake to fix.
func TestMissingKeyIsAUsageError(t *testing.T) {
	h := newHarness(t)
	delete(h.env, "TRACEPAD_API_KEY")
	got := h.run(t.Context(), false, "traces", "ls")
	if got.code != ExitUsage {
		t.Fatalf("exit = %d, want a usage error", got.code)
	}
	if !strings.Contains(got.stderr, "TRACEPAD_API_KEY") {
		t.Errorf("stderr = %q, want it to name what is missing", got.stderr)
	}
}

// TestVersionSkewWarns: a mismatch is reported and the command still runs
// (edge cases).
func TestVersionSkewWarns(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)

	var out, errOut bytes.Buffer
	code := Run(t.Context(), Options{
		Args:    []string{"traces", "ls"},
		Version: "0.9.0-from-the-future",
		Stdout:  &out,
		Stderr:  &errOut,
		Env:     func(key string) string { return h.env[key] },
	})
	if code != ExitOK {
		t.Fatalf("exit = %d, want the command to still run: %s", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "warning") || !strings.Contains(errOut.String(), testVersion) {
		t.Fatalf("stderr = %q, want a warning naming both builds", errOut.String())
	}
}

func TestStatsAndSystem(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)
	ctx := t.Context()

	stats := h.run(ctx, true, "stats", "--group-by", "model")
	if stats.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", stats.code, stats.stderr)
	}
	// The header names the unit, because a count of observations and a
	// count of traces are not comparable (Decision 23).
	if !strings.Contains(stats.stdout, "MODEL") || !strings.Contains(stats.stdout, "OBSERVATIONS") {
		t.Errorf("stats table = %s, want it to name the grouping and the unit", stats.stdout)
	}
	if !strings.Contains(stats.stdout, "claude-sonnet-5") {
		t.Errorf("stats = %s, want the model that was used", stats.stdout)
	}

	system := h.run(ctx, true, "system")
	if system.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", system.code, system.stderr)
	}
	// The endpoint map does not advertise /mcp, so `system` is where a
	// human finds out whether it is being served (Decision 27).
	for _, fragment := range []string{"tracepad test", "database", "rows", "traces", "mcp"} {
		if !strings.Contains(system.stdout, fragment) {
			t.Errorf("system output is missing %q:\n%s", fragment, system.stdout)
		}
	}
}

// TestSessionsListBothModes is the golden pair for the listing spec 007 adds:
// a table on a terminal, the endpoint's own bytes in a pipe.
func TestSessionsListBothModes(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)
	// A second session, more recently active than `s1`, so the ordering and
	// the "no cost reported" column both have something to show.
	h.seed(t, &model.Trace{ID: traceHex(3), Name: "batch", SessionID: "s2", Environment: "staging"},
		&model.Observation{TraceID: traceHex(3), ID: spanHex(4), Type: model.TypeSpan,
			Level: model.LevelError, StartTime: seedBase - 60*1000*ms, EndTime: seedBase - 59*1000*ms})
	ctx := t.Context()

	t.Run("terminal", func(t *testing.T) {
		got := h.run(ctx, true, "sessions", "ls")
		if got.code != ExitOK {
			t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
		}
		want := strings.Join([]string{
			"LAST SEEN            SESSION  TRACES  ERRORS  COST       FIRST SEEN",
			"2026-08-31 23:59:00  s2       1       1       -          2026-08-31 23:59:00",
			"2026-08-31 23:00:00  s1       1       0       $0.001000  2026-08-31 23:00:00",
			"",
		}, "\n")
		if got.stdout != want {
			t.Fatalf("table output:\n%s\nwant:\n%s", got.stdout, want)
		}
	})

	t.Run("pipe", func(t *testing.T) {
		got := h.run(ctx, false, "sessions", "ls", "--env", "staging")
		if got.code != ExitOK {
			t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
		}
		var body struct {
			Sessions []struct {
				ID         string `json:"id"`
				TraceCount int    `json:"trace_count"`
			} `json:"sessions"`
		}
		if err := json.Unmarshal([]byte(got.stdout), &body); err != nil {
			t.Fatalf("piped output is not JSON: %v (%s)", err, got.stdout)
		}
		if len(body.Sessions) != 1 || body.Sessions[0].ID != "s2" {
			t.Fatalf("sessions = %+v, want only the staging one", body.Sessions)
		}
	})

	t.Run("empty", func(t *testing.T) {
		got := h.run(ctx, true, "sessions", "ls", "--user", "nobody")
		if got.code != ExitOK || !strings.Contains(got.stdout, "no sessions") {
			t.Fatalf("sessions ls = %+v, want an honest empty answer", got)
		}
	})
}

func TestSessionsAndScores(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)
	ctx := t.Context()

	session := h.run(ctx, true, "sessions", "show", "s1")
	if session.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", session.code, session.stderr)
	}
	if !strings.Contains(session.stdout, "session s1") || !strings.Contains(session.stdout, "support-chat") {
		t.Errorf("session output = %s", session.stdout)
	}

	empty := h.run(ctx, true, "scores", "ls")
	if empty.code != ExitOK || !strings.Contains(empty.stdout, "no scores") {
		t.Fatalf("scores = %+v, want an honest empty answer", empty)
	}
}

func TestPromptsRoundTrip(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	file := filepath.Join(t.TempDir(), "prompt.json")
	writeFile(t, file, `{"type":"text","prompt":"Be brief.","config":{"temperature":0.2}}`)
	push := h.run(ctx, true, "prompts", "push", "support", "--file", file, "--label", "production")
	if push.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", push.code, push.stderr)
	}
	if !strings.Contains(push.stdout, "support version 1 created") {
		t.Fatalf("push said %q", push.stdout)
	}

	writeFile(t, file, `{"prompt":"Be brief and cite the source."}`)
	if got := h.run(ctx, true, "prompts", "push", "support", "--file", file); got.code != ExitOK {
		t.Fatalf("second push: exit = %d, stderr = %s", got.code, got.stderr)
	}

	list := h.run(ctx, true, "prompts", "ls")
	if !strings.Contains(list.stdout, "production=1") {
		t.Errorf("prompts ls = %s, want the label and where it points", list.stdout)
	}

	get := h.run(ctx, true, "prompts", "get", "support", "--label", "production")
	if !strings.Contains(get.stdout, "support version 1") || !strings.Contains(get.stdout, "Be brief.") {
		t.Errorf("prompts get = %s", get.stdout)
	}

	diff := h.run(ctx, true, "prompts", "diff", "support", "--from", "1", "--to", "2")
	if !strings.Contains(diff.stdout, "+\"Be brief and cite the source.\"") {
		t.Errorf("prompts diff = %s", diff.stdout)
	}

	both := h.run(ctx, true, "prompts", "get", "support", "--label", "production", "--version", "2")
	if both.code != ExitUsage {
		t.Fatalf("exit = %d, want a usage error for two selectors", both.code)
	}
}
