# Spec 042 — What the packages cost when nothing is recorded, and how long they may wait

**Status:** 🟡 DRAFT
**Sprint:** September 2026

> A web application that moved to the Python package found three places
> where the package costs more than it should or waits longer than it says.
> Every `span()` and `generation()` serialises its input and metadata to
> JSON before the span exists — tens of kilobytes per chat turn — and
> throws the work away when the span does not record: with tracing off, or
> when a sampler drops it. `update()` and `update_trace()` warn "outside a
> span" on every call in the same state, inside a perfectly good `with`
> block. And the exporter is built with OpenTelemetry's default timeout while
> `flush(timeout)` promises a bound the OpenTelemetry batch processor does
> not keep: in 1.44 its `force_flush` exports synchronously and ignores the
> deadline it is given, so a request that flushes in a `finally` holds its
> thread for as long as a dead Tracepad takes to time out, retries included.
> This spec makes the three honest in all three packages.

---

## Overview

Deliverables, one PR (the last commit flips the status):

- Attributes that cost something are built only for a span that records
  (Decision 1).
- `update` / `update_trace` silent where nothing can be written by design
  (Decision 2).
- `init(export_timeout=)` and its Node and Go counterparts, with a default
  of five seconds (Decision 3).
- `flush(timeout)` that returns within its timeout (Decision 4).
- Observation metadata that merges by key on `update` (Decision 5).
- The three SDK pages: *init*, *flush*, *What raises and what does not*.

Builds on spec 039 as merged (its Decision 7: tracing is off when `init`
never ran and the global provider is OpenTelemetry's no-op one).

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-23** — **Two tiers of attributes.** The span is started with the **cheap** ones only — the kind, the model, the prompt reference, the trace-level keys a call sets — and the **costly** ones — input, output, metadata, model parameters, messages, anything the package serialises to JSON — are set **after** the span exists and **only if `is_recording()`**. The same in `update`, `end` and the generation readers: nothing is serialised for a span that does not record | The waste is the JSON, not the span; the cheap attributes are what a sampler or a span processor may want to see at start, and they stay there. `is_recording()` is OpenTelemetry's own answer to "will this be kept", and it covers every case at once: tracing off, an application's sampler, a span processor that drops. The cost of the check is one call per span. |
| 2 | **2026-09-23** — **`update` and `update_trace` warn only when they are a mistake**: in an initialised process with **no span in context at all** (the call is outside every block). When a span is in context but does not record — tracing off, or sampled out — the call is a **no-op with a debug line**, like spec 039's `score` | Spec 039 settled that tracing-off is a configuration, not an error, and a sampled-out span is the sampler doing its job. A warning on every call in either state teaches an operator to ignore the package's warnings, which is worse than no warning. The real mistake — updating from outside every block in a process that traces — keeps its warning. |
| 3 | **2026-09-23** — **`export_timeout`**: `init(export_timeout=5.0)` in Python (seconds), `init({ exportTimeoutMillis: 5000 })` in Node, `WithExportTimeout(5 * time.Second)` in Go, and `TRACEPAD_EXPORT_TIMEOUT` (seconds) in all three when the argument is absent; the value is handed to the OTLP exporter as its per-export timeout. Ignored, with a warning, when `init` adopts an application's own provider and adds no exporter of its own (`export=False`) | OpenTelemetry's default is ten seconds per export and the OTLP exporters retry within it, which is a reasonable default for a batch job and a long one for a request thread. Five seconds is still generous for one POST to a server on the same network; the knob is there for the ones that are not. The environment variable is the one a deployment can set without a code change, named in the package's own vocabulary; the OTel variable (`OTEL_EXPORTER_OTLP_TRACES_TIMEOUT`) keeps working underneath when neither is given. |
| 4 | **2026-09-23** — **`flush(timeout)` returns within `timeout`**. Python runs the provider's `force_flush` on a helper thread and waits for it at most the budget left after the scores; when the budget runs out it logs one warning and returns, and the export finishes in the background. Node races `forceFlush()` against a timer the same way. Go's `ForceFlush` already honours its context, and the Go `Flush` derives one from the timeout it is given — verified by a test with a server that never answers. In all three, a test proves the bound against an exporter that hangs | The docstring's promise is the right promise — a caller that gives a flush five seconds has a reason — and the OpenTelemetry Python batch processor in 1.44 does not keep it (`force_flush` calls `_export(EXPORT_ALL)` synchronously and never reads `timeout_millis`). A helper thread is the one way to bound a call the package does not own; the export it leaves running is the same export the batch processor would have run on its own schedule. |
| 5 | **2026-09-23** — **Observation metadata merges by key.** The packages write metadata as **one attribute per top-level key**, `tracepad.observation.metadata.<key>` (the value JSON-encoded when it is not a string, number or boolean), instead of one serialised object under `tracepad.observation.metadata`. So `update(metadata={"flag": 1})` adds or replaces `flag` and leaves the keys the span already carried; passing a key with `None` / `undefined` / a nil value writes nothing for it (it does not delete). `span(metadata=)`, `generation(metadata=)`, `update(metadata=)` alike, in all three packages; trace metadata keeps the merge `update_trace` already has. The mapper needs nothing: it already reads `tracepad.observation.metadata.*` per key and merges it (`docs/ingest.md`) | Found by the first application ported from the Langfuse SDK, whose OpenTelemetry exporter writes `langfuse.observation.metadata.<key>` one attribute per key, so adding a flag to a span that already carries identifiers keeps them. Ours wrote one attribute, which OpenTelemetry replaces whole on a second write — a silent loss of exactly the identifiers a reader filters by, in code that looked correct to its author. Per-key attributes are the merge OpenTelemetry already has, with no read-modify-write in the package; they also fit Decision 1, since each key is serialised on its own and only for a recording span. Deletion by `None` is left out on purpose: an attribute cannot be unset once written, and pretending otherwise would be a promise the wire cannot keep. |

## Testing

Per package:

- A capture (spec 040 when merged; the package's own fixture until then)
  with a sampler that drops every span: `span(input=<object whose
  serialisation is counted>)` serialises nothing; with tracing on it
  serialises once.
- Tracing off: `update()` / `update_trace()` inside a block log at debug
  and write nothing; in an initialised process outside every block they
  still warn once.
- `export_timeout` reaches the exporter (the exporter's configured timeout
  read back, or a server that answers after the timeout and a failed
  export observed); the environment variable, and the argument winning over
  it.
- `flush(0.2)` against an exporter that blocks for five seconds returns
  within the budget plus a small margin and logs the warning.
- `span(metadata={"a": 1})` then `update(metadata={"b": 2})`: the stored
  observation carries both keys (server e2e); `update(metadata={"a": 3})`
  replaces `a` only.

## Out of scope

- Changing OpenTelemetry's batch schedule or queue sizes; the defaults stay.
- An asynchronous `flush` in Python.
