package mapping

// The mapping table (spec 002 #10): one priority chain per target field,
// explicit `langfuse.*` before OTel GenAI semconv before bare fallbacks.
// Chains are data, so adding a dialect is an edit here rather than a
// refactor. Semantics for the `langfuse.*` dialect are ported from
// Langfuse's OtelIngestionProcessor (MIT — see NOTICE).
//
// Where the reference implementation reads a key the spec's table does not
// name, the reference wins and the extra key is marked below: it is the only
// executable description of what the Langfuse SDKs actually emit
// (spec 002 Decision 19, 2026-08-26).

// Langfuse dialect: trace-level.
const (
	lfTraceName     = "langfuse.trace.name"
	lfTraceTags     = "langfuse.trace.tags"
	lfTraceMetadata = "langfuse.trace.metadata"
	lfUserID        = "langfuse.user.id"
	lfSessionID     = "langfuse.session.id"
	lfEnvironment   = "langfuse.environment"
)

// Langfuse dialect: observation-level.
const (
	lfObsType            = "langfuse.observation.type"
	lfObsMetadata        = "langfuse.observation.metadata"
	lfObsLevel           = "langfuse.observation.level"
	lfObsStatusMessage   = "langfuse.observation.status_message"
	lfObsInput           = "langfuse.observation.input"
	lfObsOutput          = "langfuse.observation.output"
	lfObsModel           = "langfuse.observation.model.name"
	lfObsUsageDetails    = "langfuse.observation.usage_details"
	lfObsCostDetails     = "langfuse.observation.cost_details"
	lfObsModelParameters = "langfuse.observation.model.parameters"
	// Spelling used by the spec's mapping table. The SDKs emit the dotted
	// form above; both are accepted so neither reading loses data.
	lfObsModelParametersAlt = "langfuse.observation.model_parameters"
)

// Priority chains. First non-empty wins; every key in a chain is consumed,
// hit or miss, because they all carry the same meaning.
var (
	// trace.name falls back to the root span's name, which is not an
	// attribute and is handled by the mapper.
	traceNameKeys = []string{lfTraceName}

	traceUserKeys = []string{lfUserID, "user.id"}

	// gen_ai.conversation.id is the GenAI semconv name for the same idea
	// and is read by the reference implementation.
	traceSessionKeys = []string{lfSessionID, "session.id", "gen_ai.conversation.id"}

	// deployment.environment is the pre-1.0 semconv spelling, still emitted
	// by deployed SDKs; the reference reads both.
	traceEnvironmentKeys = []string{lfEnvironment, "deployment.environment.name", "deployment.environment"}

	traceTagsKeys = []string{lfTraceTags}

	// Explicit model name beats the requested one beats the one the
	// provider answered with; bare `model` is the last-resort guess.
	obsModelKeys = []string{
		lfObsModel,
		"gen_ai.request.model",
		"gen_ai.response.model",
		"llm.model_name",
		"model",
	}

	obsStatusMessageKeys = []string{lfObsStatusMessage}
	obsLevelKeys         = []string{lfObsLevel}

	// Input/output chains. The bare `gen_ai.prompt`/`gen_ai.completion`
	// keys are read directly; the flattened `gen_ai.prompt.0.content` form
	// is reassembled separately (see reassemble).
	obsInputKeys  = []string{lfObsInput, "gen_ai.input.messages", "gen_ai.prompt"}
	obsOutputKeys = []string{lfObsOutput, "gen_ai.output.messages", "gen_ai.completion"}
)

// Attribute prefixes collected wholesale rather than by exact key.
const (
	genAIRequestPrefix = "gen_ai.request."
	genAIUsagePrefix   = "gen_ai.usage."
	genAIPromptPrefix  = "gen_ai.prompt"
	genAICompletion    = "gen_ai.completion"
)

// genAIUsageCost is a cost, not a token count: it is kept out of usage and
// feeds the cost chain instead (spec 002 Decision 20, 2026-08-26).
const genAIUsageCost = "gen_ai.usage.cost"

// levelAliases normalizes the many spellings instrumentations use into the
// four levels schema 0002 allows. Ported from the reference implementation;
// an unknown spelling maps to nothing so that the span-status fallback
// applies instead of being masked by DEFAULT.
var levelAliases = map[string]string{
	"DEBUG":    "DEBUG",
	"TRACE":    "DEBUG",
	"VERBOSE":  "DEBUG",
	"DEFAULT":  "DEFAULT",
	"INFO":     "DEFAULT",
	"LOG":      "DEFAULT",
	"NOTICE":   "DEFAULT",
	"OK":       "DEFAULT",
	"SUCCESS":  "DEFAULT",
	"WARNING":  "WARNING",
	"WARN":     "WARNING",
	"ERROR":    "ERROR",
	"FATAL":    "ERROR",
	"CRITICAL": "ERROR",
}

// observationTypeAliases collapses the Langfuse observation-type vocabulary
// onto the three types schema 0002 allows (spec 002 Decision 21,
// 2026-08-26). An embedding call is a model call, so it is a generation;
// every other structural type is a span. The original value is preserved in
// metadata, so nothing is lost and a later spec can widen the column.
var observationTypeAliases = map[string]string{
	"span":       "span",
	"generation": "generation",
	"event":      "event",
	"embedding":  "generation",
	"agent":      "span",
	"tool":       "span",
	"chain":      "span",
	"retriever":  "span",
	"guardrail":  "span",
	"evaluator":  "span",
}
