# Spec 005 — Retention & Admin API

**Status:** 🔄 IN PROGRESS
**Sprint:** September 2026

> Retention is a first-class feature (design §5.5): the store must be able to
> forget — on a schedule, on a user-erasure request, and on project deletion —
> without an operator ever running SQL. This spec ships the retention sweeper
> and the admin surface (projects, keys, retention, user-data erasure), under
> one safety model: every destructive operation is a dry run until confirmed,
> and the most destructive one is reversible for a week. The agent-facing
> surface (MCP) deliberately gets none of it.

---

## Overview

Deliverables:

- Retention sweeper: hourly goroutine deleting expired traces (and their
  observations, payloads, scores) and expired raw batches, in chunks, through
  the group-commit writer; incremental vacuum so the file actually shrinks.
- Admin API under `/api/v1`: projects CRUD (soft delete + restore), API-key
  create/list/revoke, retention settings, `DELETE
  /projects/{p}/users/{user_id}/data` (user-data erasure, design §5.5).
- Dry-run/confirm contract on every destructive endpoint.
- CLI: `projects`, `keys`, `retention`, `users rm-data` command groups —
  pure API clients, interactive confirmation on a TTY, `--yes` for scripts.
- Schema 0005: `traces.ingested_at`, nullable `retention_days`,
  `raw_retention_days`, `deleted_at`, sweep index, vacuum mode switch.
- Docs: `docs/retention.md` (incl. the data-erasure position), `docs/admin.md`;
  `docs/api.md`, `docs/cli.md`, `openapi.json` updated (same PR).

MCP is intentionally untouched: it remains a read-only surface (Decision 13).

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | The retention clock is **arrival, not the client's clock**: `traces.ingested_at` (Unix ns, server clock) is set when the trace row is first created, and `retention_days` is compared against it | `traces.timestamp` is client bytes: a client that sends the past gets its trace deleted on the next sweep, one that sends the future (within the 1678–2262 window ingest accepts) gets immortality. Retention means "how long *we* keep what *we* received" — only the server's clock can say that. A late span arriving after its trace was swept recreates a fragment that lives another N days from *its* arrival: predictable, and the honest answer for data we genuinely hold again. `ingested_at` is not refreshed by later spans — a trace's lease starts once. |
| 2 | `retention_days` becomes nullable; **NULL means keep forever, and NULL is the default** — for new projects and, via migration, for every existing one | A self-hosted tool must not silently discard the data of someone who installed it and configured nothing. The 0001 default of 30 never acted (no sweeper existed), so setting existing rows to NULL preserves every deployment's observed behavior; a finite window is an explicit opt-in from this spec on. The cost is unbounded growth for the unconfigured — at the reference rate (~22 MB/month) that is the right default for this audience. |
| 3 | The sweeper is one goroutine on a `TRACEPAD_SWEEP_INTERVAL` tick (default 1h), deleting per project in chunks of ~1000 traces, each chunk a `WriteJob` through the group-commit writer | Design §5.5 (short transactions, no long locks) plus spec 003 #9's invariant: there is exactly one write path. A second write connection would reintroduce the BUSY_SNAPSHOT class 003 #24 killed; as a `WriteJob` the sweeper serializes with ingest for milliseconds at a time and inherits fsync semantics for free. Deletion order within a chunk: observations → payloads → scores → the traces themselves; raw batches by their own window (Decision 6). |
| 4 | The sweep also collects **orphaned payloads**: rows no longer referenced by any `metadata_id`/`input_id`/`output_id` column, left behind by overwriting upserts | Migration 0002's own comment defers exactly this ("orphan cleanup after an overwriting upsert is the retention stage's concern") — this spec is that stage. `payloads` has no owner column by design (spec 002 #8), so referenced ids are gathered from the deleting rows inside the chunk transaction, and a periodic orphan pass catches the upsert leftovers. |
| 5 | Migration 0005 switches the database to `auto_vacuum=INCREMENTAL` (one full `VACUUM` to flip the mode), and the sweeper runs `PRAGMA incremental_vacuum(N)` after chunks | Deleting rows without vacuum returns nothing to the OS, and "the file actually shrinks" is the operator-visible half of retention (design §5.5). The one-time VACUUM is acceptable exactly now: pre-beta files are megabytes. Incremental thereafter — a full VACUUM rewrites the file under a lock the writer cannot afford. |
| 6 | Raw batches get their own per-project window: `raw_retention_days`, **NULL = follow `retention_days`**; swept by `received_at` (already server arrival) | Raw is the insurance policy (design: dialect drift, `remap`, `export --otlp`) — a shorter default would silently cap all three, so by default it lives exactly as long as the parsed data. Operators for whom raw is the heavy, sensitive part shorten it deliberately. A batch is swept only by its own age — never because the traces it fed were swept, since one batch feeds many traces with different fates. |
| 7 | `DELETE /api/v1/projects/{p}/users/{user_id}/data` erases the user's **parsed** data synchronously — traces where `user_id` matches, their observations, payloads, and scores — and does not touch raw batches; `docs/retention.md` states the archive position | The operator, not tracepad, is the GDPR controller; our job is the tool. Erasure from queryable stores is immediate (well inside the one-month Art. 12(3) response window). Raw is a documented archive: not served by any read endpoint, expiring on a schedule — the standard backup posture regulators accept — with two stated caveats: the full-erasure guarantee assumes a bounded raw window, and a `remap` before that window expires can resurrect erased data. Strictly regulated deployments run `TRACEPAD_STORE_RAW=off`; the doc says so. |
| 8 | Every destructive endpoint is a **dry run by default**: without `confirm` it changes nothing and returns a preview of the consequences (entity names, row counts, oldest affected date); it executes only when `confirm` echoes the identity of what is destroyed — the project **name** for project deletion and retention decrease, the **user id** for user-data erasure, the project name for revoking a project's last key | An id is a string you paste; a name is a thing you mean. Requiring the echo makes the confirmed request self-describing — a typoed or hallucinated target cannot match — which is spec 003 #23's rule ("a request that cannot mean what it says is rejected") applied to destruction. The preview is the same response an interactive client shows before asking, so CLI and UI confirmation dialogs cost the server nothing extra. |
| 9 | Project deletion is a **soft delete with a fixed 7-day grace**: keys stop authenticating immediately, the project vanishes from listings (admin `?include=deleted` shows it with its purge date), `POST /api/v1/projects/{id}/restore` undoes it, and the sweeper purges the data after the window | The most irreversible operation in the product gets a rollback: an agent- or fat-finger-triggered deletion is recoverable until the following Monday. Seven days fixed, not a knob — a knob would make the guarantee configuration-dependent, and the point of a safety net is knowing it is there. Purging rides the existing sweeper chunks, which also answers sync-vs-async: deletion never blocks the writer, and the HTTP answer is 202 with the purge date. The name stays reserved during grace (creating a same-named project → 409 naming the restorable one) so restore always has its name to come back to. |
| 10 | During the grace window, a deleted project's keys authenticate **only** `GET /api/v1/projects/{id}` and `POST …/restore` | Decision 9 kills the keys at deletion, but without this exception a no-admin-token deployment (the default install) that deletes its only project has no credential left that could restore it — the safety net would have a hole exactly where it is needed most. Ingest and reads stay dead; the keys' one remaining power is undoing the deletion. |
| 11 | A project's `sk` key is the **admin of its own project**: retention get/set, key create/list/revoke, user-data erasure, restore (Decision 10). `TRACEPAD_ADMIN_TOKEN` is required for everything cross-project: create/list/delete/restore *any* project, read or change *another* project's settings. Without a configured token those endpoints answer 403 with an explanatory message | Resolves the design's tension: "retention changes without restart" must hold in the default (token-less) install, so the project's own credential does project-scoped administration. Project **deletion** is the exception — admin token only, even for one's own project: an `sk` key lives in application config and CI, and a leaked app credential must not be able to destroy the project's data. In a token-less install, deleting a project means setting the token or using the declarative env path — a deliberate speed bump on the one act with a blast radius. |
| 12 | Multiple active key pairs per project: `POST …/keys` creates a pair (the secret is returned once and never again — only its hash is stored, per 0001), `GET …/keys` lists public keys with creation dates, `DELETE …/keys/{pk}` revokes by row deletion. Revoking the last active key requires `confirm` per Decision 8 | Zero-downtime rotation: create the new pair, move the SDKs, revoke the old — no window where ingest 401s. The 0001 schema already holds N rows per project; this spec just stops pretending it holds one. A project with zero keys cannot ingest, hence the guard on the last one — allowed (declaratively provisioned deployments re-add keys from env), but never by accident. |
| 13 | MCP stays read-only: **no admin tools**, this spec adds nothing to the MCP surface. Admin lives in the HTTP API and the CLI, where the CLI confirms interactively on a TTY (showing the Decision 8 preview) and requires `--yes` when non-interactive | Agents should not hold destructive capability at all — a hallucinated tool call must have nothing to destroy. "The MCP surface cannot modify or delete anything" is a guarantee worth printing in bold in `docs/mcp.md`, and it is only printable if it is structural. Spec 004 #18's `readOnlyHint: true` on every tool remains literally true. |
| 14 | Retention changes take effect on the next sweep pass (≤ the sweep interval); there is no run-now endpoint. `GET /api/v1/system` grows a `sweeper` section: last run time, traces and raw batches purged since process start, next scheduled run | An immediate-sweep endpoint is a destructive trigger with none of Decision 8's semantics — the hourly cadence *is* the safety margin after a mistaken retention change (the dry-run preview already told the caller what the new window hits). Tests drive the sweeper directly and via a short `TRACEPAD_SWEEP_INTERVAL`. Observability follows spec 004 #10's pattern: honest in-process counters, scoped per Decision 33 of 004 where project-owned. |
| 15 | **Rejected: one SQLite database per project.** The store stays a single shared database file | Per-project files would make project deletion an `rm` and give structural cross-project isolation — but deletion is the *rare* operation, and retention, the frequent one, cuts *inside* a project by time, needing the same chunked sweeper in either world. The costs are structural: the "one file, backup = copy" promise becomes a directory plus a control-plane database (auth must resolve keys before any project file opens), migrations × N files with partial-failure states, and N `synchronous=FULL` writers contending for the same disk where the single group-commit writer wins precisely by batching. Isolation is held by discipline instead: every read and counter scoped by project (004 #33) and cross-project leak tests. Time-sharded monthly files (design §5.6) remain the documented ceiling answer and are orthogonal to this choice. Revisit only with evidence: hundreds of projects per instance, or sweep churn measurably degrading ingest. |
| 16 | *2026-08-28, in implementation:* migration 0005 **rebuilds `traces`** rather than adding `ingested_at` with `ALTER TABLE`, so the column has no default | `ALTER TABLE ADD COLUMN` cannot add a NOT NULL column without a constant default, and every candidate default is a lie: `0` dates a forgotten insert to 1970 and hands it to the next sweep, a far-future constant makes it immortal and invisible. Rebuilt, the column has no default at all, so a write that cannot say when it arrived fails loudly. The cost is one table copy, at the moment Decision 5 already declared a full file rewrite acceptable. Both rebuilds follow SQLite's documented procedure — foreign keys off, `PRAGMA foreign_key_check` before commit — because with them on, `DROP TABLE projects` performs an implicit `DELETE FROM` and every `ON DELETE CASCADE` would empty the database. |
| 17 | *2026-08-28, in implementation:* within a chunk the deletion order is observations → scores → traces → **payloads last**, not the "observations → payloads → scores" of Decision 3 | `traces.metadata_id` and `observations.{input,output,metadata}_id` are foreign keys *into* `payloads`, so a payload row cannot go while a row pointing at it is still there. The referenced ids are gathered from the deleting rows first, exactly as Decision 4 requires, and the payload rows follow their owners out. |
| 18 | *2026-08-28, in implementation:* `confirm` is a **query parameter** on every destructive endpoint, not a body field | The endpoints that need it span `DELETE` (no natural body) and `PATCH` (a body that describes the wanted state, not the ceremony around it), and one rule beats two: `?confirm=<name>` reads the same on all of them and keeps `curl -X DELETE ".../projects/$ID?confirm=my-project"` the whole of the interaction. The value is a name or a user id — never a secret — so it costs nothing in an access log. |
| 19 | *2026-08-28, found in review of PR #6:* a retention window is **bounded at 36500 days**; "keep it essentially forever" is `null`, not a large number | A window is turned into a nanosecond cutoff, and past ~106751 days that multiplication overflows int64 and wraps the cutoff into the *future*, where it matches every row there is. Worse, going from 30 days to a million is not a *shrink*, so Decision 8's preview and echo would never have been asked for: `retention set --days 999999`, meaning the opposite, would have emptied the project on the next sweep with no confirmation at all. The ceiling is two orders of magnitude below the wrap, the API and the CLI both refuse anything above it, and a value beyond it already in the database is read as "keep forever" — of the two ways to be wrong about a deletion, not deleting is the recoverable one. |
| 20 | *2026-08-28, found in review of PR #6:* the 0005 backfill reads a **non-positive `timestamp` as the absence of a date**, not as 1970, and gives those rows the migration time | Decision 1 says `min(timestamp, migration time)`, which assumes `timestamp` is a date. Migration 0004 writes `timestamp = 0` for a trace whose every span carried an unset start time, so `min` returns 0 and those traces arrive dated 1970 — deleted by the first sweep after any window is set, and invisible in the dry run, whose "oldest" reads 0 as "nothing affected". It is the exact silent-mis-dating failure Decision 16 rebuilt the table to prevent, arriving through the data instead of through the schema. |

## API contract

All under `/api/v1`, same auth model as spec 004 plus `TRACEPAD_ADMIN_TOKEN`
as a Bearer token where Decision 11 requires it.

Projects: `GET /projects` (admin: all; sk: own only), `POST /projects`
(admin), `GET /projects/{id}`, `PATCH /projects/{id}` (rename admin-only;
`retention_days`, `raw_retention_days` per Decision 11 — decrease needs
Decision 8 confirm), `DELETE /projects/{id}` (admin, Decisions 8+9),
`POST /projects/{id}/restore` (Decisions 9+10).

Keys: `POST /projects/{id}/keys`, `GET /projects/{id}/keys`,
`DELETE /projects/{id}/keys/{public_key}` (Decision 12).

Erasure: `DELETE /projects/{id}/users/{user_id}/data` (Decisions 7+8) —
response reports deleted counts per store.

Previews (Decision 8) return `200 {"dry_run": true, "would_delete": {…},
"confirm": "<what to echo>"}`; confirmed destructive calls return the final
counts (or `202` + purge date for project deletion). A wrong `confirm` value
is `400` and does nothing.

Every new endpoint appears in the router table, `GET /api/v1`, and
`openapi.json` with the parity tests of spec 004 #9/#27 extended to cover
methods beyond GET.

## Data contract

Migration `0005_retention_admin.sql` (STRICT, forward-only):

- `traces` gains `ingested_at INTEGER NOT NULL` — backfilled as
  `min(timestamp, migration time)` (timestamp is total since 004 #26);
  new index `idx_traces_ingested ON traces(project_id, ingested_at)` for the
  sweep window (EXPLAIN QUERY PLAN verified, method of 003 #25).
- `projects` rebuilt (STRICT tables cannot alter column constraints):
  `retention_days INTEGER` nullable, existing rows set to NULL (Decision 2);
  new `raw_retention_days INTEGER` (Decision 6) and `deleted_at INTEGER`
  (Decision 9).
- Vacuum mode switch per Decision 5.

## Config

| Variable | Default | Purpose |
|----------|---------|---------|
| `TRACEPAD_SWEEP_INTERVAL` | `1h` | Sweeper cadence (Decision 3) |
| `TRACEPAD_ADMIN_TOKEN` | unset | Cross-project admin auth (Decision 11) |

## Testing

1. Sweep correctness: only traces past their project's window go, with their
   observations, payloads, and scores; a NULL-retention project is untouched;
   other projects' data is untouched (cross-project isolation test).
2. Orphan payloads from overwriting upserts are collected (Decision 4).
3. Sweeper vs. ingest concurrency through the writer: no lost acks, no
   BUSY errors, method of 003 #24's harness.
4. File size shrinks after a sweep of a bulk-loaded project (Decision 5).
5. Soft delete → keys 401 everywhere except restore → restore → keys work,
   data intact; purge after grace; name reservation 409.
6. Dry-run/confirm on every destructive endpoint: preview mutates nothing,
   wrong echo 400s, correct echo executes.
7. Auth matrix: sk on own vs. other project, admin token, no token → 403.
8. User-data erasure: parsed stores emptied for the user, raw batches intact,
   other users' data intact.

## Out of scope

UI for any of this (design §8 iteration 2); rollup tables; time-sharded
database files (design §5.6); a metrics subsystem beyond `/system`.
