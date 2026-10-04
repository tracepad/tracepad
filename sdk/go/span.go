package tracepad

import (
	"context"
	"encoding"
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
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

// WithMetadata is free metadata on the observation: a map, or anything that
// encodes as a JSON object. It is written one attribute per top-level key, so
// that an Update adds its keys to the ones the span carries and replaces only
// its own; a nil value writes nothing, and deletes nothing (spec 042 #5).
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
	costly(span, f)
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

// observationAttributes are the cheap attributes a set of fields writes,
// whatever the call that collected them: what a span starts with, where a
// sampler or a span processor may read them.
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
	set(attrObservationLevel, f.level)
	set(attrObservationStatusMsg, f.statusMessage)
	return attrs
}

// costly writes the attributes that cost a serialisation, after the span
// exists and only when it records: with tracing off, or the span sampled
// out, nothing is serialised (spec 042 #1).
func costly(span trace.Span, f *fields) {
	if !span.IsRecording() {
		return
	}
	var attrs []attribute.KeyValue
	set := func(key, text string) {
		if text != "" {
			attrs = append(attrs, attribute.String(key, text))
		}
	}
	if f.hasInput {
		set(attrInput, dumps(f.input))
	}
	if f.hasOutput {
		set(attrOutput, dumps(f.output))
	}
	if f.hasMetadata {
		attrs = append(attrs, metadataAttributes(f.metadata)...)
	}
	for _, key := range slices.Sorted(maps.Keys(f.parameters)) {
		attrs = append(attrs, scalar(attrRequestPrefix+key, f.parameters[key]))
	}
	span.SetAttributes(attrs...)
}

// maxMetadataKeys is how many keys one write splits: past it the metadata is
// written whole, as one attribute. A span holds 128 attributes by default
// (OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT), and when it is full the ones dropped are
// the step's own, End's result among them (found in review of PR #83).
const maxMetadataKeys = 32

// metadataAttributes are metadata one attribute per top-level key, in key
// order, a value kept as the scalar it is or as JSON (spec 042 #5). Metadata
// that is no JSON object, has more than maxMetadataKeys keys, or a key the
// per-key form cannot name — an empty one — is written whole under the one key.
func metadataAttributes(metadata any) []attribute.KeyValue {
	entries := map[string]any{}
	encoded := ""
	whole := func() []attribute.KeyValue {
		if encoded == "" {
			encoded = dumps(metadata)
		}
		return []attribute.KeyValue{attribute.String(attrObservationMetadata, encoded)}
	}
	v := reflect.ValueOf(metadata)
	_, marshals := metadata.(json.Marshaler)
	_, texts := metadata.(encoding.TextMarshaler)
	if v.Kind() == reflect.Map && v.Type().Key().Kind() == reflect.String && !marshals && !texts {
		for _, key := range v.MapKeys() {
			entries[key.String()] = v.MapIndex(key).Interface()
		}
	} else {
		// As it encodes — a map type's own MarshalJSON included — with the
		// numbers as they were spelled, so that an int64 id past 2^53 is not
		// rounded through a float64 (found in review of PR #83).
		encoded = dumps(metadata)
		decoder := json.NewDecoder(strings.NewReader(encoded))
		decoder.UseNumber()
		if err := decoder.Decode(&entries); err != nil {
			return whole()
		}
	}
	if _, empty := entries[""]; empty || len(entries) > maxMetadataKeys {
		return whole()
	}
	var attrs []attribute.KeyValue
	for _, key := range slices.Sorted(maps.Keys(entries)) {
		if value := entries[key]; !isNil(value) {
			attrs = append(attrs, scalar(attrObservationMetadata+"."+key, value))
		}
	}
	return attrs
}

// isNil is a value that is nothing: nil itself, or a nil pointer, map,
// slice or interface inside one — a *string left unset writes nothing, as
// nil does, rather than the text "null".
func isNil(value any) bool {
	switch v := reflect.ValueOf(value); v.Kind() {
	case reflect.Invalid:
		return true
	case reflect.Pointer, reflect.Map, reflect.Slice:
		return v.IsNil()
	}
	return false
}

// Span opens a step of the trace under the context's current span and
// returns the context carrying it. End it with step.End(), or step.Fail(err).
func Span(ctx context.Context, name string, opts ...SpanOption) (context.Context, *Observation) {
	f := fields{typ: attrObservationTypeDefault}
	for _, opt := range opts {
		opt.applyUpdate(&f)
	}
	o := &Observation{}
	return open(ctx, name, &f, o), o
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
	o := &Observation{at: at}
	return open(ctx, name, &f, o, trace.WithTimestamp(at)), o
}

// open starts the span with the attributes the fields write, into o — the
// one place an Observation, or the one a Call embeds, is made.
func open(ctx context.Context, name string, f *fields, o *Observation, start ...trace.SpanStartOption) context.Context {
	start = append(start, trace.WithAttributes(observationAttributes(f)...))
	ctx, o.span = tracer().Start(ctx, name, start...)
	costly(o.span, f)
	o.traced = o.span.SpanContext().IsValid() && !tracingOff()
	return ctx
}

// Update writes observation attributes on the context's current span,
// whoever started it (spec 017 #11). With no span in a process that traces
// it warns and does nothing; on a span that does not record it does nothing.
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
// all, in a process that traces, is the caller's mistake and is warned
// about; a span that does not record — tracing off, or a sampler's choice —
// is a configuration, and says so at debug, or every dropped request would
// warn a line per call (spec 042 #2).
func spanOf(ctx context.Context, caller string) (trace.Span, bool) {
	span := trace.SpanFromContext(ctx)
	switch {
	case span.IsRecording():
		return span, true
	case !span.SpanContext().IsValid() && !tracingOff():
		def.log().Warn(caller + " outside a span: nothing was written")
	default:
		def.log().Debug(caller + ": the span does not record; nothing was written")
	}
	return nil, false
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
// where the handler stands, and the mapper resolves them for the trace. Calls
// on one span add up (spec 033 #22): the tags join the ones the package wrote
// there, in the order first seen, and the metadata adds its keys, the later
// value winning. With no span it logs and does nothing.
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
	span.SetAttributes(attrs...)
	if f.hasTags || f.hasMetadata {
		writeTrace(span, &f)
	}
}
