# The Python package

```sh
pip install tracepad
```

`tracepad` is a thin layer of ergonomics over the OpenTelemetry SDK. It owns
no transport, no batching, no retry and no context propagation — those are the
OTel SDK's — and it wraps no provider client. Everything it does is reachable
with `opentelemetry-sdk` and `curl`, which the rest of these docs keep showing;
what it adds is the one line that points an application at a store, and the
handful of shapes a person writes the same way every time.

Python 3.10 or newer. Two dependencies: `opentelemetry-sdk` and
`opentelemetry-exporter-otlp-proto-http`.

> The package installs no `tracepad` command. `tracepad` on your `PATH` is the
> server binary; the CLI is [cli.md](cli.md).

## init

```python
import tracepad

tracepad.init()
```

| Argument | Environment | Meaning |
|---|---|---|
| `host` | `TRACEPAD_HOST` | Where the store is, e.g. `http://localhost:4318` |
| `key` | `TRACEPAD_API_KEY` | A secret key (`tp-sk-…`), sent as `Bearer` |
| `environment` | `TRACEPAD_ENVIRONMENT` | The deployment this process is |
| `release` | `TRACEPAD_RELEASE` | The version of its own logic |
| `export` | — | `False` attaches everything except the exporter |

The arguments win over the environment, and with neither a host nor a key the
call raises `TracepadConfigError` — misconfiguration discovered as a `401` in a
log file an hour later is the bug report that rule prevents.

Standard OpenTelemetry variables (`OTEL_SERVICE_NAME`,
`OTEL_RESOURCE_ATTRIBUTES`, the batch processor's own limits) are honoured by
the OTel SDK as they are; the package neither reads them nor sets them.

**`init` adapts to the provider it finds.** If the application has already set
a global `TracerProvider` — FastAPI instrumentation, `opentelemetry-instrument`,
another SDK — the call adds a batching OTLP/HTTP exporter to *that* provider.
Our spans and theirs then share one pipeline, one flush and one trace. Only
when there is no provider does `init` build one, with `service.name` from
`OTEL_SERVICE_NAME` or the process name and the environment and release on its
resource.

Two consequences worth knowing:

- Under a provider somebody else built, `environment` and `release` are
  refused with a warning: a resource is fixed when its provider is, and
  `service.version` is [read from the resource only](ingest.md#where-an-attribute-came-from).
  Set `OTEL_RESOURCE_ATTRIBUTES=deployment.environment.name=prod,service.version=1.4.0`
  instead.
- If the application already exports to Tracepad through another SDK, pass
  `export=False`. Otherwise every span arrives twice; it is upserted once and
  nothing is lost, but the bytes are wasted. No detection is attempted —
  reading another exporter's endpoint out of a provider is private API.

A second `init` is a no-op with a warning.

## Steps

```python
@tracepad.observe
def answer(question: str) -> str:
    ...
```

The decorator makes the call a span. Its arguments become the observation's
`input`, by parameter name and with `self`/`cls` dropped, and its return value
the `output`; `@tracepad.observe(capture_input=False)` and `capture_output=False`
opt out. Payloads are serialized as JSON with `default=repr`, so an argument
the encoder cannot express is a string in the trace rather than an exception in
your function. A plain `str` is sent as it is, not as a JSON string of one.

There is no client-side size cap. `OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT` is
the OTel SDK's knob, and the server's own `TRACEPAD_MAX_BODY_BYTES` cut marks
the payload as [every payload is marked](api.md#the-response-budget).

| Form | Notes |
|---|---|
| `@observe` / `@observe(...)` | With or without parentheses |
| `name=` | The span's name; the function's own by default |
| `type=` | One of the [ten kinds](ingest.md#the-kind-of-each-step); `"span"` by default |
| `type="generation"` | The return value is read as a model response (below) |

`async` functions are wrapped, and so are generators of both kinds: the span
ends when the generator is exhausted or closed, and its `output` is the list of
what it yielded — a `break` out of a `for` keeps what came before it.

An exception ends the span at level `ERROR` with the exception recorded as an
OTel event, and propagates unchanged.

The same thing without a decorator, plus the zero-duration kind:

```python
with tracepad.span("retrieve", input={"query": q}) as step:
    step.update(output=documents, metadata={"hits": len(documents)})

with tracepad.event("cache.miss"):
    pass
```

`span`, `event` and `generation` hand out an `Observation` carrying
`trace_id`, `span_id`, `update(...)` and the OTel span itself as `.span`.

## Generations

```python
with tracepad.generation(
    "chat",
    model="gpt-4o-mini",
    model_parameters={"temperature": 0.2},
    input=messages,
) as call:
    response = client.chat.completions.create(model="gpt-4o-mini", messages=messages)
    call.end(response=response)
```

`end(response=…)` reads an OpenAI-compatible answer — a `dict` or any object
with the attributes, which the OpenAI client's models are:

| Read from | Written as |
|---|---|
| `model` | `gen_ai.response.model` |
| `usage.prompt_tokens` | `gen_ai.usage.input_tokens` |
| `usage.completion_tokens` | `gen_ai.usage.output_tokens` |
| `usage.prompt_tokens_details.cached_tokens` | `gen_ai.usage.cache_read_input_tokens` |
| `usage.completion_tokens_details.reasoning_tokens` | `gen_ai.usage.reasoning_tokens` |
| `usage.cost` | `gen_ai.usage.cost` |
| `choices[0].message.content` | the `output` |

**The cost is the one you were charged.** There is no price table here and none
in the store: OpenRouter puts the charge in `usage.cost`, and a provider that
does not report one leaves the field unset — the UI then shows *no data*, never
`$0`. A response with no `usage` at all (a stream's last chunk without
`stream_options`) still records the model and the output.

Every other provider is carried by the explicit arguments, which win over
anything read:

```python
call.end(
    model="claude-sonnet-5",
    usage={"input_tokens": 128, "output_tokens": 41, "cache_read_input_tokens": 96},
    cost=0.0011,
    output=text,
)
```

`usage` keys are written verbatim under `gen_ai.usage.*` and the store keeps
them under the names you sent.

`call.first_token()` stamps the completion start the moment the first chunk
arrives — that is where the TTFT column comes from. Only the first call counts.
Leaving the block without `end` ends the span with what it has.

### Streams

A streamed answer goes through `call.stream(…)`, which yields every chunk
unchanged and does the bookkeeping on the way:

```python
with tracepad.generation("chat", model="gpt-4o-mini", input=messages) as call:
    stream = client.chat.completions.create(
        model="gpt-4o-mini", messages=messages, stream=True,
        stream_options={"include_usage": True},
    )
    for chunk in call.stream(stream):
        print(chunk.choices[0].delta.content or "", end="")
```

The first chunk with content stamps the first token, so `first_token()` is not
needed; the `choices[0].delta.content` pieces are joined into the `output`; the
`model` is taken from the chunks that name it and the `usage` — the cost
included — from the chunk that carries it, which is the last one. When the
stream is exhausted the generation ends with all of that, through the same
table as `end(response=…)`. A stream you leave early, with `break` or an
exception, is ended by the block around it, with what had been read by then
and the exception recorded; an `end(…)` you call yourself before the stream is
over wins, and nothing ends twice. An `async` client reads the same way:
`async for chunk in call.astream(stream)`.

**Whether the stream carries usage is the provider's choice**, and the package
cannot make it, because the request is yours. Ask for it: an OpenAI-compatible
client takes `stream_options={"include_usage": True}`, which adds a final chunk
with `usage` and no `choices`; OpenRouter takes
`extra_body={"usage": {"include": True}}`, and the cost rides in that same
final chunk as `usage.cost`. Without either, the stream records the model and
the output and no usage — no token counts, no cost — the same as a whole
response with no `usage` in it.

`@tracepad.observe(type="generation")` is the same reader over a function's
return value, for a helper that already returns the provider's response. A
generator is the exception: what it returns is the list of its chunks rather
than an answer, so that list is recorded as the `output` and nothing is read
from it. For a stream, `call.stream` is the helper, not the decorator.

## The trace around a step

A request handler rarely holds the root span — the framework does — and the one
thing it knows is who the user is:

```python
tracepad.update_trace(name="support-chat", user_id="u-42", session_id="s-7",
                      tags=["support"], metadata={"channel": "web"})
tracepad.update(level="WARNING", status_message="retried once")
```

Both act on the *current* span, whoever started it, and the store resolves the
trace-level ones for the trace. Outside a span both log a warning and do
nothing.

## Scores

```python
tracepad.score("helpful", 0.9, comment="cited the source")
tracepad.score("verdict", string_value="pass", data_type="categorical")
tracepad.score("grounded", 1, data_type="boolean", observation=True)
```

With no target given, the target is the trace of the active span — and its
observation too when `observation=True`. Outside a span, with no `trace_id`,
the call raises `ValueError`: a score that silently went nowhere is the failure
this API is worst at surfacing.

`score` does not call the server. It enqueues, and a daemon thread posts
[`POST /api/v1/scores`](scores.md) in batches of up to 100 every two seconds.
A rejected batch is retried once, then logged with the server's own message —
which names the offending item — and dropped: a scoring failure must not fail
the request that produced the trace. Pass `id=` for the
[idempotency](scores.md#idempotency-and-corrections) the API offers.

`tracepad.flush(timeout=10.0)` drains the queue and then the span processors,
and runs at interpreter exit on its own.

## Prompts

```python
support = tracepad.prompt("support-answer", label="production")
messages = support.compile(product="Tracepad")

with tracepad.generation("chat", prompt=support, model=support.config["model"]):
    ...
```

`Prompt` carries `name`, `version`, `type`, `text` or `messages`, `labels` and
`config`. `compile(**variables)` is `str.format`-style substitution of
`{placeholder}`s — in the text, or in every message's content. Nothing else: a
template language is a product, and what the store stores is plain text.

Passing the prompt to `generation` records which prompt ran, so the trace can
be [filtered by it](api.md#listing-traces).

The answer is cached per `(name, label | version)` for as long as the server
said — `Cache-Control: max-age=60` on
[every prompt](prompts.md#fetching-a-prompt) — so a label move reaches the
application within a minute of being made. When the store is away, the last
answer is served stale with a warning, because a restart of your observability
must not take your chat down. With nothing cached the call raises: a fallback
prompt baked into the code is a prompt the trace cannot name.

## Evals

The package is also the harness of [datasets.md](datasets.md): a dataset
object, a run that opens and closes itself, and a block inside which every
span carries the run and the case it answered. It runs nothing — the cases are
your program.

```python
golden = tracepad.dataset("support-golden")
golden.put_items(cases)          # same cases → same version, nothing written

with golden.run("prompt v7", metadata={"prompt": "support-answer@7"}) as run:
    for case in golden.items(version=run.dataset_version):
        with run.item(case) as attempt:
            answer = app.answer(case.input["question"])
            attempt.score("accuracy", judge(answer, case.expected_output))

print(run.get()["summary"])
```

| Name | What it is |
|---|---|
| `dataset(name)` | A `Dataset`. No request is made here — it is a name. |
| `Dataset.create(description=…, metadata=…)` | Create it, or replace those two. |
| `Dataset.put_items(items)` | One batch, one version tick → `(version, changed)`. |
| `Dataset.items(version=…)` | A generator of `Item`s over every page, whole. |
| `Dataset.run(name, *, metadata=…, id=…, dataset_version=…)` | Opens a `Run`. |
| `Dataset.runs()`, `Run.get()`, `Run.items(unknown=…, limit=…)`, `compare(a, b)` | The server's JSON as `dict`s — no number is computed here. A run's items inline their payloads and are budget-checked, so that listing pages at the server's own size unless you name one. |
| `Dataset.delete(confirm=name)` | The name must be echoed, as the API asks. |
| `score_configs([...])`, `ScoreConfig` | `PUT` each, in order, synchronously. |
| `item_id(key)` | `sha256(key)[:32]`, for a natural key of your own. |

**The run pins a version, and `run.dataset_version` is it.** Fetching by that
number rather than by "the current one" is what makes the version the harness
*fetched* and the version it *ran* one number.

**`with … as run` closes it**: `finished` on a clean exit, `failed` with the
exception's `repr` on an error, which is then re-raised. A run finished by
hand is not finished twice. `finish(timeout=30.0)` flushes the scores and then
the spans *before* it posts, so `run.get()` on the next line is over
everything the run produced; a late span still links, so a flush that timed
out is a number read early rather than a trace lost.

**`run.item(case)` opens no span of its own.** It sets a `contextvars` value
that a span processor reads at every span's start — so the root span may be
the framework's, another SDK's or a decorator's, and it is still stamped. That
also says exactly how far the block reaches:

| Where the work runs | Stamped |
|---|---|
| the same function, and anything it calls | yes |
| an `await`, and a task spawned inside the block | yes — `asyncio` inherits the context |
| a thread started with `contextvars.copy_context().run(fn)` | yes |
| a thread started bare (`Thread(target=fn)`) | **no** — it has no context to inherit |
| another process, over HTTP | **no** — see below |
| the loop itself already inside a span of yours | stamped, but see below |

**Do not trace the harness.** If the loop runs inside a span of your own — a
`@observe`d driver, an instrumented test runner — then the case's spans are
children of it, and one trace covers the whole run. Everything is still
stamped and `attempt.score(...)` still has a trace to score (the block's first
span starts the case, not the trace's root), but the run then has one trace
for every case, and a trace links to one item: its coverage collapses to the
last case stamped. A case wants a trace of its own.

The two attributes do not travel with the trace context, by design. For a
service the block cannot reach, `attempt.attributes()` is the `dict` to
forward by your own means:

```python
requests.post(url, json=payload, headers={"x-eval": json.dumps(attempt.attributes())})
```

`attempt.traces` is every root span that started inside the block, in order —
a case run three times is three traces of one item, and the run's summary
counts them all — and `attempt.trace_id` is the last of them, which is what
`attempt.score(...)` scores. Scoring before anything has run raises
`ValueError`; after the block, the `Attempt` is a record and stamps nothing.

`init(export=False)` still registers the processor: an application exporting
through another SDK wants its spans stamped all the same.

## What raises and what does not

| Path | On failure |
|---|---|
| `init` after configuration, the decorators, `update`, `end`, the exporter, the score queue | Logged through the `tracepad` logger; never raised into your code |
| `prompt`, `flush`, and every call of the harness above | `TracepadError`, or `TracepadHTTPError(status, body)` for a non-2xx |
| `init` with no host or key | `TracepadConfigError` |
| `score` with no target at all | `ValueError` — a programming error, visible at the call site |

Instrumentation that can break the function it observes is worse than none.
Everything the package logs goes to the `tracepad` logger, which has no handler
of its own: your logging configuration owns it.

```python
import logging
logging.getLogger("tracepad").setLevel(logging.DEBUG)
```

## What it writes

The OTel GenAI semantic conventions where a name exists, and `tracepad.*` where
none does — a trace name, tags, free metadata, an observation kind, a prompt
reference. The whole table is in
[ingest.md](ingest.md#what-tracepad-reads-from-your-spans); the `tracepad.*`
half is [its own section](ingest.md#the-tracepad-dialect). A span this package
produced means the same thing to any OTel backend, and `langfuse.*` is never
written.

## What it does not do

- **No provider-client wrapper and no auto-instrumentation.** A wrapper per
  provider is a release per provider release. OpenLLMetry and the OpenTelemetry
  GenAI instrumentations are the answer, and they work because the transport is
  shared: point them at the same endpoint and their spans join yours.
- **No async client.** The REST calls are `urllib`, synchronously; their callers
  are scripts and start-up code. The one thing written from inside a request
  handler — a score — is queued instead.
- **No price table**, no prompt templating beyond `{placeholders}`, and no
  JavaScript twin yet.
- **The harness runs nothing.** No judge, no retries, no concurrency helpers,
  no `pytest` plugin and no `tracepad eval …` command: the loop is your
  program, and this is the part of it that talks to the store.
