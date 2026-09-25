package mcpserver

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// TestPanicLogStaysBounded: however many kinds of panic a caller finds, the
// log grows with the logarithm of the loop and the memory with a constant.
// The first panicKindsLimit kinds get their stack; past that, new kinds are
// counted together and summarised at powers of two.
func TestPanicLogStaysBounded(t *testing.T) {
	var recorded bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&recorded, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	var log panicLog
	const extra = 1000
	for i := range panicKindsLimit + extra {
		log.record(fmt.Sprintf("method/%d", i), "a value")
	}

	if n := len(log.kinds); n > panicKindsLimit+1 {
		t.Fatalf("remembers %d kinds, want at most %d", n, panicKindsLimit+1)
	}
	out := recorded.String()
	if n := strings.Count(out, "stack="); n != panicKindsLimit {
		t.Fatalf("%d stacks logged, want one per remembered kind (%d)", n, panicKindsLimit)
	}
	// The overflow entry speaks at occurrences 1, 2, 4 … 512: ten lines for
	// a thousand panics.
	if n := strings.Count(out, "mcp handler panicked again"); n != 10 {
		t.Fatalf("%d overflow lines for %d panics, want 10", n, extra)
	}
	if strings.Contains(out, "a value") {
		t.Fatal("the panic's value reached the log")
	}
}
