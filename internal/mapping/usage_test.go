package mapping_test

import (
	"testing"

	"github.com/tracepad/tracepad/internal/mapping"
	"github.com/tracepad/tracepad/internal/otlptest"
)

// The third usage source (spec 030 #1): the bare token keys Claude Code, the
// Anthropic SDK's own `usage` object and OpenAI-style clients put straight on
// a span. They are a *fallback*, so every test here is as much about when they
// are not read as about when they are.

// Bare keys alone are the usage, stored under the key as sent — and claimed,
// which is what takes them out of metadata.
//
// The four counts arrive here as decimal *strings*, which is the shape this
// source has to survive: `asNumber` has read ints, doubles and decimal strings
// alike since spec 002, and an exporter that stringifies its attributes is not
// sending a different fact. Fixture 012 is the same four keys as ints.
func TestBareUsageKeys(t *testing.T) {
	result := mapping.Map(otlptest.SpanWith(
		"model", "claude-haiku-4-5-20251001",
		"input_tokens", "10",
		"output_tokens", "118",
		"cache_read_tokens", "17580",
		"cache_creation_tokens", "10434",
	))
	observation := result.Observations[0]

	want := map[string]int64{
		"input_tokens": 10, "output_tokens": 118,
		"cache_read_tokens": 17580, "cache_creation_tokens": 10434,
	}
	if len(observation.Usage) != len(want) {
		t.Fatalf("usage = %v, want the four counts the span carried", observation.Usage)
	}
	for key, count := range want {
		if observation.Usage[key] != count {
			t.Errorf("usage[%q] = %#v, want %d under the key as sent", key, observation.Usage[key], count)
		}
		if _, stillThere := observation.Metadata[key]; stillThere {
			t.Errorf("metadata still holds %q: a key carried into usage is claimed", key)
		}
	}
}

// A bare word is the likeliest key to mean something else entirely, so a value
// that is not a number is left where it was rather than coerced.
func TestBareUsageIgnoresNonNumericValues(t *testing.T) {
	result := mapping.Map(otlptest.SpanWith(
		"input_tokens", "many",
		"output_tokens", "118",
	))
	observation := result.Observations[0]

	if _, counted := observation.Usage["input_tokens"]; counted {
		t.Errorf("usage = %v, want `many` refused rather than coerced", observation.Usage)
	}
	if observation.Metadata["input_tokens"] != "many" {
		t.Errorf("metadata = %v, want the refused value preserved (spec 002 #11)", observation.Metadata)
	}
	if observation.Usage["output_tokens"] != int64(118) {
		t.Errorf("usage = %v, want the neighbouring count still read", observation.Usage)
	}
}

// `total_tokens` on its own is a usage: one number is what some clients send,
// and a source that needed a pair would answer nothing for them.
func TestBareUsageTotalAlone(t *testing.T) {
	observation := mapping.Map(otlptest.SpanWith("total_tokens", "169")).Observations[0]
	if len(observation.Usage) != 1 || observation.Usage["total_tokens"] != int64(169) {
		t.Errorf("usage = %v, want the lone total", observation.Usage)
	}
}

// Not every `*_tokens` attribute is a token count. The list in rules.go is
// closed on purpose (spec 030 #2), and a spelling outside it stays in
// metadata where a reader can still find it.
func TestBareUsageListIsClosed(t *testing.T) {
	observation := mapping.Map(otlptest.SpanWith("thinking_tokens", "64")).Observations[0]
	if observation.Usage != nil {
		t.Errorf("usage = %v, want nothing: `thinking_tokens` is not in the list", observation.Usage)
	}
	if observation.Metadata["thinking_tokens"] != "64" {
		t.Errorf("metadata = %v, want the unread attribute preserved", observation.Metadata)
	}
}

// Precedence, both steps of it (spec 030 #1). An exporter sending a standard
// spelling *and* a bare one is describing one number twice; the standard
// spelling wins whole and the bare keys stay unclaimed in metadata, so both
// readings remain visible and neither is merged into the other.
func TestBareUsageYieldsToTheTwoConventions(t *testing.T) {
	cases := []struct {
		name      string
		attrs     []string
		wantUsage map[string]any
	}{
		{
			name: "gen_ai.usage.* wins over the bare keys",
			attrs: []string{
				"gen_ai.usage.input_tokens", "92",
				"input_tokens", "10",
				"output_tokens", "118",
			},
			wantUsage: map[string]any{"input_tokens": int64(92)},
		},
		{
			name: "usage_details wins over both",
			attrs: []string{
				"langfuse.observation.usage_details", `{"input":128,"output":41}`,
				"gen_ai.usage.input_tokens", "92",
				"input_tokens", "10",
			},
			wantUsage: map[string]any{"input": float64(128), "output": float64(41)},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			observation := mapping.Map(otlptest.SpanWith(c.attrs...)).Observations[0]
			if len(observation.Usage) != len(c.wantUsage) {
				t.Fatalf("usage = %v, want %v — the sources are a chain, not a merge",
					observation.Usage, c.wantUsage)
			}
			for key, count := range c.wantUsage {
				if observation.Usage[key] != count {
					t.Errorf("usage[%q] = %#v, want %#v", key, observation.Usage[key], count)
				}
			}
			for _, bare := range []string{"input_tokens", "output_tokens"} {
				sent := false
				for i := 0; i < len(c.attrs); i += 2 {
					sent = sent || c.attrs[i] == bare
				}
				if _, kept := observation.Metadata[bare]; sent && !kept {
					t.Errorf("metadata = %v, want the losing %q still visible",
						observation.Metadata, bare)
				}
			}
		})
	}
}

// `gen_ai.usage.cost` is a price, not a count: it feeds the cost chain and
// leaves the usage source with nothing, so the bare keys are still reached.
// Reading it as "a `gen_ai.usage.*` is present" would cost a Claude Code span
// its four numbers the moment somebody's proxy stamped a cost on it.
func TestBareUsageIsReachedPastAGenAICost(t *testing.T) {
	observation := mapping.Map(otlptest.SpanWith(
		"gen_ai.usage.cost", "0.0012",
		"input_tokens", "10",
	)).Observations[0]

	if observation.Usage["input_tokens"] != int64(10) {
		t.Errorf("usage = %v, want the bare count: a cost is not a count", observation.Usage)
	}
	if observation.CostDetails == nil {
		t.Errorf("cost_details = %v, want the cost where it belongs", observation.CostDetails)
	}
}
