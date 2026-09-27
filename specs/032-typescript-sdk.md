# Spec 032 — The TypeScript package: `init`, `observe`, generations, prompts, scores and the eval harness for Node

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> Spec 017 gave Python the one line that points an application at
> Tracepad and the ten after it — a step as a span, a generation with its
> model, usage and the cost the provider charged, a score against the trace
> in flight, a prompt by label — and spec 018 the eval loop over it. Node
> is the other half of where LLM applications are written, and today it
> has the raw OpenTelemetry SDK and `fetch`. This spec adds the `tracepad`
> package on npm: the same surface, the same vocabulary on the wire, the
> same failure rules, shaped for a language where every step is a
> promise.

---

## Overview

Deliverables, two PRs in this order (the last commit of the second flips
the status):

- **PR A — the package**: `tracepad` under `sdk/js/`, thin over the
  OpenTelemetry Node SDK (Decision 1): `init`, `observe`, `span`, `event`,
  `generation` with the OpenAI-compatible reader and the streaming
  pass-through, `update`, `updateTrace`, `score`, `prompt`, `flush`
  (Decisions 2–9); `docs/sdk-js.md`; the quickstart's *Point an
  application at it* gains the Node paragraph; a golden fixture written by
  the package itself (Decision 12); the `sdk-js` CI job and
  `release-sdk-js.yml` (Decision 11).
- **PR B — the eval harness**: `dataset`, `Run`, `run.item(case, fn)`,
  `scoreConfigs`, `compare`, `itemId` (Decision 10); the *same loop from
  Node* section in `docs/datasets.md`; `docs/sdk-js.md#evals`.

*Amended:* the fixture is `013-tracepad-sdk-js.pb` (Decision 13).

Not here: a browser build (owner decision 2026-09-14: no browser SDK until
a live request; the package is Node-only and says so); any wrapper of a
provider client (design §6.5: never); Deno and Bun as supported targets
(they may work; nothing is tested there); the Go package (spec 033).

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-14** — One package, **`tracepad` on npm**, source in `sdk/js/`, TypeScript, **thin over the OpenTelemetry JS SDK**: it owns no transport, no batching, no retry and no context propagation — those are `@opentelemetry/sdk-trace-node`, `@opentelemetry/exporter-trace-otlp-proto` and `@opentelemetry/api`, its only runtime dependencies (the `api` as a peer dependency, the two SDK packages as dependencies). Node **20 or newer** (global `fetch`, `AsyncLocalStorage`). Shipped as **ESM and CommonJS** with type declarations, built by one zero-config bundler (`tsup` or equivalent — the implementer's choice, named in the README), `exports` map pointing at both. No runtime dependency beyond the three | Spec 017 #1 word for word, for the same reasons: the OTel SDK is the transport every instrumentation already uses, and a package that re-implemented batching would be a second exporter with a second set of bugs. Node 20 is the oldest supported line and the first with stable `fetch`; targeting older would mean a polyfill dependency for a runtime nobody should be on. Dual output because half the ecosystem still `require`s, and a package that refuses the other half is not thin, it is picky. The name is free on npm (checked 2026-09-14) and matches PyPI. |
| 2 | **2026-09-14** — `init({host?, key?, environment?, release?, export?})` **adapts to the provider it finds**: when the application has registered a global `TracerProvider` (through the `@opentelemetry/api` global), the call adds a `BatchSpanProcessor` with an OTLP/HTTP protobuf exporter aimed at `{host}/v1/traces` to it — which requires the provider to expose `addSpanProcessor`; a provider that does not is refused with a warning and nothing is attached; when there is none, the call builds a `NodeTracerProvider` — resource `service.name` from `OTEL_SERVICE_NAME` or `process.title`, `deployment.environment.name` and `service.version` from the arguments — registers it with the `AsyncLocalStorage` context manager, and sets it global. A second `init` is a no-op with a warning. `export: false` attaches everything except the exporter. `environment` and `release` are refused with a warning when `init` adopts a provider it did not build (spec 017 #13) | Spec 017 #2 applied to Node: an application on Express or Next with an instrumentation already registered must not lose it, and the OTel API refuses to override a global provider for that reason. The `addSpanProcessor` requirement is the JS SDK's own shape (the 2.x `NodeTracerProvider` takes processors in its constructor and exposes none after; the 1.x one exposes `addSpanProcessor`) — so the adaptation is tested against both major lines and the refusal is the honest answer where the API has no hook, with `docs/sdk-js.md` saying to pass the processor `tracepad.spanProcessor()` returns to the provider's constructor instead. |
| 3 | **2026-09-14** — The attributes are **exactly spec 017 #3's**: the OTel GenAI semantic conventions where a name exists, `tracepad.*` where none does, one constant per key in `attributes.ts`, one serializer (`JSON.stringify` with a replacer that turns a `bigint` into a number, an `Error` into `{name, message}` and anything unserializable into its `String()`; a `string` is sent as is, spec 015 #12). The mapper gains no new key: the package writes what the Python one writes | The store reads one dialect and the fixture of Decision 12 proves the package speaks it. A second vocabulary for a second language is the drift `internal/mapping/rules.go` exists to refuse. |
| 4 | **2026-09-14** — **`observe(fn, {name?, type?, captureInput?, captureOutput?})`** wraps a function and returns one of the same shape: a synchronous function runs inside `context.with` and ends the span on return; a function returning a promise ends it on settlement; an async generator or a generator records the list of yielded values as the `output` (spec 017's rule) and ends on completion. The name defaults to `fn.name`, then `"anonymous"`. Arguments become `input` as a positional array — JavaScript has no parameter names at runtime worth trusting after a bundler — and the return value `output`, unless told otherwise. A thrown error or a rejected promise ends the span with status `ERROR`, the exception recorded as an OTel event, and propagates unchanged. There is **no decorator syntax**: `observe` is a wrapper, and `docs/sdk-js.md` shows it on a method with a one-line assignment | Decorators in TypeScript are two incompatible proposals and a compiler flag; a wrapper works in every setup, on arrow functions and on methods, and is what every instrumented codebase in Node already looks like. Positional `input` rather than named because the names are gone after minification and wrong after a `.bind`; an object built from them would be fiction. |
| 5 | **2026-09-14** — **`span(name, {input?, metadata?}, fn)`**, **`event(name, …)`** and **`generation(name, {model?, prompt?, modelParameters?, input?}, fn)`** are **callback-scoped**: `fn` receives the `Observation` (or `Generation`) and runs inside its context; the span ends when `fn` returns or its promise settles, unless the callback ended it. `generation` hands a `Generation` with `end(response?, {model?, usage?, cost?, output?})`, `firstToken()` and **`stream(chunks)`** — an async iterable that yields each chunk through and ends the generation on exhaustion with the gathered model, output, usage and cost, exactly spec 031 #7 and #21 (ended by the enclosing callback on `break` or a throw, not by the wrapper). The OpenAI-compatible reader is spec 017 #5's table field for field | `with` does not exist in JavaScript and `using` is not yet in Node's stable line; a callback is the shape `context.with` already has and the one that guarantees the span ends. The streaming helper is here from the first version because Node is where streaming is the default, not the exception. |
| 6 | **2026-09-14** — **`score(name, value?, {stringValue?, dataType?, comment?, id?, traceId?, observationId?, observation?})`** finds its target from the active span when none is given and **throws** with no active span and no `traceId` (spec 017 #7); it **does not call the server**: it enqueues, and a timer (unref'd, so it never holds the process open) posts `POST /api/v1/scores` in batches of up to 100 every 2 s or when the batch fills; a rejected batch is retried once, then logged and dropped (spec 017 #6). **`flush({timeout?})`** drains the queue, then the provider's `forceFlush`. A `beforeExit` listener registered by `init` calls `flush` once, so a script that returns without calling it still delivers | Spec 017 #6 and #7. `unref` is the Node idiom for "a helper timer must not keep a finished program alive", and `beforeExit` is the closest thing Node has to `atexit` — it fires when the event loop drains, which is when a script is done, and it is not fired on `process.exit()`, which `docs/sdk-js.md` says out loud beside `flush`. |
| 7 | **2026-09-14** — **`prompt(name, {label?, version?})`** is **async** and returns a `Prompt` with `name`, `version`, `text` or `messages`, `labels`, `config` and `compile(variables)`; cached in memory per `(name, label \| version)` for the `Cache-Control: max-age` the server sent, served stale on transport or 5xx error with a warning, rejected with `TracepadError` when nothing is cached (spec 017 #8). `generation({prompt})` writes `tracepad.prompt.name` / `tracepad.prompt.version` | The rules are the Python ones; only the shape changes, and in Node a network read that was not `await`ed is a bug at the call site, so it is a promise and nothing else. |
| 8 | **2026-09-14** — **Failure semantics are spec 017 #9's**: the tracing path never throws into application code and logs through a `tracepad`-prefixed `console.warn` (overridable by `init({logger})`); the REST path rejects with `TracepadError` / `TracepadHTTPError(status, body)`; `init` with no host or key throws `TracepadConfigError`. Configuration is spec 017 #10's: the arguments, then `TRACEPAD_HOST`, `TRACEPAD_API_KEY`, `TRACEPAD_ENVIRONMENT`, `TRACEPAD_RELEASE`; the OTel variables are the OTel SDK's business. REST goes over the global `fetch` | Same rules, same names, so that a team with a Python service and a Node service configures both from one `.env`. `console.warn` because Node has no standard logger and pulling one in would be the fourth dependency; the override is for the applications that do have one. |
| 9 | **2026-09-14** — **`update({...})`** and **`updateTrace({...})`** act on the active span (spec 017 #11), warn and do nothing outside one. `Observation` carries `traceId`, `spanId`, `update` and `span` (the OTel span); `Generation` adds `end`, `firstToken`, `stream` | Same contract; `camelCase` because the language's own conventions win over a cross-language rename that would make the package read like a port. |
| 10 | **2026-09-14** — **The harness** (PR B) is spec 018 with promises: `dataset(name)` returns a `Dataset` that has made no request; `putItems`, `items(version?)` (an async iterable over the pages), `create`, `delete(confirm)`, `run(name, {metadata?, id?, datasetVersion?})`, `runs()` are async. **`run.item(case, async (attempt) => …)`** runs the callback inside `context.with` carrying `(runId, itemId)`; a `SpanProcessor` registered by `init` stamps `tracepad.run_id` and `tracepad.item_id` in `onStart` on every span whose context carries them; the `Attempt` records every root span it saw, `traces`, `traceId`, `score(name, value, fields)`. **`run.finish()`** flushes scores, then the provider, then posts; **`run.fail(error)`** likewise; a `Run` is **`await using`-compatible** (`Symbol.asyncDispose` → `finish`) and also has `run.wrap(async () => …)` for runtimes without `using`. `scoreConfigs`, `compare`, `itemId` (`sha256(key)` hex, first 32 characters) as in spec 018 #6–#7. The read side is the server's JSON as plain objects (spec 018 #8) | The context manager becomes a callback (Decision 5's reason) and the processor is the same stamping the Python one does — it is what lets a framework's own root span carry the run link. `asyncDispose` is offered because TypeScript 5.2 compiles it and Node 20+ runs it with a polyfill the compiler injects, and `wrap` is there because not every setup has that yet. |
| 11 | **2026-09-14** — **Release and CI.** Version in `sdk/js/package.json`; tag `sdk-js/vX.Y.Z` triggers `release-sdk-js.yml` — the tag and `package.json` must agree, the suite runs, `npm publish --provenance --access public` with **npm trusted publishing** (OIDC, no token in the repository), as `REPOS.md` §2 planned. The `sdk-js` CI job runs the suite on Node 20 and 22 and the e2e against a real binary through `scripts/sdk-js-test.sh` (the twin of `sdk-test.sh`), plus `make sdk-js-lines` against a **1,800**-line application budget (`scripts/sdk-js-lines.sh`). `make smoke` gains the package as a fourth pinned exporter | Spec 017 #12's shape. The budget is higher than Python's 1,600 because TypeScript spends lines on types that Python spends on nothing; it is a warning, never a failure, like the others. Two Node lines for the reason there are two Pythons: the floor and where the OTel SDK moves first. |
| 12 | **2026-09-14** — **The golden fixture** is `testdata/otlp/011-tracepad-sdk-js.pb`, written by `scripts/fixtures/tracepad_sdk_js.mjs` from the package's own exporter (an in-memory OTLP collector in the script, the same shape `tracepad_sdk.py` uses), numbered after the Python one, and exercised through the OTLP path end to end in the server's suite like 010 is | The fixture is what proves Decision 3: the mapper reads the package's real output, and a change in either side fails a test rather than a user. |
| 13 | **2026-09-14** — Amends Decision 12: the fixture is **`testdata/otlp/013-tracepad-sdk-js.pb`**, its golden `013-tracepad-sdk-js.json`, and the Go package's (spec 033 #12) is 014. The clocks are pinned at generation rather than rewritten afterwards: while the fixture's application runs, `Date.now` stands still and `performance.now` — the two readings the OTel JS span takes — moves a fixed step per reading, so every instant in the export is a rank on a 10 ms grid and the bytes are the SDK's own, untouched. The user and the session are the Python fixture's (`user-9001`, `session-91`) | 011 and 012 were taken by spec 015 #12's plain-text prompt and spec 030 #4's Claude Code session before this spec was written; the next free numbers are 013 and 014 (coordinator decision 2026-09-14). Rewriting the bytes needs a protobuf decoder, which the JS exporter no longer ships (`@opentelemetry/otlp-transformer` hand-writes its serializer since 0.200), and a decoder of our own in a fixture script would be the builder the fixture exists not to be; pinning the clocks the SDK reads is the same editing done one step earlier. The corpus is seeded whole into the interface's end-to-end suite, which counts sessions: a session of the Python fixture's user is two services of one application rather than a sixth session and a second count to move. The one number that does move is the trace total on the Stats screen, 17 → 19. |
| 14 | **2026-09-14** — **`@opentelemetry/resources` is a fourth runtime dependency**, beside the three of Decision 1 and at the SDK's version line (`^2.0.0`) | Decision 2 puts `service.name`, `deployment.environment.name` and `service.version` on the resource of the provider `init` builds, and in the JS SDK a resource is built by `resourceFromAttributes` / `detectResources` in a package of its own — `sdk-trace-node` depends on it but does not re-export it, and importing a transitive dependency undeclared is a broken install under any package manager that isolates (`pnpm`, `npm --install-strategy=nested`). Declaring the package we import is the honest count; it is still the OTel SDK, split the way the JS SDK is split. |
| 15 | **2026-09-14** — The unit suite runs on **vitest 3**, not 4 | vitest 4's optional peer dependencies crash npm 10.9's resolver (`Cannot read properties of null (reading 'edgesOut')`, the npm Node 22 ships), so the lockfile could only be written with `--legacy-peer-deps` — and a lockfile that needs a flag breaks the first contributor's `npm install` (coordinator decision 2026-09-14). |
| 16 | **2026-09-14** — The package's application-line budget rises from 1,800 to **1,900** | The raise rule of spec 015 #9, as spec 031 #22 applied it: PR A after three review rounds is 1,450 lines, the harness of Decision 10 is 374 more (1,824), and a review cycle of PR B needs room. The lines are Decision 10's shape — a callback-scoped block, `wrap` beside `asyncDispose`, the paging loop, the two closes — not drift in what was there; the budget is a signal to revise (spec 017 #1), and what it would ask to revise here is the feature. PR B reports `make sdk-js-lines` before and after. |
| 17 | **2026-09-26** — **The key and the path**, the Node side of spec 017 #19. (a) **Redirects** of a `GET` or `HEAD` keep `fetch`'s own rule — followed with the key within the store's origin, without `Authorization` to another one and for the rest of the chain — which the suite pins against two local servers. **A write is never re-sent** (`redirect: 'manual'` for any other method): a 3xx rejects with `TracepadHTTPError` naming the `Location`. (b) **The key is a non-enumerable property** of the configuration: `config.key` reads it, and `console.log`, `util.inspect`, `JSON.stringify` and a spread do not list it; `init`'s merge after `spanProcessor()` names it for that reason. The exporter is given its headers as a function, so the key is not a value on it either. (c) **The run id the server issued and both ids of `compare` are encoded** with `encodeURIComponent`, as every name already was. (d) **A path with an empty, `.` or `..` segment rejects with `TracepadError` before any request.** (e) The tarball carries **`LICENSE` and `NOTICE`** — `NOTICE` in `files`, `LICENSE` picked up by npm — copies of the repository's own that `make sdk-notices` keeps identical. (f) Budget (spec 036 #8): `main` measured 2,257 application lines, this adds 17, to 2,274, and the budget rises to **2,300**; the review round took it to 2,284 | (b) A plain object is printed whole by anything that prints it, and an error tracker serialises values the same way; a non-enumerable property is the one change that keeps `config.key` as it was. (a) `fetch` turns a `POST` into a `GET` on 301–303, and `GET /api/v1/scores` is a listing: a score batch would have read its `200` as delivered (found in review of PR #89). (d) The URL parser resolves `..` before `fetch` sends anything, so `dataset('..')` addressed `/api/v1/` rather than failing. No name the store accepts is empty or begins with a dot. (e) As spec 017 #19 (e). |
| 18 | **2026-09-26** — **The release is two jobs, and the maps are ESM's only.** (a) `build`, with read access, checks the tag, installs with `npm ci`, runs the unit suite and `npm pack`s; `publish`, the only one with `id-token: write`, downloads the tarball, installs `npm@11.20.0` — one exact version, not `latest` — and runs `npm publish <tarball> --provenance --ignore-scripts` (spec 020 #19). (b) The package ships source maps for its ESM build only: tsup's CommonJS pass re-bundles the split ESM output and its maps name that output by absolute path, the build machine's, so `onSuccess` removes `*.cjs.map` and their `sourceMappingURL` lines. `npm pack --dry-run` lists no `.cjs.map`, and no file in `dist` names a home directory. (c) The gate's `lockfile-lint` allows `sdk-trace-base-v1`, the alias for `@opentelemetry/sdk-trace-base` 1.x that the suite tests against | (a) As spec 017 #20: the publishing job ran `npm ci`, the suite and the build, so every dev dependency could publish. (b) A path from the machine that built a release is nothing a user needs and something a reader of the package should not learn; an ESM map is relative and enough to debug with. |
| 19 | **2026-09-27** — **Node 22 and newer, and TypeScript 7 beside 6.** Amends #1 and #11. `engines.node` is `>=22` and tsup's target `node22`: Node 20 left maintenance in April 2026. The `sdk-js` job runs on **22, 24 and 26** — the lines that still get releases. `@types/node` stays on major 22, the oldest Node supported, so that the package cannot type-check a call 22 does not have; Dependabot does not propose another major. TypeScript is two packages: `@typescript/native` is `tsc` 7 and type-checks the package (`npm run check`, `scripts/sdk-js-test.sh`), called by its own path — `@typescript/old`, the 6.x the alias pulls in, declares a `tsc` too, and which one `node_modules/.bin/tsc` names is npm's choice; `typescript` is the alias to 6.0's API, which tsup's declaration pass and `test/docs.test.ts` import, and the one the interface uses (spec 006 #16). Dependabot proposes no new major of either. tsup's declaration pass always sets `baseUrl`, an option 6.0 deprecates, so that pass alone gets `ignoreDeprecations: "6.0"`; `test/docs.test.ts` gives `paths` absolute instead of a `baseUrl`. The package's tsconfig needed no change. The alias goes when TypeScript 7.1 ships a stable API and tsup builds declarations on it | A package that runs inside other people's applications supports every Node that is still maintained, and tests every one of them. The declarations built on 6.0 are byte for byte the ones 5.9 built, and the bundles differ only in the `package.json` they embed. Measured on Node 24: `tsc --noEmit` 0.95 s on 5.9, 0.18 s on 7; the build the same within noise. |
| 20 | **2026-09-28** — The unit suite moves to **vitest 5** (5.0.2, the newest stable), which lifts #15 | #15 held vitest back because vitest 4's optional peers crashed the resolver of npm 10.9, the npm Node 22 ships. vitest 5 does not: under Node 22.22 / npm 10.9.4 both `npm ci` from the lockfile and a fresh `npm install` from `package.json` alone succeed, and the lockfile is written by that npm, so the first contributor's install needs no flag. vitest 5 requires Node `^22.12`, inside the package's `>=22` for its users because it is a dev dependency only. The suite passes unchanged on 22, 24 and 26 |

## Package contract

```
sdk/js/
  package.json            # name "tracepad", engines.node >= 20, exports (import/require/types), files ["dist"]
  tsconfig.json
  src/
    index.ts              # init, observe, span, event, generation, update, updateTrace, score, prompt, flush, spanProcessor, errors, types
    config.ts             # arguments → environment → TracepadConfigError
    tracing.ts            # the provider adaptation, observe, span/event/generation, Observation, Generation
    generation.ts         # the OpenAI-compatible reader and the stream collector
    attributes.ts         # the vocabulary of Decision 3: one constant per key, one serializer
    scores.ts             # the queue, the timer, the batch
    prompts.ts            # the cache and Prompt
    http.ts               # fetch, auth, TracepadError / TracepadHTTPError
    log.ts                # the warn function and its override
    harness.ts            # PR B: the processor, Run, Attempt, scoreConfigs, compare, itemId
    datasets.ts           # PR B: Dataset, Item
  test/                   # vitest; test/e2e/ runs against a real binary (TRACEPAD_BINARY)
  README.md               # what npm shows: install, init, three lines, a link to docs/sdk-js.md
```

**Public surface** (everything else is private):

| Name | Signature | Notes |
|---|---|---|
| `init` | `(options?: {host?, key?, environment?, release?, export?, logger?}) => void` | Decisions 2, 8. Idempotent. |
| `observe` | `<F>(fn: F, options?: {name?, type?, captureInput?, captureOutput?}) => F` | Decision 4. `type` ∈ the ten kinds `docs/ingest.md` lists. |
| `span` / `event` | `(name, options, fn: (o: Observation) => T \| Promise<T>) => T \| Promise<T>` | Decision 5. `options` may be omitted. |
| `generation` | `(name, options, fn: (g: Generation) => T \| Promise<T>) => T \| Promise<T>` | Decision 5. |
| `update` / `updateTrace` | Decision 9 | |
| `score` | Decision 6 | Synchronous: it enqueues. |
| `prompt` | `(name, options?) => Promise<Prompt>` | Decision 7. |
| `flush` | `(options?: {timeout?: number}) => Promise<void>` | Scores, then the provider. |
| `spanProcessor` | `() => SpanProcessor` | Decision 2: for a provider that takes processors only in its constructor. |
| `TracepadError`, `TracepadHTTPError`, `TracepadConfigError` | Decision 8 | |
| PR B: `dataset`, `Run`, `Attempt`, `Item`, `scoreConfigs`, `compare`, `itemId` | Decision 10 | |

**`Generation.end(response?, fields?)`** reads spec 017 #5's table from
any object with the properties; **`stream(chunks)`** is spec 031 #7/#21.

## Ingest contract

Unchanged: the package writes the `tracepad.*` dialect of spec 017 and the
GenAI conventions, nothing new for the mapper. The fixture of Decision 12
pins it.

## Testing

- Unit (vitest, in-memory span exporter): `init` adopts a 1.x provider by
  `addSpanProcessor`, builds a `NodeTracerProvider` when none is set,
  refuses a 2.x provider without the hook and points at `spanProcessor()`,
  warns on a second call; `observe` on a sync function, an async function,
  a generator and an async generator — input as an array, output, the
  error path with status and event, the name fallback; `span`/`generation`
  callbacks end the span on return, on settlement and on throw; the reader
  over the spec 017 #5 table; `stream` over a hand-written chunk list
  (first token once, deltas joined, usage from the last chunk, cost,
  model, a stream without usage, explicit `end` mid-stream, `break` leaves
  the ending to the callback); `score` enqueues, batches at 100 and at
  2 s, retries once, drops with a warning, throws with no target; `flush`
  drains; `prompt` caches for `max-age`, serves stale on 503, rejects on an
  empty cache, `compile` on text and on messages; `update` outside a span
  warns; `beforeExit` flushes.
- E2E against a real binary (`TRACEPAD_BINARY`, like the Python suite): a
  script with `init`, one `observe`d async function with a `generation`
  inside and a `score` — `GET /api/v1/traces/{id}` shows the tree, the
  model, the usage, the cost and the score; the stream case lands usage,
  output and time to first token.
- PR B: the harness against the real binary — items put and read at a
  version, a run over three items with two roots each, `traces` per
  attempt, scores landing on the right trace, `finish` then `get()`
  showing the summary, `fail` on a throw inside `wrap`, `compare` of two
  runs; unit tests for the processor stamping a framework-started span.
- The fixture: `scripts/fixtures/tracepad_sdk_js.mjs` regenerates
  `013-tracepad-sdk-js.pb` (Decision 13) and the server's OTLP suite reads it back —
  name, type, level, usage, cost, prompt name and version, run and item
  ids.
- Docs: the anchor checker; the `docs/sdk-js.md` examples compile under
  `tsc --noEmit` in the suite (the way the CLI usage lines are parsed).
- CI: `sdk-js` job green on Node 20 and 22; `release-sdk-js.yml` refuses a
  tag that disagrees with `package.json` (tested with a dry run, not a
  publish).

## Out of scope

- A browser build, a `public key` credential or CORS on the server —
  owner decision 2026-09-14.
- Decorator syntax — Decision 4.
- Deno, Bun, edge runtimes as supported targets.
- Any provider-client wrapper — design §6.5.
