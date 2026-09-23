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
| 6 | **2026-09-23** — **Budgets, by measurement** (spec 036 #8): main measured 1,886 / 2,166 / 2,332 application lines (Python / Node / Go); this spec adds 59 / 42 / 95, to 1,945 / 2,208 / 2,427, and the budgets rise to **1,975 / 2,250 / 2,450** — round numbers with room for a review round | Go's share is the largest for two reasons Decision 7 and Decision 9 give: the export timeout is set in two places, and metadata of any type — a struct, a typed map — is split by key through reflection or a JSON round trip, where Python and Node are handed a dictionary. |
| 7 | **2026-09-23** — **In Go the export timeout is also the batch processor's.** `otlptracehttp.WithTimeout` is the HTTP client's timeout, the bound of one attempt, and a timed-out attempt is a retryable error that the exporter retries with backoff for up to a minute (1.46); so `WithExportTimeout` sets both the exporter's timeout and `sdktrace.WithExportTimeout` on the batch processor, whose context bounds the whole export, retries included. `OTEL_EXPORTER_OTLP_TRACES_TIMEOUT` keeps OpenTelemetry Go's own meaning — one attempt — and its test is the resolution itself (`exportTimeout`), since a flush cannot observe a bound the retries outlive | Decision 3 promises a bound on one export, retries included, and Python's and Node's exporters keep it with their one timeout; Go's does not. Found by the test against a store that never answers, which took the flush's whole ten seconds before the batch processor was bounded too. |
| 8 | **2026-09-23** — **Go's `Flush` returns the error rather than logging it.** Against an export that hangs, `Flush(ctx)` returns within the context's deadline with `context.DeadlineExceeded`, and the export goes on under its own timeout; no warning is logged. Node's `flush` already raced `forceFlush()` against a timer, warning when it lost, so Decision 4 there is a test | Testing's "logs the warning" is the shape of a call that returns nothing; Go's `Flush` returns an error (spec 033), and the caller that set the deadline holds it. A log line as well would say the same thing twice. |
| 9 | **2026-09-23** — **Details the Decisions above leave open.** (a) A `TRACEPAD_EXPORT_TIMEOUT` that is not a positive, finite number of seconds is ignored with a warning, not raised. (b) The warning under `export=False` is for the argument only: the variable is deployment-wide and says nothing about one `init`. (c) Node's `spanProcessor()` takes `exportTimeoutMillis` too: it is where a 2.x provider's exporter is built. (d) In Node `null` is a key without a value as `undefined` is. (e) Metadata that is no mapping — a string, a list — is written whole under `tracepad.observation.metadata`, as before, rather than refused; in Go "a mapping" is a map with string keys or anything that encodes as a JSON object. (f) Decision 2's warning is one per call, as it was | (a) A knob of the exporter must not stop an application from starting; the environment is the channel a typo reaches without review. (e) The tracing path never raises (spec 017 #4), and the mapper reads a JSON object at the bare key as before. |
| 10 | **2026-09-23** — **"A sampler that drops every span" is a caller that chose not to sample.** `capture()` takes no sampler (spec 040), so the tests start the steps under an unsampled remote parent, which the default `ParentBased` sampler drops, and repeat the count with tracing off; that the cheap attributes are still what a span starts with is its own test, read by a span processor's `on_start` (Python, Node) and by a sampler's `ShouldSample` (Go) | The case is real — an upstream service decided — and it is the same sampler an application gets by default, in all three packages, with no private hook into the capture. |
| 11 | **2026-09-23** — **Found in review of PR #83.** (a) Python's `flush` keeps one helper thread per provider: a flush that finds the last one still exporting waits for it instead of starting another, so a store that is away costs one thread, not one per request. (b) An `export_timeout` argument that is not a positive number of seconds is ignored with a warning, as the variable is (Decision 9a); in Node an option or a variable past 2^31 − 1 ms, what a timer holds, too, and nothing reaches the exporter that would make it throw out of `init`; in Go a negative `WithExportTimeout`. (c) A metadata entry is written the way a model parameter is (`scalar`), with two corrections to that rule: a Python `int` outside 64 bits is JSON, since protobuf refuses it at export along with its whole batch, and a value that encodes as a JSON string — a `Date`, a `time.Time`, a `UUID` through `repr` — is that string without its quotes. (d) Node writes metadata that is no object whole, as Decision 9(e) says; Go decodes a struct's numbers with `UseNumber` and writes nothing for a typed nil. (e) **Kept, and documented:** a string entry whose text is a JSON object or array is stored as that structure — the mapper reads every per-key metadata entry that way (`looseJSON`), the Langfuse exporter's per-key form included. (f) **Not done here:** the golden fixtures `testdata/otlp/010`, `013`, `014` keep the whole-object metadata they were written with; the server's fixture test asserts no metadata, and each package's e2e test reads the per-key form back through the real binary. (g) The round measured 1,969 / 2,227 / 2,462 application lines; the Go budget rises to **2,475**, the other two hold | (a) to (d) are defects in this spec's own code. (e) is the one ambiguity the per-key form has on the wire: telling a string that looks like JSON from JSON would need a type marker the mapper does not read, which Decision 5 set out not to add; an application that needs the literal string wraps it in an object. (f) Regenerating the fixtures would change three binary files for a test that does not read what changed. |

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
