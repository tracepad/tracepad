package e2e

import (
	"context"
	"errors"
	"testing"
	"time"

	tracepad "github.com/tracepad/tracepad/sdk/go"
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
