package server

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// Tokens per bucket (spec 031 #4, spec 049 #7): a `tokens` object with only
// the keys something in the bucket carried, and no object at all when none
// did.

type tokensJSON struct {
	Input      *int64 `json:"input"`
	Output     *int64 `json:"output"`
	CacheRead  *int64 `json:"cache_read"`
	Reasoning  *int64 `json:"reasoning"`
	CacheWrite *int64 `json:"cache_write"`
}

// String spells the five classes in order, NULL for an absent one.
func (t *tokensJSON) String() string {
	if t == nil {
		return "absent"
	}
	var parts []string
	for _, n := range []*int64{t.Input, t.Output, t.CacheRead, t.Reasoning, t.CacheWrite} {
		if n == nil {
			parts = append(parts, "NULL")
		} else {
			parts = append(parts, strconv.FormatInt(*n, 10))
		}
	}
	return "{" + strings.Join(parts, " ") + "}"
}

type tokensBucket struct {
	Key    string      `json:"key"`
	Count  int         `json:"count"`
	Tokens *tokensJSON `json:"tokens"`
}

func (h *harness) tokensBuckets(t *testing.T, path string) []tokensBucket {
	t.Helper()
	rec := h.get(t, path)
	expectStatus(t, rec, 200)
	return decodeJSON[struct {
		Buckets []tokensBucket `json:"buckets"`
	}](t, rec).Buckets
}

// seedTokens puts one trace in an hour with one generation carrying the
// given usage, or none.
func (h *harness) seedTokens(t *testing.T, hour int64, n int, environment, name string, usage map[string]any) {
	t.Helper()
	start := hour*int64(time.Second) + int64(n)*int64(time.Second)
	trace := &model.Trace{ID: traceHex(n), Environment: environment, UserID: "u1"}
	h.seed(t, trace, &model.Observation{
		TraceID: trace.ID, ID: spanHex(n), Type: model.TypeGeneration,
		Level: model.LevelDefault, Model: name,
		StartTime: start, EndTime: start + 100*ms, Usage: usage,
	})
}

func TestStatsTokens(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedTokens(t, statsHour, 1, "production", "claude-sonnet-5",
		map[string]any{"input_tokens": 100, "output_tokens": 10, "cache_read_input_tokens": 5})
	h.seedTokens(t, statsHour, 2, "production", "gpt-4o-mini",
		map[string]any{"prompt_tokens": 200, "completion_tokens": 20})
	h.seedTokens(t, statsHour, 3, "staging", "claude-sonnet-5", nil)

	check := func(t *testing.T) {
		t.Helper()
		byEnvironment := h.tokensBuckets(t, "/api/v1/stats?group_by=environment")
		if len(byEnvironment) != 2 {
			t.Fatalf("buckets = %+v, want both environments", byEnvironment)
		}
		production, staging := byEnvironment[0], byEnvironment[1]
		if got := production.Tokens.String(); got != "{300 30 5 NULL NULL}" {
			t.Errorf("production tokens = %s, want 300 in, 30 out, 5 cached", got)
		}
		// Nothing in staging carried a count: no object, not five zeroes.
		if staging.Tokens != nil {
			t.Errorf("staging tokens = %s, want the object absent", staging.Tokens)
		}

		byModel := h.tokensBuckets(t, "/api/v1/stats?group_by=model")
		if len(byModel) != 2 {
			t.Fatalf("buckets = %+v, want both models", byModel)
		}
		// Only the keys that have something: the OpenAI spelling carried no
		// cache-read count, so `cache_read` is absent on its bucket.
		mini := byModel[1]
		if mini.Key != "gpt-4o-mini" || mini.Tokens.String() != "{200 20 NULL NULL NULL}" {
			t.Errorf("gpt-4o-mini bucket = %s %s, want input and output only", mini.Key, mini.Tokens)
		}
	}

	t.Run("live", check)

	// The same answer from the rollup, and then across the seam: the rolled
	// hour with tokens beside a live hour without, which is the rolled sums
	// and not a zero (spec 031 #5).
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))
	t.Run("rolled", check)

	h.seedTokens(t, statsHour+3*3600, 4, "production", "claude-sonnet-5", nil)
	t.Run("straddling the seam", func(t *testing.T) {
		byEnvironment := h.tokensBuckets(t, "/api/v1/stats?group_by=environment")
		if len(byEnvironment) != 2 || byEnvironment[0].Count != 3 {
			t.Fatalf("buckets = %+v, want production with the live trace added", byEnvironment)
		}
		production := byEnvironment[0]
		if production.Tokens == nil || production.Tokens.Input == nil || *production.Tokens.Input != 300 {
			t.Errorf("production tokens = %s across the seam, want the rolled 300 in", production.Tokens)
		}
		byHour := h.tokensBuckets(t, "/api/v1/stats?group_by=hour")
		if len(byHour) != 2 || byHour[1].Tokens != nil {
			t.Errorf("hours = %+v, want the live hour without a tokens object", byHour)
		}
	})

	// A `user_id` answer carries tokens on every grouping since the per-user
	// rollup rolls them (spec 049 #5, amending spec 031 #11): the rolled hour
	// from `users_hourly`, the live one from the raw rows, and a bucket that
	// straddles the watermark adds the two.
	t.Run("for one user, across the seam", func(t *testing.T) {
		h.seedTokens(t, statsHour+3*3600, 5, "production", "claude-sonnet-5",
			map[string]any{"input_tokens": 7, "reasoning_tokens": 3})
		for groupBy, want := range map[string]map[string]string{
			"hour": {
				"2026-08-26T10:00:00Z": "{300 30 5 NULL NULL}",
				"2026-08-26T13:00:00Z": "{7 NULL NULL 3 NULL}",
			},
			"model": {
				"claude-sonnet-5": "{107 10 5 3 NULL}",
				"gpt-4o-mini":     "{200 20 NULL NULL NULL}",
			},
			"total": {"": "{307 30 5 3 NULL}"},
		} {
			buckets := h.tokensBuckets(t, "/api/v1/stats?group_by="+groupBy+"&user_id=u1")
			got := map[string]string{}
			for _, bucket := range buckets {
				got[bucket.Key] = bucket.Tokens.String()
			}
			for key, tokens := range want {
				if got[key] != tokens {
					t.Errorf("%s bucket %q = %s, want %s (all: %v)", groupBy, key, got[key], tokens, got)
				}
			}
		}
	})
}

// Every key a bucket can carry is in openapi.json, and the `tokens` object's
// keys are the five the handler writes — the generated UI types are made from
// this document, so a key it does not name is a key the interface cannot read.
// The object is declared once and referenced by every row that carries it
// (spec 049, API contract).
func TestStatsTokensAreDocumented(t *testing.T) {
	body, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
		Paths map[string]struct {
			Get struct {
				Responses map[string]struct {
					Content map[string]struct {
						Schema struct {
							Properties struct {
								Buckets struct {
									Items struct {
										Properties map[string]json.RawMessage `json:"properties"`
									} `json:"items"`
								} `json:"buckets"`
							} `json:"properties"`
						} `json:"schema"`
					} `json:"content"`
				} `json:"responses"`
			} `json:"get"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	bucket := doc.Paths["/api/v1/stats"].Get.Responses["200"].Content["application/json"].
		Schema.Properties.Buckets.Items.Properties
	tokens, ok := bucket["tokens"]
	if !ok {
		t.Fatal("openapi.json does not document `tokens` on a stats bucket")
	}
	const ref = `"#/components/schemas/Tokens"`
	if !strings.Contains(string(tokens), ref) {
		t.Errorf("a stats bucket's `tokens` does not reference the Tokens schema: %s", tokens)
	}
	for _, schema := range []string{"TraceRow", "SessionRow", "UserRow"} {
		if !strings.Contains(string(doc.Components.Schemas[schema].Properties["tokens"]), ref) {
			t.Errorf("%s's `tokens` does not reference the Tokens schema", schema)
		}
	}
	declared := doc.Components.Schemas["Tokens"].Properties
	for _, key := range []string{"input", "output", "cache_read", "reasoning", "cache_write"} {
		if _, ok := declared[key]; !ok {
			t.Errorf("openapi.json does not document `tokens.%s`", key)
		}
	}
	if len(declared) != 5 {
		t.Errorf("the Tokens schema declares %d classes, the handler writes 5", len(declared))
	}
}
