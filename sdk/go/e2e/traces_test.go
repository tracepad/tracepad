package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	tracepad "github.com/tracepad/tracepad/sdk/go"
	"github.com/tracepad/tracepad/sdk/go/tracepadtest"
)

// Deleting traces against a real binary (spec 036 #7): one by id, one by
// filter, and the listing no longer has them.
func TestTwoTracesGoOneByIDAndOneByFilter(t *testing.T) {
	s := serve(t)
	ctx := context.Background()
	var ids []string
	for _, name := range []string{"first", "second"} {
		stepCtx, step := tracepad.Span(ctx, name)
		tracepad.UpdateTrace(stepCtx, tracepad.WithTraceName("doomed"), tracepad.WithTags("doomed"))
		step.End()
		ids = append(ids, step.TraceID())
	}
	flush(t)
	byID, byFilter := s.trace(ids[0])["id"].(string), s.trace(ids[1])["id"].(string)

	preview, err := tracepad.DeleteTrace(ctx, byID, false)
	if err != nil || preview["dry_run"] != true || preview["confirm"] != byID {
		t.Fatalf("preview = %v, err = %v", preview, err)
	}
	gone, err := tracepad.DeleteTrace(ctx, byID, true)
	if err != nil || gone["id"] != byID || gone["deleted"].(map[string]any)["traces"] != 1.0 {
		t.Fatalf("deleted = %v, err = %v", gone, err)
	}

	filter := tracepad.TraceFilter{To: time.Now().Add(time.Minute), Tag: []string{"doomed"}}
	preview, err = tracepad.DeleteTraces(ctx, filter, "")
	if err != nil || preview["matched"] != 1.0 || preview["confirm"] != "e2e" {
		t.Fatalf("preview = %v, err = %v", preview, err)
	}
	total, err := tracepad.DeleteTraces(ctx, filter, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	counts, _ := total["deleted"].(map[string]any)
	if counts["traces"] != 1.0 || counts["observations"] != 1.0 || total["rounds"] != 1.0 {
		t.Errorf("total = %v, want one trace of one observation in one round", total)
	}

	if left, _ := s.call("GET", "/api/v1/traces?tag=doomed", nil)["traces"].([]any); len(left) != 0 {
		t.Errorf("the listing still has %v", left)
	}
	var h *tracepad.HTTPError
	if _, err := tracepad.DeleteTrace(ctx, byFilter, false); !errors.As(err, &h) || h.Status != 404 {
		t.Errorf("err = %v, want a 404", err)
	}
}

// An ingest key covers the production path — a span, a score, a prompt
// fetch — and deleting traces answers it the server's 403 (spec 045 #16).
// Init is process-wide, so the test takes the package from the shared key
// and hands it back: the reset's cleanup, then the Init with the shared key
// on the application's provider, whose exporter is still attached.
func TestAnIngestKeyCoversTheProductionPathAndNotDeletion(t *testing.T) {
	s := serve(t)
	ctx := context.Background()
	s.call("POST", "/api/v1/prompts/ingest-answer/versions",
		map[string]any{"type": "text", "prompt": "Answer {topic}.", "labels": []string{"production"}})
	ingest := s.mint("ingest")

	t.Cleanup(func() {
		otel.SetTracerProvider(application)
		if _, err := tracepad.Init(ctx, tracepad.WithHost(s.host), tracepad.WithKey(key), tracepad.WithExport(false)); err != nil {
			t.Error(err)
		}
	})
	tracepadtest.Reset(t)
	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = provider.Shutdown(ctx) })
	if _, err := tracepad.Init(ctx, tracepad.WithHost(s.host), tracepad.WithKey(ingest),
		tracepad.WithTracerProvider(provider)); err != nil {
		t.Fatal(err)
	}

	if p, err := tracepad.Prompt(ctx, "ingest-answer", tracepad.WithLabel("production")); err != nil || p.Version != 1 {
		t.Fatalf("prompt = %+v, err = %v", p, err)
	}
	stepCtx, step := tracepad.Span(ctx, "ingest-only")
	tracepad.UpdateTrace(stepCtx, tracepad.WithTags("ingest-only"))
	if err := tracepad.Score(stepCtx, "helpful", tracepad.WithValue(1)); err != nil {
		t.Fatal(err)
	}
	step.End()
	flush(t)
	s.trace(step.TraceID())
	scores, _ := s.call("GET", "/api/v1/scores?trace_id="+step.TraceID(), nil)["scores"].([]any)
	if len(scores) != 1 || scores[0].(map[string]any)["name"] != "helpful" {
		t.Errorf("scores = %v, want the one", scores)
	}

	filter := tracepad.TraceFilter{To: time.Now().Add(time.Minute), Tag: []string{"ingest-only"}}
	_, err := tracepad.DeleteTraces(ctx, filter, "e2e")
	var h *tracepad.HTTPError
	if !errors.As(err, &h) || h.Status != 403 ||
		!strings.Contains(err.Error(), "this key's scopes are ingest; DELETE /api/v1/traces needs write") {
		t.Errorf("err = %v, want the server's 403", err)
	}
	if got := s.trace(step.TraceID())["id"]; got != step.TraceID() {
		t.Errorf("the trace is gone: %v", got)
	}
}
