package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/store"
)

// `users ls` and `users show` (spec 023 #7). Three clients, one surface: the
// commands are the endpoints, so what `--json` prints is what `curl` gets, byte
// for byte.

// rollUsers runs the job the aggregator submits for the hours the corpus sits
// in, so the listing — which answers from the rollup alone — has something in
// it (spec 023 #4).
func rollUsers(t *testing.T, h *harness, hours ...int64) {
	t.Helper()
	for _, hour := range hours {
		if err := h.writer.Submit(t.Context(),
			store.RollHour(h.projectID(t), hour, seedBase)); err != nil {
			t.Fatal(err)
		}
	}
}

// corpusHour is the hour `seedCorpus`'s first trace falls in.
const corpusHour = (seedBase - 3600*1000*ms) / 1e9 / 3600 * 3600

func TestUsersListRendersTheRollup(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)
	rollUsers(t, h, corpusHour)

	got := h.run(t.Context(), true, "users", "ls")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	for _, want := range []string{"USER", "SESSIONS", "TOKENS", "u1"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("output does not mention %q:\n%s", want, got.stdout)
		}
	}
	// The trace that named no user is not a user (spec 023 #1).
	if strings.Count(got.stdout, "\n") > 3 {
		t.Errorf("more rows than the one user the corpus has:\n%s", got.stdout)
	}
}

// TestUsersCommandsAreTheEndpoint: the whole of spec 004 #1 in one assertion —
// `--json` is the endpoint's own bytes, so a client cannot be told a different
// story by the command line.
func TestUsersCommandsAreTheEndpoint(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)
	rollUsers(t, h, corpusHour)

	for _, tc := range []struct {
		name string
		args []string
		path string
	}{
		{"users ls", []string{"users", "ls"}, "/api/v1/users"},
		{"users ls --sort", []string{"users", "ls", "--sort", "cost"}, "/api/v1/users?sort=cost"},
		{"users ls --sort tokens", []string{"users", "ls", "--sort", "tokens"}, "/api/v1/users?sort=tokens"},
		{"users show", []string{"users", "show", "u1"}, "/api/v1/users/u1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := h.run(t.Context(), false, append(tc.args, "--json")...)
			if got.code != ExitOK {
				t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
			}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.url+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+testKey)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var endpoint, printed any
			if err := json.NewDecoder(response.Body).Decode(&endpoint); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(got.stdout), &printed); err != nil {
				t.Fatalf("the command did not print JSON: %v\n%s", err, got.stdout)
			}
			a, _ := json.Marshal(endpoint)
			b, _ := json.Marshal(printed)
			if string(a) != string(b) {
				t.Errorf("the command and the endpoint disagree:\n%s\n%s", a, b)
			}
		})
	}
}

func TestUsersShowRendersTheSummary(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)
	rollUsers(t, h, corpusHour)

	got := h.run(t.Context(), true, "users", "show", "u1")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	// Tokens are input plus output, with every other class reported beside
	// them (spec 049 #11): the corpus's generation sent 128 in and 41 out.
	for _, want := range []string{"user u1", "traces", "sessions", "latency", "window", "tokens    169\n"} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("output does not report %q:\n%s", want, got.stdout)
		}
	}
}

// TestStatsByUserAddsTheSessionsColumn (spec 023 #6): the column appears with
// `--user` and a timeline, and not otherwise.
func TestStatsByUserAddsTheSessionsColumn(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)

	with := h.run(t.Context(), true, "stats", "--group-by", "hour", "--user", "u1")
	if with.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", with.code, with.stderr)
	}
	if !strings.Contains(with.stdout, "SESSIONS") {
		t.Errorf("--user hides the sessions column:\n%s", with.stdout)
	}
	without := h.run(t.Context(), true, "stats", "--group-by", "hour")
	if strings.Contains(without.stdout, "SESSIONS") {
		t.Errorf("the sessions column appears without --user:\n%s", without.stdout)
	}
	// And the filter is a filter: the corpus has a second trace under no
	// user at all, which the filtered answer must not count.
	if strings.Contains(with.stdout, "nightly-eval") {
		t.Errorf("the filtered stats mention another user's trace:\n%s", with.stdout)
	}
}

func TestUsersRejectsAnUnknownSubcommand(t *testing.T) {
	h := newHarness(t)
	got := h.run(t.Context(), false, "users", "rm")
	if got.code != ExitUsage {
		t.Fatalf("exit = %d, want %d (stderr: %s)", got.code, ExitUsage, got.stderr)
	}
	if !strings.Contains(got.stderr, "ls, show, rm-data, erasure or erasures") {
		t.Errorf("stderr = %q, want it to name the five subcommands", got.stderr)
	}
}

// The tree's `N tokens` reads the classes every listing reads (spec 049 #11):
// an OpenAI-style usage with no `total` shows input plus output, and a usage
// that names no class still shows the total it sent.
func TestTreeTokensReadTheClasses(t *testing.T) {
	for _, tc := range []struct {
		usage map[string]any
		want  string
	}{
		{map[string]any{"prompt_tokens": 120.0, "completion_tokens": 30.0, "total_tokens": 150.0}, "150 tokens"},
		{map[string]any{"input_tokens": 10.0, "output_tokens": 5.0, "reasoning_tokens": 400.0}, "15 tokens"},
		{map[string]any{"input": 128.0, "output": 41.0, "total": 999.0}, "169 tokens"},
		{map[string]any{"total_tokens": 77.0}, "77 tokens"},
		{map[string]any{"cache_read_input_tokens": 9.0}, ""},
		{nil, ""},
	} {
		if got := totalTokens(tc.usage); got != tc.want {
			t.Errorf("totalTokens(%v) = %q, want %q", tc.usage, got, tc.want)
		}
	}
}

// `--min-tokens` is the endpoint's `min_tokens`, passed through as typed.
func TestMinTokensIsTheEndpoint(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)
	for flag, rows := range map[string]int{"169": 1, "170": 0} {
		got := h.run(t.Context(), false, "traces", "ls", "--min-tokens", flag, "--json")
		if got.code != ExitOK {
			t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
		}
		var listing struct {
			Traces []json.RawMessage `json:"traces"`
		}
		if err := json.Unmarshal([]byte(got.stdout), &listing); err != nil {
			t.Fatal(err)
		}
		if len(listing.Traces) != rows {
			t.Errorf("--min-tokens %s listed %d traces, want %d", flag, len(listing.Traces), rows)
		}
	}
	bad := h.run(t.Context(), false, "traces", "ls", "--min-tokens", "lots")
	if bad.code == ExitOK || !strings.Contains(bad.stderr, "min_tokens") {
		t.Errorf("--min-tokens lots: exit %d, stderr %s", bad.code, bad.stderr)
	}
}
