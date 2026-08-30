package server

import (
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// The API half of spec 012: the new row fields, the four filters and what each
// of them refuses.

func version(n int64) *int64 { return &n }

// seedWireCorpus writes two releases of one service and a trace that named
// none, each with the observations the filters ask about.
func seedWireCorpus(t *testing.T, h *harness) {
	t.Helper()

	h.seed(t, &model.Trace{ID: traceHex(1), Name: "checkout", Environment: "production",
		Release: "2026.8.30", Version: "checkout-v9"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(1), Type: model.TypeGeneration,
			Name: "answer", Level: model.LevelDefault,
			StartTime: seedBase, EndTime: seedBase + 900*ms,
			CompletionStartTime: seedBase + 388*ms,
			PromptName:          "support-answer", PromptVersion: version(7),
			Input: map[string]any{"role": "user"}, Output: "ok"},
		&model.Observation{TraceID: traceHex(1), ID: spanHex(2), Type: model.TypeTool,
			Name: "catalog-search", Level: model.LevelDefault,
			StartTime: seedBase + 10*ms, EndTime: seedBase + 200*ms})

	h.seed(t, &model.Trace{ID: traceHex(2), Name: "checkout", Environment: "production",
		Release: "2026.8.31", Version: "checkout-v9"},
		&model.Observation{TraceID: traceHex(2), ID: spanHex(3), Type: model.TypeGeneration,
			Name: "answer", Level: model.LevelDefault,
			StartTime: seedBase + 1000*ms, EndTime: seedBase + 1900*ms,
			PromptName: "support-answer", PromptVersion: version(6)})

	h.seed(t, &model.Trace{ID: traceHex(3), Name: "quote", Environment: "staging"},
		&model.Observation{TraceID: traceHex(3), ID: spanHex(4), Type: model.TypeEmbedding,
			Name: "embed", Level: model.LevelDefault, Model: "text-embedding-3-large",
			StartTime: seedBase + 2000*ms, EndTime: seedBase + 2100*ms,
			// A scoped name, the shape a registry that namespaces its
			// prompts produces. The `@` it starts with is part of the
			// name, not a version separator.
			PromptName: "@acme/embed", PromptVersion: version(2)})
}

func TestWireFiltersOnTheListing(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedWireCorpus(t, h)

	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"release", "?release=2026.8.30", []string{traceHex(1)}},
		{"version", "?version=checkout-v9", []string{traceHex(2), traceHex(1)}},
		{"type", "?type=tool", []string{traceHex(1)}},
		{"type, exact", "?type=generation", []string{traceHex(2), traceHex(1)}},
		{"type of a trace that has only that one", "?type=embedding", []string{traceHex(3)}},
		{"prompt at any version", "?prompt=support-answer", []string{traceHex(2), traceHex(1)}},
		{"prompt at a version", "?prompt=support-answer@7", []string{traceHex(1)}},
		{"prompt at a version nobody ran", "?prompt=support-answer@5", nil},
		{"a prompt name that starts with @", "?prompt=@acme/embed", []string{traceHex(3)}},
		{"a scoped prompt name at a version", "?prompt=@acme/embed@2", []string{traceHex(3)}},
		{"a filter over observations beside one over the trace",
			"?type=generation&release=2026.8.31", []string{traceHex(2)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := listedIDs(t, h.get(t, "/api/v1/traces"+tc.query))
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("ids = %v, want %v", got, tc.want)
			}
		})
	}
}

// A filter value the column cannot hold is a 400, not an empty listing: a
// well-formed answer to a typo is worse than an error (spec 012, API
// contract).
func TestWireFiltersRefuseNonsense(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	for _, tc := range []struct {
		name  string
		query string
		says  string
	}{
		{"a type outside the vocabulary", "?type=workflow-step", "type must be one of"},
		{"a prompt version that is not a number", "?prompt=support-answer@latest", "whole number"},
		{"a prompt version that is empty", "?prompt=support-answer@", "whole number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, path := range []string{"/api/v1/traces", "/api/v1/traces/last"} {
				rec := h.get(t, path+tc.query)
				expectStatus(t, rec, 400)
				if !strings.Contains(rec.Body.String(), tc.says) {
					t.Errorf("%s answered %q, want it to say %q", path, rec.Body.String(), tc.says)
				}
			}
		})
	}
}

// The shortcut takes every filter of the listing (spec 004 #7), which is the
// promise the four new ones have to keep too.
func TestLastTraceTakesTheWireFilters(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedWireCorpus(t, h)

	body := decodeJSON[struct {
		ID      string `json:"id"`
		Release string `json:"release"`
	}](t, h.get(t, "/api/v1/traces/last?type=tool"))
	if body.ID != traceHex(1) || body.Release != "2026.8.30" {
		t.Errorf("last trace = %+v, want the one with a tool call", body)
	}
}

// The trace row and the observation carry what the wire sent (spec 012, API
// contract).
func TestWireFieldsOnTheRowAndTheObservation(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedWireCorpus(t, h)

	t.Run("the listing row", func(t *testing.T) {
		body := decodeJSON[struct {
			Traces []struct {
				ID      string `json:"id"`
				Release string `json:"release"`
				Version string `json:"version"`
				TTFTMs  *int64 `json:"ttft_ms"`
			} `json:"traces"`
		}](t, h.get(t, "/api/v1/traces?release=2026.8.30"))

		if len(body.Traces) != 1 {
			t.Fatalf("listed %d traces", len(body.Traces))
		}
		row := body.Traces[0]
		if row.Release != "2026.8.30" || row.Version != "checkout-v9" {
			t.Errorf("release/version = %q/%q", row.Release, row.Version)
		}
		if row.TTFTMs == nil || *row.TTFTMs != 388 {
			t.Errorf("ttft_ms = %v, want 388", row.TTFTMs)
		}
	})

	// A trace no observation of which reported a completion start has no
	// TTFT, and the field is absent rather than zero.
	t.Run("a trace with no completion start", func(t *testing.T) {
		rec := h.get(t, "/api/v1/traces?release=2026.8.31")
		expectStatus(t, rec, 200)
		if strings.Contains(rec.Body.String(), "ttft_ms") {
			t.Errorf("row = %s, want no ttft_ms at all", rec.Body.String())
		}
	})

	t.Run("the observation", func(t *testing.T) {
		body := decodeJSON[struct {
			Observations []struct {
				ID                  string `json:"id"`
				Type                string `json:"type"`
				CompletionStartTime string `json:"completion_start_time"`
				TTFTMs              *int64 `json:"ttft_ms"`
				InputBytes          *int64 `json:"input_bytes"`
				OutputBytes         *int64 `json:"output_bytes"`
				Prompt              *struct {
					Name    string `json:"name"`
					Version *int64 `json:"version"`
				} `json:"prompt"`
				Children []struct {
					ID     string `json:"id"`
					Type   string `json:"type"`
					Prompt *struct {
						Name string `json:"name"`
					} `json:"prompt"`
				} `json:"children"`
			} `json:"observations"`
		}](t, h.get(t, "/api/v1/traces/"+traceHex(1)))

		if len(body.Observations) != 2 {
			t.Fatalf("read %d root observations, want the generation and the tool call",
				len(body.Observations))
		}
		generation := body.Observations[0]
		if generation.Type != "generation" {
			t.Errorf("type = %q", generation.Type)
		}
		if generation.CompletionStartTime == "" {
			t.Error("completion_start_time is absent, want the instant the client sent")
		}
		if generation.TTFTMs == nil || *generation.TTFTMs != 388 {
			t.Errorf("ttft_ms = %v, want 388", generation.TTFTMs)
		}
		if generation.Prompt == nil || generation.Prompt.Name != "support-answer" ||
			generation.Prompt.Version == nil || *generation.Prompt.Version != 7 {
			t.Errorf("prompt = %+v", generation.Prompt)
		}
		// The sizes ride the tree without `?expand=io`: that is what the
		// panel shows beside the payload headings (#6).
		if generation.InputBytes == nil || generation.OutputBytes == nil {
			t.Errorf("sizes = %v/%v, want them without an expansion",
				generation.InputBytes, generation.OutputBytes)
		}

		tool := body.Observations[1]
		if tool.Type != "tool" {
			t.Errorf("type = %q, want the widened vocabulary", tool.Type)
		}
		if tool.Prompt != nil {
			t.Errorf("prompt = %+v on an observation that ran none", tool.Prompt)
		}
		if tool.InputBytes != nil {
			t.Errorf("input_bytes = %v on an observation with no payload", tool.InputBytes)
		}
	})
}

// `?fields=` selects from the row's fields, and the two new ones joined that
// list (spec 004, API contract).
func TestWireFieldsAreSelectable(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedWireCorpus(t, h)

	rec := h.get(t, "/api/v1/traces?fields=id,release,ttft_ms")
	expectStatus(t, rec, 200)
	body := rec.Body.String()
	if !strings.Contains(body, `"release"`) || !strings.Contains(body, `"ttft_ms"`) {
		t.Errorf("body = %s, want the selected fields", body)
	}
	if strings.Contains(body, `"observation_count"`) {
		t.Errorf("body = %s, want only the selected fields", body)
	}
}

// `group_by=release` joins the statistics, and a trace that named no release
// is a bucket rather than an omission (spec 012 #4).
func TestStatsGroupByRelease(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedWireCorpus(t, h)

	body := decodeJSON[struct {
		GroupBy string `json:"group_by"`
		Unit    string `json:"unit"`
		Buckets []struct {
			Key   string `json:"key"`
			Count int    `json:"count"`
		} `json:"buckets"`
	}](t, h.get(t, "/api/v1/stats?group_by=release"))

	if body.GroupBy != "release" || body.Unit != "trace" {
		t.Errorf("group_by/unit = %q/%q", body.GroupBy, body.Unit)
	}
	counts := map[string]int{}
	for _, bucket := range body.Buckets {
		counts[bucket.Key] = bucket.Count
	}
	if counts["2026.8.30"] != 1 || counts["2026.8.31"] != 1 {
		t.Errorf("buckets = %v, want one trace per release", counts)
	}
	if counts[""] != 1 {
		t.Errorf("buckets = %v, want the trace with no release under the empty key", counts)
	}
}

func TestStatsRefusesAnUnknownGrouping(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	rec := h.get(t, "/api/v1/stats?group_by=prompt")
	expectStatus(t, rec, 400)
	if !strings.Contains(rec.Body.String(), "release") {
		t.Errorf("the error does not list the groupings that do work: %s", rec.Body.String())
	}
}

// A prompt name may contain an `@`, so the version is split off the last one
// rather than the first — and a name may *begin* with one, so a leading `@`
// is part of the name and not a separator at all (found in review of PR #19:
// the interface's own prompt badge linked to `?prompt=@acme/support`, and the
// API answered its own link with a 400).
func TestPromptFilterSplitsOnTheLastAt(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		name    string
		version *int64
	}{
		{"support-answer", "support-answer", nil},
		{"support-answer@7", "support-answer", version(7)},
		{"team@acme/answer@3", "team@acme/answer", version(3)},
		{"@acme/support", "@acme/support", nil},
		{"@acme/support@7", "@acme/support", version(7)},
		{"@", "@", nil},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parsePrompt(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != tc.name {
				t.Errorf("name = %q, want %q", got.Name, tc.name)
			}
			switch {
			case tc.version == nil && got.Version != nil:
				t.Errorf("version = %d, want any version", *got.Version)
			case tc.version != nil && (got.Version == nil || *got.Version != *tc.version):
				t.Errorf("version = %v, want %d", got.Version, *tc.version)
			}
		})
	}

	// An empty `prompt=` never reaches this far, the handler treating an
	// empty parameter as absent — but a filter on no name is not a filter,
	// and the guard refuses it rather than building one that matches
	// whatever the column happens to hold.
	t.Run("no name at all", func(t *testing.T) {
		if _, err := parsePrompt(""); err == nil {
			t.Error("parsePrompt(``) returned a filter, want a refusal")
		}
	})
}
