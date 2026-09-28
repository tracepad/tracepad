# Spec 014 — Datasets, runs & score configs: evals that live in the trace store

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> Today an eval is a directory of JSON files, a runner per harness, and a
> markdown report that someone compares to last week's by eye. The traces the
> run produced already land in this store; the cases it ran and the numbers it
> got do not, so the one question evals exist to answer — *did this change
> make it better* — is answered outside the tool that holds the evidence.
> This spec adds the three missing nouns: a **dataset** of versioned test
> cases, a **run** that groups the traces one pass over a dataset produced, and
> a **score config** that pins what a score's name means. The harness stays the
> client's; the store links, keeps, summarizes and compares.

---

## Overview

Deliverables, in two PRs (the second flips the status):

**PR 1 — the nouns and the link.** Schema 0010: `datasets`, `dataset_items`
(append-only, one version clock per dataset), `dataset_runs`, `score_configs`,
and two nullable columns on `traces` — `run_id`, `item_id`. JSON API under
`/api/v1` for datasets, items, runs and score configs. Ingest claims
`tracepad.run_id` / `tracepad.item_id` into the trace columns. The retention
sweeper spares traces that belong to a live run. `POST /api/v1/scores`
validates a score against its name's config when one exists.
`GET /api/v1/system` counts the new tables and reports the pinned and orphan
run traces. Docs:
`docs/datasets.md` (new), `docs/scores.md`, `docs/retention.md`,
`docs/ingest.md`, `docs/api.md`, `openapi.json`.

**PR 2 — reading it back.** `GET /api/v1/runs/{id}` with the run's summary,
`GET /api/v1/runs/{id}/items` with per-item attempts, and
`GET /api/v1/runs/{a}/compare/{b}`. CLI groups `datasets`, `runs`,
`score-configs`; six read-only MCP tools. Docs: `docs/cli.md`, `docs/mcp.md`.

Not here: a UI for any of it (spec 016), annotation queues, quality trends
over the rollup, the server executing anything, `langfuse.experiment.*`
compatibility.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-03** — The store never executes an eval. The client's harness fetches the items, runs its own function, exports the traces over OTLP as it does in production, and posts scores as it does today (spec 003). A run is a **container** the harness opens and closes; the store links, keeps, summarizes and compares (owner decision 2026-09-03) | Executing means holding provider credentials, a queue, a budget gate and a sandbox for someone else's code — the whole of iteration 3's online judge (design §12), pulled forward into a spec about bookkeeping. And it would make the store a second runner beside the nine the brief counts, instead of the place their results finally meet. Every affordance below is reachable with `curl` and one attribute on a span, which is the bar design §3 sets for the agent that writes the harness. |
| 2 | **2026-09-03** — A trace joins a run through two span attributes, `tracepad.run_id` and `tracepad.item_id`, claimed by the mapper into two new trace columns `traces.run_id` / `traces.item_id` — trace-level fields upserted per field like `session_id` (`internal/mapping/mapping.go:271-279`, `internal/store/ingest.go:105`). No link table: the columns **are** the link, and one item may own many traces in one run. **No foreign key** to `dataset_runs`: the columns record what the client said (spec 012 #5, `prompt_name`) | The harness already exports the trace; asking it to also `POST` a link per item is a second call per case that can fail independently and leaves a trace without its run when it does — spec 003 #4's reasoning against target checks, applied to the write side. An attribute rides in the export, inherits its retry, and works from any OTel SDK with no client of ours (design §6.5 is iteration 2's SDK, and this must not wait for it). A separate link table would be the same two ids one join away, with the ingest transaction deciding when to write it; two columns and an index over `(project_id, run_id, item_id)` answer every read below and cost the write path nothing it does not already pay for `session_id`. Many-to-one because repeating a non-deterministic case N times is the ordinary shape of an eval, and a retry after a crash is the ordinary shape of a harness — collapsing to one trace would lose either (owner decision 2026-09-03: all attempts kept, the summary counts them). |
| 3 | **2026-09-03** — A run is created **explicitly** (`POST /api/v1/datasets/{name}/runs`) before its traces arrive, and the harness stamps the returned id. A trace naming a run that does not exist in the project is stored as any trace is, with its columns filled; the ingest transaction counts it and logs once per unknown run id, and `GET /api/v1/system` reports the count (owner decision 2026-09-03) | Creating runs from the first span seen would make a typo in an attribute a new run, and would leave the run's own facts — what was being tried, against which version — nowhere but on every span. Explicit creation is one call the harness makes anyway to learn the dataset version it is about to run (#7), and it is where `metadata` lives. The orphan is not refused because refusing would drop the one artifact that shows what went wrong; it is counted because a harness that stamps a wrong id must find out before it reads an empty run. The cost is one primary-key lookup per trace that carries the attribute, inside the transaction that writes the trace, paid only by eval traffic. |
| 4 | **2026-09-03** — A dataset item is `input`, `expected_output` and `metadata`, each opaque JSON, plus an optional `source_trace_id` / `source_observation_id` naming where the case came from — **strings, not foreign keys** | The store never reads inside `input` or `expected_output`: what a case looks like is the harness's business, and a schema per dataset would make "add a case" a ceremony (owner decision 2026-09-03). The three names are the structure the reading side needs — a run's item view shows expected beside produced — and nothing more. The source pair is what an annotation queue (a later spec) will fill when a production trace is promoted to a golden case; it is not a key because the source lives under retention and may be gone, and a case does not stop being a case when its origin does. Strict decoding (spec 003 #17) still applies to the envelope: an unknown top-level field is a 400. |
| 5 | **2026-09-03** — Items are **append-only** under **one version clock per dataset**: `datasets.version` is an integer that advances by exactly one on every write that changes the item set — a `POST` of one or many items, a `DELETE` of one — and every item row carries the `dataset_version` it was written at. An item's history is its rows; an edit is a new row; a delete is a row with `archived = 1`. "The dataset at version V" is, per item, the row with the greatest `dataset_version ≤ V`, minus the archived ones | An eval result is only evidence if the cases it was measured against can be recovered, and a mutable item silently rewrites every past run's meaning (owner decision 2026-09-03: append-only, as prompts are — spec 003 #10). One clock rather than a version per item because a run needs one number to pin (#7), and "which set of items" is a property of the dataset, not of each item; per-item versions would have to be reassembled into a set anyway. A batch is one tick because it is one change in the author's mind — an import of two hundred cases is not two hundred versions — and because the number the harness reads back (#7) must describe the whole of what it wrote. The version is assigned inside the write transaction, exactly as prompt versions are (`internal/store/prompts.go:117`), so concurrent writers get consecutive numbers with no gaps. |
| 6 | **2026-09-03** — A write that changes nothing writes nothing: an item posted with `input`, `expected_output` and `metadata` byte-equal (after JSON compaction) to its current row creates no row, and a batch in which no item changed does not advance the version; the response says which version the batch landed on either way | The harness that declares its cases at the top of every CI run — the shape #5 exists to support — must be able to re-run without minting a version per run; otherwise the version history is a record of executions, not of edits, and #7's "same version" comparison never fires. Retry-safety follows for free, by the reasoning of spec 003 #3: a re-POST after a lost response is idempotent. Comparison is on the compacted JSON text, the same normalization the store applies before writing (spec 003 #6 keeps metadata inline as TEXT), so key order does not count as a change. |
| 7 | **2026-09-03** — A run records `dataset_version` at creation — the dataset's version at that instant, or an explicit `dataset_version` in the body when the harness ran an older one — and every read of the run resolves its items at that version. The creation response carries it | The version the harness *fetched* and the version it *ran* must be the same number, and the only way to make that true by construction is to hand the number out at the moment the run opens and let the harness fetch by it (`GET …/items?version=V`). A run that pins nothing shows today's items against yesterday's traces the first time someone edits a case. The explicit override exists for the harness that fetched, then created — the order the CLI recipe in `docs/datasets.md` avoids, but a script may not. |
| 8 | **2026-09-03** — A run has a `status`: `running` at creation, then `finished` or `failed` by an explicit `POST /api/v1/runs/{id}/finish` (`{"status": "failed", "error": "…"}` for the latter). The store never infers completion; a run left `running` is reported as such with its age (owner decision 2026-09-03) | Spans arrive late and harnesses die, and the two look the same from inside the store: no new link for a while. Guessing from a timeout would turn a slow judge into a "failed" run and a crashed harness into a "finished" one — the wrong answer in both directions, with nothing the harness can do about it. An explicit close is one call; `running` with a created-at is an honest "the harness has not said". Linking does not stop at finish: a late span of a trace that started inside the run still belongs to it, and refusing would make the summary depend on exporter batching (spec 004 #32's lesson). |
| 9 | **2026-09-03** — Datasets are addressed by **name**, unique per project, with the prompt-name grammar (`^[A-Za-z0-9][A-Za-z0-9._-]*$`, ≤ 200; `internal/server/api.go:42`); runs and items have 32-hex ids — server-generated, or client-supplied and upserted like scores (spec 003 #3) | A dataset is a thing a person names in a CI file and a conversation ("the support golden set"); prompts set the precedent and the URL shape (`/api/v1/prompts/{name}`). Items and runs are many and machine-made; an id the client may supply lets a harness with a natural key (a case file's path, a commit hash) hash it and stay idempotent, which #6 needs. A client-supplied run id makes "create the run" itself retry-safe: a second `POST` with the same id returns the existing run unchanged. |
| 10 | **2026-09-03** — Datasets, items, runs and the traces they own live strictly in **one project**; nothing here crosses a project boundary (owner decision 2026-09-03) | The link is written by the ingest transaction of a key that belongs to one project (#2), so a run must be in that project or the columns point at nothing. The brief's "app and eval in separate projects" is honoured by putting the dataset where the eval traffic goes; the golden case cut from a production trace crosses over as **content** — `input`/`expected_output` copied into the item, the source pair kept as a note (#4) — which is what the case needs anyway once retention takes the original. Cross-project reads would be the first ones in the product outside the admin token (spec 005 #11), for a benefit a copy already provides. |
| 11 | **2026-09-03** — A run's metrics are the **ordinary scores on its traces** (spec 003), aggregated through the columns of #2; cost, latency and errors come from the trace row's aggregates (spec 002 #22). No run-level or item-level score target, no client-written summary | One write path for production and eval scores means one trend line later (design §5.2's `scores(project_id, name, timestamp)` index; spec 013's rollup reads the same rows when quality trends come). A judge that grades a run item is grading the trace that item produced; giving it a second target would make "how did this prompt score this week" a union of two tables. A client-written summary would be a number the store cannot check against the rows it holds, and a compare between two such numbers is a compare of two claims. |
| 12 | **2026-09-03** — The run summary derives the **models** and **prompts** it ran from the traces' own observations — distinct `observations.model` over model calls, distinct `(prompt_name, prompt_version)` (spec 012 #5) — and carries the harness's free-form `metadata` verbatim. Compare diffs `metadata` key by key and lists the two derived sets side by side | What the run *declared* and what it *did* can differ, and the trace is the witness: a harness that says `model: gpt-5` in its metadata while a fallback served half the calls is exactly the case a compare should expose, not hide. Structured `model`/`prompt` fields on the run would be a declaration the store cannot verify, and the observation columns spec 012 promoted were promoted for this. `metadata` stays free because the dimensions of an experiment are the harness's (temperature, retrieval depth, a feature flag) and a schema for them would be wrong the first week. |
| 13 | **2026-09-03** — A trace that belongs to a **live run is not swept** by retention: `expiredTraceIDs` (`internal/store/sweep.go:656`) excludes rows whose `run_id` names an existing `dataset_runs` row of the project. Deleting the run releases its traces to the ordinary window; nothing else does. The retention dry run (spec 005 #8) counts what the sweep would take, so it excludes them too (owner decision 2026-09-03, against the recommendation to snapshot) | A run is the evidence for a number someone will act on, and the interesting run is the old one — the baseline from before the regression. Snapshotting a summary into the run (the spec 013 pattern) keeps the numbers and loses the ability to *look*: the output that scored 0.2, the tool call that failed. The owner's call is that for eval traffic — hundreds of traces per pass, not a million a day — the payloads are worth their disk, and that the size is a choice the operator makes per run rather than a window that quietly amputates the baseline. It is a stated exception to design §5.5's "retention bounds the file": the bound now reads *retention plus what you chose to keep*, and `docs/retention.md` says so in its first paragraph. Deleting a run is the release valve (owner decision 2026-09-03), and `GET /api/v1/system` reports how many traces are pinned so the operator can see the exception's size. Raw batches are **not** pinned: a batch feeds many traces with different fates (spec 005 #6), and raw is the archive, not the evidence — after the raw window a pinned trace keeps its parsed rows and loses its raw body, which `docs/retention.md` already frames as the archive's position. |
| 14 | **2026-09-03** — User-data erasure (spec 005 #7) **overrides the pin**: `DELETE …/users/{user_id}/data` deletes the user's traces whether or not a run holds them, and the run then shows those items as missing | Erasure is a request the operator is legally obliged to honour and the pin is a convenience for measurement; the second cannot outrank the first. An eval trace ordinarily carries no `user_id`, so the case is rare — but a harness that replays production sessions under their real ids is exactly the harness that will receive the request. The dry run names the runs affected so the operator sees the hole before it opens. |
| 15 | **2026-09-03** — `score_configs` arrive in this spec (owner decision 2026-09-03), keyed by `(project_id, name)`, **bound by name**: a score whose name has a config must satisfy it — `data_type` equal, numeric `value` within `min`/`max` when set, categorical `string_value` among `categories` — checked inside the score write transaction as a typed rejection (spec 003 #20's model, `internal/store/scores.go:55`), so a bad item fails its whole batch with a 400 naming it (spec 003 #7). A name without a config is as free as today. Existing scores are never re-validated. No `config_id` column on `scores` (closing spec 003 #8) | The brief wants configs so that "names and scales do not drift between harnesses", and that promise is only kept if the name *is* the binding: a config a client must opt into by id (the reference platform's shape) is drift with extra steps. Binding by name also answers what compare needs — the direction of a score — with one lookup. The check runs in the transaction for the reason spec 003 #20 gives: a config can be replaced between a handler-side read and the commit. Old scores are left alone because a config is a rule for what comes next, not a claim that the past was clean; a listing of violations is a later affordance, not a migration. |
| 16 | **2026-09-03** — A config carries `direction` ∈ `higher` \| `lower` \| `none` — **required** for `numeric` and `boolean`, **forbidden** for `categorical` and `text` — and compare marks an item `improved`/`regressed`/`same` only for names whose direction is `higher` or `lower`; every other name shows `changed`/`same` | Without a sign the server would either guess ("higher is better") and be wrong about every latency and cost score, or stay mute about the one word the compare exists to say. Requiring it where a sign is meaningful makes the config author state it once; forbidding it where it is not keeps a categorical "verdict" from pretending to be an axis. `none` exists for the informational number — a token count, a retrieval depth — that a harness wants typed and bounded but not judged. |
| 17 | **2026-09-03** — Score configs are **declarative**: `PUT /api/v1/score-configs/{name}` creates or replaces the whole config (a re-`PUT` of the same body is a no-op), `DELETE` removes it. No versions, no partial update | A harness declares its configs at the top of a run the way it declares its dataset (#6) — idempotent, re-runnable, in a file under version control. A version history of a config would be a second append-only structure with nothing reading it: a score records the value it was given, and the rule that admitted it is the rule that was current then. Replacing a config changes what is accepted from now on and nothing else (#15). |
| 18 | **2026-09-03** — `GET /api/v1/runs/{a}/compare/{b}` is a **server endpoint**, one call for CLI, MCP and (spec 016) the UI: per score name the two means, the delta and the counts of items that improved, regressed and stayed; cost, latency and errors for both; then the items, paginated, each with its per-name pair and verdict. Runs of **different datasets** are a 400; different **versions** of one dataset compare over the intersection and name the items outside it | The comparison is the product — "did this change make it better" — and spec 004 #1's rule that a client contains no logic makes it a server question by construction: a compare implemented twice (Go for the CLI, TypeScript for the UI) would be the first place the two disagree. An item's value under N attempts is the mean of its attempts (the same rule as the run mean, so the item rows sum to the header); equality is exact, which the docs state, because a tolerance would be a number the store invents. Different datasets have no items in common, so an item-by-item compare cannot mean anything and says so; different versions do, and the honest answer is the intersection with its edges labelled rather than a refusal that would block comparing a run against last month's baseline. |
| 19 | **2026-09-03** — `GET /api/v1/datasets/{name}/items` returns item bodies whole and is **budget-exempt** — the second endpoint after `/observations/{id}/io` (spec 004 #3) — bounded by `limit` (≤ 500) and by `TRACEPAD_MAX_BODY_BYTES` per item at write time. Every other endpoint here is budgeted and marks its cuts as the read API does (spec 004 #2) | The consumer of the items listing is the harness, and a truncated test case is not a smaller test case, it is a different one: a run over cut inputs measures nothing and says nothing about it. The bound is the page and the item size the store already enforces at write. The run's item view (`…/runs/{id}/items`) is for the reader, not the runner — it shows `expected_output` beside each attempt's `output` with the ordinary markers, and the marker's `trace_id`/`observation_id` pair is the same follow-up every consumer already has. |
| 20 | **2026-09-03** — Deleting a **dataset** is destructive in the spec 005 #8 sense — a dry run until `?confirm=` echoes the name — because it cascades every run and releases every pinned trace. Deleting a **run** or a **score config** is a plain `DELETE`: one row, and the traces it held are not deleted but returned to the window | The confirm ceremony exists for the act with a blast radius: a dataset takes its history of runs with it and, through #13, the evidence they held. A run's deletion destroys one row of bookkeeping; the traces go on living as long as retention says — forever by default (spec 005 #2) — and a harness that creates a run per CI job must be able to prune old ones from a script without a preview round-trip. A config's deletion changes what is accepted next (#17) and touches no score. |
| 21 | **2026-09-03** — Items are listed in order of **first appearance** — a per-dataset `seq` assigned at an item's first insert and carried by every later row of that item — and the cursor walks `seq` | Cases are written in an order that means something to their author (the easy ones, then the regression from last week, then the adversarial block), and a listing sorted by a random id would scramble it on every read. `seq` is assigned in the write transaction like everything else with an order (spec 003 #10), is stable across edits because an edit is a new row of the same item (#5), and gives keyset pagination one integer to seek on. |
| 22 | **2026-09-03** — MCP gains six **read-only** tools — `list_datasets`, `get_dataset_items`, `list_runs`, `get_run`, `get_run_items`, `compare_runs` — and no write; score configs are not a tool (spec 005 #13 stands) | "Why did the eval regress" is an agent question in the same class as "why did the last run fail" (spec 004 #17), and compare is the answer to it. Every tool is one GET through `API.Get` (`internal/mcpserver/api.go:23`); creating runs and posting items stays in the CLI and the HTTP API, where a hallucinated call has a human or a script to answer to. Score configs are read by nobody but the config author, and `list_score_configs` would be a tool with no question behind it. |
| 23 | **2026-09-03** — The "changes nothing" comparison of #6 covers the **source pair** as well as the three bodies: an item re-posted with the same `input`, `expected_output` and `metadata` but a different `source_trace_id` / `source_observation_id` is a change, one row and one tick (found implementing PR 1) | #6 reads "byte-equal to its current row", and the source pair is part of the row. A case that learned where it came from has changed — the pair is what an annotation queue will fill when a production trace is promoted (#4), and a write that could not record it without also touching a body would leave that fact nowhere. The harness that re-declares its cases is unaffected: it sends the same pair every time. |
| 24 | **2026-09-03** — A `POST …/{name}/runs` with an `id` that already exists **in another dataset** of the project is a **409**, not the 200-with-the-existing-run of #9 (found implementing PR 1) | #9's retry-safety is for the harness that re-sends the create it already made; answering with a run of a different dataset would hand that harness the wrong container without a word, and its traces would land in the other dataset's run. Two harnesses that hash the same natural key into an id are the collision, and the conflict is the message that names it. |
| 25 | **2026-09-03** — `GET /api/v1/score-configs` and `GET /api/v1/datasets/{name}/items/{id}/versions` return their lists **whole**, with no `limit` and no `cursor` — a stated exception to spec 003 #18, which every other list endpoint here keeps (found implementing PR 1) | Both lists are bounded by the shape of the data rather than by a page: a project has as many configs as it has score names — the names a human chose and a harness reuses — and an item has as many versions as the dataset has ticks it took part in, each one a deliberate edit someone wrote. A cursor over a list that cannot grow without a person growing it is ceremony with a cost: the caller writes a loop it will never take twice, and a first page that is the whole answer is indistinguishable from a first page that is not. The response budget (spec 004 #2) still applies, so a list that does become long is cut and says so, which is the honest failure — a page that silently ends is not. |
| 26 | **2026-09-03** — `idx_traces_run` is **partial** (`WHERE run_id IS NOT NULL`) (owner decision 2026-09-03, from review of PR 1) | Eval traffic is a slice of what a project ingests, and a full index would make every ordinary trace — the ones that will never name a run — pay a B-tree entry on write for a row no reader will ever seek. Every reader that seeks the column filters `run_id IS NOT NULL` (the pinned count) or `run_id IN (…)` (the per-dataset count, and PR 2's run reads), and neither form matches a NULL, so the partial index is the whole index for them; `PinnedTraces` has an `EXPLAIN QUERY PLAN` test that fails if the seek is ever lost. The sweep is the one place that asks about NULLs — `run_id IS NULL OR NOT EXISTS (…)`, which is true of nearly every row — and it never wanted this index: it seeks `idx_traces_ingested` for its window and evaluates the pin per row, which #13's plan test already holds it to. Migration 0010 is edited in place rather than followed by an 0011 because it has not shipped (the precedent of spec 003 #25). |

| 27 | **2026-09-03** — A delivery that **moves a trace to another run** carries its item with it: `item_id` is taken from that delivery, empty included, rather than merged field-wise against the stored one (found in review of PR 1) | The per-field upsert of spec 002 #6 is right for fields that stand alone and wrong for a pair that only means something together. Merged independently, a re-delivery naming run B with no item would leave run A's item on the row, and the store would then assert that run B answered a case it never saw — a fact nobody sent, in the table the run view reads. A missing item is a gap the reader can see; an invented one is a wrong number in a comparison. The pair is only unpicked when the run actually changes: a re-delivery of the same run, which is how a late span arrives, still leaves the item alone. |
| 28 | **2026-09-03** — `tracepad.item_id` on a span whose own attributes carry no run is **read but not claimed**: it links the trace if any span of the export supplies a run, and otherwise stays in that span's metadata (found in review of PR 1) | The ingest contract binds the item to a run *on the trace*, and a harness that stamps the run on the root span and the item on the span that answered is exactly the shape the docs' recipe grows into — reading the item only beside its own run filed those traces under a run with no item. But claiming it unconditionally would eat it: whether it names anything is unknown until every span has been seen, and by then the span's metadata is built. Reading without claiming keeps both promises, at the price of one duplicated attribute — an item that does link stays visible in the metadata of the span it was set on, where its harness put it. |
| 29 | **2026-09-03** — `summary.items.unknown` counts the run's **traces**, not items, and a trace that named a run and no item is one of them (found implementing PR 2) | The three numbers beside it are about the dataset — how many cases there are, how many were answered, how many were not — and `unknown` is about the traffic that those numbers cannot explain. Counting distinct unknown ids instead would leave a reader unable to reconcile `traces.count` with what the item view shows, and a trace with no item at all would be invisible in both. `?unknown=true` lists exactly what this counts, grouped by the id the traces named, so the number and the view agree. |
| 30 | **2026-09-03** — An item's value for a **categorical or text** name is its **newest attempt's** value, not an aggregate; and a name whose scores carry **two data types** in one run is reported under the type most of its scores used (found implementing PR 2) | #18 defines an item's value under N attempts as the mean of its attempts, which is a rule for numbers: there is no mean of a word, and picking the most frequent one would invent an answer for two attempts that disagree. The newest is the one a reader would quote — the last thing the judge said about that case. The second half is the same kind of tie-break: #15 lets a name that already carried one type accept another, so one run can hold both, and a summary that reported one aggregate over two types would be arithmetic across a category boundary. The minority rows are counted and not aggregated. |
| 31 | **2026-09-03** — `GET /api/v1/runs/{a}/compare/{b}` reads **both runs whole** — their items and their scores — and pages the response out of that, rather than computing page by page (found implementing PR 2) | The counts in the header are statements about the pair: how many items improved, regressed or stayed. A page cannot produce them, and a header that changed as the reader walked would be worse than no header. The bound is the runs' own traces, which #13 already keeps on disk in full for exactly this reading, and the endpoint is one an operator calls after a run rather than one a screen polls. |
| 32 | **2026-09-23** — The "changes nothing" comparison of #6 is on the **JSON value**, not the compacted text: object keys in any order, strings with or without `\uXXXX` escapes, and numbers as the decimal value they spell (`1`, `1.0`, `1e0` and `100e-2` are equal; `-0` equals `0`; no float rounding, so digits past float64 precision still count) are the same body. The row keeps the compacted text as first sent; an equivalent re-post writes nothing, so it does not replace that text. Compared by decoding both sides with `UseNumber` and walking them (`internal/store/jsonequal.go`), after a byte-equal fast path; no canonical hash is stored. Where the decoded value may not stand for the text, the bytes decide (a change, one tick): a body with anything after its value, a repeated key (last-wins would hide an edit to the shadowed one), or text the decoder would not keep apart: invalid UTF-8 or a lone `\uD800`–`\uDFFF` escape, both of which it writes as U+FFFD, checked on the raw bytes so a U+FFFD sent as such (a truncated LLM output) compares like any other character. A spelling-only edit of a number (`1.0` → `1`) writes nothing; it is written the next time anything else in the item changes, since a changed item's row takes all three bodies as sent, or by archiving and re-posting the item. Supersedes the "byte-equal (after JSON compaction)" wording of #6 and #23 (found on a live dataset: cases posted as raw UTF-8 and re-declared through `sdk/python` ticked a version and marked the Cyrillic ones edited) | #6 claimed key order was not a change, and compaction never delivered that: it strips whitespace and leaves key order, escapes and number spelling as sent. Each client spells the same value its own way — Python's `json.dumps` escapes non-ASCII and writes `1.0` for a float, `JSON.stringify` writes `1`, Go's `json.Marshal` escapes `<`, `>` and `&` — so a harness that moves between clients, or a case file first loaded by hand, minted a version per switch and #7's "same version" comparison stopped firing. Value equality is the promise #6 made. Number equality is by value because a harness does not choose how its language prints a float; a string `"1"` is still not the number `1`, and an array's order still counts. Decode-and-compare over a stored canonical hash: the hash needs a column, a migration and a backfill of every row, while the cost here is paid only by an item whose bytes differ from its stored row. A harness that re-declares through the client that wrote the rows stays a byte comparison; one whose rows were written in another spelling pays one parse of both bodies per item on every re-declaration, for as long as the rows keep that spelling (an equivalent re-post does not rewrite them), inside the write transaction. That is the price, and it is small: item bodies are test cases, not payloads, and the parse is beside a SELECT the loop already makes per item. Measured on the token walk that also rejects repeated keys: about 26 µs for a pair of 1 KB bodies and 1.7 ms for a pair of 100 KB ones, no slower than a plain `Decode` of both. Numbers compare exactly, not through float64: a client that rounds `12345678901234567890123` to `1.2345678901234568e+22` has sent another value, and the row records the value it was given. `sdk/python` now sends raw UTF-8 (`ensure_ascii=False`, encoded with `backslashreplace` so a lone surrogate, which has no UTF-8 form, goes as its `\u` escape and the rest stays raw), as the JS and Go clients already did: fewer bytes for non-ASCII text, and the stored text reads as it was written. The SDK and the server ship together from this repository and neither has a release yet, so no server that predates this Decision will see the raw-UTF-8 body against rows the escaping SDK wrote. |
| 33 | **2026-09-26** — Erasure deletes every row of an item cut from an erased trace and ticks the dataset's version (spec 044 #9) — the one exception to #5's append-only history, beside #14's for runs | An item is a verbatim copy of the user's input and output, and the API offered no purge short of the dataset. See spec 044 #9. |
| 34 | **2026-09-28** — **A batch of items holds at most 10,000** (owner decision; spec 043 #36), and the clients send a longer list as several writes. (a) `POST …/items` with more is `413` `{"error": "this request carries N items; the server takes at most 10000 per request — send them in batches"}`, before any item is validated or queued, writing nothing — the rule and the answer of spec 003 #28. #5 holds inside the cap: one batch, one tick. (b) `tracepad datasets push` sends a file of more than 10,000 cases as the fewest consecutive writes of up to 10,000 that carry it, in file order, each a tick of its own if it changes anything — amending the CLI contract's "sends it as one `POST`". The text answer ends `, sent as N writes` when there was more than one; `--json` answers a file one write carried with the server's own answer, and a longer one with `{ids, version, changed}` — every id in order, the last version, the sum of the changes. A write that fails ends the push with the server's error and where it happened: the server counts its indexes from the first case of the write and names none for a write of one case, so the error says which cases of the file the write was (`cases 3–5 of the file; an index in this message counts from case 3`), how many are written before it and at which version, and what is known of the write itself — refused with a 4xx, so whole and not written, or, when the connection dropped or the server answered 5xx, that whether it landed is not known; nothing is rolled back, and pushing the file again finishes it when its cases carry ids, and adds the ones without an id a second time. (c) Before sending a file it will split, `push` refuses one that gives an `id` to two cases, naming both indexes, and sends nothing. The SDKs do the same (spec 018 #15) | An item array is one transaction and 100,000 items took 3.4 s (spec 043 #35), all of it holding the only writer; 10,000 is a third of a second, and larger than any golden set we have seen pushed. Splitting in the client keeps a big file pushable without a new endpoint; the price is that a file over the cap is several versions — one per write that changes something — and a run opened between two of them pins a version with part of the file — acceptable, because a push over 10,000 cases is a bulk load, not the edit-and-rerun loop #5 is for. (c) is what one request would have done: the server refuses a repeated id within a batch, and split across two writes the second occurrence would silently become an edit of the first and `changed` would count it twice. |

## Data contract (schema 0010)

```sql
CREATE TABLE datasets (
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT,
    metadata    TEXT,                       -- JSON, inline
    version     INTEGER NOT NULL DEFAULT 0, -- advances by one per changing write (Decision 5)
    next_seq    INTEGER NOT NULL DEFAULT 0, -- Decision 21
    created_at  INTEGER NOT NULL,           -- ns, server clock
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (project_id, name)
) STRICT;

CREATE TABLE dataset_items (
    project_id      TEXT NOT NULL,
    dataset         TEXT NOT NULL,
    item_id         TEXT NOT NULL,          -- 32-hex (Decision 9)
    dataset_version INTEGER NOT NULL,       -- the version this row was written at (Decision 5)
    seq             INTEGER NOT NULL,       -- first-appearance order (Decision 21)
    archived        INTEGER NOT NULL DEFAULT 0 CHECK (archived IN (0, 1)),
    input           TEXT,                   -- JSON, compacted
    expected_output TEXT,                   -- JSON, compacted
    metadata        TEXT,                   -- JSON, compacted
    source_trace_id       TEXT,
    source_observation_id TEXT,
    created_at      INTEGER NOT NULL,
    PRIMARY KEY (project_id, dataset, item_id, dataset_version),
    FOREIGN KEY (project_id, dataset) REFERENCES datasets(project_id, name) ON DELETE CASCADE
) STRICT;
CREATE INDEX idx_dataset_items_seq ON dataset_items(project_id, dataset, seq, dataset_version);

CREATE TABLE dataset_runs (
    project_id      TEXT NOT NULL,
    id              TEXT NOT NULL,          -- 32-hex
    dataset         TEXT NOT NULL,
    dataset_version INTEGER NOT NULL,       -- Decision 7
    name            TEXT,                   -- free label, not unique
    metadata        TEXT,                   -- JSON, inline (Decision 12)
    status          TEXT NOT NULL CHECK (status IN ('running', 'finished', 'failed')),
    error           TEXT,                   -- client's reason when failed
    created_at      INTEGER NOT NULL,
    finished_at     INTEGER,
    PRIMARY KEY (project_id, id),
    FOREIGN KEY (project_id, dataset) REFERENCES datasets(project_id, name) ON DELETE CASCADE
) STRICT;
CREATE INDEX idx_dataset_runs_dataset ON dataset_runs(project_id, dataset, created_at DESC, id DESC);

CREATE TABLE score_configs (
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    data_type   TEXT NOT NULL CHECK (data_type IN ('numeric', 'boolean', 'categorical', 'text')),
    direction   TEXT CHECK (direction IN ('higher', 'lower', 'none')),  -- Decision 16
    min_value   REAL,
    max_value   REAL,
    categories  TEXT,                       -- JSON array of strings, categorical only
    description TEXT,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    PRIMARY KEY (project_id, name)
) STRICT;

ALTER TABLE traces ADD COLUMN run_id  TEXT;   -- Decision 2, no FK
ALTER TABLE traces ADD COLUMN item_id TEXT;
CREATE INDEX idx_traces_run ON traces(project_id, run_id, item_id)
    WHERE run_id IS NOT NULL;                 -- Decision 26
```

`traces.run_id`/`item_id` join the per-field upsert in `upsertTrace`
(`internal/store/ingest.go:127-137`) and the ranked trace-field chain in the
mapper (`internal/mapping/rules.go`, `mapping.go:271-279`); the claimed keys
leave metadata as every claimed attribute does (spec 012 #7).

"Items at version V" is one query, and every read of a dataset or a run uses
it:

```sql
SELECT i.* FROM dataset_items i
 WHERE i.project_id = ? AND i.dataset = ?
   AND i.dataset_version = (SELECT MAX(dataset_version) FROM dataset_items
                             WHERE project_id = i.project_id AND dataset = i.dataset
                               AND item_id = i.item_id AND dataset_version <= ?)
   AND i.archived = 0
 ORDER BY i.seq
```

The sweep's pick (`expiredTraceIDs`) becomes:

```sql
SELECT id FROM traces t
 WHERE t.project_id = ? AND t.ingested_at < ?
   AND (t.run_id IS NULL OR NOT EXISTS
        (SELECT 1 FROM dataset_runs r WHERE r.project_id = t.project_id AND r.id = t.run_id))
 ORDER BY t.ingested_at LIMIT ?
```

still driven by `idx_traces_ingested`; the plan is asserted as spec 005's
`TestSweepWindowSeeks` does.

## API contract

Auth, error shape, strict bodies and query parameters, `limit` + cursor:
per specs 003–004. Timestamps RFC 3339 out, ns in. Every write below is a
`WriteJob` (spec 003 #9); typed rejections render as 400/404/409 through
`Server.submit` (`internal/server/api.go:68`).

### Datasets

`PUT /api/v1/datasets/{name}` — body `{"description"?, "metadata"?}`; creates
or updates the envelope (not the items), 200 with the dataset object. A
dataset also comes into being on the first `POST …/items` to its name.

`GET /api/v1/datasets` — `{"datasets": [{"name", "description", "version",
"item_count", "run_count", "created_at", "updated_at"}], "next_cursor",
"prev_cursor"}`, ordered by name. `GET /api/v1/datasets/{name}` — the object
with `item_count` at the current version.

`DELETE /api/v1/datasets/{name}[?confirm=<name>]` — Decision 20: dry run
returns `{"dataset", "items", "runs", "pinned_traces"}`; confirmed, deletes
the dataset, its items and runs, and answers 200 with the same counts.

### Items

`POST /api/v1/datasets/{name}/items` — one object or an array:

```json
{
  "id": "optional 32-hex",
  "input": {"question": "…"},
  "expected_output": {"answer": "…"},
  "metadata": {"tags": ["regression"]},
  "source_trace_id": "…", "source_observation_id": "…"
}
```

`input` required; the rest optional. All-or-nothing (spec 003 #7). Response
201 `{"ids": […], "version": V, "changed": n}` — `version` is the dataset's
version after the write, `changed` how many items produced a row (Decision 6;
`changed: 0` leaves `version` where it was).

`GET /api/v1/datasets/{name}/items[?version=V]` — Decision 19: full bodies,
ordered by `seq`, `limit` ≤ 500, cursor both ways. Default `version` is the
current one; a `version` above the current is a 400. Response
`{"dataset", "version", "items": [{"id", "seq", "version" (the row's),
"input", "expected_output", "metadata", "source_trace_id",
"source_observation_id", "created_at"}], "next_cursor", "prev_cursor"}`.

`GET /api/v1/datasets/{name}/items/{id}[?version=V]` — one item as of V.
`GET /api/v1/datasets/{name}/items/{id}/versions` — every row of the item,
newest first, archived rows included.

`DELETE /api/v1/datasets/{name}/items/{id}` — archives at a new version
(Decision 5); 200 `{"id", "version"}`; 404 if the item is unknown or already
archived at the current version.

### Runs

`POST /api/v1/datasets/{name}/runs` — body `{"id"?, "name"?, "metadata"?,
"dataset_version"?}`; 201 with the run object; a known `id` returns the
existing run unchanged, 200 (Decision 9). `dataset_version` above the current
is a 400.

`POST /api/v1/runs/{id}/finish` — body `{}` or `{"status": "failed",
"error": "…"}`; a run already finished or failed is a 409. 200 with the run
object.

`GET /api/v1/datasets/{name}/runs` — newest first, `{"runs": [run objects
without `summary`], …cursors}`.

> **2026-09-04 (spec 016 #2):** `GET /api/v1/runs` is added beside it — the
> same rows across every dataset of the project, newest first by
> `(created_at, id)`, with filters `dataset` (exact name) and `status`,
> cursors both ways and `count`, over schema 0011's
> `idx_dataset_runs_created`. `tracepad runs ls` without a dataset and MCP
> `list_runs` without `dataset` read it. In the same change the comparison's
> response schemas in `openapi.json` — `traces`, and `ComparedScore.a`/`b`
> as `ComparedSide` — were tightened to the shape the server already
> answers with; no behaviour changed.

`GET /api/v1/runs/{id}` — the run object with its summary:

```json
{
  "id": "…", "dataset": "support-golden", "dataset_version": 12,
  "name": "prompt v7 / gpt-5", "metadata": {"prompt": "support-answer@7"},
  "status": "finished", "error": null,
  "created_at": "…", "finished_at": "…",
  "summary": {
    "items":  {"total": 200, "covered": 198, "missing": 2, "unknown": 0},
    "traces": {"count": 214, "attempts_max": 3, "error_count": 3,
               "total_cost": 1.42, "latency_ms": {"p50": 812, "p95": 2410}},
    "scores": {
      "accuracy": {"data_type": "numeric", "direction": "higher",
                   "count": 214, "mean": 0.81, "min": 0, "max": 1},
      "verdict":  {"data_type": "categorical", "direction": null,
                   "count": 214, "distribution": {"pass": 170, "fail": 44}}
    },
    "models":  ["gpt-5", "gpt-5-mini"],
    "prompts": [{"name": "support-answer", "version": 7}]
  }
}
```

`items.unknown` counts traces whose `item_id` is not an item of the dataset at
the run's version (a harness that ran newer cases, or a typo); `missing` are
the version's items with no trace. Cost is summed over `provided_cost` rows
only and is `null` when none carried one (spec 002 #14). Percentiles are
exact over the run's traces (hundreds, not millions — not the histogram of
spec 013, and the docs say which is which). `mean`/`min`/`max` for numeric
and boolean; `distribution` for categorical; text scores report `count`
only.

`GET /api/v1/runs/{id}/items` — the version's items with their attempts,
ordered by `seq`, paginated:

```json
{"items": [{
  "id": "…", "seq": 3,
  "expected_output": {…budgeted…},
  "attempts": [{
    "trace_id": "…", "timestamp": "…", "error_count": 0,
    "total_cost": 0.007, "latency_ms": 640,
    "output": {…budgeted, marker carries trace_id + observation_id…},
    "scores": [{"name": "accuracy", "data_type": "numeric", "value": 1}]
  }]
}], "next_cursor": …, "prev_cursor": …}
```

`output` is the trace's root observation's output (the earliest-starting
observation without a parent); a trace with no payload there shows `null`.
`?unknown=true` appends the unknown-item traces as rows marked `"unknown":
true`, grouped by the id they named — `"id": null` for the traces that named
none (Decision 29).

`DELETE /api/v1/runs/{id}` — 200 `{"id", "released_traces": n}` (Decision
20).

### Compare

`GET /api/v1/runs/{a}/compare/{b}` — Decision 18:

```json
{
  "a": {"id", "name", "dataset_version", "status", "created_at"},
  "b": {…},
  "dataset": "support-golden", "same_version": false,
  "metadata": {"prompt": {"a": "support-answer@7", "b": "support-answer@8"}},
  "models":  {"a": ["gpt-5"], "b": ["gpt-5"]},
  "prompts": {"a": [{"name": "support-answer", "version": 7}], "b": […]},
  "traces": {"count": {"a": 214, "b": 200}, "error_count": {"a": 3, "b": 0},
             "total_cost": {"a": 1.42, "b": 1.10, "delta": -0.32},
             "latency_ms": {"p50": {"a": 812, "b": 700}, "p95": {…}}},
  "scores": [{
    "name": "accuracy", "data_type": "numeric", "direction": "higher",
    "a": {"mean": 0.81, "count": 214}, "b": {"mean": 0.86, "count": 200},
    "delta": 0.05, "improved": 14, "regressed": 3, "same": 181
  }],
  "items": [{
    "id": "…", "seq": 3, "in": "both",
    "scores": {"accuracy": {"a": 0.5, "b": 1, "delta": 0.5, "verdict": "improved"}}
  }],
  "next_cursor": …, "prev_cursor": …
}
```

`metadata` lists only keys whose values differ (compact JSON, both sides).
`in` ∈ `both` \| `a` \| `b` \| `only_in_version_a` \| `only_in_version_b` —
the last two when `same_version` is false. An item covered by one run and
not the other has `in: "a"` and no verdicts. `verdict` per Decision 16;
`changed`/`same` for names with `direction: "none"` or no config. Categorical
and text names appear in `scores` with distributions and in `items` with the
two string values and `changed`/`same`.

### Score configs

`PUT /api/v1/score-configs/{name}` — Decision 17:

```json
{"data_type": "numeric", "direction": "higher", "min": 0, "max": 1,
 "description": "LLM judge, 0–1"}
```

`categories` (non-empty array of distinct strings) required for
`categorical`; `min`/`max` allowed for `numeric` only, `min ≤ max`;
`direction` per Decision 16. 200 with the config object (`created_at`,
`updated_at` included). `GET /api/v1/score-configs`,
`GET /api/v1/score-configs/{name}`, `DELETE /api/v1/score-configs/{name}`
(200 `{"name"}`; 404 when absent).

`POST /api/v1/scores` — Decision 15: a score whose name has a config is
checked against it inside the write; the batch's 400 names the item and the
rule (`scores[3]: "accuracy" is numeric in its config, got categorical`;
`value 1.5 is above the config's max 1`; `"maybe" is not among the config's
categories`). `GET /api/v1/scores` rows gain nothing; the config is one
`GET` away by name.

### System

`GET /api/v1/system` counts `datasets`, `dataset_items`, `dataset_runs`,
`score_configs` (spec 004 #33 scoping) and adds
`"runs": {"pinned_traces": n, "orphan_traces": n}` — the first from the
store (Decision 13), the second the process counter of Decision 3.

## Ingest contract

| Attribute | Claimed into | Notes |
|---|---|---|
| `tracepad.run_id` | `traces.run_id` | 32-hex; any other shape is left in metadata unclaimed and neither column is set. Trace-level, ranked like `session_id` (spec 012 #11); any span may carry it, the root is the convention. |
| `tracepad.item_id` | `traces.item_id` | 32-hex; set only when `run_id` was also claimed on the trace. |

Both keys are level-agnostic (span, scope or resource — spec 012 #7).
`langfuse.experiment.*` stays unclaimed in metadata (owner decision
2026-09-03: one namespace). In the ingest transaction, a trace whose `run_id`
names no run of the project increments the orphan counter and logs one
warning per distinct unknown id per process.

## CLI contract

```
tracepad datasets ls
tracepad datasets show <name> [--version N]            # items, whole
tracepad datasets push <name> --file cases.jsonl [--description …]
tracepad datasets rm-item <name> <item-id>
tracepad datasets rm <name> [--yes]

tracepad runs ls <dataset>
tracepad runs create <dataset> [--name …] [--metadata-file run.json] [--dataset-version N] [--id …]
tracepad runs show <id> [--items]
tracepad runs finish <id> [--failed "reason"]
tracepad runs compare <a> <b>
tracepad runs rm <id>

tracepad score-configs ls | show <name> | push <name> --file cfg.json | rm <name>
```

`push` reads a `.jsonl` (one item per line) or a `.json` array, sends it as
one `POST`, and prints the version it landed on and how many items changed —
`unchanged at version 12` when nothing did. `runs create` prints the id, and
in JSON mode the whole run object, so a script does
`RUN=$(tracepad runs create support-golden --json | jq -r .id)`. `runs
compare` renders the header block, then one line per score name with the
delta and the improved/regressed counts, then the items whose verdict is not
`same`; `--all` lists every item. `rm` for a dataset confirms on a TTY with
the dry-run preview and needs `--yes` otherwise (spec 005 #13 pattern); `rm`
for a run and a config does not. Output modes and exit codes per spec 004
#12. The recipe in `docs/datasets.md` is the whole loop: declare configs,
push items, create the run, iterate items stamping the two attributes, post
scores, finish, compare.

## MCP contract

Six tools per Decision 22, mirroring their endpoints' parameters:
`list_datasets`, `get_dataset_items` (`name`, `version`, paging),
`list_runs` (`dataset`, paging), `get_run` (`id`), `get_run_items` (`id`,
paging), `compare_runs` (`a`, `b`, paging). Descriptions are when-to-use
triggers with a "does NOT" clause naming the neighbour, as
`internal/mcpserver/tools.go:25-30` prescribes; `readOnlyHint: true` on all
six; `structuredContent` byte-equal to the endpoint (spec 004 #16).
Truncation markers in `get_run_items` pass through and feed
`get_observation_io` as everywhere else.

## Testing

- **Go, store — versions**: a batch of three items advances the version by
  one; re-posting the same batch advances nothing and returns the same
  version; editing one of three advances by one and creates one row;
  archiving hides at the new version and shows at the old; "items at V" for
  every V of a scripted history equals the expected set; concurrent `POST`s
  to one dataset yield consecutive versions with no gaps (the spec 003 #10
  concurrency test, re-aimed); `seq` is stable across edits.
- **Go, mapping** (`internal/mapping/wire_test.go` precedent): the two
  attributes claim into the columns and leave metadata; a malformed id claims
  nothing; `item_id` without `run_id` claims nothing; the ranked chain keeps
  the root's value over a child's absence; golden fixtures gain one eval
  export (`testdata/otlp/009-*`) and the golden diff is the change.
- **Go, ingest**: a trace naming an unknown run is stored with its columns
  and bumps the counter once per id; a known run links; a second trace of the
  same item links beside the first.
- **Go, sweep** (`internal/store/sweep_test.go` precedent): an expired trace
  of a live run survives the pass; deleting the run releases it and the next
  pass takes it; a trace whose `run_id` names no run is swept; the pick's
  `EXPLAIN QUERY PLAN` still seeks `idx_traces_ingested`; the retention dry
  run's count excludes pinned traces; user-data erasure deletes a pinned
  trace and the run's summary reports the item missing (Decision 14);
  project purge leaves no dataset rows.
- **Go, scores** (`TestScoreValidationMatrix` precedent): the config matrix —
  type mismatch, below min, above max, category outside the list, boolean
  with a numeric config — each a 400 naming the item and the rule; a name
  without a config is unchanged; a batch with one violating item writes
  nothing; a config replaced between two writes governs only the second;
  `direction` required/forbidden per type; `min > max` is a 400.
- **Go, summary and compare**: on a seeded pair of runs the summary's counts,
  means, distributions, models and prompts equal hand-computed truth; N
  attempts of one item average into one item value and the item rows sum to
  the header; compare of different datasets is a 400; of different versions
  labels the edges; `metadata` diff lists only differing keys; verdicts follow
  direction and `none` yields `changed`/`same`; cost `null` when no attempt
  carried one.
- **Go, budgets**: the items listing returns a 400 KB item whole; the run
  items view cuts `output` and `expected_output` with markers whose pair
  resolves through `/observations/{id}/io`.
- **Go, HTTP**: dataset envelope CRUD, dry-run and confirm on dataset delete,
  run create/idempotent re-create/finish/409 on double finish/delete with
  `released_traces`, item versions endpoint, unknown query parameter and
  unknown field 400s on every new route, cursor walks at `limit=1` both ways,
  OpenAPI↔router parity (`internal/server/meta_test.go`).
- **CLI**: golden output for every new command in both modes; `push` with
  `.jsonl` and `.json`; the flag-parity test covers the new flags; `rm` on a
  dataset requires `--yes` off a TTY; exit codes.
- **MCP**: every new tool called over both transports, `structuredContent`
  byte-equal to the endpoint; `tools/list` order and cache fields unchanged
  in kind.
- **Cost**: the ingest benchmark with and without the attributes present —
  the run-existence lookup must be inside the noise for traffic without them,
  and the number for traffic with them goes in the PR.

## Edge cases

- **Run created, dataset then edited, run still running**: the run's
  `dataset_version` is the older one; traces for the new items count as
  `unknown`, visible with `?unknown=true`.
- **Item archived, then re-posted with the same id**: a new row with
  `archived = 0` at a new version; `seq` is the original one, so it returns
  to its place.
- **Two runs compared while one is still `running`**: allowed; the response
  carries both statuses and the docs say a running run's numbers move.
- **`finish` on a run with zero traces**: allowed — a harness that failed
  before its first case still closes the run, with `failed` and its reason.
- **Trace re-delivered with a different `run_id`**: per-field upsert, last
  delivery wins (spec 002 #6) — the trace moves runs. Deliberate, and the
  same rule as every other trace field.
- **Score config for a name that scores already used with another type**:
  accepted; old scores stand (Decision 15), new ones must comply.
- **Dataset deleted while a run's traces are mid-ingest**: the columns are
  written, the run is gone, the traces are orphans of Decision 3 and mortal.
- **Retention window shortened below the age of pinned traces**: the dry run
  and the sweep both skip them; `/system` shows the pinned count so the
  operator sees why the file did not shrink.
- **Compare `a` with itself**: 400 — a request that cannot mean what it says
  (spec 003 #23).

## Config additions

None. `TRACEPAD_MAX_BODY_BYTES` bounds item batches as it bounds every body.

## Out of scope (later specs)

Datasets/Runs screens and the compare view in the UI (016, over the JSON viewer of 015); annotation
queues and promoting a trace to an item through them; quality trends over
time (spec 013's rollup gaining score dimensions); a violations listing for
scores older than their config; `langfuse.experiment.*` mapping; export of a
dataset (`tracepad datasets show --json` already is one); the server running
judges (iteration 3); renaming datasets; per-item or per-run score targets.
