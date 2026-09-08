# Quality

A score lives on one trace. Open it and there is *hallucination 0.2*, *verdict
pass*, a thumbs-up. Ask "did hallucination drop after 2.5.0" and, until now,
the only answer was a scan of every score the project holds — the scan the
statistics rollup exists to remove.

So scores get a rollup of their own: the same hourly grain the
[statistics](api.md#where-the-numbers-come-from) use, one dimension further
along. A *Quality* screen with a card per score name, a detail view with the
trend and three breakdowns, and the same numbers in the API, the CLI and MCP.

## What is counted, and what is not

A score is counted in **the hour of the trace it names** — not in the hour it
was graded — and it takes that trace's environment, release and model. That is
what makes a quality curve line up with the traffic and cost curves on the
[Stats](ui.md#screens) screen: a judge that grades yesterday's traffic today
moves the point where the traffic was, not where the judge was.

Two kinds of score are therefore absent:

- **A score that names only a session.** There is no trace to borrow an hour, an
  environment or a release from. The API accepts such a score on purpose — a
  CSAT rating grades a conversation — and it is listed on the session; it is
  simply not on a timeline.
- **A `text` score.** A rationale has nothing to add up.

A score whose `observation_id` names a generation is filed under **that
observation's model**; every other score sits in the row whose model is empty —
the same discriminator `stats_hourly` uses. So a breakdown by model can only
show observation-level scores, and the answer says so in its `targets` field
rather than quietly counting a trace-level score under an empty model.

A **frozen hour** — one past the project's `retention_days` that this table
already holds rows for — is left as it stands, the way the statistics of such
an hour are (see [retention.md](retention.md#what-outlives-what)). An hour past
the window that this table holds *nothing* for is not frozen: there is nothing
to protect, so the roll writes whatever the raw rows still say. Usually that is
nothing, because the sweep took the traces and the scores with them — and such
a score is absent from the trend while still being listed on its trace, right
up until retention takes it too. But a year of history *imported* into an
existing install is past the window by its client timestamps and completely
intact, and it gets its rows rather than a permanent gap.

## The four numbers

Each row of the rollup carries `count`, `sum`, `min` and `max`, and the mean
and the rate are read-time divisions of them. That is what makes a day the
exact merge of its 24 hours:

| Type | What a bucket reports | Out of |
|---|---|---|
| `numeric` | `mean`, `min`, `max` | Σ`sum` / Σ`count`, and the extremes of the extremes |
| `boolean` | `rate` — the share whose value is 1 | Σ`sum` / Σ`count` |
| `categorical` | `categories`: how many scores carried each value | one row per value seen |

A **mean of means** is the wrong number here, and the rollup is shaped so that
nothing can compute one: a day is its hours' sums over its hours' counts, never
the average of 24 averages.

What these four numbers cannot give is a **distribution** — a p50 of a score,
"how many below 0.5". That needs a histogram whose buckets depend on the
config's range, and it is deliberately out of scope; the row can grow a JSON
column for it later, exactly as `stats_hourly` carries its `latency`.

One name graded two ways — a name without a config, scored numerically here and
categorically there — is **two series** with the same name, told apart by
`data_type`, and two cards on the screen.

## The lag

The trend is answered from the rollup for the hours behind the aggregator's
watermark and from the raw rows for the tail, exactly as the statistics are. So
it **trails live traffic by up to twice `TRACEPAD_ROLLUP_INTERVAL`** — five
minutes by default, so ten in the worst case — for hours that have closed. The
hour in progress is always live.

A score's own arrival is part of what the aggregator watches. A judge grading
yesterday's traffic writes only the `scores` table, and nothing about the trace
moves — so each pass also asks which scores were written since the last one and
re-rolls the hours of the traces they name. An **upsert** of a score moves its
`created_at` too, so a correction is found by the same question.

Two writes a "what changed" question cannot answer on its own, because both
leave an hour that nothing afterwards points at:

- **Deleting a score.** The row is gone, so `created_at` can say nothing about
  it. `DELETE /api/v1/scores/{id}` re-rolls that score's hour before it
  answers, the way erasing a user's data does.
- **Moving a score.** A re-POST that re-points a score at a different trace —
  or drops the target for a session-only one — dirties the hour it moved *to*
  and leaves the hour it came from counting it. The write re-rolls that hour
  too, before the `201`.

A frozen hour stays as it is in both cases.

### On an upgrade

An existing install already has its statistics rolled up to now, so nothing
about the history would look "changed" and this table would stay empty. The
migration therefore asks the aggregator to walk the rolled history once: the
first pass after the upgrade re-rolls every hour it holds, which fills the score
rows and rewrites the identical statistics and per-user ones. It is background
work, it happens once, and every screen keeps answering throughout.

## How much it costs

One row per `(hour, environment, release, model, name, data_type, category)`.
So the row count is the statistics tuple multiplied by

> score names × (the categories of a categorical name)

and nothing else. An hour nobody graded costs nothing at all.

In numbers, for one environment, one release and two models, at 24 rolled hours
a day and roughly 110 bytes a row:

| Shape | Rows an hour | Rows a day | A month |
|---|---|---|---|
| 3 numeric names, trace-level | 3 | 72 | ~240 KB |
| 6 names, one of them 4 categories | 9 | 216 | ~710 KB |
| 6 names across 5 releases and 3 models | ~110 | 2,600 | ~8.6 MB |

A categorical name with **thousands** of distinct values pays for that
linearly — the same bargain unbounded release strings make in the statistics.
If a "category" is really free text, it is a `text` score, and a text score is
not in this table at all.

Reading one name over a long window is an indexed range over this table, which
is the whole point: on a synthetic month of 36,000 graded traces the roll-up
answers in about **2 ms** where the same question asked of the raw rows takes
about **0.3 s**, and the raw rows may not be there at all.

`stats_retention_days` sweeps these rows on the same schedule as the other two
rollup tables ([retention.md](retention.md#what-outlives-what)); there is no
window of its own. `GET /api/v1/system` counts `scores_hourly`, so the size is
a number rather than a guess.

## From the API

```sh
curl … "http://localhost:4318/api/v1/stats/scores?group_by=day&from=2026-09-01T00:00:00Z"
```

See [api.md](api.md#score-trends) for the shape, the filters and the
groupings.

## From the command line

```sh
tracepad scores trend                                    # every name, by day
tracepad scores trend --name hallucination --since 30d   # one name
tracepad scores trend --name hallucination --group-by release
tracepad scores trend --group-by model --env production
```

`--json` prints the endpoint's own bytes. Full flags:
[cli.md](cli.md#scores-trend).

## From an agent

One MCP tool, one endpoint: `get_score_trends` — whether a score moved, whether
a release made the judge happier, which model scores best. See
[mcp.md](mcp.md).

## In the web interface

*Quality* sits in the *Evals* group after *Queues*, which is where it belongs:
quality is what evals produce. The overview is a card per score name; a card
opens the detail view with the trend and the model, environment and release
breakdowns. See [ui.md](ui.md#quality).
