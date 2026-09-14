package tracepad

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func TestSpanWritesTheVocabulary(t *testing.T) {
	r := setup(t)
	ctx, step := Span(context.Background(), "retrieve",
		WithInput(map[string]any{"query": "reset"}), WithMetadata(map[string]any{"hits": 2}))
	_, child := Span(ctx, "child")
	child.End()
	step.Update(WithOutput([]string{"the ingest page"}), WithLevel("WARNING"),
		WithStatusMessage("one result"), WithType("retriever"), WithName("docs-search"))
	step.End()
	step.End() // the second call is nothing

	attrs := r.attrs(t, "docs-search")
	want := map[string]string{
		attrObservationType:      "retriever",
		attrInput:                `{"query":"reset"}`,
		attrOutput:               `["the ingest page"]`,
		attrObservationMetadata:  `{"hits":2}`,
		attrObservationLevel:     "WARNING",
		attrObservationStatusMsg: "one result",
	}
	for key, value := range want {
		if got := str(t, attrs, key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
	if len(r.spans()) != 2 {
		t.Fatalf("spans = %d, want the step and its child", len(r.spans()))
	}
	// The child is under the step: the returned context carries the span.
	if got := r.one(t, "child").Parent.SpanID(); got != step.Span().SpanContext().SpanID() {
		t.Errorf("child parent = %s, want %s", got, step.SpanID())
	}
	if step.TraceID() != r.one(t, "child").SpanContext.TraceID().String() || len(step.SpanID()) != 16 {
		t.Errorf("ids = %s / %s", step.TraceID(), step.SpanID())
	}
}

func TestAStringInputIsSentAsItIs(t *testing.T) {
	r := setup(t)
	_, step := Span(context.Background(), "s", WithInput("plain text"))
	step.End()
	if got := str(t, r.attrs(t, "s"), attrInput); got != "plain text" {
		t.Errorf("input = %q", got)
	}
}

func TestAValueJSONCannotExpressIsStillAString(t *testing.T) {
	r := setup(t)
	_, step := Span(context.Background(), "s", WithInput(map[string]any{"ch": make(chan int)}))
	step.End()
	if got := str(t, r.attrs(t, "s"), attrInput); !strings.Contains(got, "ch") {
		t.Errorf("input = %q, want fmt.Sprint of the value", got)
	}
}

func TestEventIsZeroDuration(t *testing.T) {
	r := setup(t)
	_, event := Event(context.Background(), "cache.miss", WithMetadata(map[string]any{"key": "k"}))
	event.End()
	s := r.one(t, "cache.miss")
	if !s.StartTime.Equal(s.EndTime) {
		t.Errorf("start %v != end %v", s.StartTime, s.EndTime)
	}
	if got := str(t, attrsOf(s), attrObservationType); got != "event" {
		t.Errorf("type = %q", got)
	}
}

func TestFailRecordsTheErrorAndEnds(t *testing.T) {
	r := setup(t)
	_, step := Span(context.Background(), "fails")
	step.Fail(errors.New("upstream timeout"))
	step.End()
	s := r.one(t, "fails")
	if s.Status.Code != codes.Error || s.Status.Description != "upstream timeout" {
		t.Errorf("status = %+v", s.Status)
	}
	if len(s.Events) != 1 || s.Events[0].Name != "exception" {
		t.Fatalf("events = %+v, want the exception event", s.Events)
	}
}

func TestUpdateActsOnTheCurrentSpanWhoeverStartedIt(t *testing.T) {
	r := setup(t)
	ctx, span := r.provider.Tracer("the.framework").Start(context.Background(), "GET /answer")
	Update(ctx, WithLevel("ERROR"), WithType("agent"))
	UpdateTrace(ctx, WithTraceName("support-chat"), WithUserID("u-42"), WithSessionID("s-7"),
		WithTags("support", "beta"), WithTraceMetadata(map[string]any{"channel": "web"}))
	span.End()
	attrs := r.attrs(t, "GET /answer")
	want := map[string]string{
		attrObservationLevel: "ERROR", attrObservationType: "agent",
		attrTraceName: "support-chat", attrUserID: "u-42", attrSessionID: "s-7",
		attrTraceTags: `["support","beta"]`, attrTraceMetadata: `{"channel":"web"}`,
	}
	for key, value := range want {
		if got := str(t, attrs, key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
}

func TestUpdateOutsideASpanLogsAndWritesNothing(t *testing.T) {
	r := setup(t)
	Update(context.Background(), WithLevel("ERROR"))
	UpdateTrace(context.Background(), WithUserID("u"))
	if !strings.Contains(r.logs.String(), "tracepad.Update outside a span") ||
		!strings.Contains(r.logs.String(), "tracepad.UpdateTrace outside a span") {
		t.Errorf("logs = %q", r.logs.String())
	}
	if len(r.spans()) != 0 {
		t.Errorf("spans = %d", len(r.spans()))
	}
}

func TestAnUnknownTypeIsWrittenWithAWarning(t *testing.T) {
	r := setup(t)
	_, step := Span(context.Background(), "s", WithType("thought"))
	step.End()
	if got := str(t, r.attrs(t, "s"), attrObservationType); got != "thought" {
		t.Errorf("type = %q", got)
	}
	if !strings.Contains(r.logs.String(), "not one of the observation types") {
		t.Errorf("logs = %q", r.logs.String())
	}
}

func TestSpansGoToTheProviderInitWasHanded(t *testing.T) {
	r := setup(t)
	_, step := Span(context.Background(), "s")
	step.End()
	if len(r.spans()) != 1 {
		t.Fatalf("spans = %d", len(r.spans()))
	}
	// The global provider was neither set nor replaced.
	if otel.GetTracerProvider() == trace.TracerProvider(r.provider) {
		t.Fatal("WithTracerProvider must not set the provider global")
	}
}
