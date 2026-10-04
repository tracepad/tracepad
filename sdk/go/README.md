# tracepad

The Go package for [Tracepad](https://github.com/tracepad/tracepad) — LLM
observability and evals in a single binary: traces, prompts, datasets and
scores for LLM and agent applications, self-hosted.

It is a thin layer over the OpenTelemetry Go SDK: it owns no transport, no
batching and no context propagation, and it wraps no provider client.
Everything it does is reachable with `go.opentelemetry.io/otel` and `curl` —
this is the one call that points an application at the store, and the ten
lines after it that a person writes the same way every time, in the shape the
OTel API already has: a `context.Context` in, a `context.Context` out.

```sh
go get github.com/tracepad/tracepad/sdk/go
```

Go 1.25+. Four dependencies: `go.opentelemetry.io/otel`,
`go.opentelemetry.io/otel/trace`, `go.opentelemetry.io/otel/sdk` and the OTLP/HTTP
trace exporter.

## Point an application at a store

```go
import tracepad "github.com/tracepad/tracepad/sdk/go"

shutdown, err := tracepad.Init(ctx) // or WithHost("http://localhost:4318"), WithKey("tp-sk-…")
if err != nil {
	log.Fatal(err)
}
defer shutdown(ctx)
```

`TRACEPAD_URL` and `TRACEPAD_API_KEY` are the two variables it reads;
`TRACEPAD_ENVIRONMENT`, `TRACEPAD_RELEASE` and `TRACEPAD_EXPORT_TIMEOUT` are the
ones it can also use. If
the application already has a `TracerProvider` — `otelhttp`, another SDK —
`Init` registers an exporter on it rather than replacing it.

## Three lines

```go
func answer(ctx context.Context, question string) (string, error) {
	ctx, step := tracepad.Span(ctx, "answer", tracepad.WithInput(question)) // a span, with its input
	defer step.End()
	tracepad.UpdateTrace(ctx, tracepad.WithUserID("u-42"), tracepad.WithTags("support"))

	ctx, call := tracepad.Generation(ctx, "chat", tracepad.WithModel("gpt-4o-mini"))
	response, err := client.Chat.Completions.New(ctx, params)
	if err != nil {
		call.Fail(err)
		return "", err
	}
	call.End(tracepad.Result{ // the model, the usage, and the cost as charged
		Model:  response.Model,
		Usage:  tracepad.Usage{"input_tokens": response.Usage.PromptTokens, "output_tokens": response.Usage.CompletionTokens},
		Output: response.Choices[0].Message.Content,
	})

	tracepad.Score(ctx, "helpful", tracepad.WithValue(1)) // against the trace in flight
	return response.Choices[0].Message.Content, nil
}
```

A prompt by label, cached for as long as the server says and served stale when
the server is away:

```go
support, err := tracepad.Prompt(ctx, "support-answer", tracepad.WithLabel("production"))
messages := support.Compile(map[string]any{"product": "Tracepad"}).Messages
```

## What it does not do

No provider-client wrapper, no response reader and no auto-instrumentation:
the OpenTelemetry GenAI instrumentations are the answer there, and they work
because the transport is shared. No price table either — the cost recorded is
the one the provider charged, and what cannot be read is not sent.

Full documentation:
[docs/sdk-go.md](https://github.com/tracepad/tracepad/blob/main/docs/sdk-go.md).

Apache-2.0.
