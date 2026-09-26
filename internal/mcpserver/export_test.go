package mcpserver

import (
	"encoding/json"
	"time"
)

// PanicKindsLimit is panicKindsLimit, for the tests of the panic log.
const PanicKindsLimit = panicKindsLimit

// NewPanicRecorder is a panic log on the given clock, reduced to its one
// operation, so the tests can drive time instead of waiting for it.
func NewPanicRecorder(now func() time.Time) func(method, tool string, recovered any, site string) {
	return (&panicLog{now: now}).record
}

// Summaries are the one-line texts beside the structured results, by the name
// of what they summarize, for the test that feeds them a hostile trace.
var Summaries = map[string]func(json.RawMessage) string{
	"trace list":   summarizeTraceList,
	"search":       summarizeSearch,
	"trace":        summarizeTrace,
	"io":           summarizeIO,
	"session list": summarizeSessionList,
	"session":      summarizeSession,
	"user list":    summarizeUserList,
	"user":         summarizeUser,
	"prompt":       summarizePrompt,
	"scores":       summarizeScores,
	"stats":        summarizeStats,
	"score trends": summarizeScoreTrends,
	"runs":         summarizeRuns,
	"run":          summarizeRun,
}
