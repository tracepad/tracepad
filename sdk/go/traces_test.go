package tracepad

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Deleting traces: the dry run, the echo, the rounds (spec 036).

const traceID = "4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f"

func TestDeleteTraceDryRunSendsNoConfirmAndConfirmEchoesTheID(t *testing.T) {
	_, fs := harness(t)
	fs.answer("/api/v1/traces/"+traceID, map[string]any{"dry_run": true, "confirm": traceID})
	preview, err := DeleteTrace(context.Background(), traceID, false)
	if err != nil || preview["dry_run"] != true {
		t.Fatalf("preview = %v, err = %v", preview, err)
	}
	if _, err := DeleteTrace(context.Background(), traceID, true); err != nil {
		t.Fatal(err)
	}
	want := []string{"DELETE /api/v1/traces/" + traceID, "DELETE /api/v1/traces/" + traceID + "?confirm=" + traceID}
	if got := fs.paths(); !reflect.DeepEqual(got, want) {
		t.Errorf("paths = %v, want %v", got, want)
	}
}

func TestDeleteTraceUnknownIsTheError(t *testing.T) {
	_, fs := harness(t)
	fs.answer("/api/v1/traces/"+traceID, 404)
	answer, err := DeleteTrace(context.Background(), traceID, true)
	var h *HTTPError
	if !errors.As(err, &h) || h.Status != 404 || answer != nil {
		t.Errorf("answer = %v, err = %v, want a 404 *HTTPError and no answer", answer, err)
	}
}

func TestDeleteTracesDryRunPassesTheFiltersThrough(t *testing.T) {
	_, fs := harness(t)
	fs.answer("/api/v1/traces", map[string]any{"dry_run": true, "matched": 3})
	filter := TraceFilter{
		From:        time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		To:          time.Date(2026, 9, 17, 16, 2, 17, 0, time.FixedZone("CEST", 2*3600)),
		Environment: "staging",
		Tag:         []string{"a", "b"},
	}
	preview, err := DeleteTraces(context.Background(), filter, "", WithRoundLimit(5))
	if err != nil || preview["matched"] != 3.0 {
		t.Fatalf("preview = %v, err = %v", preview, err)
	}
	// The preview as the API gave it; one call, the times in UTC, and
	// neither confirm nor limit on it — the dry run has no rounds.
	want := "DELETE /api/v1/traces?environment=staging&from=2026-09-01T00%3A00%3A00.000Z&tag=a&tag=b&to=2026-09-17T14%3A02%3A17.000Z"
	if got := fs.paths(); !reflect.DeepEqual(got, []string{want}) {
		t.Errorf("paths = %v, want %v", got, want)
	}
}

func TestDeleteTracesWithoutToIsAnErrorBeforeAnyRequest(t *testing.T) {
	_, fs := harness(t)
	if _, err := DeleteTraces(context.Background(), TraceFilter{Environment: "staging"}, "my-project"); err == nil {
		t.Fatal("want an error for a zero To")
	}
	if len(fs.paths()) != 0 {
		t.Errorf("calls = %v, want none", fs.paths())
	}
}

func TestDeleteTracesConfirmedWalksTheRoundsAndSums(t *testing.T) {
	_, fs := harness(t)
	fs.answer("/api/v1/traces",
		map[string]any{"deleted": map[string]any{"traces": 1000, "observations": 4000, "payloads": 9}, "more": true},
		map[string]any{"deleted": map[string]any{"traces": 1000, "observations": 3000, "payloads": 0}, "more": true},
		map[string]any{"deleted": map[string]any{"traces": 12, "observations": 30, "payloads": 1}, "more": false},
	)
	filter := TraceFilter{To: time.Date(2026, 9, 17, 14, 2, 17, 0, time.UTC), Environment: "staging"}
	total, err := DeleteTraces(context.Background(), filter, "my-project")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"deleted": map[string]any{"traces": 2012.0, "observations": 7030.0, "payloads": 10.0},
		"rounds":  3.0,
	}
	if !reflect.DeepEqual(total, want) {
		t.Errorf("total = %v, want %v", total, want)
	}
	round := "DELETE /api/v1/traces?confirm=my-project&environment=staging&limit=1000&to=2026-09-17T14%3A02%3A17.000Z"
	if got := fs.paths(); !reflect.DeepEqual(got, []string{round, round, round}) {
		t.Errorf("paths = %v", got)
	}
}

func TestDeleteTracesARoundOfNothingIsOneRound(t *testing.T) {
	_, fs := harness(t)
	fs.answer("/api/v1/traces", map[string]any{"deleted": map[string]any{"traces": 0}, "more": false})
	total, err := DeleteTraces(context.Background(), TraceFilter{To: time.Now()}, "my-project", WithRoundLimit(10))
	if err != nil || total["rounds"] != 1.0 || total["deleted"].(map[string]any)["traces"] != 0.0 {
		t.Errorf("total = %v, err = %v", total, err)
	}
	if last := fs.last(); !strings.Contains(last.query, "limit=10") {
		t.Errorf("query = %s, want limit=10", last.query)
	}
}

func TestDeleteTracesAWrongEchoStopsAtTheFirstRound(t *testing.T) {
	_, fs := harness(t)
	fs.answer("/api/v1/traces", 400, map[string]any{"more": false})
	if _, err := DeleteTraces(context.Background(), TraceFilter{To: time.Now()}, "wrong"); !isClientError(err) {
		t.Fatalf("err = %v, want the 400", err)
	}
	if n := len(fs.paths()); n != 1 {
		t.Errorf("%d calls, want one", n)
	}
}
