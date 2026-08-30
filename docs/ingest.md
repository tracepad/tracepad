# Sending traces to Tracepad

Tracepad speaks OTLP. Any OpenTelemetry-instrumented application can send to
it by changing an endpoint and an auth header — there is no Tracepad SDK to
install.

## Endpoints

| Method | Path | Notes |
|---|---|---|
| `POST` | `/v1/traces` | Canonical OTLP/HTTP trace endpoint |
| `POST` | `/api/public/otel/v1/traces` | Alias, so the Langfuse SDK works unchanged |

Both routes are the same endpoint and accept the same credentials.

- **Body**: `ExportTraceServiceRequest` protobuf.
- **Content-Type**: `application/x-protobuf` (`application/protobuf` is
  accepted too). Anything else is `415`.
- **Content-Encoding**: `gzip` is supported and transparently decoded.
- OTLP/JSON and OTLP over gRPC are not implemented.

## Authentication

Either scheme works on either route:

```
Authorization: Bearer <secret key>
Authorization: Basic base64(<public key>:<secret key>)
```

Keys are printed when a project is created (see the server's startup output).
Unknown credentials get `401 {"error": "unauthorized"}`.

## Connecting an application

**OpenTelemetry SDK** — nothing but environment:

```sh
export OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://localhost:4318/v1/traces
export OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer tp-sk-…"
```

**Langfuse SDK** — point it at Tracepad instead of Langfuse:

```sh
export LANGFUSE_HOST=http://localhost:4318
export LANGFUSE_PUBLIC_KEY=tp-pk-…
export LANGFUSE_SECRET_KEY=tp-sk-…
```

Working examples of both live in [`scripts/smoke`](../scripts/smoke), which is
also the test that keeps them working.

## Responses

| Status | Meaning |
|---|---|
| `200` | Committed to disk. An empty body means everything was accepted; a body carries `partial_success` with the number of skipped spans. |
| `400` | The body is not a decodable OTLP export. |
| `401` | Unknown credentials. |
| `413` | The body is over `TRACEPAD_MAX_BODY_BYTES`. |
| `415` | Wrong `Content-Type`. |
| `429` | The write queue is saturated; retry after the `Retry-After` delay. Standard OTLP exporters do this on their own. |

A `200` means the spans are committed and fsynced, not merely queued.

A span that cannot be mapped — a missing or malformed trace/span id — is
skipped and counted in `partial_success` rather than failing the whole export.
Its bytes are still in the stored raw body, so nothing is lost.

## What Tracepad reads from your spans

Attributes are resolved by a priority chain per field: an explicit
`langfuse.*` attribute beats an OTel GenAI semconv one, which beats a bare
fallback. Resource and scope attributes participate at lower priority than the
span's own.

| Field | Attributes, highest priority first |
|---|---|
| trace name | `langfuse.trace.name` · the root span's name |
| user | `langfuse.user.id` · `user.id` |
| session | `langfuse.session.id` · `session.id` · `gen_ai.conversation.id` |
| environment | `langfuse.environment` · `deployment.environment.name` · `deployment.environment` · `default` |
| release | `langfuse.release` · the resource's `service.version` |
| version | `langfuse.version` |
| tags | `langfuse.trace.tags` (JSON array, comma-separated list, or single value) |
| trace metadata | `langfuse.trace.metadata` (JSON object) and `langfuse.trace.metadata.*` |
| observation type | `langfuse.observation.type` · a model attribute ⇒ `generation` · a zero-duration childless span ⇒ `event` · otherwise `span` |
| completion start | `langfuse.observation.completion_start_time` (RFC 3339, or whole nanoseconds) |
| prompt | `langfuse.observation.prompt.name` and `langfuse.observation.prompt.version` |
| model | `langfuse.observation.model.name` · `gen_ai.request.model` · `gen_ai.response.model` · `llm.model_name` · `model` |
| model parameters | `langfuse.observation.model.parameters` (JSON object) · every `gen_ai.request.*` except the model |
| input | `langfuse.observation.input` · `gen_ai.input.messages` · `gen_ai.prompt` (including the flattened `gen_ai.prompt.0.content` form) |
| output | `langfuse.observation.output` · `gen_ai.output.messages` · `gen_ai.completion` (same flattened form) |
| usage | `langfuse.observation.usage_details` (JSON object) · every `gen_ai.usage.*` count, key kept as sent |
| cost | `langfuse.observation.cost_details` (JSON object) · `gen_ai.usage.cost` |
| level | `langfuse.observation.level` · span status `ERROR` ⇒ `ERROR` · otherwise `DEFAULT` |
| status message | `langfuse.observation.status_message` · the span's status message |
| observation metadata | `langfuse.observation.metadata` and `langfuse.observation.metadata.*`, **plus every attribute no rule above consumed**, plus the span's events under `events`, plus the instrumentation scope's own name and version under `scope.name` and `scope.version` |

Two consequences worth knowing:

- **Nothing is dropped.** An attribute Tracepad does not recognize shows up in
  the observation's metadata — and so does one it *did* recognize but could
  not use: an unknown level spelling, a `cost_details` that is not a JSON
  object, or the runner-up of a chain (`gen_ai.response.model` when
  `gen_ai.request.model` won, since those are two different facts). A new SDK
  convention degrades to visible metadata, never to lost data.
- **Cost is never estimated.** There is no price table. `total_cost` is
  present only when the client sent one; otherwise the UI shows "no data",
  not `$0`.
- **Exceptions count as errors.** OTel records a failure as a span *event*,
  not an attribute. Every event is kept under `metadata.events` with its
  attributes intact — stack traces included — and a span carrying an
  `exception` event is stored at level `ERROR` with the exception's message
  as its status message, so it reaches `error_count` and error filters even
  when the exporter never set an ERROR span status. An explicit
  `langfuse.observation.level` still wins. Span links are not mapped.

### The kind of each step

`type` holds the whole Langfuse vocabulary, stored as your client sent it:
`span`, `generation`, `event`, `agent`, `tool`, `chain`, `retriever`,
`guardrail`, `evaluator`, `embedding`. A spelling outside those ten is kept in
the observation's metadata and the span is classified by the heuristics above.

The trace listing can be asked for the traces that contain one of them
(`?type=tool`). The match is exact: `generation` does not find an `embedding`,
even though the cost and model breakdowns count both as calls to a model.

### Where an attribute came from

The three levels of an OTLP export — Resource, InstrumentationScope and the
span — are one namespace to the priority chains above: `deployment.environment`
sets the environment whether it sits on the resource or on the span.

One name is the exception. `service.version` is read from the Resource only,
because on a span it is the version of whatever that span talked to rather
than of the service that produced the trace; a span carrying one keeps it in
metadata and does not name the release.

What no rule claims keeps its origin instead of being merged:

| It arrived on | It lands in metadata as |
|---|---|
| the span | `<key>` |
| the InstrumentationScope | `scope.<key>` |
| the Resource | `resource.<key>` |

So a `service.name` on the resource and a `service.name` on the span are two
entries — `resource.service.name` and `service.name` — rather than one
overwriting the other. The scope's own name and version, which are not
attributes in OTLP at all, are `scope.name` and `scope.version`: they are what
says which SDK sent the span.

### Time to first token

`langfuse.observation.completion_start_time` is when the first token came
back. Tracepad accepts it as an RFC 3339 instant or as whole nanoseconds, and
it accepts the doubly-quoted form (`"\"2026-08-30T10:15:03.412Z\""`) some SDK
versions put on the wire. Anything else stays in metadata.

From it come two numbers: the observation's own `ttft_ms`, and the trace's,
which is the earliest completion start among its observations minus the moment
the trace started. Both are stored as sent — no clamping and no clock
correction — so a client whose completion start precedes its own span produces
a negative TTFT rather than a silently corrected one.

## Configuration

| Environment variable | Default | Meaning |
|---|---|---|
| `TRACEPAD_STORE_RAW` | `on` | Keep every accepted body (zstd) so mapping can be replayed later |
| `TRACEPAD_MAX_BODY_BYTES` | `20971520` | Request body cap; over it the answer is `413` |

Raw bodies are the reason a mapping bug is fixable after the fact rather than
being data loss. Turning them off makes ingest work exactly the same and
leaves nothing to replay.
