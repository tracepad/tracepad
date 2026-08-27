package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tracepad/tracepad/internal/otlptest"
	"github.com/tracepad/tracepad/internal/store"
)

// End to end (spec 004, Testing #2): the corpus goes in through the real OTLP
// handler and comes back out through every read endpoint. Nothing here reaches
// past HTTP — if a fact is not readable over the API, it does not exist
// (design §3.1).

func TestReadAPIEndToEnd(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	for _, fixture := range otlptest.Fixtures() {
		rec := h.post(t, "/v1/traces", fixtureBody(t, fixture.Name))
		if rec.Code != http.StatusOK {
			t.Fatalf("ingest %s: status = %d, body = %s", fixture.Name, rec.Code, rec.Body)
		}
	}

	// The listing is where an agent starts.
	list := h.get(t, "/api/v1/traces")
	expectStatus(t, list, 200)
	listed := decodeJSON[struct {
		Traces []struct {
			ID          string `json:"id"`
			Environment string `json:"environment"`
			Timestamp   string `json:"timestamp"`
		} `json:"traces"`
	}](t, list)
	if len(listed.Traces) < 5 {
		t.Fatalf("listed %d traces, want the whole corpus", len(listed.Traces))
	}

	// Every listed trace is readable whole, and the tree accounts for
	// exactly the observations the row claims.
	for _, row := range listed.Traces {
		rec := h.get(t, "/api/v1/traces/"+row.ID+"?expand=io")
		expectStatus(t, rec, 200)
		trace := decodeJSON[struct {
			ID               string       `json:"id"`
			ObservationCount int          `json:"observation_count"`
			Observations     []treeNodeJS `json:"observations"`
		}](t, rec)
		if trace.ID != row.ID {
			t.Fatalf("asked for %s, got %s", row.ID, trace.ID)
		}
		if counted := countNodes(trace.Observations); counted != trace.ObservationCount {
			t.Errorf("trace %s: tree holds %d observations, row claims %d",
				row.ID, counted, trace.ObservationCount)
		}
	}

	// The Langfuse fixture is the one with everything: a session, a user,
	// tags, cost and a generation with payloads.
	const richTrace = "4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f"
	rec := h.get(t, "/api/v1/traces/"+richTrace+"?expand=io")
	expectStatus(t, rec, 200)
	rich := decodeJSON[struct {
		Name         string   `json:"name"`
		UserID       string   `json:"user_id"`
		SessionID    string   `json:"session_id"`
		Tags         []string `json:"tags"`
		TotalCost    *float64 `json:"total_cost"`
		Observations []struct {
			Children []struct {
				Model string `json:"model"`
				Input []struct {
					Content string `json:"content"`
				} `json:"input"`
			} `json:"children"`
		} `json:"observations"`
	}](t, rec)
	if rich.Name != "support-chat" || rich.UserID != "user-4821" || rich.SessionID != "session-77" {
		t.Errorf("trace fields = %+v, want what the export carried", rich)
	}
	if len(rich.Tags) != 2 || rich.TotalCost == nil {
		t.Errorf("tags = %v, cost = %v, want both from the export", rich.Tags, rich.TotalCost)
	}
	generation := rich.Observations[0].Children[0]
	if generation.Model != "claude-sonnet-5" {
		t.Errorf("model = %q, want the generation's", generation.Model)
	}
	if len(generation.Input) != 1 || !strings.Contains(generation.Input[0].Content, "reset my password") {
		t.Errorf("input = %+v, want the payload inlined by ?expand=io", generation.Input)
	}

	// The session the same export declared.
	session := h.get(t, "/api/v1/sessions/session-77")
	expectStatus(t, session, 200)
	if decodeJSON[map[string]any](t, session)["trace_count"] != float64(1) {
		t.Errorf("session = %s, want the one trace that named it", session.Body)
	}

	// The task shortcut over the same data.
	last := h.get(t, "/api/v1/traces/last?environment=production")
	expectStatus(t, last, 200)
	if decodeJSON[map[string]any](t, last)["environment"] != "production" {
		t.Errorf("last = %s, want a production trace", last.Body)
	}

	// Statistics over what was just ingested.
	stats := h.get(t, "/api/v1/stats?group_by=model")
	expectStatus(t, stats, 200)
	models := decodeJSON[statsBody](t, stats)
	if len(models.Buckets) == 0 {
		t.Errorf("stats = %s, want the models the corpus used", stats.Body)
	}

	// And the API describes itself to whoever arrived without docs.
	index := h.get(t, "/api/v1")
	expectStatus(t, index, 200)
	expectStatus(t, h.get(t, "/api/v1/system"), 200)
}

// treeNodeJS is the shape the e2e walk needs: an id and its children.
type treeNodeJS struct {
	ID       string       `json:"id"`
	Children []treeNodeJS `json:"children"`
}

func countNodes(nodes []treeNodeJS) int {
	total := 0
	for _, node := range nodes {
		total += 1 + countNodes(node.Children)
	}
	return total
}
