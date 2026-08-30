// Package otlptest builds the OTLP bodies the ingest tests replay.
//
// The corpus is synthetic: it reproduces the *structure* of exports from the
// SDK families spec 002 targets — attribute names, nesting, value types,
// scope and resource shape — with content invented on the spot. No captured
// traffic and no real content pass through here (spec 002, Testing #1).
// Once live captures exist they replace these builders; the fixture files on
// disk are the contract either way.
package otlptest

import (
	"encoding/hex"
	"encoding/json"
	"strings"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Fixture is one named export body.
type Fixture struct {
	Name          string
	ResourceSpans []*tracepb.ResourceSpans
}

// Base timestamp for every fixture: 2026-08-26T10:00:00Z in Unix
// nanoseconds. Fixed, so goldens never depend on when the tests ran.
const base int64 = 1787738400_000_000_000

const ms = int64(1_000_000)

// Fixtures returns the whole corpus in file order.
func Fixtures() []Fixture {
	return []Fixture{
		langfuseSDKGeneration(),
		genAISemconvChat(),
		genAIFlattenedPrompt(),
		partialInvalidSpans(),
		crossResourceTree(),
		langfuseExtendedTypes(),
		largePayloads(),
		wireColumns(),
	}
}

// 001 — Langfuse Python SDK: trace-level fields and an explicitly typed
// generation, everything stamped as `langfuse.*`.
func langfuseSDKGeneration() Fixture {
	root := span("4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f", "1a2b3c4d5e6f7a8b", "", "handle-request",
		base, base+820*ms,
		str("langfuse.trace.name", "support-chat"),
		str("langfuse.user.id", "user-4821"),
		str("langfuse.session.id", "session-77"),
		str("langfuse.trace.tags", `["support","beta"]`),
		str("langfuse.trace.metadata.channel", "web"),
		i64("langfuse.trace.metadata.retries", 0),
		str("langfuse.environment", "production"),
		str("langfuse.observation.type", "span"),
	)
	generation := span("4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f", "2b3c4d5e6f7a8b9c", "1a2b3c4d5e6f7a8b", "chat-completion",
		base+40*ms, base+780*ms,
		str("langfuse.observation.type", "generation"),
		str("langfuse.observation.model.name", "claude-sonnet-5"),
		str("langfuse.observation.model.parameters", `{"temperature":0.2,"max_tokens":512}`),
		str("langfuse.observation.input", `[{"role":"user","content":"how do I reset my password?"}]`),
		str("langfuse.observation.output", `{"role":"assistant","content":"Open Settings and choose Reset."}`),
		str("langfuse.observation.usage_details", `{"input":128,"output":41,"total":169}`),
		str("langfuse.observation.cost_details", `{"input":0.00038,"output":0.00062}`),
		str("langfuse.observation.metadata.prompt_name", "support-v3"),
	)
	return Fixture{
		Name: "001-langfuse-sdk-generation",
		ResourceSpans: []*tracepb.ResourceSpans{
			resourceSpans(
				[]*commonpb.KeyValue{
					str("service.name", "support-bot"),
					str("telemetry.sdk.language", "python"),
					str("telemetry.sdk.name", "opentelemetry"),
				},
				scope("langfuse-sdk", "3.6.1", root, generation),
			),
		},
	}
}

// 002 — plain OpenTelemetry GenAI semconv: no `langfuse.*` at all, model and
// usage carried by the standard attributes, environment on the resource.
func genAISemconvChat() Fixture {
	chat := span("aa11bb22cc33dd44ee55ff6677889900", "0102030405060708", "", "chat gpt-4o-mini",
		base+5*ms, base+430*ms,
		str("gen_ai.system", "openai"),
		str("gen_ai.operation.name", "chat"),
		str("gen_ai.request.model", "gpt-4o-mini"),
		f64("gen_ai.request.temperature", 0.7),
		i64("gen_ai.request.max_tokens", 256),
		str("gen_ai.response.model", "gpt-4o-mini-2026-04-01"),
		str("gen_ai.response.finish_reasons", "stop"),
		i64("gen_ai.usage.input_tokens", 92),
		i64("gen_ai.usage.output_tokens", 33),
		str("gen_ai.input.messages", `[{"role":"user","parts":[{"type":"text","content":"summarize the changelog"}]}]`),
		str("gen_ai.output.messages", `[{"role":"assistant","parts":[{"type":"text","content":"Three fixes and one new flag."}]}]`),
		str("user.id", "user-1109"),
		str("session.id", "session-12"),
	)
	return Fixture{
		Name: "002-genai-semconv-chat",
		ResourceSpans: []*tracepb.ResourceSpans{
			resourceSpans(
				[]*commonpb.KeyValue{
					str("service.name", "changelog-writer"),
					str("deployment.environment.name", "staging"),
				},
				scope("opentelemetry.instrumentation.openai", "0.42.0", chat),
			),
		},
	}
}

// 003 — the flattened dialect: messages spread one attribute per field, and
// the model name only under the bare fallbacks of the priority table.
func genAIFlattenedPrompt() Fixture {
	call := span("bb22cc33dd44ee55ff66770088990011", "1112131415161718", "", "openai.chat",
		base+2*ms, base+610*ms,
		str("gen_ai.prompt.0.role", "system"),
		str("gen_ai.prompt.0.content", "You answer in one sentence."),
		str("gen_ai.prompt.1.role", "user"),
		str("gen_ai.prompt.1.content", "what changed in v2?"),
		str("gen_ai.completion.0.role", "assistant"),
		str("gen_ai.completion.0.content", "Batching became the default."),
		str("llm.model_name", "mistral-large"),
		i64("gen_ai.usage.prompt_tokens", 61),
		i64("gen_ai.usage.completion_tokens", 12),
		f64("gen_ai.usage.cost", 0.00041),
		str("traceloop.workflow.name", "release-notes"),
	)
	return Fixture{
		Name: "003-genai-flattened-prompt",
		ResourceSpans: []*tracepb.ResourceSpans{
			resourceSpans(
				[]*commonpb.KeyValue{str("service.name", "release-notes")},
				scope("opentelemetry.instrumentation.mistral", "0.19.3", call),
			),
		},
	}
}

// 004 — one usable span among spans that cannot be mapped: OTLP partial
// success, not a rejected batch (spec 002 #13).
func partialInvalidSpans() Fixture {
	good := span("cc33dd44ee55ff6677008899001122aa", "2122232425262728", "", "embed-batch",
		base, base+95*ms,
		str("gen_ai.request.model", "text-embedding-3-small"),
		i64("gen_ai.usage.input_tokens", 512),
	)
	zeroTrace := &tracepb.Span{
		TraceId:           make([]byte, 16),
		SpanId:            mustHex("3132333435363738"),
		Name:              "orphan-zero-trace",
		StartTimeUnixNano: uint64(base),
		EndTimeUnixNano:   uint64(base + 5*ms),
	}
	shortSpanID := &tracepb.Span{
		TraceId:           mustHex("cc33dd44ee55ff6677008899001122aa"),
		SpanId:            []byte{0x01, 0x02},
		Name:              "orphan-short-span-id",
		StartTimeUnixNano: uint64(base),
		EndTimeUnixNano:   uint64(base + 5*ms),
	}
	return Fixture{
		Name: "004-partial-invalid-spans",
		ResourceSpans: []*tracepb.ResourceSpans{
			resourceSpans(
				[]*commonpb.KeyValue{str("service.name", "indexer")},
				scope("opentelemetry.instrumentation.openai", "0.42.0", good, zeroTrace, shortSpanID),
			),
		},
	}
}

// 005 — a tree delivered out of order across two resources: the child
// arrives before its parent, an errored span raises the level, and a
// zero-duration childless span becomes an event (spec 002 #12, edge cases).
func crossResourceTree() Fixture {
	const traceID = "dd44ee55ff6677008899001122aabb33"

	child := span(traceID, "4142434445464748", "3132333435363738", "tool.search",
		base+30*ms, base+120*ms,
		str("langfuse.observation.level", "warning"),
		str("langfuse.observation.status_message", "no results, retrying"),
	)
	failing := span(traceID, "5152535455565758", "3132333435363738", "tool.fetch",
		base+130*ms, base+300*ms)
	failing.Status = &tracepb.Status{
		Code:    tracepb.Status_STATUS_CODE_ERROR,
		Message: "upstream timeout",
	}
	// A plain-OTel app records the failure itself as an event, and the
	// stack trace lives nowhere else.
	failing.Events = []*tracepb.Span_Event{
		ExceptionEvent("TimeoutError", "read timed out after 30s",
			"tools/fetch.py, line 88, in fetch\n  response = session.get(url)"),
	}
	// Same failure shape, but with no span status at all — the case that
	// would silently miss error_count without the promotion rule.
	silent := span(traceID, "7071727374757677", "3132333435363738", "tool.parse",
		base+305*ms, base+308*ms)
	silent.Events = []*tracepb.Span_Event{
		ExceptionEvent("ValueError", "unparseable response", "tools/parse.py, line 12"),
	}
	marker := span(traceID, "6162636465666768", "3132333435363738", "cache.miss",
		base+310*ms, base+310*ms)
	root := span(traceID, "3132333435363738", "", "answer-question",
		base, base+900*ms,
		str("langfuse.trace.metadata", `{"tenant":"acme","plan":"team"}`),
	)

	return Fixture{
		Name: "005-cross-resource-tree",
		ResourceSpans: []*tracepb.ResourceSpans{
			resourceSpans(
				[]*commonpb.KeyValue{
					str("service.name", "agent-worker"),
					str("deployment.environment", "prod"),
				},
				scope("agent-tools", "1.4.0", child, failing, silent, marker),
			),
			resourceSpans(
				[]*commonpb.KeyValue{str("service.name", "agent-api")},
				scope("langfuse-sdk", "3.6.1", root),
			),
		},
	}
}

// 006 — the Langfuse observation-type vocabulary beyond our three types, plus
// scope-level attributes, exercising the collapse of spec 002 Decision 21.
func langfuseExtendedTypes() Fixture {
	const traceID = "ee55ff6677008899001122aabb33cc44"

	agent := span(traceID, "7172737475767778", "", "research-agent",
		base, base+1200*ms,
		str("langfuse.observation.type", "agent"),
	)
	tool := span(traceID, "8182838485868788", "7172737475767778", "web-search",
		base+10*ms, base+200*ms,
		str("langfuse.observation.type", "tool"),
		str("langfuse.observation.input", `{"query":"otlp partial success"}`),
	)
	embedding := span(traceID, "9192939495969798", "7172737475767778", "embed-query",
		base+210*ms, base+260*ms,
		str("langfuse.observation.type", "embedding"),
		str("langfuse.observation.model.name", "text-embedding-3-large"),
		str("langfuse.observation.usage_details", `{"input":18,"total":18}`),
	)
	unknown := span(traceID, "a1a2a3a4a5a6a7a8", "7172737475767778", "policy-gate",
		base+270*ms, base+275*ms,
		str("langfuse.observation.type", "workflow-step"),
	)

	scopeSpans := scope("langfuse-sdk", "3.6.1", agent, tool, embedding, unknown)
	scopeSpans.Scope.Attributes = []*commonpb.KeyValue{str("langfuse.environment", "development")}

	return Fixture{
		Name: "006-langfuse-extended-types",
		ResourceSpans: []*tracepb.ResourceSpans{
			resourceSpans([]*commonpb.KeyValue{str("service.name", "research")}, scopeSpans),
		},
	}
}

// 007 — a generation whose payloads are large enough to meet a response
// budget. Every other fixture logs a sentence or two, which is not the shape
// that exercises truncation markers (spec 004 #2) or the interface's
// load-the-whole-thing affordance (spec 006), so this one carries a document.
func largePayloads() Fixture {
	const traceID = "0071122334455667788990aabbccddee"

	// One paragraph repeated: big enough to be cut by a small budget, boring
	// enough that the golden diff stays readable.
	const paragraph = "The exporter batches spans and flushes them on an interval; " +
		"a span that arrives after its parent has been written is still stored " +
		"and re-parented on read. "
	document, err := json.Marshal([]map[string]string{
		{"role": "system", "content": "Summarise the release notes below."},
		{"role": "user", "content": strings.Repeat(paragraph, 24)},
	})
	if err != nil {
		panic(err)
	}
	summary, err := json.Marshal(map[string]string{
		"role":    "assistant",
		"content": strings.Repeat("Batching became the default. ", 12),
	})
	if err != nil {
		panic(err)
	}

	root := span(traceID, "0102030405060708", "", "summarise-release-notes",
		base, base+2100*ms,
		str("langfuse.trace.name", "summarise-release-notes"),
		str("langfuse.environment", "production"),
		str("langfuse.observation.type", "span"),
	)
	generation := span(traceID, "1112131415161719", "0102030405060708", "summarise",
		base+20*ms, base+2050*ms,
		str("langfuse.observation.type", "generation"),
		str("langfuse.observation.model.name", "claude-sonnet-5"),
		str("langfuse.observation.input", string(document)),
		str("langfuse.observation.output", string(summary)),
		str("langfuse.observation.usage_details", `{"input":2048,"output":96,"total":2144}`),
		str("langfuse.observation.cost_details", `{"input":0.0061,"output":0.0009,"total":0.007}`),
	)

	return Fixture{
		Name: "007-large-payloads",
		ResourceSpans: []*tracepb.ResourceSpans{
			resourceSpans(
				[]*commonpb.KeyValue{str("service.name", "release-notes")},
				scope("langfuse-sdk", "3.6.1", root, generation),
			),
		},
	}
}

// 008 — what the wire already carries (spec 012): the release and version of
// the deployment, the completion start in both of the shapes SDKs send it,
// the prompt a generation ran, and the same attribute name at all three
// levels of the export.
//
// Two resources, because `release` has two sources and they must be seen to
// disagree: the first carries `langfuse.release` beside a `service.version`
// that loses the chain and so keeps its own place in metadata; the second
// carries only `service.version`, which is then what the release is.
func wireColumns() Fixture {
	const withDialect = "ff6677008899001122aabb33cc44dd55"
	const plainOTel = "1122aabb33cc44dd55ee6677008899ff"

	// The double-JSON-encoded instant is what the Langfuse SDK 4.7 puts on
	// the wire; the quotes are part of the string value, not of this
	// literal (spec 012 #3).
	root := span(withDialect, "b1b2b3b4b5b6b7b8", "", "checkout",
		base, base+1500*ms,
		str("langfuse.trace.name", "checkout"),
		str("langfuse.release", "2026.8.30-rc1"),
		str("langfuse.version", "checkout-v9"),
		str("langfuse.observation.type", "span"),
	)
	answer := span(withDialect, "c1c2c3c4c5c6c7c8", "b1b2b3b4b5b6b7b8", "answer",
		base+100*ms, base+1400*ms,
		str("langfuse.observation.type", "generation"),
		str("langfuse.observation.model.name", "claude-sonnet-5"),
		str("langfuse.observation.completion_start_time", `"2026-08-26T10:00:00.488Z"`),
		str("langfuse.observation.prompt.name", "support-answer"),
		i64("langfuse.observation.prompt.version", 7),
		// The observation's own version is not the trace's, and stays
		// where the mapper found it (spec 012 #4).
		str("langfuse.observation.version", "answer-3"),
		// Same key as the resource's and the scope's: three facts that
		// used to be one (spec 012 #7).
		str("service.name", "span-level"),
	)
	lookup := span(withDialect, "d1d2d3d4d5d6d7d8", "b1b2b3b4b5b6b7b8", "catalog-search",
		base+120*ms, base+300*ms,
		str("langfuse.observation.type", "tool"),
		// The other accepted shape: whole nanoseconds.
		i64("langfuse.observation.completion_start_time", base+180*ms),
		// A version that is not an integer: the name is still the
		// prompt, the version stays in metadata (spec 012 #5).
		str("langfuse.observation.prompt.name", "catalog-query"),
		str("langfuse.observation.prompt.version", "latest"),
	)
	gate := span(withDialect, "e1e2e3e4e5e6e7e8", "b1b2b3b4b5b6b7b8", "policy-check",
		base+310*ms, base+330*ms,
		str("langfuse.observation.type", "guardrail"),
		// Neither an instant nor a number: unclaimed, and so visible.
		str("langfuse.observation.completion_start_time", "as soon as it could"),
	)

	scopeSpans := scope("langfuse-sdk", "4.7.0", root, answer, lookup, gate)
	scopeSpans.Scope.Attributes = []*commonpb.KeyValue{str("service.name", "scope-level")}

	// A plain-OTel app that never heard of the dialect: the release comes
	// from the resource attribute it already sets.
	priced := span(plainOTel, "f1f2f3f4f5f6f7f8", "", "price-quote",
		base+200*ms, base+900*ms,
		str("gen_ai.request.model", "gpt-4o-mini"),
		str("langfuse.observation.completion_start_time", `2026-08-26T10:00:00.640Z`),
	)

	return Fixture{
		Name: "008-wire-columns",
		ResourceSpans: []*tracepb.ResourceSpans{
			resourceSpans(
				[]*commonpb.KeyValue{
					str("service.name", "resource-level"),
					str("service.version", "2026.8.3"),
					str("deployment.environment", "production"),
				},
				scopeSpans,
			),
			resourceSpans(
				[]*commonpb.KeyValue{
					str("service.name", "pricing"),
					str("service.version", "1.9.0"),
				},
				scope("opentelemetry.instrumentation.openai", "0.42.0", priced),
			),
		},
	}
}

// SpanWith builds a minimal one-span export carrying the given string
// attributes, for tests that probe a single mapping rule rather than a whole
// dialect. Keys and values alternate.
func SpanWith(keyValues ...string) []*tracepb.ResourceSpans {
	return Export(ProbeSpan(keyValues...))
}

// ProbeSpan is SpanWith's span, for tests that need to decorate it further —
// with events or a status — before exporting it.
func ProbeSpan(keyValues ...string) *tracepb.Span {
	attrs := make([]*commonpb.KeyValue, 0, len(keyValues)/2)
	for i := 0; i+1 < len(keyValues); i += 2 {
		attrs = append(attrs, str(keyValues[i], keyValues[i+1]))
	}
	return span("00112233445566778899aabbccddeeff", "0011223344556677", "", "probe",
		base, base+ms, attrs...)
}

// Export wraps spans in the resource/scope envelope an exporter would.
func Export(spans ...*tracepb.Span) []*tracepb.ResourceSpans {
	return []*tracepb.ResourceSpans{resourceSpans(nil, scope("probe", "0.0.0", spans...))}
}

// Levels describes an export whose three attribute levels are set
// independently, for the tests that are *about* the levels: which one an
// unclaimed attribute keeps its name from, and which one wins a priority
// chain (spec 012 #7, #11). Keys and values alternate in each list.
type Levels struct {
	Resource     []string
	Scope        []string
	ScopeName    string
	ScopeVersion string
	// Spans is one attribute set per span. The first span is the root and
	// the rest are its children — the shape that puts a trace-level
	// attribute on the root while the resource's fallback stays visible to
	// every span below it.
	Spans [][]string
}

// ExportLevels builds the export Levels describes.
func ExportLevels(l Levels) []*tracepb.ResourceSpans {
	const traceID = "aabbccddeeff00112233445566778899"
	const rootID = "1000000000000001"

	spans := make([]*tracepb.Span, 0, len(l.Spans))
	for i, keyValues := range l.Spans {
		id, parent := rootID, ""
		if i > 0 {
			id, parent = hex.EncodeToString([]byte{0x20, 0, 0, 0, 0, 0, 0, byte(i)}), rootID
		}
		spans = append(spans, span(traceID, id, parent, "probe", base, base+ms, pairs(keyValues)...))
	}

	scopeSpans := scope(l.ScopeName, l.ScopeVersion, spans...)
	scopeSpans.Scope.Attributes = pairs(l.Scope)
	return []*tracepb.ResourceSpans{resourceSpans(pairs(l.Resource), scopeSpans)}
}

// pairs turns alternating keys and values into string attributes. An empty
// list yields nil, which is an absent attribute list rather than an empty one.
func pairs(keyValues []string) []*commonpb.KeyValue {
	if len(keyValues) == 0 {
		return nil
	}
	out := make([]*commonpb.KeyValue, 0, len(keyValues)/2)
	for i := 0; i+1 < len(keyValues); i += 2 {
		out = append(out, str(keyValues[i], keyValues[i+1]))
	}
	return out
}

// ExceptionEvent builds the event OTel records when a span fails.
func ExceptionEvent(exceptionType, message, stacktrace string) *tracepb.Span_Event {
	return &tracepb.Span_Event{
		Name:         "exception",
		TimeUnixNano: uint64(base + ms/2),
		Attributes: []*commonpb.KeyValue{
			str("exception.type", exceptionType),
			str("exception.message", message),
			str("exception.stacktrace", stacktrace),
		},
	}
}

// ErrorStatus marks a span as failed the way an exporter would.
func ErrorStatus(message string) *tracepb.Status {
	return &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: message}
}

func resourceSpans(resourceAttrs []*commonpb.KeyValue, scopes ...*tracepb.ScopeSpans) *tracepb.ResourceSpans {
	return &tracepb.ResourceSpans{
		Resource:   &resourcepb.Resource{Attributes: resourceAttrs},
		ScopeSpans: scopes,
	}
}

func scope(name, version string, spans ...*tracepb.Span) *tracepb.ScopeSpans {
	return &tracepb.ScopeSpans{
		Scope: &commonpb.InstrumentationScope{Name: name, Version: version},
		Spans: spans,
	}
}

func span(traceID, spanID, parentID, name string, start, end int64, attrs ...*commonpb.KeyValue) *tracepb.Span {
	s := &tracepb.Span{
		TraceId:           mustHex(traceID),
		SpanId:            mustHex(spanID),
		Name:              name,
		Kind:              tracepb.Span_SPAN_KIND_INTERNAL,
		StartTimeUnixNano: uint64(start),
		EndTimeUnixNano:   uint64(end),
		Attributes:        attrs,
	}
	if parentID != "" {
		s.ParentSpanId = mustHex(parentID)
	}
	return s
}

func str(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_StringValue{StringValue: value},
	}}
}

func i64(key string, value int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_IntValue{IntValue: value},
	}}
}

func f64(key string, value float64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{
		Value: &commonpb.AnyValue_DoubleValue{DoubleValue: value},
	}}
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic("otlptest: bad hex id " + s)
	}
	return b
}
