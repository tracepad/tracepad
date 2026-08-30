# Spec 013 — Stats rollup: history that outlives the raw data

**Status:** 📝 DRAFT
**Sprint:** September 2026

> The statistics screen answers from a full scan of the raw rows, twice over
> the envelope the README publishes: a month's chart at a million spans a day
> is a scan of tens of millions of rows per page view, with every latency of
> a bucket buffered in Go to sort for a percentile. And the moment a
> retention window is set, the sweep that deletes a trace also deletes the
> only place its numbers lived — configuring retention silently amputates
> the charts. This spec adds an hourly rollup: a few thousand rows a month
> that answer any range in milliseconds, and that stay when the raw data
> goes.

---

## Overview

Deliverables, one PR (REPOS §2):

- A `stats_hourly` table: per project, hour, environment, release and model —
  counts, errors, cost, and a latency histogram. Thousands of rows where the
  raw tables hold millions.
- A background **aggregator** beside the sweeper: rolls up closed hours,
  re-rolls hours that late spans touched, keeps a per-project watermark.
- `GET /api/v1/stats` answers from the rollup for hours behind the
  watermark and from the live scan for the tail — same shape, same
  filters, same groupings; percentiles become histogram-based everywhere.
- Aggregates **survive retention**: the sweep deletes traces, not history.
  A new per-project `stats_retention_days` (NULL = keep forever, the
  default) for deployments that want even the aggregates mortal.
- The Settings screen and admin API/CLI grow the new field; `system`
  counts the new table.

Not here: new chart types, new groupings, per-user or per-session rollup,
time-sharding (design §5.6 item 2), changing the sweep itself.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-08-30** — One table, `stats_hourly`, carries both units: rows with `model = ''` aggregate **traces** (the hour/day/environment/release groupings), rows with a model aggregate **observations** (the model grouping). The dimension tuple is `(project_id, hour, environment, release, model)` | The five existing groupings are all projections of one tuple, and one table means one aggregator, one watermark and one deletion path instead of two of each. The two units cannot mix — spec 004 #23 made `unit` part of the answer precisely because a trace count and an observation count must not look alike — and the `model` column is the discriminator the scan already uses (`o.model IS NOT NULL`). Cardinality is bounded by what people actually run: environments × releases × (models + 1) per hour, dozens of rows an hour in practice; a deployment that emits unbounded release strings pays for exactly that choice, linearly. |
| 2 | **2026-08-30** — Latency lives in each row as a **log-bucketed histogram**: bucket `i` counts latencies in `[1.25^i, 1.25^(i+1))` milliseconds, i = 0…62 plus an underflow bucket (< 1 ms), stored as a JSON array of counts. Percentiles are computed from the histogram — **everywhere**, the live tail included (owner decision 2026-08-30: one path) | A percentile cannot be summed; a histogram can, which is what lets day buckets be 24 hour-rows merged and lets one row absorb re-rolls idempotently. Log buckets bound the relative error by the bucket ratio — ±12% worst case, ±6% expected — which is inside the natural noise of a latency chart; 64 buckets span 1 ms to ~3.7 hours, past both the ingest guard's range and any plausible LLM call. One path rather than exact-when-raw-exists because two paths would answer the same question with two numbers, and the number *changing* when the raw data expires is exactly the kind of surprise a chart must not spring. The old exact-sort path — and the unbounded `latencies []int64` buffer with it — is deleted, not kept as a fallback. `docs/api.md` states it in one line: *latency percentiles are histogram-based, accurate to a few percent*. |
| 3 | **2026-08-30** — The aggregator is a goroutine on its own tick (`TRACEPAD_ROLLUP_INTERVAL`, default 5 m), beside the sweeper, writing through the group-commit writer in per-`(project, hour)` `WriteJob`s (owner decision 2026-08-30: background, not in the ingest transaction). An hour is **closed** when it ended more than one tick ago; each pass rolls closed hours up to now and advances the project's watermark | In the ingest transaction the rollup would be delta-maintained — and a delta is exactly what spec 002 #22 refused for trace aggregates, because re-delivery is routine and a delta double-counts every retried span. Recomputing a whole `(project, hour)` from the raw rows is idempotent by construction: the job is a `DELETE` + `INSERT … SELECT`, bounded by one hour of one project, serialized through the one write path (spec 003 #9, spec 005 #3). The writer's cost per batch — measured every PR — does not move at all. |
| 4 | **2026-08-30** — Late arrivals: each pass re-rolls every already-rolled hour that gained or lost rows since the last pass, found by `ingested_at > last pass` on traces (and the deletion paths mark hours dirty explicitly). The watermark never moves backwards | A span can arrive for an hour that was rolled minutes or days ago (spec 002 #6 tolerates any order), and a user-data erasure (spec 005 #7) can subtract from one. "Re-roll what changed" costs one indexed query to find the dirty hours and one bounded job per hour, and it converges within one interval — which the docs state: *the rollup trails the raw data by up to the interval*. The alternative — trusting hours to close — is the lie spec 005 #1 already refused to tell about client clocks. |
| 5 | **2026-08-30** — `GET /api/v1/stats` splits the asked range at the project's watermark: hours behind it come from `stats_hourly`, hours at or past it from the live scan that exists today; the two halves merge before bucketing. The response shape, filters, groupings and `unit` do not change | The seam is invisible because both halves produce the same thing — dimension tuples with counts and histograms — and Decision 2 already made percentiles histogram-based on both sides. Before the first pass ever runs, the watermark is zero and every query is the live scan: the migration needs no synchronous backfill, the aggregator's first passes *are* the backfill, and the screen never regresses while it catches up. A range entirely behind the watermark — the month view that motivates this spec — touches raw tables not at all, which is also what makes the answer survive their deletion. |
| 6 | **2026-08-30** — Aggregates **outlive retention** (owner decision 2026-08-30): the trace sweep does not touch `stats_hourly`. A new nullable project field `stats_retention_days` (NULL = keep forever, the default, matching spec 005 #2's posture) is swept by hour age when set. Project purge deletes the project's rollup rows with everything else | The whole point: history is the cheap thing — thousands of rows a month — and deleting it alongside the expensive thing it summarizes would re-break the charts that this spec exists to keep. Forever-by-default mirrors the retention default's reasoning: a self-hosted tool must not silently discard what costs nothing to keep. The separate knob exists because "no trace older than 30 days" and "no *record* older than 30 days" are different promises, and an operator who means the second must be able to keep it. |
| 7 | **2026-08-30** — User-data erasure (spec 005 #7) **corrects** the rollup: the hours the erased traces occupied are re-rolled by the same dirty-hour mechanism, in the same request, before it answers | The erasure endpoint's contract is "the user's parsed data is gone when the 200 arrives", and a per-hour count is parsed data derived from it. Aggregates carry no user id, but "how many traces this user contributed that hour" is recoverable by diffing the rollup before and after — so the honest reading of the promise re-rolls. The cost is bounded: the erased traces' distinct hours, each one job. |
| 8 | **2026-08-30** — Observability of the machinery itself: `GET /api/v1/system` counts `stats_hourly` (spec 004 #10, same reasoning as spec 011 #13), and the aggregator logs one line per pass with hours rolled and duration. No new endpoint | An operator watching disk and freshness has these two questions; a row count and a log line answer both without growing the API. |
| 9 | **2026-08-30** — Three clients, no new surface: the CLI `stats` command and MCP `get_stats` are untouched (same endpoint, same shape); Settings' Administration section gains *Stats retention* beside the existing retention field, and the admin API/CLI grow `stats_retention_days` wherever `retention_days` already travels | The rollup is an implementation of the same question, not a new question. The one user-visible knob rides the paths spec 005 already built for its sibling. |

## Data contract (schema 0009)

```sql
CREATE TABLE stats_hourly (
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    hour        INTEGER NOT NULL,          -- Unix seconds, top of the hour, UTC
    environment TEXT NOT NULL,
    release     TEXT NOT NULL DEFAULT '',  -- '' = the trace named none
    model       TEXT NOT NULL DEFAULT '',  -- '' = trace-unit row (Decision 1)
    count       INTEGER NOT NULL,
    error_count INTEGER NOT NULL,
    total_cost  REAL,                      -- NULL = nothing in this cell carried a cost
    latency     TEXT NOT NULL,             -- JSON: [underflow, b0, b1, …] (Decision 2)
    PRIMARY KEY (project_id, hour, environment, release, model)
) STRICT;

-- The aggregator's bookkeeping: how far each project is rolled, and when
-- the last pass ran (the dirty-hour cutoff of Decision 4).
CREATE TABLE stats_rollup (
    project_id   TEXT NOT NULL PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    rolled_until INTEGER NOT NULL,         -- Unix seconds; hours before this are rolled
    last_pass    INTEGER NOT NULL          -- Unix ns, server clock, for ingested_at
) STRICT;

ALTER TABLE projects ADD COLUMN stats_retention_days INTEGER;  -- NULL = forever
```

The primary key is the query: a range read is one indexed scan. No separate
day table — a day is 24 hour-rows merged at read time (Decision 2 makes the
merge exact for counts and correct for histograms).

The aggregation of one `(project, hour)` is, in shape:

```sql
DELETE FROM stats_hourly WHERE project_id = ? AND hour = ?;
INSERT INTO stats_hourly
SELECT …traces grouped by environment, release…        -- model = ''
UNION ALL
SELECT …observations joined to traces, grouped by
       environment, release, model WHERE model != ''…  -- observation rows
```

with histograms built in Go (SQLite has no aggregate for them) inside the
same `WriteJob`.

## API contract

Unchanged in shape. Behavioural notes, stated in `docs/api.md`:

- `latency_ms.p50`/`p95` are histogram-based (Decision 2): accurate to a few
  percent, stable across raw-data expiry.
- The statistics trail the raw data by up to the rollup interval for closed
  hours (Decision 4); the current hour is always live.
- Ranges older than the project's `retention_days` keep answering — from the
  rollup — as long as `stats_retention_days` allows (Decision 6).

Admin surface: `stats_retention_days` appears wherever `retention_days`
does — project create/update payloads, `GET` responses, `openapi.json`, the
admin CLI flags — with the same NULL-means-forever semantics.

## Aggregator contract

- One goroutine, `TRACEPAD_ROLLUP_INTERVAL` (default `5m`, same parser and
  bounds style as `TRACEPAD_SWEEP_INTERVAL`).
- Each pass, per project: (1) find dirty rolled hours (traces with
  `ingested_at > last_pass`, plus hours the deletion paths marked); (2)
  re-roll each; (3) roll forward every closed hour up to `now − interval`;
  (4) advance `rolled_until` and `last_pass`. Every step is a bounded
  `WriteJob`; a crash mid-pass redoes at most one pass's work.
- The first pass on an existing database is the backfill (Decision 5): it
  walks from the oldest trace hour forward; progress is logged; queries
  keep answering live meanwhile.
- When `stats_retention_days` is set, the pass also deletes rollup rows
  whose hour is older — the same chunked-delete manner as the sweeper.

## Application contract

Settings → Administration: a *Stats retention* field beside *Retention*,
same input affordances, same "days or forever" semantics, wired to
`stats_retention_days`. Nothing else in the UI changes; the Stats screen's
numbers now come faster and reach further back. Budget measured at the end
(spec 010 #7).

## Testing

- **Go, histogram**: bucket boundaries and the percentile read-back —
  a known distribution lands within the error bound; merge of two
  histograms equals the histogram of the concatenation; empty histogram
  answers `null` percentiles.
- **Go, aggregator**: one hour rolls to the same buckets the live scan
  answers for that hour (dimension tuples, counts, costs, error counts;
  percentiles within one bucket ratio); re-delivery then re-roll leaves one
  set of rows (idempotence); a late span dirties and corrects its hour; the
  watermark advances and never regresses; the first pass backfills a
  pre-0009 database fully.
- **Go, read seam**: a range straddling the watermark equals live-scan
  truth on the tuples and counts; a range fully behind it touches no raw
  table (asserted by dropping the raw rows and asking again); day buckets
  equal the merge of their hours.
- **Go, retention interplay**: sweeping traces leaves `stats_hourly`
  intact; `stats_retention_days` sweeps rollup rows and nothing else;
  user-data erasure re-rolls the touched hours before answering (the count
  drops); project purge leaves no rollup rows.
- **Go, cost**: ingest benchmark unchanged with the aggregator idle (it
  must be — Decision 3); one pass over the benchmark fixture timed, the
  number in the PR; `/stats` latency over a synthetic month, live scan vs
  rollup, both numbers in the PR.
- **CLI / MCP / openapi**: parity tests over the new admin field; stats
  command output unchanged on the fixture.
- **UI**: Settings round-trip of the new field; existing Stats E2E stays
  green (same shape).
- **Chrome (DoD)**: Stats screen over a seeded corpus before and after the
  aggregator's pass — same charts; Settings edits the new field; both
  themes, 375 px, console clean.

## Edge cases

- An hour with traces but no costed one: `total_cost` NULL, `count` real —
  same absent-cost honesty as spec 002 #14.
- A trace whose timestamp is in the future (within ingest's accepted
  window): its hour is past the watermark, so it lives in the live tail
  until the clock catches up — never rolled early, never lost.
- `group_by=environment` with an `environment=` filter: filter first, group
  after — unchanged.
- A project created mid-pass: watermark row appears on its first roll; until
  then it is all live tail.
- Retention shorter than the rollup interval cannot orphan an unrolled
  hour: `retention_days` is at least a day, the interval is minutes.

## Out of scope

- Trend charts over the long history, score/quality rollups — iteration 2
  (design §12) reads this table when it comes.
- Per-user or per-session aggregation; distinct counts (they do not merge).
- Time-sharded database files (design §5.6 item 2).
- Exposing raw histograms in the API (a later spec may, for client-side
  heatmaps; the row format is ready for it).
