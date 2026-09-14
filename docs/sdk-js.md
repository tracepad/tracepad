# The Node package

```sh
npm install tracepad @opentelemetry/api
```

`tracepad` is a thin layer of ergonomics over the OpenTelemetry JS SDK. It
owns no transport, no batching, no retry and no context propagation — those
are the OTel SDK's — and it wraps no provider client. Everything it does is
reachable with `@opentelemetry/sdk-trace-node` and `fetch`, which the rest of
these docs keep showing; what it adds is the one line that points an
application at a store, and the handful of shapes a person writes the same
way every time. It is the [Python package](sdk-python.md) with promises where
Python has context managers, and it writes exactly what the Python one
writes.

Node 20 or newer. Shipped as ESM and CommonJS with type declarations (built by
`tsup`), so both `import * as tracepad from 'tracepad'` and
`const tracepad = require('tracepad')` work. `@opentelemetry/api` is a peer
dependency — the one copy your application and every instrumentation share —
and the SDK packages underneath (`@opentelemetry/sdk-trace-node`,
`@opentelemetry/exporter-trace-otlp-proto`, `@opentelemetry/resources`) come
with the package.

> Node only. There is no browser build: a browser has no key it can keep, and
> the store has no CORS. Deno and Bun may work; nothing is tested there.

## init

```ts
import * as tracepad from 'tracepad';

tracepad.init();
```

| Option | Environment | Meaning |
|---|---|---|
| `host` | `TRACEPAD_HOST` | Where the store is, e.g. `http://localhost:4318` |
| `key` | `TRACEPAD_API_KEY` | A secret key (`tp-sk-…`), sent as `Bearer` |
| `environment` | `TRACEPAD_ENVIRONMENT` | The deployment this process is |
| `release` | `TRACEPAD_RELEASE` | The version of its own logic |
| `export` | — | `false` attaches everything except the exporter |
| `logger` | — | Where the warnings go; `console` by default |

The options win over the environment, and with neither a host nor a key the
call throws `TracepadConfigError` — misconfiguration discovered as a `401` in
a log file an hour later is the bug report that rule prevents.

Standard OpenTelemetry variables (`OTEL_SERVICE_NAME`,
`OTEL_RESOURCE_ATTRIBUTES`, the batch processor's own limits) are honoured by
the OTel SDK as they are; the package neither reads them nor sets them.

**`init` adapts to the provider it finds.** If the application has already
registered a global `TracerProvider` — Express or Next instrumentation, another
SDK — the call adds a batching OTLP/HTTP exporter to *that* provider when the
provider takes one after the fact (`addSpanProcessor`, the 1.x line of the
SDK). Our spans and theirs then share one pipeline, one flush and one trace.
Only when there is no provider does `init` build a `NodeTracerProvider`, with
`service.name` from `OTEL_SERVICE_NAME` or the process title and the
environment and release on its resource, and register it with the
`AsyncLocalStorage` context manager.

The 2.x line of the SDK takes its processors in the constructor and exposes
no hook afterwards, so a `NodeTracerProvider` of yours is left alone with a
warning. Hand it the processor instead, and `init` after that adopts the
processor's configuration — a bare `init()` needs no arguments and no
environment, and one that names a different host or key is warned about:

```ts
import { NodeTracerProvider } from '@opentelemetry/sdk-trace-node';
import * as tracepad from 'tracepad';

new NodeTracerProvider({
  spanProcessors: [yourProcessor, tracepad.spanProcessor()],
}).register();
tracepad.init();
```

Two consequences worth knowing:

- Under a provider somebody else built, `environment` and `release` are
  refused with a warning: a resource is fixed when its provider is, and
  `service.version` is [read from the resource only](ingest.md#where-an-attribute-came-from).
  Set `OTEL_RESOURCE_ATTRIBUTES=deployment.environment.name=prod,service.version=1.4.0`
  instead.
- If the application already exports to Tracepad through another SDK, pass
  `export: false`. Otherwise every span arrives twice; it is upserted once and
  nothing is lost, but the bytes are wasted. No detection is attempted —
  reading another exporter's endpoint out of a provider is private API.

A second `init` is a no-op with a warning.

## Steps

```ts
const answer = tracepad.observe(async (question: string) => {
  // ...
  return 'done';
});
```

`observe` wraps a function and returns one of the same shape. Each call is a
span: the arguments become the observation's `input`, as the positional array
they are, and the return value the `output`; `{ captureInput: false }` and
`captureOutput: false` opt out. Positional rather than by name because
parameter names do not survive a bundler, and an object built from them would
be fiction. Payloads are serialized as JSON under a replacer that never
throws — a `bigint` becomes a number, an `Error` its name and message, and a
value the encoder still refuses is its `String()` — so an argument that is not
JSON is a string in the trace rather than an exception in your function. A
plain string is sent as it is, not as a JSON string of one.

There is no client-side size cap. `OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT` is
the OTel SDK's knob, and the server's own `TRACEPAD_MAX_BODY_BYTES` cut marks
the payload as [every payload is marked](api.md#the-response-budget).

| Option | Notes |
|---|---|
| `name` | The span's name; the function's own by default, then `"anonymous"` |
| `type` | One of the [ten kinds](ingest.md#the-kind-of-each-step); `"span"` by default |
| `type: "generation"` | The return value is read as a model response (below) |

A function returning a promise ends the span when it settles; a generator or
an async generator — or any iterator the function returns, which is what a
bound generator and a compiler's downleveled async generator are — ends it
when it is done, and its `output` is the list of what it yielded; a `break`
out of a `for` keeps what came before it and closes the generator. A
thrown error or a rejection ends the span at level `ERROR` with the exception
recorded as an OTel event, and propagates unchanged.

There is no decorator syntax. On a method, it is one assignment:

```ts
class Bot {
  answer = tracepad.observe(async function (this: Bot, question: string) {
    return this.reply(question);
  }, { name: 'Bot.answer' });

  async reply(question: string): Promise<string> {
    return `re: ${question}`;
  }
}
```

The same thing as a callback, plus the zero-duration kind:

```ts
await tracepad.span('retrieve', { input: { query } }, async (step) => {
  const documents = await search(query);
  step.update({ output: documents, metadata: { hits: documents.length } });
});

tracepad.event('cache.miss', () => {});
```

The span ends when the callback returns, or when the promise it returned
settles. `span`, `event` and `generation` hand out an `Observation` carrying
`traceId`, `spanId`, `update({...})` and the OTel span itself as `.span`.

## Generations

```ts
const reply = await tracepad.generation(
  'chat',
  { model: 'gpt-4o-mini', modelParameters: { temperature: 0.2 }, input: messages },
  async (call) => {
    const response = await client.chat.completions.create({ model: 'gpt-4o-mini', messages });
    call.end(response);
    return response.choices[0].message.content;
  },
);
```

`end(response)` reads an OpenAI-compatible answer — any object with the
properties, which the OpenAI client's responses are:

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

Every other provider is carried by the explicit fields, which win over
anything read:

```ts
call.end(undefined, {
  model: 'claude-sonnet-5',
  usage: { input_tokens: 128, output_tokens: 41, cache_read_input_tokens: 96 },
  cost: 0.0011,
  output: text,
});
```

`usage` keys are written verbatim under `gen_ai.usage.*` and the store keeps
them under the names you sent.

`call.firstToken()` stamps the completion start the moment the first chunk
arrives — that is where the TTFT column comes from. Only the first call counts.
Returning from the callback without `end` ends the span with what it has.

### Streams

A streamed answer goes through `call.stream(…)`, an async iterable that yields
every chunk unchanged and does the bookkeeping on the way:

```ts
await tracepad.generation('chat', { model: 'gpt-4o-mini', input: messages }, async (call) => {
  const stream = await client.chat.completions.create({
    model: 'gpt-4o-mini', messages, stream: true,
    stream_options: { include_usage: true },
  });
  for await (const chunk of call.stream(stream)) {
    process.stdout.write(chunk.choices[0]?.delta.content ?? ''); // the last chunk has no choices
  }
});
```

The first chunk with content stamps the first token, so `firstToken()` is not
needed; the `choices[0].delta.content` pieces are joined into the `output`; the
`model` is taken from the chunks that name it and the `usage` — the cost
included — from the chunk that carries it, which is the last one. When the
stream is exhausted the generation ends with all of that, through the same
table as `end(response)`. A stream you leave early, with `break` or an
exception, is ended by the callback around it, with what had been read by
then and the exception recorded; an `end(…)` you call yourself before the
stream is over wins, and nothing ends twice. A sync iterable reads the same
way.

**Whether the stream carries usage is the provider's choice**, and the package
cannot make it, because the request is yours. Ask for it: an OpenAI-compatible
client takes `stream_options: { include_usage: true }`, which adds a final
chunk with `usage` and no `choices`; OpenRouter takes
`usage: { include: true }` in the request body, and the cost rides in that
same final chunk as `usage.cost`. Without either, the stream records the model
and the output and no usage — no token counts, no cost — the same as a whole
response with no `usage` in it.

`observe(fn, { type: 'generation' })` is the same reader over a function's
return value, for a helper that already returns the provider's response. A
generator is the exception: what it returns is the list of its chunks rather
than an answer, so that list is recorded as the `output` and nothing is read
from it. For a stream, `call.stream` is the helper, not the wrapper.

## The trace around a step

A request handler rarely holds the root span — the framework does — and the one
thing it knows is who the user is:

```ts
tracepad.updateTrace({ name: 'support-chat', userId: 'u-42', sessionId: 's-7',
                       tags: ['support'], metadata: { channel: 'web' } });
tracepad.update({ level: 'WARNING', statusMessage: 'retried once' });
```

Both act on the *current* span, whoever started it, and the store resolves the
trace-level ones for the trace. Outside a span both warn and do nothing.

## Scores

```ts
tracepad.score('helpful', 0.9, { comment: 'cited the source' });
tracepad.score('verdict', { stringValue: 'pass', dataType: 'categorical' });
tracepad.score('grounded', 1, { dataType: 'boolean', observation: true });
```

With no target given, the target is the trace of the active span — and its
observation too when `observation: true`. Outside a span, with no `traceId`,
the call throws: a score that silently went nowhere is the failure this API
is worst at surfacing.

`score` does not call the server and returns nothing to await. It enqueues,
and a timer posts [`POST /api/v1/scores`](scores.md) in batches of up to 100
every two seconds; the timer is `unref`'d, so it never holds a finished
program open. A rejected batch is retried once, then logged with the server's
own message — which names the offending item — and dropped: a scoring failure
must not fail the request that produced the trace. Pass `id` for the
[idempotency](scores.md#idempotency-and-corrections) the API offers.

`await tracepad.flush({ timeout: 10_000 })` drains the queue and then the span
processors. `init` registers a `beforeExit` listener that calls it once, so a
script that returns without calling it still delivers — but `beforeExit`
does not fire on `process.exit()`, so a script that exits that way calls
`flush` first.

## Prompts

```ts
const support = await tracepad.prompt('support-answer', { label: 'production' });
const compiled = support.compile({ product: 'Tracepad' });

await tracepad.generation('chat', { prompt: support, model: String(support.config.model) }, async () => {
  // ...
});
```

`prompt` is async and nothing else: a network read that was not awaited is a
bug at the call site. `Prompt` carries `name`, `version`, `type`, `text` or
`messages`, `labels` and `config`. `compile(variables)` substitutes
`{placeholder}`s — in the text, or in every message's content — and throws
on a placeholder with no variable. Nothing else: a template language is a
product, and what the store stores is plain text.

Passing the prompt to `generation` records which prompt ran, so the trace can
be [filtered by it](api.md#listing-traces).

The answer is cached per `(name, label | version)` for as long as the server
said — `Cache-Control: max-age=60` on
[every prompt](prompts.md#fetching-a-prompt) — so a label move reaches the
application within a minute of being made. When the store is away, the last
answer is served stale with a warning, because a restart of your observability
must not take your chat down. With nothing cached the call rejects: a fallback
prompt baked into the code is a prompt the trace cannot name.

## What throws and what does not

| Path | On failure |
|---|---|
| `init` after configuration, `observe`, the callbacks, `update`, `end`, the exporter, the score queue, `flush` | Warned through the logger; never thrown into your code — a `flush` that ran out of time says so and resolves |
| `prompt` | Rejects with `TracepadError`, or `TracepadHTTPError` with `status` and `body` for a non-2xx |
| `init` with no host or key | `TracepadConfigError` |
| `score` with no target at all | `Error` — a programming error, visible at the call site |

Instrumentation that can break the function it observes is worse than none.
Everything the package warns about goes through `console.warn` with a
`tracepad:` prefix, or through whatever `init({ logger })` was given — anything
with a `warn(message)` method:

```ts
tracepad.init({ logger: { warn: (message) => log.warn(message) } });
```

## What it writes

The OTel GenAI semantic conventions where a name exists, and `tracepad.*` where
none does — a trace name, tags, free metadata, an observation kind, a prompt
reference. The whole table is in
[ingest.md](ingest.md#what-tracepad-reads-from-your-spans); the `tracepad.*`
half is [its own section](ingest.md#the-tracepad-dialect). It is the table
the Python package writes, key for key: a golden fixture written by this
package's own exporter is read back by the server's suite, so the two cannot
drift apart without a test saying so.

## What it does not do

- **No provider-client wrapper and no auto-instrumentation.** A wrapper per
  provider is a release per provider release. The OpenTelemetry GenAI
  instrumentations are the answer, and they work because the transport is
  shared: point them at the same endpoint and their spans join yours.
- **No browser build**, and no `using` — `Observation`s are handed to
  callbacks, which is the shape that guarantees the span ends.
- **No price table**, no prompt templating beyond `{placeholders}`.
- **No client of its own.** The REST calls are the global `fetch`; the one
  thing written from inside a request handler — a score — is queued instead.
