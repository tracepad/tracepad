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

// Observation types (spec 002 #12). The set is closed by a CHECK constraint
// in schema 0002; richer dialect types collapse into it during mapping.
const (
	TypeSpan       = "span"
	TypeGeneration = "generation"
	TypeEvent      = "event"
)

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
	Tags        []string
	Metadata    map[string]any
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
	StartTime       int64
	EndTime         int64
	Model           string
	ModelParameters map[string]any
	Level           string
	StatusMessage   string
	Usage           map[string]any
	// CostDetails is present only when the client provided cost; there is
	// no price table and no estimation (spec 002 #14).
	CostDetails map[string]any
	Input       any
	Output      any
	Metadata    map[string]any
}

// ProvidedCost reports whether the client supplied cost for this observation.
func (o *Observation) ProvidedCost() bool { return len(o.CostDetails) > 0 }
