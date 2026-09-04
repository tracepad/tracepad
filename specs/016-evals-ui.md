# Spec 016 — Evals in the web interface: datasets, runs and the comparison

**Status:** IN PROGRESS
**Sprint:** September 2026

> Spec 014 gave the store its three eval nouns and a comparison that the
> CLI and MCP can read. A person still has no screen: the cases live in a
> JSONL file, a run is an id in a terminal, and "did this change make it
> better" is a table printed in monospace. This spec adds the screens — an
> *Evals* section with datasets, runs and score configs, an item editor
> over spec 015's surface, a run page, and the comparison as the screen it
> was designed for — plus the one gesture that closes the loop from
> production to test set: *add this observation to a dataset*.

---

## Overview

Deliverables, in two PRs (the second flips the status):

**PR 1 — read.** The *Evals* sidebar section (Decision 1). `/datasets`,
`/datasets/{name}` (items and runs), `/runs` (Decision 2's new endpoint),
`/runs/{id}` with its summary and items, `/runs/{a}/compare/{b}`,
`/score-configs` read-only. Item, run-item and compare peek panels. The
API client grows the read methods. `docs/ui.md` gains an *Evals* section.

**PR 2 — write.** The item editor (new, edit, archive) over `JsonEditor`;
*Add to dataset* on the observation panel; dataset deletion through
`ConfirmCard`; run deletion; score-config create/replace/delete. The API
client gains `PUT`. `docs/ui.md` and `docs/datasets.md` (the UI recipe)
updated. The status flips.

Not here: creating or finishing runs from the UI (Decision 9), annotation
queues, quality trends, a diff view of two payloads, dataset renaming.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-03** — The sidebar gains its first **section**: *Evals* with three children — *Datasets* (`/datasets`), *Runs* (`/runs`), *Score configs* (`/score-configs`) — rendered as a labelled group in the existing `SECTIONS` table (`Sidebar.svelte:16-21`), the label not a link, children indented, active state per child (owner decision 2026-09-03) | Three flat items would put a project's seven-item nav on one level where four of them are one topic; a section says what they have in common and leaves room for the annotation queue and trends the design lists after them (§12). The table stays data: a group is `{ label, children: [...] }`, the `<ul>` nests once, and `aria-current="page"` (`:70`) keeps meaning what it means. On the narrow layout the same list renders; no drawer or accordion. |
| 2 | **2026-09-03** — A **project-wide runs listing**, `GET /api/v1/runs`, is added to the read API: newest first, filters `dataset` and `status`, cursors both ways, rows as `GET /datasets/{name}/runs` returns them. `tracepad runs ls` without a dataset argument and MCP `list_runs` without `dataset` read it. A dated amendment to spec 014's API contract | *Runs* as a sidebar item is "what ran lately, whatever the set", and spec 014 only lists per dataset; fanning out per dataset from the client would be logic in a client (spec 004 #1) and N requests for one table. The row shape already carries `dataset`, so it is one query over `idx_dataset_runs_dataset`'s sibling — an index over `(project_id, created_at DESC, id DESC)` is added with it. The CLI and MCP changes make the three clients agree again (spec 004 #17's rule that a tool is one endpoint). |
| 3 | **2026-09-03** — Every listing here — datasets, items, runs, run items, compared items — is the shared **listing loader** (spec 010) with `UrlSpot` for pages and `Walk` for the ones with a peek panel; nothing is paginated by hand | Spec 010 exists so that a listing defect is one defect. Five new tables written any other way would be five new places for the cursor bugs spec 009 fixed to return. |
| 4 | **2026-09-03** — The dataset page has two tabs, *Items* and *Runs*, in the URL (`?tab=`), and a **version selector** in its header that rewrites the items tab to `?version=V` (Decision 7 of spec 014). The default is the current version; an older version renders read-only with a banner naming it | Items and runs are two listings of one thing with different columns and different peeks; two tabs are cheaper to learn than two pages. The version selector is how the append-only history (spec 014 #5) becomes visible without a second screen, and read-only because an edit *is* a new version at the head — editing "version 3" would mean something the store cannot do. |
| 5 | **2026-09-03** — The item editor is a **full page** (`/datasets/{name}/items/new`, `/datasets/{name}/items/{id}/edit`), not a peek panel: three `JsonEditor` panes — *Input*, *Expected output*, *Metadata* — with the parse state of each, *Save* enabled when input parses and nothing else is invalid, and the answer shown as *saved as version V* or *unchanged* (spec 014 #6). Save posts one item with its id (a re-post is an edit, spec 014 #9) | Three editors need width, and the peek panel is a reading surface half a screen wide (spec 008). A page also has an address, which is what *Add to dataset* (Decision 8) links to. "Unchanged" is shown rather than hidden because the store's own answer is the only proof the author's edit was a no-op, and a silent save that changed nothing is the confusion spec 014 #6 exists to prevent. |
| 6 | **2026-09-03** — Destruction follows spec 005 #8 exactly where the server does: **dataset deletion** goes through `ConfirmCard` with the server's dry run (`DatasetDeletion`: items, runs, pinned traces) and the name echoed. **Run** and **score-config** deletion and **item archiving** are a plain confirmation dialog (bits-ui `AlertDialog`) naming the consequence — for a run, *its traces return to the retention window* — because the server takes no `confirm` for them (spec 014 #20) and an archived item can be re-posted | The echo ceremony is for the act with a blast radius and the UI must not invent one where the API has none, or omit one where it has. The dialog for the cheap deletions is what keeps a mis-click from deleting a run, which is still a row someone wanted; `ConfirmCard`'s dry-run round trip would be theatre for an endpoint that has no preview. |
| 7 | **2026-09-03** — The run page **polls** while the run is `running` (the traces listing's 5 s live cadence, `traces/+page.svelte:24`, same visibility gate) and stops at `finished`/`failed`; a run's age is shown beside `running` | A run in progress is the one eval state a person watches, and the store never guesses completion (spec 014 #8), so the screen has to show the harness's progress as it lands. Reusing the live gate means one rule for "the tab is hidden". |
| 8 | **2026-09-03** — *Add to dataset* is a button on the **observation panel** (any observation; the root is the default the trace header offers) that opens the item editor page prefilled: the observation's `input` → *Input*, `output` → *Expected output*, `source_trace_id`/`source_observation_id` set, the dataset chosen on the editor page from a select of the project's datasets (`?dataset=` preselects). Payloads are fetched **whole** through `/observations/{id}/io`, never from a preview (owner decision 2026-09-03) | The golden case is what the model *should* have said, which is usually the output with a correction — so the flow lands in the editor, not in a "saved" toast (owner decision: edit before save). Any observation, because in an agent trace the case is often one generation, not the whole run; the root as default because a plain trace has one. A preview is a cut document (spec 004 #2), and a cut document saved as a test case is a wrong test case. |
| 9 | **2026-09-03** — Runs are **not created or finished from the UI**; the run page shows the two attributes and a `curl` for the harness instead of a *New run* button. Score configs **are** editable in the UI (name, type, direction, min/max or categories, description) through a form that `PUT`s the whole config | A run is opened by the process that will stamp its traces (spec 014 #1, #3); a run created by hand has nothing to fill it and would sit `running` for ever. A config is a declaration a person makes once and a form is the right place to make it; the form `PUT`s because the endpoint is declarative (spec 014 #17), so the same form creates and edits. |
| 10 | **2026-09-03** — The comparison page reads `GET /runs/{a}/compare/{b}` and **contains no comparison logic**: the header block (per-name means, deltas, improved/regressed/same; cost, latency, errors; the metadata diff; models and prompts of both) is rendered from the response; the items table shows `in` and the per-name verdicts as chips; a *changed only* toggle (`?changed=1`) hides `same` rows client-side within the page; row → peek with *Expected*, *Output A*, *Output B* as three `JsonView`s (budgeted, loadable) and the per-name pair. A *swap* link goes to `/runs/{b}/compare/{a}` | Spec 014 #18 put the comparison on the server precisely so the UI could not disagree with the CLI; the screen is a rendering. The toggle filters the page, not the query, because the endpoint has no such filter and adding one for a toggle would be the comparison logic creeping back; the count of `same` rows is in the header either way. |
| 11 | **2026-09-03** — Two ways in: the run page's **Compare with…** (a select of the dataset's other runs, newest first) and **two checkboxes** in any runs table, with a *Compare* button that is disabled — with the reason as its title — unless exactly two runs of one dataset are ticked | The first is the common gesture (this run against the baseline); the second is how a person finds two runs in a list without opening either. Disabling with a reason instead of letting the server 400 (spec 014 #18 refuses different datasets) keeps the mistake from becoming a page. |
| 12 | **2026-09-03** — A run item's attempts open the trace **in the peek panel with one level of drill-down**, the pattern the session panel already has (`sessions/+page.svelte:275-284`): the item body is replaced by `TraceDetail` and a breadcrumb leads back | The reader's question at an attempt is "what did it actually say and why", which is the trace; the session panel already solved "a list whose rows are traces" and its breadcrumb is the same affordance. |
| 13 | **2026-09-03** — The runs and compared-items listings declare their filters as `RUN_FILTERS` and get the same **openapi parity test** as `TRACE_FILTERS` (`traces.test.ts:36-40`); the score-config form's `direction` and `data_type` options are read from `schema.d.ts`'s enums, not typed twice | Spec 014's PR 2 already paid for that test once (the filter bar gained *Run*/*Item* because the bar must offer what the API accepts); a new listing without it would be the first to drift. |
| 14 | **2026-09-03** — The application-line budget is **14,000** (spec 015 #9) and this spec must land under it with spec 015; the PR reports the number per screen | The number was raised for these screens; landing over it would mean the estimate was wrong, and the per-screen figure is how the next spec learns what a screen costs. |
| 15 | **2026-09-03** — Empty states teach the CLI: a dataset with no items shows *New item* and the `tracepad datasets push` line; a run with no traces shows the two attributes to stamp (`tracepad.run_id = <id>`, `tracepad.item_id`) and the `finish` command; a project with no datasets shows the whole loop from `docs/datasets.md` in six lines | The UI is one of three clients (spec 004 #1) and the loop is driven by the harness; a person landing on an empty screen is usually the one about to write that harness. |

| 16 | **2026-09-04** — The runs tables carry **no coverage column**; the version the run pinned and its status stand where the Application contract lists `coverage` (found implementing PR 1) | The rows of both run listings are the run object without `summary` (spec 014, API contract: "a page of runs is for choosing one"), and coverage is a number only the summary has. Filling the column would be one `GET /runs/{id}` per row — the fan-out Decision 2 refused for the listing itself — or a summary on the listing, which spec 014 declined so that choosing a run would not cost what reading one costs. The run page shows the coverage one click later, computed once, by the server. |
| 17 | **2026-09-04** — Where the peeks get their payloads (found implementing PR 1). The **run-item** peek renders the listing's row — the item view has no single-item address — and loads a cut `expected_output` whole from `GET /datasets/{name}/items/{id}?version=V` (budget-exempt, spec 014 #19) and a cut attempt `output` from `/observations/{id}/io` through the marker's pair; a row the page no longer holds says so. The **compare-item** peek reads the item at the version the run that had it pinned, and each side's output as the root observation of the **newest** trace `GET /traces?run_id=&item_id=` names, through `GET /traces/{id}?expand=io`, with a link to every attempt when there were several. The run page's item rows show **every attempt's value** per score name, in order, rather than a mean | The comparison's rows carry scores and verdicts and nothing else, and the endpoints spec 014 shipped are the whole vocabulary a client has: the trace filters spec 014 #2 added exist for exactly this join, and the trace read is the one place a payload arrives with the marker `Payload` already knows how to load. The newest attempt is the one a reader would quote (spec 014 #30's reasoning). A mean over N attempts is the server's rule (spec 014 #18) and would be client arithmetic here (spec 004 #1); listing the values is what the row can honestly say. |
| 18 | **2026-09-04** — The e2e suite seeds a **project of its own** (`createProject`) rather than the default one the Testing section names: fixture 009 is delivered there before the run it names exists and again after, and the second run the comparison needs is exported as a minimal OTLP request built in the suite (found implementing PR 1) | The default corpus is counted by the pagination and stats suites — seven traces, asserted by number — and a second run's traces moved those numbers in both Playwright projects. What the Testing section wanted is kept whole: the client-supplied id fixture 009 stamps, and a re-delivery that resolves the link after the run opens (spec 002 #6, spec 014 #3); it happens in a project the other suites do not read. The comparison needs traces on both sides, and the fixture carries one run. |
| 19 | **2026-09-04** — The walk of spec 010 gains one flag, `ascending`, for a listing read oldest first — a dataset's items and a run's, which walk `seq` (spec 014 #21). The pure part of `$lib/peek` is untouched: the walk hands it the page reversed and turns its steps round (found implementing PR 1) | Decision 3 puts every listing here on the shared loader, and the pure part orders newest first by key. Inverting each caller's key would have been a trick written three times; a flag on the walk is written once, beside the code that assumes the order, with a test. |
| 20 | **2026-09-04** — In PR 1 the item peek's **⤢** points at the dataset view with the item open (`?version=V&peek=`), and the run-item and compare-item peeks at the item in its dataset at the run's version; PR 2's editor page becomes the item peek's canonical link when it exists (found implementing PR 1) | The panel's ⤢ must lead somewhere that renders; the editor route is PR 2's, and a link into a route that does not exist yet is a dead link in a shipped PR. |

## Application contract

Routes (all behind the layout guard, `+layout.ts:20-36`):

| Route | Screen |
|---|---|
| `/datasets` | Listing: name, description, version, items, runs, updated. Row → `/datasets/{name}`. Header: *New dataset* (PR 2: name + description, `PUT`). |
| `/datasets/{name}` | Header: description, `version` selector (Decision 4), *New item*, *Delete* (PR 2). Tabs: **Items** — seq, id (short), input and expected previews (first 120 chars of compact JSON), row version; row → item peek (three `JsonView`s, source pair as links, *Versions* list, *Edit* / *Archive* in PR 2). **Runs** — name, status, created, coverage, checkboxes (Decision 11). |
| `/datasets/{name}/items/new`, `…/{id}/edit` | Item editor (Decision 5). PR 2. |
| `/runs` | Project-wide listing (Decision 2): dataset, name, status, created, coverage; filter bar with `dataset` (select) and `status`; checkboxes. |
| `/runs/{id}` | Header: name, `dataset@version` link, status (+ age while running, Decision 7), created/finished, `error`. Cards: coverage (total/covered/missing/unknown), traffic (count, attempts max, errors, cost, p50/p95), **scores** table (name, type, direction, count, mean/min/max or a distribution bar), models, prompts, metadata (key/value). *Compare with…* (Decision 11), *Delete* (PR 2). Items listing from `/runs/{id}/items`: seq, id, attempts, per-name value, `?unknown=true` toggle; row → run-item peek: expected (`JsonView`), attempts with trace link, cost, latency, errors, scores; attempt → trace drill-down (Decision 12). |
| `/runs/{a}/compare/{b}` | Decision 10. |
| `/score-configs` | Table: name, type, direction, bounds/categories, description; PR 2: form (Decision 9), delete. |

The observation panel (`ObservationDetail`) gains *Add to dataset*
(Decision 8, PR 2). `Sidebar` gains the section (Decision 1). The API
client gains one method per endpoint in the spec 014 API contract plus
`GET /runs`, and the `PUT` method in its `Request` union
(`client.svelte.ts:332-338`).

Every table is the shared listing; every peek is `PeekPanel` with its
`fullHref` pointing at the row's page; every JSON is spec 015's `JsonView`;
every destructive action is Decision 6. Widths: tables scroll inside their
container at 375 px, never the page. Both themes; console clean.

## Server contract (Decision 2)

`GET /api/v1/runs` — filters `dataset` (exact name), `status`; `limit`,
`cursor`, `direction`, `count` as every listing; newest first by
`(created_at, id)`. Response `{"runs": [run objects without summary],
"next_cursor", "prev_cursor", "total"?, "total_capped"?}`. Schema 0011:
`CREATE INDEX idx_dataset_runs_created ON dataset_runs(project_id,
created_at DESC, id DESC)`. `openapi.json`, `docs/api.md`, `docs/cli.md`
(`runs ls` without an argument), `docs/mcp.md` (`list_runs` with optional
`dataset`), and spec 014's API contract amended with a dated note.

## Testing

- **Vitest**: sidebar renders the section and marks the active child;
  `RUN_FILTERS` parity with `openapi.json`; the compare toggle hides `same`
  rows and the header counts are untouched; the checkbox rule enables
  *Compare* only for two runs of one dataset; the item editor's Save gate
  (input must parse, others may be empty) and its *unchanged* rendering;
  the score-config form refuses `direction` on categorical/text and
  requires it on numeric/boolean (mirrors spec 014 #16 client-side, with the
  server as the oracle).
- **Go**: `GET /runs` filters, cursors both ways at `limit=1`, the index in
  `EXPLAIN QUERY PLAN`, parity of openapi ↔ router; CLI `runs ls` without
  an argument; MCP `list_runs` without `dataset` byte-equal to the endpoint.
- **e2e** (`evals.spec.ts`; seeding: `createProject` is not enough here —
  the spec **creates a dataset, its items and a run with the client-supplied
  id that fixture 009 stamps** through the API in the default project, then
  re-posts fixture 009 so the link resolves on re-delivery, spec 002 #6):
  the section appears and navigates; the dataset page lists items and
  switches versions; the run page shows the summary the API returns and
  the item peek drills into the trace and back; the compare page renders
  header and verdicts, the toggle hides `same`, swap flips the URL; *Add to
  dataset* lands in the editor prefilled with the whole payload and saves
  a new version; dataset delete shows the dry run and refuses a wrong echo;
  375 px for every screen.
- **Lines and bytes**: `make ui-lines` under 14,000 with the per-screen
  breakdown; `dist` under spec 015 #10's ceiling.
- **Chrome (DoD)**: every route above on a seeded corpus, both themes,
  375 px, console clean, one screenshot per screen in the PR.

## Edge cases

- **Dataset with hundreds of versions**: the selector is a number input
  with the current as max, not a dropdown of every version.
- **Run of a dataset version that no longer exists as head**: the run page
  links `dataset@version` to the items tab at that version (read-only).
- **Compare where one run is still running**: allowed (spec 014 edge
  cases); the header shows both statuses and the page polls per Decision 7
  while either runs.
- **Item peek for an archived item** (reached from an older version): no
  *Edit*, an *archived at version V* note.
- **`Add to dataset` on an observation with no output**: *Expected output*
  starts empty; Save still works (spec 014 #4: only `input` is required).
- **Project with no datasets and the sidebar section**: the section shows;
  `/datasets` shows Decision 15's empty state.

## Config additions

None.

## Out of scope

Annotation queues (a later spec fills the source pair from a queue);
quality trends over runs (charts read spec 013's rollup when it learns
scores); a textual diff of two outputs; bulk item import from the browser
(the CLI's `push` is the import); run creation/finish from the UI
(Decision 9); dataset rename; per-user compare presets.
