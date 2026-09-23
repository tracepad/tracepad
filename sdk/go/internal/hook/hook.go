// Package hook is the door the tracepadtest helpers open into the package's
// process-wide state (spec 040 #6): the package sets these when it loads, the
// helpers call them, and neither is an API of the package itself.
package hook

import "go.opentelemetry.io/otel/trace"

var (
	// Reset returns the process to never initialised: tracing off.
	Reset func()
	// Capture resets, then initialises the package over provider with host
	// and key and nothing from the environment: the global provider's tracers
	// follow provider, and each score's body goes to keep, not to a store.
	Capture func(host, key string, provider trace.TracerProvider, keep func(map[string]any)) error
)
