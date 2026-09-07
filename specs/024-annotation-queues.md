# Spec 024 — Annotation queues: what to review, who reviewed it, and the desk to do it at

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> Spec 022 lets a person score the trace they happen to be reading. A
> review programme is the other way round: somebody decides *which* traces
> deserve a human verdict, several people work through them, and the team
> can see what is done and what is left. This spec adds the queue — a
> named list of traces or observations with the score configs a reviewer
> must fill — the ways to fill it (one at a time, or every trace a filter
> matches), the annotation desk that walks it, and the bookkeeping that
> says who completed what.

---

## Overview

Deliverables, one PR (the last commit flips the status):

- Schema 0014: `annotation_queues`, `annotation_items` (Decisions 1–3).
- `PUT|GET|DELETE /api/v1/queues/{name}`, `GET /api/v1/queues`; items:
  add one or many, add every trace a filter matches (capped), list, next
  with a soft claim, complete, skip, reopen, delete (Decisions 4–8).
- CLI `queues …`; MCP `list_queues`, `get_queue_items` (Decision 9).
- The *Queues* item in the *Evals* section; `/queues`, `/queues/{name}`,
  `/queues/{name}/annotate` — the desk; *Add to queue* on the trace
  header, the observation panel and the traces listing (Decisions 10–13).
- `docs/annotation.md` (new), `docs/api.md`, `docs/cli.md`, `docs/mcp.md`,
  `docs/retention.md`, `docs/admin.md`, `docs/ui.md`, `openapi.json`,
  `schema.d.ts`.

Not here: sampling rules applied at ingest, per-annotator agreement
statistics, queues over sessions or dataset items, notifications.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-07** — A **queue** is `(project_id, name)` with a `description` and an ordered list of **score config names** — the scores a reviewer must set on every item. The name follows the prompt grammar (`^[A-Za-z0-9][A-Za-z0-9._-]*$`, ≤ 200). `PUT /api/v1/queues/{name}` is declarative (create or replace; a re-`PUT` of the same body is a no-op); every named config must exist (`400` naming the missing one); the list may be changed later and applies to items not yet completed | A queue *is* a question ("rate these on accuracy and tone"), and the configs are the question's shape — which is what makes the desk a form the server can check (Decision 7). Declarative like score configs (spec 003) so a team keeps its queues in a file beside them. Configs are referenced by name, the binding spec 003 chose, so the queue never disagrees with what a score of that name means. |
| 2 | **2026-09-07** — An **item** is one trace or one observation of a trace: `{id, trace_id, observation_id?, status: pending\|completed\|skipped, seq, added_at, claimed_by?, claimed_until?, completed_by?, completed_at?, skip_reason?}`; `id` server-generated 32-hex; `seq` monotonic per queue; unique on `(queue, trace_id, observation_id)` — adding a target already in the queue returns the existing item and counts it as `existing`, whatever its status. The target need not exist yet (spec 003's rule for scores) | The unit is what spec 022 scores and spec 016 adds to datasets, and for the same reason: in an agent trace the thing to judge is often one generation. Idempotent add is what lets a filter be re-run and a script be retried without doubling the queue. |
| 3 | **2026-09-07** — Items **follow their trace**: the retention sweep that deletes a trace deletes its items in the same job, user-data erasure (spec 005 #7) deletes the items of the erased traces, and project purge takes everything. Deleting a queue takes its items and nothing else — the scores written while annotating stay | An item is a pointer, and a pointer to a deleted trace is a desk showing an empty page. Scores stay because they are the product of the work, attached to the trace, not to the queue (spec 003: retention on the score's own timestamp). |
| 4 | **2026-09-07** — Two ways in: **`POST /api/v1/queues/{name}/items`** with one `{trace_id, observation_id?}` or an array (all-or-nothing, `{"ids": […], "added": N, "existing": M}`), and **`POST /api/v1/queues/{name}/items/from-traces`** taking **the trace listing's filters as query parameters** (spec 004, spec 012 — the same names, the same validation, `400` on an unknown one) plus `limit` (1–1000, default 100): the newest `limit` matching traces are added as trace items, answer `{"matched": total, "added", "existing", "capped": bool}` (owner decision 2026-09-07: manual plus by filter) | The filter is the vocabulary the listing already has and the CLI already prints; reusing it means "queue what I am looking at" is one call with no second grammar. The cap is what keeps one request from enqueuing a month; `capped` says so, and a second call with `to=` set at the oldest added continues. Newest first because the listing is newest first and a reviewer's question is usually about now. |
| 5 | **2026-09-07** — **`GET /api/v1/queues/{name}/next?annotator=NAME`** hands out the item to work on and **claims** it: the annotator's own unexpired claim if one exists (resume), else the oldest `pending` item whose claim is absent or expired, marked `claimed_by = NAME, claimed_until = now + 10 min`. `{"item": null, "pending": N}` when nothing is claimable; `pending` counts what is claimed by others. Any completion, skip or reopen clears the claim; reading an item does not claim it (owner decision 2026-09-07: soft claim with TTL) | Two people at one desk should not review one trace twice by accident, and a claim that expires is what keeps an abandoned tab from blocking an item for ever. Ten minutes is longer than a review and shorter than a coffee; it is a constant, documented, not a knob. Resume-first because a reload must not hand the reviewer a different trace mid-verdict. |
| 6 | **2026-09-07** — The **annotator** is a name the client sends (`annotator=` on `next`, in the body of complete/skip/reopen; 1–200 chars); the interface asks for it once and keeps it in the browser (owner decision 2026-09-07). It is written to `completed_by`/`claimed_by` and, by the interface, into every score's `metadata` as `{"source": "annotation", "queue": NAME, "annotator": NAME}` | The store has no users and this spec does not invent them; a name typed once is what a small team needs to read "who said this" and nothing it could not fake by other means. `source: "annotation"` beside spec 022's `"web"` keeps the chip on the trace honest about where a verdict came from. |
| 7 | **2026-09-07** — **`POST …/items/{id}/complete`** `{annotator}` succeeds only when **every config the queue names has a score on the item's target** — a score with that `name` on that `trace_id` and, for an observation item, that `observation_id`, whoever wrote it. Otherwise `409 {"error": …, "missing": ["tone"]}` and nothing changes. Completing a completed item is `409` naming who completed it; **`…/skip`** `{annotator, reason}` marks it skipped with the reason; **`…/reopen`** `{annotator}` returns a completed or skipped item to `pending` (owner decision 2026-09-07: all configs) | The queue promised a shape, and *completed* must mean the shape was filled — checked where the scores are, by the server, so the CLI and the desk agree. "Whoever wrote it" because a judge's verdict already on the trace is a verdict; the reviewer sees it prefilled (Decision 12) and confirms or edits. The second completion is refused rather than absorbed so a race between two reviewers is visible to the one who lost it. |
| 8 | **2026-09-07** — Listings: **`GET /api/v1/queues`** returns every queue in name order with `{name, description, score_configs, counts: {pending, completed, skipped}, created_at, updated_at}` — no paging (spec 003's reasoning for configs); **`GET …/items?status=&annotator=&limit=&cursor=`** in `seq` order (oldest first, spec 016 #19's ascending walk), cursors both ways, `count`; **`DELETE /api/v1/queues/{name}`** with spec 005 #8's dry run (`{"would_delete": {"items": N}}`, note: *scores stay*) and `?confirm=<name>`; **`DELETE …/items/{id}`** plain | A project has as many queues as it has review programmes. Items page because a queue filled by filter is a thousand rows. Deleting a queue is the act with a blast radius; deleting one item is a row a re-add recreates. |
| 9 | **2026-09-07** — **CLI**: `queues ls`, `queues put <name> --config N…  [--description D]`, `queues rm <name> [--confirm]`, `queues add <name> (--trace ID [--observation ID] \| --from-traces [listing filters] [--limit N])`, `queues items <name> [--status S] [--annotator A] …`, `queues next <name> --annotator A`, `queues complete\|skip\|reopen <name> <item> --annotator A [--reason R]`. **MCP**: `list_queues`, `get_queue_items` — reads only (spec 004 #16, #17) | Three clients, one surface (spec 004 #1); the desk's every step has a command so a script can annotate with a judge model through the same door. |
| 10 | **2026-09-07** — *Queues* is the **fourth child of the *Evals* section** (spec 016 #1): `/queues` — name, description, configs as chips, a progress bar (completed / total, skipped hatched), *New queue* (dialog: name, description, configs multi-select from the project's configs), row → `/queues/{name}` | A queue is an eval noun — the design lists it beside datasets and runs — and the section was made to hold it. |
| 11 | **2026-09-07** — **`/queues/{name}`**: header (description, configs, progress, *Start annotating*, *Delete* through `ConfirmCard`); the items table (shared listing, `seq` ascending): seq, target (trace name, observation name when set), status chip, by, when, skip reason; `?status=` filter; row → the trace in the peek panel (spec 016 #12's drill-down, the observation opened when the item names one); *Reopen* / *Remove* on a row | The manager's screen: what is done, by whom, what was skipped and why. The peek is the trace because that is what the item points at. |
| 12 | **2026-09-07** — **The desk, `/queues/{name}/annotate`**: on first use a dialog asks the annotator's name (kept in the browser, changeable from the desk header). The page calls `next`, shows the item's trace in `TraceDetail` (the observation panel opened when the item names one) on the left, and on the right the **form**: one control per config in queue order — the control spec 022's dialog builds per type, **extracted into a shared `ScoreControl`** — prefilled from the scores already on the target, a comment per score, *Complete & next*, *Skip…* (reason), *Later* (releases the claim and takes the next). Saving posts the scores that changed (new ones created, existing ones re-posted by id with Decision 6's metadata), then `complete`; a `409` with `missing` marks those controls. Progress *12 of 40* in the header; when `next` returns nothing, the done state with the pending-by-others count and a link back | The desk is the point of the spec: read, judge, next, with nothing to navigate. Prefilling from existing scores is Decision 7's "whoever wrote it" made visible — the reviewer confirms a judge rather than repeating it. `ScoreControl` is extracted rather than copied because spec 022's control already encodes the config-to-control rule once. |
| 13 | **2026-09-07** — ***Add to queue*** on the trace header and the observation panel (a select of queues; `POST …/items` with one target; the answer *added* or *already in the queue*); on the **traces listing** an *Add to queue…* action beside the filter bar that names the current filters' match count (the listing already holds it, spec 009) and the cap, and calls `from-traces` with the page's filters; the answer shows `added`/`existing`/`capped` | The two gestures the owner asked for, each at the surface where the choice is made: the reader at a trace, the manager at a filtered list. The count is shown before the call so a filter that matches ten thousand is seen before the cap is hit. |
| 14 | **2026-09-07** — The application-line budget is **16,000** (spec 023 #11); this spec lands under it, the PR reports the number per screen, and the `ScoreControl` extraction counts as a saving against it | Two screens and a desk on ~1,250 lines is tight; the extraction is where the lines come from. |
| 15 | **2026-09-08** — The CLI spells the confirmation flag **`--yes`**, not the `--confirm` Decision 9's line wrote: `queues rm <name> [--yes]` | Every destructive command in this binary already asks the same way — `datasets rm`, `prompts rm`, `projects rm`, `keys rm`, `users rm-data` — through one `destructive` helper that shows the server's dry run and then sends the server's own echo. `queues rm --confirm` would be one command in the binary using a different word for that, and `--confirm` reads as "here is the echo" where it is in fact "I have read the preview". |
| 16 | **2026-09-08** — ***Later*** **releases the claim and leaves the desk**, and **`reopen` on an item that is already pending is not refused**: what it does to one is clear the claim | Decision 12 asks *Later* to release the claim and take the next, and Decision 5's `next` cannot do the second half: it resumes the annotator's own claim first and otherwise hands out the *oldest* claimable item, so releasing this one and asking again returns it. Something has to give, and it is the "next": the reviewer who wants a different trace picks one from the queue page, and the claim goes so nobody waits out its ten minutes. The mechanism is `reopen` because #5 already says every completion, skip or reopen clears the claim — the alternative, a per-annotator deferral list on the server, is a bigger idea than this spec has (owner decision 2026-09-08: accepted). |
| 17 | **2026-09-08** — The application-line ceiling is **18,000** (owner decision 2026-09-08), superseding #14 and spec 023 #11. The gate stays a warning, and the PR still reports the number per screen | Measured, not estimated: this spec came to ~1,750 lines against #14's ~1,250, and the saving #14 expected from extracting `ScoreControl` was not there — it cost **+34**, because a component boundary buys props, types and a header of its own, and the second consumer that would have paid for it arrived in the same spec that made it. The quality trends are still on the design's list, so the ceiling is raised once for the known set rather than per spec (spec 023 #11's own reasoning). The revision the warning asked for is a PR of its own: the traces and sessions listings are twins at ~400 lines each and have never been read side by side. |

## Data contract (schema 0014)

```sql
CREATE TABLE annotation_queues (
    project_id    TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    score_configs TEXT NOT NULL,              -- JSON array of config names, in order
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    PRIMARY KEY (project_id, name)
) STRICT;

CREATE TABLE annotation_items (
    project_id     TEXT NOT NULL,
    queue          TEXT NOT NULL,
    id             TEXT NOT NULL,             -- 32 hex, server-generated
    trace_id       TEXT NOT NULL,
    observation_id TEXT,                      -- NULL = the trace itself
    status         TEXT NOT NULL,             -- pending | completed | skipped
    seq            INTEGER NOT NULL,
    added_at       INTEGER NOT NULL,
    claimed_by     TEXT,
    claimed_until  INTEGER,
    completed_by   TEXT,
    completed_at   INTEGER,
    skip_reason    TEXT,
    PRIMARY KEY (project_id, id),
    FOREIGN KEY (project_id, queue) REFERENCES annotation_queues(project_id, name) ON DELETE CASCADE,
    UNIQUE (project_id, queue, trace_id, observation_id)
) STRICT;
CREATE INDEX idx_annotation_items_next  ON annotation_items(project_id, queue, status, seq);
CREATE INDEX idx_annotation_items_trace ON annotation_items(project_id, trace_id);
```

`seq` is `MAX(seq) + 1` per queue inside the write transaction (spec 003's
rule for prompt versions). The trace sweep's delete job joins
`idx_annotation_items_trace`; the erasure path does the same. `system`
counts both tables.

## API contract

All under `Authorization` as every write. Every write goes through the
writer queue (spec 001). Unknown fields and parameters are `400`.

| Method | Path | Notes |
|---|---|---|
| `GET` | `/api/v1/queues` | Decision 8 |
| `PUT` | `/api/v1/queues/{name}` | Decision 1; `201` on create, `200` on replace or no-op |
| `GET` | `/api/v1/queues/{name}` | the queue with counts |
| `DELETE` | `/api/v1/queues/{name}[?confirm=]` | Decision 8 |
| `POST` | `/api/v1/queues/{name}/items` | Decision 4 |
| `POST` | `/api/v1/queues/{name}/items/from-traces?…&limit=` | Decision 4 |
| `GET` | `/api/v1/queues/{name}/items` | Decision 8 |
| `GET` | `/api/v1/queues/{name}/next?annotator=` | Decision 5 |
| `GET` | `/api/v1/queues/{name}/items/{id}` | one item |
| `POST` | `/api/v1/queues/{name}/items/{id}/complete` | Decision 7 |
| `POST` | `/api/v1/queues/{name}/items/{id}/skip` | Decision 7 |
| `POST` | `/api/v1/queues/{name}/items/{id}/reopen` | Decision 7 |
| `DELETE` | `/api/v1/queues/{name}/items/{id}` | Decision 8 |

`openapi.json` + `schema.d.ts` in one commit; router ↔ openapi parity.
`docs/annotation.md` walks the loop (create, fill by filter, annotate
from the desk and from the CLI, read the results as scores with
`metadata.queue`); `docs/scores.md` cross-links the `annotation` source.

## Application contract

| Route | Screen |
|---|---|
| `/queues` | Decision 10. Empty state: the `queues put` line and *New queue*. |
| `/queues/{name}` | Decision 11. |
| `/queues/{name}/annotate` | Decision 12. The desk keeps `?item=` in the URL so a reload resumes the claim. |

`TraceDetail` header and `ObservationDetail` gain *Add to queue*; the
traces listing gains *Add to queue…* (Decision 13). `Sidebar` gains the
child. The API client gains one method per endpoint; `QUEUE_ITEM_FILTERS`
gets the openapi parity test (spec 016 #13). `ScoreControl` is extracted
from spec 022's dialog and used by both. Both themes; console clean;
375 px never scrolls the page (the desk stacks trace over form).

## Testing

- **Go, store/API**: queue put/replace/no-op and the missing-config `400`;
  item add single, batch all-or-nothing, dedupe with `existing`, a target
  that does not exist; `from-traces` with each listing filter family
  (`environment`, `tag`, `status`, `from`/`to`, `q`), `matched` vs
  `added`, `capped` at `limit`, `400` on an unknown filter; `next`: resume
  own claim, skip others' unexpired claims, take an expired one, `item:
  null` with `pending` counting others' claims; complete with every score
  present, `409` with `missing` when one lacks (trace item ignores
  observation scores of the same name and vice versa), second completion
  `409` naming the annotator; skip, reopen, delete; items listing order,
  filters, cursors both ways at `limit=1`, `count`; queue delete dry run,
  wrong echo, confirm — scores remain; the sweep deletes items of swept
  traces and no others; erasure deletes items of the user's traces;
  project purge leaves no rows; `system` counts.
- **CLI / MCP / openapi**: parity; `queues next` + `complete` round trip;
  `list_queues` and `get_queue_items` byte-equal to the endpoints.
- **Vitest**: sidebar child; the New-queue dialog's gate (name grammar, at
  least one config); the desk form's completeness rule mirrors Decision 7
  client-side; `ScoreControl` per type (moved tests, not new ones);
  `QUEUE_ITEM_FILTERS` parity; the listing's *Add to queue…* names the
  count and disables above the cap with the reason.
- **e2e** (`queues.spec.ts`, own project, spec 016 #18): put two configs
  and a queue over them; add one trace by hand and the rest by filter
  from the listing → the queue page shows the progress and the items;
  the desk asks for a name, shows the trace and two controls, refuses
  *Complete* until both are set, completes, moves to the next, *Skip* with
  a reason, done state; the completed item's scores appear on the trace
  header with the `annotation` source; reopen from the queue page; delete
  the queue through the dry run — scores still on the trace; 375 px.
- **Mutations** (table in the PR): the completeness check ignoring
  `observation_id`; `next` handing out another annotator's live claim;
  dedupe dropped (`added` counts twice); the sweep leaving items behind;
  the desk's client-side gate removed (the server's `409` must then be
  shown at the controls).
- **Lines and bytes**: `make ui-lines` under 16,000, per screen;
  `dist` under spec 015 #10's ceiling.
- **Chrome (DoD)**: the three routes and both *Add to queue* gestures on
  the demo corpus, both themes, 375 px, console clean, screenshots in
  the PR.

## Edge cases

- **A queue whose config was deleted** (spec 003: deleting a config
  touches no score): the queue keeps the name; the desk shows the control
  as a free-typed score of that name with a note; `PUT` again to drop it.
- **An item whose trace has not arrived**: the desk shows the trace page's
  not-found state with *Skip* and *Later* available; `complete` is `409`
  with every config missing.
- **An observation item whose observation is not in the trace** (a wrong
  id): the desk shows the trace with a note; scores go to the observation
  id as given.
- **The annotator's claim expires while the form is open**: the save
  still posts scores (they are scores), and `complete` succeeds if nobody
  else completed it meanwhile — the claim gates `next`, not completion.
- **`from-traces` with no filter**: allowed — the newest `limit` traces of
  the project; the listing's action shows the project's total as the
  match count.
- **Two queues holding the same target**: independent items; the scores
  are shared (same name, same target), so completing the second may need
  nothing new — the desk prefills and *Complete* is immediately enabled.

## Config additions

None (the claim TTL is a documented constant).

## Out of scope

Sampling rules at ingest; agreement or throughput statistics per
annotator; queues over sessions, dataset items or runs; assignment of
items to named annotators; notifications; a queue's scores exported as
a dataset (add to dataset from the trace already exists, spec 016 #8).
