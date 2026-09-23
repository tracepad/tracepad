# Spec 040 — Testing an application's instrumentation

**Status:** 🟡 DRAFT
**Sprint:** September 2026

> An application that traces wants to test that it does: that the analysis
> step is a span with the right name, that the model call is a generation
> with its usage, that the thumbs-up posts a score. Each package can do this
> for its own suite — a fixture resets the process, installs an in-memory
> exporter and initialises without export — but the reset reaches into
> private names: the initialised flag, the configuration and prompt caches,
> the score queue, the warned-kind set, and OpenTelemetry's own global
> provider. The first application to move to the packages copied that list
> into a dozen test files, where the next refactor of the package will break
> it without a word. This spec ships the reset and the capture as a public,
> supported surface in all three packages.

---

## Overview

Deliverables, one PR (the last commit flips the status):

- Python: `tracepad.testing` — `capture()`, `reset()`, and a pytest plugin
  that offers them as fixtures when a suite opts in (Decisions 1–4).
- Node: the `tracepad/testing` entry point — `capture()`, `reset()`
  (Decisions 1–3, 5).
- Go: package `tracepadtest` — `Capture(t)`, `Reset(t)` (Decisions 1–3, 6).
- Each package's own suite moves onto the public helpers where they cover
  what the private fixture did (Decision 7).
- A *Testing your instrumentation* section on the three SDK pages.

Not here: mocking the REST calls (`prompt`, datasets, deletion) — an
application stubs its HTTP as it does for any other service; asserting on
what the server made of the spans (that is the e2e suite's job).

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-23** — **`capture()` gives a fresh, initialised process that records instead of exporting**: it resets everything the package keeps process-wide, installs a tracer provider with an in-memory exporter as the global one, initialises the package against it with export off and a placeholder host and key, and swaps the score queue for one that appends each score's body to a list instead of posting it. It returns a **`Capture`**: `spans` (every finished span, in order), `one(name)` (the single span of that name; fails naming what there was), `attributes(name)`, and `scores` (the bodies `score()` would have posted). Leaving it — end of the `with` block, `restore()` / `Symbol.dispose` in Node, `t.Cleanup` in Go — resets the process again | The four things an application asserts on are the span tree, a span's attributes, the scores, and nothing leaking into the next test; that is the whole surface. The in-memory exporter is OpenTelemetry's own, so a `Capture`'s spans are the SDK's `ReadableSpan` / `SpanStub` an OTel-literate reader already knows. The placeholder host and key make `init` succeed without a server; nothing is sent to them because export is off and the queue does not post. |
| 2 | **2026-09-23** — **`reset()` alone** returns the process to *never initialised* — the state spec 039 defines as tracing off — without capturing anything, so an application can test that its code runs with tracing off (no exception from `score`, absent ids) | Tracing-off is a supported configuration since spec 039 and deserves a test as much as tracing-on; it needs the same private reset, and nothing else. |
| 3 | **2026-09-23** — **The private reset lives in the package, in one place**, and the helpers are the only public door to it. That includes resetting OpenTelemetry's global provider, which the OTel API deliberately allows only once per process: the Python helper touches `opentelemetry.trace`'s private globals, Node calls `trace.disable()` / `context.disable()`, Go replaces the global provider. The package's OpenTelemetry version range (already pinned) is what makes that dependency safe, and a test in each package fails if the reset stops working against the pinned range | The reset has to reach private state somewhere; the question is whether every application does it or the package does it once, behind a name it promises to keep working. The OTel global is the sharpest edge — its "set once" rule is right for applications and wrong for a test suite — and it is exactly the part an application copying our conftest would get wrong on an OTel upgrade. |
| 4 | **2026-09-23** — **Python**: `tracepad.testing` is imported only by tests (the tracing path never imports it). The pytest plugin is **opt-in** — `pytest_plugins = ["tracepad.testing"]` in the suite's `conftest.py` — and provides two fixtures: `tracepad_capture` (a `Capture`, reset around the test) and `tracepad_off` (a reset process). No `pytest11` entry point: an installed package must not load itself into every pytest run on the machine | Opt-in is the difference between a helper and a side effect: an entry-point plugin runs in the suites of projects that merely have the package installed. The fixtures are named with the package's prefix so they cannot collide with an application's own. |
| 5 | **2026-09-23** — **Node**: `import { capture, reset } from 'tracepad/testing'`, a separate entry in `package.json`'s `exports` (ESM and CJS like the root). Framework-agnostic: `capture()` returns a handle with `restore()` and `[Symbol.dispose]`, so `using c = capture()` works where supported and `beforeEach`/`afterEach` everywhere else. `@opentelemetry/sdk-trace-base`'s in-memory exporter comes from the dependencies the package already has | A subpath keeps the testing code out of the root bundle an application ships. Vitest and Jest are both common; a handle with an explicit restore serves both without importing either. |
| 6 | **2026-09-23** — **Go**: package `github.com/tracepad/tracepad/sdk/go/tracepadtest`, in the idiom of `httptest`: `tracepadtest.Capture(t testing.TB) *Recorder` registers its own cleanup; `Recorder.Spans()`, `One(t, name)`, `Attributes(t, name)`, `Scores()`; `tracepadtest.Reset(t)` for the tracing-off state. The main package exposes what the helper needs through an internal hook, not a public API | Go's convention for test helpers is a sibling package that takes `testing.TB`; `t.Cleanup` makes forgetting the restore impossible. |
| 7 | **2026-09-23** — **The packages eat their own helper**: each suite's private fixture is rewritten on top of `capture()` / `reset()` wherever the helper covers it (the environment-variable scrub and the scripted `fetch` stay local), so the public surface is exercised by every test the package runs | A helper the package's own tests do not use is a helper that drifts; here it cannot, because the suite breaks first. |
| 8 | **2026-09-23** — **Line budgets**: the testing modules count toward their package's application lines like any other module, and each budget rises by the measured size of its module plus a review cycle, spec 036 #8's rule; the PR reports the three numbers | The helper is shipped, supported code with the same review bar. Node stands at its ceiling after spec 039; raising by measurement, once, is the rule the budgets have always followed. |

## Testing

- Per package: a capture records spans in order, `one` fails with the names
  it saw, `scores` holds the body `score()` built (trace id of the active
  span, value, name); nothing reaches the network (a transport that fails
  the test on any call); leaving the capture leaves a never-initialised
  process (`score` inside a span is a no-op, ids absent — spec 039); two
  captures in a row do not see each other's spans or scores; `reset()` alone
  gives the tracing-off state.
- Python: a suite that opts into the plugin gets both fixtures; a suite
  that does not is untouched.
- Node: ESM and CJS imports of `tracepad/testing`; `using` works on a
  runtime that has it.
- Go: `Capture` from two sequential tests; cleanup runs on `t.Fatal`.
- The reset test of Decision 3 in each package.

## Edge cases

- An application that installs its own global provider in production code
  at import time: `capture()` replaces the global for the test; the
  application's provider is not restored afterwards (a test process should
  not depend on import-time globals surviving a reset), and the docs say so.
- Parallel tests in one process (pytest-xdist workers are processes, so
  unaffected; Go `t.Parallel()`): the capture is process-global, and the
  Go helper fails a parallel test that calls it, naming why.
