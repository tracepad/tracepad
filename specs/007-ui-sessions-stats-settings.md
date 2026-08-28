# Spec 007 — Sessions, Stats & Settings: the API Grows Into All Three Clients

**Status:** 🔄 IN PROGRESS
**Sprint:** September 2026

> Spec 006 built the shell and the core value path; this spec finishes the
> MVP screens (design §8): Sessions, Stats and Settings. One of them needs
> something the API does not have — a session listing — and that makes this
> spec the first time the API surface grows after 004. The growth happens the
> only way the design allows: the endpoint lands in the API, the CLI, MCP
> and the UI in the same PR, so no client ever knows more than the others
> (design §3).

---

## Overview

Deliverables:

- **API**: `GET /api/v1/sessions` — cursor-paginated session listing
  aggregated from traces; router table, `GET /api/v1`, `openapi.json` and
  the parity tests extend as spec 004 built them to.
- **CLI**: `tracepad sessions ls` (filters, `--json`). **MCP**: a
  `list_sessions` tool wrapping the endpoint 1:1 (spec 004 #16/#17 rules).
- **Sessions** screen: session list + session view (totals, its traces).
- **Stats** screen: four uPlot time-series charts + model/environment
  breakdown, over the existing `GET /api/v1/stats`.
- **Settings** screen: own-project management with the sk key (rename,
  retention, keys, user-data erasure) and an **Administration** section
  unlocked by the admin token (project lifecycle: list, create, delete,
  restore) — every destructive card rendering the server's dry-run/confirm
  contract from spec 005.
- A shared date-range component (bits-ui DateRangePicker + presets) used by
  Stats and the Traces filter bar.
- Docs: `docs/ui.md`, `docs/api.md`, `docs/cli.md`, `docs/mcp.md` updated.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-08-29** — The session listing ships in **all three clients in one PR**: `GET /api/v1/sessions`, `tracepad sessions ls`, MCP `list_sessions`, and the UI screen that motivated it | The UI needing an endpoint the API lacks is exactly the moment design §3 exists for: if the endpoint landed for the UI alone, "everything the UI can do, the CLI/MCP/agent can do" would become false on the first growth of the surface. This PR sets the precedent: the API is the product, and it grows for all consumers at once. |
| 2 | **2026-08-29** — `GET /api/v1/sessions` aggregates from `traces` (`session_id IS NOT NULL`): rows `{id, trace_count, error_count, total_cost, first_seen, last_seen}`, ordered `last_seen DESC, id DESC`, keyset cursor over that pair, filters `from`/`to` (on trace `timestamp`, half-open like the trace list), `environment`, `user_id`. No new table and no ingest-time aggregates | Sessions are a grouping of traces, not an entity we store (spec 002 made traces the unit); maintaining session aggregate rows at ingest would buy speed this product's scale does not need and cost a second source of truth. The keyset pair mirrors the trace list (spec 004 #4); the query is EXPLAIN-verified in a test (the 003 #25 method) against `idx_traces_session`, and a cross-project isolation test guards it like every listing (004 #33). |
| 3 | **2026-08-29** — Settings runs on **two credentials with disjoint powers**: the session's sk key manages its own project (rename, retention, keys, erasure — spec 005 #11's own-project scope), and an Administration section unlocks when an **admin token** is entered, stored separately in `localStorage` and sent only to the admin endpoints that require it. The admin token never touches the data plane | This closes the question 006 #13 deferred. The admin token stays exactly what 005 #11 made it — the management-plane credential — so no server change and no new read scope is needed; the UI composes the two credentials instead of asking the server to blur them. Login keeps rejecting the admin token (006 #13 unchanged): it is entered in Settings, where the endpoints it can call actually live. |
| 4 | **2026-08-29** — The Administration section covers **project lifecycle only**: list (including soft-deleted with their purge dates), create (the minted key pair shown once), delete (confirm-echo of the name), restore. Editing *another* project's retention or keys stays with that project's own key or the CLI | Lifecycle is what only the admin token can do (005 #11) — that is the section's reason to exist. Cross-project settings editing would need a project picker wired into every settings card and a per-project admin view: real budget for a task the CLI already does in one line, and a UI blur of 005's deliberate credential split. |
| 5 | **2026-08-29** — Destructive UI is the server's contract, rendered: every destructive card (retention decrease, key revoke, erasure, project delete) first shows the **server's dry-run preview** — the same response the API returns without `confirm` — and enables its execute button only when the person has typed the identity echo (project name / user id) into the card. The UI never fabricates a preview client-side | Spec 005 #8 built dry-run/confirm precisely so that "CLI and UI confirmation dialogs cost the server nothing extra" — this spec is that promise cashed in. Typing the echo in the browser is the same speed bump the API enforces, so the UI cannot be a softer path to destruction than curl; and a preview computed by the server is the only one that tells the truth about row counts. |
| 6 | **2026-08-29** — Stats renders the four time series — volume, cost, latency p50/p95, errors — with **uPlot** over `group_by=hour\|day` buckets, and the model/environment breakdown as a **table with CSS proportion bars**, no second chart library | uPlot (design §8, ~40 KB) is built for aligned time series and earns its place on the four charts. `group_by=model\|environment` returns categories, not series: category bars in uPlot mean hand-built paths and poor long-label ergonomics, while a table row with a proportional bar reads better exactly where model names get long — and costs zero dependencies. Bucket granularity: a switcher, defaulting to `hour` for ranges ≤ 48 h and `day` above. |
| 7 | **2026-08-29** — One shared date-range component: bits-ui **DateRangePicker** (calendar, styled with our tokens) plus preset shortcuts (1 h / 24 h / 7 d / 30 d), used by Stats and the Traces filter bar; the value lives in the URL as the `from`/`to` the API takes | The API speaks half-open RFC 3339 ranges everywhere, so the range UX should be built once and mean the same thing on every screen. Presets cover the daily reflex; the calendar answers "what happened last Tuesday" without hand-typing timestamps. bits-ui is already in the tree (006 #3) — its date-range primitive is the a11y-hard kind of component that headless libraries exist for. Stats defaults to the last 7 days. |
| 8 | **2026-08-29** — No live mode on Sessions or Stats: both refetch on filter change and on an explicit refresh control; the Traces live toggle (006 #12) stays the only poller | Live-updating charts re-render whole series every tick for a screen people glance at, not stare at; and a session's aggregates move only when its traces do — the traces screen is where arrival is watched. One poller in the app keeps the "who hammers the server" question one-line auditable. |
| 9 | **2026-08-29** — Key management UX: minting shows the pair **once** in a dialog with copy affordances (both env formats, as first-run prints them); the list shows public keys with creation dates; revocation follows 005 #12 — the last key's revocation demands the confirm-echo card like any destructive act | The secret is stored hashed (005 #12) and can never be shown again — the UI must say so instead of pretending otherwise. Rendering both connection formats makes the dialog the same artifact first-run prints, so docs teach one shape. |

| 10 | **2026-08-29** — In `GET /api/v1/sessions`, the filters select **traces**, and the roll-up describes the traces they selected: a window narrows `trace_count`, `total_cost` and the `first_seen`/`last_seen` pair to what happened inside it, rather than reporting the session's whole life beside a window it does not match | Decision 2 fixed "a session matches when any of its traces falls in the window" and left the totals unsaid. Both readings are defensible, and this one is the one the aggregation actually computes — a `WHERE` before a `GROUP BY`. It is also the one the Stats screen's neighbour already gives: "cost in the last 24 h" means cost incurred in the last 24 h everywhere else in this API. `GET /api/v1/sessions/{id}` takes no window and so keeps reporting the whole session, which is what a detail view is for. |
| 11 | **2026-08-29** — The CLI filter is spelled `tracepad sessions ls --env`, not `--environment` as the spec's CLI line wrote it | Every other command in the binary spells it `--env` (`traces ls`, `traces last`, `tail`, `stats`), and a flag that is `--env` on four commands and `--environment` on the fifth is a papercut on every muscle-memory invocation. The API parameter is `environment` either way; the CLI's job is to be a comfortable front for it (spec 004 #1), not to transliterate it. |
| 12 | **2026-08-29** — Renaming the project stays on the Settings project card, as the Screens contract asks, but it is the one control there that runs on the **admin token**: the field and its button are disabled with an explanation until the Administration section below is unlocked | `PATCH /api/v1/projects/{id}` accepts a new name only from the admin token (spec 005 #11 — a project's name is the echo every destructive confirmation is typed against, so renaming one is a cross-project act). Decision 3 rules out changing the server for the UI's convenience, and hiding the control would leave the screen quietly less capable than `tracepad projects rename`. Showing it, disabled, with the reason and the CLI equivalent, is the honest third option — and it keeps the token on the endpoints that demand it. |
| 13 | **2026-08-29** — The UI-line budget in `scripts/ui-lines.sh` moves from 5,000 to 7,300 | 5,000 was spec 006's own scope. 7,300 is this spec's Budget line made executable — 3,813 after 006 plus the ~3,500 it allows — so the warning fires exactly when *this* spec's budget is exceeded rather than at a number two specs old. Design §8's 6–9k envelope is unchanged. |

## API contract

`GET /api/v1/sessions` — filters: `from`/`to` (RFC 3339, half-open, a
session matches when any of its traces falls in the window), `environment`,
`user_id`. Response: `{"sessions": [{"id", "trace_count", "error_count",
"total_cost", "first_seen", "last_seen"}], "next_cursor"}` — newest activity
first (`last_seen DESC, id DESC`), opaque cursor (spec 003 #18 shape).
`total_cost` is null when no trace in the session carried a cost, mirroring
the stats contract. Authenticated by project key like every read.

CLI: `tracepad sessions ls [--since …] [--until …] [--environment …]
[--user …]` — TTY table / `--json`, cursor walked the way `traces ls` does.

MCP: `list_sessions` — thin wrapper, `structuredContent` byte-identical to
the endpoint (004 #16), description written as a when-to-use trigger.

## Screens contract

Routes: `/sessions`, `/sessions/{id}`, `/stats`, `/settings` — sidebar
entries appear for all three areas; filter and range state in the URL.

**Sessions.** Table: session id (mono), trace count, errors, total cost,
first seen, last seen; the shared range picker plus environment/user
filters; keyset "load more". Row → `/sessions/{id}`: a totals header
(the `GET /api/v1/sessions/{id}` fields) over the session's traces rendered
with the same table component as the Traces screen — a row opens the trace
detail. Empty state explains that sessions appear when the SDK sends
`session.id`.

**Stats.** The shared range picker + environment filter + bucket switcher
(hour/day, auto default per Decision 6); four uPlot charts — traces
(count), cost, latency (p50 and p95 as two series), errors — sharing one
x-axis cursor; below, two breakdown tables (by model, by environment) with
proportion bars for count/cost/errors. Empty buckets render as gaps, never
fabricated zeros (the API already refuses to fabricate). Charts respect
`prefers-reduced-motion` and both themes (uPlot theming via our tokens).

**Settings.** Own-project sections, all on the sk key: project (name +
rename), retention (`retention_days`, `raw_retention_days`, NULL rendered
as "keep forever"; a decrease runs the dry-run → confirm-echo card per
Decision 5), API keys (Decision 9), danger zone (user-data erasure with
dry-run preview → user-id echo). Below, **Administration** (Decision 3-4):
admin-token input with a "what this unlocks" note; once unlocked — project
table (incl. soft-deleted with purge dates and restore buttons), create
project (keys-once dialog), delete project (confirm-echo card). A 403 from
an admin endpoint renders the server's explanatory message verbatim.

## Testing

- **Go**: aggregation correctness incl. traces without `session_id`
  excluded and window overlap semantics; keyset walk at `limit=1` with a
  duplicate `last_seen` tie; EXPLAIN QUERY PLAN asserts the session index;
  cross-project isolation; parity tests pick up the route automatically —
  a failure here is a missing table entry, not a new test.
- **CLI/MCP**: golden output for `sessions ls`; `list_sessions` result
  byte-identical to the endpoint's JSON.
- **vitest**: range-picker ↔ URL round-trip incl. presets; bucket
  auto-default boundary (48 h); stats series mapping (gaps stay gaps, cost
  null ≠ 0); confirm-echo cards (button disabled until echo matches, server
  preview rendered, wrong echo → 400 surfaced); admin unlock (token stored
  separately, sent only to admin endpoints — asserted by a fetch-spy test);
  breakdown bar math. Contrast audit extends to any new tokens.
- **E2E**: ingest a fixture with sessions → list shows aggregates → session
  view → trace detail; stats renders all four charts over the fixture and
  the breakdown tables match; settings: retention decrease dry-run shows
  counts → echo → applied; mint key (secret visible once) → revoke; admin
  unlock → create project → delete with echo → restore from the deleted
  list. Every scenario on desktop and 375×812; both themes smoke-checked;
  zero external-origin requests re-asserted.
- **Live check** (process, not CI): chrome-devtools MCP pass over all three
  screens, desktop + mobile emulation, clean console.

## Budget

`ui/` after this spec stays inside design §8's ~6–9k total (3813 after 006;
target ≤ ~3.5k added). Go side: one endpoint + CLI group + MCP tool, in the
established shapes.

## Out of scope

- Prompts, datasets/runs, annotation queues — design §8 iteration 2.
- Search/FTS; server push; admin editing of another project's settings
  (Decision 4); any new admin-token read scope (Decision 3).
