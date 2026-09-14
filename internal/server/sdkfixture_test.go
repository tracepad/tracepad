package server

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/tracepad/tracepad/internal/store"
)

// Our own packages' exports, through the OTLP path end to end (spec 017 #16,
// spec 032 #12). The bodies are not synthetic: `scripts/fixtures` writes each
// one from the package's own exporter, so a rule the mapper dropped or a key
// a package renamed fails here — on the rows a person would read — rather
// than in a user's store. One table for every package, so that the same
// facts are asserted of each and a fourth language is a row.
func TestOurPackagesExportsLandWhole(t *testing.T) {
	type generation struct {
		name, model, promptName string
		promptVersion           int64
		inputTokens, cacheRead  float64
		cost                    float64
	}
	cases := []struct {
		fixture      string
		trace        string
		name, user   string
		session      string
		release      string
		observations int
		warned       string // the observation stamped WARNING with a status message
		generation   generation
	}{
		{
			fixture: "010-tracepad-sdk", trace: "a0b1c2d3e4f5a6b7c8d9ea0000000001",
			name: "docs-chat", user: "user-9001", session: "session-91", release: "2026.9.4",
			observations: 4, warned: "docs-search",
			generation: generation{
				name: "chat-completion", model: "claude-sonnet-5", promptName: "tracepad-answer",
				promptVersion: 3, inputTokens: 128, cacheRead: 96, cost: 0.0011,
			},
		},
		{
			fixture: "013-tracepad-sdk-js", trace: "a0b1c2d3e4f5a6b7c8d9eb0000000001",
			name: "help-chat", user: "user-9001", session: "session-91", release: "2026.9.14",
			observations: 4, warned: "docs-search",
			generation: generation{
				name: "chat-completion", model: "gpt-5-mini", promptName: "docs-answer",
				promptVersion: 4, inputTokens: 96, cacheRead: 64, cost: 0.0007,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			h := newHarness(t, nil, store.WriterOptions{})
			body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "otlp", tc.fixture+".pb"))
			if err != nil {
				t.Fatal(err)
			}
			expectStatus(t, h.post(t, "/v1/traces", body), http.StatusOK)

			trace, err := h.store.Trace(h.project.ID, tc.trace)
			if err != nil || trace == nil {
				t.Fatalf("trace = %v, err = %v", trace, err)
			}
			if trace.Name != tc.name || trace.UserID != tc.user || trace.SessionID != tc.session {
				t.Errorf("trace = %q by %q in %q", trace.Name, trace.UserID, trace.SessionID)
			}
			if trace.Environment != "production" || trace.Release != tc.release {
				t.Errorf("environment = %q, release = %q", trace.Environment, trace.Release)
			}
			if trace.TotalCost == nil || *trace.TotalCost != tc.generation.cost {
				t.Errorf("total_cost = %v, want the cost the provider charged", trace.TotalCost)
			}

			observations, err := h.store.Observations(h.project.ID, tc.trace, store.WithIO)
			if err != nil {
				t.Fatal(err)
			}
			if len(observations) != tc.observations {
				t.Fatalf("observations = %d, want %d", len(observations), tc.observations)
			}
			named := func(name string) *store.ObservationRow {
				for _, o := range observations {
					if o.Name == name {
						return o
					}
				}
				t.Fatalf("no observation named %q in the export", name)
				return nil
			}
			if warned := named(tc.warned); warned.Level != "WARNING" || warned.StatusMessage == "" {
				t.Errorf("%s = level %q, status %q; want the WARNING update() wrote", tc.warned, warned.Level, warned.StatusMessage)
			}
			if event := named("cache.miss"); event.Type != "event" || event.StartTime != event.EndTime {
				t.Errorf("cache.miss = %s from %d to %d, want a zero-duration event", event.Type, event.StartTime, event.EndTime)
			}

			want := tc.generation
			got := named(want.name)
			if got.Type != "generation" || got.Model != want.model {
				t.Errorf("%s = %s on %q", want.name, got.Type, got.Model)
			}
			if got.Usage["input_tokens"] != want.inputTokens || got.Usage["cache_read_input_tokens"] != want.cacheRead {
				t.Errorf("usage = %v", got.Usage)
			}
			if !got.ProvidedCost || got.CostDetails["total"] != want.cost {
				t.Errorf("cost = %v (provided %v), want %v as charged", got.CostDetails, got.ProvidedCost, want.cost)
			}
			if got.PromptName != want.promptName || got.PromptVersion == nil || *got.PromptVersion != want.promptVersion {
				t.Errorf("prompt = %q@%v", got.PromptName, got.PromptVersion)
			}
			if got.CompletionStartTime <= got.StartTime || got.CompletionStartTime > got.EndTime {
				t.Errorf("completion_start_time = %d, want inside [%d, %d]", got.CompletionStartTime, got.StartTime, got.EndTime)
			}
			if got.Output != "When the last chunk does." && got.Output != "In the trace you are reading." {
				t.Errorf("output = %#v, want the answer's text as a string", got.Output)
			}
		})
	}
}
