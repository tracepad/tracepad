# Sending traces to Tracepad

Tracepad speaks OTLP. Any OpenTelemetry-instrumented application can send to
it by changing an endpoint and an auth header — nothing has to be installed.
There *is* a Python package ([sdk-python.md](sdk-python.md)), and it is a
convenience over exactly this endpoint, not a way around it.

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

**The `tracepad` package** — the same exporter with the ergonomics on top
([sdk-python.md](sdk-python.md)):

```sh
pip install tracepad
export TRACEPAD_HOST=http://localhost:4318
export TRACEPAD_API_KEY=tp-sk-…
```

Working examples of all three live in [`scripts/smoke`](../scripts/smoke),
which is also the test that keeps them working.

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
`langfuse.*` attribute beats the `tracepad.*` one that mirrors it, which beats
an OTel GenAI semconv one, which beats a bare fallback. Resource and scope
attributes participate at lower priority than the span's own.

| Field | Attributes, highest priority first |
|---|---|
| trace name | `langfuse.trace.name` · `tracepad.trace.name` · the root span's name |
| user | `langfuse.user.id` · `user.id` |
| session | `langfuse.session.id` · `session.id` · `gen_ai.conversation.id` |
| environment | `langfuse.environment` · `deployment.environment.name` · `deployment.environment` · `default` |
| release | `langfuse.release` · the resource's `service.version` |
| version | `langfuse.version` |
| run | `tracepad.run_id` (a 32-hex run id; see [The run link](#the-run-link)) |
| item | `tracepad.item_id` (a 32-hex item id, claimed only beside a run id) |
| tags | `langfuse.trace.tags` · `tracepad.trace.tags` (JSON array, comma-separated list, or single value) |
| trace metadata | `langfuse.trace.metadata` and `langfuse.trace.metadata.*` · `tracepad.trace.metadata` and `tracepad.trace.metadata.*` (JSON objects, merged) |
| observation type | `langfuse.observation.type` · `tracepad.observation.type` · a model attribute ⇒ `generation` · a zero-duration childless span ⇒ `event` · otherwise `span` |
| completion start | `langfuse.observation.completion_start_time` · `tracepad.observation.completion_start_time` (RFC 3339, or whole nanoseconds) |
| prompt | `langfuse.observation.prompt.name`/`.version` · `tracepad.prompt.name`/`tracepad.prompt.version` (a whole number from 1; anything else stays in metadata and the name is still recorded) |
| model | `langfuse.observation.model.name` · `gen_ai.request.model` · `gen_ai.response.model` · `llm.model_name` · `model` |
| model parameters | `langfuse.observation.model.parameters` (JSON object) · every `gen_ai.request.*` except the model |
| input | `langfuse.observation.input` · `gen_ai.input.messages` · `gen_ai.prompt` (including the flattened `gen_ai.prompt.0.content` form) |
| output | `langfuse.observation.output` · `gen_ai.output.messages` · `gen_ai.completion` (same flattened form) |
| usage | `langfuse.observation.usage_details` (JSON object) · every `gen_ai.usage.*` count, key kept as sent |
| cost | `langfuse.observation.cost_details` (JSON object) · `gen_ai.usage.cost` |
| level | `langfuse.observation.level` · `tracepad.observation.level` · span status `ERROR` ⇒ `ERROR` · otherwise `DEFAULT` |
| status message | `langfuse.observation.status_message` · `tracepad.observation.status_message` · the span's status message |
| observation metadata | `langfuse.observation.metadata` and `langfuse.observation.metadata.*` · the same two under `tracepad.`, **plus every attribute no rule above consumed**, plus the span's events under `events`, plus the instrumentation scope's own name and version under `scope.name` and `scope.version` |

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

### The `tracepad` dialect

The GenAI semantic conventions have no name for a trace name, for tags, for
free metadata, for the kind of a step or for the prompt one ran. Where they do
have a name, the [Python package](sdk-python.md) uses it — `gen_ai.*`,
`user.id`, `session.id`, `deployment.environment.name`, the resource's
`service.version` — and where they do not, it writes these:

| Attribute | What it sets |
|---|---|
| `tracepad.trace.name` | the trace's name |
| `tracepad.trace.tags` | its tags, as a JSON array |
| `tracepad.trace.metadata` | its metadata, as a JSON object |
| `tracepad.observation.type` | the [kind](#the-kind-of-each-step) of this step |
| `tracepad.observation.level` | its level |
| `tracepad.observation.status_message` | why |
| `tracepad.observation.metadata` | its metadata, as a JSON object |
| `tracepad.observation.completion_start_time` | when the first token came back |
| `tracepad.prompt.name`, `tracepad.prompt.version` | the prompt it ran |

Each sits at the same rank as the `langfuse.*` key it mirrors, and a span
carrying both resolves to the `langfuse.*` one — a span written by two SDKs was
configured by the operator in that order. All of them are read at any of the
three levels of an export, and none of them is a signature: a `tracepad.*` key
on a span some other SDK exported is claimed exactly the same, because the
dialect is a vocabulary rather than a marker.

`tracepad.run_id` and `tracepad.item_id` are the exception. They are
[the run link](#the-run-link), stamped by an eval harness in any language over
whatever SDK the application already runs, so they say nothing about who wrote
the span and do not label a batch `tracepad`.

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

### The run link

An eval harness ties each trace to the run it belongs to and the dataset item
it answered with two attributes ([datasets.md](datasets.md)):

```
tracepad.run_id  = 0e5a7c1d2b3f4a6980c1d2e3f4a5b6c7
tracepad.item_id = a1b2c3d4e5f60718293a4b5c6d7e8f90
```

They are trace-level, resolved like `session.id`: any span may carry them and
the root span is the convention, at any of the three levels. Both must be the
32 lower-case hex characters the API handed out. Any other shape — a run's
name, a case's label — is left in metadata unclaimed and sets neither column,
and an item without a run on the same trace is left in metadata too: an item
is a position inside a run. One namespace: `langfuse.experiment.*` is not
mapped and stays in metadata.

The run is not required to exist. A trace naming a run this project does not
have is stored with its columns as sent, counted under
`runs.orphan_traces` in `GET /api/v1/system`, and logged once per unknown id
per process — so a harness that stamps a wrong id finds out before it reads an
empty run. What the link costs the write path is one primary-key lookup per
trace that carries it, paid by eval traffic only.

A trace re-delivered with a different `tracepad.run_id` moves to the new run:
per-field upsert, last delivery wins, like every other trace field.

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

The trace's is null when no observation carried a completion start, and also
when none of them said when it started: a wait needs a moment to be measured
from, and a trace like that has no latency either.

## Configuration

| Environment variable | Default | Meaning |
|---|---|---|
| `TRACEPAD_STORE_RAW` | `on` | Keep every accepted body (zstd) so mapping can be replayed later |
| `TRACEPAD_MAX_BODY_BYTES` | `20971520` | Request body cap; over it the answer is `413` |

Raw bodies are the reason a mapping bug is fixable after the fact rather than
being data loss. Turning them off makes ingest work exactly the same and
leaves nothing to replay.
