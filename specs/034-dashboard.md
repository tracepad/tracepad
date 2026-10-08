# Spec 034 — Dashboard: the project's front page

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> A project opens on its trace listing. That is the right screen for the
> person who came to read one trace and the wrong one for everybody else:
> a person who opens the project in the morning wants to know how much it
> ran, what it cost, whether it broke and whether the scores moved — and
> the screen that answers all four already exists, seventh in the sidebar,
> under a name that says *report* rather than *home*. This spec makes the
> Stats screen the project's front page: a row of figures with their
> movement against the previous window, the charts and breakdowns it has
> today, the quality cards from the Quality screen, the moment the last
> trace arrived, and a fresh project's first instructions. It is called
> Dashboard, it is what `/p/{id}` opens, and a person can hide and reorder
> its blocks — an arrangement the server keeps per account, so it follows
> the person between browsers.

---

## Overview

Deliverables, one PR (its last commit flips the status):

- **Stats becomes Dashboard**: `/p/{id}/dashboard`, first in the sidebar,
  the target of `/p/{id}`, `/` and the switcher; `/stats` redirects
  (Decision 1).
- A **summary row** above the charts: traces, cost, error rate and p95
  latency for the window, each with its change against the window of the
  same length before it (Decisions 2–3).
- `GET /api/v1/stats?group_by=total` — one bucket for the whole window,
  the number the summary row shows; the CLI and MCP take the value like
  any other grouping (Decision 3).
- **Last trace** — when the newest trace arrived, in the header, so the
  front page says whether anything is arriving at all (Decision 4).
- **Quality cards** — up to six score names of the window, as the Quality
  overview draws them, leading there (Decision 5).
- A **fresh project** sees the exporter instructions on the dashboard,
  and the Traces empty state points here (Decision 6).
- The **time window is remembered**: a preset as the preset, a calendar
  range as the dates, in this browser, for every screen that has the
  control (Decision 7).
- **Customize**: every block can be hidden and the blocks reordered by
  dragging, with a keyboard path; the arrangement lives in the account's
  **preferences** on the server, per project (Decisions 8–10).
- `accounts.preferences` (schema 0019), `preferences` on `GET
  /api/v1/auth/me`, `PATCH /api/v1/auth/me {preferences}` (Decision 9).
- The application-line ceiling rises to 21,200 (Decision 11).
- `docs/ui.md`, `docs/api.md`, `docs/cli.md`, `docs/mcp.md`,
  `docs/accounts.md`, `docs/quickstart.md`, `openapi.json`, `schema.d.ts`.

Not here: new metrics or chart types, widgets composed from a metric and
a dimension, several named dashboards, a dashboard shared by a team, a
server-wide dashboard across projects, live refresh, the window
remembered on the server.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-15** — The Stats screen **is** the dashboard: it moves to `/p/{id}/dashboard`, is renamed *Dashboard* in the sidebar and page title, and takes the first row of the sidebar. `/p/{id}` and `/` open it, and so does the switcher when it has no section to keep (spec 029 #6, #14 — the one line that named `/traces` now names `/dashboard`). `/p/{id}/stats` redirects (`307`, client-side) to `/p/{id}/dashboard` with the query kept; the bare `/stats` reaches the same place through spec 029 #3. No other screen moves | One screen, not two. A *Home* beside a *Stats* would be the same numbers drawn twice, and the person who wanted the front page would find the fuller one a click away and wonder which to trust. The screen that already draws the traffic, the cost, the latency and the errors is the front page; it only lacked the row that says the totals at a glance and the position that says it is the door. The redirect keeps every link and bookmark that names `/stats`. Traces stays what a link to *a trace* opens; it stops being what a link to *the project* opens. |
| 2 | **2026-09-15** — The **summary row**: four tiles above the charts — *Traces* (count), *Cost* (`total_cost`), *Errors* (`error_count / count`, as a percentage), *Latency* (`latency_ms.p95`) — for the window on screen, each with the change against the **previous window**: the same length, ending where this one begins (`from − (to − from)` to `from`, with `to` read as now for an open window). The change is shown as a signed percentage for traces and cost, in percentage points for the error rate, and as a signed duration for p95, with a direction glyph; cost, errors and latency colour an increase as worse and a decrease as better, traces colour neither. A tile whose number is absent (no cost reported, no traces at all) shows a dash and no change; a change whose previous value is zero or absent shows *new* rather than a division. The row replaces the totals line the page header carries today | A dashboard is the figures and their movement; the totals line already had the figures and the movement is what turns "412 traces, $1.82" into "quieter than last week, and twice the cost". The previous window of equal length is the one comparison nobody has to be told the meaning of. Traces are not coloured because more traffic is not better or worse — it is the denominator. p95 rather than p50: the median hides what a person opens a dashboard to catch. |
| 3 | **2026-09-15** — `GET /api/v1/stats` gains **`group_by=total`**: one bucket, `key: ""`, `unit: trace`, the whole window in the same shape as a day bucket — count, errors, cost, tokens, and a p50/p95 **merged over the window's histograms**. It takes every filter the other groupings take, `user_id` included (then with `sessions`, as the timed groupings carry it). `tracepad stats --group-by total` and MCP `get_stats {"group_by": "total"}` accept the value with no other change. The summary row is two requests with `group_by=total`, this window and the previous one; the comparison is arithmetic in the interface, and neither the API nor the CLI grows a *compare* of its own | The one figure the row needs and the endpoint could not give: a p95 over a week is not a function of seven daily p95s, and summing the counts in the browser was already a computation the page did that the API did not (the totals line). A grouping that returns the whole window as one bucket makes the number the server's, merged from the same histograms spec 013 merges within an hour, and is the smallest possible growth of a surface every client shares (design §3): a value in an enum. The comparison stays in the interface because a CLI user who wants last week beside this week runs the command twice, and a `compare` parameter would be a second response shape for one subtraction. |
| 4 | **2026-09-15** — **Last trace**: the header's meta line shows *Last trace 4 min ago* (relative, with the timestamp in the tooltip), from `GET /api/v1/traces?limit=1` with the environment filter and **no window**; *No traces yet* when the listing is empty. It is read with the rest of the page and by *Refresh*, and not polled (spec 007 #8 stands) | The one question a window of statistics cannot answer is whether anything is arriving now: the rollup trails, and a week of charts looks the same whether the last trace was a minute or a day ago. The listing is exact and cheap — one row, no payloads — and already reads the live table. Not `traces/last`, which returns the trace whole. Not polled: the Traces live toggle stays the one poller in the app, and a person who wants to watch arrivals is on that screen. |
| 5 | **2026-09-15** — **Quality cards**: a block *Quality* below the charts — up to **six** score names of the window ordered by how many scores each holds, each card drawn as the Quality overview draws it (the mean or rate as a small chart, over the dashboard's window and bucket), leading to `/quality?name=…`, with a *See all* link to `/quality` when more names exist. One request, `GET /api/v1/stats/scores` with the page's window and grouping. The block is not rendered when the window holds no score, and its request is skipped when the block is hidden | The fourth thing a person opens the front page for is whether the scores moved, and the Quality screen already has the card; drawing it here rather than a summary number keeps one card, one meaning. Six is what fits in a row and what a project graded by more than six judges will still recognise; the rest are a click away. Ordered by count because the most-graded name is the one most likely to be the project's own verdict. |
| 6 | **2026-09-15** — **A fresh project** — the project has no traces at all, which is *Last trace* saying so — shows, in place of the summary row and charts, the instructions Traces shows today: *No traces yet*, the exporter settings with the copy button, the quickstart link. The block set below it is not rendered. Traces keeps a shorter empty state: *No traces yet* and a link to the dashboard, where the setup is. The snippet becomes one component rendered from both places | The dashboard is now the first screen a new project shows, and a first screen of empty charts says nothing about what to do next. The instructions move rather than copy so there is one text to keep true; Traces keeps a one-line pointer because "no traces" there means "nothing matches" as often as "nothing yet", and the filter branches stay. The condition is the listing's emptiness, not the window's: a project with traces last month and none this week is not fresh. |
| 7 | **2026-09-15** — The **time window is remembered** in this browser, under `localStorage` key `tracepad.range.<account id>`: a preset as its key (`24h`), re-resolved against the clock when applied; a calendar range as its `from`/`to`. The screens that open on a default preset today — the dashboard, Quality and the user page — read it when their URL carries no window, in place of that default; every screen with the range control writes it when the control changes, Traces and Sessions included. Traces and Sessions keep opening on *Any time*: a listing that silently narrowed itself to last week would hide traces from the person who came for one. The URL stays what it is: a window in the address wins, and a link opened by somebody else shows their window, not the reader's. Nothing in it goes to the server | A person who works in *the last 24 hours* sets it on every screen, every morning, because spec 007 #7 made the window part of the link and the link starts empty. Remembering the preset as a preset — rather than the instant it resolved to — is what keeps "the last 24 hours" ending now a week later; a calendar range names days and is remembered as the days. Per account, as the remembered project is (spec 028 #13), so two people on one browser do not share a window. Not in the server's preferences (Decision 9): the window changes many times a day and a `PATCH` per change is traffic for a setting whose loss costs one click. |
| 8 | **2026-09-15** — **Blocks and Customize.** The dashboard is ten blocks in a default order: `summary`, `traces`, `cost`, `tokens`, `latency`, `errors`, `models`, `environments`, `releases`, `quality`. A *Customize* button in the header enters a mode in which each block wears a handle and a hide control, blocks are reordered by dragging (`svelte-dnd-action`, whose keyboard path — focus the handle, space to lift, arrows to move, space to drop — is kept and announced), hidden blocks are listed in a strip at the bottom to be brought back, and *Reset* restores the default; *Done* leaves the mode. The order is one list across the summary, the charts and the breakdowns — a chart may sit above the summary, if that is what somebody wants. A hidden block does not make its own request (`quality`, the three breakdowns); the timeline request serves the five charts and is made while any of them shows. The shared x-axis cursor keeps linking the charts that show | Customization the size of the product: which of ten blocks, in what order. Not a widget builder — a metric × dimension × chart type × filter grid is a second product with its own schema, editor and storage, and the "navigation" it needs is what made the reference implementations hard to use. Hide and reorder answers the actual complaint — *I never look at tokens, and I want errors first* — with no new numbers to explain. Dragging because that is what reordering a grid looks like; the keyboard path because a control that cannot be reached without a mouse is not finished (design §8). |
| 9 | **2026-09-15** — The arrangement lives on the server, in **`accounts.preferences`** (schema 0019: `TEXT NOT NULL DEFAULT '{}'`, a JSON object, at most 16 KiB), carried as `preferences` on `Me.account` and written whole by `PATCH /api/v1/auth/me {"preferences": {…}}` — the same route that changes the name, `preferences` beside `name` and `password`, the object replaced rather than merged, validated only as *a JSON object under the size*; `422` otherwise. The dashboard's key is `dashboard`, an object keyed by project id: `{"dashboard": {"<project id>": {"order": ["errors", "summary", …], "hidden": ["tokens"]}}}`. Unknown block ids are ignored, blocks missing from `order` are appended in default order, so a block added by a later spec appears without a migration. The interface reads it from `me` (one call on load, as today), writes it on every change in Customize mode, and *Reset* deletes the project's key. A `403`/`422` from the write renders in the Customize strip and the change stays on screen | The person asked for the arrangement to follow them between browsers, which puts it on the account, and the account already has one read on load and one route that edits it. A preferences object rather than a typed resource: the server has no opinion on how a dashboard is arranged and should not need a migration when a block is added — the interface owns the shape, the server owns the bytes, and the size cap is what keeps an opaque object from becoming a store. Replaced whole because a merge of nested objects is a contract nobody can read back from a table; the interface holds the whole object from `me` and writes the whole object back. Per project because the blocks a person hides in a production project are not the ones they hide in a sandbox. Not the CLI's or MCP's business: a preference is how the interface is arranged, not something a program reads about the project (design §3 is about the data). |
| 10 | **2026-09-15** — A **project key** in the interface's position — there is none: the interface signs in with an account only (spec 028 #13) — so `preferences` is always an account's. The CLI and MCP never touch it. `GET /api/v1/auth/me` for a session whose account has never written preferences returns `"preferences": {}` | Stated so the question is not asked again on review: the key has no preferences because nothing a key does has a screen. |
| 11 | **2026-09-15** — The application-line ceiling (`UI_BUDGET`, spec 029 #9) rises from 20,500 to **21,200** | The summary row, the block frame with Customize, the last-trace line, the preferences client and the moved onboarding are one screen's worth of interface, ~500 lines by the count of the pieces they extend; the ceiling is spec 029's made current. Design §8's envelope is a guide, and the number it is checked against is this spec's scope. |
| 12 | **2026-09-15** — Every link that opens *the project* rather than a trace opens the dashboard, not only the three Decision 1 names: the login form with no `next` to return to, the setup and invitation screens once the password is set, a project just made from the switcher's *New project*, and the error page's way out. Traces keeps every link that names a trace, and the Server tab's *New project* keeps its tab (spec 029 #15) | Decision 1's rule — "Traces stops being what a link to *the project* opens" — applied to the four places that name `/traces` as a landing rather than a listing. Found while moving the redirects: each was a `bareTarget('/traces')` or `under('/traces', id)` written when the listing was the door. A project just made is the clearest case: its dashboard is where the exporter instructions now are (Decision 6), and landing a fresh project on an empty listing that points at the dashboard would be a hop the person did not ask for. |
| 13 | **2026-09-15** — Decision 2's *new* against a zero previous applies to the two changes that **divide** by it, traces and cost. The error rate and the p95 are differences, and a difference has an answer against zero: no errors last week and 3% this week reads `+3 pt`, coloured worse; a p95 against a zero previous reads the duration. Against an **absent** previous every change still reads *new* | Found in review: with the guard on every figure, a project with no errors in either window read `new` on every load, and the one movement the tile exists to catch — errors appearing where there were none — was the one it hid behind a word. Decision 2's own reason is "rather than a division", and only two of the four divide. |
| 14 | **2026-10-08** — **A lone value is a point.** `Chart` draws every point while a series has fewer than 40, as before, and past that draws a point for each value whose neighbours on both sides are gaps or the end of the series (`lonely` in `chart.ts`, pure, tested), through uPlot's `points.filter`. Nothing else about gaps moves: a bucket the server did not return is still `null`, never zero, and two adjacent values are still joined by the line. The rule is the component's, so the Quality screen and the dashboard's quality cards draw the same way | Found on a project read over *Last 7 days* by the hour, with a burst in one hour and a few traces scattered over the week: 168 points held a handful of values, each surrounded by gaps. uPlot joins two adjacent values and nothing else, and points were off past 40 — every chart was an empty frame under a y axis stretched to the burst, the scale of a value it did not draw. A point exactly where no line can go keeps "a gap is information" and stops a burst from being invisible; drawing all points at 168 would be noise, and filling the gaps with zero is the fabrication spec 007 #7 refuses. |
| 15 | **2026-10-08** — **Minutes.** `GET /api/v1/stats` takes `group_by=minute`: keys `YYYY-MM-DDTHH:MM:00Z` in UTC, the bucket shape of an hour, the same filters, `sessions` with `user_id` as on every timed grouping, no key for a minute that holds nothing. It is answered by the **live scan of `traces`** alone over `idx_traces_timestamp` — the rollup has no minutes (spec 013 #19) — and the window is bounded: `from` required, at most **24 hours** to `to` (or to now), with **five minutes of grace** for an open window resolved a moment before the request was read, by a client whose clock may run behind; anything else is `400` naming hour or day. An open window is scanned to that bound rather than to the end of the table. The enum grows in `openapi.json`, the CLI's `--group-by` and MCP `get_stats` (parity). On the dashboard, *Minutely* sits before *Hourly* and *Daily*; it is disabled over a window longer than a day or ending before the last day, with the reason written beside the buttons (`aria-describedby`), and a link with `group_by=minute` on such a window reads as unchosen — it is never sent. The interface offers minutes up to a day and a minute — a preset's age — leaving four minutes of the server's grace for latency and clock skew. The automatic size (spec 007 #14) is minutes for windows of **two hours or less that end within the last day**, and a chosen minute obeys the same recency: an older hour may have lost its traces to retention while the rollup still answers it by the hour. The API keeps answering minutes for any window of a day; the rule is the interface's. The size, the button and the request are read against one clock, set each time the dashboard asks — a refresh, a filter, a block shown or hidden — so an open window that has grown past a day falls back to hours rather than asking for a `400`. The summary row and the three breakdowns do not group by time and are untouched; the **quality cards stay hourly** under a minute timeline, because `stats/scores` is answered from `scores_hourly`. Quality and the user page keep hour and day | Found on the same project as #14: *Last hour* read by the hour is one point, and a twenty-minute run is a dot — the window people open while something is happening is the one the dashboard could not draw. Every number a timeline shows is already on the trace row (count, errors, cost, latency) or one join away (tokens), so minutes need no table, no migration and no aggregator: the live scan the hourly answer already uses for its open hour, with a finer key. The bound keeps it a scan of one day rather than of the database, through the index the hourly live tail already uses. Two hours as the automatic edge is #6's rule one size down: hours above 48 h would be two points or fewer of the coarser size, and so would minutes above 2 h — so *Last hour* is 60 points and *Last 24 hours* stays 24, not 1,440. The quality cards are not given minutes because that would be a minute rollup of scores, a new table for a block nobody asked to change. |
| 16 | **2026-10-08** — **Icons on the dashboard, and a plain caption for a missing comparison** (owner decision 2026-10-08). Each of the four tiles, the five charts, the three breakdown tables and the *Quality* block's heading wears a small muted glyph before its label, `@lucide/svelte` only, one icon per idea in one map (`ui/src/lib/dashboard-icons.ts`): Traces and Quality are the sidebar's own icons, *Cost* `circle-dollar-sign`, *Errors* `triangle-alert`, *Latency* `timer`, *Tokens* `text-initial` (letters, not coins, which read as Cost), *By model* `cpu`, *By environment* `layers`, *By release* `tag`. All glyphs are muted except *Errors* with something to show — a rate above zero on the tile, a series above zero on the chart — which takes the danger colour; they are `aria-hidden` and the label stays text. The per-score cards inside *Quality* have no icon: a score name is not one of these ideas. The tile's change line for a missing comparison reads **`no earlier data`** instead of *new*, in the muted colour with no arrow, and its screen-reader suffix *against the previous window* is said only when there is a comparison; the cases are Decision 2's and 13's, only the word changes. The application-line ceiling (`UI_BUDGET`, spec 015 #9) rises from 24,000 to **24,200** (owner decision 2026-10-08), and is not raised again for this change | *new* was meant as "nothing to compare with" and read as a status of the figure itself; saying what is missing is shorter than explaining the word, and a caption that is always there was chosen over an empty line because the tile keeps its height either way and a blank under a number reads as a loading failure. One map rather than imports in each component keeps *Cost* the same coin in the tile, the chart and anywhere a later block wants it, and a test holds that no two ideas share a glyph. `text-initial` is `letter-text`'s canonical file (the latter re-exports it and has no types of its own). The ceiling: `main` measured 23,959 lines; this change is ~85 non-blank lines (a little over the 30–60 a change this size was estimated at) (the nine imports of the map are the part that cannot shrink), landing at 24,043 (157 under the ceiling) |

## API contract

**`GET /api/v1/stats?group_by=total`** — policy `member`, every existing
filter (`from`, `to`, `environment`, `release`, `user_id`). Response:

```json
{
  "group_by": "total",
  "unit": "trace",
  "buckets": [
    {
      "key": "",
      "count": 2914,
      "error_count": 41,
      "total_cost": 12.4,
      "tokens": {"input": 8_120_400, "output": 611_200, "cache_read": 2_004_300},
      "latency_ms": {"p50": 655, "p95": 2380}
    }
  ]
}
```

Exactly one bucket when the window holds anything; **no buckets** when it
holds nothing (spec 007 #7: nothing fabricated). The percentiles are merged
over every hour's histogram in the window, rollup and live tail alike (spec
013's read seam); `total_cost` and `tokens` follow the existing absence
rules. With `user_id`, `sessions` is present as on the timed groupings.
`openapi.json` adds the value to the enum; the CLI's `--group-by` and MCP
`get_stats`'s `group_by` accept it through the same enum.

**`GET /api/v1/auth/me`** — `account` gains `preferences`: a JSON object,
`{}` until written. **`PATCH /api/v1/auth/me`** — the body accepts
`preferences` (an object) beside `name` and `password`; when present it
replaces the stored object. `422 "preferences: must be an object"`;
`422 "preferences: larger than 16 KiB"`. The response is the updated
`Me` as today. `openapi.json` and `schema.d.ts` describe the field as an
object with additional properties and no schema of its own.

Schema **0019**: `ALTER TABLE accounts ADD COLUMN preferences TEXT NOT NULL
DEFAULT '{}'`.

Nothing else on the API changes.

## Application contract

**Routes.** `routes/p/[project]/stats/` becomes `routes/p/[project]/dashboard/`;
a `stats/+page.ts` remains and redirects with the query. `routes/p/+page.ts`,
`routes/+page.ts` and `switchTarget` name `/dashboard`. The sidebar's
`SECTIONS` starts with `{ href: '/dashboard', label: 'Dashboard', icon:
LayoutDashboard }` (lucide) and drops the Stats row; `project.svelte.ts`'s
section set gains `dashboard` and keeps `stats` for the redirect.

**The screen.** `PageHeader` title *Dashboard*; meta: the last-trace line
(Decision 4) and the loading spinner; actions: *Refresh*, *Customize*. The
filter bar is unchanged (range, environment, hourly/daily). Below, the
blocks in the account's order (Decision 8), the hidden ones absent:

- `summary` — four `SummaryTile`s in a responsive row (four across on
  `lg`, two on narrower widths), each: label, the figure large in tabular
  numerals, the change under it with its glyph and colour (Decision 2),
  the previous figure in the tooltip. Loading shows the tiles with a
  placeholder figure at the same height (no layout shift).
- `traces`, `cost`, `tokens`, `latency`, `errors` — the five `Chart`s as
  today, in the existing two-column grid.
- `models`, `environments`, `releases` — the three `BreakdownTable`s as
  today.
- `quality` — `QualityCards`: the card component the Quality overview uses,
  extracted if it is not one already, six at most, plus *See all*.

The onboarding branch (Decision 6): `OnboardingCard` (the snippet, the
copy button, the quickstart link) shown instead of the blocks when the
last-trace read returns nothing and no error; the filter bar stays.

**Customize mode.** `customizing` state on the page; the block frame
(`DashboardBlock`) renders the handle and the hide button only in that
mode; `svelte-dnd-action` on the block list with `flipDurationMs` honouring
`prefers-reduced-motion`; the hidden strip lists hidden blocks by label with
*Show*; *Reset* and *Done* in the header replace *Customize*. Writes go
through `preferences.svelte.ts`: `dashboard(projectId)` reads the
arrangement out of `auth.me.account.preferences`, `setDashboard(projectId,
arrangement)` writes the whole preferences object back with
`api.patchMe({preferences})` and updates `auth.me` from the response; a
failure keeps the on-screen arrangement and shows the server's message in
the strip.

**The remembered window** (Decision 7): `range.svelte.ts` (or an extension
of `api/range.ts`) with `rememberedRange(now)` and `rememberRange(range,
now)`; the three screens that apply a default preset on an empty URL (the
dashboard, Quality, the user page) call `rememberedRange` instead and fall
back to their default; every `RangePicker` change site (those three, Traces,
Sessions) calls `rememberRange`. A range matching a preset (`matchPreset`)
is stored as `{preset}`; otherwise as `{from, to}`.

**Stats client.** `api.getStats` accepts `group_by: 'total'`; `stats.ts`
gains `summarize(bucket, previous)` returning the four figures with their
changes, pure, tested.

## Testing

Unit (vitest):

- `summarize`: percentage and point changes, *new* on a zero or absent
  previous, dashes on absent figures, the p95 change as a duration.
- `preferences.svelte.ts`: an arrangement read from `me`, unknown ids
  dropped, missing ids appended in default order, reset deletes the key,
  a write failure keeps the state and surfaces the message.
- `rememberedRange`: a preset is re-resolved against the clock given,
  a calendar range comes back as dates, a malformed value falls back to
  the default, keys are per account.
- Sidebar and `project.test.ts`: `/dashboard` first, `/stats` absent from
  the sections, the redirect keeps its query, `switchTarget` with no section
  lands on `/dashboard`.
- The dashboard page (component test): the blocks render in the order
  given, hidden ones do not, a hidden `quality` block makes no request,
  the onboarding card replaces the blocks when the listing is empty, the
  last-trace line renders relative time and *No traces yet*.
- Traces: the empty state without filters links to the dashboard.

Go:

- `group_by=total`: one bucket over a window spanning rollup and live
  hours, its p95 equal to the merged histogram's (compare against a window
  covered by rollup only, and against `group_by=hour` summed for count and
  cost); no buckets on an empty window; with `user_id`, `sessions`
  present; enum validation refuses `group_by=all`.
- `preferences`: `{}` on a fresh account, round-trip through `PATCH`,
  `422` for an array, a string and a 17 KiB object, an owner's `PATCH
  /accounts/{id}` does not touch it, `DELETE` removes it with the row.
- Parity tests: the enum in `openapi.json`, the CLI table, MCP's schema.

Browser (chrome-devtools, part of the definition of done): open a project
and land on the dashboard; the summary row shows figures and changes on
the seeded data; switch to *Last 24 hours*, open Traces, see the same
window; Customize — drag *Errors* above *Summary*, hide *Tokens*, reload,
the arrangement holds; sign in from a second browser context, the
arrangement holds there; Reset restores; a fresh project shows the
onboarding card; both themes; a phone width.

## Edge cases

- **A window with an explicit `to` in the past.** The previous window is
  the same length before `from`; the last-trace line still reads the live
  listing with no window (Decision 4).
- **A window whose previous window predates the project.** The previous
  bucket is absent; every change reads *new*.
- **`stats_retention_days` shorter than twice the window.** The previous
  window may be partly or wholly swept; the change is against what
  remains, and a wholly absent previous reads *new*. Documented next to
  the rollup retention note.
- **A project reached through the bare `/stats` link.** Spec 029 #3
  prefixes the project, then this spec's redirect lands on
  `/dashboard` — two hops, one visible.
- **Preferences written by another tab.** Last write wins; the object is
  small and the arrangement is not collaborative. A tab reads `me` on
  load and after its own writes only.
- **A block id this build does not know** (from a newer build that added
  one): ignored on read, preserved on write — the interface writes back
  the object it holds, with only the project's `order` and `hidden`
  replaced.
- **A hidden block that is the only one showing a request's data.** The
  timeline request is made while any chart shows; when all five are
  hidden it is skipped.
- **Quality with more than six names.** The six with the most scores;
  *See all* names how many are there.
- **The project has traces but the window has none.** The summary row
  shows dashes, the charts their empty axes, the last-trace line the true
  time: not the onboarding card.
- **A viewer role.** Customize is available: the arrangement is the
  account's, not the project's.
- **Reduced motion.** No flip animation while dragging; the keyboard path
  is unchanged.

## Config additions

`UI_BUDGET := 24200` (Decisions 11 and 16). No server configuration.

## Out of scope

A widget builder (a metric, a dimension, a chart type and a filter per
widget); several named dashboards; a dashboard shared by a project or a
team; a dashboard across projects for owners; live refresh of the front
page; remembering the window on the server; the CLI or MCP reading or
writing preferences; any new metric, grouping (beyond `total`) or chart.
