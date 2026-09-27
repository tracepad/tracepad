# Spec 047 — Erasure at scale: chunks that follow the hours, and an erasure that answers at once

**Status:** 🚧 IN PROGRESS
**Sprint:** September 2026

> Spec 044 made an erasure reach every copy the store holds, and measured
> what that costs (044 #20 l): a user of 20,000 traces spread over 30 days
> took 1,151 s under load. 8.6 s went to the raw archive, 0.24 s to the tail,
> and **19 minutes to the parsed phase's 10,664 chunks**. The chunks are small
> because of how they are chosen, not because of the hour bound itself. Each
> one reads the user's next 500 traces in arrival order, keeps those of the
> first hour it meets and leaves the rest for later. When traces arrive in
> the order they started, that is a full chunk. When they do not, it is about
> two traces, one commit window and one whole-hour roll per chunk. Meanwhile
> the request that started the erasure hangs. The interface gives up at 30 s
> and the CLI at 60 s, both saying "still running on the server". The server
> writes no answer after five minutes (spec 001 #15), and a stop of the server
> leaves the erasure half done with nothing to say so.
>
> This spec does two things. **The chunks follow the hours**: a chunk takes
> the user's traces in start order, whole hours at a time, up to a bound on
> the traces it deletes and on the rows its rolls recompute. Each hour is then
> rolled about once, still inside the transaction that emptied it (spec 023
> #19 unchanged). That turns ~10,000 transactions into ~100. The same rule
> serves bulk trace deletion (spec 035). **An erasure is a task**: the
> confirmed request answers `202` with an erasure resource, a single
> background worker runs it, and its progress, counts and outcome are
> recorded in a table. The table survives a restart, and the erasure resumes
> from its phase. The record forgets the user id when the erasure ends.

---

## Overview

Deliverables, two PRs:

- **PR 1 — chunks by hour order** (Decisions 1–5). Migration `0029_user_hour_order`
  rebuilds `idx_traces_user` as `(project_id, user_id, timestamp)`.
  `UserDataErase` selects in start order and takes whole hours until it
  reaches `Limit` traces or the roll budget. `TraceDelete` rounds (spec 035)
  group their newest-first rows the same way. The PR measures the 20k
  synthetic user of 044 #20 (l) again. No API change.
- **PR 2 — the erasure task** (Decisions 6–19). A migration
  adds the `erasures` table. The store gets the erasure worker, the confirmed
  `DELETE` returns `202`, and there are two `GET` routes, a `running` block
  in the dry run, the interface's progress, `users rm-data` that waits by
  polling, and docs.

PR 1 comes first: it is independent, it keeps every contract, and without it
an erasure task would still take tens of minutes and hold its worker slot
for as long.

Builds on spec 044 (the raw → parsed → tail order of #4, the tail of
#20 c, the patient submission of #20 h, "records nothing about the person"
of #15), spec 023 #19 (rolls in the chunk's own transaction), spec 013 #7
and #11 (the correction, the freeze), spec 035 #3, #14 (chunks and rounds of
bulk deletion), spec 043 (read slots, `WithoutCancel`, background contexts
#25 g) and spec 001 #15–#16 (the write deadline, the stop).

## What `main` does today

- `EraseUserData` (`internal/store/erasure.go:639`) runs the four steps of
  044 #4 inside the handler's request under `context.WithoutCancel`. Only the
  writer closing stops it.
- A parsed chunk (`UserDataErase.apply`, `internal/store/admin.go:733`)
  selects `id, timestamp, … FROM traces WHERE project_id = ? AND user_id = ?
  LIMIT 500` through `idx_traces_user (project_id, user_id)`, which returns
  rows in rowid (arrival) order. It keeps the traces of the first `HourLimit`
  (= 1) distinct hours it meets and skips the others, setting `More`. Then it
  deletes them and runs one whole-hour `statsRoll` per hour below the
  watermark, in the same transaction (023 #19).
- Bulk deletion (`handleDeleteTraces`, `internal/server/tracedelete.go:182`)
  reads a round of ≤ 1,000 matches newest first. It cuts them into chunks
  of one hour and ≤ 500 traces, at most 50 chunks a round (035 #14).
- The interface and `users rm-data` treat a timeout, a `502` and a `504`
  after the request was written as "still running" (044 #20 m).

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-28** — **An erasure chunk takes the user's traces in start order, whole hours at a time.** Migration `0029_user_hour_order` drops `idx_traces_user (project_id, user_id)` and creates `idx_traces_user (project_id, user_id, timestamp)` under the same name, as 0027 did for the tree. A chunk reads `… WHERE project_id = ? AND user_id = ? ORDER BY timestamp LIMIT Limit + 1` (ties in the index's order) and keeps hours from the first, in order, while the chunk stays within `Limit` traces (500) **and** the roll budget of #2. It ends at an hour boundary. The one exception is a first hour that alone holds more than `Limit`: it is cut at `Limit`, and the next chunk rolls that hour again. A trace with no start time (`timestamp` NULL) sorts first and is read as today, at hour 0. `HourLimit` goes | The hour bound was right about the transaction and wrong about the selection. Arrival order is start order only when a client exports in the order its traces started, and the 20k measurement is the case where it does not. In start order, a user's hours are contiguous, so each hour is rolled once (twice for a cut first hour) and a chunk is full. The old index is a prefix of the new one, so every query it served is served under the same name. A trace's `timestamp` can move earlier when a late span arrives. The chunk reads it inside its own transaction, as the hours are read today, so a moved trace is taken in the chunk that meets it, and the roll follows what is there. |
| 2 | **2026-09-28** — **A chunk's rolls are bounded by the rows they recompute, not by a count of hours.** Hours are added while what the chunk's rolls recompute stays within **`DeleteRollBudget`**. The count comes from `stats_hourly`: `SUM(count)` over all of the hour's rows (the traces, and the observations with a model), read in the chunk's transaction. Hours at or past the watermark are not rolled and cost nothing. A frozen hour (013 #11) is counted like any other, so the budget can overestimate a chunk but never underestimate it. A chunk always takes at least its first hour. `DeleteRollBudget` is a constant, set in PR 1 by measurement so that a chunk's transaction stays near what one aggregator job holds the writer for today (target: p99 ≤ 250 ms on the seeded stand under the gate's load). It is not a setting | A roll recomputes the project's whole hour, every user in it, so its cost follows the hour's density, not the erased user's share. One hour per chunk bounded that cost only by making every chunk pay it on its own. The rollup already knows how dense each hour it can roll is, so the budget reads that count instead of guessing. The target is the aggregator's because that is what ingest already waits behind (023 #19's "one is what the aggregator's own jobs already cost"). A fixed number shapes nothing a client sees, so it belongs to the code (spec 043 #22). |
| 3 | **2026-09-28** — **The rolls stay in the chunk's transaction** (spec 023 #19 unchanged, and spec 013 #7's "corrected with the deletion"). No dirty-hour table, no roll deferred to the end of the erasure | A roll committed with its deletion is one no hang-up, stop or crash can lose. Deferring it would reopen the window 023 #19 closed, and spec 013 #7 names what the window leaks: diffing the rollup before and after an erasure recovers how many traces the person had each hour. Once #1 makes each hour rolled about once, deferring saves nothing: the number of rolls is the same, only their place changes. |
| 4 | **2026-09-28** — **Bulk deletion groups its round the same way.** The round's rows already arrive newest first, so an hour's traces are contiguous. A chunk takes whole hours while it stays within 500 traces and `DeleteRollBudget`, instead of one hour per chunk. `deleteRoundChunks` (50) and `deleteRound` (1,000) stay | 035 #14 measured the cost of one hour per chunk: 300 traces over 118 hours took 9.2 s, 1.6 s of it in the bodies and the rest in 118 commits. Budgeted hours make that one to three chunks, about 2 s. The round bounds are the client-facing contract and do not move. |
| 5 | **2026-09-28** — **PR 1's target and measurement.** On the synthetic 20k user of 044 #20 (l) (40,000 spans, 401 batches, start times over 30 days, arrival order shuffled), at the same load: the parsed phase in **≤ 60 s** (19 min today), at most **~100 chunks** (10,664), and every chunk's transaction ≤ 250 ms at p99. Ingest latency during the erasure is recorded next to an aggregator pass for comparison. The numbers go into this log as a dated Decision | 023 #19 measured 738 in-transaction rolls for 20,985 traces at 11.1 s unloaded, so rolls at one per hour are cheap. What cost 19 minutes was 10,664 commits and 10,664 rolls. |
| 6 | **2026-09-28** — **A confirmed erasure is a task and answers `202 Accepted`.** `DELETE /api/v1/projects/{id}/users/{user_id}/data?confirm=<user id>` checks everything that can refuse it synchronously, as today: role and scope, the project live, the echo, `writes are not available`. Then it records an erasure in state `queued` and answers **`202`**, `Location: /api/v1/projects/{id}/erasures/{erasure_id}`, with the erasure resource as the body. A refusal is still a `4xx`/`503` with nothing recorded. The dry run (no `confirm`) stays synchronous and unchanged, except for #13 | The erasure's cost is the person's history, not a request's, so a request cannot hold it. Tens of seconds after PR 1, but a year of traffic or a loaded server is still minutes, and 044 #20 (m) already concedes that a caller cannot wait for it. A task makes that concession explicit: the answer says the erasure is accepted and where to watch it, instead of a timeout that means "probably running". Refusals stay synchronous so that `202` always means "it will run". |
| 7 | **2026-09-28** — **`?wait=<seconds>`, 0–30, default 0.** The request waits up to that long for the erasure to end. If it ended (`done` or `failed`), it answers **`200`** with the same resource. Otherwise `202`. The body's shape is the same in both | A small erasure, the common case after PR 1, can be answered in one round trip. The interface's dialog does that and never shows progress for a user of a few traces. Thirty seconds fits under the interface's 30 s clock with the margin it already keeps, and far under the write deadline. One shape for both codes keeps a client from having two parsers. |
| 8 | **2026-09-28** — **The erasure resource.** `{"id", "state", "phase", "user_id", "created_at", "started_at", "finished_at", "progress": {"traces_at_start", "traces_deleted"}, "deleted": {…}, "compaction": {…}, "pre_migration_backup": {…}, "error"}`. `state` is one of `queued`, `running`, `done`, `failed`. `phase` is `raw`, `parsed` or `tail` while running, and `null` otherwise. `deleted` carries today's keys and counts what has been committed so far. `compaction` and `pre_migration_backup` are today's blocks. `error` is a sentence, or `null`. The id is 16 random bytes in hex, never derived from the user id. The confirmed body of today (`dry_run: false`, `deleted`, `user_id`, `compaction`, `pre_migration_backup`) is a subset of this resource, and `dry_run: false` is kept in it | `traces_at_start` is step 1's count, so `traces_deleted / traces_at_start` is honest progress for the phase that takes the time. The raw phase reports its counts in `deleted` as batches land. A hash of a user id is a record of the person, since an id space is small enough to enumerate. Keeping today's keys means a client that reads `deleted` from a `200` keeps working. |
| 9 | **2026-09-28** — **The record forgets the person when the erasure ends.** The `erasures` row holds `user_id` only while `queued` or `running`. The transition to `done` or `failed` sets it to NULL in the same statement. From then on `user_id` is `null` in every answer except the one to the request that started the erasure, which knows the id anyway. The error sentence never contains the id. Log lines carry the erasure id and counts, never the user id (044 #15) | Spec 044 #15: "An erasure records nothing about the person." A running task has to know whom it erases, and while it runs the person's traces carry the id anyway. Once it ends, the row keeps what an operator needs (that an erasure ran, when, what it took) and nothing that names anyone. `secure_delete` (044 #10) overwrites the cleared bytes, and the erasure's own compaction request covers the index. |
| 10 | **2026-09-28** — **One erasure runs at a time per server; the rest queue, oldest first.** One worker, owned by the store beside the sweeper and the aggregator, takes the oldest `queued` erasure of any project. It runs the four steps of 044 #4 under the worker's context, not a request's. Its reads are background reads (043 #25 g): no read slot, no read deadline | An erasure is mostly writer work plus decoding on one core. Two at once share the writer and gain little, and they double what ingest queues behind. One worker also bounds the pool connections an erasure takes (043 #28 d's headroom). FIFO across projects is the simplest fair order, and with PR 1 the head of the queue clears in seconds to minutes. |
| 11 | **2026-09-28** — **A second request for the same user while one is `queued` or `running` answers that one.** Same project, same user id: no new row. The answer is `202` (or `200` under `wait` if it ended meanwhile) with the existing resource. After it ends, a new request creates a new erasure, which finds what arrived since or nothing | Repeating the request is what an operator does when unsure, and what 044 #20 (m) tells them to do. Two erasures of one person would race on the same batches (044 #4's optimistic check makes that safe, but slow) and would each count half. Matching on the user id is possible only while it is stored, which is exactly while the erasure is not over (#9). |
| 12 | **2026-09-28** — **An erasure survives a restart and resumes from its phase.** Progress is written in the transactions that make it. Every `RawScrub` job adds its counts to the erasure row, and every `UserDataErase` chunk adds its counts, its `traces_deleted` and the tail windows it collected (044 #20 c, merged) in its own transaction. The phase moves in the job that ends it. On start, after migrations and once the writer runs, the worker takes a `running` erasure before any `queued` one and resumes. Phase `raw` runs step 2 again from step 1's stored moment (batches already scrubbed hold no erased span and are left untouched). Phase `parsed` continues the chunks. Phase `tail` reads the stored windows. `attempts` counts starts, and a third interrupted start ends the erasure `failed` ("interrupted by three restarts") | 044 #4 made a cut-off erasure recoverable only by repeating it, and 044 #20 (m) names what a repeat cannot recover: the late batches of traces already deleted. Those windows are now stored with the deletion that produced them, so a resume can finish the tail that a repeat never could. Counts written with the jobs never double on resume. The attempt cap keeps an erasure that crashes the server from doing so on every start. |
| 13 | **2026-09-28** — **The dry run says when an erasure of that user is under way.** The preview gains `running: {"id", "state", "phase"}` when one is `queued` or `running` for the same user, and omits the key otherwise | The dry run is what the interface opens and what `users rm-data` prints first. A person looking up a user whose erasure is running should see that before the counts, which are shrinking under them. |
| 14 | **2026-09-28** — **`GET /api/v1/projects/{id}/erasures/{erasure_id}` and `GET /api/v1/projects/{id}/erasures`.** Role `editor`, scope `write` (spec 045), the same as the `DELETE`. The listing returns the project's erasures newest first, at most 100, not paginated. An unknown or expired id is `404` | The status belongs to whoever may start an erasure. A read-only key has no business watching erasures, and the listing names the person while one runs (#9). A hundred covers 30 days of any plausible erasure traffic. The listing exists so the Settings card can show what is running after a reload, without the user id in hand. |
| 15 | **2026-09-28** — **Finished erasures are kept 30 days, then removed by the sweeper.** Measured from `finished_at`. `queued` and `running` erasures are never removed. A project's purge takes its erasures (FK cascade) | Long enough for "did the erasure from last week finish?" and short enough not to become a log of the project's erasures. With the user id gone the row says little: a controller who needs proof keeps the `200` or `GET` answer themselves. |
| 16 | **2026-09-28** — **Failure.** A job that fails with a database condition (043 #2) is retried with backoff, as `submitPatiently` already waits out a full queue (044 #20 h), for up to two minutes. Any other failure ends the erasure `failed` with a sentence in `error`, after step 4 has run for the chunks that committed (044 #20 m, kept). The operator repeats the request, and the dry run shows what is left | A full disk or a busy database is a condition that passes. Nobody watches a background task to retry it by hand, so the task retries itself, within a bound. Any other failure is a bug, and retrying a bug hides it. |
| 17 | **2026-09-28** — **A stop interrupts the worker; it does not wait for it.** The stop cancels the worker's context when it begins (spec 001 #16). The job in flight commits or does not, the erasure stays `running`, and #12 resumes it on the next start. The worker returns before the writer closes | A stop cannot wait tens of seconds for an erasure without overrunning 001 #16's ten-second budget. Because progress commits with the jobs, an interrupted erasure loses nothing a resume cannot redo. |
| 18 | **2026-09-28** — **The clients.** The interface's dialog sends `wait=20`. A `200` is today's sentence (`erased(...)`). A `202` turns the dialog into a progress line (phase, `traces_deleted / traces_at_start`) polled every 2 s while it is open. Closing the dialog leaves the erasure running, and the user page and the Settings erase card show a running erasure from #13 and #14. `users rm-data` sends `wait=30`, then polls `GET` every 2 s and prints the phase and progress to stderr until the erasure ends. `--no-wait` prints the id and exits. `tracepad users erasure <id>` prints one erasure. The "no answer means still running" wording of 044 #20 (m) goes away: the answer now says so. No MCP tool (every MCP tool is a read, spec 004 #20) and no SDK method exists for erasure, and none is added | The dialog waiting 20 s covers the common case with no new screen. Polling is the one mechanism both clients already have. The CLI waits by default because an operator at a terminal asked for the erasure to happen, not for a ticket. |
| 19 | **2026-09-28** — **The contract changes now, without a synchronous mode.** Before the beta the API may change (REPOS.md), and the interface and the CLI are updated in the same PR. A client that expected `200` gets `202` and the resource, whose `deleted` it can read after polling. `wait` stands in for the synchronous answer where it fits | A `?sync=true` would keep a request that holds a connection for the erasure's length, the thing this spec removes, for a client that does not exist yet. |
| 20 | **2026-09-28** (found in implementing PR 1) — **The session queries say which index they mean.** With no `ANALYZE`, SQLite breaks a tie between two indexes that answer the same equalities by the order they were created in. `idx_traces_user` and `idx_traces_session` both answer the correlated subquery of `sessionStartCondition` (`x.session_id = t.session_id AND x.user_id = t.user_id`), and `dirtySessionHoursQuery` could walk `idx_traces_user` as a range on `user_id` instead of seeking `idx_traces_session` once per changed session. Migration 0029 recreates the user index after the session index, which flipped both choices to the user index. Both now write the user-id terms with a unary `+` (`+x.user_id = t.user_id`, `+other.user_id IS NOT NULL AND +other.user_id != ''`), so the plan no longer depends on the order the indexes were created in; `TestSessionStartSeeksTheSessionIndex` holds it. The queries that filter one user by time (the user page's live tail and session starts, `?user_id=` statistics and the capped trace count) now seek `idx_traces_user` on `(user_id, timestamp)` instead of walking the project's hour range, which is the index working as intended | The dirty-session query runs on every aggregator pass. Walking every trace with a user there is a scan of most of the table, five minutes apart, the shape spec 023's review of PR #42 removed once already. A plan that held only because of the order two migrations ran in is not a plan anyone chose. |
| 21 | **2026-09-28** (measured in PR 1) — **What PR 1 measured, and the budget it set.** The synthetic user of 044 #20 (l) again, in a fresh store: 400 batches, each holding 50 of the user's traces and 50 of 200 other users', two spans a trace, start times spread over 30 days and shuffled within each batch, every closed hour rolled. On a laptop at a load average of 4–38, shared with other work: erasing the user's **20,000 traces took 19.4 s end to end**, 400 raw batches read, **42 chunks** (10,664 before), 719 chunk-hours for the 719 hours the user touched, so every hour was rolled once. A chunk's transaction took 235 ms at p50, 287 ms at p99 and 415 ms at most, measured inside the writer. That is above #5's 250 ms at p99: one whole-hour roll cost 6.5 ms on average here, 38.9 µs per unit the budget counts, so the rolls are about 120 ms of a chunk and deleting its ~480 traces with their observations is the rest, a cost the 500-trace bound has always carried and this spec does not change. **`DeleteRollBudget` is 5,000**, about 200 ms of rolls at the measured rate. It does not bind on this corpus, whose hours are light, and it is what keeps a chunk of dense hours from adding seconds. #5's 60 s and ~100 chunks hold. Migration 0029 on a synthetic file of 1,000,000 traces (801 MB) took 12.4 s, 0.8 s of it CPU and the rest disk, at a load average of 36; like every migration it runs once, before the server listens, and is logged (043 #24 m) | The 19 minutes of 044 #20 (l) were 10,664 commits and 10,664 rolls. The same work in start order is 42 of the one and 719 of the other. The budget comes from the measured rate, not a guess, and a first hour is always taken whatever it costs. A dense hour on its own costs a chunk what it cost before. |

## API contract

**`DELETE /api/v1/projects/{id}/users/{user_id}/data`** (editor, scope
`write`)

- No `confirm`: the dry run, unchanged, plus `running` (#13):

```json
{
  "dry_run": true,
  "would_delete": {"traces": 12, "…": 0},
  "running": {"id": "4f0c…", "state": "running", "phase": "parsed"},
  "confirm": "user-4711",
  "note": "…"
}
```

- `confirm=<user id>`, optional `wait=<0..30>`: `202 Accepted` with
  `Location`, or `200 OK` when the erasure ended within `wait` (#6, #7).
  `wait` out of range or not an integer is `400`.

```json
{
  "id": "4f0c9d3e8a1b2c3d4e5f60718293a4b5",
  "state": "running",
  "phase": "parsed",
  "user_id": "user-4711",
  "dry_run": false,
  "created_at": "2026-10-02T09:00:00Z",
  "started_at": "2026-10-02T09:00:00Z",
  "finished_at": null,
  "progress": {"traces_at_start": 20000, "traces_deleted": 5123},
  "deleted": {
    "traces": 5123, "observations": 10246, "scores": 0, "session_scores": 0,
    "payloads": 20492, "annotation_items": 0, "dataset_items": 0,
    "media": 0, "media_bytes": 0,
    "raw_spans": 40000, "raw_batches_rewritten": 401, "raw_batches_deleted": 0
  },
  "compaction": {"requested_at": "…", "expected_by": "…"},
  "pre_migration_backup": {"created_at": "…", "remove_after": "…"},
  "error": null
}
```

**`GET /api/v1/projects/{id}/erasures/{erasure_id}`** (editor, `write`):
the resource. After the end `user_id` is `null` (#9). `404` for an unknown
or expired id, or one of another project.

**`GET /api/v1/projects/{id}/erasures`** (editor, `write`):
`{"erasures": [ … ]}`, newest first, at most 100 (#14).

`openapi.json` and `schema.d.ts` carry all three. The parity tests, the
scope matrix (spec 045) and the six-caller matrix cover the new routes.

## CLI contract

- `tracepad users rm-data <user> --confirm <user>` sends `wait=30`. On
  `202` it polls and prints `erasing: parsed 5123/20000 traces` to stderr,
  one line per change. On the end it prints today's lines (counts, raw
  line, compaction line, backup line). A `failed` erasure exits 1 with the
  error sentence.
- `--no-wait`: prints `erasure 4f0c… queued; tracepad users erasure 4f0c…`
  and exits 0.
- `tracepad users erasure <id>` prints one erasure. `tracepad users erasures`
  lists them.
- The preview prints `an erasure of this user is running: 4f0c… (parsed)`
  when `running` is present.
- `confirmErasure`'s "no answer" path (044 #20 m) goes, except for a
  connection lost before the answer. There `users rm-data` says to run
  `tracepad users erasures` to see whether it was accepted.

## Interface

- The erase dialog (Settings card and the user page) confirms with
  `wait=20`. On `200` it shows today's sentence and, on the user page, leaves
  as today. On `202` it shows a progress line polled every 2 s. Closing the
  dialog does not cancel anything, and the page stays where it is, as it
  does now for a running erasure (`Erasure.running`).
- The user page shows `Erasure in progress — parsed, 5,123 of 20,000
  traces` from the dry run's `running` (#13). The Settings erase card lists
  running and recent erasures from the listing (#14), with ids, states and
  counts, and no user ids for finished ones.
- `stillRunning` in `ui/src/lib/erasure.ts` goes; a timeout of the `202`
  request itself is shown as a failure to reach the server, since the answer
  no longer depends on the erasure's length.
- Live check in a browser, per the Definition of Done: a 5,000-trace user
  shows progress and ends. A reload during the erasure shows it as running.

## Store contract

- `UserDataErase`: `HourLimit` is replaced by the budget of #2 (`RollBudget`
  on the job, zero meaning the `DeleteRollBudget` constant), and `HourChunks`
  is the one rule that cuts a run of traces ordered by hour, for both
  callers. In PR 2 it gains `ErasureID`: when
  set, `apply` adds its counts, `traces_deleted` and tail windows to the
  `erasures` row in the same transaction (#12).
- `RawScrub`: gains `ErasureID`, and adds its tally to the row the same way.
- `EraseUserData` becomes `(*Store).runErasure(ctx, writer, id)`, run by the
  worker. `StartErasure(ctx, writer, UserErasure) (Erasure, error)` inserts
  or finds (#11) and wakes the worker. `Erasure(ctx, projectID, id)` and
  `Erasures(ctx, projectID)` read.
- `TraceDelete` is unchanged. The server's round builder groups hours by the
  budget (#4), reading the hour counts through one new store call
  (`RollCosts(ctx, projectID, hours)`) in the round's read slot.
- The sweeper removes finished erasures past 30 days (#15), as its last
  job of a pass.

## Data contract

`0029_user_hour_order` (PR 1):

```sql
DROP INDEX idx_traces_user;
CREATE INDEX idx_traces_user ON traces(project_id, user_id, timestamp);
```

The `erasures` migration (PR 2, numbered when it lands):

```sql
CREATE TABLE erasures (
    id              TEXT NOT NULL PRIMARY KEY,           -- 32 hex, random
    project_id      TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id         TEXT,                                -- NULL once finished (#9)
    state           TEXT NOT NULL,                       -- queued|running|done|failed
    phase           TEXT,                                -- raw|parsed|tail while running
    created_at      INTEGER NOT NULL,
    started_at      INTEGER,
    finished_at     INTEGER,
    since           INTEGER,                             -- step 1's moment (044 #20 c)
    now             INTEGER NOT NULL,                    -- the freeze clock, read once (013 #11)
    attempts        INTEGER NOT NULL DEFAULT 0,
    traces_at_start INTEGER,
    counts          TEXT NOT NULL DEFAULT '{}',          -- the `deleted` keys
    tail_windows    TEXT NOT NULL DEFAULT '[]',          -- merged [from, to] pairs
    compaction      INTEGER NOT NULL DEFAULT 0,
    error           TEXT
) STRICT;
CREATE INDEX idx_erasures_project ON erasures(project_id, created_at DESC);
CREATE UNIQUE INDEX idx_erasures_running ON erasures(project_id, user_id)
    WHERE user_id IS NOT NULL;                           -- #11, enforced
CREATE INDEX idx_erasures_state ON erasures(state, created_at)
    WHERE state IN ('queued', 'running');
```

Measured in PR 1 (#21): 12.4 s for the index rebuild on a synthetic file
of 1,000,000 traces, logged like every migration (043 #24 m).

## Docs

- `docs/retention.md`, the data-subject section: an erasure is accepted,
  then runs; how to watch it; what a restart does; that the record forgets
  the id when it ends and is itself gone after 30 days.
- `docs/api.md`: the three routes, `wait`, the `202`/`200` rule, the
  resource. The erasure paragraph under "a response has five minutes" goes.
- `docs/cli.md`: `rm-data` waiting, `--no-wait`, `users erasure(s)`.
- `docs/admin.md`: the bulk deletion paragraph on chunks (#4).

## Testing

PR 1:
- EXPLAIN: the chunk's selection seeks `idx_traces_user (project_id=? AND
  user_id=?)` in index order with no temp B-tree. The density read seeks
  `stats_hourly`'s key.
- A user whose traces arrived in shuffled start order across 50 hours, each
  hour rolled: the erasure takes `⌈traces / 500⌉ + (cut hours)` chunks, not
  one per hour. Every hour is rolled once (a roll counter seam). A client
  hang-up after chunk k leaves no hour counting an erased trace (023 #19's
  test, kept green).
- The budget: with `RollBudget` set low on the job, a chunk of dense hours
  takes one hour, and one of sparse hours takes several. The first hour is
  always taken.
- A trace whose start moves earlier between chunks is still erased, and its
  new hour rolled.
- Bulk deletion: 300 traces over 118 hours delete in ≤ 3 chunks, and the
  round's `more` is unchanged.
- Mutation: back to arrival order, the chunk-count test fails.

PR 2:
- `202` with `Location`. `wait` ending within the window gives `200` with
  the same shape. `wait=31` or `wait=x` is `400`.
- Refusals record nothing: a wrong echo, a soft-deleted project, a viewer,
  a `read` key.
- #11: two confirmed requests for one user give one row and the same id,
  and a third after `done` gives a new one.
- #9: after `done` and after `failed`, `SELECT user_id FROM erasures` is
  NULL and no `GET` answer carries the id. After a compaction the id is in
  no page of the file (044's file scan, reused).
- #12: the worker is killed (the writer closed) in each phase. On restart
  the erasure resumes from that phase, ends `done` with counts equal to an
  uninterrupted run's, and the late batch of a trace deleted before the
  stop is scrubbed. Three interrupted starts give `failed`.
- #16: a job failing with a condition is retried, and one failing
  otherwise gives `failed` after the tail.
- #15: a finished erasure 31 days old goes with the sweeper's pass, and a
  running one of any age stays.
- The stop: 001 #16's tests with an erasure running. The stop stays within
  its budget and the erasure is `running` on the next start.
- CLI: `rm-data` polls to the end and exits 0. `--no-wait` prints the id.
  A `failed` erasure exits 1.
- e2e: the dialog with a small user answers within `wait`. A user of
  thousands shows progress, survives a reload, and ends.

## Edge cases

- **The same user in two projects**: two erasures, since #11 matches per
  project. The worker runs them one after the other.
- **The project is soft-deleted while an erasure is queued**: it runs; the
  data is there until the purge. A purge while it runs cascades the row
  away, and the worker's next job finds no project and ends quietly.
- **`TRACEPAD_STORE_RAW=off`**: as 044 #20 (b), the raw phase runs on what
  the archive holds.
- **An erasure of a user with no traces**: it ends `done` with zeros on its
  first job, and `wait` gives `200` at once.
- **A restart during `queued`**: the erasure stays queued and runs.
- **A clock that moves back across a restart**: `now` and `since` are the
  stored ones, so the freeze and the tail keep the first start's view.
- **The erasures listing names a running user**: to editors of the
  project, who can read that user's traces anyway.

## Out of scope

- Asynchronous bulk trace deletion. Rounds with `more` already bound it,
  and nothing about it grows with a person's history.
- Cancelling an erasure. Stopping half way leaves the raw scrub done and
  the parsed rows partly there, which is a state no one asked for. A
  `DELETE` of the erasure resource can come later if anyone needs it.
- Parallel erasures, and priorities between projects.
- Notifications (webhook, email) when an erasure ends.

## Amendments to shipped specs

| Spec | PR | Amendment |
|------|----|-----------|
| 023 | 1 | #19's "a chunk is at most 500 traces of one hour" becomes "at most 500 traces, whole hours in start order, within the roll budget" (spec 047 #1–#2); the rolls stay in the chunk's transaction. |
| 035 | 1 | #3's chunk of one hour becomes whole hours within the budget (spec 047 #4); #14's round bounds stand. |
| 035 | 2 | #14's "erasure keeps its own contract — to completion in one request" is superseded: an erasure is a task (spec 047 #6). |
| 013 | 2 | #7's "in the same request, before it answers" becomes "in the chunk's transaction, before the erasure reports done". |
| 044 | 2 | #4's "one request" and #20 (m) become the task of spec 047 #6–#12; #15 gains the record that forgets the id when it ends (#9). |
| 045 | 2 | The two new routes are `write` routes. |
