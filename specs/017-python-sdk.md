# Spec 017 — The Python package: `init`, `@observe`, generations, prompts and scores

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> Everything the store does is reachable with the OpenTelemetry SDK and
> `curl`, and the docs say so on every page. What is missing is the one
> line that makes a Python application talk to Tracepad, and the ten lines
> after it that a person writes the same way every time: a span around a
> step, a generation with its model, its usage and — the thing every
> auto-wrapper gets wrong — the cost the provider actually charged, a score
> against the trace in flight, a prompt fetched by label. This spec adds the
> `tracepad` package: a thin layer over `opentelemetry-sdk` that owns none
> of the transport and all of the ergonomics.

---

## Overview

Deliverable, in one PR (which flips the status): the `tracepad` package
under `sdk/python/`, published to PyPI from the tag `sdk-py/v*`.

- **`tracepad.init(...)`** — one call: an OTLP/HTTP exporter aimed at the
  store, attached to the application's existing `TracerProvider` or to one
  the call creates when there is none (Decision 2).
- **`@observe`** and the context managers **`span`, `generation`, `event`**
  — steps as spans, arguments and return values captured as `input` and
  `output` unless told otherwise (Decision 4), attributes in the vocabulary
  of Decision 3.
- **The generation helper** — `model`, the full usage breakdown and the
  cost, read from an OpenAI-compatible response object or given by hand
  (Decision 5), with time-to-first-token for streams.
- **`tracepad.score(...)`** — a score against the trace (or observation) in
  flight, queued and delivered in batches by a background thread
  (Decisions 6, 7).
- **`tracepad.prompt(...)`** — a labelled prompt, cached for as long as the
  server says, served stale when the server is away (Decision 8), and
  named on the generation that used it.
- **The `tracepad.*` dialect** in the mapper: the handful of attributes the
  GenAI semantic conventions have no word for (Decision 3), claimed at the
  same rank as `langfuse.*`.
- `docs/sdk-python.md`, the quickstart's *Connecting an application* step
  in Python, `release-sdk-py.yml`, an `sdk` CI job.

Not here: the eval harness — datasets, runs, the run/item context and the
span processor that stamps it — which is spec 018 over this package; any
wrapper of a provider client (design §6.5: never); a JavaScript package.

---

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-04** — One package, `tracepad` on PyPI, source in `sdk/python/`, **thin over `opentelemetry-sdk`**: it owns no transport, no batching, no retry and no context propagation — those are the OTel SDK's — and it wraps no provider client. Everything it does is reachable with the OTel SDK and `curl`, and the docs keep saying so. Hard dependencies: `opentelemetry-sdk` and `opentelemetry-exporter-otlp-proto-http`, both `>= 1.44, < 2`; nothing else. Python `>= 3.10`. The package installs **no console script** — `tracepad` on a `PATH` is the Go binary. Application lines are budgeted at **1,500** across this spec and spec 018, reported by `make sdk-lines` (owner decision 2026-09-04: the whole of design §6.5 in one package, the harness in its own spec) | Design §6.5 sets the frame — the model the Langfuse v4 SDK proved, where the OTel SDK does the heavy lifting and the vendor package is ergonomics — and the frame is what keeps this from becoming the SDK treadmill: a wrapper per provider is a release per provider release. Two dependencies is what "thin" means in a `pip install` line; a third would be the first of many. The console script is withheld because a Python entry point named `tracepad` shadowing the server binary in a virtualenv is a support ticket. The line budget is the same instrument spec 015 #9 gave the interface: a number the PR must report so that growth is a decision. |
| 2 | **2026-09-04** — `tracepad.init(host=None, key=None, environment=None, release=None, export=True)` **adapts to the provider it finds**: when the application has set a global `TracerProvider`, the call adds a `BatchSpanProcessor` with an OTLP/HTTP exporter aimed at `{host}/v1/traces` to it; when the global provider is still the API's default proxy, the call builds one — resource `service.name` from `OTEL_SERVICE_NAME` or the process name, `deployment.environment.name` and `service.version` from the arguments — and sets it. A second `init` is a no-op with a warning. `export=False` attaches everything except the exporter (owner decision 2026-09-04: adapt, never replace) | The reference application already runs a provider (FastAPI instrumentation, or the Langfuse SDK pointed at Tracepad), and an `init` that replaced it would silently detach every instrumentation registered before the call — the OTel API refuses to override a global provider for exactly that reason and logs instead. Adding a processor is what the OTel SDK offers for "one more destination", and it puts our spans and theirs in one pipeline with one flush. Creating a provider only when none exists is what makes the one-liner true for a script. `export=False` exists for the application whose traces already reach the store through another exporter and wants only the ergonomics (or, in spec 018, only the stamping): without it, `init` would double every span on the wire — harmless to the store, which upserts by span id, and still a waste. |
| 3 | **2026-09-04** — The attributes the package writes are the **OTel GenAI semantic conventions where a name exists** and **`tracepad.*` where none does**. Semconv: `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.request.*` for model parameters, `gen_ai.input.messages` / `gen_ai.output.messages` (a JSON string), `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `gen_ai.usage.cache_read_input_tokens`, `gen_ai.usage.cache_creation_input_tokens`, `gen_ai.usage.reasoning_tokens`, `gen_ai.usage.cost`, `user.id`, `session.id`, `deployment.environment.name`, `service.version` (resource). Tracepad's own: `tracepad.trace.name`, `tracepad.trace.tags` (JSON array), `tracepad.trace.metadata` (JSON object), `tracepad.observation.type`, `tracepad.observation.level`, `tracepad.observation.status_message`, `tracepad.observation.metadata` (JSON object), `tracepad.observation.completion_start_time` (RFC 3339), `tracepad.prompt.name`, `tracepad.prompt.version`. The mapper gains the **`tracepad` dialect**: each `tracepad.*` key enters its field's chain at the rank of the `langfuse.*` key it mirrors, level-agnostic like the rest (spec 012 #7), trace-level keys ranked like `session_id` (spec 012 #11), and `dialectOf` labels an export carrying any of them `tracepad`. `docs/ingest.md`'s table gains the column. `langfuse.*` is never written (owner decision 2026-09-04) | Standards first, because a span our decorator produced should mean the same thing to any OTel backend and to any instrumentation reading beside it; the generation vocabulary is the one place the conventions are complete enough to use whole, usage keys included — the mapper keeps every `gen_ai.usage.*` count under the key it was sent. The gaps are real: the conventions have no trace name, no tags, no free metadata, no observation kind and no prompt reference, and inventing `gen_ai.*` names for them would be a lie about whose vocabulary they are. Writing `langfuse.*` would work on day one — the mapper reads it — and would make Tracepad's own SDK speak a competitor's dialect, a thing no reader of the trace could explain. A dialect is a table edit (`rules.go`), which is what spec 002 #10 built the table for. |
| 4 | **2026-09-04** — `@observe` **captures by default**: the call's arguments (by parameter name, `self`/`cls` dropped) become `input` and the return value `output`, serialized as JSON with `default=repr`; `@observe(capture_input=False)` / `capture_output=False` opt out per function, and `tracepad.update(input=…, output=…)` from inside the function replaces either. The decorator wraps sync and `async` functions, and sync and async generators (the span ends when the generator is exhausted or closed, `output` is the list of yielded values unless replaced). No client-side size cap: `OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT` is the knob, and the server's `TRACEPAD_MAX_BODY_BYTES` cut marks the payload as every payload is marked (owner decision 2026-09-04) | The first trace a newcomer sees must have something in it, and the store is self-hosted — the privacy argument for an empty default belongs to a SaaS. `default=repr` is the rule that never raises: a payload the encoder cannot express is still a string in the trace, and the alternative is a decorator that breaks the function it decorates. The size knob is left to the OTel SDK because it already has one and a second cap would be two answers to one question; the server's cut is the visible failure spec 004 #2 designed. Generators are wrapped because a streamed answer is the ordinary shape of an LLM call in the reference application, and a span that closed when the generator object was returned would time nothing. |
| 5 | **2026-09-04** — The generation helper reads **an OpenAI-compatible response** and takes **explicit arguments** for everything else. `generation(name, model=…, prompt=…, model_parameters=…)` opens the span; `gen.end(response=obj)` reads `model`, `usage.prompt_tokens` → `input_tokens`, `usage.completion_tokens` → `output_tokens`, `usage.prompt_tokens_details.cached_tokens` → `cache_read_input_tokens`, `usage.completion_tokens_details.reasoning_tokens` → `reasoning_tokens`, `usage.cost` → `gen_ai.usage.cost`, and `choices[0].message.content` → `output` when no `output` was given; the object may be a `dict` or anything with those attributes (the OpenAI client's pydantic models qualify). `gen.end(model=…, usage={…}, cost=…, output=…)` sets each directly and wins over the parsed value. `gen.first_token()` stamps `completion_start_time` once. The reader is one function of forty lines, and no other response shape is known (owner decision 2026-09-04) | Design §6.5 names the bug this exists to close: an auto-wrapper that shows `$0` because it computed a price instead of reading the one it was charged. OpenRouter puts the charge in `usage.cost`, and OpenAI's shape is what every proxy and most providers speak, so one reader carries the reference application and the common case; the explicit arguments carry every other provider without the package having to know it. Reading a response is not wrapping a client — the call is still the application's, made however it likes — and stopping at one shape is what keeps Decision 1 true. The cost is never estimated here either (`docs/ingest.md`): what the helper cannot read, it does not send. |
| 6 | **2026-09-04** — REST calls go over **`urllib` from the standard library, synchronously**; there is no async client. `score()` **does not call the server**: it enqueues, and a daemon thread posts `POST /api/v1/scores` in batches of up to 100 every 2 s or when the batch fills; `tracepad.flush()` drains the queue and the span processors, and `atexit` flushes. A rejected batch is retried once, then logged with the server's message — which names the offending item — and dropped; the caller is never raised into (owner decision 2026-09-04) | The callers of the REST side are scripts and start-up code (prompts at boot, the harness of spec 018), where a blocking call is the honest shape and an async twin would double the API for a caller that has none. Scores are the exception: they are written from inside request handlers, and the OTel exporter has already shown what the right shape is — a queue, a thread, a flush. A dropped score is logged and not raised because a scoring failure must not fail the request that produced the trace, the same asymmetry the exporter has. One retry, not more: a 400 is deterministic, and a queue that retries forever is a memory leak with a log line. |
| 7 | **2026-09-04** — `tracepad.score(name, value, *, string_value=None, data_type=None, comment=None, id=None, trace_id=None, observation_id=None, observation=False)` finds its target **from the current context** when none is given: the trace id of the active span, and its span id when `observation=True`. With no active span and no `trace_id`, it **raises** `ValueError`. `id` is passed through for the client-side idempotency spec 003 #3 offers (owner decision 2026-09-04) | Inside a request the trace is right there, and asking the caller to fetch the id from the span they are standing in is ceremony; outside, there is no trace to guess, and a score that silently went nowhere is the failure mode this API is worst at surfacing. Raising here is the one exception to Decision 6's "never raise": it is a programming error visible at the call site, not a delivery failure discovered later. |
| 8 | **2026-09-04** — `tracepad.prompt(name, *, label=None, version=None)` returns a `Prompt` with `name`, `version`, `text` (or `messages` for a chat prompt), `labels`, `config`; **cached in memory** per `(name, label \| version)` for the `Cache-Control: max-age` the server sent (60 s, `docs/prompts.md`), **served stale** on any transport or 5xx error with a warning, and **raised** (`TracepadError`) when there is nothing cached — never an empty prompt. `Prompt.compile(**variables)` is `str.format`-style substitution of `{name}` placeholders, applied to `text` or to every message's content. `generation(prompt=p)` writes `tracepad.prompt.name` / `tracepad.prompt.version` (owner decision 2026-09-04) | A prompt is read per request and changes per deploy; the server already said how long a label may be trusted, and a client that ignored it would either hammer the store or ship a moved label late. Stale-on-error is the availability the reference application needs — the store restarting must not take the chat down — and the raise on an empty cache is the honesty it needs more: a fallback prompt baked into the code is a prompt the trace cannot name, and `?label=` is a 400 on the server for the same reason (`docs/prompts.md`). `str.format` and nothing else because a template language is a product, and the one the store stores is plain text. |
| 9 | **2026-09-04** — **Failure semantics** are split by path. The tracing path — `init` after configuration, the decorators, `update`, `end`, the exporter, the score queue — **never raises into application code**; it logs through the `tracepad` logger. The REST path — `prompt`, `flush`, and spec 018's clients — raises `TracepadError`, with `TracepadHTTPError(status, body)` for a non-2xx answer. `init` with no host or key raises `TracepadConfigError` (owner decision 2026-09-04) | Instrumentation that can break the function it observes is worse than none, and the OTel SDK holds the same line. A REST call is the caller's own request for a value, and a value it cannot have must be an exception rather than `None` with a log line nobody reads. `init` is the one call that is both: it is configuration, and misconfiguration discovered at the first export — a 401 in a log file an hour later — is the bug report this rule prevents. |
| 10 | **2026-09-04** — Configuration: the arguments of `init`, then the environment — `TRACEPAD_HOST`, `TRACEPAD_API_KEY` (the secret key; `Bearer` auth), `TRACEPAD_ENVIRONMENT`, `TRACEPAD_RELEASE`. Standard OTel variables are honoured by the OTel SDK as they are (`OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`, the BSP limits); the package sets none of them and reads none of them | Twelve-factor is what a deploy expects, and two names beside the host are the whole of what the store needs to know a caller. Reading OTel's own variables would be a second implementation of the SDK's configuration with a second set of defaults; the SDK already does it, and the package is not in its way. |
| 11 | **2026-09-04** — `tracepad.update(...)` and `tracepad.update_trace(...)` act on the **current** span: `update(name=, input=, output=, metadata=, level=, status_message=, type=)` writes observation attributes; `update_trace(name=, user_id=, session_id=, tags=, metadata=)` writes the trace-level ones **on the current span**, which the mapper resolves for the trace (spec 012 #11's ranking; the root is the convention, not a requirement). Outside a span both log a warning and do nothing | A request handler rarely holds the root span — the framework does — and the one thing it knows is who the user is. Writing it where the handler stands, and letting the mapper carry it to the trace, is what the mapper's ranking is for; the alternative, walking up to the root, is not possible through the OTel API. Warn-and-drop follows Decision 9. |
| 12 | **2026-09-04** — Release: version in `sdk/python/pyproject.toml`, tag `sdk-py/vX.Y.Z` triggers `release-sdk-py.yml` — build with `uv`, publish with PyPI trusted publishing — as `REPOS.md` §2 and §7.2 planned. The `sdk` job in `ci.yml` runs the package's tests on Python 3.10 and 3.13 and the e2e of the Testing section against a real binary. `make smoke` gains the package as a third pinned exporter beside `opentelemetry-sdk` and `langfuse` | The monorepo's argument was that "a field in the API + the mapper + a fixture + the SDK" is one PR of one agent, and this PR is the first proof. Two Pythons because 3.10 is the floor and the newest is where the OTel SDK moves first. The smoke test is the drift detector for SDK conventions (`scripts/smoke/requirements.txt`); our own package belongs in it for the same reason the other two do. |
| 13 | **2026-09-04** — `environment` and `release` are **refused with a warning when `init` adopts a provider it did not build**, and `docs/sdk-python.md` says to put them in `OTEL_RESOURCE_ATTRIBUTES` instead. They still work in full when `init` builds the provider, which is Decision 2's other half | Both are resource attributes, a resource is fixed when its provider is built, and `service.version` is read from the Resource only (spec 012 #7, `keyLevel`) — so a release stamped on spans would not name the release at all, it would land in metadata. Two arguments that work in one mode and silently do nothing in the other is the worse answer; a warning naming the variable that does work is the honest one. Stamping the environment per span through a processor was the alternative and was refused for making the pair behave differently from each other. |
| 14 | **2026-09-04** — `score(name, value=None, ...)`: the value is **optional**, because a `categorical` or `text` score carries `string_value` and no number — which is how spec 018's own example calls it. And the two model names are two facts: `generation(model=…)` writes `gen_ai.request.model` (what was asked for) while `end(model=…)` and the response reader write `gen_ai.response.model` (what answered), so a call that names both keeps both, as `docs/ingest.md` already promises for that chain | Decision 5's signature was written for the numeric case and spec 018 #4 immediately needs the other one; a required positional that half the call sites pass `None` to is a signature that lies. The model split is what the mapper's own chain expects — it ranks the requested model above the answering one and keeps the loser in metadata — and collapsing the two onto one key would throw away the distinction the store is built to keep. |
| 15 | **2026-09-04** — The three exceptions live in **`_errors.py`** rather than in `_http.py` as the Package contract's file list has them, and the package ships a `py.typed` marker | `_config` raises `TracepadConfigError` and is imported *by* `_http`; putting the exceptions in the HTTP client would make configuration import the transport, or make one of the two do its imports inside a function. A module of three class statements is the smaller price. `py.typed` is what makes the annotations the package already carries visible to its callers' type checkers. |
| 16 | **2026-09-04** — The golden fixture is **`testdata/otlp/010-tracepad-sdk.pb`**, numbered like the rest of the corpus, written by `scripts/fixtures/tracepad_sdk.py` under the *pinned* SDKs of `scripts/smoke/requirements.txt`. The script fixes the ids with a generator on the provider and lays every timestamp of the finished export on a grid, which is the only editing done to the bytes. `make fixtures` runs it first and skips it with a message when `uv` is absent; the Go `-update` pass then regenerates goldens for bodies no builder produced, from the bytes on disk | The corpus is otherwise synthetic because it reproduces somebody else's SDK; this one *is* our SDK, and a builder written beside the mapper would be edited to agree with it — which is the one thing a fixture must not be able to do. What had to be pinned down instead is churn: ids and clocks change every run, so they are replaced, and the OTel version travels in the resource, so the pin the smoke test already maintains is the version this is generated under. A bump there moving this golden is the drift detector working. `uv` stays a dev prerequisite of the package half only, the way Node is of the interface: a Go-only checkout runs `make fixtures` and gets every golden. |
| 17 | **2026-09-04** — "Beside" (Decision 3) is implemented as **chain order**: each `tracepad.*` key follows the `langfuse.*` key it mirrors in the same chain, rather than through an equal-rank mechanism added to `rankedField` | The two differ in exactly one case — two spans of one trace, one carrying each dialect — and there the rationale of Decision 3 says the `langfuse.*` value should win, which is what chain order gives. Equal rank would hand it to whichever span arrived last. The simpler reading is also the one the spec argues for, and it keeps the table a list of strings (spec 002 #10). |

---

## Package contract

```
sdk/python/
  pyproject.toml            # name = "tracepad", requires-python >= 3.10
  src/tracepad/
    __init__.py             # init, observe, span, generation, event, update, update_trace, score, prompt, flush
    _config.py              # arguments → environment → TracepadConfigError
    _tracing.py             # the provider adaptation, the decorators, the context managers
    _generation.py          # the helper and the OpenAI-compatible reader
    _attributes.py          # the vocabulary of Decision 3: one constant per key, one serializer
    _scores.py              # the queue, the thread, the batch
    _prompts.py             # the cache and Prompt
    _http.py                # urllib, auth, TracepadError / TracepadHTTPError
    _log.py                 # the `tracepad` logger
  tests/
  README.md                 # what PyPI shows: install, init, three lines, a link to docs/sdk-python.md
```

**Public surface** (everything else is private):

| Name | Signature | Notes |
|---|---|---|
| `init` | `(host=None, key=None, *, environment=None, release=None, export=True) -> None` | Decision 2, 10. Idempotent. |
| `observe` | `(fn=None, *, name=None, type="span", capture_input=True, capture_output=True)` | Decorator, with or without parentheses. `type` ∈ the ten kinds `docs/ingest.md` lists; `"generation"` makes the wrapped call a generation whose `end` reads the return value as the response (Decision 5). |
| `span` / `event` | `(name, *, input=None, metadata=None) -> ContextManager[Observation]` | `event` is a zero-duration span with `tracepad.observation.type = "event"`. |
| `generation` | `(name, *, model=None, prompt=None, model_parameters=None, input=None) -> ContextManager[Generation]` | `Generation.end(response=None, *, model=None, usage=None, cost=None, output=None)`, `Generation.first_token()`. Leaving the block without `end` ends the span with what it has. |
| `update` / `update_trace` | Decision 11 | |
| `score` | Decision 7 | |
| `prompt` | Decision 8 → `Prompt` | `Prompt.compile(**variables) -> str \| list[dict]` |
| `flush` | `(timeout: float = 10.0) -> None` | Scores, then the provider's `force_flush`. |
| `TracepadError`, `TracepadHTTPError`, `TracepadConfigError` | Decision 9 | |

**`Observation`** (what `span`, `event` and the decorator hand out) carries
`trace_id`, `span_id` and `update(...)`; **`Generation`** adds `end` and
`first_token`. Both are thin over the OTel `Span` and expose it as `.span`.

**Serialization** (one function, `_attributes.dumps`): `json.dumps(value,
default=repr, ensure_ascii=False, separators=(",", ":"))`. A `str` is sent
as is — a plain-text prompt is a plain-text payload, not a JSON string of
one (spec 015 #12).

**Exceptions inside a decorated function** end the span with status
`ERROR` and the exception recorded as an OTel event — the mapper's rule for
levels (`docs/ingest.md`) — and the exception propagates unchanged.

---

## Ingest contract (the `tracepad` dialect)

| Attribute | Field | Rank |
|---|---|---|
| `tracepad.trace.name` | trace name | beside `langfuse.trace.name`, before the root span's name |
| `tracepad.trace.tags` | tags | beside `langfuse.trace.tags` |
| `tracepad.trace.metadata` | trace metadata | beside `langfuse.trace.metadata` |
| `tracepad.observation.type` | observation type | beside `langfuse.observation.type` |
| `tracepad.observation.level` | level | beside `langfuse.observation.level` |
| `tracepad.observation.status_message` | status message | beside `langfuse.observation.status_message` |
| `tracepad.observation.metadata` | observation metadata | beside `langfuse.observation.metadata` |
| `tracepad.observation.completion_start_time` | completion start | beside `langfuse.observation.completion_start_time` |
| `tracepad.prompt.name` / `tracepad.prompt.version` | prompt | beside `langfuse.observation.prompt.*` |

"Beside" means the same rank: when both are present the `langfuse.*` key
wins, since a span carrying both was written by two SDKs and the older one
is the one the operator configured first. All are level-agnostic (spec 012
#7); the trace-level three are ranked like `session_id` (spec 012 #11). The
`gen_ai.*`, `user.id`, `session.id` and `deployment.environment.name` keys
the package writes are already in the table. `dialectOf` returns
`"tracepad"` when a span carries any `tracepad.*` key other than the run
link of spec 014, ranked above `genai` and below `langfuse`.

---

## Testing

**Unit** (`pytest`, `InMemorySpanExporter`, no network):

- `init` on a fresh process creates a provider; `init` after
  `trace.set_tracer_provider(...)` adds a processor to *that* provider and
  creates none; a second `init` is a no-op; `export=False` attaches no
  exporter; no host and no key raise `TracepadConfigError`.
- `@observe` on a sync function, an `async` function, a sync generator and
  an async generator: one span each, `input` is the arguments by name,
  `output` the return value or the yielded list, `self` absent; the opt-outs
  leave the attribute unset; `update` inside replaces; an exception ends the
  span `ERROR` with the event and propagates.
- `generation.end(response=…)` on a `dict`, on a pydantic-like object, and
  on an OpenRouter-shaped `usage` with `cost`: every key of Decision 5 lands
  under its `gen_ai.*` name; explicit arguments win; `first_token` stamps
  once. `dumps` of a `str` is the `str`.
- `score` inside a span targets it; `observation=True` adds the span id;
  outside a span without `trace_id` raises; the queue batches at 100 and at
  2 s (a fake clock), retries once, logs the server's message and drops.
- `prompt` caches for `max-age`, refreshes after, serves stale on a
  connection error with a warning, raises with nothing cached; `compile`
  substitutes in text and in messages.
- The mapper: a golden fixture `testdata/otlp/tracepad-sdk.pb`, generated by
  the package's own exporter against a fake collector with fixed ids (the
  `make fixtures` path), asserting every row of the Ingest contract; a span
  carrying both `tracepad.trace.name` and `langfuse.trace.name` resolves to
  the latter; `dialectOf` labels.

**Mutation** — every invariant above by reverting its line; the table in
the PR.

**E2e** (`sdk/python/tests/e2e`, the `sdk` CI job): a real binary on a temp
database, the package exporting a trace with a decorated function, a
generation ended from a canned OpenAI-shaped response with `usage.cost`, a
score from inside the span, a prompt created through the API and fetched by
label; then, through the read API, the trace's name, user and tags, the
generation's model, `usage` and `total_cost`, the score's presence and the
prompt name/version on the observation.

**Smoke**: `scripts/smoke/export_tracepad.py` beside the two existing
exporters, pinned to the package's own path (an editable install from
`sdk/python`), asserted by `check.py` like the others.

**Measurements in the PR**: `make sdk-lines` (budget 1,500 across 017 and
018); the wheel's size; the import time of `import tracepad` (must stay
under the OTel SDK's own, which it re-exports nothing of).

---

## Edge cases

- **`init` under an instrumented FastAPI app** whose provider was set by
  `opentelemetry-instrument`: the processor is added, and the app's request
  spans and ours share a trace. This is the e2e's second scenario.
- **The application already exports to the store through the Langfuse
  SDK** and calls `init` with the default `export=True`: every span arrives
  twice, is upserted once (spec 002 #6), and `docs/sdk-python.md` says to
  pass `export=False`. No detection is attempted — reading another
  exporter's endpoint out of a provider is private API.
- **`@observe` on a method of a class instance that is not JSON**: `self`
  is dropped; another argument that is not JSON is `repr`'d, never a
  failure.
- **A generator closed early** (`break` out of a `for`): the span ends at
  `close()` with the values yielded so far.
- **`score` after `flush` at interpreter exit**: the thread is gone; the
  score is logged as dropped, not silently lost.
- **A prompt label moved while cached**: served for up to 60 s more, as the
  server's `max-age` allows and `docs/prompts.md` explains.
- **`generation.end(response=…)` with a response that has no `usage`**
  (a stream's final chunk without `stream_options`): model and output are
  read, usage and cost stay unset — the store shows *no data*, never `$0`.
- **A `tracepad.*` key on a span exported by a different SDK**: claimed all
  the same; the dialect is a vocabulary, not a signature.

---

## Config additions

None on the server. The package reads Decision 10's four variables.

---

## Out of scope

- The eval harness — spec 018.
- A JavaScript package (design §6.5: TypeScript second, by demand).
- Any provider-client wrapper or auto-instrumentation; OpenLLMetry and the
  OTel GenAI instrumentations remain the answer, and they work because the
  transport is shared.
- A local price table; the cost is what the provider said.
- Prompt templating beyond `str.format` placeholders.
