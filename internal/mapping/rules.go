package mapping

import "github.com/tracepad/tracepad/internal/model"

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
	// The deployment a trace ran in and the version of its own logic. The
	// SDK distinguishes them, so they are two targets (spec 012 #4).
	lfRelease = "langfuse.release"
	lfVersion = "langfuse.version"
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
	// When the first token came back — the number a person means by "time
	// to first token" (spec 012 #3).
	lfObsCompletionStartTime = "langfuse.observation.completion_start_time"
	// The prompt this observation ran, as the client labelled it
	// (spec 012 #5).
	lfObsPromptName    = "langfuse.observation.prompt.name"
	lfObsPromptVersion = "langfuse.observation.prompt.version"
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

	// `service.version` is the OTel resource attribute every plain-OTel
	// app already sets, so the fallback makes the release filter work for
	// people who never heard of the Langfuse dialect (spec 012 #4). It is
	// read from the Resource only — see keyLevel in value.go for why the
	// same name on a span is a different fact.
	traceReleaseKeys = []string{lfRelease, "service.version"}

	// Not `langfuse.observation.version`: that is the observation's own
	// version and stays in metadata (spec 012 #4).
	traceVersionKeys = []string{lfVersion}

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

// Span events. OTel records a failure as an event named `exception` rather
// than as attributes, so this is where a plain-OTel app's stack traces live
// (spec 002 Decision 26, 2026-08-27).
const (
	eventException        = "exception"
	eventExceptionMessage = "exception.message"
	eventExceptionType    = "exception.type"
	// metadataEventsKey is where the whole event list lands.
	metadataEventsKey = "events"
)

// The InstrumentationScope's own fields in metadata. They are not attributes
// in OTLP, and they are the answer to "which SDK sent this" (spec 012 #7).
const (
	metadataScopeName    = "scope.name"
	metadataScopeVersion = "scope.version"
)

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

// observationTypes is the Langfuse observation-type vocabulary, stored as
// sent (spec 012 #2, superseding spec 002 Decision 21). Schema 0002 closed
// the column at three values and the mapper collapsed the rest, keeping the
// original spelling in metadata precisely so that schema 0008 could widen it;
// now that it has, a type the client named is the type the column holds and
// nothing has to be recovered from metadata.
//
// The set is the model package's, so the vocabulary the CHECK constraint
// allows, the filter validates and the clients enumerate is one list.
var observationTypes = func() map[string]bool {
	out := make(map[string]bool, len(model.ObservationTypes))
	for _, t := range model.ObservationTypes {
		out[t] = true
	}
	return out
}()
