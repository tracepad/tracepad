# Users

A user id rides on most production traces, and until now the store could
filter by it, erase by it, and nothing else. "Who are my heaviest users",
"when did this one last show up", "what does this account cost me" needed a
scan of every trace the project holds — the scan the statistics rollup exists
to avoid.

So users get a rollup of their own: the same hourly grain, one dimension over.
A listing of who is there, a page for each of them, and the same charts and
breakdowns the [Stats](ui.md#screens) screen draws, restricted to one account.

## What a user is

Any trace that carries a user id, which the mapper claims from
`langfuse.user.id` or `user.id`:

```python
tracepad.update_trace(user_id="user-4821")
```

There is nothing else to configure. A trace that names no user belongs to no
user here — there is no "anonymous" pseudo-row, because the trace listing with
no `user_id` filter already is that view.

## What is counted

Every number counts **traces**, not observations — the same rule the session
roll-up follows:

| Field | Means |
|---|---|
| `traces` | How many traces are attributed to this user |
| `error_count` | How many of *those traces* carry at least one failed observation |
| `total_cost` | Summed over the traces whose client reported a cost; **absent** when none did, which is not the same as zero |
| `sessions` | How many of this user's sessions have begun |
| `first_seen`, `last_seen` | The hours the rollup holds for them, at either end |

`sessions` counts a session where it **starts** — in the hour of the earliest
trace this user filed under that session id. A distinct count does not merge
across hours and a start does, so a sum over any range is exact: two hours of
`sessions` added together is the number of sessions begun in those two hours,
never a double count of one that spanned both.

Latency is on `GET /api/v1/users/{id}` only, as `latency_ms.p50` / `p95`. Like
every percentile in this API it is histogram-based: accurate to a few percent,
stable across the expiry of the rows it came from.

## The lag, and where there is none

The listing is answered from the rollup **and nothing else**. It therefore
trails live traffic by up to twice `TRACEPAD_ROLLUP_INTERVAL` — five minutes by
default, so ten in the worst case — exactly as the statistics do
([api.md](api.md#where-the-numbers-come-from)). A user first seen a minute ago
is not on it yet.

Sorting and paging two sources on every page view would be the scan this
design removes, through a side door. One user is another matter: `GET
/api/v1/users/{id}` merges the **live tail** — the traces past the rollup's
watermark — so it is exact for any id, listed or not, and `last_seen` is exact
to the second when the tail holds them. An id nothing was ever filed under is
a `404`.

The web interface says the same thing in the place it matters: the empty
listing does not claim there are no users, it names the lag and links to the
Traces screen, which is live.

### On an upgrade

An existing install already has its statistics rolled up to now, so nothing
about the history would look "changed" and these two tables would stay empty.
The migration therefore asks the aggregator to walk the rolled history once:
the first pass after the upgrade re-rolls every hour it holds, which fills the
per-user tables and rewrites the identical statistics rows. It is background
work, it happens once, and the statistics keep answering from the rollup
throughout.

One thing that pass cannot reach: an hour older than the project's
`retention_days` is **frozen** ([retention.md](retention.md#what-outlives-what))
— its raw rows are gone by design, so it never gets per-user rows. Such a
user's `first_seen` therefore starts where their retained traces do.

## How much it costs

One row per `(user, hour, environment, release, model)`, plus a trace-unit row
per `(user, hour, environment, release)`. So the row count is

> active user-hours × environments × (models + 1)

and nothing else — an *inactive* user costs nothing at all, and a user active
in three hours of a day costs three hours of rows however many traces they ran
in them.

In numbers, for one environment and two models, at 24 rolled hours per row and
roughly 120 bytes a row:

| Shape | Active user-hours a day | Rows a day | A month |
|---|---|---|---|
| 1,000 users, ~4 active hours each | 4,000 | 12,000 | ~43 MB |
| 100,000 users, ~2 active hours each | 200,000 | 600,000 | ~2.2 GB |
| 1,000,000 users, ~1 active hour each | 1,000,000 | 3,000,000 | ~11 GB |

The summary table beside it is one row per user the project has ever seen, at
about 60 bytes: a million users is ~60 MB, once, not per day.

Two knobs bound all of it. `stats_retention_days` sweeps these rows on the
same schedule as the statistics rollup ([retention.md](retention.md)), and a
deployment that emits a user id per *request* rather than per account is
paying for exactly that choice, linearly — the same bargain unbounded release
strings make in the statistics.

`GET /api/v1/system` counts both tables (`users_hourly`, `users`) so the size
is a number rather than a guess.

## Retention and erasure

- **Retention.** The summary is over what the rollup *retains*: when
  `stats_retention_days` sweeps a user's oldest hours, `first_seen` moves
  forward and the totals shrink with it. A user whose last row goes leaves the
  listing entirely.
- **Erasure.** `DELETE /api/v1/projects/{id}/users/{user_id}/data` deletes the
  user's rows in both tables outright, in the same request that erases their
  traces — see [admin.md](admin.md#erasing-a-users-data). They are *about*
  the user, so they go rather than being recomputed: for an hour past the trace
  retention window there is nothing left to recompute them from. The
  project-wide `stats_hourly` for such a frozen hour goes on counting the
  traces, which is the archive's documented position
  ([retention.md](retention.md#what-outlives-what)).

The same user id in two environments is one user: one row, with the split
visible in the page's environment breakdown.

## From the command line

```sh
tracepad users ls                          # by last seen
tracepad users ls --sort cost --limit 20   # the expensive ones
tracepad users ls --prefix acme:           # case-sensitive prefix
tracepad users show user-4821
tracepad stats --user user-4821 --group-by day --since 30d
```

`users ls` walks with `--cursor`, `--oldest` and `--newer` like every other
listing, and `--total` adds the capped count. `--json` prints the endpoint's
own bytes. Full flags: [cli.md](cli.md#users-ls).

## From an agent

Three MCP tools, each one endpoint:

- `list_users` — who is heaviest, most expensive, most error-prone, or here
  lately.
- `get_user` — one account's totals, exact including traffic too recent for
  the listing.
- `get_stats` with `user_id` — that account over time, or split by model or
  environment.

See [mcp.md](mcp.md).

## In the web interface

*Users* sits between *Sessions* and *Stats*, which is what it joins: a user is
a set of sessions, and their page is Stats for one of them. See
[ui.md](ui.md#screens).
