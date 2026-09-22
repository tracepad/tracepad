# Getting a service's traces into Tracepad

Tracepad is an OTLP/HTTP endpoint. Anything that exports OpenTelemetry spans
over HTTP can write to it; the question is only what the service already has.
Work in this order: look, choose, connect, confirm the first trace arrived,
then add the detail that makes traces useful.

## 1. Look before adding

Search the service for what already traces it:

```sh
grep -rniE "opentelemetry|TracerProvider|langfuse|OTEL_EXPORTER_OTLP|TRACEPAD_HOST" --include="*.py" --include="*.ts" --include="*.js" --include="*.go" --include="*.env*" .
```

A service that already exports through one SDK needs configuration, not a
second SDK: two exporters to the same store send every span twice.

## 2. Choose

| The service has | Do this |
|---|---|
| OpenTelemetry | Environment only — `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` set to the server's URL plus `/v1/traces`, and `OTEL_EXPORTER_OTLP_HEADERS` set to `authorization=Bearer <key>`. |
| The Langfuse SDK | The bridge — `LANGFUSE_HOST` set to the Tracepad URL, `LANGFUSE_PUBLIC_KEY` and `LANGFUSE_SECRET_KEY` to a Tracepad key pair (`tp-pk-…`, `tp-sk-…`). No code changes. |
| Nothing, in Python, Node or Go | The `tracepad` package for that language (below). |
| Nothing, in another language | That language's OpenTelemetry SDK with its OTLP/HTTP exporter, configured as in the first row. |

Two settings people lose an afternoon to:

- **HTTP, never gRPC.** Set `OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf` (or
  `http/json`). An exporter left on gRPC reaches nothing and says nothing.
- **Base or path.** `OTEL_EXPORTER_OTLP_ENDPOINT` is the base URL — the
  exporter appends `/v1/traces` — while `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`
  is the full path. Set one, not both.

## 3. The minimum with the package

| Language | Install | At startup |
|---|---|---|
| Python | `pip install tracepad` | `tracepad.init()` |
| Node | `npm install tracepad @opentelemetry/api` | `tracepad.init()` |
| Go | `go get github.com/tracepad/tracepad/sdk/go` | `shutdown, err := tracepad.Init(ctx)`, then `defer shutdown(ctx)` |

- The packages read **`TRACEPAD_HOST`** and `TRACEPAD_API_KEY` — `HOST`, not
  the CLI's `TRACEPAD_URL`. `TRACEPAD_ENVIRONMENT` and `TRACEPAD_RELEASE` name
  the deployment and its version; a variant inside one release — an
  experiment arm, a prompt bundle — is the trace's own version, set where the
  trace is updated. Without a host or a key `init`
  fails loudly, which is the point: do not catch it into silence.
- `init` joins a `TracerProvider` the application already has instead of
  building a second one.
- Wrap each model call in the package's generation block, so the model, the
  token usage, the price, the input and the output land on one observation.
- Put the key in the service's secret store or its untracked environment,
  never in a committed file.
- A short-lived process — a script, a test, a CLI — must flush before it
  exits: the exporter batches. Python flushes at exit on its own; in Node call
  `await tracepad.flush()`; in Go, `shutdown` is the flush.

The shapes of the generation block, scores, prompts and streams are in each
package's own documentation — read it rather than writing them from memory:
[Python](https://github.com/tracepad/tracepad/blob/main/docs/sdk-python.md),
[Node](https://github.com/tracepad/tracepad/blob/main/docs/sdk-js.md),
[Go](https://github.com/tracepad/tracepad/blob/main/docs/sdk-go.md). Where a
span attribute is read from, and how the price and time-to-first-token are
carried from any SDK, is in
[the ingest guide](https://github.com/tracepad/tracepad/blob/main/docs/ingest.md).

## 4. Confirm the first trace arrived

Run the code path once, wait a few seconds for the batch, then:

```sh
tracepad traces ls --since 10m --limit 5
tracepad traces last --since 10m --full
```

Read the tree: the generation carries a model, usage, and a cost if the
provider reports one; the trace carries the environment you set. Nothing
there after half a minute — check, in this order:

1. The process flushed before it exited.
2. The protocol is HTTP and the endpoint is base-or-path as above.
3. The key is for the project you are reading with.
4. The server counted the batch at all — `tracepad system` shows ingest since
   the server started, per dialect, with the spans it skipped.

## 5. Then the detail

What makes a trace answer questions later, roughly in order of value:

- The **environment** and **release** on every process: filters, statistics
  and retention are per environment, and a release ties a change in cost or
  quality to a deploy.
- A **trace version** when two variants of the logic run in one release, so
  the listing can set one against the other.
- A **user id** and a **session id** where the application has them.
- The **prompt name and version** on the generation that ran it, so "which
  prompt produced this" has an answer.
- The **price** as the provider reported it: Tracepad records the cost the
  client sends and never computes one, so a call without it shows no cost.
- The **first-token time** of a streamed call, for time-to-first-token.

Deleting traces from application code is also in the packages — and follows
the dry-run rule of SKILL.md whoever calls it.
