// Package model holds the trace data model produced by attribute mapping and
// consumed by the store (spec 002). It is deliberately free of both OTLP and
// SQL types so that neither side has to know about the other.
package model

// Observation levels. DEFAULT is the value for a span that says nothing about
// its severity; ERROR is what the trace list counts.
const (
	LevelDebug   = "DEBUG"
	LevelDefault = "DEFAULT"
	LevelWarning = "WARNING"
	LevelError   = "ERROR"
)

// Observation types: the Langfuse vocabulary, kept whole (spec 012 #2). The
// set is closed by a CHECK constraint in schema 0008; a spelling outside it
// is preserved in metadata and left to the heuristics of spec 002 #12.
//
// The first three are what a span with no explicit type can become; the rest
// arrive only when the client named them.
const (
	TypeSpan       = "span"
	TypeGeneration = "generation"
	TypeEvent      = "event"
	TypeAgent      = "agent"
	TypeTool       = "tool"
	TypeChain      = "chain"
	TypeRetriever  = "retriever"
	TypeGuardrail  = "guardrail"
	TypeEvaluator  = "evaluator"
	// TypeEmbedding is a call to a model, so every aggregate that sums
	// model calls counts it beside TypeGeneration (spec 012 #2).
	TypeEmbedding = "embedding"
)

// ObservationTypes is the vocabulary in the order the schema's CHECK lists
// it. It is what validates a `type=` filter and what the clients enumerate,
// so the column and its readers cannot drift apart.
var ObservationTypes = []string{
	TypeSpan, TypeGeneration, TypeEvent, TypeAgent, TypeTool, TypeChain,
	TypeRetriever, TypeGuardrail, TypeEvaluator, TypeEmbedding,
}

// IsObservationType reports whether a string is one of the ten.
func IsObservationType(s string) bool {
	for _, t := range ObservationTypes {
		if t == s {
			return true
		}
	}
	return false
}

// DefaultEnvironment is the environment of a trace whose spans never said
// which environment they came from.
const DefaultEnvironment = "default"

// Trace is the derived, trace-level row. Every field is optional because it
// rides on spans that may arrive in any order, across batches (spec 002 #6);
// the zero value of a field means "this delivery said nothing", not "empty".
type Trace struct {
	ID          string
	Name        string
	UserID      string
	SessionID   string
	Environment string
	// Release is the deployment the trace ran in and Version is the
	// version of the trace's own logic; the SDK distinguishes them, so
	// they are two fields rather than one (spec 012 #4).
	Release  string
	Version  string
	Tags     []string
	Metadata map[string]any
}

// Observation is one mapped span.
type Observation struct {
	TraceID             string
	ID                  string
	ParentObservationID string
	Type                string
	Name                string
	// StartTime and EndTime are Unix nanoseconds, stored as sent
	// (spec 002 #4: no server-side clock correction).
	StartTime int64
	EndTime   int64
	// CompletionStartTime is when the first token came back, in Unix
	// nanoseconds, and zero when the client did not say (spec 012 #3). It
	// too is stored as sent, so a value earlier than StartTime survives as
	// the number that arrived.
	CompletionStartTime int64
	Model               string
	ModelParameters     map[string]any
	Level               string
	StatusMessage       string
	Usage               map[string]any
	// CostDetails is present only when the client provided cost; there is
	// no price table and no estimation (spec 002 #14).
	CostDetails map[string]any
	// PromptName and PromptVersion record the prompt the client said this
	// observation ran, whether or not this store manages it (spec 012 #5).
	// PromptVersion is nil when the client sent a name without a usable
	// version.
	PromptName    string
	PromptVersion *int64
	Input         any
	Output        any
	Metadata      map[string]any
}

// ProvidedCost reports whether the client supplied cost for this observation.
func (o *Observation) ProvidedCost() bool { return len(o.CostDetails) > 0 }
