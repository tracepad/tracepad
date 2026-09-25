package mcpserver_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tracepad/tracepad/internal/mcpserver"
)

// The panic log (spec 004 Decision 34), on a clock the test drives: a panic
// reachable without a credential can be sent in a loop, and the log must not
// grow with the loop.

// testClock is a clock that moves only when told to.
type testClock struct{ at time.Time }

func (c *testClock) now() time.Time           { return c.at }
func (c *testClock) advance(by time.Duration) { c.at = c.at.Add(by) }

// TestPanicSummariesAreTimed: the first panic of a kind is logged with its
// stack; repeats are a one-line count at most once a minute; the tool and
// the site are part of the kind, so two bugs are never folded into one.
func TestPanicSummariesAreTimed(t *testing.T) {
	recorded := captureLog(t)
	clock := &testClock{at: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	record := mcpserver.NewPanicRecorder(clock.now)

	record("tools/call", "list_traces", "a value", "store.read:10")
	if log := recorded.String(); strings.Count(log, "stack=") != 1 ||
		!strings.Contains(log, "tool=list_traces") || !strings.Contains(log, "site=store.read:10") {
		t.Fatalf("the first panic was not logged in full:\n%s", log)
	}

	// Five more inside the minute: nothing.
	recorded.Reset()
	for range 5 {
		clock.advance(10 * time.Second)
		record("tools/call", "list_traces", "a value", "store.read:10")
	}
	if log := recorded.String(); log != "" {
		t.Fatalf("repeats inside the minute were logged:\n%s", log)
	}

	// Past the minute: one line counting all six, and no stack.
	clock.advance(15 * time.Second)
	record("tools/call", "list_traces", "a value", "store.read:10")
	log := recorded.String()
	if strings.Count(log, "mcp handler panic repeated") != 1 || !strings.Contains(log, "occurrences=6") ||
		!strings.Contains(log, "since=2026-09-01T12:00:00Z") || strings.Contains(log, "stack=") {
		t.Fatalf("want one summary of six since the first line:\n%s", log)
	}

	// The same type at the same site in another tool, or at another site,
	// is a kind of its own and gets its own stack.
	recorded.Reset()
	record("tools/call", "get_trace", "a value", "store.read:10")
	record("tools/call", "list_traces", "a value", "store.read:20")
	if log := recorded.String(); strings.Count(log, "stack=") != 2 {
		t.Fatalf("two new kinds, want two stacks:\n%s", log)
	}
	if strings.Contains(recorded.String(), "a value") {
		t.Fatal("the panic's value reached the log")
	}
}

// TestPanicKindsAreBounded: past the limit, new kinds are counted together
// under a line that claims no method or type, first hit included, at most
// once a minute.
func TestPanicKindsAreBounded(t *testing.T) {
	recorded := captureLog(t)
	clock := &testClock{at: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	record := mcpserver.NewPanicRecorder(clock.now)

	for i := range mcpserver.PanicKindsLimit {
		record("tools/call", "list_traces", "a value", fmt.Sprintf("site:%d", i))
	}
	if n := strings.Count(recorded.String(), "stack="); n != mcpserver.PanicKindsLimit {
		t.Fatalf("%d stacks for %d kinds", n, mcpserver.PanicKindsLimit)
	}

	recorded.Reset()
	for i := range 1000 {
		record("tools/call", "get_trace", "a value", fmt.Sprintf("other:%d", i))
	}
	log := recorded.String()
	if strings.Count(log, "\n") != 1 || !strings.Contains(log, "mcp handler panics of untracked kinds") ||
		!strings.Contains(log, "occurrences=1") {
		t.Fatalf("want one overflow line for the first untracked panic:\n%s", log)
	}
	for _, claim := range []string{"method=", "panic_type=", "tool=", "again", "stack="} {
		if strings.Contains(log, claim) {
			t.Fatalf("the overflow line claims %q:\n%s", claim, log)
		}
	}

	recorded.Reset()
	clock.advance(time.Minute)
	record("tools/call", "get_trace", "a value", "other:1000")
	if log := recorded.String(); strings.Count(log, "\n") != 1 || !strings.Contains(log, "occurrences=1000") {
		t.Fatalf("want one overflow line counting the thousand since the last:\n%s", log)
	}
}

// brokenRuntimeError is a value that claims to be a runtime error and panics
// when asked what it is.
type brokenRuntimeError struct{}

func (*brokenRuntimeError) RuntimeError() {}
func (*brokenRuntimeError) Error() string { panic("Error() itself panicked") }

// TestPanicLogSurvivesABrokenError: reading a runtime error's message is a
// call into code the panicking side chose, and it does not get to panic past
// the recovery.
func TestPanicLogSurvivesABrokenError(t *testing.T) {
	recorded := captureLog(t)
	record := mcpserver.NewPanicRecorder(time.Now)
	record("tools/call", "list_traces", (*brokenRuntimeError)(nil), "site:1")
	if log := recorded.String(); !strings.Contains(log, "panic_type=*mcpserver_test.brokenRuntimeError") ||
		strings.Contains(log, "panic=") {
		t.Fatalf("want the type and no message:\n%s", log)
	}
}
