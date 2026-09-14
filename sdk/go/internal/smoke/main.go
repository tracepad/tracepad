// Command smoke exports one trace with the Go package into the binary the
// smoke test started (spec 033 #11): the drift detector for our own
// conventions, installed from this checkout so that what it writes is what
// the mapper of the same commit has to read. It prints nothing but the trace
// id, into the file named on the command line, for check.py.
package main

import (
	"context"
	"fmt"
	"os"

	tracepad "github.com/tracepad/tracepad/sdk/go"
)

func run(target string) error {
	ctx := context.Background()
	shutdown, err := tracepad.Init(ctx,
		tracepad.WithHost(os.Getenv("SMOKE_HOST")), tracepad.WithKey(os.Getenv("SMOKE_SECRET_KEY")),
		tracepad.WithEnvironment("smoke"), tracepad.WithRelease("smoke-1"))
	if err != nil {
		return err
	}
	ctx, workflow := tracepad.Span(ctx, "smoke-workflow", tracepad.WithInput(map[string]any{"question": "ping"}))
	tracepad.UpdateTrace(ctx, tracepad.WithTraceName("tracepad-go-smoke"), tracepad.WithUserID("smoke-user"),
		tracepad.WithSessionID("smoke-session"), tracepad.WithTags("smoke", "spec-033"),
		tracepad.WithTraceMetadata(map[string]any{"suite": "smoke"}))
	_, call := tracepad.Generation(ctx, "smoke-generation", tracepad.WithModel("claude-sonnet-5"),
		tracepad.WithModelParameters(map[string]any{"temperature": 0.1, "max_tokens": 64}),
		tracepad.WithInput([]tracepad.Message{{Role: "user", Content: "ping"}}))
	call.FirstToken()
	cost := 0.0003
	call.End(tracepad.Result{Model: "claude-sonnet-5-2026-08-01", Usage: tracepad.Usage{"input_tokens": 11, "output_tokens": 5},
		Cost: &cost, Output: "pong"})
	workflow.End()
	if err := shutdown(ctx); err != nil {
		return err
	}
	if err := os.WriteFile(target, []byte(workflow.TraceID()), 0o644); err != nil {
		return err
	}
	fmt.Printf("tracepad (Go) trace %s exported to %s\n", workflow.TraceID(), os.Getenv("SMOKE_HOST"))
	return nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: smoke <trace id file>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
