# Spec 036 — Deleting traces from the SDKs

**Status:** 🟡 DRAFT
**Sprint:** September 2026

> Spec 035 gave the store a way to remove traces, and the case that asked
> for it was a script's: an eval harness that exported under the wrong key,
> a test suite that wants the traces it just produced gone before the next
> run, a batch job cleaning up after itself. Those callers already hold an
> SDK, and the SDK is where they would look. This spec adds the two
> deletions to the three packages — one trace by id, many by the listing's
> filter — in the shape each package already gives `Dataset.delete`, with
> the rounds the bulk endpoint answers in walked by the package rather than
> by every caller.

---

## Overview

Deliverables, one PR (the last commit flips the status):

- Python: `tracepad.delete_trace(id, *, confirm=False)` and
  `tracepad.delete_traces(*, to, confirm="", limit=1000, **filters)`
  (Decisions 1–5).
- Node: `deleteTrace(id, { confirm })` and
  `deleteTraces({ to, …filters }, { confirm, limit })` (Decisions 1–5).
- Go: `DeleteTrace(ctx, id, confirm bool)` and
  `DeleteTraces(ctx, TraceFilter, confirm string, ...DeleteOption)`
  (Decisions 1–5).
- A *Deleting traces* section in `docs/sdk-python.md`, `docs/sdk-js.md`,
  `docs/sdk-go.md`; a line in `docs/admin.md`'s deletion section pointing
  at them (Decision 6).
- Unit tests against a fake server per package, and one e2e case per
  package against the real binary (Decision 7).

Not here: deleting observations, sessions or datasets' runs from the SDK;
a harness-level "delete what this run produced"; anything in the MCP.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-17** — The **single form takes a boolean** — `confirm=False` / `{ confirm: false }` / `confirm bool` — and the package sends the trace id as the echo when it is true; false is the API's dry run and returns its preview. The **bulk form takes the project name as a string**, exactly as `Dataset.delete(confirm)` does: empty is the dry run, the name deletes | Spec 035 #1 keeps the id as the API's echo for uniformity and says who pastes it: "a client that has the id in hand". The SDK is that client — it was just given the id — so asking the caller to type it twice is a ritual, not a check (spec 035 #8's reasoning for the prefilled card). The bulk form destroys a slice of the project, the package does not know the project's name, and a name the caller has to type is the check spec 005 #8 wants. |
| 2 | **2026-09-17** — The **bulk form walks the rounds itself**: it repeats the confirmed call while the answer says `more`, and returns one total — `{"deleted": {traces, observations, scores, payloads, annotation_items}, "rounds": N}` (the same keys in each language's idiom). `limit` bounds one round (1–1000, default 1000), not the whole. The dry run makes one call and returns the API's preview as it is (`matched`, `would_delete`, `affected_runs`, `oldest`, `confirm`, `note`) | The rounds exist for the interface's clock (spec 035 #4); a script has no clock and wants the job done. A loop every caller writes is a loop some caller gets wrong — stopping at the first answer and believing the deletion complete. The package returning after the last round is the contract a script can trust. `rounds` is reported so a test can see the loop ran. |
| 3 | **2026-09-17** — **Filters are the listing's, by their API names**, and **`to` is required by the signature**, not by a runtime check: Python keyword-only `to` with `**filters` (`from_` for `from`, the one reserved word); Node a `filters` object whose type requires `to`; Go a `TraceFilter` struct with one field per listing parameter (`From`, `To` as `time.Time`; the rest strings; `Tag` a `[]string`), and `To.IsZero()` is an error before any request. Times are accepted as the language's time type or an RFC 3339 string and sent as RFC 3339 UTC. Unknown filter names are not checked by the package: the API answers `400` and the package raises it | The API's grammar is one grammar (spec 024 #4, spec 035 #2); a second spelling in each package is three more things to keep in step. `to` is required at the API for a reason (spec 035 #2) and a signature that cannot be called without it is the cheapest enforcement. Not validating names client-side keeps the package from lagging the server's filter list (spec 034 added `prompt` to it). |
| 4 | **2026-09-17** — **Synchronous, raising**, like every REST call the packages make: Python raises `TracepadHTTPError` for a non-2xx answer, Node rejects, Go returns the error. A `404` on the single form is that error, not `None`. Timeouts: the request helper's default for the dry run; the confirmed bulk call uses the same per-round timeout, so a long deletion is bounded per round, not in total | The packages' rule for the harness side (spec 018, spec 032 #10, spec 033): a script wants a value or an exception. Deletion is a script's act. |
| 5 | **2026-09-17** — **Placement**: top-level functions beside `score` and `prompt` (Python `tracepad.delete_trace`, Node `import { deleteTrace, deleteTraces } from 'tracepad'`, Go `tracepad.DeleteTrace`), in a module of their own (`_traces.py`, `traces.ts`, `traces.go`) that the tracing path never imports | A trace is not a dataset; hanging deletion off `Dataset` would be a category error, and there is no `Trace` object in any package to hang it on — nor should there be one for two functions. A module of its own keeps the REST import (`urllib` in Python) off the tracing path, which spec 032/033's line budgets and the Python package's import-time rule both care about. |
| 6 | **2026-09-17** — **Docs**: each SDK page gains a *Deleting traces* section after *Prompts* with the two calls, the dry-run-then-confirm rhythm in five lines, and the raw-bodies note carried over from `docs/admin.md`; `docs/admin.md`'s deletion section gains one sentence pointing at the three | The rhythm is the thing to teach; the reference table each page has already lists the signatures. |
| 7 | **2026-09-17** — **Tests**: per package, unit tests against the fake server the package's suite already uses — the dry run's request (`to` present, filters passed through, no `confirm`), the confirmed single call (echo is the id), the bulk loop (two answers with `more: true` then `false` → three calls, totals summed), a `404` raised; and one e2e case against the real binary that exports two traces, deletes one by id and the other by filter, and reads the listing to see them gone. The SDK line budgets are reported as every SDK PR reports them | The loop is the logic in this spec and a fake server is where it is proved; the e2e is what proves the wire shape against the real thing, which the golden fixtures do not cover (they are about spans). |
| 8 | **2026-09-17** — The **application-line budgets rise** with the measured addition: Python 1,600 → **1,700**, Node 1,900 → **2,000**, Go 1,900 → **2,100** | The raise rule of spec 031 #22 as spec 032 #16 and 033 #17 applied it: the measured `main`, plus the measured addition, plus a review cycle. The packages stood at 1,563, 1,842 and 1,870 on `main`, at 37, 58 and 30 lines under their ceilings; the module of Decision 5 is 62 lines in Python, 78 in Node (two of them the repeated query parameter `tag` needs in the request helper) and 119 in Go, where a struct with a field per filter and its walk to `url.Values` is the price of Decision 3's typed filter — 1,625, 1,920 and 1,989 after. A ceiling left as a standing warning is a ceiling nobody reads (spec 035 #17), so each is raised once for the known set. |
| 9 | **2026-09-17** — In Go, the confirmed bulk answer is a `map[string]any` whose numbers are **`float64`**, `rounds` included, like every answer the package decodes; the dry run is the API's map as it came | Decision 2's "the same keys in each language's idiom": the Go package's idiom for an answer is the server's JSON as `encoding/json` decodes it (spec 018 #8, spec 033 #10), and a sum the package built that reads differently from the preview beside it — `int` here, `float64` there — would be two conventions in one call. A struct for the sum alone would make `DeleteTraces` return one type for the dry run and another for the deletion. |

## API contract (what the packages call)

| Package call | Request |
|---|---|
| `delete_trace(id)` / `deleteTrace(id)` / `DeleteTrace(ctx, id, false)` | `DELETE /api/v1/traces/{id}` |
| `delete_trace(id, confirm=True)` | `DELETE /api/v1/traces/{id}?confirm={id}` |
| `delete_traces(to=…, environment="staging")` | `DELETE /api/v1/traces?to=…&environment=staging` |
| `delete_traces(to=…, environment="staging", confirm="my-project")` | `DELETE /api/v1/traces?to=…&environment=staging&confirm=my-project&limit=1000`, repeated while `more` |

Answers are the API's (spec 035 §API contract); the bulk confirmed answer is
the package's sum.

## Testing

See Decision 7. The e2e case runs under each package's existing e2e
harness (`SDK_SKIP_E2E` respected); the unit tests under the package's
default suite.

## Edge cases

- `delete_traces(to=…, confirm="wrong")`: the API's `400`, raised on the
  first round; nothing was deleted.
- A bulk deletion whose first round finds nothing: one call, totals of zero,
  `rounds: 1`.
- `to` given as a naive `datetime` in Python: treated as UTC and said so in
  the docstring, the package's rule for every time it sends.
- `limit` outside 1–1000: the API's `400`, not a client-side clamp.

## Out of scope

- A `Trace` handle or a listing call in the packages: the packages trace
  and evaluate; reading the store is the CLI's, the API's and the MCP's.
- Deleting by run from the harness (`run.discard()`): a run's traces are
  `run_id=` in the filter, and the harness has no cleanup step today.
