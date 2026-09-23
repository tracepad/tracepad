package tracepad

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// fields is what the options of Span, Event, Generation and Update collect.
// One struct for all four, because they write one vocabulary; the option
// types below say which of them each call takes.
type fields struct {
	name, typ, level, statusMessage, model string
	input, output, metadata                any
	hasInput, hasOutput, hasMetadata       bool
	parameters                             map[string]any
	prompt                                 *PromptVersion
}

// UpdateOption configures Update. Every SpanOption is one too.
type UpdateOption interface{ applyUpdate(*fields) }

// GenerationOption configures Generation. Every SpanOption is one too.
type GenerationOption interface{ applyGeneration(*fields) }

// SpanOption configures Span and Event — and, since the input, the metadata
// and the kind mean the same thing there, Generation and Update as well.
type SpanOption interface {
	UpdateOption
	GenerationOption
}

type spanOption func(*fields)

func (o spanOption) applyUpdate(f *fields)     { o(f) }
func (o spanOption) applyGeneration(f *fields) { o(f) }

type updateOption func(*fields)

func (o updateOption) applyUpdate(f *fields) { o(f) }

type generationOption func(*fields)

func (o generationOption) applyGeneration(f *fields) { o(f) }

// WithInput is the step's input: the question, the messages, the arguments.
// A string is sent as it is; anything else as JSON.
func WithInput(input any) SpanOption {
	return spanOption(func(f *fields) { f.input, f.hasInput = input, true })
}

// WithMetadata is free metadata on the observation, a JSON object.
func WithMetadata(metadata any) SpanOption {
	return spanOption(func(f *fields) { f.metadata, f.hasMetadata = metadata, true })
}

// WithType is the kind of the step: one of the ten the store classifies by
// (docs/ingest.md) — "span" by default, "event" for Event, "generation" for
// Generation. Another spelling is kept in the observation's metadata, with a
// warning once per spelling. An empty kind is no kind: the default stands
// (spec 038 #8).
func WithType(typ string) SpanOption {
	return spanOption(func(f *fields) {
		if typ != "" {
			f.typ = typ
		}
	})
}

// WithName renames the span.
func WithName(name string) UpdateOption { return updateOption(func(f *fields) { f.name = name }) }

// WithOutput is the step's output.
func WithOutput(output any) UpdateOption {
	return updateOption(func(f *fields) { f.output, f.hasOutput = output, true })
}

// WithLevel is DEBUG, DEFAULT, WARNING or ERROR.
func WithLevel(level string) UpdateOption { return updateOption(func(f *fields) { f.level = level }) }

// WithStatusMessage says why the level is what it is.
func WithStatusMessage(message string) UpdateOption {
	return updateOption(func(f *fields) { f.statusMessage = message })
}

// Observation is one step of a trace: a thin handle over the OTel span,
// which Span() hands out. Span, Event and Generation return one with the
// context that carries its span; the caller ends it.
type Observation struct {
	span  trace.Span
	ended atomic.Bool
	// at is the instant an Event is of: it ends where it started.
	at time.Time
	// traced is whether a trace is behind it: with tracing off, OTel's no-op
	// tracer hands out an invalid context, or echoes a propagated caller's —
	// the caller's ids, not this step's (spec 039 #3).
	traced bool
}

// Span is the OTel span underneath, for anything this handle does not do.
func (o *Observation) Span() trace.Span { return o.span }

// TraceID is the trace's id as the API spells it, 32 lower-case hex digits —
// or "" with no trace behind the span, which is tracing off, as an Attempt
// has none before it runs (spec 039 #3).
func (o *Observation) TraceID() string {
	if o.traced {
		return o.span.SpanContext().TraceID().String()
	}
	return ""
}

// SpanID is the span's id as the API spells it, 16 lower-case hex digits —
// or "", as TraceID.
func (o *Observation) SpanID() string {
	if o.traced {
		return o.span.SpanContext().SpanID().String()
	}
	return ""
}

// End ends the span with what it has. The second call, and every one after
// it, does nothing: `defer step.End()` beside an explicit Fail is fine.
func (o *Observation) End() {
	if !o.ended.CompareAndSwap(false, true) {
		return
	}
	if o.at.IsZero() {
		o.span.End()
		return
	}
	o.span.End(trace.WithTimestamp(o.at))
}

// Fail records the error as an OTel event, sets the status to ERROR with its
// message, and ends the span. It exists because a defer cannot see the
// error the function is about to return, and these are the three lines every
// caller would write.
func (o *Observation) Fail(err error) {
	if err != nil && !o.ended.Load() {
		o.span.RecordError(err)
		o.span.SetStatus(codes.Error, err.Error())
	}
	o.End()
}

// Update writes observation attributes on this span.
func (o *Observation) Update(opts ...UpdateOption) {
	var f fields
	for _, opt := range opts {
		opt.applyUpdate(&f)
	}
	update(o.span, &f)
}

func update(span trace.Span, f *fields) {
	if f.name != "" {
		span.SetName(f.name)
	}
	span.SetAttributes(observationAttributes(f)...)
}

// warnedKinds are the spellings already warned about, so that a step in a
// loop says it once (spec 038 #5). A spelling is remembered only once Init
// has said where warnings go, and at most maxWarnedKinds of them: past that a
// new one warns every time, which is what a kind computed at run time is.
var warnedKinds = struct {
	sync.Mutex
	seen map[string]bool
}{seen: map[string]bool{}}

const maxWarnedKinds = 256

func warnKind(typ string) {
	if observationTypes[typ] {
		return
	}
	ready := initialized()
	warnedKinds.Lock()
	seen := warnedKinds.seen[typ]
	if !seen && ready && len(warnedKinds.seen) < maxWarnedKinds {
		warnedKinds.seen[typ] = true
	}
	warnedKinds.Unlock()
	if !seen {
		def.log().Warn("tracepad: not one of the observation types the store classifies by; "+
			"it will be kept in the observation's metadata", "type", typ)
	}
}

// observationAttributes are the attributes a set of fields writes, whatever
// the call that collected them.
func observationAttributes(f *fields) []attribute.KeyValue {
	var attrs []attribute.KeyValue
	set := func(key, value string) {
		if value != "" {
			attrs = append(attrs, attribute.String(key, value))
		}
	}
	if f.typ != "" {
		warnKind(f.typ)
		set(attrObservationType, f.typ)
	}
	if f.hasInput {
		set(attrInput, dumps(f.input))
	}
	if f.hasOutput {
		set(attrOutput, dumps(f.output))
	}
	if f.hasMetadata {
		set(attrObservationMetadata, dumps(f.metadata))
	}
	set(attrObservationLevel, f.level)
	set(attrObservationStatusMsg, f.statusMessage)
	return attrs
}

// Span opens a step of the trace under the context's current span and
// returns the context carrying it. End it with step.End(), or step.Fail(err).
func Span(ctx context.Context, name string, opts ...SpanOption) (context.Context, *Observation) {
	f := fields{typ: attrObservationTypeDefault}
	for _, opt := range opts {
		opt.applyUpdate(&f)
	}
	return open(ctx, name, &f)
}

// Event is a zero-duration observation: something that happened, not
// something that took time. It is ended at the instant it was opened,
// whenever End is called.
func Event(ctx context.Context, name string, opts ...SpanOption) (context.Context, *Observation) {
	f := fields{typ: attrObservationTypeEvent}
	for _, opt := range opts {
		opt.applyUpdate(&f)
	}
	at := time.Now()
	ctx, o := open(ctx, name, &f, trace.WithTimestamp(at))
	o.at = at
	return ctx, o
}

// open starts the span with the attributes the fields write.
func open(ctx context.Context, name string, f *fields, start ...trace.SpanStartOption) (context.Context, *Observation) {
	start = append(start, trace.WithAttributes(observationAttributes(f)...))
	ctx, span := tracer().Start(ctx, name, start...)
	return ctx, &Observation{span: span, traced: span.SpanContext().IsValid() && !tracingOff()}
}

// Update writes observation attributes on the context's current span,
// whoever started it (spec 017 #11). With no span it logs and does nothing.
func Update(ctx context.Context, opts ...UpdateOption) {
	span, ok := spanOf(ctx, "tracepad.Update")
	if !ok {
		return
	}
	var f fields
	for _, opt := range opts {
		opt.applyUpdate(&f)
	}
	update(span, &f)
}

// spanOf is the context's span when there is one to write on. No span at
// all is the caller's mistake and is logged; a span a sampler dropped is
// not recording and is skipped without a word, or every dropped request
// would log a line per call.
func spanOf(ctx context.Context, caller string) (trace.Span, bool) {
	span := trace.SpanFromContext(ctx)
	if !span.SpanContext().IsValid() {
		def.log().Warn(caller + " outside a span: nothing was written")
		return nil, false
	}
	return span, span.IsRecording()
}

// TraceOption configures UpdateTrace.
type TraceOption func(*traceFields)

type traceFields struct {
	name, userID, sessionID string
	version                 string
	tags                    []string
	metadata                any
	hasTags, hasMetadata    bool
}

// WithTraceName names the trace.
func WithTraceName(name string) TraceOption { return func(f *traceFields) { f.name = name } }

// WithUserID is who the trace is for.
func WithUserID(id string) TraceOption { return func(f *traceFields) { f.userID = id } }

// WithSessionID groups traces into a session.
func WithSessionID(id string) TraceOption { return func(f *traceFields) { f.sessionID = id } }

// WithTraceVersion is the version of this trace's own logic — a pipeline
// revision, a prompt bundle, an experiment arm — beside WithRelease, the
// deployment's version set once at Init (spec 038 #3).
func WithTraceVersion(version string) TraceOption {
	return func(f *traceFields) { f.version = version }
}

// WithTags are the trace's tags.
func WithTags(tags ...string) TraceOption {
	return func(f *traceFields) { f.tags, f.hasTags = tags, true }
}

// WithTraceMetadata is free metadata on the trace, a JSON object.
func WithTraceMetadata(metadata any) TraceOption {
	return func(f *traceFields) { f.metadata, f.hasMetadata = metadata, true }
}

// UpdateTrace writes trace-level attributes on the context's current span
// (spec 017 #11). A request handler rarely holds the root span — the
// framework does — and the one thing it knows is who the user is: these land
// where the handler stands, and the mapper resolves them for the trace. With
// no span it logs and does nothing.
func UpdateTrace(ctx context.Context, opts ...TraceOption) {
	span, ok := spanOf(ctx, "tracepad.UpdateTrace")
	if !ok {
		return
	}
	var f traceFields
	for _, opt := range opts {
		opt(&f)
	}
	var attrs []attribute.KeyValue
	set := func(key, value string) {
		if value != "" {
			attrs = append(attrs, attribute.String(key, value))
		}
	}
	set(attrTraceName, f.name)
	set(attrUserID, f.userID)
	set(attrSessionID, f.sessionID)
	set(attrTraceVersion, f.version)
	if f.hasTags {
		if f.tags == nil {
			f.tags = []string{}
		}
		attrs = append(attrs, attribute.String(attrTraceTags, dumps(f.tags)))
	}
	if f.hasMetadata {
		attrs = append(attrs, attribute.String(attrTraceMetadata, dumps(f.metadata)))
	}
	span.SetAttributes(attrs...)
}
