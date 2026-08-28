package server

import (
	"testing"

	"github.com/tracepad/tracepad/internal/model"
	"github.com/tracepad/tracepad/internal/store"
)

// Statistics and the session roll-up (spec 004 #8, API contract).

type statsBody struct {
	GroupBy string `json:"group_by"`
	Unit    string `json:"unit"`
	Buckets []struct {
		Key        string   `json:"key"`
		Count      int      `json:"count"`
		ErrorCount int      `json:"error_count"`
		TotalCost  *float64 `json:"total_cost"`
		LatencyMs  struct {
			P50 *int64 `json:"p50"`
			P95 *int64 `json:"p95"`
		} `json:"latency_ms"`
	} `json:"buckets"`
}

func TestStatsGroupings(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)

	t.Run("by day counts traces", func(t *testing.T) {
		rec := h.get(t, "/api/v1/stats?group_by=day")
		expectStatus(t, rec, 200)
		body := decodeJSON[statsBody](t, rec)
		if body.Unit != "trace" {
			t.Fatalf("unit = %q, want traces", body.Unit)
		}
		if len(body.Buckets) != 1 || body.Buckets[0].Key != "2026-09-01" {
			t.Fatalf("buckets = %+v, want one day", body.Buckets)
		}
		bucket := body.Buckets[0]
		if bucket.Count != 3 || bucket.ErrorCount != 1 {
			t.Errorf("bucket = %+v, want 3 traces of which 1 failed", bucket)
		}
		if bucket.TotalCost == nil || *bucket.TotalCost != 0.25 {
			t.Errorf("total_cost = %v, want only what was actually provided", bucket.TotalCost)
		}
		if bucket.LatencyMs.P50 == nil || bucket.LatencyMs.P95 == nil {
			t.Errorf("latency = %+v, want exact percentiles", bucket.LatencyMs)
		}
	})

	t.Run("by model counts observations", func(t *testing.T) {
		rec := h.get(t, "/api/v1/stats?group_by=model")
		expectStatus(t, rec, 200)
		body := decodeJSON[statsBody](t, rec)
		// A trace has no model, so this grouping counts a different unit
		// and says so rather than letting the two counts look alike
		// (Decision 23).
		if body.Unit != "observation" {
			t.Fatalf("unit = %q, want observations", body.Unit)
		}
		if len(body.Buckets) != 1 || body.Buckets[0].Key != "claude-sonnet-5" {
			t.Fatalf("buckets = %+v, want only the observation that named a model", body.Buckets)
		}
		if body.Buckets[0].Count != 1 || body.Buckets[0].ErrorCount != 1 {
			t.Errorf("bucket = %+v, want the one failed generation", body.Buckets[0])
		}
	})

	t.Run("by environment", func(t *testing.T) {
		rec := h.get(t, "/api/v1/stats?group_by=environment")
		expectStatus(t, rec, 200)
		body := decodeJSON[statsBody](t, rec)
		if len(body.Buckets) != 2 ||
			body.Buckets[0].Key != "production" || body.Buckets[1].Key != "staging" {
			t.Fatalf("buckets = %+v, want both environments in a stable order", body.Buckets)
		}
	})

	t.Run("a range with nothing in it invents nothing", func(t *testing.T) {
		rec := h.get(t, "/api/v1/stats?from=2020-01-01T00:00:00Z&to=2020-01-02T00:00:00Z")
		expectStatus(t, rec, 200)
		body := decodeJSON[statsBody](t, rec)
		if len(body.Buckets) != 0 {
			t.Fatalf("buckets = %+v, want none rather than fabricated zeroes", body.Buckets)
		}
		if body.GroupBy != "day" {
			t.Errorf("group_by = %q, want the default", body.GroupBy)
		}
	})

	t.Run("an unknown grouping is refused", func(t *testing.T) {
		expectError(t, h.get(t, "/api/v1/stats?group_by=week"), 400, "group_by must be")
	})
}

// TestStatsPercentilesAreExact: nearest rank over the real samples, never an
// approximation whose error would have to be explained.
func TestStatsPercentilesAreExact(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	for i := 1; i <= 10; i++ {
		h.seed(t, &model.Trace{ID: traceHex(i), Environment: "production"},
			&model.Observation{TraceID: traceHex(i), ID: spanHex(i), Type: model.TypeSpan,
				Level:     model.LevelDefault,
				StartTime: seedBase, EndTime: seedBase + int64(i)*100*ms})
	}

	rec := h.get(t, "/api/v1/stats?group_by=environment")
	expectStatus(t, rec, 200)
	latency := decodeJSON[statsBody](t, rec).Buckets[0].LatencyMs
	// Latencies are 100..1000 ms: the 5th value is p50, the 10th is p95.
	if latency.P50 == nil || *latency.P50 != 500 {
		t.Errorf("p50 = %v, want 500", latency.P50)
	}
	if latency.P95 == nil || *latency.P95 != 1000 {
		t.Errorf("p95 = %v, want 1000", latency.P95)
	}
}

func TestGetSession(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)

	rec := h.get(t, "/api/v1/sessions/s1")
	expectStatus(t, rec, 200)
	body := decodeJSON[struct {
		ID         string   `json:"id"`
		TraceCount int      `json:"trace_count"`
		TotalCost  *float64 `json:"total_cost"`
		ErrorCount int      `json:"error_count"`
		FirstSeen  string   `json:"first_seen"`
		LastSeen   string   `json:"last_seen"`
		Traces     []struct {
			ID string `json:"id"`
		} `json:"traces"`
		NextCursor *string `json:"next_cursor"`
	}](t, rec)

	if body.TraceCount != 2 || body.ErrorCount != 1 {
		t.Errorf("session = %+v, want 2 traces of which 1 failed", body)
	}
	if len(body.Traces) != 2 || body.Traces[0].ID != traceHex(2) {
		t.Errorf("traces = %+v, want both, newest first", body.Traces)
	}
	if body.FirstSeen == "" || body.LastSeen == "" {
		t.Errorf("session = %+v, want the window it spans", body)
	}
	if body.NextCursor != nil {
		t.Errorf("next_cursor = %v, want none on a complete page", *body.NextCursor)
	}
}

func TestGetSessionPages(t *testing.T) {
	h := newHarness(t, nil, store.WriterOptions{})
	seedCorpus(t, h)

	rec := h.get(t, "/api/v1/sessions/s1?limit=1")
	expectStatus(t, rec, 200)
	body := decodeJSON[struct {
		Traces []struct {
			ID string `json:"id"`
		} `json:"traces"`
		NextCursor *string `json:"next_cursor"`
	}](t, rec)
	if len(body.Traces) != 1 || body.NextCursor == nil {
		t.Fatalf("first page = %+v, want one trace and a cursor", body)
	}

	next := h.get(t, "/api/v1/sessions/s1?limit=1&cursor="+*body.NextCursor)
	expectStatus(t, next, 200)
	second := decodeJSON[struct {
		Traces []struct {
			ID string `json:"id"`
		} `json:"traces"`
	}](t, next)
	if len(second.Traces) != 1 || second.Traces[0].ID != traceHex(1) {
		t.Fatalf("second page = %+v, want the older trace", second.Traces)
	}
}
