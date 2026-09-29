package server

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// Tokens where people look (spec 049 #6): the `tokens` object on trace,
// session and user rows, `min_tokens` on the trace listing and `sort=tokens`
// on the user listing.

func (h *harness) seedTokenTrace(t *testing.T, n int, user, session string, usage map[string]any) {
	t.Helper()
	start := statsHour*int64(time.Second) + int64(n)*int64(time.Second)
	trace := &model.Trace{ID: traceHex(n), Environment: "production", UserID: user, SessionID: session}
	h.seed(t, trace, &model.Observation{
		TraceID: trace.ID, ID: spanHex(n), Type: model.TypeGeneration,
		Level: model.LevelDefault, Model: "m",
		StartTime: start, EndTime: start + 100*ms, Usage: usage,
	})
}

func TestTokensOnRows(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedTokenTrace(t, 1, "alice", "s1", map[string]any{
		"input_tokens": 100, "output_tokens": 10, "reasoning_tokens": 4, "cache_creation_input_tokens": 2})
	h.seedTokenTrace(t, 2, "alice", "s1", map[string]any{"input_tokens": 50, "cache_read_input_tokens": 30})
	h.seedTokenTrace(t, 3, "bob", "s2", nil)
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	type row struct {
		ID     string      `json:"id"`
		UserID string      `json:"user_id"`
		Tokens *tokensJSON `json:"tokens"`
	}
	const window = "from=2026-08-26T00:00:00Z&to=2026-08-27T00:00:00Z"

	traces := decodeJSON[struct {
		Rows []row `json:"traces"`
	}](t, h.get(t, "/api/v1/traces?"+window)).Rows
	byID := map[string]string{}
	for _, r := range traces {
		byID[r.ID] = r.Tokens.String()
	}
	for id, want := range map[string]string{
		traceHex(1): "{100 10 NULL 4 2}",
		traceHex(2): "{50 NULL 30 NULL NULL}",
		traceHex(3): "absent",
	} {
		if byID[id] != want {
			t.Errorf("trace %s tokens = %s, want %s", id, byID[id], want)
		}
	}

	// `fields=` selects it like any other row field.
	selected := decodeJSON[struct {
		Rows []map[string]any `json:"traces"`
	}](t, h.get(t, "/api/v1/traces?fields=id,tokens&"+window)).Rows
	if len(selected) != 3 {
		t.Fatalf("fields=id,tokens listed %d rows, want 3", len(selected))
	}
	for _, r := range selected {
		if len(r) > 2 {
			t.Errorf("fields=id,tokens rendered %v", r)
		}
	}

	detail := decodeJSON[row](t, h.get(t, "/api/v1/traces/"+traceHex(1)))
	if got := detail.Tokens.String(); got != "{100 10 NULL 4 2}" {
		t.Errorf("trace detail tokens = %s", got)
	}

	sessions := decodeJSON[struct {
		Rows []row `json:"sessions"`
	}](t, h.get(t, "/api/v1/sessions?"+window)).Rows
	if len(sessions) != 2 || len(traces) != 3 {
		t.Fatalf("listed %d sessions and %d traces, want 2 and 3", len(sessions), len(traces))
	}
	for _, r := range sessions {
		want := map[string]string{"s1": "{150 10 30 4 2}", "s2": "absent"}[r.ID]
		if got := r.Tokens.String(); got != want {
			t.Errorf("session %s tokens = %s, want %s", r.ID, got, want)
		}
	}
	session := decodeJSON[row](t, h.get(t, "/api/v1/sessions/s1"))
	if got := session.Tokens.String(); got != "{150 10 30 4 2}" {
		t.Errorf("session detail tokens = %s", got)
	}

	users := decodeJSON[struct {
		Rows []row `json:"users"`
	}](t, h.get(t, "/api/v1/users")).Rows
	if len(users) != 2 {
		t.Fatalf("listed %d users, want 2", len(users))
	}
	for _, r := range users {
		want := map[string]string{"alice": "{150 10 30 4 2}", "bob": "absent"}[r.UserID]
		if got := r.Tokens.String(); got != want {
			t.Errorf("user %s tokens = %s, want %s", r.UserID, got, want)
		}
	}

	// The user page merges the live tail: a trace after the watermark adds
	// to the rolled sums.
	h.seedTokenTrace(t, 4*3600, "alice", "s3", map[string]any{"input_tokens": 1, "reasoning_tokens": 1})
	alice := decodeJSON[row](t, h.get(t, "/api/v1/users/alice"))
	if got := alice.Tokens.String(); got != "{151 10 30 5 2}" {
		t.Errorf("alice's page tokens = %s, want the rolled sums plus the live trace", got)
	}
}

func TestMinTokensFilter(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedTokenTrace(t, 1, "", "", map[string]any{"input_tokens": 60, "output_tokens": 40})
	h.seedTokenTrace(t, 2, "", "", map[string]any{"output_tokens": 99, "reasoning_tokens": 900})
	h.seedTokenTrace(t, 3, "", "", nil)

	ids := func(query string) string {
		t.Helper()
		rec := h.get(t, "/api/v1/traces?from=2026-08-26T00:00:00Z&"+query)
		expectStatus(t, rec, 200)
		var out []string
		for _, r := range decodeJSON[struct {
			Rows []struct {
				ID string `json:"id"`
			} `json:"traces"`
		}](t, rec).Rows {
			out = append(out, r.ID[len(r.ID)-1:])
		}
		return strings.Join(out, ",")
	}
	for query, want := range map[string]string{
		"min_tokens=0":   "2,1",
		"min_tokens=100": "1",
		"min_tokens=101": "",
	} {
		if got := ids(query); got != want {
			t.Errorf("%s: traces %q, want %q", query, got, want)
		}
	}
	for _, bad := range []string{"-1", "1.5", "lots", "1e3"} {
		rec := h.get(t, "/api/v1/traces?min_tokens="+url.QueryEscape(bad))
		expectStatus(t, rec, 400)
		if !strings.Contains(rec.Body.String(), "min_tokens must be a non-negative integer") {
			t.Errorf("min_tokens=%s: %s", bad, rec.Body.String())
		}
	}
}

func TestUsersSortByTokensOverHTTP(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	h.seedTokenTrace(t, 1, "light", "", map[string]any{"input_tokens": 5})
	h.seedTokenTrace(t, 2, "heavy", "", map[string]any{"input_tokens": 500, "output_tokens": 5})
	h.seedTokenTrace(t, 3, "none", "", nil)
	h.rollTheCorpus(t, time.Unix(statsHour+3*3600, 0))

	var walked []string
	path := "/api/v1/users?sort=tokens&limit=1"
	for range 5 {
		page := decodeJSON[struct {
			Rows []struct {
				UserID string `json:"user_id"`
			} `json:"users"`
			NextCursor *string `json:"next_cursor"`
		}](t, h.get(t, path))
		for _, r := range page.Rows {
			walked = append(walked, r.UserID)
		}
		if page.NextCursor == nil {
			break
		}
		path = "/api/v1/users?sort=tokens&limit=1&cursor=" + url.QueryEscape(*page.NextCursor)
	}
	if got := strings.Join(walked, ","); got != "heavy,light,none" {
		t.Errorf("sort=tokens walked %s, want heavy,light,none", got)
	}

	rec := h.get(t, "/api/v1/users?sort=bogus")
	expectStatus(t, rec, 400)
	if !strings.Contains(rec.Body.String(), "last_seen, traces, cost, tokens, errors") {
		t.Errorf("the refusal does not name the sorts: %s", rec.Body.String())
	}
}
