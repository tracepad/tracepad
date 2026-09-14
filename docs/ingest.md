# Sending traces to Tracepad

Tracepad speaks OTLP. Any OpenTelemetry-instrumented application can send to
it by changing an endpoint and an auth header — nothing has to be installed.
There *are* packages for Python ([sdk-python.md](sdk-python.md)), Node
([sdk-js.md](sdk-js.md)) and Go ([sdk-go.md](sdk-go.md)), and each is a
convenience over exactly this
endpoint, not a way around it.

## Endpoints

| Method | Path | Notes |
|---|---|---|
| `POST` | `/v1/traces` | Canonical OTLP/HTTP trace endpoint |
| `POST` | `/api/public/otel/v1/traces` | Alias, so the Langfuse SDK works unchanged |

Both routes are the same endpoint and accept the same credentials.

- **Body**: `ExportTraceServiceRequest`, in either OTLP encoding.
- **Content-Type**: `application/x-protobuf` (`application/protobuf` is
  accepted too), or `application/json` for the OTLP/JSON encoding — see
  [The JSON encoding](#the-json-encoding). Anything else is `415`.
- **Content-Encoding**: `gzip` is supported and transparently decoded, in
  either encoding.
- OTLP over gRPC is not implemented.

## The JSON encoding

Any OpenTelemetry SDK can send JSON instead of protobuf:

```sh
export OTEL_EXPORTER_OTLP_PROTOCOL=http/json
```

Same endpoints, same credentials, same mapping — the encoding is transport, not
meaning, and a span arrives as the same row either way. The response comes back
in the encoding the request was sent in, so a JSON export gets a JSON
`ExportTraceServiceResponse` with `partialSuccess` when spans were skipped.

Two things the OTLP/JSON encoding prescribes and that Tracepad holds you to:

- **Ids are hex, not base64.** `traceId`, `spanId`, `parentSpanId` and the ids
  inside a span link are hex strings — `"4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f"`,
  not `"T4wdLjpbbH2OnwobLD1OXw=="`. This is the one place OTLP/JSON departs
  from protobuf-JSON, and it is the departure every collector implements; a
  base64 id is a `400` that names the field. Accepting both would make one body
  mean two things.
- **64-bit integers may be strings or numbers.** `"startTimeUnixNano":
  "1787738400000000000"` and `"startTimeUnixNano": 1787738400000000000` are
  both read, exactly.

Unknown fields are ignored, as they are on the protobuf path: an exporter on a
newer OTLP version keeps working.

A JSON batch is **archived as JSON**, under its own content type, and
[`tracepad export --otlp`](export.md) replays it as JSON. The archive is what
arrived; converting at ingest would make it the converter's output instead, and
a bug in that conversion would be unfixable because the original would be gone.

```sh
curl -X POST http://localhost:4318/v1/traces \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer tp-sk-…" \
  --data @export.json
```

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

**The `tracepad` package** — the same exporter with the ergonomics on top, in
Python ([sdk-python.md](sdk-python.md)) and in Node ([sdk-js.md](sdk-js.md)):

```sh
pip install tracepad        # or: npm install tracepad @opentelemetry/api
export TRACEPAD_HOST=http://localhost:4318
export TRACEPAD_API_KEY=tp-sk-…
```

Working examples of all of them live in [`scripts/smoke`](../scripts/smoke),
which is also the test that keeps them working.

One more is not an SDK at all: [Claude Code](#claude-code) exports its own
sessions, and needs only environment too.

## Claude Code

Claude Code emits OpenTelemetry spans for what it does — one
`claude_code.interaction` per prompt, one `claude_code.llm_request` per API
call, one `claude_code.tool` per tool call — and Tracepad is an OTLP endpoint,
so connecting the two is environment and nothing else:

```sh
export CLAUDE_CODE_ENABLE_TELEMETRY=1
export CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1
export OTEL_TRACES_EXPORTER=otlp
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318
export OTEL_EXPORTER_OTLP_HEADERS="Authorization=Bearer tp-sk-…"
```

The same six as the `env` object of `~/.claude/settings.json`, which is how they
outlive the shell that set them:

```json
{
  "env": {
    "CLAUDE_CODE_ENABLE_TELEMETRY": "1",
    "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA": "1",
    "OTEL_TRACES_EXPORTER": "otlp",
    "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
    "OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318",
    "OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Bearer tp-sk-…"
  }
}
```

Two of those lines are the ones people lose an afternoon to:

- `OTEL_EXPORTER_OTLP_PROTOCOL` is **not optional**. Tracepad speaks OTLP over
  HTTP and [not over gRPC](#endpoints), and an exporter left to its own default
  does not reach it — the CLI reports nothing, and the traces list simply stays
  empty.
- `OTEL_EXPORTER_OTLP_ENDPOINT` is the **base** URL, not the trace path: the
  exporter appends `/v1/traces` itself. It is the one place in this page where
  the path is not written out.

A day of prompts is a lot of traces to have arrive beside your application's,
so give them an environment of their own — the filter, the stats and the
retention policy are all per environment:

```sh
export OTEL_RESOURCE_ATTRIBUTES=deployment.environment=claude-code
```

**What arrives.** One trace per prompt, named `claude_code.interaction`, with
the API calls and the tool calls under it: a `claude_code.tool` span carries a
`claude_code.tool.blocked_on_user` child while it waits for your answer to the
permission prompt and a `claude_code.tool.execution` child for the work itself.
Each `claude_code.llm_request` is a generation on the model it called, and its
four token counts — `input_tokens`, `output_tokens`, `cache_read_tokens` and
`cache_creation_tokens`, the last two usually the largest numbers on the
span — arrive as usage, under those names. Everything else Claude Code stamps
(`tool_name`, `stop_reason`, `duration_ms`, `span.type`) is in the
observation's metadata. The session id is the CLI session, so a day's prompts
group on the Sessions screen; the release is the CLI's own version.

What does *not* arrive is the conversation. Claude Code redacts the prompt
before it exports anything — `user_prompt` is the literal string `<REDACTED>`,
beside a `user_prompt_length` — and no completion text is on the spans at all,
so the input and output panels of these observations are empty. The user id is
a hash, not an address. Cost is absent for the same reason it is absent
everywhere: none was sent, and Tracepad does not estimate one.

## Responses

| Status | Meaning |
|---|---|
| `200` | Committed to disk. An empty body means everything was accepted; a body carries `partial_success` with the number of skipped spans. |
| `400` | The body is not a decodable OTLP export. |
| `401` | Unknown credentials. |
| `413` | The body is over `TRACEPAD_MAX_BODY_BYTES`. |
| `415` | `Content-Type` is neither `application/x-protobuf` nor `application/json`. |
| `429` | The write queue is saturated; retry after the `Retry-After` delay. Standard OTLP exporters do this on their own. |

A `200` means the spans are committed and fsynced, not merely queued.

A span that cannot be mapped — a missing or malformed trace/span id — is
skipped and counted in `partial_success` rather than failing the whole export.
Its bytes are still in the stored raw body, so nothing is lost.

## What is kept, and for how long

Every accepted body is stored as it arrived, compressed, under the project's
raw retention window (`TRACEPAD_STORE_RAW`, `raw_retention_days` — see
[retention.md](retention.md)). That archive is what
[`tracepad export --otlp`](export.md) replays, and it is readable through
`GET /api/v1/raw` ([api.md](api.md#the-raw-archive)).

`GET /api/v1/system` reports its size and its reach:

```json
"raw": {
  "enabled": true,
  "batches": 12400,
  "bytes": 3328599654,
  "oldest_received_at": "2026-08-06T04:12:19Z",
  "newest_received_at": "2026-09-05T09:44:02Z",
  "traces_before_window": 214
}
```

`traces_before_window` is the honest edge of the promise: those traces still
have rows, but no body to replay. `tracepad system` prints the same block.

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
| usage | `langfuse.observation.usage_details` (JSON object) · every `gen_ai.usage.*` count, key kept as sent · the bare token keys `input_tokens`, `output_tokens`, `total_tokens`, `cache_read_tokens`, `cache_creation_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`, `prompt_tokens`, `completion_tokens`, `reasoning_tokens`, key kept as sent |
| cost | `langfuse.observation.cost_details` (JSON object) · `gen_ai.usage.cost` |
| level | `langfuse.observation.level` · `tracepad.observation.level` · span status `ERROR` ⇒ `ERROR` · otherwise `DEFAULT` |
| status message | `langfuse.observation.status_message` · `tracepad.observation.status_message` · the span's status message |
| observation metadata | `langfuse.observation.metadata` and `langfuse.observation.metadata.*` · the same two under `tracepad.`, **plus every attribute no rule above consumed**, plus the span's events under `events`, plus the instrumentation scope's own name and version under `scope.name` and `scope.version` |

The usage row is a chain like every other one: the first source that yields a
count wins whole, and the losers stay in metadata rather than being merged into
it. An exporter that sends both `gen_ai.usage.input_tokens` and `input_tokens`
is describing one number twice, and the bare list is closed — ten spellings,
named above — because a bare word like `input_tokens` is exactly the kind of key
that collides with an attribute meaning something else. For the same reason the
ten are read on the **span** only: on the Resource they would describe a whole
export at once, which no token count does.

A value that is not a number is not a count and stays where it was — and
neither is `NaN` or an infinity, anywhere the mapping reads a number. They are
kept as the text they arrived as, in metadata.

Two consequences worth knowing:

- **Nothing is dropped.** An attribute Tracepad does not recognize shows up in
  the observation's metadata — and so does one it *did* recognize but could
  not use: an unknown level spelling, a `cost_details` that is not a JSON
  object, or the runner-up of a chain (`gen_ai.response.model` when
  `gen_ai.request.model` won, since those are two different facts). A new SDK
  convention degrades to visible metadata, never to lost data.
- **Cost is never estimated.** There is no price table. `total_cost` is
  present only when the client sent one; otherwise the UI shows "no data",
  not `$0`. How to send one, for each way in, is
  [below](#where-the-price-comes-from).
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
have a name, the [Python](sdk-python.md), [Node](sdk-js.md) and [Go](sdk-go.md)
packages use it — `gen_ai.*`, `user.id`, `session.id`,
`deployment.environment.name`, the resource's `service.version` — and where
they do not, they write these:

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

### Where the price comes from

A price on an observation is one the provider reported and your side put on
the span; the mapping reads it from `langfuse.observation.cost_details` or
`gen_ai.usage.cost` (the table above) and never computes one. Which of the
two you write, and where, depends on how your spans get here.

**The `tracepad` package** reads it from the answer. `call.end(response=…)`
takes an OpenAI-compatible response and writes `usage.cost` when it is there —
OpenRouter puts the charge in that field on every non-streamed answer, and on
a streamed one when asked (`extra_body={"usage": {"include": True}}`, read by
`call.stream`). A provider that reports its charge some other way is carried
by the explicit argument, `call.end(cost=…)`. Both are in
[sdk-python.md](sdk-python.md#generations).

**A Langfuse SDK** puts it in the generation's `cost_details` — an object with
`input`, `output` and `total`, or any subset; a missing `total` is the sum of
the parts — which arrives as `langfuse.observation.cost_details`. Nothing
about that changes when the SDK points at Tracepad: what the SDK's own docs
say about recording a cost is what applies.

**Your own OpenTelemetry spans** carry it as `gen_ai.usage.cost`, a number,
set on the span before it ends:

```python
with tracer.start_as_current_span("chat") as span:
    span.set_attribute("gen_ai.request.model", "gpt-4o-mini")
    response = client.chat.completions.create(model="gpt-4o-mini", messages=messages)
    span.set_attribute("gen_ai.usage.input_tokens", response.usage.prompt_tokens)
    span.set_attribute("gen_ai.usage.cost", response.usage.cost)  # OpenRouter reports it
```

**A third-party auto-instrumentation** — OpenLLMetry, the OpenTelemetry GenAI
instrumentations — opens and closes the span around the provider call itself,
so by the time the answer is in your hands the span has ended, and an
attribute set on an ended span is dropped by the OTel SDK. The price can land
on that span only if the instrumentation reads it from the answer, and the
GenAI conventions it writes have no attribute for one: the token counts
arrive, the price does not, unless its own attribute list says otherwise. The
way to a price for that call is to make it a `tracepad.generation` block of
your own — the package reads the answer after the call returns, which is when
the price exists.

When no price arrived, the store records none: `total_cost` is absent on the
trace, the interface shows *no data* where a cost would be, and the
statistics chart the token counts — which every provider reports — beside the
cost that some do. Tracepad does not go and ask the provider afterwards.
OpenRouter's generation endpoint could answer for a span that carries the
generation's id and nothing else, but at the price of an OpenRouter key kept
in the server, a background fetch with retries and limits of its own, and a
write that revises an observation already landed; the recipes above put the
price on the span at the source, where it is exact and needs no credential.

## Configuration

| Environment variable | Default | Meaning |
|---|---|---|
| `TRACEPAD_STORE_RAW` | `on` | Keep every accepted body (zstd) so mapping can be replayed later |
| `TRACEPAD_MAX_BODY_BYTES` | `20971520` | Request body cap; over it the answer is `413` |

Raw bodies are the reason a mapping bug is fixable after the fact rather than
being data loss. Turning them off makes ingest work exactly the same and
leaves nothing to replay.
