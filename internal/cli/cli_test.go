package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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
	// observeFlags is handed every flag set the commands of this harness
	// build. The parity test sets it; it lives here rather than in a
	// package variable so that it cannot outlive the test that wanted it.
	observeFlags func(*flag.FlagSet)
	// retryBackoff shortens `export --otlp`'s first retry delay, so the
	// retry tests walk the real loop rather than a stubbed one and still
	// finish in milliseconds (spec 019 #6).
	retryBackoff time.Duration
}

func newHarness(t *testing.T) *harness { return newHarnessWithToken(t, "") }

// newHarnessWithoutRaw is the same server with TRACEPAD_STORE_RAW off: the
// deployment `export --otlp` has to refuse with a reason rather than with an
// empty result (spec 019, edge cases).
func newHarnessWithoutRaw(t *testing.T) *harness {
	return newHarnessConfigured(t, "", false)
}

// newHarnessWithToken is the same server with a cross-project admin token
// configured, which the administrative tests need and nothing else does.
func newHarnessWithToken(t *testing.T, token string) *harness {
	return newHarnessConfigured(t, token, true)
}

// newHarnessConfigured is the two knobs the harness has, in one place.
func newHarnessConfigured(t *testing.T, token string, storeRaw bool) *harness {
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

	cfg := &config.Config{Listen: ":0", StoreRaw: storeRaw, MaxBodyBytes: config.DefaultMaxBodyBytes,
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
	code := Run(ctx, h.options(&out, &errOut, tty, args))
	return result{stdout: out.String(), stderr: errOut.String(), code: code}
}

// options is one command's whole environment, shared by run and follow.
func (h *harness) options(stdout, stderr io.Writer, tty bool, args []string) Options {
	return Options{
		Args:    args,
		Version: testVersion,
		Stdout:  stdout,
		Stderr:  stderr,
		Stdin:   strings.NewReader(h.stdin),
		TTY:     tty,
		Env:     func(key string) string { return h.env[key] },
		// Set by the parity test only; nil everywhere else.
		observeFlags: h.observeFlags,
		retryBackoff: h.retryBackoff,
		Now:          func() time.Time { return time.Unix(0, seedBase).UTC() },
	}
}

// follow starts a command that runs until its context is cancelled. `tail` is
// the only such command, and it is the reason this exists: its assertions are
// about what has been printed *so far*, so the test can wait for the event it
// is about instead of sleeping for a duration that is usually long enough.
func (h *harness) follow(ctx context.Context, tty bool, args ...string) *following {
	out, errOut := &syncBuffer{}, &syncBuffer{}
	done := make(chan result, 1)
	go func() {
		code := Run(ctx, h.options(out, errOut, tty, args))
		done <- result{stdout: out.String(), stderr: errOut.String(), code: code}
	}()
	return &following{out: out, done: done}
}

// following is a command that is still running: what it has printed so far,
// and the result it will finish with once its context ends.
type following struct {
	out  *syncBuffer
	done <-chan result
}

// syncBuffer is an output buffer that can be read while it is being written.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// await blocks until the follow has printed want.
//
// The deadline is a backstop against a hang, not the thing being measured: on
// a healthy run this returns as soon as the poll that prints it lands, and on
// a loaded machine it waits as long as that takes rather than failing at a
// duration somebody guessed. That is the whole point — a sleep long enough for
// a laptop is a coin toss on a busy CI runner (INBOX, gate of PR #28).
//
// A follow that has *exited* is the other way this can end, and waiting out
// the backstop for it would be both slow and mute: whatever it printed is all
// there will be, and the reason it stopped is in the result. So the wait ends
// there and reports it, the way the timed version used to report the exit
// code of a command that had already returned.
func (f *following) await(t *testing.T, want, what string) {
	t.Helper()
	const backstop = 30 * time.Second
	expired := time.After(backstop)
	for !strings.Contains(f.out.String(), want) {
		select {
		case got := <-f.done:
			t.Fatalf("the follow stopped before %s appeared: exit = %d, stderr = %s\nit printed:\n%s",
				what, got.code, got.stderr, got.stdout)
		case <-expired:
			t.Fatalf("%s never appeared in %s; the follow printed:\n%s",
				what, backstop, f.out.String())
		default:
		}
		time.Sleep(time.Millisecond)
	}
}

// wait hands back the result of a follow whose context has been cancelled.
func (f *following) wait() result { return <-f.done }

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
			EndTime: seedBase - 3600*1000*ms + 780*ms,
			// The wait before the first token, so the ttft column of the
			// listing has something to show (spec 012, CLI contract).
			CompletionStartTime: seedBase - 3600*1000*ms + 300*ms,
			Usage:               map[string]any{"total": 169},
			CostDetails:         map[string]any{"total": 0.001},
			Input:               []any{map[string]any{"role": "user", "content": "how do I reset my password?"}}})
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
			"TIME                 ID                                NAME          ENV         OBS  ERR  LATENCY  TTFT   COST",
			"2026-08-31 23:50:00  " + traceHex(2) + "  nightly-eval  staging     1    1    1.5s     -      -",
			"2026-08-31 23:00:00  " + traceHex(1) + "  support-chat  production  2    0    820ms    300ms  $0.001000",
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

	// Each flag means the far end or a step from a cursor, and the two
	// combinations that would quietly mean the other one are refused.
	for _, args := range [][]string{
		{"traces", "ls", "--oldest", "--cursor", "whatever"},
		{"traces", "ls", "--newer"},
	} {
		got := h.run(ctx, true, args...)
		if got.code != ExitUsage {
			t.Errorf("%v exited %d, want a usage error", args, got.code)
		}
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

// TestAnEmptySessionPageSaysHowToLeave: `sessions ls` returned early on an
// empty page, skipping both the count and the way back — the same dead end
// `--oldest` had (PR #11, third review).
//
// The empty page has to be one a *cursor* led to, or there is no way back for
// the command to print and the test proves nothing (PR #11, fourth review).
func TestAnEmptySessionPageSaysHowToLeave(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)
	ctx := t.Context()

	counted := h.run(ctx, true, "sessions", "ls", "--env", "nowhere", "--total")
	if counted.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", counted.code, counted.stderr)
	}
	if !strings.Contains(counted.stdout, "no sessions") {
		t.Errorf("output does not say the page is empty:\n%s", counted.stdout)
	}
	if !strings.Contains(counted.stdout, "0 matching") {
		t.Errorf("an empty page skipped the count it was asked for:\n%s", counted.stdout)
	}

	// Now the case the early return actually broke: a cursor, and past it a
	// filter that matches nothing. A second session, so there is a cursor.
	h.seed(t, &model.Trace{ID: traceHex(3), Name: "eval", SessionID: "s2",
		Environment: "production"},
		&model.Observation{TraceID: traceHex(3), ID: spanHex(3), Type: model.TypeSpan,
			Level: model.LevelDefault, StartTime: seedBase - 7200*1000*ms,
			EndTime: seedBase - 7200*1000*ms + 10*ms})

	page := h.run(ctx, true, "sessions", "ls", "--limit", "1")
	marker := strings.Index(page.stdout, "older: --cursor ")
	if marker < 0 {
		t.Fatalf("no cursor to walk from:\n%s", page.stdout)
	}
	cursor := strings.Fields(page.stdout[marker:])[2]

	stranded := h.run(ctx, true, "sessions", "ls", "--cursor", cursor, "--env", "nowhere")
	if stranded.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", stranded.code, stranded.stderr)
	}
	if !strings.Contains(stranded.stdout, "newer: --newer --cursor ") {
		t.Errorf("an empty page reached by a cursor is a dead end:\n%s", stranded.stdout)
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
		// The unit the synopsis used to offer (spec 025 #26). The refusal
		// says which unit is missing and what the value is in hours, because
		// "takes a duration (1h, 30m)" leaves both to the reader.
		{"a day unit", []string{"scores", "trend", "--since", "30d"}, ExitUsage, "--since has no day unit: 30d is 720h"},
		// The number is there to be retyped, so it is never an exponent
		// (spec 025 #25, and #26's own review).
		{"a day unit too big for %g", []string{"scores", "trend", "--since", "100000d"}, ExitUsage, "--since has no day unit: 100000d is 2400000h"},
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

// TestTheSynopsisSpellsWindowsTheParserTakes: the synopsis is where these
// flags are learned, and `scores trend [--since 30d]` offered a unit Go's
// duration parser does not have (spec 025 #26) — an example copied straight
// out of the help text exited 2. So every window the help text spells goes
// through the parser that will read it.
func TestTheSynopsisSpellsWindowsTheParserTakes(t *testing.T) {
	// `T` and `<timestamp>` are the placeholders for an instant; anything
	// else after these two flags is a literal somebody will type.
	windows := regexp.MustCompile(`--(?:since|until)[ =]([^\] \n]+)`).FindAllStringSubmatch(Usage, -1)
	if len(windows) == 0 {
		t.Fatal("no --since or --until in the synopsis: the pattern has gone stale")
	}
	r := &run{opt: Options{Now: time.Now}}
	for _, found := range windows {
		value := found[1]
		if value == "T" {
			continue
		}
		if _, err := r.instant("--since", value); err != nil {
			t.Errorf("the synopsis spells %q, which the parser refuses: %v", value, err)
		}
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

// TestEmptyCursorIsRefused: `--cursor "$NEXT"` with nothing in NEXT is a
// script that has lost its place. Dropping the parameter answers it with the
// newest page, so a loop over the pages silently restarts and never ends —
// the reinterpretation spec 003 #23 refuses, on the parameter the walk is made
// of (found in review of PR #27).
//
// All four listings, because the flag means the same thing on each of them and
// a rule that held on one would be the next thing somebody found missing.
func TestEmptyCursorIsRefused(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)

	for _, command := range [][]string{
		{"traces", "ls"},
		{"sessions", "ls"},
		{"scores", "ls"},
		{"prompts", "ls"},
	} {
		name := strings.Join(command, " ")
		t.Run(name, func(t *testing.T) {
			out := h.run(t.Context(), true, append(command, "--cursor", "")...)
			if out.code != ExitUsage {
				t.Fatalf("%s --cursor \"\" exited %d, want %d: %s",
					name, out.code, ExitUsage, out.stderr)
			}
			if !strings.Contains(out.stderr, "--cursor") {
				t.Errorf("stderr = %q, want it to name the flag that came empty", out.stderr)
			}
			// Passing no cursor at all still means the first page.
			if out := h.run(t.Context(), true, command...); out.code != ExitOK {
				t.Fatalf("%s exited %d: %s", name, out.code, out.stderr)
			}
		})
	}
}

// TestScoresListPages is the same walk as TestPromptsListPages on the other
// listing that could reach only its first page. The hint says `older` here:
// scores come back newest first and the cursor is a timestamp, so the next
// page really is older — but there is one line and not two, because the
// endpoint has no `direction` to walk back with.
func TestScoresListPages(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	var want []string
	scores := make([]*store.Score, 0, 5)
	for i := 1; i <= 5; i++ {
		value := float64(i) / 10
		name := fmt.Sprintf("s%02d", i)
		// Newest first, so the walk meets them in reverse.
		want = append([]string{name}, want...)
		scores = append(scores, &store.Score{
			ID: traceHex(100 + i), TraceID: traceHex(1), Name: name,
			DataType: store.ScoreNumeric, Value: &value,
			Timestamp: seedBase + int64(i)*ms, CreatedAt: seedBase + int64(i)*ms,
		})
	}
	err := h.writer.Submit(ctx, &store.ScoreWrite{ProjectID: h.projectID(t), Scores: scores})
	if err != nil {
		t.Fatal(err)
	}

	var seen []string
	cursor := ""
	for page := 1; ; page++ {
		if page > len(want) {
			t.Fatalf("the walk did not end after %d pages; seen %v", page, seen)
		}
		args := []string{"scores", "ls", "--limit", "2"}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		out := h.run(ctx, true, args...)
		if out.code != ExitOK {
			t.Fatalf("page %d exited %d: %s", page, out.code, out.stderr)
		}
		cursor = ""
		for _, line := range strings.Split(out.stdout, "\n") {
			if after, found := strings.CutPrefix(line, "older: --cursor "); found {
				cursor = after
				continue
			}
			if fields := strings.Fields(line); len(fields) > 2 &&
				strings.HasPrefix(fields[2], "s0") {
				seen = append(seen, fields[2])
			}
		}
		if cursor == "" {
			break
		}
	}

	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("the walk saw %v, want every score once, newest first: %v", seen, want)
	}
}

// TestScoresAddAndRemove is spec 022 #7 on the command line: every data type
// goes in through `add`, comes back out through `ls`, and `rm` takes it away
// again. The interface may do nothing this cannot, so the write half the
// screens gained has to be here first.
func TestScoresAddAndRemove(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	trace := traceHex(1)

	cases := []struct {
		name string
		args []string
		want string
	}{
		{"numeric", []string{"--trace", trace, "--name", "helpfulness", "--value", "0.9"}, "0.9"},
		{"boolean", []string{"--trace", trace, "--name", "grounded", "--type", "boolean", "--value", "1"}, "1"},
		{"categorical", []string{"--trace", trace, "--name", "tone", "--type", "categorical", "--string", "friendly"}, "friendly"},
		{"text", []string{"--trace", trace, "--name", "verdict", "--string", "cites its sources"}, "cites its sources"},
		{"on an observation", []string{"--trace", trace, "--observation", spanHex(1), "--name", "step-ok", "--value", "1"}, "1"},
		{"on a session", []string{"--session", "s1", "--name", "csat", "--value", "5"}, "5"},
	}

	ids := map[string]string{}
	for _, one := range cases {
		out := h.run(ctx, true, append([]string{"scores", "add"}, one.args...)...)
		if out.code != ExitOK {
			t.Fatalf("%s: exit = %d, stderr = %s", one.name, out.code, out.stderr)
		}
		id := strings.TrimSpace(out.stdout)
		if len(id) != 32 {
			t.Fatalf("%s: printed %q, want the id the server assigned", one.name, id)
		}
		ids[one.name] = id
	}

	listed := h.run(ctx, true, "scores", "ls", "--limit", "20")
	if listed.code != ExitOK {
		t.Fatalf("scores ls exited %d: %s", listed.code, listed.stderr)
	}
	for _, one := range cases {
		if !strings.Contains(listed.stdout, one.want) {
			t.Errorf("%s: the listing does not carry %q:\n%s", one.name, one.want, listed.stdout)
		}
	}
	// The session score is on the session and reachable by it alone.
	bySession := h.run(ctx, true, "scores", "ls", "--session", "s1")
	if !strings.Contains(bySession.stdout, "csat") {
		t.Errorf("scores ls --session s1 = %s, want the session's own score", bySession.stdout)
	}

	// A correction is a re-post with the id (spec 003 #3), which is what the
	// dialog's edit does too.
	fixed := h.run(ctx, true, "scores", "add", "--trace", trace, "--name", "helpfulness",
		"--value", "0.4", "--id", ids["numeric"], "--comment", "on reflection")
	if fixed.code != ExitOK || strings.TrimSpace(fixed.stdout) != ids["numeric"] {
		t.Fatalf("re-post = %+v, want the same id back", fixed)
	}
	again := h.run(ctx, true, "scores", "ls", "--name", "helpfulness")
	if strings.Contains(again.stdout, "0.9") || !strings.Contains(again.stdout, "on reflection") {
		t.Errorf("the correction did not replace the row:\n%s", again.stdout)
	}

	removed := h.run(ctx, true, "scores", "rm", ids["numeric"])
	if removed.code != ExitOK || !strings.Contains(removed.stdout, ids["numeric"]) {
		t.Fatalf("scores rm = %+v, want the id it took", removed)
	}
	gone := h.run(ctx, true, "scores", "ls", "--name", "helpfulness")
	if !strings.Contains(gone.stdout, "no scores") {
		t.Errorf("after rm the listing still says:\n%s", gone.stdout)
	}
	// A second removal is the server's 404, not a cheerful nothing.
	twice := h.run(ctx, true, "scores", "rm", ids["numeric"])
	if twice.code != ExitFailure || !strings.Contains(twice.stderr, "not found") {
		t.Errorf("a second rm = %+v, want the 404 reported", twice)
	}
}

// The two flags that carry the value are exclusive and one of them is
// required, and an empty `--string` is a text score of the empty string rather
// than a missing value (mutations).
func TestScoresAddValueFlags(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()
	trace := traceHex(1)

	for _, args := range [][]string{
		{"scores", "add", "--trace", trace, "--name", "n"},
		{"scores", "add", "--trace", trace, "--name", "n", "--value", "1", "--string", "x"},
		{"scores", "add", "--trace", trace, "--name", "n", "--value", "high"},
	} {
		if out := h.run(ctx, true, args...); out.code != ExitUsage {
			t.Errorf("%v exited %d, want %d: %s", args, out.code, ExitUsage, out.stderr)
		}
	}

	empty := h.run(ctx, true, "scores", "add", "--trace", trace, "--name", "note", "--string", "")
	if empty.code != ExitOK {
		t.Fatalf("--string \"\" exited %d: %s", empty.code, empty.stderr)
	}
}

// TestPromptsListPages walks the whole listing a page at a time, which is what
// `--cursor` is for: without it the command could reach only the first page,
// and a `--limit` raised until everything fits is not pagination.
//
// The hint says `more` and not `older`: this listing is alphabetical, and the
// endpoint has no `direction`, so there is no far end and nothing true to say
// about time. The test reads the hint rather than assuming it, because a hint
// nobody can paste back is worse than none.
func TestPromptsListPages(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	var want []string
	for i := 1; i <= 5; i++ {
		name := fmt.Sprintf("p%02d", i)
		want = append(want, name)
		err := h.writer.Submit(ctx, &store.PromptVersionWrite{
			ProjectID: h.projectID(t), Name: name,
			Type: store.PromptText, TypeStated: true,
			Prompt: []byte(`"Be brief."`), CreatedAt: seedBase,
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	var seen []string
	cursor := ""
	for page := 1; ; page++ {
		if page > len(want) {
			t.Fatalf("the walk did not end after %d pages; seen %v", page, seen)
		}
		args := []string{"prompts", "ls", "--limit", "2"}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		out := h.run(ctx, true, args...)
		if out.code != ExitOK {
			t.Fatalf("page %d exited %d: %s", page, out.code, out.stderr)
		}
		cursor = ""
		for _, line := range strings.Split(out.stdout, "\n") {
			switch {
			case strings.HasPrefix(line, "more: --cursor "):
				cursor = strings.TrimPrefix(line, "more: --cursor ")
			case strings.HasPrefix(line, "p0"):
				seen = append(seen, strings.Fields(line)[0])
			}
		}
		if strings.Contains(out.stdout, "older:") {
			t.Errorf("page %d offered an older page:\n%s", page, out.stdout)
		}
		if cursor == "" {
			break
		}
	}

	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("the walk saw %v, want every prompt once, in order: %v", seen, want)
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

// `--expect` is the optimistic append from the command line (spec 021 #14):
// a push that means "add to the name as I last saw it" says so, and a name
// that moved under it is a refusal rather than a silent extension.
func TestPromptsPushExpect(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	file := filepath.Join(t.TempDir(), "prompt.json")
	writeFile(t, file, `{"type":"text","prompt":"Be brief."}`)
	if got := h.run(ctx, true, "prompts", "push", "support", "--file", file, "--expect", "0"); got.code != ExitOK {
		t.Fatalf("--expect 0 on a new name = %+v", got)
	}
	// The same push again believes the name is new, and it is not.
	again := h.run(ctx, true, "prompts", "push", "support", "--file", file, "--expect", "0")
	if again.code != ExitFailure || !strings.Contains(again.stderr, "already exists, at version 1") {
		t.Errorf("--expect 0 on a name that exists = %+v, want the server's 409", again)
	}
	// And a stale expectation names what it is actually at.
	stale := h.run(ctx, true, "prompts", "push", "support", "--file", file, "--expect", "7")
	if stale.code != ExitFailure || !strings.Contains(stale.stderr, "is at version 1, not 7") {
		t.Errorf("a stale --expect = %+v", stale)
	}
	// Without the flag the push appends, the way every push did before it.
	if got := h.run(ctx, true, "prompts", "push", "support", "--file", file); got.code != ExitOK ||
		!strings.Contains(got.stdout, "support version 2 created") {
		t.Errorf("a push with no --expect = %+v", got)
	}
}

// A label is created, moved and retired from the command line (spec 021 #8):
// the deploy path the interface offers may do nothing the CLI cannot.
func TestPromptsLabel(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	file := filepath.Join(t.TempDir(), "prompt.json")
	writeFile(t, file, `{"type":"text","prompt":"Be brief."}`)
	h.run(ctx, true, "prompts", "push", "support", "--file", file)
	writeFile(t, file, `{"prompt":"Be brief and cite the source."}`)
	h.run(ctx, true, "prompts", "push", "support", "--file", file)

	set := h.run(ctx, true, "prompts", "label", "support", "production", "--version", "1")
	if set.code != ExitOK || !strings.Contains(set.stdout, "production now points at version 1") {
		t.Fatalf("label --version 1 = %+v", set)
	}
	// Promote: the same command, a different number — no new version.
	moved := h.run(ctx, true, "prompts", "label", "support", "production", "--version", "2")
	if moved.code != ExitOK || !strings.Contains(moved.stdout, "version 2") {
		t.Fatalf("the move = %+v", moved)
	}
	if got := h.run(ctx, true, "prompts", "ls"); !strings.Contains(got.stdout, "production=2") {
		t.Errorf("prompts ls = %s, want the label where it was moved to", got.stdout)
	}

	// A removal reports the version the label had been on, which is what a
	// rollback reads.
	gone := h.run(ctx, true, "prompts", "label", "support", "production", "--rm")
	if gone.code != ExitOK || !strings.Contains(gone.stdout, "production removed from version 2") {
		t.Fatalf("--rm = %+v", gone)
	}

	for _, args := range [][]string{
		{"prompts", "label", "support", "production"},
		{"prompts", "label", "support", "production", "--version", "1", "--rm"},
		{"prompts", "label", "support"},
	} {
		if got := h.run(ctx, true, args...); got.code != ExitUsage {
			t.Errorf("%v = %+v, want a usage error", args, got)
		}
	}
	// The server's own refusals travel through unchanged.
	if got := h.run(ctx, true, "prompts", "label", "support", "latest", "--version", "1"); got.code != ExitFailure ||
		!strings.Contains(got.stderr, "reserved") {
		t.Errorf("`latest` as a label = %+v, want the server's refusal", got)
	}
	if got := h.run(ctx, true, "prompts", "label", "support", "production", "--version", "9"); got.code != ExitFailure {
		t.Errorf("a version that is not there = %+v, want the server's 404", got)
	}
}

// Deleting a name wears the ceremony every destructive command wears: the
// server's preview, and --yes off a terminal (spec 021 #7, #8).
func TestPromptsRemoveNeedsConfirmation(t *testing.T) {
	h := newHarness(t)
	ctx := t.Context()

	file := filepath.Join(t.TempDir(), "prompt.json")
	writeFile(t, file, `{"type":"text","prompt":"Be brief."}`)
	h.run(ctx, true, "prompts", "push", "support", "--file", file, "--label", "production")
	writeFile(t, file, `{"prompt":"Be brief and cite the source."}`)
	h.run(ctx, true, "prompts", "push", "support", "--file", file)

	refused := h.run(ctx, false, "prompts", "rm", "support")
	if refused.code != ExitFailure {
		t.Fatalf("rm without --yes = %+v, want a refusal", refused)
	}
	// The counts and the note, as the server sent them; the spacing between
	// a name and its number is the preview renderer's business.
	for _, want := range []*regexp.Regexp{
		regexp.MustCompile(`versions\s+2`),
		regexp.MustCompile(`labels\s+1`),
		regexp.MustCompile(`traces that ran this prompt keep`),
	} {
		if !want.MatchString(refused.stderr) {
			t.Errorf("the preview is missing %s:\n%s", want, refused.stderr)
		}
	}
	if got := h.run(ctx, true, "prompts", "ls"); !strings.Contains(got.stdout, "support") {
		t.Errorf("the prompt went without a confirmation")
	}

	done := h.run(ctx, true, "prompts", "rm", "support", "--yes")
	if done.code != ExitOK || !strings.Contains(done.stdout, "2 versions and 1 label gone") {
		t.Errorf("rm --yes = %+v", done)
	}
	if got := h.run(ctx, true, "prompts", "ls"); !strings.Contains(got.stdout, "no prompts") {
		t.Errorf("the prompt survived its deletion: %q", got.stdout)
	}
	if got := h.run(ctx, true, "prompts", "rm", "support", "--yes"); got.code != ExitFailure {
		t.Errorf("removing it twice = %+v, want the server's 404", got)
	}
}
