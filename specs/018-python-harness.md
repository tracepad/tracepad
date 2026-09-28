# Spec 018 — The eval harness in Python: datasets, runs and the item context

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> Spec 014 made an eval a loop any script can run with `curl`: declare the
> score names, push the cases, open a run, fetch the items at the version
> it pinned, stamp every trace with two attributes, post the scores, close
> the run. `docs/datasets.md` prints that loop in forty lines of shell. This
> spec gives it to Python over spec 017's package in ten: a dataset object,
> a run that opens and closes itself, and a `with run.item(case):` block
> inside which every span the application starts — its own, a framework's,
> another SDK's — carries the run and the item without the harness touching
> a span.

---

## Overview

Deliverable, in one PR (which flips the status): the harness half of the
`tracepad` package.

- **`tracepad.dataset(name)`** → `Dataset` with `put_items`, `items(version)`
  and `run(...)` (Decisions 1, 2).
- **`Run`** — a context manager that finishes or fails on exit, with
  `item(case)` giving the block of Decision 3 and `finish` flushing before
  it closes (Decision 5).
- **The stamping processor** — a `SpanProcessor` installed by `init`
  (spec 017 #2, with or without an exporter) that writes `tracepad.run_id`
  and `tracepad.item_id` on every span started inside an item block, and
  records the trace ids it saw on the block's `Attempt` (Decisions 3, 4).
- **`tracepad.score_configs([...])`** — declarative, synchronous, loud
  (Decision 6); **`tracepad.item_id(key)`** for the natural-key idempotency
  spec 014 #9 offers (Decision 7).
- **The read side, thin**: `Run.summary()`, `Run.items()`,
  `tracepad.compare(a, b)` returning the server's JSON as `dict`s
  (Decision 8).
- `docs/datasets.md` gains *The same loop from Python*; `docs/sdk-python.md`
  its harness section; the status flips.

Not here: running anything (spec 014 #1 — the store executes nothing, and
neither does the package), a judge, a CLI entry point, parallelism helpers
beyond what `contextvars` gives.

---

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-04** — `tracepad.dataset(name)` returns a `Dataset` that has made **no request**: `put_items(items)` posts the batch whole (spec 014's `POST …/items`, ids optional) and returns `(version, changed)`; `items(version=None)` is a generator over the pages of `GET …/items?version=V&limit=500`, following `next_cursor` until it is `null`, yielding `Item(id, input, expected_output, metadata, dataset_version, source_trace_id, source_observation_id)`; `create(description=…)` and `delete()` are plain calls. Everything is synchronous over spec 017's `_http` and raises `TracepadError` (spec 017 #9) | The harness is a script, and a script wants a value or an exception. The pagination loop is where `docs/datasets.md` warns a hand-written harness goes wrong — "a pass that silently stopped at the first page would be recorded as a whole run over a fraction of the cases" — so the generator owns it and the caller cannot get it wrong. A `Dataset` that fetched on construction would make `tracepad.dataset("x")` a network call in a place a person expects a name. |
| 2 | **2026-09-04** — `Dataset.run(name, *, metadata=None, id=None, dataset_version=None)` posts spec 014's `POST …/runs` and returns a `Run` with `id` and `dataset_version` — the number the harness must fetch by (spec 014 #7). `Run` is a **context manager**: `__exit__` calls `finish()` on a clean exit and `fail(error=repr(exc))` on an exception, then re-raises. A `Run` may also be `finish`ed or `fail`ed by hand and then exited without a second call | The version the harness fetched and the version it ran must be one number by construction, and handing it out on the object the harness holds for the whole loop is how; `run.dataset.items(version=run.dataset_version)` is the line the docs show. The context manager exists because a run left `running` is reported as such forever (spec 014 #8), and a harness that crashed between the last item and `finish` is exactly the harness that forgot to write the `except`. |
| 3 | **2026-09-04** — `run.item(case)` is a **context manager that sets a `contextvars.ContextVar`** to `(run_id, item_id)` and yields an `Attempt`; a **`SpanProcessor`** registered by `init` writes `tracepad.run_id` and `tracepad.item_id` in `on_start` on every span whose context carries the variable, whoever started it. The block **opens no span of its own**. Blocks nest by replacement (an inner item wins for its duration) and restore on exit (owner decision 2026-09-04: context + processor) | The code under test is the application, and the application's root span is the framework's, the Langfuse SDK's or a decorator's — never the harness's. An attribute the processor writes at `on_start` lands on all of them, the way the OTel `Resource` does but scoped to the block; a root span the harness opened instead would make every eval trace look like a trace of the harness, and would not survive an `await` boundary into a task the framework spawned differently. `contextvars` is what `asyncio` tasks inherit and what `ThreadPoolExecutor` copies when asked (`contextvars.copy_context`), so the same block covers a sync call, an `await`, and a thread the harness started with the context copied — and the docs say which is which. What the block cannot reach is a span in another process (a service the harness called over HTTP): the two attributes do not propagate with the trace context, by design (spec 014 #2), and `Attempt.attributes()` is the `dict` a harness passes to such a service by its own means. |
| 4 | **2026-09-04** — The `Attempt` records **every root span** the processor saw start inside the block — `traces` is the list of their trace ids in order, `trace_id` the last — and `Attempt.score(name, value, **kw)` posts through spec 017's `score` against `trace_id` (a `ValueError` when there is none yet). Scores keep the queue's asynchrony (spec 017 #6); `Run.finish` flushes it | A harness scores after the application returned, outside any span, and the one thing it knows is "the trace that just happened" — which the processor watched start. Keeping every root rather than the last is spec 014 #2's shape: repeating a non-deterministic case N times inside one block is N traces of one item, and the summary counts them all; a retry that opened a new trace is the same. The last is the default because it is the one the harness would quote, as the compare peek does (spec 016 #17). |
| 5 | **2026-09-04** — `Run.finish()` **flushes before it posts**: the score queue, then the provider's `force_flush`, then `POST /runs/{id}/finish`; `fail(error)` the same with `{"status": "failed", "error": …}`. The flush's timeout is the `finish(timeout=30.0)` argument; a flush that timed out is logged, and `finish` is still posted | The summary a harness reads right after `finish` — the CI step that prints the numbers — must be over every trace and score the run produced, and the exporter batches on a timer. Ordering the flush before the close is what makes `run.summary()` on the next line complete; a late span still links (spec 014 #8), so a timed-out flush degrades to "the number was read early", not to a lost trace. |
| 6 | **2026-09-04** — `tracepad.score_configs([{...}, ...])` `PUT`s each config (spec 014 #17), synchronously and in order, and raises on the first failure with the config's name in the message. A config may be a `dict` in the API's shape or a `ScoreConfig(name, data_type, direction=None, min=None, max=None, categories=None, description=None)` | Declaring the configs is step 1 of the recipe and belongs at the top of the harness, where a mistake — a `numeric` without a `direction` — should stop the job before it runs a single case, not fail every score batch quietly in the queue. `PUT` is idempotent (spec 014 #17), so the call is safe on every CI run. |
| 7 | **2026-09-04** — `tracepad.item_id(key: str) -> str` is `sha256(key)[:32]`, the derivation `docs/scores.md` shows for scores, offered for items and runs alike so that a harness with a natural key — a case file's path, a commit hash — stays idempotent across runs (spec 014 #9) | The recipe's retry-safety rests on the client supplying ids, and every harness that does will write this one line; writing it once with the same rule as the documented score-id derivation means two harnesses hashing the same key agree, which is what spec 014 #24's 409 wants to be rare and deliberate. |
| 8 | **2026-09-04** — The read side is **`dict`s of the server's JSON**, not models: `Run.get()` (the run with its summary), `Run.items(unknown=False)` (a generator over `GET /runs/{id}/items`), `Dataset.runs()` (a generator), `tracepad.compare(a, b)` (the comparison whole). No number is computed in the package | Every number a comparison reports is computed server-side so that two clients cannot disagree (spec 014 #18, spec 016 #14), and a model layer would be a place to start disagreeing; a `dict` is the JSON `docs/datasets.md` already documents, field for field. The harness needs these for one `print` at the end of a CI job. |
| 9 | **2026-09-04** — The item block **outside a `Run`** does not exist: `Run.item` is the only constructor, and an `Attempt` cannot outlive its block for stamping purposes (a span started after `__exit__` is not stamped, whatever the harness still holds) | Spec 014 #28's rule — an item is a position inside a run — has one honest shape in an API: the item belongs to the run object. An `Attempt` kept after the block is a record (its `traces`, its `trace_id`), not a context. |
| 10 | **2026-09-04** — The attempt travels in the **OpenTelemetry `Context`** (`context.set_value` / `attach`), not in a `ContextVar` of the package's own. OTel's runtime context *is* one `contextvars.ContextVar`, so this is the mechanism Decision 3 describes with the property Decision 3 asks of it — the processor reads it out of the `parent_context` it is handed, rather than out of an ambient variable that a span started with an explicit `context=` would not see | The two halves of Decision 3 cannot both be literal: a bare `ContextVar` is not readable *from* a `Context` object, which is what `on_start` receives. Taking OTel's own carrier makes them one thing — it is inherited by an `asyncio` task, copied by `contextvars.copy_context()`, and invisible to a bare thread exactly as the decision's table promises, because it is the same variable the current span rides in. It also means a harness that passes a context explicitly gets the stamping it asked for rather than the one it is standing in. |
| 11 | **2026-09-04** — `RunContextProcessor` **subclasses `opentelemetry.sdk.trace.SpanProcessor`** rather than duck-typing the four public methods, which puts the SDK's import on the path of `import tracepad`: 21 ms becomes 34 ms, still under `import opentelemetry.sdk.trace`'s own 37 ms (spec 017, Measurements) | The SDK calls a processor through private hooks as well as public ones — `_on_ending` in 1.44 — so a duck raises `AttributeError` on every span the moment a minor release adds another. Inheriting is the contract; guessing at it is a bug that a `>=1.44,<2` dependency range invites. The import cost is real and is paid by `import tracepad` alone: every application that calls `init` imports the SDK anyway, and the number spec 017 promised is still true. |
| 12 | **2026-09-04** — The run's summary is read with **`Run.get()`**, as the Package contract and Decision 8 have it; the Overview's `Run.summary()` is the same call under an earlier name and is not implemented | One reading of a spec has to win, and the contract is the one that names a return type. `get()` is also the honest name for what it does: it fetches the run, of which the summary is a field — `run.get()["summary"]` is the line the docs print. |
| 13 | **2026-09-04** — Amends Decision 4. A case begins where the **block** does, not where the trace does: the `Attempt` records the trace of every stamped span whose parent is not itself inside the block, and records one trace once however many of its spans the block opened. Under a harness that opens no span of its own this is exactly Decision 4's rule — one root, one trace per attempt — and under one that does (a traced `main`, an instrumented test runner) the case still has a trace to score instead of `Attempt.score` raising mid-run (owner decision 2026-09-04: degrade, do not refuse) | "The span has no parent in that context" read as "no parent at all" makes the harness's own instrumentation break the harness: every span of the loop is a child, no root is ever seen, and the first `attempt.score(...)` raises — a failure a mile from its cause, in a run that then closes as `failed`. Degrading keeps the block's promise (there is a trace, and it is the one this case produced) and pays for it with the truth the trace already told: a loop wrapped in one span is one trace, so its cases share it and the run's coverage collapses to the last item stamped. `docs/sdk-python.md`'s reach table says exactly that, because the fix is not a licence to trace the harness (found in review of PR #36). |
| 14 | **2026-09-04** — `Run.items()` sends **no `limit` of its own** and takes one (`limit=`); only `Dataset.items()` asks for pages of 500 | The two listings are not alike. A dataset's items are budget-exempt by spec 014 #19 — a truncated test case is a different test case — so the harness may ask for the largest page the API allows. A run's items inline every row's input, output and scores *and* are budget-checked, so a page of 500 overruns `TRACEPAD_RESPONSE_BUDGET_BYTES` and is refused, which made the call unusable above roughly a hundred cases. The server's own default is the size that is always safe, and a caller who knows its rows are small can say so (found in review of PR #36). |
| 15 | **2026-09-28** — **`put_items` sends a list longer than the 10,000 items one request takes (spec 014 #34) as consecutive writes of 10,000**, in order, each its own version. It returns the last write's `version` and the sum of `changed`; a write that fails raises its error with the writes before it in place — a second call with the same items finishes the job when they carry ids. Before splitting, a list that gives one `id` to two items raises `ValueError` naming both indexes, and nothing is sent. An empty list is still sent, for the server to refuse. Amends #1's "posts the batch whole" for lists over the cap; within it, nothing changes | The server now refuses more than 10,000 items with `413` (spec 014 #34), and `put_items` is the harness's bulk load — a script that built its cases from a file should not have to learn the cap. The repeated-id check keeps the one guarantee a single request gave that a split would lose (spec 014 #34 (c)). The Node and Go packages do the same (spec 032 #21, spec 033 #19). Scores need nothing: every package sends them in batches of 100 (spec 017 #6). |

---

## Package contract

Additions to spec 017's surface, same package:

| Name | Signature | Notes |
|---|---|---|
| `dataset` | `(name: str) -> Dataset` | Decision 1 |
| `Dataset` | `.name`, `.create(description=None, metadata=None)`, `.put_items(items: Iterable[dict \| Item]) -> tuple[int, int]`, `.items(version=None) -> Iterator[Item]`, `.run(name, *, metadata=None, id=None, dataset_version=None) -> Run`, `.runs() -> Iterator[dict]`, `.delete(confirm: str)` | `delete` requires the echoed name (spec 014 #20); no dry run is exposed. |
| `Item` | frozen dataclass: `id, input, expected_output, metadata, dataset_version, source_trace_id, source_observation_id` | `put_items` accepts `dict`s with the API's keys or `Item`s; unset fields are omitted from the body. |
| `Run` | `.id`, `.dataset`, `.dataset_version`, `.name`, `.item(case: Item \| str) -> ContextManager[Attempt]`, `.finish(timeout=30.0)`, `.fail(error, timeout=30.0)`, `.get() -> dict`, `.items(unknown=False) -> Iterator[dict]`, `__enter__`/`__exit__` (Decision 2) | `item` takes an `Item` or a bare 32-hex id. |
| `Attempt` | `.run_id`, `.item_id`, `.traces: list[str]`, `.trace_id: str \| None`, `.attributes() -> dict[str, str]`, `.score(name, value, **kw)` | Decision 4 |
| `score_configs` | `(configs: Iterable[dict \| ScoreConfig]) -> None` | Decision 6 |
| `ScoreConfig` | dataclass | |
| `item_id` | `(key: str) -> str` | Decision 7 |
| `compare` | `(a: str, b: str) -> dict` | Decision 8 |

**The processor** (`_harness.py`, `RunContextProcessor`): `on_start(span,
parent_context)` reads the `ContextVar` from `parent_context`; when set,
`span.set_attribute` for both keys, and when the span has no parent in that
context (a root), appends `format(span.context.trace_id, "032x")` to the
current `Attempt.traces`. `on_end`, `shutdown`, `force_flush` are no-ops.
`init` registers it before the exporting processor so that the exporter
sees the attributes on every span it batches.

**The loop, as `docs/datasets.md` will print it:**

```python
import tracepad

tracepad.init()  # TRACEPAD_HOST / TRACEPAD_API_KEY from the environment

tracepad.score_configs([
    {"name": "accuracy", "data_type": "numeric", "direction": "higher", "min": 0, "max": 1},
    {"name": "verdict", "data_type": "categorical", "categories": ["pass", "fail"]},
])

golden = tracepad.dataset("support-golden")
golden.put_items(cases)  # same cases → same version, nothing written

with golden.run("prompt v7 / claude-sonnet-5", metadata={"prompt": "support-answer@7"}) as run:
    for case in golden.items(version=run.dataset_version):
        with run.item(case) as attempt:
            answer = app.answer(case.input["question"])   # its spans carry the run and the item
            attempt.score("accuracy", judge(answer, case.expected_output))
            attempt.score("verdict", string_value="pass" if ok else "fail", data_type="categorical")

print(run.get()["summary"])
```

---

## Testing

**Unit** (`pytest`, `InMemorySpanExporter`, a fake `_http` recording
requests and answering canned JSON):

- `Dataset.items` follows `next_cursor` across three pages and stops at
  `null`; omits `cursor` on the first request; passes `version`.
- `put_items` sends `Item`s and `dict`s as one body, omits unset fields,
  returns `(version, changed)`.
- `Run.__exit__` finishes on a clean exit, fails with the exception's
  `repr` on an error and re-raises; a run already `finish`ed by hand is not
  finished twice.
- The processor: a span started inside `run.item` carries both attributes;
  a span started before the block or after `__exit__` carries neither; a
  child span started inside inherits through its parent's context; nested
  blocks replace and restore; an `async` task spawned inside the block is
  stamped; a thread run with `contextvars.copy_context().run` is stamped
  and one run bare is not (the documented distinction).
- `Attempt.traces` lists every root in order; `trace_id` is the last;
  `score` before any root raises `ValueError`; `attributes()` is the two
  keys.
- `finish` flushes scores, then spans, then posts, in that order (a
  recording fake); a flush timeout logs and still posts.
- `score_configs` `PUT`s in order and raises on the first non-2xx with the
  name in the message.
- `item_id("a")` equals `sha256("a").hexdigest()[:32]`.

**Mutation** — every invariant above by reverting its line; the table in
the PR.

**E2e** (`sdk/python/tests/e2e`, the `sdk` CI job): a real binary; the loop
above verbatim over a two-item dataset and an application function that
opens its own span with a generation inside; then, through the read API:
both traces carry `run_id`/`item_id`, the run is `finished`, its summary
counts two items, two traces and the scores by name, `GET /runs/{id}/items`
shows each item's attempt with its output, and a second run over the same
version followed by `tracepad.compare(a, b)` returns verdicts for
`accuracy`. A third scenario re-runs `put_items` with the same cases and
asserts `changed == 0` and the same version.

**Measurements in the PR**: `make sdk-lines` against the 1,500 budget shared
with spec 017; the harness's share.

---

## Edge cases

- **The application under test calls another service over HTTP**: that
  service's spans are not stamped (Decision 3); `Attempt.attributes()` is
  what the harness forwards, and `docs/datasets.md` says so beside the
  shell recipe's "both may be set at any level".
- **A case is run three times inside one block**: three roots, three
  traces, all in `traces`, all in the summary (spec 014 #2).
- **`run.item` with an `Item` from a different version** than the run
  pinned: stamped as given; the server reports it under `unknown` (spec 014,
  `…/runs/{id}/items?unknown=true`). The package does not check.
- **`init(export=False)` in a harness whose application exports through
  another SDK**: the processor is still registered on the application's
  provider and stamps that SDK's spans; this is the reference application's
  configuration and the e2e's second variant.
- **`Run` used without `with` and never finished**: `running` with its age,
  as spec 014 #8 says; nothing in the package times it out.
- **`finish` when the server is away**: `TracepadHTTPError` after the flush;
  the run stays `running` on the server, and the harness's own exit code
  says the job failed.
- **`put_items` with a batch the server rejects** (an unknown top-level
  field, spec 014 #4): the error names the item; nothing was written.
- **Two harnesses hash the same natural key** for a run id in two datasets:
  the second gets spec 014 #24's 409, raised as `TracepadHTTPError`.

---

## Config additions

None.

---

## Out of scope

- Executing cases, retries, concurrency helpers, a judge: the harness is
  the user's program.
- A `pytest` plugin or a CLI (`tracepad eval …`).
- Annotation queues, promoting a trace to a case from Python (the UI does
  it, spec 016 #8; the API is spec 014's).
