// Package hook is the door the tracepadtest helpers open into the package's
// process-wide state (spec 040 #6): the package sets these when it loads, the
// helpers call them, and neither is an API of the package itself.
package hook

var (
	// Reset returns the process to never initialised: tracing off.
	Reset func()
	// Keep swaps the score queue for one that hands each score's body to keep
	// instead of posting it.
	Keep func(keep func(map[string]any))
)
