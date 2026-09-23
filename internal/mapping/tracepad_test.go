package mapping_test

import (
	"testing"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/otlptest"
)

// The `tracepad` dialect (spec 017 #3): every row of the Ingest contract, read
// from a span carrying nothing but our own keys.
func TestTracepadDialect(t *testing.T) {
	result := mapping.Map(otlptest.SpanWith(
		"tracepad.trace.name", "support-chat",
		"tracepad.trace.tags", `["support","beta"]`,
		"tracepad.trace.metadata", `{"channel":"web"}`,
		"tracepad.trace.version", "retrieval-v2",
		"tracepad.observation.type", "tool",
		"tracepad.observation.level", "warning",
		"tracepad.observation.status_message", "no results, retrying",
		"tracepad.observation.metadata", `{"attempt":2}`,
		"tracepad.observation.completion_start_time", "2026-08-26T10:00:00.488Z",
		"tracepad.prompt.name", "support-answer",
		"tracepad.prompt.version", "7",
		"user.id", "user-4821",
		"session.id", "session-77",
	))

	trace := result.Traces[0]
	if trace.Name != "support-chat" {
		t.Errorf("trace name = %q", trace.Name)
	}
	if len(trace.Tags) != 2 || trace.Tags[0] != "support" {
		t.Errorf("tags = %v", trace.Tags)
	}
	if trace.Metadata["channel"] != "web" {
		t.Errorf("trace metadata = %v", trace.Metadata)
	}
	if trace.UserID != "user-4821" || trace.SessionID != "session-77" {
		t.Errorf("user = %q, session = %q", trace.UserID, trace.SessionID)
	}
	if trace.Version != "retrieval-v2" {
		t.Errorf("version = %q", trace.Version)
	}

	observation := result.Observations[0]
	if observation.Type != "tool" {
		t.Errorf("type = %q", observation.Type)
	}
	if observation.Level != "WARNING" {
		t.Errorf("level = %q", observation.Level)
	}
	if observation.StatusMessage != "no results, retrying" {
		t.Errorf("status message = %q", observation.StatusMessage)
	}
	if observation.Metadata["attempt"] != float64(2) {
		t.Errorf("observation metadata = %v", observation.Metadata)
	}
	if observation.CompletionStartTime == 0 {
		t.Error("completion start was not read")
	}
	if observation.PromptName != "support-answer" {
		t.Errorf("prompt name = %q", observation.PromptName)
	}
	if observation.PromptVersion == nil || *observation.PromptVersion != 7 {
		t.Errorf("prompt version = %v", observation.PromptVersion)
	}
	// Nothing our own rules claimed may appear in metadata a second time.
	for key := range observation.Metadata {
		if key != "attempt" && key != "scope.name" && key != "scope.version" {
			t.Errorf("metadata carries %q, which a rule should have claimed", key)
		}
	}
}

// "Beside" means the same field, and the `langfuse.*` key wins: a span
// carrying both was written by two SDKs, and the older one is the one the
// operator configured first (spec 017 #3).
func TestLangfuseWinsOverTracepad(t *testing.T) {
	result := mapping.Map(otlptest.SpanWith(
		"langfuse.trace.name", "written-by-langfuse",
		"tracepad.trace.name", "written-by-tracepad",
		"langfuse.observation.type", "generation",
		"tracepad.observation.type", "tool",
		"langfuse.observation.level", "ERROR",
		"tracepad.observation.level", "DEBUG",
		"langfuse.observation.prompt.name", "langfuse-prompt",
		"tracepad.prompt.name", "tracepad-prompt",
	))

	if name := result.Traces[0].Name; name != "written-by-langfuse" {
		t.Errorf("trace name = %q", name)
	}
	observation := result.Observations[0]
	if observation.Type != "generation" {
		t.Errorf("type = %q", observation.Type)
	}
	if observation.Level != "ERROR" {
		t.Errorf("level = %q", observation.Level)
	}
	if observation.PromptName != "langfuse-prompt" {
		t.Errorf("prompt name = %q", observation.PromptName)
	}
	// The loser is still visible: nothing is dropped (spec 002 #11).
	if observation.Metadata["tracepad.trace.name"] != "written-by-tracepad" {
		t.Errorf("metadata = %v, want the loser preserved", observation.Metadata)
	}
}

// The trace's version is the one trace field the dialect missed (spec 038 #4):
// `tracepad.trace.version` sits beside `langfuse.version`, and the
// `langfuse.*` key wins on one span and across the spans of an export alike —
// the chain's rank, not the order of delivery (spec 012 #11).
func TestTracepadTraceVersionRanksBesideLangfuse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		spans [][]string
		want  string
		// The loser of the root span's own chain, which stays visible in its
		// metadata, as the trace name's does (TestLangfuseWinsOverTracepad):
		// nothing is dropped (spec 002 #11).
		loser string
	}{
		{"alone", [][]string{{"tracepad.trace.version", "arm-b"}}, "arm-b", ""},
		{"both on one span", [][]string{{
			"langfuse.version", "written-by-langfuse",
			"tracepad.trace.version", "written-by-tracepad",
		}}, "written-by-langfuse", "written-by-tracepad"},
		{"the langfuse key on the root, ours on a later span", [][]string{
			{"langfuse.version", "written-by-langfuse"},
			{"tracepad.trace.version", "written-by-tracepad"},
		}, "written-by-langfuse", ""},
		{"ours on the root, the langfuse key on a later span", [][]string{
			{"tracepad.trace.version", "written-by-tracepad"},
			{"langfuse.version", "written-by-langfuse"},
		}, "written-by-langfuse", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := mapping.Map(otlptest.ExportLevels(otlptest.Levels{
				ScopeName: "tracepad", ScopeVersion: "0.1.0", Spans: tc.spans,
			}))
			if got := result.Traces[0].Version; got != tc.want {
				t.Errorf("version = %q, want %q", got, tc.want)
			}
			if tc.loser != "" {
				if got := result.Observations[0].Metadata["tracepad.trace.version"]; got != tc.loser {
					t.Errorf("metadata = %v, want the loser preserved", result.Observations[0].Metadata)
				}
			}
		})
	}
}

// Both dialects' metadata is collected, and the higher-priority one wins the
// entries they both name.
func TestMetadataFromBothDialects(t *testing.T) {
	result := mapping.Map(otlptest.SpanWith(
		"langfuse.observation.metadata", `{"shared":"langfuse","only-langfuse":1}`,
		"tracepad.observation.metadata", `{"shared":"tracepad","only-tracepad":2}`,
	))

	metadata := result.Observations[0].Metadata
	if metadata["shared"] != "langfuse" {
		t.Errorf("shared = %v, want the langfuse value", metadata["shared"])
	}
	if metadata["only-langfuse"] != float64(1) || metadata["only-tracepad"] != float64(2) {
		t.Errorf("metadata = %v, want both dialects' entries", metadata)
	}
}

// The dialect label says which SDK wrote a batch, so that a later remap can
// find the bodies a changed rule affects.
func TestDialectLabel(t *testing.T) {
	cases := []struct {
		name string
		keys []string
		want string
	}{
		{"plain otel", []string{"http.method", "GET"}, mapping.DialectOTel},
		{"genai semconv", []string{"gen_ai.request.model", "gpt-4o-mini"}, mapping.DialectGenAI},
		{"tracepad", []string{"tracepad.observation.type", "tool"}, mapping.DialectTracepad},
		{
			"tracepad over genai",
			[]string{"gen_ai.request.model", "gpt-4o-mini", "tracepad.trace.name", "chat"},
			mapping.DialectTracepad,
		},
		{
			"langfuse over tracepad",
			[]string{"langfuse.trace.name", "chat", "tracepad.trace.name", "chat"},
			mapping.DialectLangfuse,
		},
		{
			// The run link is stamped by any harness over any SDK, so it says
			// nothing about who wrote the span.
			"the run link alone is not a dialect",
			[]string{"tracepad.run_id", "0e5a7c1d2b3f4a6980c1d2e3f4a5b6c7"},
			mapping.DialectOTel,
		},
		{
			"the run link beside genai",
			[]string{
				"tracepad.run_id", "0e5a7c1d2b3f4a6980c1d2e3f4a5b6c7",
				"gen_ai.request.model", "gpt-4o-mini",
			},
			mapping.DialectGenAI,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mapping.Map(otlptest.SpanWith(c.keys...)).Dialect; got != c.want {
				t.Errorf("dialect = %q, want %q", got, c.want)
			}
		})
	}
}

// A `tracepad.*` key on a span exported by a different SDK is claimed all the
// same: the dialect is a vocabulary, not a signature (spec 017, edge cases).
func TestTracepadKeysOnAnotherSDKsSpan(t *testing.T) {
	result := mapping.Map(otlptest.SpanWith(
		"gen_ai.request.model", "gpt-4o-mini",
		"tracepad.observation.level", "ERROR",
	))
	if level := result.Observations[0].Level; level != "ERROR" {
		t.Errorf("level = %q, want the tracepad key read", level)
	}
}
