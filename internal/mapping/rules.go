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

// Tracepad's own namespace (spec 014 #2): the run a trace belongs to and the
// dataset item it answered. Trace-level, ranked like `session_id`; any span
// may carry them, the root is the convention. `langfuse.experiment.*` stays
// unclaimed in metadata — one namespace (spec 014, ingest contract).
const (
	tpRunID  = "tracepad.run_id"
	tpItemID = "tracepad.item_id"
)

// The `tracepad` dialect (spec 017 #3): what our own Python package writes
// where the GenAI semantic conventions have no name — a trace name, tags, free
// metadata, an observation kind, a prompt reference. Everything the
// conventions *do* name (`gen_ai.*`, `user.id`, `session.id`,
// `deployment.environment.name`, the resource's `service.version`) it writes
// under their names, which the chains above already read; these are the gaps.
//
// Each key enters its field's chain beside the `langfuse.*` key it mirrors and
// after it: a span carrying both was written by two SDKs, and the older one is
// the one the operator configured first. `langfuse.*` is never written by us.
const (
	tpTraceName              = "tracepad.trace.name"
	tpTraceTags              = "tracepad.trace.tags"
	tpTraceMetadata          = "tracepad.trace.metadata"
	tpTraceVersion           = "tracepad.trace.version"
	tpObsType                = "tracepad.observation.type"
	tpObsLevel               = "tracepad.observation.level"
	tpObsStatusMessage       = "tracepad.observation.status_message"
	tpObsMetadata            = "tracepad.observation.metadata"
	tpObsCompletionStartTime = "tracepad.observation.completion_start_time"
	// Not under `observation.`: the prompt is a fact about the call, and the
	// package names it the way the store's own API does (spec 017 #3).
	tpPromptName    = "tracepad.prompt.name"
	tpPromptVersion = "tracepad.prompt.version"
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

// Priority chains. First non-empty wins and is claimed; a runner-up stays in
// the observation's metadata, because nothing is dropped (spec 002 #11). It
// may be a second SDK's word for the same fact — `tracepad.*` beside the
// `langfuse.*` key it mirrors — or a different fact from the same SDK, such as
// the model that answered beside the one requested.
var (
	// trace.name falls back to the root span's name, which is not an
	// attribute and is handled by the mapper.
	traceNameKeys = []string{lfTraceName, tpTraceName}

	traceUserKeys = []string{lfUserID, "user.id"}

	// gen_ai.conversation.id is the GenAI semconv name for the same idea
	// and is read by the reference implementation.
	traceSessionKeys = []string{lfSessionID, "session.id", "gen_ai.conversation.id"}

	// deployment.environment is the pre-1.0 semconv spelling, still emitted
	// by deployed SDKs; the reference reads both.
	traceEnvironmentKeys = []string{lfEnvironment, "deployment.environment.name", "deployment.environment"}

	traceTagsKeys = []string{lfTraceTags, tpTraceTags}

	// The metadata chains are prefixes rather than single keys: both shapes
	// SDKs use — a JSON object at the bare key and one attribute per entry
	// under it — are collected, and the higher-priority dialect's entries win
	// where the two name the same thing.
	traceMetadataKeys = []string{lfTraceMetadata, tpTraceMetadata}
	obsMetadataKeys   = []string{lfObsMetadata, tpObsMetadata}

	// `service.version` is the OTel resource attribute every plain-OTel
	// app already sets, so the fallback makes the release filter work for
	// people who never heard of the Langfuse dialect (spec 012 #4). It is
	// read from the Resource only — see keyLevel in value.go for why the
	// same name on a span is a different fact.
	traceReleaseKeys = []string{lfRelease, "service.version"}

	// Not `langfuse.observation.version`: that is the observation's own
	// version and stays in metadata (spec 012 #4). The `tracepad` key is the
	// one trace field spec 017 #3 missed (spec 038 #4).
	traceVersionKeys = []string{lfVersion, tpTraceVersion}

	// The run link (spec 014 #2). One key each: there is no dialect to
	// fall back to, and the shape is checked before the key is claimed.
	traceRunKeys  = []string{tpRunID}
	traceItemKeys = []string{tpItemID}

	// Explicit model name beats the requested one beats the one the
	// provider answered with; bare `model` is the last-resort guess.
	obsModelKeys = []string{
		lfObsModel,
		"gen_ai.request.model",
		"gen_ai.response.model",
		"llm.model_name",
		"model",
	}

	obsStatusMessageKeys   = []string{lfObsStatusMessage, tpObsStatusMessage}
	obsLevelKeys           = []string{lfObsLevel, tpObsLevel}
	obsTypeKeys            = []string{lfObsType, tpObsType}
	obsCompletionStartKeys = []string{lfObsCompletionStartTime, tpObsCompletionStartTime}
	obsPromptNameKeys      = []string{lfObsPromptName, tpPromptName}
	obsPromptVersionKeys   = []string{lfObsPromptVersion, tpPromptVersion}

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

// bareUsageKeys is the third usage source (spec 030 #1): the spellings an
// exporter puts straight on the span when nobody normalised its usage —
// Claude Code's four, the Anthropic SDK's own `usage` object, an OpenAI-style
// client's prompt/completion pair. Read only when neither
// `langfuse.observation.usage_details` nor any `gen_ai.usage.*` count is
// there, and each winner is stored under the key as sent.
//
// The set is closed and listed here rather than found by a prefix scan
// (#2): `input_tokens` is exactly the kind of bare word that collides with
// somebody's unrelated attribute, and every attribute the mapping reads is
// named in the table in docs/ingest.md. It grows by a Decision on spec 030,
// not by a hunch.
var bareUsageKeys = []string{
	"input_tokens",
	"output_tokens",
	"total_tokens",
	"cache_read_tokens",
	"cache_creation_tokens",
	"cache_read_input_tokens",
	"cache_creation_input_tokens",
	"prompt_tokens",
	"completion_tokens",
	"reasoning_tokens",
}

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
