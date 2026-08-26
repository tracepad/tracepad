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
| tags | `langfuse.trace.tags` (JSON array, comma-separated list, or single value) |
| trace metadata | `langfuse.trace.metadata` (JSON object) and `langfuse.trace.metadata.*` |
| observation type | `langfuse.observation.type` · a model attribute ⇒ `generation` · a zero-duration childless span ⇒ `event` · otherwise `span` |
| model | `langfuse.observation.model.name` · `gen_ai.request.model` · `gen_ai.response.model` · `llm.model_name` · `model` |
| model parameters | `langfuse.observation.model.parameters` (JSON object) · every `gen_ai.request.*` except the model |
| input | `langfuse.observation.input` · `gen_ai.input.messages` · `gen_ai.prompt` (including the flattened `gen_ai.prompt.0.content` form) |
| output | `langfuse.observation.output` · `gen_ai.output.messages` · `gen_ai.completion` (same flattened form) |
| usage | `langfuse.observation.usage_details` (JSON object) · every `gen_ai.usage.*` count, key kept as sent |
| cost | `langfuse.observation.cost_details` (JSON object) · `gen_ai.usage.cost` |
| level | `langfuse.observation.level` · span status `ERROR` ⇒ `ERROR` · otherwise `DEFAULT` |
| status message | `langfuse.observation.status_message` · the span's status message |
| observation metadata | `langfuse.observation.metadata` and `langfuse.observation.metadata.*`, **plus every attribute no rule above consumed** |

Two consequences worth knowing:

- **Nothing is dropped.** An attribute Tracepad does not recognize shows up in
  the observation's metadata. A new SDK convention degrades to visible
  metadata, never to lost data.
- **Cost is never estimated.** There is no price table. `total_cost` is
  present only when the client sent one; otherwise the UI shows "no data",
  not `$0`.

Langfuse observation types beyond `span`/`generation`/`event` (`agent`,
`tool`, `chain`, `retriever`, `guardrail`, `evaluator`, `embedding`) are
stored as the closest of the three, with the original spelling kept in the
observation's metadata.

## Configuration

| Environment variable | Default | Meaning |
|---|---|---|
| `TRACEPAD_STORE_RAW` | `on` | Keep every accepted body (zstd) so mapping can be replayed later |
| `TRACEPAD_MAX_BODY_BYTES` | `20971520` | Request body cap; over it the answer is `413` |

Raw bodies are the reason a mapping bug is fixable after the fact rather than
being data loss. Turning them off makes ingest work exactly the same and
leaves nothing to replay.
