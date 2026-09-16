# Spec 035 — Delete traces: one by id, many by the listing's filter

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> A trace leaves the store in exactly two ways today: the retention sweep
> takes it when its window runs out, or the project is deleted whole. In
> between there is nothing — and in between is where the mistakes live: an
> eval harness that exported sixty traces under the application's key
> instead of its own, a load test run against production, one trace that
> holds something a person should not have typed. Each of these is a
> handful of rows the operator can point at and cannot remove, and the
> cost figures for that day carry them until retention clears them weeks
> later. This spec adds deletion of traces — one by id, or every trace a
> listing filter matches — through the same consistent path user-data
> erasure already walks, with the same ceremony every destructive act in
> this store wears.

---

## Overview

Deliverables, one PR (the last commit flips the status):

- `DELETE /api/v1/traces/{id}` and `DELETE /api/v1/traces?<filters>&to=`,
  both a dry run until `?confirm=` echoes what they destroy; the bulk form
  works in bounded rounds the client repeats (Decisions 1–6).
- A store job, `TraceDelete`, that takes the traces the way `UserDataErase`
  takes a user's — observations, scores, annotation items, payload GC,
  search entries, the hours re-rolled in the same transaction — with the
  chunk bounds it has (Decisions 2–3).
- CLI `traces rm <id>` and `traces rm --to … [filters]`; no MCP tool
  (Decision 7).
- *Delete…* on the trace header and, beside *Add to queue…*, on the traces
  listing, each through `ConfirmCard` with the server's dry run
  (Decisions 8–9).
- `docs/admin.md` (a section beside user-data erasure), `docs/api.md`,
  `docs/cli.md`, `docs/retention.md`, `docs/ui.md`, `docs/annotation.md`,
  `docs/datasets.md`, `openapi.json`, `schema.d.ts`.

Not here: moving traces between projects, a trash or undo, deleting one
observation or one session, warnings about suspicious ingest.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-17** — **`DELETE /api/v1/traces/{id}`** (role: editor, like erasure) removes one trace and everything attached to it. Without `?confirm=` it is a dry run: `{"dry_run": true, "would_delete": {"traces": 1, "observations", "scores", "annotation_items"}, "oldest", "affected_runs": […], "confirm": "<id>", "note": "raw OTLP bodies are not deleted; they expire on the raw retention window"}`; an unknown id is `404`. With `?confirm=<id>` it deletes and answers `{"dry_run": false, "deleted": {"traces", "observations", "scores", "payloads", "annotation_items"}, "id"}`. The echo is the trace id | The ceremony is spec 005 #8's, unchanged: a dry run that says what would go, and the exact string that makes it happen. The echo is the id because the id is the trace's only identity, the way the user id is the echo of erasure (spec 023 #10) — and a client that has the id in hand pastes it, which is what the CLI's `--yes` and the interface's prefilled card do. The `note` is the one erasure carries, for the same reason (#4). |
| 2 | **2026-09-17** — **`DELETE /api/v1/traces`** takes **the trace listing's filters as query parameters** (spec 004, spec 012 — the same names, the same validation, `400` on an unknown one), and **`to=` is required**, dry run and confirmed alike: without it, `400` saying so. The dry run counts the match **exactly** (not at the listing's cap) and answers `{"dry_run": true, "matched": N, "would_delete": {…}, "oldest", "affected_runs", "confirm": "<project name>", "note"}`. The echo is the **project name** | The filter is the vocabulary the listing already has and `from-traces` already takes (spec 024 #4): "delete what I am looking at" is one call with no second grammar. `to=` is what makes the set a closed one: the listing is half-open on `to` (spec 004), so a trace that starts after it can never qualify, and the operator who previewed a thousand does not delete a thousand and forty because ingest kept flowing between the preview and the confirmation. (Spans that arrive late for a trace *inside* the window are the ingest race #10 describes, not a hole in this rule.) There is no name for a filter, and the thing being destroyed is a slice of the project, so the project name is the echo — the one retention shrinking already uses for the same act by another door (spec 005 #8). |
| 3 | **2026-09-17** — The store job **`TraceDelete`** takes a set of trace ids and does what `UserDataErase.apply` does for them, in the same order and the same transaction: observations, scores, annotation items (spec 024 #3), the traces, the payloads nothing references any more, the search entries (spec 011 #7), then a whole-hour `statsRoll` for every hour the chunk emptied that is below the rollup watermark (spec 013 #7, spec 023 #19), the per-user summaries recomputed once per chunk for the users those rolls touched. A chunk is at most **500 traces of one hour** — `UserDataErase`'s own bounds, for its reasons — and the erasure job is refactored to share the body rather than copied. Raw OTLP bodies are **not** touched | One consistent path, not two: erasure's path was measured and reviewed (spec 023 #10, #19) and every promise `docs/retention.md` makes about what outlives what was made for it. A second copy would drift; the shared body is what keeps "deleted" meaning the same thing on both doors. Raw bodies are stored per *batch* (`raw_batches`), and a batch holds many traces of many kinds, so a trace cannot be cut out of one — the position erasure states (`docs/retention.md`, *what this means for a data-subject request*) holds here for the same structural reason, and the dry run says so. |
| 4 | **2026-09-17** — The **bulk form works in rounds**: a confirmed `DELETE /api/v1/traces` deletes at most **`limit`** traces (1–1000, default 1000) and at most **50 hour-chunks** under #3, whichever comes first (#14), then answers `{"dry_run": false, "deleted": {…}, "more": bool}`; the client repeats the same call while `more` is true. The single form has no rounds — one trace is one chunk. Selection order is the listing's, newest first (`traceQuery` with the filter, the way `QueueItemsFromTraces` selects) | Erasure loops to completion inside one request, and a user's history was judged rare and bounded (spec 023 #19). A filter is neither: it can name a month. A request that runs for minutes is a request the interface's thirty-second clock (spec 010 #10) cuts off every time, and then the operator sees an error while the store is in fact fine. A bounded round is the model `from-traces` already answers with (`capped`, spec 024 #4): every round is a complete, consistent act, the answer says whether there is more, and the client — the CLI, the dialog — is the one that loops and can show *2,000 of 12,000*. Newest first because that is what the listing showed and what the operator counted. |
| 5 | **2026-09-17** — A confirmed bulk deletion **recounts nothing**: it takes what the filter matches at that moment, up to `limit`. The dry run's `matched` is information, not a lock | With `to=` required (#2) the set can only shrink between preview and execution — retention, another operator, an earlier round — and a set that shrank is not a reason to refuse. A count-as-echo would make every second round of a large deletion fail its own check. |
| 6 | **2026-09-17** — **Runs**: a trace an eval run holds is deleted like any other and the run shows the item as missing afterwards; the dry run names the runs under `affected_runs`, the shape erasure answers with (spec 014 #14) | The pin a run puts on its traces is a pin against *retention*: it says "do not age this out", not "nobody may remove this". Erasure already outranks it, for the same reason a deliberate deletion does, and the preview is where that is said so the operator sees the hole before it opens. |
| 7 | **2026-09-17** — **CLI**: `traces rm <id> [--yes]`, and `traces rm --to <time> [the listing flags of `traces ls`] [--limit N] [--yes]`; the bulk form repeats rounds until `more` is false and reports the running total, `--limit` bounding one round rather than the whole. Both go through the CLI's one `destructive` helper (dry run shown, then the server's echo sent; `--yes` replaces the typing, not the check — spec 024 #15). `--to` is required for the bulk form; the CLI does not fill it in. **MCP**: nothing — the MCP surface is reads only (spec 004 #16, #17) | Three clients, one surface (spec 004 #1). The CLI does not invent a `to` because a script that deletes by filter should say what it means; the interface fills it in (#9) because there the moment is on screen. An agent that can read every trace must not be one prompt away from deleting them. |
| 8 | **2026-09-17** — ***Delete…*** on the **trace header**, beside *Add to queue* (editor only, like it): a dialog around `ConfirmCard` with the id prefilled as the echo, the server's dry run on screen (observations, scores, annotation items, affected runs, the raw-bodies note), *Delete this trace*; on success the peek panel closes and the listing re-reads, or, from `/traces/{id}`, the page leaves for the listing | The reader's gesture at the surface where the reading happens, wearing the ceremony every destructive act wears (spec 007 #5). Prefilled rather than typed: the user page prefills the id for erasure for the same reason — the id is on screen and typing thirty-two hex characters is a ritual, not a check. |
| 9 | **2026-09-17** — ***Delete…*** on the **traces listing**, beside *Add to queue…* (editor only): the dialog names the current filters (the `FilterBar`'s chips) and the listing's match count, pins **`to`** to the moment the dialog opened when the filter has none (and says so: *traces that started before 14:02:17*), runs the dry run, asks for the project name, then loops rounds (#4) with a progress line *2,000 of 12,000 deleted* and a *Stop* that finishes the round in flight and leaves the rest; on completion the listing re-reads. An empty filter is allowed — it is *every trace before `to`* — and the dialog's wording makes that plain | The manager's gesture at the surface where the choice is made (spec 024 #13). The count is on screen before the call so a filter that matches ten thousand is seen before anything goes. The pinned `to` is #2's rule made visible: what the operator confirms is the set they counted. *Stop* exists because rounds are the unit of consistency (#4) and a person who sees the number and changes their mind must not need to close the tab. |
| 10 | **2026-09-17** — **The ingest race is documented, not fought**: a deletion removes what the store holds at that moment; a span that arrives afterwards for a deleted trace creates the trace again from what arrived, as it would for a trace never seen. Nothing is remembered about deleted ids | Erasure has the same edge and the same answer; a tombstone table would be a third thing every ingest path checks for a race `to=` already makes rare. An operator deleting a trace whose spans are still arriving has a stopwatch problem, not a store problem, and `docs/admin.md` says so. |
| 11 | **2026-09-17** — The **preview reads are reads**: the dry run's counts, `affected_runs` and `matched` come from the read side before any job is submitted, and a confirmed request computes none of them (erasure's own rule, `handleEraseUserData`) | A number that would be computed and thrown away is a read that can fail a request that was going to succeed. |
| 12 | **2026-09-17** — The application-line ceiling stays where spec 024 #17 and later left it; the two dialogs are expected under ~300 lines together, and the PR reports the number per screen; the listing dialog names the active filters as the `FilterBar` already renders them — its chips — rather than inventing a prose rendering of a filter | Two small dialogs on an existing card; a second rendering of the same filter is where lines would come from. |
| 13 | **2026-09-17** (from the live check on a copy of a real database) — **Migration 0020: four partial indexes on the columns that reference `payloads(id)`** — `observations(input_id)`, `(output_id)`, `(metadata_id)` and `traces(metadata_id)`, each `WHERE … IS NOT NULL`. The store contract's "no schema change, no new index" is amended | The first round through the interface — 283 traces of one hour — ran past the thirty-second clock. Timed inside the shared body: 12 traces of one hour took 2.4 s, of which 2.26 s was `DELETE FROM payloads WHERE id IN (…)`. Under `foreign_keys=ON` every payload deleted is a foreign-key check in every referencing column, and none was indexed, so each check scanned `observations` (three times) and `traces`: ~6 ms per payload on a database of 24k observations, ~6 payloads per trace, ~36 ms per trace deleted. Pre-existing, not introduced here — erasure and the retention sweep delete payloads by the same statement, and an erasure of a 28-trace user on the same copy took 8 s — and every one of the three paths gains. Measured on the copy: 100 payloads deleted in 650 ms without the indexes, 22 ms with them, the plan a `SEARCH … USING COVERING INDEX` on each of the four (a plan test keeps it so). Partial rather than whole: the check looks for a row *equal* to the deleted id, which `IS NOT NULL` implies, so the planner takes the partial index, and a trace with no metadata — nine in ten on the copy — writes nothing into it. The price at ingest, `BenchmarkIngestBatch` with every observation carrying all three payloads (the worst case for a partial index): 7.3 → 7.8 ms per 20-span batch with the search index (+7%), 3.8 → 4.4 ms without (+16%) — about half a millisecond per batch of twenty spans, at a target scale of thousands to tens of thousands of traces a day, and less on real data, where 34% of observations carry no input or output and 89% of traces no metadata (the copy). Both alternatives are worse: leaving the payloads to the sweeper's orphan collector makes `deleted.payloads` a lie and the collector scans the same way; dropping the foreign keys is a rebuild of the two largest tables for the sake of the same seek. Reported to the owner before the push, with the numbers; accepted 2026-09-17. |
| 14 | **2026-09-17** (from the same live check) — **A round is bounded in chunks as well as in traces**: at most 50 one-hour chunks per confirmed request (`deleteRoundChunks`, beside `eraseChunk`), after which the round answers `more: true` with what it took; #4 amended to "at most `limit` traces and at most 50 hour-chunks, whichever first". Erasure keeps its own contract — to completion in one request | A chunk is one hour's traces (#3) and one transaction and commit of its own, so a set spread thinly over many hours is many small chunks, each paying the writer's commit window: with #13 in place, 300 staging traces of the copy spread over 118 hours took 9.2 s — 1.6 s inside the bodies, the rest the 118 commits — and a round of 1,000 such traces would be ~75 s, past the interface's clock every time. `limit` bounds the traces; nothing bounded the chunks. Fifty is a few seconds on the copy, and a round that ends early is still a complete, consistent act the client repeats — the model #4 chose, applied to the second dimension it has. The dense case is untouched: 1,000 traces of one hour are two chunks. |
| 15 | **2026-09-17** — The CLI's bulk form registers the listing's *following* flags and **`--to`** in their place of `--until`: `--until` is not a flag of `traces rm` | #7 spells the bound `--to` and calls it required; `traces ls` spells the same parameter `--until`. Registering both would be one parameter under two names on one command — and `--until` alone with a required bound would read as optional. One flag, the spec's name, and a `flag provided but not defined` for the other, which is the answer `tail --until` already gives. |
| 16 | **2026-09-17** (found by the Playwright run at 375 px) — The traces listing's filter bar **wraps at a phone's width**: the two actions, *Add to queue…* and *Delete…*, take a second row under the search box, the window and *Filters*; from `sm` up the row is one, as before | The bar's three controls take 307 px at their floors (spec 027 #22's arithmetic) and the row has 343 px at 375 px wide; one 50 px action beside them already overlapped the *Filters* button by a sliver, and a second one covers its centre — the `filters.spec.ts` deep-link test could not click it. Letting the bar scroll instead would put *Filters* past the viewport's edge, which spec 027 #22 forbids; narrowing the controls further has nothing left to take. A second row on a phone costs 44 px of height and no lines. |

## API contract

| Method | Path | Role | Dry run | Confirmed |
|--------|------|------|---------|-----------|
| `DELETE` | `/api/v1/traces/{id}` | editor | `{dry_run: true, would_delete: {traces, observations, scores, annotation_items}, oldest?, affected_runs: [{id, dataset, traces}], confirm: "<id>", note}` | `?confirm=<id>` → `{dry_run: false, deleted: {traces, observations, scores, payloads, annotation_items}, id}` |
| `DELETE` | `/api/v1/traces` | editor | filters + `to=` (required) → `{dry_run: true, matched, would_delete: {…}, oldest?, affected_runs, confirm: "<project name>", note}` | `?confirm=<project name>&limit=` → `{dry_run: false, deleted: {…}, more: bool}` |

Errors: `404` for an unknown trace id (single form, dry run and confirmed);
`400` for an unknown filter, a `limit` outside 1–1000, a missing `to=` on
the bulk form, or a wrong `confirm` (`Rejection{RejectInvalid}` from the
job, as erasure); `403` below editor. A bulk filter matching nothing is a
successful dry run with `matched: 0` and a successful deletion of nothing
(`more: false`), not an error — a repeat of a finished deletion must be
harmless.

`would_delete` and `deleted` are the erasure shapes so the interface's
`DryRun` type reads both; `affected_runs` is a list of objects, never a
count, for the reason `handleEraseUserData` states. `openapi.json` gains the
two operations; the `DryRun` schema's description of `affected_runs` widens
from "user-data erasure only" to "erasure and trace deletion".

## Store contract

`TraceDelete{ProjectID, IDs []string, Now int64}` fills `Counts DeleteCounts`
and `Hours []int64` and applies the shared body (#3) to exactly `IDs`. The
bulk handler selects ids with `traceQuery(project, filter)` bounded to the
round's `limit`, then hands them to the job in chunks of at most 500 traces
of one hour each, one job per chunk, each chunk its own transaction; the
single handler hands one id. `UserDataErase.apply` becomes: select the
user's next chunk, apply the shared body, then delete the per-user rollup
rows (spec 023 #10) — behaviour unchanged, which the existing erasure tests
prove.

No new table or column. One migration, 0020, with four partial indexes on the
columns that reference `payloads(id)` (Decision 13): the single form seeks the
primary key and the bulk form's selection is the listing's query on the
listing's indexes, but deleting a payload row is a foreign-key check in every
column that references it, and those were scans.

## Testing

- **Store**: deleting a trace removes its observations, scores, annotation
  items, orphaned payloads (and keeps a payload another trace still
  references), search entries; the hour is re-rolled in the same
  transaction and `stats_hourly` no longer counts it; a user whose only
  trace went leaves `users`/`users_hourly`, a user with others keeps a
  corrected summary; an hour at or past the watermark is not rolled; a
  frozen hour (past the retention window, a pinned trace) is left as it
  stands. The erasure suite still passes unchanged after the refactor.
- **Server**: single dry run shape, `404`, wrong echo `400`, confirmed
  answer; bulk: unknown filter `400`, missing `to` `400`, `limit` bounds,
  `matched` exact past the listing cap, `affected_runs` named, rounds
  (`more` true then false, running counts add up, a repeat after `more:
  false` deletes nothing, a round ends at the chunk bound with `more`),
  viewer `403`, editor key allowed; the payload delete's plan seeks the four
  reference indexes.
- **CLI**: `traces rm <id>` shows the preview and stops without `--yes`;
  `--yes` sends the echo; the bulk form refuses without `--to`, loops rounds
  and prints the total.
- **UI** (`vitest` on the dialogs, Playwright on both gestures): the header
  dialog previews, deletes, closes the peek and the row is gone; the listing
  dialog names the filter, pins `to`, shows progress across rounds, *Stop*
  leaves the rest; both hidden from a viewer.
- **Live check** in a browser (the DoD of every UI change): a filter
  matching a few hundred traces deleted through the interface on a copy of
  a real database, the Stats screen's day corrected afterwards.

## Edge cases

- A trace id that is well-formed but unknown: `404`, dry run or confirmed.
- The single form on a trace a run holds: deleted; `affected_runs` names
  the run in the dry run.
- Bulk with `q=`: allowed — the search filter is a listing filter; the
  match is what the listing would show.
- Bulk with `run_id=` or `item_id=`: allowed, the same way; deleting a
  run's traces is what `DELETE /api/v1/runs/{id}` does *not* do (it
  releases them to retention), and the dry run's `affected_runs` makes the
  difference visible.
- A round cut off by the client between chunks: the committed chunks are
  gone and consistent; the next request continues (erasure's contract).
- Two operators deleting overlapping sets: each chunk deletes what it finds;
  a chunk that finds nothing left answers with zeros.
- `to=` in the future: allowed; it bounds nothing extra but is honest.

## Out of scope

- Moving traces between projects. The raw bodies (`/api/v1/raw/{id}`) can be
  replayed into another project with that project's key by the client, and
  the parsed rows cannot be moved without inventing a second identity for
  every attached row; not this spec.
- A trash, undo or grace window for traces. Projects have one (spec 005
  #9) because a project is a thing with a name and a history; a trace is
  a row and a re-export recreates it.
- Deleting one observation, or a session's traces by session id as a first-
  class door — the latter is `DELETE /api/v1/traces?session_id=&to=`.
- Warnings at ingest about suspicious shapes (parentless generation
  traces): a single-span trace is a normal trace for many applications.
