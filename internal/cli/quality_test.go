package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/store"
)

// `scores trend` (spec 025 #8). Three clients, one surface: the command is the
// endpoint, so what `--json` prints is what `curl` gets, byte for byte.

// seedScores grades the corpus's first trace three ways, so the command has one
// series of each shape to render.
func seedScores(t *testing.T, h *harness) {
	t.Helper()
	value, verdict := 0.25, "pass"
	yes := 1.0
	if err := h.writer.Submit(t.Context(), &store.ScoreWrite{
		ProjectID: h.projectID(t),
		Scores: []*store.Score{
			{ID: traceHex(11), TraceID: traceHex(1), Name: "hallucination",
				DataType: store.ScoreNumeric, Value: &value,
				Timestamp: seedBase, CreatedAt: seedBase},
			{ID: traceHex(12), TraceID: traceHex(1), Name: "thumbs",
				DataType: store.ScoreBoolean, Value: &yes,
				Timestamp: seedBase, CreatedAt: seedBase},
			{ID: traceHex(13), TraceID: traceHex(1), Name: "verdict",
				DataType: store.ScoreCategorical, StringValue: &verdict,
				Timestamp: seedBase, CreatedAt: seedBase},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestScoresTrendRendersASeriesPerName(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)
	seedScores(t, h)

	got := h.run(t.Context(), true, "scores", "trend", "--group-by", "day")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	// One block per series, each headed by the name, the type and what was
	// counted — the three columns a reader needs to know what the number is.
	for _, want := range []string{
		"hallucination (numeric, any scores)",
		"MEAN (MIN..MAX)",
		"0.25 (0.25..0.25)",
		"thumbs (boolean, any scores)",
		"RATE",
		"100%",
		"verdict (categorical, any scores)",
		"CATEGORIES",
		"pass 1",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("output does not mention %q:\n%s", want, got.stdout)
		}
	}
}

// A truncated answer that said nothing about being truncated would be a wrong
// one, so the count comes out under the tables (spec 025 #24).
func TestScoresTrendSaysWhatTheLimitLeftOut(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)
	seedScores(t, h)

	got := h.run(t.Context(), true, "scores", "trend", "--limit", "1")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "2 rarer score names not shown; raise --limit to see them") {
		t.Errorf("output does not say what was left out:\n%s", got.stdout)
	}
	// And nothing at all when the whole answer fits.
	whole := h.run(t.Context(), true, "scores", "trend")
	if strings.Contains(whole.stdout, "not shown") {
		t.Errorf("a complete answer claims to be truncated:\n%s", whole.stdout)
	}
}

// A numeric score is not bounded to 0..1 — `output_tokens` and `latency_ms` are
// ordinary names — and the column has to read the way the interface's does
// (spec 025 #25). `ui/src/lib/api/quality.test.ts` asserts the same numbers of
// `figure`, which is the other half of this claim.
func TestAScoreValueReadsTheSameInBothClients(t *testing.T) {
	for _, tc := range []struct {
		value float64
		want  string
	}{
		{1.0 / 3.0, "0.333"},
		{0.25, "0.25"},
		{12.3456, "12.3"},
		{0, "0"},
		{1234.5, "1230"},
		{98765, "98800"},
		{-1234.5, "-1230"},
	} {
		if got := scoreFigure(tc.value); got != tc.want {
			t.Errorf("scoreFigure(%v) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

// A window the corpus does not reach says so rather than printing an empty
// table, which is the command line's half of the screen's empty state.
func TestScoresTrendSaysWhenNothingNamesATrace(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)

	got := h.run(t.Context(), true, "scores", "trend")
	if got.code != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "no score names a trace in this range") {
		t.Errorf("output = %q, want the empty state", got.stdout)
	}
}

// TestScoresTrendIsTheEndpoint: spec 004 #1 in one assertion — `--json` is the
// endpoint's own bytes, so the command line cannot tell a different story.
func TestScoresTrendIsTheEndpoint(t *testing.T) {
	h := newHarness(t)
	seedCorpus(t, h)
	seedScores(t, h)

	for _, tc := range []struct {
		name string
		args []string
		path string
	}{
		{"the default", []string{"scores", "trend"}, "/api/v1/stats/scores"},
		{"one name", []string{"scores", "trend", "--name", "hallucination"},
			"/api/v1/stats/scores?name=hallucination"},
		{"by model", []string{"scores", "trend", "--group-by", "model"},
			"/api/v1/stats/scores?group_by=model"},
		{"one environment", []string{"scores", "trend", "--env", "production"},
			"/api/v1/stats/scores?environment=production"},
		{"a limit", []string{"scores", "trend", "--limit", "2"},
			"/api/v1/stats/scores?limit=2"},
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
			var fromCommand, fromEndpoint any
			if err := json.Unmarshal([]byte(got.stdout), &fromCommand); err != nil {
				t.Fatalf("--json is not JSON: %v (%s)", err, got.stdout)
			}
			if err := json.NewDecoder(response.Body).Decode(&fromEndpoint); err != nil {
				t.Fatal(err)
			}
			a, _ := json.Marshal(fromCommand)
			b, _ := json.Marshal(fromEndpoint)
			if string(a) != string(b) {
				t.Errorf("the command and the endpoint disagree:\n%s\n%s", a, b)
			}
		})
	}
}

// An unknown subcommand names the four this noun has, which is how somebody
// finds `trend` without reading the help.
func TestScoresNamesItsFourVerbs(t *testing.T) {
	h := newHarness(t)
	got := h.run(t.Context(), true, "scores", "wat")
	if !strings.Contains(got.stderr, "ls, add, rm or trend") {
		t.Errorf("stderr = %q, want the four verbs", got.stderr)
	}
}
