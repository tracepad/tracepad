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

## Out of scope

- Changing OpenTelemetry's batch schedule or queue sizes; the defaults stay.
- An asynchronous `flush` in Python.
