# Spec 025 — Quality trends: scores rolled up beside the statistics

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> A score lives on one trace. Open it and there is *hallucination 0.2*,
> *verdict pass*, a thumbs-up; ask "did hallucination drop after 2.5.0" and
> the interface has no page for the question, and the API has only a scan
> the design forbids at scale (design §5.2 names the trend query). Spec 013
> built the machinery that turns millions of rows into thousands and keeps
> them when the raw data goes; spec 023 rode it one dimension over. This
> spec rides it once more: scores rolled up per hour and name, a Quality
> screen with a card per score and a trend behind each, and the same
> numbers in the API, the CLI and MCP.

---

## Overview

Deliverables, one PR (the last commit flips the status):

- Schema 0015: `scores_hourly` — spec 013's dimension tuple plus the score's
  name, type and category — rolled by spec 013's aggregator in the same
  pass, with a one-line backfill (Decisions 1–5).
- `GET /api/v1/stats/scores` — the score series and breakdowns, answered
  through spec 013's read seam (Decisions 6–7). CLI `scores trend`; MCP
  `get_score_trends` (Decision 8).
- The *Quality* screen under *Evals*: an overview with a card per score
  name and a detail view with the trend and the three breakdowns
  (Decisions 9–12).
- `docs/quality.md` (new), `docs/scores.md`, `docs/api.md`, `docs/cli.md`,
  `docs/mcp.md`, `docs/retention.md`, `docs/ui.md`, `openapi.json`,
  `schema.d.ts`.

Not here: per-user quality (spec 023 out of scope), distributions or
percentiles of a score (Decision 3 keeps the row format open for them),
scores by their source, text scores, scores that name no trace.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-08** — A third rollup table, **`scores_hourly`**, keyed by spec 013's tuple **plus `name`, `data_type` and `category`**: `(project_id, hour, environment, release, model, name, data_type, category)`. A score is counted in **the hour of the trace it names** — `HourOf(trace.timestamp)` — and takes the trace's environment and release (owner decision 2026-09-08). A score with an `observation_id` lands in the row whose `model` is that observation's model; a trace-level score in the `model = ''` row (spec 013 #1's discriminator, read the same way). A score that names **no trace** — `session_id` only — is not in the table; a `text` score is not either | The hour of the trace is the hour whose *traffic* the score judges, which is what makes the curve line up with the traces and cost curves on the Stats screen and what gives every row an environment, a release and a model without inventing any. Bucketing by the score's own `timestamp` — "when it was graded" — is the simpler join but answers a different question, one that drifts from the traffic by however long the judge took. A session-only score has no trace to borrow a tuple from, and spec 003 #4 lets it exist for exactly that reason; a text score has nothing to add up. Both are stated in `docs/quality.md`. Cardinality: the spec 013 tuple × names × (categories for a categorical name), and a name with thousands of distinct categories pays linearly for that choice, as spec 013 #1 says of releases. |
| 2 | **2026-09-08** — Per row: `count`, `sum`, `min`, `max`. Numeric: all four over `value`. Boolean: `count` and `sum` (the number of `1`s; `min`/`max` NULL). Categorical: one row per `string_value` with `count` (the rest NULL, `sum` 0). Mean and rate are read-time divisions | Four numbers that merge exactly across hours: the mean of a day is `Σsum / Σcount` over its 24 rows, the rate likewise, and the extremes are extremes. A distribution — p50 of a score, "how many below 0.5" — does not follow from these and is deliberately left out (owner decision 2026-09-08): it needs a histogram whose buckets depend on the config's range, and this row can grow a JSON column later exactly as `stats_hourly` carries its `latency`. |
| 3 | **2026-09-08** — The rollup rides spec 013's **aggregator**: the same pass, the same `(project, hour)` `WriteJob` writes the three tables, the same watermark, the same frozen-hour rule (spec 013 #11, #14), the same `stats_retention_days` sweep. **The dirty set grows by the scores' own changes**: a score written since the last pass — `scores.created_at > last_pass`, which an upsert moves too (spec 003 #3) — dirties the hour of its trace, through a new index `(project_id, created_at)`. A trace whose own hour moved (spec 013 #16) carries its scores with it, since they are keyed by *its* hour and the existing rule already re-rolls both hours | Three tables, one job, one watermark: the second and third aggregators spec 023 #2 refused would each be a place for spec 013's five reviews of corrections to go wrong again. The trace-side dirtying is inherited for free precisely because Decision 1 keys a score by the trace's hour — nothing about a score's own time enters the key. The score-side dirtying exists because a score's arrival touches no trace row: a judge grading yesterday's traffic today writes only `scores`, and `traces.updated_at` — spec 013 #15's whole mechanism — never hears of it. |
| 4 | **2026-09-08** — **Deleting a score** (spec 022's `DELETE /scores/{id}`) re-rolls the trace's hour before it answers, the way a user-data erasure does (spec 013 #7): the handler submits `RollHour` for that one hour when it lies behind the watermark; a frozen hour stays as it is, which the docs say. A user-data erasure, which deletes scores with their traces, corrects `scores_hourly` through the same re-roll it already performs; project purge cascades | A deleted row is not found by `created_at > last_pass`: it is gone. Erasure solved the same problem by re-rolling the emptied hours in the request, and one score is one hour, cheaper than erasure's set. The alternative — a dirty-hours table written by every deletion path — is a second bookkeeping table for one caller. |
| 5 | **2026-09-08** — Migration 0015 ends with `UPDATE stats_rollup SET last_pass = 0` (spec 023 #15's backfill), so the first pass after an upgrade re-rolls every rolled hour and writes the score rows for the whole history. `rolled_until` is not touched, for spec 023 #15's reason; hours past the trace-retention window are frozen and get no score rows, ever | Spec 023 met this on the demo stand — a watermark already past the whole history, every hour rolled, nothing dirty, two empty tables for ever — and the fix is one line that costs one pass. The scores of a frozen hour may still exist (retention sweeps them with their targets, spec 003 edge cases), but the traces that give them an hour and a tuple do not, which is the same position spec 013 #11 takes about a late span. |
| 6 | **2026-09-08** — **`GET /api/v1/stats/scores`**: filters `from`/`to`/`environment` as `/stats`, `group_by=hour\|day\|environment\|release\|model` (default `day`), optional `name=`. Response `{"group_by", "targets": "any"\|"observation", "series": [{"name", "data_type", "buckets": [{"key", "count", …}]}]}` — per bucket a numeric series carries `mean`, `min`, `max`; a boolean series `rate`; a categorical series `categories: {value: count}`. Without `name` every name in the range is a series; with it, one. Grouped by `model` only observation-level scores count and `targets` says `observation`; otherwise every score counts once and `targets` says `any`. An unknown parameter or an empty value is a `400` (spec 003 #21, #23) | A new endpoint rather than a `metric=` on `/stats`, because the answer is a different shape — a series per name, a distribution per categorical bucket — and spec 004 #23 made `unit` part of the answer so that unlike counts never look alike; `targets` is that honesty here, since a trace-level score has no model and a model grouping cannot count it. Every name at once is what the overview needs in one request; `name` is what the detail view needs. |
| 7 | **2026-09-08** — The endpoint answers through **spec 013's read seam**: hours behind the watermark from `scores_hourly`, hours at or past it — and hours older than the rollup's oldest row (spec 013 #17) — from a live scan of `scores` joined to `traces` (and to `observations` for the model), merged before bucketing; the same watermark, the same floor, the same lag sentence in the docs | One seam, one set of rules, one place for the lag to be stated; the live half is the query design §5.2 describes, bounded to the tail. |
| 8 | **2026-09-08** — **CLI** `scores trend [--name N] [--group-by G] [--from] [--to] [--environment]`; **MCP** `get_score_trends` with the same five arguments — each the one endpoint (spec 004 #16, #17). `tracepad stats` is unchanged | Three clients, one surface (spec 004 #1). The verb sits under `scores` because that is the noun; `stats` keeps answering the traffic question it always did. |
| 9 | **2026-09-08** — ***Quality* is an item in the *Evals* group**, after *Queues*, icon `chart-spline`, route `/quality` (owner decision 2026-09-08: a separate screen, not a tab or block on Stats). Filter bar as Stats': `RangePicker` (`?from=&to=`, default the last 30 days), the environment box, the hour/day bucket choice (`?group_by=`) | Quality is what evals produce, so it sits with datasets, runs and the annotation desk rather than with traffic. Stats stays a screen about traces; the shared filter bar is what keeps the two answering the same window. |
| 10 | **2026-09-08** — **Overview** (`/quality` without `name`): one request without `name`, one **card per series** in a grid, ordered by the score configs' order first (spec 014) and unconfigured names after, alphabetically. A card is the name, the type, the count in the window, and a small `Chart`: the mean for numeric (with the config's `min`/`max` as the axis when both are set), the rate for boolean, one line per category for categorical (share of the bucket). The card is a link to the detail view (owner decision 2026-09-08: everything at once) | A dashboard is what "how are we doing" wants: six names, six sparklines, one glance. The configs give the order and the axis; a name without one still shows, because the trend is real whether or not somebody declared it. |
| 11 | **2026-09-08** — **Detail** (`/quality?name=X`): the same screen with the name in the header and a back link, the trend as a full-width `Chart` (numeric: mean with min and max as two fainter lines; boolean: rate; categorical: a line per category), a count `Chart` beside it, then three breakdown tables — model, environment, release — each row the key, the count, and the same summary (mean / rate / the category distribution as `value n · value n`). Four requests: the series, and the three groupings | The detail view is the Stats screen's composition for one score, which is what keeps it inside the budget (Decision 13); the breakdown by model is the question a judge score on generations exists to answer. |
| 12 | **2026-09-08** — Empty states: no series in the window → *no score names a trace in this window* with the SDK line and a link to `docs/scores.md`; a `name` the range does not hold → the detail view's own empty state, not a 404. The overview's cards render the config's `description` as the card's title attribute | The trend of a score that has not been recorded yet is a real answer, and the SDK line is how it starts being recorded. |
| 13 | **2026-09-08** — The screen lands **under the 18,000 ceiling** (spec 024 #17), the PR reporting the number per screen. If the measurement says otherwise, the numbers come first and the cut is a decision, not a silent trim; a breakdown component for the score summary is written fresh only if generalising `BreakdownTable` costs more lines than it saves | Two screens landed at 16,714; this is one screen of composition over `Chart`, `RangePicker` and a table, and about a thousand lines are free. |
| 14 | **2026-09-08** — Every new query against `scores`, `traces` and `scores_hourly` ships with an `EXPLAIN QUERY PLAN` test naming the index it must use (the hour roll, the score-side dirty query, the seam's range read, the live tail), on the method of spec 003 #25 and spec 023's unary `+` | The store never runs `ANALYZE`; spec 023 lost 29× to a planner that picked an index by its leading column, and the fix was found by a test that reads the plan. |
| 15 | **2026-09-08** (from the implementation) — The CLI's window flags are **`--since`, `--until` and `--env`**, not the `--from`, `--to` and `--environment` Decision 8 spells; MCP keeps the endpoint's own five names, as its contract requires | Decision 8's list is the *endpoint's* parameters, which is what spec 004 #1 asks all three clients to be a client of — and MCP mirrors query parameters by name on purpose. The spelling of a *flag* is the command line's own convention, fixed once by spec 007 #11 and used by `tracepad stats`, which is the command this one sits beside and is read against. Two names for one window on two neighbouring commands is exactly the drift #11 exists to prevent, and `--json` prints the endpoint's own bytes either way, so nothing about the contract moves. |
| 16 | **2026-09-08** (from the implementation) — The breakdown is a **new component**, `ScoreBreakdown.svelte` (44 lines), rather than a generalised `BreakdownTable`. Decision 13's condition, resolved | `BreakdownTable`'s three columns are traces, errors and cost, and a score has neither an error count nor a cost; worse, its third column *means* something different per data type — a mean, a rate, a distribution. Making it take a column spec would have been a prop-and-type change to five call sites on two shipped screens (Stats' three tables, the user page's two) in service of one new caller, which is more lines and more risk than the forty-four. The bar, the empty state and the truncation rule are copied deliberately, so the two tables read as one table. |
| 17 | **2026-09-08** (from the implementation) — `Chart` gains two optional props: **`range`**, a y axis pinned to given bounds, and **`sync`**, which cursor group the chart belongs to. Both default to what the shipped screens already do | Decision 10 asks for the config's `min`/`max` as the axis, which uPlot expresses as a scale range and the component had no way to be given: drawing 0.2..0.3 across the full height makes a stable score read as a cliff, which is the opposite of what a quality chart is for. `sync` follows from the same screen — the four Stats charts are stacked and read together, so one x cursor across them is right, while a grid of cards about *different scores* is not one question and a cursor moving on six of them at once is a cursor nobody put there. |
| 18 | **2026-09-08** (from the implementation) — A score whose observation carries **no model** — an `event`, a `span`, a guardrail without one — lands in the `model = ''` row beside the trace-level scores, and is therefore not counted by `group_by=model`. Decision 1's "the row whose `model` is that observation's model", read literally | The alternatives are worse in both directions: keying such a row by the observation id would add a dimension nobody asked for and would break the property that summing over models counts each score once, and counting the empty-model row under a model grouping would put every trace-level score in it. `targets: "observation"` already says that a model grouping counts a subset rather than the whole; `docs/quality.md` says which subset. |
| 19 | **2026-09-08** (from the measurement) — The live half of the seam takes a **unary `+` before `s.name`**, so that a `name` filter cannot be read as an index key. Decision 14's check is what found it | Without it the planner drives the join off `idx_scores_name`, which carries no `trace_id`: for **every trace of the asked range** it walks **every score of that name in the project** and compares `s.trace_id = t.id` by hand. Nothing here runs `ANALYZE`, so a shipped database has no `sqlite_stat1` and the planner has no way to know the trace side is the selective one. Measured on the month fixture — 36,000 traces, one score name — a 30-day range took **1,100 s** without the `+` and **0.28 s** with it; the rollup answers the same range in 1.7 ms. It is the same defect and the same fix spec 023 found twice (`sessionStartCondition`, `dirtySessionHoursQuery`), which is why #14 asked for the plan to be asserted rather than assumed — and the assertion now names the index that must *not* appear. |

## Data contract (schema 0015)

```sql
CREATE TABLE scores_hourly (
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    hour        INTEGER NOT NULL,          -- Unix seconds, top of the hour, UTC: the trace's hour
    environment TEXT NOT NULL,
    release     TEXT NOT NULL DEFAULT '',
    model       TEXT NOT NULL DEFAULT '',  -- '' = trace-level scores; else the observation's model
    name        TEXT NOT NULL,
    data_type   TEXT NOT NULL CHECK (data_type IN ('numeric', 'boolean', 'categorical')),
    category    TEXT NOT NULL DEFAULT '',  -- categorical: the string_value; '' otherwise
    count       INTEGER NOT NULL,
    sum         REAL NOT NULL,             -- numeric: Σ value; boolean: Σ value; categorical: 0
    min         REAL,                      -- numeric only
    max         REAL,                      -- numeric only
    PRIMARY KEY (project_id, hour, environment, release, model, name, data_type, category)
) STRICT;
CREATE INDEX idx_scores_hourly_name ON scores_hourly(project_id, name, hour);

-- The score-side dirty set (Decision 3).
CREATE INDEX idx_scores_created ON scores(project_id, created_at);

-- The backfill (Decision 5).
UPDATE stats_rollup SET last_pass = 0;
```

The `(project, hour)` job of spec 013 gains, in the same transaction, a
`DELETE`/`INSERT … SELECT` over `scores_hourly` for the hour: scores joined
to the hour's traces on `(project_id, trace_id)`, and to observations on
`(project_id, trace_id, observation_id)` for the model, `WHERE trace_id IS
NOT NULL AND data_type != 'text'`, grouped by the tuple. `system` counts
the table.

## API contract

- `GET /api/v1/stats/scores` — Decision 6; `openapi.json` + `schema.d.ts`
  in one commit; router ↔ openapi parity. `docs/api.md` gains a *Score
  trends* section beside *Statistics*, with the seam and lag sentences
  shared by reference.
- `DELETE /api/v1/scores/{id}` — unchanged shape; Decision 4's re-roll
  stated in `docs/scores.md`.
- `docs/quality.md`: what is counted and what is not (Decision 1), the
  four numbers and how a day is formed (Decision 2), the lag, the
  cardinality in numbers, the frozen-hour and retention interplay, the CLI
  and MCP lines. `docs/retention.md` *what outlives what* gains the table.

## Application contract

| Route | Screen |
|---|---|
| `/quality` | Decision 10; empty state per Decision 12. |
| `/quality?name=X` | Decision 11; empty state per Decision 12. |

`Sidebar` gains the item. The API client gains `getScoreTrends` and a pure
`$lib/quality.ts` for the series shaping (the mean/rate/share per bucket
over the range's x axis, `buildSeries`' way) and the card order; the
query-parameter set gets the openapi parity test (spec 016 #13). Both
themes; console clean; 375 px never scrolls the page; the cards stack on
the narrow layout as Stats' charts do.

## Testing

- **Go, rollup**: one hour rolls `scores_hourly` equal to a live scan over
  the same hour (tuples, counts, sums, extremes); an observation-level
  score lands in its model's row and a trace-level one in `model = ''`;
  session-only and text scores produce no rows; re-delivery is idempotent;
  frozen hours untouched.
- **Go, dirtying**: a score posted after the pass for an already-rolled
  hour dirties that hour and the next pass corrects it (a new score, an
  upserted one); a deleted score corrects its hour before the `DELETE`
  answers; a trace whose timestamp moved carries its scores to the new hour
  and the old hour drops them.
- **Go, read seam**: a range straddling the watermark equals live truth per
  series and bucket; a range fully behind it touches no raw table (asserted
  by dropping the raw rows and asking again); day buckets equal the merge
  of their hours (mean, rate, min/max, categories); `group_by=model` counts
  only observation-level scores and says so; `name` narrows to one series;
  `400` on unknown or empty params.
- **Go, retention interplay**: the trace sweep leaves `scores_hourly`
  intact; `stats_retention_days` sweeps it with the other two; user-data
  erasure corrects the touched hours; project purge leaves no rows; the
  0015 backfill fills the table on a database rolled before it.
- **Go, cost**: ingest benchmark unchanged (the aggregator is off the write
  path); one pass timed with and without scores on the benchmark fixture,
  both numbers in the PR; `EXPLAIN QUERY PLAN` tests per Decision 14.
- **CLI / MCP / openapi**: parity; `scores trend` and `get_score_trends`
  byte-equal to the endpoint.
- **Vitest**: the series shaping (a gap stays a gap; mean and rate from the
  four numbers; a categorical bucket's shares sum to one; the config's
  range becomes the axis); the card order; the URL state.
- **e2e** (`quality.spec.ts`, own project, spec 016 #18): deliver fixtures,
  post numeric, boolean and categorical scores on traces and one on an
  observation, drive a pass the way `users.spec.ts` does → the overview
  shows a card per name with points; a card opens the detail; the model
  breakdown lists the observation's model only; the environment box and
  the range narrow the numbers; a score posted after the pass appears
  after the next; 375 px.
- **Mutations** (table in the PR): the score-side dirty query dropped; the
  delete path's re-roll dropped; text scores admitted; the model grouping
  counting trace-level rows; the mean computed as a mean of means.
- **Lines and bytes**: `make ui-lines` under 18,000 with the per-screen
  breakdown; `dist` under spec 015 #10's ceiling.
- **Chrome (DoD)**: both views on the demo corpus after a pass, both
  themes, 375 px, console clean, screenshots in the PR.

## Edge cases

- **A score for a trace that has not arrived**: no hour to land in; when
  the trace arrives, its hour is the live tail or is dirtied by the
  trace's own write, and the score is counted then.
- **A score in the live tail only** (graded after the last pass): counted
  by the live half of the seam immediately, since the trace's hour is
  either live or re-rolled by the next pass — the docs give the lag.
- **One name, two data types** (a name without a config graded both ways):
  two series with the same name, distinguished by `data_type`; the
  overview shows two cards.
- **A categorical name with a config whose categories the data exceeds**:
  the rows are what the data says; the card shows every category seen.
- **A numeric config without `min`/`max`**: the axis is the data's range.
- **`stats_retention_days` shorter than the window asked**: hours older
  than the rollup's oldest row come from the live scan, as spec 013 #13
  and #17 say for `/stats`.
- **A score's trace in a frozen hour**: the score is listed on the trace
  and absent from the trend; `docs/quality.md` says so beside spec 013's
  own sentence.

## Config additions

None (the aggregator's interval and `stats_retention_days` govern all
three tables).

## Out of scope

Distributions and percentiles of a score (a histogram column, later);
per-user quality; grouping by the score's source or by the annotator; text
scores; session-only scores; alerting on a trend; comparing two ranges
side by side.
