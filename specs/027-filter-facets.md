# Spec 027 — Facets: many-valued filters, and the values to pick from

**Status:** 🟡 DRAFT
**Sprint:** September 2026

> The filter panel asks for an environment as text, in a box whose
> placeholder is `production`. The store knows the eight environments the
> project has ever used, and says so on the Stats screen, one screen away.
> The same is true of the release and of the trace name: three columns
> whose values are a short, finite set per project, filtered today by
> typing one of them from memory, exactly, one at a time. This spec makes
> the three filters *any of* a list, and gives the panel the list — with
> counts, for the range in view — from the rollup that already has two of
> the three columns, plus one more hourly table for the third.

---

## Overview

Deliverables, one PR (the last commit flips the status):

- `environment`, `release` and `name` accept a comma-separated list on
  every endpoint that takes them as a trace filter; the match is *any of*
  (Decision 1).
- `GET /api/v1/facets`: the distinct values of the three columns over a
  time range, each with its trace count, from the rollup behind the
  watermark and the live tail past it (Decisions 2–4).
- Schema 0016: `names_hourly`, rolled by spec 013's aggregator in the same
  pass, frozen and swept by the same rules (Decision 3).
- CLI `facets`, MCP `get_facets`; `--env`, `--release`, `--name` pass the
  list through (Decision 5).
- The filter panel: the three fields become checkbox lists with counts,
  loaded when the panel opens, for the range in view (Decisions 6–8).
- The application-line ceiling rises to 18,500 (Decision 9).
- `docs/api.md`, `docs/cli.md`, `docs/mcp.md`, `docs/ui.md`,
  `docs/stats.md`, `openapi.json`, `schema.d.ts`.

Not here: lists for `user_id`, `session_id`, `version`, `tag` (the first
two are unbounded and have their own screens; `version` is not in any
rollup; tags stay ANDed); a *none of* operator; facet counts that respect
the other filters; hidden-by-default environments; free-text entry beside
the list.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-09** — `environment`, `release` and `name` on `GET /traces` (with or without `q=`), `GET /traces/last` and the CLI/MCP mirrors, plus `environment` on `GET /sessions`, `GET /stats` and `GET /stats/scores`, accept **a comma-separated list**; a trace matches when its column equals **any** item. One item behaves exactly as today. Items are trimmed; an empty item (`a,,b`, `a,`) is a `400` by spec 003's empty-value rule; duplicates collapse. A value containing a comma is not expressible through these parameters, and `docs/api.md` says so. `tag` keeps its repeatable form and its AND (owner decision 2026-09-09: a list in one parameter) | One parameter reads as one filter in a URL, a shell and a chip, which is where these values are typed and shown; `?environment=production,staging` is the string a person would write unprompted. Environments, releases and trace names are identifiers, and an identifier with a comma in it is a choice its owner made against every tool that will ever list it. `tag` is not changed because its two forms already differ from these on purpose — a tag list is a conjunction — and because it is repeatable in the API today; two spellings for two semantics is clearer than one spelling for both. The SQL is `col IN (?, …)` over the index the column already has, so the cost is the single-value cost times the list. |
| 2 | **2026-09-09** — **`GET /api/v1/facets?from=&to=`** returns `{from, to, environment: [{value, count}], release: [...], name: [...], omitted: {environment, release, name}}`: the distinct values of each column among the traces in the range, with the number of traces carrying each, sorted by count descending then value; at most **100** values per column, the rest counted in `omitted`. `release` omits the empty string (a trace with no release is not a value to pick); `name` omits `NULL`. `from`/`to` as on `/stats` (half-open, default the last 30 days — the listing's own default range) | The panel needs one answer per opening, and three columns in one round trip is that answer; three endpoints would be three fetches racing three spinners. Counts ride because they cost nothing from the rollup (`SUM(count)`) and because they are what tells a reader that `prod: 1` is a typo and `production: 4656` is the environment (owner decision 2026-09-09). The cap exists for the project that emits a release string per commit: a hundred is more than a checkbox list can show, and `omitted` says the list is not the whole truth rather than pretending. |
| 3 | **2026-09-09** — The answer splits at the project's watermark the way `/stats` does (spec 013 #5): hours behind it from the rollup — `stats_hourly` trace-unit rows (`model = ''`) for `environment` and `release`, and a new **`names_hourly`** `(project_id, hour, name) → count, error_count` for `name` — and hours at or past it from the live scan over `traces` bounded by `idx_traces_timestamp`. `names_hourly` is written by the same `(project, hour)` `WriteJob` as the other three tables, frozen by its own rows (spec 025 #21, spec 026 #7), swept with `stats_retention_days`, and answered live past that window (spec 013 #13). Migration 0016 creates it and ends with `UPDATE stats_rollup SET last_pass = 0` (spec 023 #15) so the first pass backfills it | Two of the three columns are already in the tuple; the third is not, and putting it there would multiply `stats_hourly` by the number of names on every row and touch every stats query for a column none of them reads. A table of its own with the smallest tuple that answers the question is spec 025's move one more time, and it rides every rule the aggregator already enforces — one watermark, one dirty set, one freeze, one sweep. The live tail is what keeps the list *complete*: an environment first seen a minute ago is in the panel now, not after the next pass, without the scan spec 013 exists to avoid (the tail is minutes of one project). `error_count` is in the tuple because it is free and because "which trace names fail" is the next question the same table answers. |
| 4 | **2026-09-09** — The facets endpoint takes **only the range**: the counts do not respect the other filters in the panel, and the list is the same whichever boxes are checked | Filter-aware counts mean a different query per combination, a refetch on every checkbox and the "why did production vanish from the list when I picked staging" confusion every faceted search has to explain. The comparison product that motivated this spec makes the same choice for the same reason, and the time range is the one filter that actually changes what values exist. |
| 5 | **2026-09-09** — **CLI** `facets [--since 30d] [--until T]` prints the three lists with counts; `--env`, `--release`, `--name` on `traces ls`, `traces last`, `tail`, `sessions ls` (`--env` only) and `stats` pass the string through, so `--env production,staging` is the list. **MCP** `get_facets` with `from`/`to`; the `environment`, `release` and `name` arguments of the existing tools document the list form | One surface, three clients (spec 004 #1). Passing through rather than parsing keeps the CLI a mirror of the URL: what the flag accepts is exactly what the parameter accepts. |
| 6 | **2026-09-09** — In `FilterBar`, `environment`, `release` and `name` become a new field kind, **`facet`**: a checkbox list of the values `GET /facets` returned for the panel's `from`/`to`, each with its count, sorted as returned; a value present in the URL but not in the list is shown checked at the top without a count (it came from a link, and unchecking it must be possible). The list loads **when the panel opens** and refetches when `from`/`to` change while it is open; while loading, the checked values render alone; on failure, the field shows the shared failure line and the checked values. Above eight values the list gets a filter box that narrows it by substring. No free-text entry (owner decision 2026-09-09: the list, with the live tail, is complete) | A checkbox is how a person says *these* rather than *this*, and the count beside it is what makes the choice informed. Loading on open rather than with the page keeps the listing's first paint what it is: the panel is opened by a minority of visits. The out-of-list value exists because a URL is a document — a link to `?environment=canary` from before canary was retired must still show that it filters, and must still be undone. The substring box appears only when a list is long enough to need one, for the reason the search box is not on every listing. |
| 7 | **2026-09-09** — The chip for a many-valued filter reads `Environment: production, staging` up to two values and `Environment: 3 values` beyond; the tooltip lists them all. The URL carries the comma form (`?environment=production,staging`), so the listing, the chips and the API spell the filter identically | The chip row is where the current filter is read at a glance, and a glance holds two names. Above that, the number is the information; the tooltip keeps the names one hover away. |
| 8 | **2026-09-09** — The user page's tabs and the Stats screen's environment filter take the same list form through the same clients (`environment?: string` stays a string in `TraceFilters`, `SessionFilters` and the stats query; the split is the server's); `RangePicker` is unchanged. The sessions panel shows the facet list for `environment` only, from the same endpoint | The client already treats the value as an opaque string it puts in the URL, which is exactly right for a comma list: nothing in the interface parses it except the chip. |
| 9 | **2026-09-09** — The application-line ceiling rises from 18,000 to **18,500**. `main` after spec 026 is 17,554; the facet field (list, filter box, loading and failure states), the chip, the facets client and the sessions panel's share are estimated at 350–450 lines, and one review cycle needs room | Spec 026 was the revision the ceiling exists to trigger, and it came out neutral (spec 026 #12): the duplication it removed paid for the header fix and the checker. This spec is a feature with a component that has no cheaper shape — a checkbox list *is* its lines — and 18,500 is `main` plus the measured estimate plus a cycle, the way every raise since spec 015 #9 has been justified. The PR reports `make ui-lines` before and after, by file. |
| 10 | **2026-09-09** (from the implementation) — `GET /api/v1/system` counts `names_hourly` beside the other three rollup tables | Spec 013 #8's rule is "a store whose size an operator cannot infer from the rows they can already see", and this is one: it multiplies by the number of distinct trace names, which is exactly the shape — a request id in the trace name — an operator wants to see before the file grows. Asked of `stats_hourly` (#8), `users_hourly` (spec 023) and `scores_hourly` (spec 025) already; the fourth table costs one `COUNT(*)` in a loop that already runs and no schema change, since `/system`'s `rows` is an open map. |
| 11 | **2026-09-09** (from the implementation) — The client method is `api.getFacets` on the shared `Api` class in `ui/src/lib/api/client.svelte.ts`, and `ui/src/lib/api/facets.ts` holds the **pure** part — the comma form, the options a list draws, the chip. The Application contract's `ui/src/lib/api/facets.ts: getFacets({from, to})` reads as one file doing both | Every other endpoint's fetch is a method on `Api`, and the `*.ts` beside it is the pure half that a test can reach without a DOM: `quality.ts`, `users.ts`, `runs.ts`, `traces.ts` are all that shape. A second place that issues requests would have its own answer to the auth header, the 401 handling and the version header, which is what that class exists to have one of. The split is the contract's, spelled the way the code already spells it. |
| 12 | **2026-09-09** (from the implementation) — The Overview's `docs/stats.md` does not exist, and what it names is `docs/retention.md`'s table of the rollup — now **four tables** — plus `docs/api.md`'s *Where the numbers come from*. Both are updated; no file is created | The statistics have never had a page of their own: `api.md` documents the endpoint and `retention.md` documents what the rollup keeps and for how long, which is where a fourth table has to appear or the sweep's promise is written down for three of four. Creating a `stats.md` to satisfy the list would put the rollup's story in two places, which is the duplication `AGENTS.md` opens by refusing. |

## Data contract (schema 0016)

```sql
-- Trace names, rolled up beside the traffic (spec 027 #3). One row per
-- (project, hour, name); a trace with no name is not in it.
CREATE TABLE names_hourly (
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    hour        INTEGER NOT NULL,   -- Unix seconds, top of the hour, UTC
    name        TEXT NOT NULL,
    count       INTEGER NOT NULL,
    error_count INTEGER NOT NULL,
    PRIMARY KEY (project_id, hour, name)
) STRICT;

-- The first pass after the upgrade backfills it (spec 023 #15).
UPDATE stats_rollup SET last_pass = 0;
```

Rolled in `statsRoll.apply` after `rollStats`/`rollUsers`, by the same
`DELETE` + `INSERT … SELECT` shape, with `hourFrozenIn(tx, "names_hourly",
…)` deciding the freeze for this table alone. The sweep that trims
`stats_hourly` to `stats_retention_days` trims this table in the same job.
`internal/store/facets.go` holds the read side: `FacetRows(projectID,
fromHour, toHour)` over the three rollup sources and `FacetTail(projectID,
from, to)` over `traces`, merged by the server the way `/stats` merges its
halves.

## API contract

| Endpoint | Change |
|---|---|
| `GET /traces` (with or without `q=`), `GET /traces/last` | `environment`, `release`, `name`: comma-separated, any of (Decision 1). Empty item → `400 "environment: empty item in list"`. |
| `GET /sessions` | `environment`: the same. |
| `GET /stats`, `GET /stats/scores` | `environment`: the same. `/stats/scores?name=` is the **score** name and is unchanged. |
| `GET /facets` | New (Decision 2). `from`/`to` as `/stats`; an unknown parameter or an empty value is a `400`. Answer: `{"from": …, "to": …, "environment": [{"value": "production", "count": 4656}, …], "release": [...], "name": [...], "omitted": {"environment": 0, "release": 0, "name": 0}}`. Sorted by count descending, then value ascending. |

`openapi.json` describes the three parameters as `type: string` with the
list form in the description (the wire shape does not change — a client
that sent one value goes on working), and documents `/facets` in full.

## Application contract

`ui/src/lib/api/facets.ts`: `getFacets({from, to})`. `FilterBar` gains the
`facet` kind (Decision 6) for `environment`, `release`, `name`, renders the
chip form of Decision 7, and keeps the field-name parity test with
`openapi.json` (spec 016 #13) — `/facets`'s keys are checked against the
three field names. `TraceFilters`, `SessionFilters` and the stats query keep
`environment` as a string. `docs/ui.md`'s filter-bar paragraph names the three lists and
where the values come from. Both themes; console clean; 375 px never
scrolls the page; the list scrolls inside the panel past ten values.

## Testing

- **Store**: `IN` lists for each of the three columns on the listing, the
  session listing and the stats live half; one value reproduces today's
  rows exactly (the existing tests, unchanged); `EXPLAIN QUERY PLAN`
  asserts `idx_traces_environment` for an environment list.
- **Rollup**: `names_hourly` rolled for an hour; re-rolled when a late
  trace lands in it; a nameless trace absent; frozen by its own rows with
  `pastTheWindow()` (not "+2 h" — spec 025's trap) and the two siblings
  from spec 026 #7's suite; swept with `stats_retention_days`; the 0016
  backfill fills it on the first pass after the migration.
- **Facets**: a range entirely behind the watermark answers from the
  rollup alone (the raw rows deleted first, as spec 013's seam test does);
  a range past it answers from the tail; a range across it sums both
  without double-counting the watermark hour; empty release and `NULL`
  name omitted; the cap and `omitted`; sort order; empty item → `400`.
- **Server**: `?environment=a,b` on each endpoint; `a,,b` → `400`;
  `a, b` trims; `a,a` collapses; `/stats/scores?name=` still the score
  name; `/facets` parameter validation.
- **CLI / MCP**: `facets` output; `--env a,b` passes through; `get_facets`.
- **UI (vitest)**: the facet field — checked-from-URL value outside the
  list, the filter box above eight, loading and failure states, the chip
  forms; `prune` keeps a list.
- **e2e** (`filters.spec.ts`): open the panel on `/traces`, tick two
  environments, apply → URL carries `environment=a,b`, every row shows one
  of the two, the chip reads both; tick a name → rows match; a deep link
  with an unknown environment shows it checked and removable; 375 px.
- **Live check** on a copy of the demo database: the first pass after
  0016 fills `names_hourly` (count the rows, time the pass, put both in
  the PR), the panel lists the eight environments with the counts the
  Stats breakdown shows, a new environment ingested while the panel is
  open appears on reopen without waiting for a pass.

## Edge cases

- `?environment=production` — one value, one condition, as today.
- `?environment=production,` — `400`; `?environment=` — `400` (spec 003).
- A value with a comma — not expressible; the docs say so (Decision 1).
- A URL with a value absent from the list — checked at the top, no count,
  removable (Decision 6).
- More than 100 values in a column — the top 100 by count, `omitted`
  carries the rest, the panel says "and N more" under the list.
- Before the first pass ever ran — the watermark is zero, the answer is
  the live scan, the way `/stats` is (spec 013 #5).
- `stats_retention_days` shorter than the range — hours past the window
  come from the live scan (spec 013 #13).
- The panel open while `from`/`to` change — refetch, checked values kept.
- Traces with `name IS NULL` — absent from the name facet and from
  `names_hourly`; they still list, and `?name=` never matches them.

## Config additions

None. `stats_retention_days` governs the new table as it does the others.

## Out of scope

`user_id` and `session_id` lists; *none of*; filter-aware counts; hidden
environments; `version` and `tag` facets; free-text entry beside the list;
a facet for observation-level columns (`model` is on Stats already).
