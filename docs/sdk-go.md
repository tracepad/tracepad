# The Go package

```sh
go get github.com/tracepad/tracepad/sdk/go
```

`tracepad` is a thin layer of ergonomics over the OpenTelemetry Go SDK. It
owns no transport, no batching, no retry and no context propagation — those
are the OTel SDK's — and it wraps no provider client. Everything it does is
reachable with `go.opentelemetry.io/otel` and `curl`, which the rest of these
docs keep showing; what it adds is the one call that points an application at
a store, and the handful of shapes a person writes the same way every time, in
the shape Go gives them: a `context.Context` in, a `context.Context` out.

Go 1.25 or newer — the floor the OTel SDK sets. Three dependencies:
`go.opentelemetry.io/otel`, `go.opentelemetry.io/otel/sdk` and the OTLP/HTTP
trace exporter. It is a nested module, so `go get` brings none of the server's
dependencies with it.

> The module installs no command. `tracepad` on your `PATH` is the server
> binary; the CLI is [cli.md](cli.md).

## Init

```go
import tracepad "github.com/tracepad/tracepad/sdk/go"

shutdown, err := tracepad.Init(ctx)
if err != nil {
	log.Fatal(err)
}
defer shutdown(ctx)
```

| Option | Environment | Meaning |
|---|---|---|
| `WithHost` | `TRACEPAD_HOST` | Where the store is, e.g. `http://localhost:4318` |
| `WithKey` | `TRACEPAD_API_KEY` | A secret key (`tp-sk-…`), sent as `Bearer` |
| `WithEnvironment` | `TRACEPAD_ENVIRONMENT` | The deployment this process is |
| `WithRelease` | `TRACEPAD_RELEASE` | The version of this deployment — per trace, [`WithTraceVersion`](#the-trace-around-a-step) |
| `WithExport(false)` | — | Attaches everything except the exporter |
| `WithExportTimeout(time.Duration)` | `TRACEPAD_EXPORT_TIMEOUT` (seconds) | How long one export may take, its retries included; five seconds by default |
| `WithLogger` | — | Where the tracing path says what it could not do; `slog.Default()` otherwise |
| `WithTracerProvider` | — | Attach to this provider instead of the global one — for tests |

The options win over the environment, and with neither a host nor a key the
call returns `ErrConfig` — misconfiguration discovered as a `401` in a log
file an hour later is the bug report that rule prevents.

Standard OpenTelemetry variables (`OTEL_SERVICE_NAME`,
`OTEL_RESOURCE_ATTRIBUTES`, the batch processor's own limits) are honoured by
the OTel SDK as they are; the package neither reads them nor sets them.

**`WithExportTimeout`** bounds one export, its retries included.
OpenTelemetry's default is ten seconds for one attempt, retried for up to a
minute — long for a request that flushes before it answers — so the package's
is five, set both as the exporter's timeout and as the batch processor's
export timeout, which is what bounds the retries. With neither the option nor
`TRACEPAD_EXPORT_TIMEOUT`, `OTEL_EXPORTER_OTLP_TRACES_TIMEOUT` keeps working
underneath when it is set, with the OTel SDK's meaning: the bound of one
attempt; an `OTEL_BSP_EXPORT_TIMEOUT` set by the operator keeps bounding the
batch processor's export. A `TRACEPAD_EXPORT_TIMEOUT` that is not a number of seconds, or a
negative `WithExportTimeout`, is ignored with a warning, and so is the option
under `WithExport(false)`, which adds no exporter to bound.

**`Init` adapts to the provider it finds.** If the application has already set
a global `TracerProvider` — `otelhttp`, `otelgrpc`, another SDK — the call
registers a batching OTLP/HTTP exporter on *that* provider. Our spans and
theirs then share one pipeline, one flush and one trace. Only when the global
provider is still the API's no-op default does `Init` build one and set it,
with `service.name` from `OTEL_SERVICE_NAME` or the executable's name, the
environment and the release on its resource, and a `TraceContext` propagator.

Two consequences worth knowing:

- Under a provider somebody else built, `WithEnvironment` and `WithRelease`
  are refused with a warning: a resource is fixed when its provider is, and
  `service.version` is [read from the resource only](ingest.md#where-an-attribute-came-from).
  Set `OTEL_RESOURCE_ATTRIBUTES=deployment.environment.name=prod,service.version=1.4.0`
  instead.
- If the application already exports to Tracepad through another SDK, pass
  `WithExport(false)`. Otherwise every span arrives twice; it is upserted once
  and nothing is lost, but the bytes are wasted.

A second `Init` is a no-op with a warning. **The returned `shutdown`** flushes
the scores and the spans and, when `Init` built the provider, shuts it down;
a provider it adopted is the application's to close. Call it where the
application closes the rest — Go has no exit hook, and every server already
has that place.

### Which key

The package sends one key for everything it does, so the key's
[scopes](api.md#scopes) decide which calls go through:

| Calls | The key needs |
|---|---|
| The exporter, [`Score`](#scores), [`Prompt`](#prompts) | `ingest` |
| [`DeleteTrace`, `DeleteTraces`](#deleting-traces) | `write` |
| The [eval harness](#evals): datasets, runs and their scores | `ingest`, `read`, `write` |

A production application needs nothing but `ingest`, so the key to set as its
`TRACEPAD_API_KEY` is one minted with that scope alone
([admin.md](admin.md#keys)); the harness runs under a key of its own that holds
all three. A call the key does not cover is the server's `403`, message
included — `this key's scopes are ingest; DELETE /api/v1/traces needs write` —
an `*HTTPError` from the calls that return errors, and a logged, dropped batch
from the exporter and the score queue, as a `401` is.

## Steps

```go
func answer(ctx context.Context, question string) (string, error) {
	ctx, step := tracepad.Span(ctx, "answer", tracepad.WithInput(question))
	defer step.End()

	documents, err := retrieve(ctx, question)
	if err != nil {
		step.Fail(err)
		return "", err
	}
	step.Update(tracepad.WithOutput(documents), tracepad.WithMetadata(map[string]any{"hits": len(documents)}))
	...
}
```

`Span` opens a span under the context's current span and returns the context
carrying it — the OTel shape, so `retrieve` above starts its own spans as
children whether it uses this package, `otelhttp` or a bare tracer. The
caller ends it: `defer step.End()` is the idiom, and `step.Fail(err)` records
the error as an OTel event, sets the status to `ERROR` and ends — it exists
because a `defer` cannot see the error the function is about to return. A
second `End` does nothing, so the two lines above coexist.

| Option | Meaning |
|---|---|
| `WithInput(any)` | The step's input. A `string` is sent as it is; anything else as JSON |
| `WithMetadata(any)` | Free metadata: a map, or anything that encodes as a JSON object. Written one attribute per top-level key, so an `Update` adds its keys and replaces only its own |
| `WithType(string)` | One of the [ten kinds](ingest.md#the-kind-of-each-step); `"span"` by default, and an empty one keeps the default. Another spelling is sent all the same and warned about once. It sets the kind written, not the shape: the function names that, so a generation is opened with `Generation` (a `*Call`, with `End(Result)`) and an event with `Event` — `Span` with `WithType("generation")` is a plain step stored as a generation without a model. On `Generation` it types a model call of another kind, `WithType("embedding")` |

**What a step costs when nothing records.** A span starts with the cheap
attributes only — its kind, the model, the prompt reference — which is what a
sampler or a span processor sees. Input, output, metadata and model
parameters, everything serialised to JSON, are set after the span exists and
only if it records: with tracing off, or under a sampler that dropped the
span, none of them is serialised at all, in the opening call, in `Update` and
in `End` alike.

**Metadata merges by key.** It is written one attribute per top-level key,
`tracepad.observation.metadata.<key>` — a string, a number or a boolean as it
is, anything else as JSON — so `Update(ctx, WithMetadata(map[string]any{"flag": 1}))`
adds `flag` and replaces only it, and the keys the step opened with stay. A
key with a `nil` value — a nil pointer, map or slice as well — writes
nothing; it does not delete the key, since an attribute cannot be unset once
written. A struct's numbers keep the integers they are. Metadata that encodes
as no JSON object at all is written whole under
`tracepad.observation.metadata`. More than 32 keys in one write, or an empty key, are written whole under
`tracepad.observation.metadata` as well: a span holds 128 attributes by
default, and a full one drops the step's own. The store merges the whole
object with the per-key entries, and a per-key entry wins — so a large write
does not replace a key written on its own before it, and a second large write
replaces the first whole: give a step its large metadata once, when it opens,
and update a few keys at a time after. An unsigned integer past
`math.MaxInt64` is its exact decimal string. A string whose text is a JSON object or array
is stored as that structure, as every per-key metadata is read.

There is no function wrapper like Python's `@observe`: Go has no decorators
and no way to capture a function's arguments by name, so the input is what
you hand `WithInput`. Payloads are serialized with `encoding/json` (HTML
escaping off); a value the encoder cannot express — a channel, a cycle — is a
string in the trace naming its type and the reason, rather than an error in
your function.

There is no client-side size cap. `OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT` is
the OTel SDK's knob, and the server's own `TRACEPAD_MAX_BODY_BYTES` cut marks
the payload as [every payload is marked](api.md#the-response-budget).

The zero-duration kind:

```go
_, miss := tracepad.Event(ctx, "cache.miss", tracepad.WithMetadata(map[string]any{"key": key}))
miss.End()
```

`Span`, `Event` and `Generation` hand out a handle carrying `TraceID()`,
`SpanID()`, `Update(...)`, `End()`, `Fail(err)` and the OTel span itself as
`Span()`. With no trace behind the span — tracing off: `Init` never called and
no provider of the application's own — the two ids are `""`, not a string of zeros, as `Attempt.TraceID()` is before
anything has run.

## Generations

```go
ctx, call := tracepad.Generation(ctx, "chat",
	tracepad.WithModel("gpt-4o-mini"),
	tracepad.WithModelParameters(map[string]any{"temperature": 0.2}),
	tracepad.WithInput(messages))

response, err := client.Chat.Completions.New(ctx, params)
if err != nil {
	call.Fail(err)
	return err
}
call.End(tracepad.Result{
	Model:  response.Model,
	Usage:  tracepad.Usage{"input_tokens": response.Usage.PromptTokens, "output_tokens": response.Usage.CompletionTokens},
	Output: response.Choices[0].Message.Content,
})
```

`End` takes an explicit `Result`, and there is no reader over a provider's
response: Go has no dominant client with one response shape — `openai-go`,
`anthropic-sdk-go`, `go-openai` and a raw `net/http` caller each hand back a
different struct — and five lines at the call site cannot silently read the
wrong field.

| Field | Written as |
|---|---|
| `Model` | `gen_ai.response.model` |
| `Usage` (`map[string]int64`) | every entry as `gen_ai.usage.<key>`, verbatim — `input_tokens`, `output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`, `reasoning_tokens` are the names the store charts |
| `Cost` (`*float64`) | `gen_ai.usage.cost` |
| `Output` | the `output` |

**The cost is the one you were charged.** There is no price table here and none
in the store: OpenRouter puts the charge in the response's `usage.cost`, and a
provider that does not report one leaves `Cost` nil — the UI then shows *no
data*, never `$0`. Every field is optional and an absent one is absent from
the span; `call.End(tracepad.Result{})` ends the span with what it has.

`call.FirstToken()` stamps the completion start the moment the first chunk
arrives — that is where the TTFT column comes from. Only the first call counts.
In a stream loop it is the three lines that stay:

```go
stream := client.Chat.Completions.NewStreaming(ctx, params)
var text strings.Builder
for stream.Next() {
	chunk := stream.Current()
	if len(chunk.Choices) > 0 {
		call.FirstToken()
		text.WriteString(chunk.Choices[0].Delta.Content)
	}
}
if err := stream.Err(); err != nil {
	call.Fail(err)
	return err
}
call.End(tracepad.Result{Model: model, Output: text.String()})
```

Whether the stream carries usage is the provider's choice, and the package
cannot make it: ask for it (`stream_options.include_usage` on an
OpenAI-compatible client) and fill `Usage` from the last chunk.

## The trace around a step

A request handler rarely holds the root span — the framework does — and the one
thing it knows is who the user is:

```go
tracepad.UpdateTrace(ctx, tracepad.WithTraceName("support-chat"), tracepad.WithUserID("u-42"),
	tracepad.WithSessionID("s-7"), tracepad.WithTags("support"), tracepad.WithTraceMetadata(map[string]any{"channel": "web"}),
	tracepad.WithTraceVersion("retrieval-v2"))
tracepad.Update(ctx, tracepad.WithLevel("WARNING"), tracepad.WithStatusMessage("retried once"))
```

Both act on the context's current span, whoever started it, and the store
resolves the trace-level ones for the trace. `Update` takes `WithName`,
`WithInput`, `WithOutput`, `WithMetadata`, `WithLevel`, `WithStatusMessage` and
`WithType`. Outside every span, in a process that traces, both log a warning
and do nothing — that call is a mistake. On a span that does not record —
tracing off, or a sampler's choice — they do nothing with a debug line: that
is configuration, and a warning on every call would teach an operator to
ignore the package's warnings.

| `UpdateTrace` option | Meaning |
|---|---|
| `WithTraceName(string)` | The trace's name |
| `WithUserID(string)` | Who the trace is for |
| `WithSessionID(string)` | The session it belongs to |
| `WithTags(...string)` | Its tags |
| `WithTraceMetadata(any)` | Free metadata on the trace, a JSON object |
| `WithTraceVersion(string)` | The version of this trace's own logic |

`WithTraceVersion` is the version of *this trace's* logic — a pipeline
revision, a prompt bundle, an experiment arm — and the trace listing filters on
it (`version=`). It sits beside `WithRelease`, the deployment's version set
once at `Init`: two arms running in one release are told apart by the version.

## Scores

```go
tracepad.Score(ctx, "helpful", tracepad.WithValue(0.9), tracepad.WithComment("cited the source"))
tracepad.Score(ctx, "verdict", tracepad.WithStringValue("pass"), tracepad.WithDataType("categorical"))
tracepad.Score(ctx, "grounded", tracepad.WithValue(1), tracepad.WithDataType("boolean"), tracepad.OnObservation())
```

With no target given, the target is the trace of the context's span — and its
observation too with `OnObservation()`. Outside a span, with no
`WithTraceID`, the call returns `ErrNoTrace`: a score that silently went
nowhere is the failure this API is worst at surfacing, and so is an empty
`WithTraceID` — an id stored while tracing was off, say. Where nothing traces —
no `Init`, a no-op global provider, no recording span in the context — the same
call returns `nil` and logs a debug line instead: every span is a no-op there,
and the call site is not wrong. A `WithTraceID` is scored either way.

`Score` does not call the server. It enqueues, and a goroutine posts
[`POST /api/v1/scores`](scores.md) in batches of up to 100 every two seconds.
A rejected batch is retried once, then logged with the server's own message —
which names the offending item — and dropped: a scoring failure must not fail
the request that produced the trace. Pass `WithID` for the
[idempotency](scores.md#idempotency-and-corrections) the API offers.

`tracepad.Flush(ctx)` drains the queue and then the span processors; the
`shutdown` returned by `Init` does the same before it closes anything. Both
keep to the context's deadline: against a store that never answers, `Flush`
returns `context.DeadlineExceeded` when the context ends, and the export goes
on under its own timeout.

## Prompts

```go
support, err := tracepad.Prompt(ctx, "support-answer", tracepad.WithLabel("production"))
if err != nil {
	return err
}
messages := support.Compile(map[string]any{"product": "Tracepad"}).Messages

ctx, call := tracepad.Generation(ctx, "chat", tracepad.WithPrompt(support),
	tracepad.WithModel(support.Config["model"].(string)))
```

The value is a `PromptVersion` carrying `Name`, `Version`, `Type`, `Text` or
`Messages`, `Labels` and `Config`. `Compile(vars)` substitutes `{placeholder}`s
— in the text, or in every message's content — and hands back a `Compiled`
with `Text` or `Messages` filled, whichever the prompt is; `{{` and `}}` are
literal braces, and a placeholder `vars` does not name is left as it is.
Nothing else: a template language is a product, and what the store stores is
plain text.

Passing the prompt to `Generation` records which prompt ran, so the trace can
be [filtered by it](api.md#listing-traces).

The answer is cached per `(name, label | version)` for as long as the server
said — `Cache-Control: max-age=60` on
[every prompt](prompts.md#fetching-a-prompt) — so a label move reaches the
application within a minute of being made. When the store is away, the last
answer is served stale with a warning, because a restart of your observability
must not take your chat down. With nothing cached the call returns the error:
a fallback prompt baked into the code is a prompt the trace cannot name. A
script that only fetches a prompt need not call `Init`: the environment is
read on the first call.

## Deleting traces

```go
filter := tracepad.TraceFilter{To: time.Now(), Environment: "loadtest"}
preview, err := tracepad.DeleteTraces(ctx, filter, "")
if err != nil {
	return err
}
log.Println(preview["matched"], preview["would_delete"])
total, err := tracepad.DeleteTraces(ctx, filter, "checkout-service")
if err != nil {
	return err
}
log.Println(total["deleted"], total["rounds"])

if id := step.TraceID(); id != "" { // "" with tracing off
	_, err = tracepad.DeleteTrace(ctx, id, true)
}
```

A script's door to what [`traces rm`](cli.md#traces-rm) does: an eval harness that
exported under the wrong key, a test suite that wants its traces gone before
the next run. Both calls are a dry run until confirmed, the way every
destructive act in the store is, and both return `*HTTPError` for what the
API refuses — a `404` for an unknown id, a `400` for a wrong echo or a filter
name it does not know.

`DeleteTrace(ctx, id, confirm bool)` takes one trace with everything attached
to it. With `false` the answer is the preview — `would_delete`,
`affected_runs`, `confirm`, `note`; with `true` the package sends the id as
the echo, because it has the id in hand, and the answer says what went.

`DeleteTraces(ctx, filter, confirm string, ...DeleteOption)` takes every
trace the `TraceFilter` matches — one field per
[listing filter](api.md#filters), `Tag` a slice, `From` and `To` a
`time.Time` sent as UTC — before `To`, which is required so that the set is
closed: a zero `To` is an error before any request. With an empty `confirm`
it is one dry run and the answer is the API's preview, `matched` counted
exactly. With the project's name — a name you type, since the package does
not know it — it deletes in rounds of at most `WithRoundLimit` traces (1000
by default), each given sixty seconds, repeats while the API says there is
more, and answers one total, `deleted` with `traces`, `observations`, `scores`, `payloads` and
`annotation_items`, and `rounds` — `float64`s, as in every answer the package
decodes. A filter that matches nothing is one round of zero, not an error.

Raw OTLP bodies are **not** touched — a raw batch holds many traces of many
kinds, and a trace cannot be cut out of one — and the preview says so in its
`note`. What goes, what stays and the ingest race are in
[admin.md](admin.md#deleting-traces).

## Evals

The package is also the harness of [datasets.md](datasets.md): a dataset
value, a run that pins a version, and a context inside which every span
carries the run and the case it answered. It runs nothing — the cases are
your program. Its key holds `ingest`, `read` and `write`
([Which key](#which-key)).

```go
golden := tracepad.NewDataset("support-golden")
golden.PutItems(ctx, cases)          // same cases → same version, nothing written

run, err := golden.Run(ctx, "prompt v7", tracepad.WithRunMetadata(map[string]any{"prompt": "support-answer@7"}))
if err != nil {
	return err
}
for item, err := range golden.Items(ctx, run.DatasetVersion) {
	if err != nil {
		run.Fail(ctx, err)
		return err
	}
	itemCtx, attempt := run.Item(ctx, item)
	answer := app.Answer(itemCtx, item.Input.(map[string]any)["question"].(string))
	attempt.Score(ctx, "accuracy", tracepad.WithValue(judge(answer, item.ExpectedOutput)))
}
summary, err := run.Finish(ctx)
```

| Name | What it is |
|---|---|
| `NewDataset(name)` | A `*Dataset`. No request is made here — it is a name. |
| `Dataset.Create(ctx, description, metadata)` | Create it, or replace those two. |
| `Dataset.PutItems(ctx, items)` | One batch, one version tick → `(version, changed, err)`. |
| `Dataset.Items(ctx, version)` | An `iter.Seq2[Item, error]` over every page, whole; `tracepad.CurrentVersion` for the dataset as it is now — `0` is a version, the one before the first item. |
| `Dataset.Run(ctx, name, opts…)` | Opens a `*Run`; `WithRunMetadata`, `WithRunID`, `WithDatasetVersion`. |
| `Dataset.Runs(ctx)`, `Run.Get(ctx)`, `Run.Items(ctx, opts…)`, `Compare(ctx, a, b)` | The server's JSON as `map[string]any` — no number is computed here. A run's items inline their payloads and are budget-checked, so that listing pages at the server's own size unless `WithLimit` names one; `WithUnknown` adds the traces that link to no case. |
| `Dataset.Delete(ctx, confirm)` | The name must be echoed, as the API asks; an empty `confirm` is the API's dry run. |
| `ScoreConfigs(ctx, configs)`, `ScoreConfig` | `PUT` each, in order, synchronously. |
| `ItemID(key)` | `sha256(key)[:32]`, for a natural key of your own. |

**The run pins a version, and `run.DatasetVersion` is it.** Fetching by that
number rather than by "the current one" is what makes the version the harness
*fetched* and the version it *ran* one number.

**A run is closed by `Finish` or `Fail`**, and there is no block to close it
for you: a run left running is reported as such forever, so the `Fail` on
the error path is yours to write. Both flush the scores and then the spans
*before* they post, so `Get` on the next line is over everything the run
produced; a late span still links, so a flush that ran out of time is a
number read early rather than a trace lost, and the close still posts.

**`run.Item(ctx, item)` opens no span of its own.** It returns a context
carrying the run and the item, which a span processor reads at every span's
start — so the root span may be the framework's, another SDK's or `Span`'s,
and it is still stamped. That also says exactly how far the context reaches,
because in Go a context *is* the block:

| Where the work runs | Stamped |
|---|---|
| a call handed the context, and anything it calls with it | yes |
| a goroutine handed the context | yes |
| a goroutine started with `context.Background()` | **no** — it has no context to inherit |
| a span started with a context from before `Item` | **no** |
| another process, over HTTP | **no** — see below |
| the loop itself already inside a span of yours | stamped, but see below |

**Do not trace the harness.** If the loop runs inside a span of your own —
an instrumented test runner, a `Span` around the loop — then the case's spans
are children of it, and one trace covers the whole run. Everything is still
stamped and `attempt.Score` still has a trace to score (the context's first
span starts the case, not the trace's root), but the run then has one trace
for every case, and a trace links to one item: its coverage collapses to the
last case stamped. A case wants a trace of its own.

The two attributes do not travel with the trace context, by design. For a
service the context cannot reach, `attempt.Attributes()` is the map to
forward by your own means — a header, a field in the request.

`attempt.Traces()` is every root span that started inside the context, in
order — a case run three times is three traces of one item, and the run's
summary counts them all — and `attempt.TraceID()` is the last of them, which
is what `attempt.Score` scores. Scoring before anything has run is
`ErrNoTrace`; after the loop moves on, the `Attempt` is a record, and a span
started from a context taken before `Item` is not stamped.

`Init` with `WithExport(false)` still registers the processor: an
application exporting through another SDK wants its spans stamped all the
same. Without `Init` at all, the REST calls above work — the environment is
read on the first one — but nothing registers the processor, so nothing is
stamped and the run covers no case; the first `run.Item` says so in the log.

## Testing your instrumentation

Package `tracepadtest` is what a test suite uses to check that the code
traces: that a step is a span with the right name, a model call a generation
with its usage, a thumbs-up a score. It is `httptest`'s idiom — the helpers
take the test and register their own cleanup.

```go
import "github.com/tracepad/tracepad/sdk/go/tracepadtest"

func TestTheAnswerIsTraced(t *testing.T) {
	rec := tracepadtest.Capture(t)
	answer(context.Background(), "why is the sky blue")

	if got := rec.Attributes(t, "answer")["tracepad.observation.type"].AsString(); got != "span" {
		t.Errorf("type = %q, want span", got)
	}
	if got := rec.Scores(); len(got) != 1 || got[0]["name"] != "helpful" {
		t.Errorf("scores = %v", got)
	}
}
```

`Capture` resets everything the package keeps process-wide and initialises
the package against a provider that records into memory, with export off and
nothing read from the environment; the score queue keeps each body instead of
posting it. The global provider follows the capture's, so a package-level
`otel.Tracer("app")` records into every capture, not only the first. The
test's cleanup — on `t.Fatal` too — resets the process again, and what was
captured stays readable, from a parent test after its subtest too.

| On a `*Recorder` | What it is |
|---|---|
| `Spans()` | Every finished span, in the order it ended — the SDK's own `tracetest.SpanStubs` |
| `One(t, name)` | The single span of that name; the test fails naming the spans there were otherwise |
| `Attributes(t, name)` | That span's attributes, by key |
| `Scores()` | The bodies `Score` would have posted: `name`, `trace_id`, `value` and the rest |

`tracepadtest.Reset(t)` returns the process to one that never called `Init` —
tracing off, as [above](#steps) — for a test of the code with tracing off:
`Score` returns `nil`, and the ids are empty. A reset shuts down a provider
`Init` built, takes back the propagator it set and drops the scores still
queued; an `Init` after it builds a provider as it would in a fresh process.

Nothing is sent: the placeholder host is `tracepad.test`, which does not
resolve, so a REST call a test forgot to stub — `Prompt`, a dataset,
`DeleteTrace` — fails rather than reaching a store. A provider your
application sets at start-up is replaced for the capture and not restored
after it. The capture is process-wide, so it refuses a parallel test: called after
`t.Parallel`, the helper fails the test saying why, and a `t.Parallel` after
the helper panics in the testing package.

## What fails and what does not

| Path | On failure |
|---|---|
| `Init` after configuration, `Span`, `Generation`, `Update`, `End`, the exporter, the score queue | Logged through `slog` (`WithLogger`, or the default logger); never a panic, never an error into your code |
| `Prompt`, `Flush`, `shutdown`, `DeleteTrace`, `DeleteTraces`, and every call of the harness above | An error: `*HTTPError{Status, Body}` for a non-2xx answer, the transport's own otherwise |
| `Init` with no host or key | `ErrConfig`, wrapped with what is missing |
| `Score` with no target — or an empty `WithTraceID` — outside every span while tracing: initialised, or through a provider of the application's own | `ErrNoTrace` — a programming error, visible at the call site |
| `Score` with no target where nothing traces (no `Init`, no-op global provider, no recording span) | `nil`; a debug line — tracing is off |
| `Update`, `UpdateTrace` on a span that does not record (tracing off, or sampled out) | Nothing; a debug line |
| A name that is empty, `.` or `..` | An error, before any request — no name the store accepts is one |

Both sentinels are for `errors.Is`, and `*HTTPError` for `errors.As`.

A REST call follows a redirect of a `GET` with the key only within the store's
origin — the same scheme, host and port; anywhere else it goes without
`Authorization`, as `fetch` has it. A write is never re-sent: its redirect is an
error that names where it pointed. The store itself never redirects, so
`WithHost` or `TRACEPAD_HOST` should be the address it answers on.

Instrumentation that can break the function it observes is worse than none.

## What it writes

The OTel GenAI semantic conventions where a name exists, and `tracepad.*` where
none does — a trace name, tags, free metadata, a trace version, an observation
kind, a prompt reference. The whole table is in
[ingest.md](ingest.md#what-tracepad-reads-from-your-spans); the `tracepad.*`
half is [its own section](ingest.md#the-tracepad-dialect). It is the same
vocabulary the [Python package](sdk-python.md#what-it-writes) writes, and the
fixture `testdata/otlp/014-tracepad-sdk-go.pb` — written by the package's own
exporter — pins it.

## What it does not do

- **No provider-client wrapper and no auto-instrumentation.** A wrapper per
  provider is a release per provider release. The OpenTelemetry GenAI
  instrumentations are the answer, and they work because the transport is
  shared: point them at the same endpoint and their spans join yours.
- **No response reader and no stream wrapper.** An explicit `Result` is five
  lines at the call site, and it cannot read the wrong field.
- **No price table**, and no prompt templating beyond `{placeholders}`.
- **No client type.** One default the package keeps, configured by `Init`;
  `WithTracerProvider` is the one per-instance knob, for tests.
- **The harness runs nothing.** No judge, no retries, no worker pool and no
  `tracepad eval …` command: the loop is your program, and this is the part
  of it that talks to the store.
