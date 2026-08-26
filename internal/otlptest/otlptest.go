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
				scope("agent-tools", "1.4.0", child, failing, marker),
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

// SpanWith builds a minimal one-span export carrying the given string
// attributes, for tests that probe a single mapping rule rather than a whole
// dialect. Keys and values alternate.
func SpanWith(keyValues ...string) []*tracepb.ResourceSpans {
	attrs := make([]*commonpb.KeyValue, 0, len(keyValues)/2)
	for i := 0; i+1 < len(keyValues); i += 2 {
		attrs = append(attrs, str(keyValues[i], keyValues[i+1]))
	}
	probe := span("00112233445566778899aabbccddeeff", "0011223344556677", "", "probe",
		base, base+ms, attrs...)
	return []*tracepb.ResourceSpans{resourceSpans(nil, scope("probe", "0.0.0", probe))}
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
