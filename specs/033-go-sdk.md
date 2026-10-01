# Spec 033 — The Go package: `tracepad.Init`, spans and generations in context, scores, prompts and the eval harness

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> Go is where the agents and the services that call models from the
> backend are written when they are not written in Python, and it is the
> language the store itself is in. An application in Go already has the
> OpenTelemetry SDK one import away; what it does not have is the ten
> lines that make a span a *step*, a span a *generation* with its usage
> and the price the provider charged, a score against the trace in flight
> and a prompt by label. This spec adds the `tracepad` Go package: the
> same surface as specs 017 and 032, in the shape Go gives it — a
> `context.Context` in, a `context.Context` out, and nothing global that
> the language would not forgive.

---

## Overview

Deliverables, two PRs in this order (the last commit of the second flips
the status):

- **PR A — the package**: a **nested Go module** `sdk/go`
  (`github.com/tracepad/tracepad/sdk/go`, package `tracepad`), thin over
  `go.opentelemetry.io/otel` (Decision 1): `Init` and its `shutdown`, `Span`,
  `Event`, `Generation` with `End`, `FirstToken` and an explicit
  `Result`, `Update`, `UpdateTrace`, `Score`, `Prompt`, `Flush`
  (Decisions 2–9); `docs/sdk-go.md`; the quickstart's Go paragraph; a
  golden fixture written by the package (Decision 12); the `sdk-go` CI
  job (Decision 11).
- **PR B — the eval harness**: `Dataset`, `Run`, `run.Item(ctx, case)`,
  `ScoreConfigs`, `Compare`, `ItemID` (Decision 10); the *same loop from
  Go* section in `docs/datasets.md`; `docs/sdk-go.md#evals`.

Not here: an OpenAI-compatible response reader (Decision 5 says why), a
streaming wrapper, any wrapper of a provider client (design §6.5), a
`Client` type with per-instance configuration beyond what tests need
(Decision 2).

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-14** — **A nested module**, `sdk/go/go.mod` with module path `github.com/tracepad/tracepad/sdk/go`, package name `tracepad`, **Go 1.22 or newer**, dependencies `go.opentelemetry.io/otel`, `go.opentelemetry.io/otel/sdk`, `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp` and nothing else — no dependency on the server's module, no `replace`. Released by the tag **`sdk/go/vX.Y.Z`** (the Go toolchain's rule for a module in a subdirectory); `REPOS.md`'s `sdk-go/` prefix is corrected to this. No release workflow: the tag is the release, and the `sdk-go` CI job is what runs on it | A package inside the root module would hand every user the server's dependency graph (SQLite, the UI embed, the MCP server) in their `go.sum`; a nested module is the standard answer and `go get github.com/tracepad/tracepad/sdk/go` is what a reader expects to type. Thin over OTel for spec 017 #1's reasons. The tag form is not a preference: `go get …/sdk/go@v0.1.0` resolves the tag `sdk/go/v0.1.0` and nothing else. |
| 2 | **2026-09-14** — **`Init(ctx, opts ...Option) (shutdown func(context.Context) error, err error)`** with `WithHost`, `WithKey`, `WithEnvironment`, `WithRelease`, `WithExport(bool)`, `WithLogger(*slog.Logger)`, `WithTracerProvider(trace.TracerProvider)`; the environment (`TRACEPAD_HOST`, `TRACEPAD_API_KEY`, `TRACEPAD_ENVIRONMENT`, `TRACEPAD_RELEASE`) fills what the options did not (spec 017 #10). It **adapts to the provider it finds**: when `otel.GetTracerProvider()` is an SDK `*sdktrace.TracerProvider`, the call registers a `BatchSpanProcessor` with an OTLP/HTTP exporter aimed at `{host}/v1/traces` on it; when it is the API's no-op default, the call builds one — resource from `resource.Default()` merged with `service.name` (`OTEL_SERVICE_NAME` or the executable's name), `deployment.environment.name`, `service.version` — and sets it global with a `TraceContext` propagator. `WithEnvironment`/`WithRelease` are refused with a warning when `Init` adopts a provider it did not build (spec 017 #13). A second `Init` is a no-op with a warning. The package keeps **one default** the functions below use; there is no exported client type | Spec 017 #2 in Go: an application on `otelhttp` or `otelgrpc` has a provider, and replacing it would detach them. Options rather than a struct because that is how the OTel Go SDK itself is configured, so the reader learns nothing new. One package-level default because a span helper that needed a receiver on every call would not be a helper; the explicit provider option is what tests and the rare two-destination application use. |
| 3 | **2026-09-14** — The attributes are **exactly spec 017 #3's**, one constant per key in `attributes.go`, one serializer (`encoding/json` with `SetEscapeHTML(false)`; a `string` is sent as is, spec 015 #12; a value that does not marshal is sent as `fmt.Sprint` of it) | The store reads one dialect; the fixture of Decision 12 proves the package speaks it. |
| 4 | **2026-09-14** — **`Span(ctx, name, opts ...SpanOption) (context.Context, *Observation)`** and **`Event(ctx, name, …)`** open a span under the context's current span and return the context carrying it; the caller ends it with `obs.End()` (the Go idiom: `defer obs.End()`), and `obs.Fail(err)` records the error as an OTel event, sets status `ERROR` and ends. Options: `WithInput(any)`, `WithMetadata(any)`, `WithType(string)`. There is **no function wrapper like `observe`**: Go has no decorators and no way to capture a function's arguments by name; the input is what the caller hands `WithInput` | The OTel Go API is `ctx, span := tracer.Start(ctx, …)` / `defer span.End()`, and a helper that changed that shape would fight every other instrumentation in the process. `Fail` exists because `defer` cannot see the error the function is about to return, and the three lines that record it are the ones every caller would write. |
| 5 | **2026-09-14** — **`Generation(ctx, name, opts ...GenerationOption) (context.Context, *Generation)`** with `WithModel`, `WithPrompt(*Prompt)`, `WithModelParameters(map[string]any)`, `WithInput`; **`gen.End(Result{Model, Usage, Cost, Output})`** writes the model, every `Usage` entry under `gen_ai.usage.<key>`, the cost and the output, then ends; **`gen.FirstToken()`** stamps the completion start once; `gen.Fail(err)` as on a span. `Usage` is `map[string]int64` and `Cost` is `*float64` — absent is absent, never zero. **No OpenAI-compatible reader, no stream wrapper**: `docs/sdk-go.md` shows the five lines that fill a `Result` from `openai-go`'s `ChatCompletion` and the three that stamp `FirstToken` in a stream loop | Go has no dominant client with one response shape the way Python and Node have the OpenAI envelope: `openai-go`, `anthropic-sdk-go`, `go-openai` and the raw `net/http` caller each hand back a different struct, and a reader over `any` would be reflection guessing at field names. An explicit `Result` is five lines at the call site and cannot silently read the wrong field. A pointer for the cost is the language's own "not set", and spec 002 #14's "never $0" needs exactly that. |
| 6 | **2026-09-14** — **`Score(ctx, name string, opts ...ScoreOption) error`** — `WithValue(float64)`, `WithStringValue`, `WithDataType`, `WithComment`, `WithID`, `WithTraceID`, `WithObservationID`, `OnObservation()` — targets the context's span when none is given and returns **`ErrNoTrace`** with no span and no id (spec 017 #7). It **does not call the server**: it enqueues, and a goroutine started by `Init` posts `POST /api/v1/scores` in batches of up to 100 every 2 s or when the batch fills; a rejected batch is retried once, then logged and dropped (spec 017 #6). **`Flush(ctx) error`** drains the queue, then the provider's `ForceFlush`; **`shutdown`** (returned by `Init`) flushes and shuts the provider down when the package built it, and only flushes when it adopted one | Spec 017 #6–#7. An error return instead of a panic is Go's `ValueError`, and a sentinel so the caller can `errors.Is`. The goroutine is the daemon thread; `shutdown` is `atexit`, and it is returned rather than registered because Go has no exit hook and every server already has a place where it closes things. |
| 7 | **2026-09-14** — **`Prompt(ctx, name string, opts ...PromptOption) (*Prompt, error)`** with `WithLabel`, `WithVersion`; `Prompt` has `Name`, `Version`, `Text`, `Messages`, `Labels`, `Config` and `Compile(vars map[string]any) (string \| []Message)` — `{name}` substitution as spec 017 #8; cached in memory per `(name, label \| version)` for the `Cache-Control: max-age` the server sent, served stale on transport or 5xx error with a warning, and an error when nothing is cached (`*HTTPError` or the transport's). `Generation(…, WithPrompt(p))` writes `tracepad.prompt.name` / `tracepad.prompt.version` | The rules are the Python ones. Two return shapes for `Compile` are one method with two fields (`Text`, `Messages`) on the result, not an interface: a chat prompt and a text prompt are different data and the caller knows which they asked for. |
| 8 | **2026-09-14** — **Failure semantics are spec 017 #9's**: the tracing path (`Init` after configuration, `Span`, `Generation`, `Update`, `End`, the exporter, the score queue) never panics and logs through `slog` (the default logger, or `WithLogger`); the REST path returns errors — `*HTTPError{Status, Body}` for a non-2xx answer, the transport's error otherwise; `Init` with no host or key returns **`ErrConfig`** (wrapped with what is missing). REST goes over `net/http` with the default client and a 10 s timeout | Same rules; Go spells "never raises" as "never panics, returns errors", and `slog` is the standard library's logger since 1.21, so no dependency. |
| 9 | **2026-09-14** — **`Update(ctx, opts ...UpdateOption)`** (`WithName`, `WithInput`, `WithOutput`, `WithMetadata`, `WithLevel`, `WithStatusMessage`, `WithType`) and **`UpdateTrace(ctx, …)`** (`WithTraceName`, `WithUserID`, `WithSessionID`, `WithTags`, `WithTraceMetadata`) act on the context's span (spec 017 #11), log and do nothing without one. `Observation` carries `TraceID()`, `SpanID()`, `Span()` (the OTel span) | Same contract, Go names. |
| 10 | **2026-09-14** — **The harness** (PR B): `Dataset(name) *Dataset` (no request made), `PutItems(ctx, items) (version, changed int, err)`, `Items(ctx, version) iter.Seq2[Item, error]` over the 500-item pages, `Create`, `Delete(ctx, confirm)`, `Run(ctx, name, opts…) (*Run, error)`, `Runs(ctx)`. **`run.Item(ctx, case) (context.Context, *Attempt)`** returns a context carrying `(runID, itemID)`; a `SpanProcessor` registered by `Init` stamps `tracepad.run_id` and `tracepad.item_id` in `OnStart` on every span whose context carries them, whoever started it; the `Attempt` records every root span it saw (`Traces()`, `TraceID()`) and `Score(ctx, name, opts…)` posts against the last. **`run.Finish(ctx)`** flushes scores, then the provider, then posts; `run.Fail(ctx, err)` likewise. `ScoreConfigs(ctx, configs)`, `Compare(ctx, a, b)`, `ItemID(key) string` as in spec 018 #6–#7. The read side is the server's JSON decoded into `map[string]any` (spec 018 #8) | Spec 018 in Go: the item block is a context, because in Go a context *is* the block, and the processor reads it at `OnStart` the way the Python one reads the `ContextVar`. `iter.Seq2` because Go 1.23 has range-over-func and a paging generator is what it is for; the floor stays 1.22 for the package, and `Items` is built with the `iter` package from 1.23 — so the floor is **1.23** for PR B, stated in `go.mod` then. |
| 11 | **2026-09-14** — **CI.** The `sdk-go` job runs `go vet` and `go test ./...` inside `sdk/go` on the two newest Go lines, then the e2e against a real binary through `scripts/sdk-go-test.sh` (build the server, start it on a free port, run the package's `e2e` test package with `TRACEPAD_BINARY`), and `make sdk-go-lines` against a **1,600**-line application budget (`scripts/sdk-go-lines.sh`). `make gate` includes the module's tests. `make smoke` gains the package as a pinned exporter | Spec 017 #12's shape. Two Go lines like two Pythons. No release workflow (Decision 1). |
| 12 | **2026-09-14** — **The golden fixture** is `testdata/otlp/012-tracepad-sdk-go.pb`, written by `scripts/fixtures/tracepad_sdk_go/main.go` (a small program in the SDK module's test tree, with an in-memory OTLP collector) and read back by the server's OTLP suite like 010 and 011 | Decision 3's proof. |
| 13 | **2026-09-14** — The fixture is **`testdata/otlp/014-tracepad-sdk-go.pb`**, not 012: the corpus already had `011-plain-text-prompt` (spec 015) and `012-claude-code-interaction` (spec 030) when this spec was written, and spec 032's fixture takes 013. The program that writes it lives **inside the SDK module**, at `sdk/go/internal/fixture/main.go`, and `make fixtures` runs it as `cd sdk/go && go run ./internal/fixture …` before the Go golden pass; the smoke exporter sits beside it (`sdk/go/internal/smoke`). It is deterministic without post-processing: a fixed id generator on the provider and an exporter wrapper that lays the batch's instants on the grid as the OTLP exporter reads them — so a second run is byte-for-byte the first, and `Usage` and model-parameter keys are written in sorted order, which is what made it so | Decision 12's path, `scripts/fixtures/tracepad_sdk_go/main.go`, is in the *root* module's tree: a Go program there could import the package only through a `replace` in the server's `go.mod`, which would hand the server the OTel SDK's dependency graph for the sake of a script — the reverse of what Decision 1 refuses. `internal/` keeps the two programs out of the module's public API and off pkg.go.dev. A fixture the mapper reads back is also seeded into the interface's end-to-end corpus, whose counts moved by two traces and one session (PR A says which expectations). |
| 14 | **2026-09-14** — **Go has one namespace for a function and a type**, so the handles are named apart from the calls: `Generation(ctx, …)` returns a **`*Call`**, `Prompt(ctx, …)` returns a **`*PromptVersion`**, and `Compile` returns a **`Compiled`** with `Text` or `Messages` filled. The calls keep the spec's names — `Span`, `Event`, `Generation`, `Score`, `Prompt` — because they are what every call site reads; the types appear in signatures and struct fields. `UpdateTrace`'s options are a `TraceOption`; `WithInput`, `WithMetadata` and `WithType` are `SpanOption`s, which every `Generation` and `Update` also takes, so one constructor serves the three calls | The contract's `Generation` (function) beside `Generation` (type) and `Prompt` beside `Prompt` do not compile. The call is the shape the docs family shares (spec 017, 032); a handle named after what it is — a call to a model, one version of a stored prompt — costs a Go reader nothing, and the OTel Go API itself pairs `tracer.Start` with `trace.Span`. |
| 15 | **2026-09-14** — **The floor is Go 1.25**, not 1.22, and the module pins **OTel v1.46.0** (current). Decision 10's "1.23 for PR B" is moot: `iter` is in. CI's two lines are 1.26 and 1.27; the module also builds and tests on 1.25 (verified with `GOTOOLCHAIN=go1.25.0`) | A `go.mod` cannot declare a floor below its dependencies', and every OTel release since v1.42 says `go 1.25.0`; the last one on 1.22 is v1.35, a year old. Pinning that for the sake of the number would hand every user a stale SDK that MVS upgrades the moment anything else in their graph asks for a newer one, so the floor would be a fiction either way. |
| 16 | **2026-09-14** — **The harness's shapes.** `NewDataset(name) *Dataset` (Decision 14's rule again: `Dataset` is the type); `run.Item(ctx, item Item)` takes the `Item` struct, so a bare id is `Item{ID: id}` and a case with no id is logged and stamps nothing rather than an error return the contract does not have; `Finish` and `Fail` return the closed run; there is no block that closes a run, so `Fail` on the error path is the caller's line, and the docs say so. The processor test starts the framework's span with another tracer from the item context instead of `otelhttp`'s handler | A typed parameter is what the compiler checks; Python's `case_id` accepts three shapes because Python has to. `otelhttp` would be a test-only dependency in the module's `go.mod` for a test that proves what a bare tracer proves the same way: the processor reads the *context* at `OnStart`, and what started the span is not its business. |
| 17 | **2026-09-14** — The application-line budget is **1,900**, not 1,600. PR A landed at 1,413 after its three review rounds; the harness is 429 lines (`harness.go` 276, `datasets.go` 148, five in `Init`); 1,842 with both, and one review cycle needs room | Spec 031 #22's raise rule: the measured `main`, plus the measured addition, plus a cycle. The lines are the feature Decision 10 asked for — a paging iterator, a processor, two clients and their options — in a language that spells every error out; what a budget of 1,600 would have asked to revise is the error handling. `make sdk-go-lines` reports the number, and the PR reports it before and after. |
| 18 | **2026-09-26** — **The key and the path**, the Go side of spec 017 #19. (a) **Both clients redirect by origin** (`CheckRedirect: sameOrigin`): a hop within the store's scheme, host and port keeps the key, a hop anywhere else drops `Authorization` for the rest of the chain, and the chain stops at ten, net/http's own limit. **A write is never re-sent**: a redirect of any method but `GET` and `HEAD` is an error naming its target. (b) **`config` and `options` print without the key**: `String` and `GoString` redact it under `%v`, `%+v`, `%#v` and `%s`. A known limit: the OpenTelemetry exporter holds the header in its own objects, which are not this package's to change. (c) **The run id the server issued and both ids of `Compare` are escaped** with `url.PathEscape`, as every name already was. (d) **A path with an empty, `.` or `..` segment is an error before any request.** (e) `sdk/go/NOTICE` is a copy of the repository's, kept identical by `make sdk-notices`; the module zip already carries `LICENSE`, because the go command copies the repository root's into a nested module that has none. (f) Budget (spec 036 #8): `main` measured 2,494 application lines, this adds 53, to 2,547, and the budget rises to **2,575**; the review round took it to 2,550 | (a) net/http drops `Authorization` only when the *host name* changes and keeps it for a subdomain, so a redirect from `https://store` to `http://store` — a load balancer that downgrades — sent the key in clear text, and one to another port on the same host sent it there. `fetch` compares origins, and the Node and Go packages share its rule. net/http also turns a `POST` into a `GET` on 301–303, and a score batch would have read a listing's `200` as delivered (found in review of PR #89). The https-to-http downgrade on one host and port is not tested: a local server cannot hold both schemes on one port. (b) Both types are unexported and nothing prints them today; a debugging `%+v` is the one-line mistake that would put the key in a log. (d) As spec 032 #17 (d): the store's router answers `..` with a redirect to the path it names, which is never the object meant. |
| 19 | **2026-09-28** — **`PutItems` writes a long list in batches**, as spec 018 #15 has `put_items` do: more than 10,000 items go as consecutive writes of 10,000, `(version, changed)` is the last version and the sum, a failure returns its error (and zeros, as every error here does) with the writes before it in place, and a list that gives one `ID` twice returns an error before anything is sent. The application-line budget moves to **2,600** | Spec 014 #34 caps a request at 10,000 items. The package landed 7 lines over 2,575 with the split and the repeated-id check; the raise rule of spec 031 #22 moves the number. |
| 20 | **2026-09-29** — **The address of the store is `TRACEPAD_URL`; `TRACEPAD_HOST` is a deprecated synonym** (amends spec 017 #10). The Go package reads the argument, then `TRACEPAD_URL`, then `TRACEPAD_HOST`; when only the old name is set it works and says once per process that it is deprecated; when both are set the new one wins, silently. The secret dialog and `tracepad keys create` print `TRACEPAD_URL` alone (amends spec 045 #23). The line budget is 2,620 (from 2,600): the picker and its warning. | The three packages are not published yet, so this is the one moment the rename is free: after it, every `.env` in the wild names the variable. The CLI (spec 004 #15), the MCP server and the server (spec 028 #11) already read `TRACEPAD_URL`, so a team's shared `.env` or Compose file carried two names for one address, and the server warned about the second as an unknown variable; `docs/agents.md` admitted it was "the variable an agent mixes up". The synonym costs a dozen lines a package and keeps a `.env` written from the old docs working; it is not removed before 1.0. |
| 21 | **2026-10-02** — **A workflow checks the tag, and publishes nothing.** Amends #1 and #11's "no release workflow": `release-sdk-go.yml` runs on `sdk/go/v*`, holds the tag to `const Version` in `http.go` (`scripts/sdk-go-release-check.sh`, also the step before pushing it) and runs the unit suite. A pre-release is `sdk/go/v0.1.0-rc.1` with `Version = "0.1.0-rc.1"`; the toolchain accepts that spelling, and a tag fetched from a git repository by that name resolved (rehearsal, spec 020 #28) | #1 said the tag is the whole release, and it is: the check cannot hold the proxy back, and nothing here claims to. What it changes is that a tag naming a different version than the one `Version` sends as the User-Agent is a red run within minutes, while nobody has asked the proxy for it, and a tag nobody fetched can still be deleted. The `Version` was the one number nothing compared with the tag (spec 020 #28 has the other two). |

## Package contract

```
sdk/go/
  go.mod                  # module github.com/tracepad/tracepad/sdk/go; go 1.25 (Decision 15)
  tracepad.go             # Init, Shutdown, Option; the default and its adaptation
  span.go                 # Span, Event, Observation, Update, UpdateTrace
  generation.go           # Generation, Result, Usage, FirstToken
  attributes.go           # the vocabulary of spec 017 #3: one constant per key, one serializer
  scores.go               # Score, the queue, the goroutine, Flush
  prompts.go              # Prompt, the cache, Compile
  http.go                 # net/http, auth, HTTPError, ErrConfig, ErrNoTrace
  harness.go              # PR B: the processor, Run, Attempt, ScoreConfigs, Compare, ItemID
  datasets.go             # PR B: Dataset, Item
  e2e/                    # against a real binary (TRACEPAD_BINARY)
  internal/fixture/       # writes testdata/otlp/014-tracepad-sdk-go.pb (Decision 13)
  internal/smoke/         # the exporter `make smoke` runs (Decision 11)
  README.md               # what pkg.go.dev shows: install, Init, three lines, a link to docs/sdk-go.md
```

**Public surface** (everything else is unexported): `Init`, `Flush`,
`Span`, `Event`, `Generation`, `Update`, `UpdateTrace`, `Score`, `Prompt`,
the option constructors named above, `Observation`, `Call` (the generation's
handle), `Result`, `Usage`, `PromptVersion`, `Compiled`, `Message`,
`HTTPError`, `ErrConfig`, `ErrNoTrace` (Decision 14); PR B: `Dataset`,
`Item`, `Run`, `Attempt`, `ScoreConfig`, `ScoreConfigs`, `Compare`, `ItemID`.

## Ingest contract

Unchanged: the package writes the `tracepad.*` dialect of spec 017 and the
GenAI conventions. The fixture of Decision 12 pins it.

## Testing

- Unit (in-memory span exporter from `sdk/trace/tracetest`): `Init`
  adopts an SDK provider by registering a processor, builds one when the
  global is the no-op, warns on a second call, refuses environment and
  release on an adopted provider, returns `ErrConfig` without host or key;
  `Span`/`Event`/`Generation` write the attributes of spec 017 #3, `End`
  and `Fail` set status and the event, `Result` with a nil cost writes no
  cost, `Usage` keys land under `gen_ai.usage.`; `FirstToken` once;
  `Score` enqueues, batches at 100 and at 2 s, retries once, drops with a
  log line, returns `ErrNoTrace`; `Flush` drains; `Prompt` caches for
  `max-age`, serves stale on 503, errors on an empty cache, `Compile` on
  text and on messages; `Update` without a span logs; `shutdown` flushes
  and shuts down only what it built.
- E2E against a real binary: one `Span` with a `Generation` and a `Score`
  inside — `GET /api/v1/traces/{id}` shows the tree, the model, the usage,
  the cost and the score; a generation with `FirstToken` lands time to
  first token.
- PR B: the harness against the real binary — items put and read at a
  version, a run over three items with two roots each, `Traces` per
  attempt, scores on the right trace, `Finish` then `Get` with the
  summary, `Fail`, `Compare`; a unit test for the processor stamping a
  span started by `otelhttp`'s handler inside an item context.
- The fixture: `012-tracepad-sdk-go.pb` regenerated and read back by the
  server's OTLP suite.
- Docs: the anchor checker; the `docs/sdk-go.md` examples compile
  (`go vet` over an `examples_test.go` in the module).
- CI: `sdk-go` job green on the two newest Go lines; `make gate` green
  with the module included.

## Out of scope

- A response reader or a stream wrapper — Decision 5.
- A `Client` type beyond `WithTracerProvider` for tests — Decision 2.
- Any provider-client wrapper — design §6.5.
