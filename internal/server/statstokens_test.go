package server

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// Tokens per bucket (spec 031 #4): a `tokens` object with only the keys
// something in the bucket carried, and no object at all when none did.

type tokensBucket struct {
	Key    string `json:"key"`
	Count  int    `json:"count"`
	Tokens *struct {
		Input     *int64 `json:"input"`
		Output    *int64 `json:"output"`
		CacheRead *int64 `json:"cache_read"`
	} `json:"tokens"`
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
		if production.Tokens == nil || production.Tokens.Input == nil || *production.Tokens.Input != 300 ||
			production.Tokens.Output == nil || *production.Tokens.Output != 30 ||
			production.Tokens.CacheRead == nil || *production.Tokens.CacheRead != 5 {
			t.Errorf("production tokens = %+v, want 300 in, 30 out, 5 cached", production.Tokens)
		}
		// Nothing in staging carried a count: no object, not three zeroes.
		if staging.Tokens != nil {
			t.Errorf("staging tokens = %+v, want the object absent", staging.Tokens)
		}

		byModel := h.tokensBuckets(t, "/api/v1/stats?group_by=model")
		if len(byModel) != 2 {
			t.Fatalf("buckets = %+v, want both models", byModel)
		}
		// Only the keys that have something: the OpenAI spelling carried no
		// cache-read count, so `cache_read` is absent on its bucket.
		mini := byModel[1]
		if mini.Key != "gpt-4o-mini" || mini.Tokens == nil || mini.Tokens.CacheRead != nil ||
			mini.Tokens.Input == nil || *mini.Tokens.Input != 200 {
			t.Errorf("gpt-4o-mini bucket = %+v, want input and output only", mini)
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
			t.Errorf("production tokens = %+v across the seam, want the rolled 300 in", production.Tokens)
		}
		byHour := h.tokensBuckets(t, "/api/v1/stats?group_by=hour")
		if len(byHour) != 2 || byHour[1].Tokens != nil {
			t.Errorf("hours = %+v, want the live hour without a tokens object", byHour)
		}
	})

	// A `user_id` answer never carries tokens: its rolled half is the
	// per-user rollup, which holds none, and a live tail that reported them
	// would show the seam.
	t.Run("not for one user", func(t *testing.T) {
		h.seedTokens(t, statsHour+3*3600, 5, "production", "claude-sonnet-5",
			map[string]any{"input_tokens": 7})
		for _, groupBy := range []string{"hour", "model"} {
			buckets := h.tokensBuckets(t, "/api/v1/stats?group_by="+groupBy+"&user_id=u1")
			if len(buckets) == 0 {
				t.Fatalf("no %s buckets for the user, so the test proves nothing", groupBy)
			}
			for _, bucket := range buckets {
				if bucket.Tokens != nil {
					t.Errorf("%s bucket %s = %+v, want no tokens on a user's timeline",
						groupBy, bucket.Key, bucket.Tokens)
				}
			}
		}
	})
}

// Every key a bucket can carry is in openapi.json's bucket schema, and the
// `tokens` object's keys are the three the handler writes — the generated
// UI types are made from this document, so a key it does not name is a key
// the interface cannot read.
func TestStatsTokensAreDocumented(t *testing.T) {
	body, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Paths map[string]struct {
			Get struct {
				Responses map[string]struct {
					Content map[string]struct {
						Schema struct {
							Properties struct {
								Buckets struct {
									Items struct {
										Properties map[string]struct {
											Properties map[string]json.RawMessage `json:"properties"`
										} `json:"properties"`
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
	for _, key := range []string{"input", "output", "cache_read"} {
		if _, ok := tokens.Properties[key]; !ok {
			t.Errorf("openapi.json does not document `tokens.%s`", key)
		}
	}
}
