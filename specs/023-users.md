# Spec 023 — Users: a per-user rollup, the listing, the user page

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> A user id rides on two thirds of production traces and the interface can
> filter by it, erase by it, and nothing else. "Who are my heaviest users",
> "when did this one last show up", "what does this account cost me" are
> questions the store can answer only with a scan the design forbids at
> scale. This spec adds the per-user rollup beside spec 013's, a listing
> of users, a user page with the activity chart and the breakdowns the
> Stats screen already knows how to draw, and the erase gesture where it
> belongs.

---

## Overview

Deliverables, one PR (the last commit flips the status):

- Schema 0013: `users_hourly` — spec 013's tuple plus `user_id` — and a
  `users` summary table; the aggregator rolls both in the same pass
  (Decisions 1–4).
- `GET /api/v1/users`, `GET /api/v1/users/{id}`; a `user_id` filter on
  `GET /api/v1/stats` (Decisions 5–6). CLI `users ls`, `users show`,
  `stats --user`; MCP `list_users`, `get_user`, `get_stats` with `user_id`
  (Decision 7).
- The *Users* sidebar item, `/users` and `/users/{id}` (Decisions 8–10);
  user ids in the trace and session tables become links.
- The application-line ceiling rises to 16,000 (Decision 11).
- `docs/users.md` (new), `docs/api.md`, `docs/cli.md`, `docs/mcp.md`,
  `docs/retention.md`, `docs/admin.md`, `docs/ui.md`, `openapi.json`,
  `schema.d.ts`.

Not here: user metadata or names (the store knows an id), distinct-user
counts on the Stats screen, per-session rollup, a "no user" pseudo-row.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-07** — A second rollup table, **`users_hourly`**, with spec 013's dimension tuple **plus `user_id`**: `(project_id, hour, user_id, environment, release, model)` → `count`, `error_count`, `total_cost`, `latency` histogram, and on trace-unit rows (`model = ''`, spec 013 #1) **`sessions_started`** — the sessions of that user whose earliest trace falls in the hour. Traces with no user id are not in it (owner decision 2026-09-07: user × hour, with breakdowns) | The same tuple means the same aggregation SQL with one more `GROUP BY` column, the same histogram, the same read-time merge, and — the point — the same `/stats` answer restricted to one user (Decision 6) with nothing invented. Sessions are counted where they *start* because a distinct count does not merge across hours (spec 013, out of scope) and a start does: the sum over any range is exact. Cardinality is what people run: a user is active in a few hours a day, so rows are active-user-hours × environments × (models + 1), and a deployment with a million users a day pays for exactly that — the docs say so in numbers. |
| 2 | **2026-09-07** — The rollup rides spec 013's **aggregator**: the same pass, the same `(project, hour)` `WriteJob` writes both tables, the same watermark, the same dirty-hour rule, the same frozen-hour rule (spec 013 #11, #14, #16), the same `stats_retention_days` sweep. **One addition to the dirty set**: a changed trace that carries a session id also dirties the hours in which the other traces of that session start. *Known limit:* a session whose start moved because a trace of it was re-delivered with an earlier start keeps its `sessions_started` in the old hour until that hour is dirtied by something else — the same shape as spec 013 #16's limit | Two aggregators would be two watermarks, two seams and two places for the corrections spec 013 spent five reviews on to be wrong again; one job per hour that writes two tables costs one more `INSERT … SELECT`. The extra dirtying exists because `sessions_started` in hour H depends on traces in *other* hours (a late trace that starts before the session's known first moves the start from H to an earlier hour, and H would keep counting it); it is bounded by the session's trace count, the way #16's is by the observation count. |
| 3 | **2026-09-07** — A **`users` summary table** — per `(project_id, user_id)`: `traces`, `error_count`, `total_cost`, `sessions`, `first_seen`, `last_seen` (hours) — recomputed by the pass for every user whose hours it rolled, as `SUM`/`MIN`/`MAX` over that user's `users_hourly` rows. Indexes over `(project_id, <sort key> DESC, user_id)` for each sort Decision 5 offers. A user whose last row the retention sweep removed is deleted from it | The listing sorts and pages over aggregates, and a keyset cursor needs a row per user with the sort key on it — a `GROUP BY` over users × hours on every page view is the scan spec 013 exists to avoid. Recompute-not-delta, for spec 013 #3's reason; bounded to the users the pass touched. |
| 4 | **2026-09-07** — The listing (Decision 5) answers from `users` **only**: it trails the raw data by up to twice the rollup interval (spec 013 API contract), so a user first seen minutes ago is not yet listed. The user page (Decision 6) merges the **live tail** — traces past the watermark, filtered by `user_id` — so it is exact for any id, listed or not. `docs/users.md` states both | A live merge into a sorted, cursor-paged listing would mean sorting two sources on every page, which is the scan again through a side door; the page is one user and the tail is minutes of one user's traces, cheap and honest. The lag is the one the Stats screen already lives with, and the same sentence in the docs covers it. |
| 5 | **2026-09-07** — **`GET /api/v1/users`**: rows `{user_id, traces, error_count, total_cost, sessions, first_seen, last_seen}`; `sort=last_seen\|traces\|cost\|errors` (default `last_seen`, always descending, ties by `user_id`); `prefix=` (case-sensitive prefix of the id); `limit`, `cursor`, `direction`, `count` as every listing (spec 009, spec 010); an unknown parameter or an empty value is a `400` (spec 003's rule). **`GET /api/v1/users/{id}`**: `{user_id, traces, error_count, total_cost, sessions, first_seen, last_seen, latency_ms: {p50, p95}}` over the rollup **plus the live tail** (Decision 4), `last_seen` exact when the tail holds the user; `404` when neither does | Four sorts are the four questions the listing is for (recent, heavy, costly, failing); ascending order of any of them is nobody's question. A prefix rather than a substring because the index answers a prefix and a substring is a scan; the listing is not search (spec 011 is). The summary shape repeats the row so a client reads one type. |
| 6 | **2026-09-07** — **`GET /api/v1/stats` gains a `user_id` filter**: with it, the rollup half of the seam reads `users_hourly` and the live half filters traces by the id; shape, groupings and `unit` unchanged. `group_by=hour\|day` with `user_id` additionally carries `sessions` per bucket (from `sessions_started`; absent without the filter). No new series endpoint | The user page's chart and breakdowns *are* the Stats screen's for one user, and spec 013 #5's seam already knows how to split a range; a filter is the smallest change that gives every client the per-user series at once. `sessions` rides only with the filter because it has no meaning in `stats_hourly`. |
| 7 | **2026-09-07** — **CLI** `users ls [--sort S] [--prefix P] [--limit N] [--cursor C] [--oldest] [--newer] [--total]`, `users show <id>`, `stats --user U`; **MCP** `list_users`, `get_user`, `get_stats` with `user_id` — each one endpoint (spec 004 #16, #17) | Three clients, one surface (spec 004 #1). |
| 8 | **2026-09-07** — *Users* is a **top-level sidebar item** between *Sessions* and *Stats*, icon `users`. `/users` is the shared listing (spec 010): id, traces, sessions, errors, cost, first seen, last seen; a sort select and a prefix box in the filter bar (`?sort=`, `?prefix=`); row → `/users/{id}` | It sits between the two screens it joins: a user is a set of sessions, and the page is Stats for one of them. |
| 9 | **2026-09-07** — **`/users/{id}`**: a header with the id and the summary cards (traces, sessions, errors, cost, first/last seen, p50/p95); a `RangePicker` (`?from=&to=`, default the last 30 days) governing an **activity chart** — traces and errors per bucket, sessions as a second series — and a cost chart, both `Chart` over `GET /stats?user_id=` with `buildSeries`; two **breakdown tables**, environments and models, `BreakdownTable` over `group_by=environment` and `group_by=model` with the same filter; then two tabs (`?tab=sessions\|traces`) — the sessions and traces tables as they are, filtered by `user_id`, each with a ⤢ to the full listing with the filter set. *Erase data* in the header (Decision 10) | Every piece is a component the Stats and listing screens already have; the page is composition, which is what keeps it inside a budget (Decision 11). The tabs reuse the tables rather than embedding the pages so the peek panel and the walk come along (spec 016 #3). |
| 10 | **2026-09-07** — *Erase data* on the user page goes through **`ConfirmCard`** with the server's dry run (`DELETE …/users/{id}/data` without `confirm`: traces, observations, scores, affected runs) and the id echoed, exactly as Settings does (spec 005 #8, spec 007 #5); on success the page navigates to `/users`. Settings keeps its form. Erasure **deletes the user's `users_hourly` and `users` rows** outright, in the same request, beside spec 013 #7's re-roll of `stats_hourly` | The gesture is the same act with the same blast radius; one card renders one contract. The per-user rows are deleted rather than re-rolled because they are *about* the user — a re-roll would recompute them to nothing from raw rows that are gone, and for a frozen hour (spec 013 #11) could not recompute them at all; deleting is the only reading of "everything stored about one user" that holds for frozen hours too. |
| 11 | **2026-09-07** — The application-line ceiling is **16,000** (owner decision 2026-09-07), from spec 016 #14's 14,000, with this spec landing under it and the PR reporting the number per screen. The gate stays a warning. **Superseded 2026-09-08 by spec 024 #17: the ceiling is 18,000** | The ceiling is a signal for revision, not a prohibition (design §8.2); two screens landed at 13,895 with nothing to cut, and two more — the annotation queue and quality trends — are on the design's own list. Raising it once for the known set is the honest move; raising it per spec would be the drift the number exists to catch. |
| 12 | **2026-09-07** (from the implementation) — `sessions_started` counts a session in the hour of **the earliest trace that user filed under it**, ties broken by trace id. Decision 1's "the earliest trace" made precise | A session id is not scoped to a user, and nothing stops two users sharing one. Counting by the session's *global* earliest trace would put another user's session start in this user's row — or, worse, in an hour where this user has no row at all, so the sum over a range would stop being this user's session count. Per `(user, session)` the sum is exact by construction, which is the whole property Decision 1 buys, and it is the same number in the ordinary case where a session belongs to one user. The tie-break on the id exists because two traces may share an instant, and without it both would count as starts. |
| 13 | **2026-09-07** (from the implementation) — `SessionTable` and the session header do **not** render a user id: neither has one. `GET /api/v1/sessions` returns `{id, trace_count, error_count, total_cost, first_seen, last_seen}` and `GET /api/v1/sessions/{id}` the same — a session is a grouping of traces (spec 007 #2), and the traces of one may name different users. The Application contract's sentence is honoured where a user id exists — `TraceTable`'s cell and the trace page's header — and its other half is struck there with a dated note (owner-accepted 2026-09-07) | Putting a user id on a session row means deciding what a session's user *is* when its traces disagree, and then growing the sessions endpoint to carry the answer — a change to a listing this spec's Overview does not touch, for a link that is one row away on the user page's own sessions tab. Stating the gap beats shipping a `SessionRow.user_id` nobody specified. |
| 14 | **2026-09-07** (from the implementation) — A trace row now holds **two** links: its own (the timestamp cell, spec 008 #16) and the user id. Spec 008 #16's "exactly one thing in the row is tabbable" is narrowed to "exactly one thing in the row is *the row's own* link", which stays first in DOM order | The alternative was a link reachable only with a mouse, which is not a destination. Two tab stops per row is the ordinary cost of a second destination, and keeping the row's own link first is what makes "the row's link" go on meaning what every keyboard reader — and every e2e locator — takes it to mean. The suites that said `getByRole('link')` of a row now say `.first()`, with the reason written where they say it. |
| 15 | **2026-09-07** (from the live check) — Migration 0013 ends with `UPDATE stats_rollup SET last_pass = 0`, so the first pass after an upgrade finds every already-rolled hour **dirty** and re-rolls it. `rolled_until` is deliberately not touched. *Known limit:* an hour past the project's trace-retention window is frozen (spec 013 #11) and gets no per-user rows, ever | Spec 013 #5 could say "the aggregator's first passes are the backfill" because a fresh watermark makes every hour a forward roll. Here the watermark is already past the whole history: every hour is rolled, none is dirty, and both new tables would stay empty until new traffic arrived — for ever, on a project that has stopped receiving any. Found on the demo stand, where the Users listing answered 0 users over thirty days of traffic through nineteen passes. Moving `rolled_until` back instead would make the read seam answer `/stats` from the live scan meanwhile, and for hours whose traces retention has taken the live scan finds nothing — a month's chart empty for the length of a pass, which is exactly the wrong answer spec 013 #12 exists to prevent. The dirty set reaches the same hours without touching the claim. Measured on the stand: 698 hours in 40.2 s, once, against `main`'s own 38.6 s backfill of the same corpus. |
| 16 | **2026-09-08** (from manual testing on the stand) — The **session id** in `TraceTable` and in the `/traces/{id}` header is a link to `/sessions/{id}`, built the way Decision 14 built the user link: `stopPropagation` so the row's own click still opens the panel, the row's own link still first in DOM order, a third tab stop after the user id. The trace header, which never showed the session at all, gains it next to the user id, with the same responsive class. Both themes, 375 px, an e2e for each site. *(2026-09-09, from review: a linked id loses the double click and the drag that select it, as the user id did in Decision 14. Accepted on the same terms — the id is still copied by a triple click and from the session's own page, and a copy button in every row is noise the row cannot afford.)* | Spec 007 built the sessions screen before any spec said what a session id in *another* table should do, and this spec's contract named only user ids — so the user cell became a destination and the session cell beside it stayed text. Found by the owner clicking it. A session is the reading unit spec 007 #2 chose, and the trace table is where a reader meets one; a destination reachable only through the Sessions listing and a prefix box is not one. Decision 14 already paid for the second tab stop and the `.first()` locators; a third costs nothing new. |
| 17 | **2026-09-09** (from review of PR #47) — Three amendments to #16. (a) `TraceTable` takes **`linkSession`**, and `SessionDetail` passes `false`: on a session's page, in its peek panel, in the panel the Sessions listing drills into and in the user page's sessions tab, the cell stays the text it was. (b) **`TracePeekMeta` gains the session link** — no `stopPropagation`, there is no row click over a panel's meta — with `session` in its `hide` list, which the two sites already inside a session pass. (c) The `/traces/{id}` header is **not** folded into `TracePeekMeta`: it renders the user id link, which no panel does, so the two are still different renderings and spec 026 #4 leaves a different rendering to its site (spec 026, divergence 10) — and the header carries the same shrink rule by hand, which is the price of not folding. *(2026-09-09, from the second review: the sixth part squeezes a `flex` row of shrinkable items, so the short parts are `shrink-0` in both files and the truncating ones give way. Measured at 1024 px, where the panel is at its 544 px floor: the trace id was **21 px** before this spec's session part and is **14–16 px** after — an ellipsis either way, with the copy button beside it. Hiding the release there was measured too and returns **3 px**, so it is not done.)* | (a) A link to where the reader stands is not a destination, and from a peek panel it is worse than inert: following it tears down the listing and the panel to arrive at the session that panel is already inside. (b) The panel is the path spec 008 #3 calls the normal one — the row opens it, and `/traces/{id}` is the shareable form — so a destination the table and the full page both offer cannot stop at the panel's door. (c) Folding it in means a `user` part with exactly one consumer, which is the copy-of-one spec 026 #4 declined to make; the header is one site's rendering, and it says so here rather than in a comment |
| 18 | **2026-09-09** (from the tails of PR #47/#48) — The `/traces/{id}` header's own shrink rule is pinned by an **e2e case at 900 px** on a trace this suite sows with a long user id and a long session id: the timestamp is no taller than one line, the bar is one row, and both ids are clipped. `header.spec.ts`, in a project of its own | #17c chose two renderings over one shared part, and priced it as "the header carries the same rule by hand". That price was only half paid: `TracePeekMeta`'s classes are pinned by a unit test, and the header's were pinned by nothing — so when the second review found the row squeezing, the panel was corrected by the test that was already watching it and the header was corrected by hand, which is exactly the asymmetry that let the defect exist. A unit test over the header's classes would be the wrong shape here: this is a whole page rather than a component with a class list, and what matters is the row it produces, not the spelling. So the pin is the measurement — and a measurement also catches the next way the row can break, which a class list cannot. |
| 19 | **2026-09-14** (closing the gap spec 010 #10 named; found in review of PR #61) — **Each erase chunk re-rolls the hours it emptied inside its own transaction.** `UserDataErase.apply` ends by applying the whole `statsRoll` for every hour the chunk's traces occupied, under a `Now` the handler passes once per request; the handler no longer submits `RollHour` after the last chunk. Decision 10 amended: the per-user rows still go outright on every chunk, and now the project-wide correction of spec 013 #7 rides beside them, chunk by chunk. Spec 013 #7's "in the same request, before it answers" is kept and made stronger: in the same *commit* as the deletion it corrects | The handler rolled the touched hours only after the last chunk, each roll its own `Submit` under the request's context. A client that hung up between chunks — a closed tab, and since spec 010 #10 the interface's thirty-second clock — cancelled that context, `Submit` returned `ctx.Err()`, and the handler left: the chunks already committed had their traces gone and their hours still counting them, and a repeat of the request could not find those hours, because only the traces that remained named any. The alternative was to finish under a context the client cannot cancel (`context.WithoutCancel` with a server-side ceiling) — cheaper on paper, but a request that keeps working after its client was told it failed is a second thing to document and a second thing to get wrong, and it does not close the window between a chunk's commit and its roll, which is the window the writer's own contract opens: it commits a job whose caller has already gone. In the transaction there is no window: the chunk commits corrected or not at all, which is what spec 025 #22 concluded for the score rollup and for the same reason. Measured on a copy of the demo stand with a synthetic user of 20,985 traces over 699 hours, the rollup counting them: `main` took **53.2 s** for the whole erasure, and a client that hung up at three seconds left **110 hours** counting erased traces — still 113 after a repeat ran to completion. This branch took **11.1 s** for the same erasure, and the same hang-up left **0** dirty hours, 0 after the repeat. Faster, not slower: the chunks follow `idx_traces_user`, which is arrival order, so an hour straddles a chunk boundary rarely — 738 rolls for the 699 hours — and each lone `RollHour` submission had been paying the writer's 50 ms commit window on its own, 699 of them, which was most of the request. |

## Data contract (schema 0013)

```sql
CREATE TABLE users_hourly (
    project_id       TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    hour             INTEGER NOT NULL,          -- Unix seconds, top of the hour, UTC
    user_id          TEXT NOT NULL,
    environment      TEXT NOT NULL,
    release          TEXT NOT NULL DEFAULT '',
    model            TEXT NOT NULL DEFAULT '',  -- '' = trace-unit row (spec 013 #1)
    count            INTEGER NOT NULL,
    error_count      INTEGER NOT NULL,
    total_cost       REAL,
    latency          TEXT NOT NULL,             -- histogram (spec 013 #2)
    sessions_started INTEGER NOT NULL DEFAULT 0, -- trace-unit rows only (Decision 1)
    PRIMARY KEY (project_id, hour, user_id, environment, release, model)
) STRICT;
CREATE INDEX idx_users_hourly_user ON users_hourly(project_id, user_id, hour);

CREATE TABLE users (
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id     TEXT NOT NULL,
    traces      INTEGER NOT NULL,
    error_count INTEGER NOT NULL,
    total_cost  REAL,
    sessions    INTEGER NOT NULL,
    first_seen  INTEGER NOT NULL,               -- hour
    last_seen   INTEGER NOT NULL,               -- hour
    PRIMARY KEY (project_id, user_id)
) STRICT;
CREATE INDEX idx_users_last_seen ON users(project_id, last_seen DESC, user_id);
CREATE INDEX idx_users_traces    ON users(project_id, traces DESC, user_id);
CREATE INDEX idx_users_cost      ON users(project_id, total_cost DESC, user_id);
CREATE INDEX idx_users_errors    ON users(project_id, error_count DESC, user_id);
```

The `(project, hour)` job of spec 013 gains, in the same transaction: a
`DELETE`/`INSERT … SELECT` over `users_hourly` for the hour (traces with
a user id grouped by user, environment, release; observations joined
likewise with model), `sessions_started` from a subquery over the hour's
sessions' earliest traces; then the `users` recompute for the user ids the
hour holds or held. `system` counts both tables.

## API contract

- `GET /api/v1/users` and `GET /api/v1/users/{id}` — Decision 5;
  `openapi.json` + `schema.d.ts` in one commit; router ↔ openapi parity.
- `GET /api/v1/stats?user_id=` — Decision 6; `docs/api.md` *Statistics*
  gains the filter and the per-user lag sentence.
- `DELETE /api/v1/projects/{id}/users/{user_id}/data` — unchanged shape;
  Decision 10's extra deletion, stated in `docs/admin.md` and
  `docs/retention.md`.
- `docs/users.md`: what is counted, the lag, the cardinality in numbers,
  the erase interplay, the CLI and MCP lines.

## Application contract

| Route | Screen |
|---|---|
| `/users` | Decision 8. Empty state: *no user has been seen yet* and the two attribute names (`user.id`, `langfuse.user.id`) with the SDK line `update_trace(user_id=…)`. |
| `/users/{id}` | Decision 9. Unknown id: the page's not-found state with the id and a link to `/traces?user_id=`. |

`TraceTable` and the trace header render the user id as a link to
`/users/{id}`. *(Superseded 2026-09-07 by Decision 13, owner-accepted the
same day: this line first named `SessionTable` and the session header too,
and neither carries a user id — a session is a grouping of traces, and the
traces of one may name different users. The link goes where the id exists.)*
The session id in `TraceTable`'s cell and the trace page's header — and in
`TracePeekMeta`, Decision 17 — is a link to `/sessions/{id}`
*(added 2026-09-08 by Decision 16, extended 2026-09-09 by Decision 17)*.
`Sidebar` gains the item. The API client gains `listUsers`, `getUser`, and
`user_id` in the stats filters; `USER_FILTERS` gets the openapi parity test
(spec 016 #13). Both themes; console clean; 375 px never scrolls the page;
the charts stack on the narrow layout as Stats' do.

## Testing

- **Go, rollup**: one hour rolls `users_hourly` equal to a live
  per-user scan (tuples, counts, costs, errors; percentiles within one
  bucket ratio); `sessions_started` counts a two-hour session once, in its
  first hour; a late earlier trace of a session moves the start and the
  re-roll corrects both hours (Decision 2's dirtying); anonymous traces
  produce no rows; re-delivery is idempotent; frozen hours untouched.
- **Go, summary**: `users` equals the sums over `users_hourly` after a
  pass; a pass touching one hour recomputes only that hour's users; the
  retention sweep of the last row deletes the user.
- **Go, API**: listing sorts (all four, ties by id), `prefix`, cursors
  both ways at `limit=1`, `count`; `400` on unknown or empty params;
  `/users/{id}` merges the live tail (a trace ingested after the last pass
  changes `traces` and `last_seen`); `404` for an unseen id; `/stats` with
  `user_id` equals `/stats` over a corpus of that user alone, both halves
  of the seam; `sessions` present only with the filter; erasure removes the
  rows and `/users/{id}` answers `404` after.
- **Go, cost**: ingest benchmark unchanged (the aggregator is off the write
  path, spec 013 #3); one pass timed with and without users on the
  benchmark fixture, both numbers in the PR; `EXPLAIN QUERY PLAN` shows
  the sort indexes.
- **CLI / MCP / openapi**: parity; `users ls`/`show` and `get_user`
  byte-equal to the endpoint.
- **Vitest**: `USER_FILTERS` parity; the sort select and prefix box write
  the URL; the page's tab and range state.
- **e2e** (`users.spec.ts`, own project, spec 016 #18): deliver fixtures
  with two users, drive a pass (the e2e harness's way of making the
  aggregator run, or `TRACEPAD_ROLLUP_INTERVAL` at its floor) → the listing
  shows both, sorted by last seen; sort by cost reorders; prefix narrows;
  the user page shows the cards, the chart with points, both breakdowns,
  the sessions tab and the traces tab; a user id in the traces table
  links to the page; *Erase data* shows the dry run, refuses a wrong echo,
  erases and lands on `/users` without the id; 375 px.
- **e2e** (`header.spec.ts`, #18): a trace of its own with a long user id
  and a long session id, at 900 px — the timestamp is one line high, the
  bar is one row, and both ids are clipped, so the case ran out of room.
- **Mutations** (table in the PR): `sessions_started` counting every hour
  of a session; the summary recompute skipping untouched users' deletion;
  the listing's tie-break dropped; the live tail not merged on the page.
- **Lines and bytes**: `make ui-lines` under 16,000 with the per-screen
  breakdown; `dist` under spec 015 #10's ceiling.
- **Chrome (DoD)**: both routes on the demo corpus after a pass, both
  themes, 375 px, console clean, screenshots in the PR.

## Edge cases

- **A user in the live tail only** (first seen after the last pass): not
  in `/users`, exact on `/users/{id}`; the listing's empty state does not
  claim there are no users when `traces` has some — it links to
  `/traces?user_id=`'s filter and names the lag.
- **A user id longer than the table cell**: middle-truncated with the
  whole id as the title and a copy button.
- **Erasure of a user with rows in frozen hours**: the per-user rows go
  (Decision 10); `stats_hourly` for those hours keeps counting the traces
  (spec 013 #11's documented position).
- **`stats_retention_days` shorter than a user's activity**: the summary
  covers what the rollup retains; `first_seen` moves forward as rows are
  swept — the docs say the summary is over the retained history.
- **`sort=cost` and users with no costed trace**: `total_cost` NULL sorts
  last, ties by id.
- **The same user id in two environments**: one user row; the page's
  environment breakdown shows the split.

## Config additions

None (the aggregator's interval and `stats_retention_days` govern both
tables).

## Out of scope

User names, metadata or grouping into accounts; distinct-user counts on
the Stats screen (they do not merge); a per-session rollup; scores by
user; a "no user" pseudo-row (the traces listing with no `user_id` filter
is that view); exporting a user's data (spec 019's raw export is not
per user).
