// Package tracepad is the ergonomics of tracing an LLM application in Go,
// over the OpenTelemetry SDK.
//
//	shutdown, err := tracepad.Init(ctx) // TRACEPAD_URL / TRACEPAD_API_KEY
//	defer shutdown(ctx)
//
//	ctx, step := tracepad.Span(ctx, "answer", tracepad.WithInput(question))
//	defer step.End()
//
//	ctx, gen := tracepad.Generation(ctx, "chat", tracepad.WithModel("gpt-4o-mini"))
//	response, err := client.Chat.Completions.New(ctx, params)
//	if err != nil {
//		gen.Fail(err)
//		return err
//	}
//	gen.End(tracepad.Result{Model: response.Model, Usage: usage, Output: text})
//
//	tracepad.Score(ctx, "helpful", tracepad.WithValue(1)) // against the trace in flight
//
// Everything this package does is reachable with the OpenTelemetry SDK and
// curl — see docs/sdk-go.md and docs/ingest.md in the repository. It owns no
// transport, no batching, no retry and no context propagation; those are the
// OTel SDK's, and it wraps no provider client. A context.Context goes in and
// a context.Context comes out, the way the OTel API itself is shaped, so a
// span opened here is the parent of whatever otelhttp or another SDK opens
// next, and nothing is global that the language would not forgive.
package tracepad
