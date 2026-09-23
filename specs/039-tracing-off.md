# Spec 039 — The packages with tracing off

**Status:** 🟡 DRAFT
**Sprint:** September 2026

> An application traces in production and not in its tests, or not on a
> developer's machine that has no key: it simply does not call `init`. The
> packages promise that instrumentation never breaks the function it
> observes, and in that process they break it twice. A `score()` inside a
> `with tracepad.span(...)` block raises, because OpenTelemetry's no-op
> tracer hands out the invalid span context and the package cannot tell
> "tracing is off" from "you scored outside every span". And an
> observation's `trace_id` is thirty-two zeros — OpenTelemetry's word for
> *no trace* leaking out as if it were an id, which an application then
> stores on its own rows. The first downstream application to move to the
> packages found both on the day it tried. This spec makes tracing-off a
> state the packages know and answer honestly in, in all three.

---

## Overview

Deliverables, one PR (the last commit flips the status):

- `score` with no explicit target in a process that never initialised the
  package: a no-op with a debug log, in Python, Node and Go (Decisions
  1–2).
- An observation's trace and span id with no real trace behind them: `None`
  (Python), `undefined` (Node), `""` (Go) (Decision 3).
- The three SDK pages: *What raises and what does not* (and its Node and Go
  counterparts) and the observation reference (Decision 4).

Not here: a switch to turn tracing off in an initialised process, sampling,
anything on the server.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-23** — **`score` with no target — no `trace_id` given — and no valid span context** is a **no-op with one debug-level log line** when the package has **never been initialised** in this process (`init` not called: Python `_initialized`, Node's and Go's equivalent). In an **initialised** process the same call keeps raising (`ValueError` in Python, a thrown `Error` in Node, `ErrNoTrace` returned in Go): there, no span means the caller scored outside every span, which is the programming error spec 017 #7 made the one exception to "the tracing path never raises" | The exception was written for a mistake the author can fix at the call site, and it fires as well for a configuration the author chose on purpose — tests, a keyless development machine — where the call site is correct and nothing can be fixed there. The package can tell the two apart by the one fact it owns, whether `init` ran; the span context cannot, because the no-op tracer's spans are invalid inside a block exactly as outside one. Initialised-and-outside keeps the raise so the mistake it was for is still loud. |
| 2 | **2026-09-23** — A `score` **with an explicit `trace_id`** is unchanged in either state: it is enqueued and posted with the configuration the environment gives, as a script that scores stored traces without tracing anything does today | Scoring by id is REST, not tracing (spec 017 #7, the judge script of `docs/scores.md`): it needs a host and a key, not a provider, and turning it off with tracing would break the offline judge to fix the application. |
| 3 | **2026-09-23** — **An observation's trace and span id** read as **`None` / `undefined` / `""`** when its span context is invalid — which is every span of a process with no provider, and only those. Python's `Observation.trace_id` and `span_id` become `str \| None`, Node's `traceId` / `spanId` `string \| undefined`; Go's `TraceID()` / `SpanID()` keep `string` and return `""`. This is the contract the harness's `Attempt.trace_id` already has in all three (`None` / `undefined` / `""` before any trace), now applied to the observation | Thirty-two zeros is a value that looks like an id and is not one: stored on an application's row it points at nothing, joins to nothing, and passes every "is it set" check. The absent value is the honest answer, the one each language uses for "no value", and the one the same packages already give for the same question on an attempt. The type change in Python and Node is the point — a checker now asks the caller what to do with no trace. Go returns `""` because a string cannot be nil, as `Attempt.TraceID` does. |
| 4 | **2026-09-23** — **Docs**: each SDK page's table of what raises gains the row *`score` with no target, never initialised* → *nothing; a debug line*, and the row for no target narrows to *initialised, outside every span*; the observation reference states the absent id; the Python and Node pages add one sentence under *init* — "not calling `init` is how tracing is turned off: spans are no-ops, ids are absent, scores without a target are dropped" | The pattern is common enough to be a sentence in the docs rather than a discovery; the tables are where a reader looks for what can raise. |
| 5 | **2026-09-23** — **Where the debug line goes.** Python logs it at `DEBUG` on the `tracepad` logger and Go at `slog.LevelDebug` on the package's logger — `slog.Default()` in a process that never called `Init`, since `WithLogger` is an `Init` option. Node's `Logger` had `warn` alone, so it gains an optional `debug(message)`: the default `console` has one, and a logger given to `init({ logger })` without it hears nothing, which is what a debug line is for. "Initialised" in Node is `init` or `spanProcessor`, the two calls that configure the package; Go's is the `initialized()` the harness already read | Decision 1 names a debug line in all three and Node had no level to put it at; an optional method keeps every logger that satisfied the interface satisfying it. |
| 6 | **2026-09-23** — **A span that does not record, in a process never initialised, is no trace** (found in review of PR #78). With tracing off, OTel's no-op tracer does not only hand out the invalid context: under a propagated parent — a request that arrived with a `traceparent`, carried by a propagator or a context manager the application set up — it hands every child the *caller's* context, valid and not this step's. So the test in Decisions 1 and 3 is "never initialised and the current span does not record", not "the context is invalid": `score` without a target drops there too, and an observation's ids are absent when its span was not recording when it opened and the package was not initialised (the handle remembers it, since a span stops recording when it ends). An application's own provider without `init` — the first edge case — records, so its ids are real and its scores enqueued, as before. In Node the default logger is `console.warn` alone: `console.debug` is stdout, and Decision 5's line is not for a CLI's output | The rule "no trace here" has to hold for every span the no-op tracer makes, and an echoed parent is one of them: its ids join to a trace this process never wrote, and a score against it is posted with no store configured. Python's `DEBUG` and Go's `slog.LevelDebug` are filtered out by default; Node's console has no level, so the default leaves debug out rather than printing it. |
| 7 | **2026-09-23** — **Tracing off is "never initialised and no provider of the application's own"**, read from the global provider — the API's no-op or proxy with no delegate — and not from whether a span records (second review of PR #78; amends Decisions 1, 3 and 6). Decision 6's recording test stood in for "no provider" and caught three things it should not: a score after the span ended, under the application's own provider, was dropped though its trace was real; a sampler's non-recording spans lost their real ids; and a process tracing through its own provider without `init`, scoring outside every span, lost the raise spec 017 #7 made loud. With the provider as the test, all three stand as they were, and the no-op tracer's echo of a propagated parent is still no trace, because no provider is the only way to get it. An observation still decides at open. In Go an empty `WithTraceID` or `WithObservationID` — an `Observation`'s id with tracing off — is no target given, as `None` and `undefined` are in the other two | "Did `init` run" is the one fact the package owns, but it is not the question: a process can trace without it. The global provider answers "is anything tracing here" directly, and each package already asks it — `init` adapts to the provider it finds (spec 017 #2), through `isDefault`, `delegateOf` and the API's own provider classes. |
| 8 | **2026-09-23** — **Third review of PR #78; amends Decisions 5–7.** (a) A `score` without a target drops only where **nothing traces**: no `init`, a no-op global provider, *and* no recording span in the context. An echoed parent never records; a live span of a provider the application wired into its framework without registering it globally — `otelhttp.WithTracerProvider(tp)`, `instrument_app(app, tracer_provider=tp)` — does, and scores against its trace as it did before this spec. An observation's ids keep Decision 7's rule, since under a no-op global the package's own span is always the no-op's. (b) Node's line goes to OpenTelemetry's `diag` logger at debug, and the optional `Logger.debug` of Decision 5 is gone: the package's logger is set only by `init`, and `init` turns tracing on, so no application could ever have heard it; `diag` can be enabled without `init`, and is silent by default, as Python's `DEBUG` and Go's `slog.LevelDebug` are. (c) The providers whose tracers are all no-ops count as none: Python's `OTEL_SDK_DISABLED=true`, Go's deprecated `trace.NewNoopTracerProvider()`. (d) Go's empty `WithTraceID` is no target only where nothing traces; while tracing it is `ErrNoTrace` — an id stored while tracing was off must not quietly score the context's trace instead | Each amendment closes a case the previous rule got wrong in the other direction; the rule now reads the three facts that together say "nothing traces here", and each package asks them with the API it already has. |

## Testing

Per package, in a process (or a test with the package reset) that never
initialised it:

- `score("x", 1)` inside `span(...)` and outside any span: no exception
  (Go: `nil`), nothing enqueued, one debug log line.
- `score("x", 1, trace_id=…)` (Go: `WithTraceID`): enqueued as today.
- `span(...).trace_id` / `.span_id`: `None` / `undefined` / `""`.

In an initialised process: `score` outside every span still raises (Go:
`ErrNoTrace`), inside a span it scores the span's trace; `trace_id` is the
32-hex id. The existing suites pass unchanged except where they asserted the
zero id or the old raise.

## Edge cases

- An application that set its own global `TracerProvider` and never called
  `init`: its spans are valid, so ids are real and a `score` inside a span
  enqueues against the trace — the package posts it if the environment
  configures a host and key, and logs the failure (never raises) if not,
  as the score queue already does.
- `init` called, then the provider shut down: the process is initialised;
  Decision 1's raise applies outside a valid span.
