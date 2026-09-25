package mcpserver

import "time"

// PanicKindsLimit is panicKindsLimit, for the tests of the panic log.
const PanicKindsLimit = panicKindsLimit

// NewPanicRecorder is a panic log on the given clock, reduced to its one
// operation, so the tests can drive time instead of waiting for it.
func NewPanicRecorder(now func() time.Time) func(method, tool string, recovered any, site string) {
	return (&panicLog{now: now}).record
}
