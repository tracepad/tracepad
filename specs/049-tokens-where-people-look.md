# Spec 049 — Tokens where people look: traces, sessions, users, and every class in the statistics

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> Tracepad has no price table (spec 002 #14): cost is known only when the
> client sends it, and many clients do not. Tokens are the one measure of
> usage almost every generation reports. Yet spec 031 put them only into the
> project's statistics. The trace listing, the session page, the user page
> and the per-user rollup (`users_hourly`) show cost, which is often empty,
> and no tokens. A `user_id` question to `/api/v1/stats` answers without
> tokens by design (031 #11), and reasoning and cache-write counts stay on the
> observation. This spec gives the trace a token total maintained on write, as
> it already has a cost. Sessions sum it, `users_hourly` and `users` roll it,
> and the listings show, filter and sort on it. It adds the two missing
> classes as classes of their own, which no total ever includes. The trace
> columns are the part that must land before a public release. They are
> filled from every observation in the store, which costs seconds on a small
> store today and a full scan of somebody's live data after.

---

## Overview

Deliverables, two PRs (the last commit of PR 2 flips the status):

- **PR 1 — the numbers** (Decisions 1–8, 11–19): schema 0033 adds five token
  columns to `traces`, `users_hourly` and `users`, and two to `stats_hourly`.
  The trace columns are maintained in the ingest transaction and backfilled
  by the migration; the rollups re-roll on the next pass. The read API:
  `tokens` on trace, session and user rows and on user detail, `min_tokens` on
  the trace listing, `sort=tokens` on the user listing, and `tokens` in
  `user_id` statistics. Every `tokens` object gains `reasoning` and
  `cache_write`. CLI, MCP, `openapi.json`, `schema.d.ts` and the docs follow,
  and the interface's filter and sort lists, which a test holds to
  `openapi.json` (#14).
- **PR 2 — the screens** (Decisions 9–10): a *Tokens* column on the trace,
  session and user tables, the Tokens column in the user page's breakdowns,
  and the two new classes in the stats breakdown and the dashboard's tokens
  tooltip.

Not here: a price table (spec 002 #14 stands), sorting the trace or session
listing (neither has a sort), a tokens tile on the dashboard (Decision 10),
tokens per score or per run.

Builds on spec 031 (the token classes, the counting rule, the stats
`tokens` object), spec 043 #4 (the counted domain, 0..10⁹), spec 002 #7 (the
trace's aggregates maintained on write), specs 013 and 023 (whole-hour
re-rolls, frozen hours, the `last_pass = 0` backfill, the `users` summary)
and spec 047 (the erasure's roll budget). It amends spec 031 #1, #2 and #11
by their numbers.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-29** — **Five token classes, each a closed list of keys; the first key present decides.** `input`, `output` and `cache_read` are spec 031's. The new ones are `reasoning` and `cache_write`; their lists are settled by #12. **No class is ever added to another**: every sum in this spec is per class, and the one headline number is #3's. Amends spec 031 #1 | Spec 031 #1 kept reasoning out because some providers count it inside `output` and some beside it, so adding it would double-count or under-count. That objection is about adding, and the answer is not to add. `cache_read` already lives by this rule: OpenAI counts cached tokens inside `prompt_tokens`, Anthropic beside `input_tokens`, and spec 031 shows it as its own figure. Reasoning is what makes a thinking model's bill; cache writes are what make a cached prompt's first call cost more. Both are asked about, and both are only visible today by opening observations one at a time. The key lists stay closed for spec 031's reason: a collision with somebody's unrelated key should be visible in a list. |
| 2 | **2026-09-29** — **The trace carries five token columns, maintained on write like its cost.** `traces` gains `input_tokens`, `output_tokens`, `cache_read_tokens`, `reasoning_tokens`, `cache_write_tokens`, nullable integers. `refreshAggregates` recomputes them in the ingest transaction, beside `total_cost`, over every observation of the trace, with one subquery that assigns all five. It uses the counting rule the statistics use: only observations that name a model (spec 031 #12), and only counts within 0..10⁹ (spec 043 #4). A class no counted observation carries is `NULL`, not 0. Amends spec 031 #2 | Spec 031 #2 declined the columns because "nothing lists or filters on tokens". This spec lists and filters on them. A session's tokens are a sum over its traces, computed on every read of the session listing (spec 007 #2). Summing a JSON field over every observation of every trace on that read is the query spec 031 #2 warned about. One rule for the trace and for the rollup means the trace listing, the session page and the statistics agree to the token: a number that differs by screen is a bug report. `NULL` against 0 is spec 002 #14's "no data is not zero". |
| 3 | **2026-09-29** — **The one headline number is `input + output`.** Where a listing shows one figure (the *Tokens* column, the `min_tokens` filter, the `tokens` sort), it is `input + output`, with a `NULL` class read as 0 and `NULL` when both are `NULL`. Everything else is in the `tokens` object and the column's tooltip | It is what the CLI's `stats` and the interface's `billedTokens` already call tokens, so the listing agrees with the screens that exist. Adding `cache_read` or `reasoning` would double-count for half the providers (#1). A single number is still needed: a column, a filter and a sort each need one. |
| 4 | **2026-09-29** — **Existing data is backfilled.** The migration adds the columns and, in the same migration, fills the five trace columns from the stored observations by the rule `refreshAggregates` sums with (#17), in one grouped pass over the observations. It ends with `UPDATE stats_rollup SET last_pass = 0`, so the next pass re-rolls every retained hour into `stats_hourly`, `users_hourly` and `users` (spec 023 #15). **Known limit**, as spec 031 #3 recorded: an hour past a project's `retention_days` is frozen (spec 013 #14) and keeps `NULL` for the new classes and the new tables for ever. **Meanwhile** *(recorded 2026-09-30, from review)*: until the first pass has re-rolled every retained hour, the users' tokens (the listing, `sort=tokens`, the rolled half of the user page) and the statistics' tokens for the older hours cover only the hours re-rolled so far; `docs/api.md` and `docs/users.md` say so. The trace columns are complete before the server listens | The observations are still on disk for every trace the store still holds, so nothing has to be guessed. Leaving old traces `NULL` would draw an empty column on everything before the upgrade, and the user page would show tokens starting on the upgrade's day. The re-roll is the backfill every rollup addition has used (0013, 0015, 0016, 0018, 0025). Its cost is recorded in #19. |
| 5 | **2026-09-29** — **`users_hourly` and `users` roll the five classes.** `rollUserHour` reads the token columns in its observation query as `rollHour` does, into both the model cell and the trace-unit cell (spec 031 #2's rule). `recomputeUsers` sums the trace-unit cells. A `user_id` answer from `/api/v1/stats` carries `tokens` on every grouping, from the rollup and the live scan alike; the user page's live tail reads the trace columns, which follow the same rule. Amends spec 031 #11 | Spec 031 #11 withheld tokens from a user's statistics only because `users_hourly` had no columns, and it said so: absent was the honest answer "until `users_hourly` learns the three columns, which is a spec of its own". This is that spec. The live half already knows how to count them. |
| 6 | **2026-09-29** — **Listings.** *Traces:* `tokens` is a row field (and a `fields=` name); `min_tokens=N` keeps traces whose #3 number is at least `N`; no index, as `min_cost` has none. *Sessions:* each row and the detail carry `tokens`, summed per class over the session's traces on read. *Users:* each row and the detail carry `tokens`; `sort=tokens` orders by #3's number, descending, served by an expression index on `users`. A user with no tokens sorts as 0. Every `tokens` object has the shape of the statistics' (spec 031): each class present only when it is not `NULL`, the object absent when all are | Spec 031's Out of scope named these exact places: `min_tokens`, a column, the session page, the user page. The listing's filter runs inside the page's time window like `min_cost`; an index would pay a write on every trace for a filter a person applies by hand. `users` is one row per user and is sorted as a whole, so the sort needs its index. Treating no tokens as 0 there avoids cost's separate `NULL` keyset: a user with no token data belongs at the bottom of "who uses the most". |
| 7 | **2026-09-29** — **`stats_hourly` gains `reasoning_tokens` and `cache_write_tokens`.** `/api/v1/stats` buckets carry them as `tokens.reasoning` and `tokens.cache_write`, and the live scan counts them the same way | The rollup already carries the other three classes; two more nullable integers join them in every cell. |
| 8 | **2026-09-29** — **Cost, and its budgets.** *Size:* five nullable integers add about 10–15 bytes to a trace row and to a `users_hourly` row; `stats_hourly` adds two columns on a table of a few thousand rows a month; the `users` index adds about 30 bytes a user. *Write:* `refreshAggregates` already scans the trace's observations for cost; it now also extracts five JSON fields from each. Budget: the ingest batch median grows by at most 5 %. *Roll-up:* `rollUserHour` extracts the same fields; budget: a whole-hour roll with 1,000 users (spec 023 #19's shape, 7,000 traces an hour) grows by at most 10 %, and spec 047's chunk p99 stays ≤ 250 ms, checked by CPU time or by each chunk's fastest of several runs (spec 047 #34). The measurements are #18 | The columns are cheap in bytes. What must be watched is the ingest transaction, which every span pays for, and the whole-hour roll, which the erasure runs inside its own transactions. |
| 9 | **2026-09-29** — **On screen.** *Traces*, *Sessions* and *Users* tables gain a *Tokens* column (#3's number, compact: `12.4k`), with a tooltip listing every class present. The filter bar gains *Min tokens* beside *Min cost*. The Users sort select gains *Tokens*. The user page's two breakdown tables turn on `BreakdownTable`'s `tokens` column. The stats breakdown's tokens columns and the dashboard's Tokens chart tooltip gain *reasoning* and *cache write*; the chart's stacked series stay input and output | Every place spec 031 left out gets the same number, spelled the same way. The chart stays input and output because stacking reasoning on output would draw the double count #1 refuses. |
| 10 | **2026-09-29** — **The dashboard keeps its four tiles.** No tokens tile (spec 034 #2 stands) | The tokens chart is already on the dashboard, and a fifth tile is a layout decision for spec 034, not a schema one. It can come any time without a migration. |
| 11 | **2026-09-29** — **CLI, MCP and SDKs.** *CLI:* `traces ls`, `sessions ls` and `users ls` gain a `TOKENS` column, and `sessions show` and `users show` a `tokens` line with the other classes beside the headline number; `--min-tokens N` wherever the trace filter is taken; `users ls --sort tokens`; `--json` carries the objects as the API does. The trace tree's `N tokens` reads the classes (#15) instead of only `usage.total`, which showed nothing for an OpenAI-style usage. *MCP:* the trace, session and user row schemas gain `tokens`; the trace filter gains `min_tokens`; `list_users`' sort enum gains `tokens`; `get_stats` declares the `tokens` object its output has carried since spec 031. *SDKs:* no change: none of the three reads a listing or the statistics. What each writes lands in a class: the Python and Node packages write `reasoning_tokens` from an OpenAI-compatible answer, the Go package documents `cache_creation_input_tokens`, and all three spellings are on #12's lists | The interface may do nothing the CLI cannot (spec 004 #1). The `get_stats` schema has lacked `tokens` since spec 031, which an agent reading only the schema cannot know about. |
| 12 | **2026-09-29** — **The key lists.** *reasoning* ← `reasoning_tokens`, `output_reasoning_tokens`, `reasoning.output_tokens`. *cache write* ← `cache_creation_input_tokens`, `cache_creation_tokens`, `input_cache_creation`, `input_cache_write_tokens`, `cache_write_tokens`, `cache_creation.input_tokens`. *cache read* gains `input_cache_read` and `cache_read.input_tokens` after spec 031's three. A dotted key is the suffix the mapper keeps from a `gen_ai.usage.*` attribute (spec 030 #1), one key with a dot in it, so the JSON path quotes every key. The bare `reasoning` the proposal listed is not on the list | Each spelling has a sender. `reasoning_tokens` is what the three packages write and one of spec 030's bare keys; `cache_creation_input_tokens` is Anthropic's `usage` object and the Go package's documented key; `cache_creation_tokens` is Claude Code's (spec 030's fixture). `output_reasoning_tokens`, `input_cache_creation`, `input_cache_read` and `input_cache_write_tokens` are the names Langfuse's usage details use for OpenAI's and Anthropic's nested counts, and `cache_write_tokens` is OpenAI's own. The three dotted keys are the OpenTelemetry GenAI conventions' `gen_ai.usage.reasoning.output_tokens`, `…cache_creation.input_tokens` and `…cache_read.input_tokens`; unquoted, `$.cache_read.input_tokens` would read a nested object nobody sent. Nothing is known to send a bare `reasoning`, and a bare word is the collision the closed list exists to avoid. |
| 13 | **2026-09-29** — **`min_tokens` is part of the trace filter**, so every endpoint that takes the listing's filters takes it: `GET /api/v1/traces`, `/traces/last`, the bulk `DELETE /api/v1/traces` and `POST /queues/{name}/items/from-traces`. It is a non-negative integer; anything else is a `400` | The four share one parser (`traceListFilters`), which is what makes a filter composed in one place mean the same in the others (spec 035, spec 024 #4). A count is a whole number, and reading `1.5` as 1 would be a guess. |
| 14 | **2026-09-29** — **The interface's filter and sort lists land in PR 1.** The *Min tokens* field (a whole number) and the *Tokens* sort option ship with the API that serves them; the columns, tooltips and breakdowns are PR 2 | A test holds the interface's lists to `openapi.json` in both directions (spec 016 #13), so an API that grows a filter without the screen offering it does not pass the gate. The two controls are a line each and work end to end on PR 1's API. |
| 15 | **2026-09-29** — **The tree counts with the store's rule.** The CLI's tree calls `store.UsageTokens` (#17) on each observation's `usage`: the first key present decides — a JSON `null` included — and it counts only as a number within 0..10⁹, truncated. The tree prints its #3 number, or the `total` or `total_tokens` the client sent when there is no class or the classes add up to less *(amended 2026-09-30, from review: the first form showed the total only when no class was there, so `{"input": 40, "total": 100}` read 40)*. A test holds the tree's reading to the stored sums over a table of usages | The tree prints one observation's usage, which the server does not aggregate, so the rule has to run where the tree is drawn; the CLI already takes the store's constants rather than restating them. The tree is about one observation, and a sent total above input plus output is the classes the listings do not add in — reasoning a provider counts beside the output, audio — which is no reason to hide them there; it also keeps the number a `{"total": N}` usage showed before this spec. |
| 16 | **2026-09-29** — **Where each number is read.** The trace listing, a session and the user page's live tail read the trace columns. The rollups and the live statistics keep reading the observations, as spec 031 #5 built them | The columns and the observation sums are the same rule over the same rows (#2), which the agreement test holds on both sides of the watermark. Reading the columns where a trace is the unit is what they exist for; moving the statistics onto them would be a change to shipped queries with no number that changes. |
| 17 | **2026-09-29** — **The counting rule is one Go function, and every reader decodes a row's `usage` once.** `UsageTokens` (`tokens.go`) is the rule. The trace's columns, both rollups and the live statistics select an observation's `usage` text and decode it once for all five classes; nothing on those paths carries the rule in SQL. Migration 0033's one-time backfill calls the same rule as the SQL function `tracepad_token(usage, class)`, which stays registered for good because a migration calls it. This replaces spec 031's rule spelled in SQL (a `json_type` and two `json_extract`s per key). *(Amended 2026-09-30, from review: the first form of this decision had every reader call the function, one call per class, with a process-wide memo of the last decoded text so the four calls after a row's first cost a comparison; a lock every connection shared, and state kept between rows, for what one decode per row does without either.)* | Measured first with the rule spelled in SQL at #12's twenty keys, the budgets of #8 were missed: the whole-hour roll went from 176 to 250 ms (+42 %) and the ingest batch from 7.23 to 7.77 ms with the search index and from 4.01 to 4.44 ms without (+7.5 %, +11 %), medians of three interleaved runs. A profile put a fifth of the ingest's CPU in SQLite's parser: the expression was several kilobytes, parsed again for every statement that carried it — once per trace in every batch. The roll paid for some thirty JSON lookups per observation, twice, since `rollUserHour` now reads the tokens too. Read as text and decoded in Go, a statement carries no rule and a row costs one decode. It also leaves one rule where there were two to keep in step. |
| 18 | **2026-09-29** — **The budgets, measured.** *Ingest* (`BenchmarkIngestBatch`, its spans now carrying usage in four classes; interleaved runs of 300 batches each against `main`): without the search index 4.50 → 4.55 ms (+1.1 %, fastest of five +3.3 %), with it 8.92 → 8.21 ms, medians of five runs — inside 5 %. *Whole-hour roll* (one hour of 7,000 traces over 1,000 users, each a generation with usage; the `statsRoll` job seven times per run, five interleaved runs): 191 → 193 ms (+1.0 %) by wall time, 189 → 198 ms (+4.8 %) by CPU, medians of the runs' medians — inside 10 %, with `rollUserHour` now reading the tokens as well. These are #17's decode per row, measured after review on a machine at a load average of 11–45; the first form, the function with its memo, measured +0.7 % / +4.3 % and −5 % at a load average of 2–3. *Erasure chunks* (spec 047 #22 (f)'s agent profile, rebuilt with usage on each generation: 5,000 traces of the erased user and 5,000 of 200 others over thirty days, 200 spans a trace; 194 chunks, each timed around its job, three runs on fresh copies): checked as spec 047 #34 checks it, by each chunk's CPU time and by its fastest of three runs rather than by wall time on a shared machine: CPU p99 311–343 → 294–314 ms, fastest-of-three p99 435 → 410 ms. This spec does not move the chunk. The absolute figures are this harness's — the job timed through the writer with its commit, the whole process's CPU counted — and are not 047's instrumented stages, so they are compared with `main` on the same harness and corpus, not with 047's 250 ms. Sizes, on a copy of a demo store after the upgrade: a trace row 158.6 → 167.6 bytes, a `users_hourly` row 148.9 → 157.8, a `stats_hourly` row 148.7 → 150.8, a `users` row 69.6 → 82.3 and 50.9 in `idx_users_tokens` | #8 asks for the numbers rather than the expectation. |
| 19 | **2026-09-29** — **The backfill, measured.** On a copy of a demo store of 11,822 traces and 34,271 observations over 1,121 rolled hours, migration 0033 took 0.26 s (11,783 rows written) and the first pass after it, which re-rolls every hour, 1.9 s. The backfill is one pass over the observations with a model, about 7.5 µs each here: a store of ten million such observations would spend on the order of a minute and a quarter in the migration, before the server listens (the migration logs its start and its end, spec 043 #24) | #4 asks what the upgrade costs a store that already holds data. |
| 20 | **2026-09-30** — **The chart's tooltip is its legend, and the two new classes are hidden entries in it.** The Tokens chart draws lines, not a stack (spec 031 #6), and its cursor readout is uPlot's live legend. *Reasoning* and *Cache write* join that legend as series drawn only once their entry is clicked (`hidden` on the chart component's line), so the value at the cursor is there for every class and the drawn lines stay input, output and cache read. A window where they are the only lines with data draws them anyway, rather than an empty plot, and a click on the legend is kept when the chart is rebuilt (theme, range, refresh). The Tokens column on the three tables folds under the row's name on a narrow screen with the cost (`12.4k tokens`), and each table's fold width grows by one 96 px column (traces 992, sessions 816, users 944); a desktop window of 1,000 px, which left the sessions table its full 720 px, now folds it | #9 says the tooltip gains the classes and the series stay input and output; a hidden series is the one arrangement that satisfies both, since drawing reasoning beside output is the double count #1 refuses. The fold follows spec 006 #22: what a table drops from its columns it keeps under the name. |

## Schema

Migration `0033_tokens_everywhere.sql`:

```sql
ALTER TABLE traces ADD COLUMN input_tokens       INTEGER;
ALTER TABLE traces ADD COLUMN output_tokens      INTEGER;
ALTER TABLE traces ADD COLUMN cache_read_tokens  INTEGER;
ALTER TABLE traces ADD COLUMN reasoning_tokens   INTEGER;
ALTER TABLE traces ADD COLUMN cache_write_tokens INTEGER;
-- the same five on users_hourly and users
ALTER TABLE stats_hourly ADD COLUMN reasoning_tokens   INTEGER;
ALTER TABLE stats_hourly ADD COLUMN cache_write_tokens INTEGER;
CREATE INDEX idx_users_tokens ON users(project_id,
    (coalesce(input_tokens, 0) + coalesce(output_tokens, 0)) DESC, user_id);
WITH sums(...) AS (SELECT project_id, trace_id, /* refreshAggregates' sums */
                     FROM observations WHERE model IS NOT NULL AND model != ''
                    GROUP BY project_id, trace_id)
UPDATE traces SET ... FROM sums WHERE ...;
UPDATE stats_rollup SET last_pass = 0;
```

## API contract

| Where | Change |
|---|---|
| `GET /api/v1/traces` | Rows gain `tokens` (`fields=tokens`); `min_tokens` (#6, #13). |
| `GET /api/v1/traces/{id}` | Gains `tokens`. |
| `GET /api/v1/sessions`, `/sessions/{id}` | Rows and detail gain `tokens`. |
| `GET /api/v1/users`, `/users/{id}` | Rows and detail gain `tokens`; `sort=tokens`. |
| `GET /api/v1/stats` | `tokens` gains `reasoning` and `cache_write`; with `user_id`, `tokens` is present (#5). |

`tokens` everywhere: `{"input": 1200, "output": 340, "cache_read": 800,
"reasoning": 128, "cache_write": 0}`, each key present only when not `NULL`,
the object absent when all are. `openapi.json` declares it once, as the
`Tokens` schema, and every row and bucket references it.

## Testing

- **Classes** (#1, #12): a table over spellings, one observation each: every
  key of every list lands in its class, the first present wins, an unknown
  key lands in none, a count outside 0..10⁹ is `NULL`, a nested object is not
  a dotted key.
- **Never added** (#1, #3): an observation with `output` 100 and `reasoning`
  40 reads 100 in the headline number and in the rolled output.
- **The trace on write** (#2): a trace delivered in three batches reads the
  sum of its counted observations after each; a re-delivered span replaces
  its counts; an observation with `usage` and no `model` counts nowhere; a
  trace with no counted usage reads `NULL`.
- **Agreement** (#2, #5, #16): for a synthetic month, per class, the sum of
  the trace columns over a range equals `stats_hourly`'s below the watermark
  and the live scan's above it, and per user equals `users_hourly`'s, the
  summary's and the user page's.
- **Backfill** (#4): a store at the previous schema with traces, rollups and
  a frozen hour reads the trace columns immediately, the rollups after one
  pass — both what a fresh ingest of the same spans reads — and `NULL` in the
  frozen hour. The migration sums every class through the store's rule, over
  the observations that name a model.
- **Listings** (#6, #13): `min_tokens` bounds, with and without `NULL` rows,
  and its `400`s; `sort=tokens` pages with its cursor in both directions and
  ties on `user_id`; the plan uses `idx_users_tokens`; sessions sum per
  class.
- **Stats with `user_id`** (#5): tokens present on every grouping, seamless
  across the watermark.
- **Erasure and deletion**: erasing a user and deleting traces re-roll the
  new columns with the old ones.
- **The tree against the store** (#15): `UsageTokens` over a table of
  decoded usages equals the stored columns.
- **CLI and MCP** (#11): the columns and the show lines, the tree's number,
  `--min-tokens` and `--sort tokens` as the endpoint; every MCP tool that
  returns a row or a bucket declares `tokens` with its five classes.
- **Budgets** (#8): the ingest and whole-hour roll benchmarks before and
  after, and the erasure's chunks, in #18.
- **E2E (PR 2)**: the Tokens column and its tooltip, the filter, the Users
  sort, the user page's breakdown column.

## Edge cases

- **A trace whose observations report only `total_tokens`**: no class, so
  `NULL`. `total` is not a class, as in spec 031; the observation panel shows
  it, and so does the CLI's tree (#15).
- **A provider that reports reasoning inside output**: the headline counts it
  once, inside output; `reasoning` shows how much of it was reasoning.
- **A provider that reports reasoning beside output**: the headline
  under-counts the bill by the reasoning. The tooltip shows it. This is the
  price of never adding (#1), and the same one `cache_read` already pays.
- **A late span** changes the trace's columns in its own ingest transaction
  and marks the hour dirty, as for cost.
- **A frozen hour** keeps `NULL` for the new classes (#4).
- **Saturation**: sums in Go saturate as spec 031's do (`Tokens.Add`).

## Docs to touch

- `docs/api.md`: the statistics section (the two new classes, "never
  added", the headline number, `tokens` with `user_id`), the rows, the
  filters, the users sort.
- `docs/users.md`: tokens on users, the sort, the row size.
- `docs/cli.md`, `docs/mcp.md`, `docs/ui.md` (PR 2), `openapi.json`,
  `schema.d.ts`, the agent skill where it ranks traces by cost.
- Spec 031: nothing edited in place; this spec's Decisions amend #1, #2 and
  #11 by number.

## Out of scope

- A price table, or cost estimated from tokens (spec 002 #14).
- A sort on the trace or session listing.
- A tokens tile on the dashboard (#10).
- Tokens on runs, run comparisons and dataset items.
- Classes beyond five (audio, image, tool-use tokens) — a Decision on the
  list, not a schema change: the columns would be new, the pattern the same.
- An index for `min_tokens`.
