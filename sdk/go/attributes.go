package tracepad

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// The vocabulary the package writes (spec 017 #3, spec 033 #3): the OTel
// GenAI semantic conventions where a name exists, `tracepad.*` where none
// does, and `langfuse.*` never. A span this package produced should mean
// the same thing to any OTel backend and to any instrumentation reading
// beside it, and the gaps the conventions leave — a trace name, tags, free
// metadata, an observation kind, a prompt reference — are ours to name
// rather than to borrow from a competitor's dialect.
//
// The `tracepad.*` half of this file is the mapper's table in
// `internal/mapping/rules.go`, and `docs/ingest.md` prints both.
const (
	// GenAI semantic conventions.
	attrRequestModel  = "gen_ai.request.model"
	attrResponseModel = "gen_ai.response.model"
	attrRequestPrefix = "gen_ai.request."
	attrUsagePrefix   = "gen_ai.usage."
	attrUsageCost     = "gen_ai.usage.cost"
	attrInput         = "gen_ai.input.messages"
	attrOutput        = "gen_ai.output.messages"

	// Trace-level names the conventions do have.
	attrUserID         = "user.id"
	attrSessionID      = "session.id"
	attrEnvironment    = "deployment.environment.name"
	attrServiceName    = "service.name"
	attrServiceVersion = "service.version"

	// Tracepad's own, for the facts the conventions have no word for.
	attrTraceName               = "tracepad.trace.name"
	attrTraceTags               = "tracepad.trace.tags"
	attrTraceMetadata           = "tracepad.trace.metadata"
	attrTraceVersion            = "tracepad.trace.version"
	attrObservationType         = "tracepad.observation.type"
	attrObservationLevel        = "tracepad.observation.level"
	attrObservationStatusMsg    = "tracepad.observation.status_message"
	attrObservationMetadata     = "tracepad.observation.metadata"
	attrCompletionStartTime     = "tracepad.observation.completion_start_time"
	attrPromptName              = "tracepad.prompt.name"
	attrPromptVersion           = "tracepad.prompt.version"
	attrObservationTypeDefault  = "span"
	attrObservationTypeEvent    = "event"
	attrObservationTypeGenerate = "generation"

	// The run link (spec 014 #2): trace-level, stamped on every span of an
	// eval by the processor of spec 018, and not a dialect of its own.
	attrRunID  = "tracepad.run_id"
	attrItemID = "tracepad.item_id"
)

// observationTypes are the ten kinds an observation may be (`docs/ingest.md`).
// A spelling outside them is stored in metadata and the span is classified by
// the mapper's heuristics, so the check warns rather than refuses.
var observationTypes = map[string]bool{
	"span": true, "generation": true, "event": true, "agent": true, "tool": true,
	"chain": true, "retriever": true, "guardrail": true, "evaluator": true, "embedding": true,
}

// dumps renders a payload for an attribute.
//
// A string is sent as it is — a plain-text prompt is a plain-text payload,
// not a JSON string of one (spec 015 #12) — and everything else is JSON with
// HTML escaping off. A value the encoder refuses (a channel, a function, a
// cycle) is still a string in the trace, naming its type and the reason: the
// tracing path never fails the step it observes (spec 017 #9). Not
// `fmt.Sprint` of it — that walks a cycle until the stack is gone (found in
// review of PR #69).
func dumps(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return fmt.Sprintf("<%T: %v>", value, err)
	}
	return string(bytes.TrimRight(out.Bytes(), "\n"))
}

// rfc3339 is an instant as the mapper reads it (`docs/ingest.md`, time to
// first token): UTC, millisecond precision, a `Z` suffix.
func rfc3339(at time.Time) string {
	return at.UTC().Format("2006-01-02T15:04:05.000Z")
}
