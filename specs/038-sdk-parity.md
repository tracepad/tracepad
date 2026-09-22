# Spec 038 — SDK parity: the step's kind, a generation's metadata, the trace's version

**Status:** 🟡 DRAFT
**Sprint:** September 2026

> The first application to trace through the Python package side by side
> with the Langfuse SDK it replaces found the two paths equal in everything
> the store records — the tree, the cost, the time to first token, the
> scores, the statistics — and three places where the package is narrower
> than its siblings or than the bridge. A retriever has to be opened as a
> span and retyped afterwards, because Python's `span()` takes no kind while
> Go's `Span` does. Metadata set at the start of a generation needs a second
> call, because Python's `generation()` takes none while Go's and Node's do.
> And a trace's own version — a column the store has, a filter the listing
> offers — can be written only in the Langfuse dialect: no `tracepad.*` key
> feeds it and no package writes one. This spec closes the three.

---

## Overview

Deliverables, one PR (the last commit flips the status):

- Python: `span(..., type=)`; `generation(..., metadata=)`;
  `update_trace(..., version=)` (Decisions 1–3).
- Node: `span(name, { type })`; `updateTrace({ version })` (Decisions 1, 3).
- Go: `UpdateTrace(ctx, WithTraceVersion(v))` (Decision 3). Go already has
  the other two.
- The mapper: `tracepad.trace.version` in the trace version chain;
  `docs/ingest.md`'s table (Decision 4).
- The three SDK pages' reference tables and examples; `docs/ingest.md`.

Not here: showing the answering model beside the requested one in the
interface, reasoning tokens in the statistics (both intended, spec 002 #25,
spec 031 #1), a trace version in the MCP or CLI output beyond what the API
already returns.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-23** — **`span()` takes the step's kind** in Python (`type=` keyword, default `"span"`) and Node (`type` in the span's options), with the rule `@observe(type=…)` and Go's `WithType` already apply: one of the store's ten kinds, anything else kept in the observation's metadata with a warning. The option belongs to `span` only: `event` and `generation` name their kind by being called, and Node's `GenerationOptions` does not inherit it | Go has had it since spec 033, and Python's own decorator takes it; the context manager is the one door in two packages where a retriever or a tool call has to be opened as a span and corrected by `update(type=…)` — two calls where the concept is one, and a window in which an exporter can flush the wrong kind. The kind is a fact about the step known when it opens. |
| 2 | **2026-09-23** — **Python's `generation()` takes `metadata=`**, as Go's `Generation` (through `WithMetadata`) and Node's `generation` (through `GenerationOptions`) already do; written as `tracepad.observation.metadata` exactly as `span(metadata=)` writes it | A generation's metadata is usually known at its start — which attempt this is, which prompt override is in force — and the package that has it on `span` and not on `generation` is the odd one of three. |
| 3 | **2026-09-23** — **A trace's version**: `update_trace(version=)` (Python), `updateTrace({ version })` (Node), `WithTraceVersion(v)` (Go) write `tracepad.trace.version`, a string. It is the version of *this trace's logic* — a pipeline revision, a prompt bundle, an experiment arm — beside `release`, the deployment's version set once at `init` | The store has carried a trace `version` since spec 002 and the listing filters on it (spec 004), but only `langfuse.version` feeds it, so an application moving from the bridge to the package loses a field it used. Two fields for two scopes is what the bridge had and what a team comparing arms inside one deployment needs; folding the version into `release` works until two arms run in one release. |
| 4 | **2026-09-23** — **The mapper**: `tracepad.trace.version` enters `traceVersionKeys` at the rank of `langfuse.version`, level-agnostic and ranked like the other trace-level keys (spec 017 #3, spec 012 #11); `docs/ingest.md`'s version row gains it; the golden fixtures the packages write are regenerated only where a test sets a version | Spec 017 #3's rule for the dialect, applied to the one trace field it missed: each `tracepad.*` key mirrors the `langfuse.*` key of the same field at the same rank. |
| 5 | **2026-09-23** — **One warning, and `observe` gives it too.** Decision 1 says the rule for an unknown kind is the one `@observe(type=…)` already applies, but in Python and Node only `update(type=…)` warned: the decorator and the wrapper wrote any spelling silently, and only Go's `WithType` checked everywhere. Each package now has one helper that warns and passes the kind through — Python `_kind`, Node `kind` — used by `span`, `update` and `observe`; `observe` warns once, on the function's first call, not on every call (amended in review of PR #77: not at wrap time, which is module load, before `init({ logger })` has said where warnings go) | The rule Decision 1 names is only a rule if it holds at every door; a warning per call of a hot function would be noise, and the spelling cannot change between calls. |
| 6 | **2026-09-23** — **Node: `ObservationOptions`**, `{ input, metadata }`, is split out of `SpanOptions` and exported. `SpanOptions` extends it with `type`, `GenerationOptions` extends it instead of `SpanOptions`, and `event` takes it | Decision 1 keeps `type` off `event` and `generation`, and both took `SpanOptions` or inherited it; adding `type` there would have given it to all three. A caller who passes a `SpanOptions` value to `event` still compiles — the base is a supertype. |
| 7 | **2026-09-23** — **The API's own description** of the `version` filter and field in `openapi.json` names `tracepad.trace.version` beside `langfuse.version`, and the interface's generated types follow | The self-description says where the value comes from; left alone it would have named one of two sources. |
| 8 | **2026-09-23** — **`span` with a kind that has its own shape opens that shape** (found in review of PR #77). `span(type="generation")` returns what `generation()` returns — a `Generation`, with `end`, `first_token` and `stream` — and `span(type="event")` what `event()` returns, zero duration; `input` and `metadata` are carried over. An empty kind (`None`, `""`) is the default, as Go's `WithType("")` is a no-op. In Node the two are not in `SpanOptions.type` (`generation()` and `event()` are the way to them), the runtime still routes them for a caller without types, and a `type` handed to `event` or `generation` warns rather than vanishing | `@observe(type="generation")` already builds a `Generation`: a kind decides the shape everywhere else, and a `span` that accepted `"generation"` but handed out a plain `Observation` failed at the caller's `end(…)`. Go's `Span` keeps its behaviour — it is out of this spec's reach and its `WithType` is an option on every shape. |

## Testing

- Python: `span(type="retriever")` exports `tracepad.observation.type =
  retriever`; an unknown kind warns and lands in metadata, as `@observe`;
  `generation(metadata={…})` exports the metadata; `update_trace(version=…)`
  exports `tracepad.trace.version`.
- Node: the same three through its test harness; `GenerationOptions` has no
  `type` (a type test).
- Go: `WithTraceVersion` exports the key.
- Mapper: a span carrying `tracepad.trace.version` fills the trace's
  `version`; with both keys present the rank is `langfuse.version`'s; the
  listing's `version=` filter finds the trace. One e2e per package that
  sets a version and reads it back from `GET /api/v1/traces/{id}`.
- Line budgets reported as every SDK PR reports them.

## Out of scope

- The answering model in the interface when it differs from the requested
  one (a router alias such as `openrouter/auto`): parked, not decided.
- Reasoning tokens in the statistics: spec 031 #1 stands.
