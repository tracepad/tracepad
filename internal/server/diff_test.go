package server

import (
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/store"
)

// The prompt version diff (spec 004 #21): a unified patch, because every
// consumer already reads one.

func TestUnifiedDiff(t *testing.T) {
	for _, tc := range []struct {
		name           string
		before, after  string
		wantEmpty      bool
		wantFragments  []string
		wantNotPresent []string
	}{
		{
			name:      "identical versions have no diff",
			before:    "a\nb\nc\n",
			after:     "a\nb\nc\n",
			wantEmpty: true,
		},
		{
			name:          "a changed line",
			before:        "one\ntwo\nthree\n",
			after:         "one\nTWO\nthree\n",
			wantFragments: []string{"@@ -1,3 +1,3 @@", "-two", "+TWO", " one", " three"},
		},
		{
			name:          "an added line",
			before:        "one\ntwo\n",
			after:         "one\nmiddle\ntwo\n",
			wantFragments: []string{"+middle"},
		},
		{
			name:          "a removed line",
			before:        "one\nmiddle\ntwo\n",
			after:         "one\ntwo\n",
			wantFragments: []string{"-middle"},
		},
		{
			name:          "an empty side",
			before:        "",
			after:         "new\n",
			wantFragments: []string{"@@ -0,0 +1 @@", "+new"},
		},
		{
			// Far-apart changes get their own hunks rather than one
			// hunk carrying the whole file between them.
			name:           "distant changes are separate hunks",
			before:         "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n14\n15\n16\n17\n18\n19\n20\n",
			after:          "X\n2\n3\n4\n5\n6\n7\n8\n9\n10\n11\n12\n13\n14\n15\n16\n17\n18\n19\nY\n",
			wantFragments:  []string{"-1", "+X", "-20", "+Y"},
			wantNotPresent: []string{" 10"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := unifiedDiff("prompt", "version 1", "version 2", tc.before, tc.after)
			if tc.wantEmpty {
				if got != "" {
					t.Fatalf("diff = %q, want none", got)
				}
				return
			}
			if !strings.HasPrefix(got, "--- prompt (version 1)\n+++ prompt (version 2)\n") {
				t.Fatalf("diff does not start with its header:\n%s", got)
			}
			for _, fragment := range tc.wantFragments {
				if !hasLine(got, fragment) {
					t.Errorf("diff is missing %q:\n%s", fragment, got)
				}
			}
			for _, fragment := range tc.wantNotPresent {
				if hasLine(got, fragment) {
					t.Errorf("diff should not contain %q:\n%s", fragment, got)
				}
			}
		})
	}
}

// hasLine reports whether the diff contains the fragment as a whole line,
// so that "-1" does not match "-10".
func hasLine(diff, fragment string) bool {
	for _, line := range strings.Split(diff, "\n") {
		if line == fragment {
			return true
		}
	}
	return false
}

// TestDiffAppliesToLargeInputs: the LCS table is bounded, and beyond the bound
// the two sides are reported as a wholesale replacement rather than aligned.
func TestDiffFallsBackOnHugeInputs(t *testing.T) {
	before := strings.Repeat("a\n", 3000)
	after := strings.Repeat("b\n", 3000)
	got := unifiedDiff("prompt", "version 1", "version 2", before, after)
	if !strings.Contains(got, "-a") || !strings.Contains(got, "+b") {
		t.Fatalf("diff does not describe the change:\n%s", got[:min(len(got), 200)])
	}
}

func TestPromptDiffEndpoint(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})

	expectStatus(t, h.send(t, "POST", "/api/v1/prompts/support/versions", map[string]any{
		"type":   "text",
		"prompt": "You are a support agent.\nBe brief.",
		"config": map[string]any{"temperature": 0.2},
	}), 201)
	expectStatus(t, h.send(t, "POST", "/api/v1/prompts/support/versions", map[string]any{
		"prompt": "You are a support agent.\nBe brief and cite the source.",
		"config": map[string]any{"temperature": 0.7},
	}), 201)

	rec := h.get(t, "/api/v1/prompts/support/diff?from=1&to=2")
	expectStatus(t, rec, 200)
	body := decodeJSON[struct {
		Name string `json:"name"`
		From int    `json:"from"`
		To   int    `json:"to"`
		Diff string `json:"diff"`
	}](t, rec)

	if body.Name != "support" || body.From != 1 || body.To != 2 {
		t.Errorf("body = %+v, want the versions that were asked for", body)
	}
	// Both halves of a version are diffed: a run can change because the
	// text changed or because the parameters did.
	for _, fragment := range []string{"--- prompt (version 1)", "Be brief and cite the source.",
		"--- config (version 1)", `"temperature": 0.7`} {
		if !strings.Contains(body.Diff, fragment) {
			t.Errorf("diff is missing %q:\n%s", fragment, body.Diff)
		}
	}

	// A version that does not exist is a 404 naming it, not an empty diff.
	expectError(t, h.get(t, "/api/v1/prompts/support/diff?from=1&to=9"), 404, "has no version 9")
	expectError(t, h.get(t, "/api/v1/prompts/support/diff?from=1"), 400, "to is required")
	expectError(t, h.get(t, "/api/v1/prompts/support/diff?from=0&to=1"), 400, "from must be")

	// Identical versions diff to nothing rather than to noise.
	same := h.get(t, "/api/v1/prompts/support/diff?from=2&to=2")
	expectStatus(t, same, 200)
	if diff := decodeJSON[map[string]any](t, same)["diff"]; diff != "" {
		t.Errorf("diff = %q, want empty for a version against itself", diff)
	}
}
