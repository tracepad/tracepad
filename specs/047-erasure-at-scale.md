# Spec 047 — Erasure at scale: chunks that follow the hours, and an erasure that answers at once

**Status:** ✅ SHIPPED
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
| 22 | **2026-09-28** (found in the first review of PR #131, measured again) — **What the budget counts, and when.** Amends #2 and #21's budget. (a) A chunk's cost is the rolls of its hours plus the deletion of its own traces. An hour's roll costs `25 × traces + observations` over the hour's range of `idx_traces_timestamp`, counted from the rows, not from `stats_hourly`. Deleting a trace costs `25 + 3 × its observations`. (b) An hour's cost is asked only of the hours a chunk considers, once each, and the watermark is read once a chunk. (c) The budget counts the whole chunk, its first hour included; the first hour is taken whatever it costs. A budget of zero or less, on the job or on the server, is `DeleteRollBudget`, now **30,000**. (d) A chunk rolls each hour once, even when a trace with no start time (hour 0) and one that started before the epoch interleave in the start order. (e) Not counted: the per-user summaries the chunk's rolls touch are recomputed once a chunk (spec 023 #3), over every user of its hours. Their number is at most the traces of those hours, which the budget caps: 1,200 users at the most, some 25–70 ms at spec 023's 20–60 ms per thousand, in the worst case of one trace per user. Before, a dense hour's users were bounded by nothing. (f) Measured at a load average of 2–16, with the same corpus as #21 and with an agent profile of 5,000 of the user's traces and 5,000 others', 200 spans a trace and one model call: the first took 21.7 s in 59 chunks, p50 166 ms, p99 218 ms, at most 332 ms; the agent profile took 40.3 s in 207 chunks, p50 110 ms, p99 239 ms, at most 535 ms. Every hour was rolled once in both. A unit cost about 4 µs on both corpora: a trace about 94 µs of a roll, an observation 4, and deleting one of the chunk's own observations about 13. Weighed by traces alone, one unit was 34 µs on the first corpus and 4.5 on the second | (a) A roll reads every observation of the hour's traces through the join and filters by model only afterwards. `stats_hourly` counts the traces and the observations with a model, so an agent run of 200 spans and one model call looked 50 times cheaper than it was. An hour the aggregator has not rolled since its traces arrived (an import of history, late spans) has no rows there, or old ones, and it looked free while the roll read it whole. Unweighted, a budget that suited one corpus rolled the other an hour a chunk: 591 chunks for 5,000 agent traces, most of the time in commit windows. Without the deletion term, the agent profile's p99 was 496 ms, and the rolls were a third of it. (b) A chunk inside the writer's transaction read the counts of every hour of its 501 rows to use a few. (c) The comments had said "past its first hour", and the code and #2 both meant the whole chunk. Two meanings for zero was one refactor away from a server that rolls one hour a chunk again. (d) Rolling one hour twice in a transaction is harmless but paid twice. (e) Counting users would take a read per chunk for a number the budget already caps. (f) The first review asked for the p99 on an agent's traffic, not only on the light traces #21 measured. |
| 23 | **2026-09-28** (found in the second review of PR #131) — **A chunk prices the hours it considers, and a round the chunks it runs.** Amends #22 (b). `HourChunks` stops after as many chunks as its caller will run: an erasure job one, a bulk round `deleteRoundChunks`. #22 (b) held within a chunk only. The cut still ran over the whole read (501 rows for an erasure, 1,000 for a round), and after #22 (a) every hour it considered cost a scan of that hour's traces. So a user whose traces are spread thin over hundreds of hours made each chunk price hundreds of hours inside the writer's transaction, and a round could run the read slot past its deadline pricing hours it would never delete. A first hour cut at the limit is cut before it is priced. #1's "twice for a cut first hour" is corrected: an hour holding n > `Limit` of the user's traces is rolled by each of the ⌈n / `Limit`⌉ chunks it spans. Not changed: pricing an hour reads its trace rows (`observation_count` is not in `idx_traces_timestamp`), and the roll that follows reads the same rows and every observation under them. A trace costs a roll about 94 µs and a row lookup a few, and #22 (f)'s figures were measured with the pricing inside the transaction. Measured again, twice each, at a load average of 3–6, with a third profile: a user of 2,000 traces spread over 673 of the 720 hours, among 20,000 others'. That user took 7.9 s in 22 chunks, p50 135 ms, p99 158 ms. The first corpus took 22.0 s and 21.8 s in 59 chunks, p50 176 and 172 ms, p99 334 and 224 ms. The agent profile took 41.7 s and 39.9 s in 210 and 208 chunks, p50 112 ms both times, p99 299 and 149 ms. The medians hold from run to run and the tail does not: with 59 chunks the p99 is the second-slowest chunk, and one run of each dense profile put it over 250 ms. | The first review's thread on this was answered as fixed, and it was fixed only inside a chunk; the measured corpora never had a chunk consider more than about twenty hours. A covering index for the price would be one more index on `traces` to save a small share of a chunk. |
| 24 | **2026-09-28** (found in the third review of PR #131) — **A price is capped, read where the rolls will read the watermark, and not asked of an hour that cannot fit.** Amends #22 (a), (e) and #23. (a) An hour's count stops once it has read enough traces to outweigh the budget on their own, `budget / 25 + 1` of them. Every row of the hour's range of `idx_traces_timestamp` matches, so the `LIMIT` bounds the scan and not only the answer. An hour that reaches the cap is past the budget, which is all the cut needs to know. (b) An hour at or past the watermark costs nothing only when it is priced inside the transaction that rolls, as an erasure chunk's are. A bulk round prices before its jobs run, and the aggregator may move the watermark past an hour before the chunk that holds it commits, so a round prices every hour. (c) Once a chunk's cost has reached the budget, the next hour is not priced: it cannot fit. (d) Corrects #22 (e): the users whose summaries a chunk recomputes are bounded by the budget past the first hour only. The first hour is taken whatever it costs, and its users are bounded by nothing, as before this spec. (e) `UserErasure` no longer carries a budget, since no caller set one; the job's `RollBudget` stays as the tests' seam. (f) Measured once more, at a load average of 2–5. The sparse user took 8.0 s, p50 135 ms, p99 168 ms. The first corpus took 21.6 s, p50 164 ms, p99 221 ms. The agent profile took 40.9 s, p50 113 ms, p99 406 ms. Over four runs of the agent profile its p99 was 239, 299, 149 and 406 ms, while its p50 stayed at 110–113 ms. The time is taken inside the job's `apply`, before the commit, so the tail is not the commit's checkpoint; what it is was not found | (a) A round priced up to about 51 hours in its read slot, and on a project of tens of thousands of traces an hour each price was that many row lookups, so a round that worked before could run out its read deadline and answer 503 before deleting anything. Inside an erasure chunk a dense first hour was scanned whole to learn a number past the budget. (b) Priced ahead at zero, an hour that became rollable before its chunk ran was rolled with nothing counting it, and a chunk of 500 traces over many such hours was bounded by nothing, which one hour a chunk had been. (c) A dense first hour made each job price, and then drop, the hour after it, and the next job priced it again. (d) The spec is the oracle, and it claimed a bound that the first hour escapes. |
| 25 | **2026-09-28** (found in the fourth review of PR #131) — **What the wider index costs ingest, and the edges of the cut.** (a) Migration 0029's `idx_traces_user (project_id, user_id, timestamp)` is maintained by the `UPDATE` that sets a trace's `timestamp` on every export that touches it, anonymous traces included, where the old index was touched only when `user_id` changed. Measured by ingesting 40,000 traces, each in two exports of one span so the second updates a trace that is there, a quarter of them anonymous, at a load average of 3–4: 41.0 s and 40.1 s with the old index, 41.9 s and 42.1 s with the new, **3.4 %** more. (b) A trace with no start time reads as hour 0, as it did before this spec: its chunk prices and rolls an empty hour, a range with no rows, once. (c) `HourChunks` answers an error, not a panic, when the deletion costs it is given do not match the traces. (d) A round's pricing does not read the watermark it does not obey (#24 b). (e) Specs 023 #20 and 035 #19 say the budget counts the deletion of the chunk's own traces as well as its rolls | (a) The same `UPDATE` already maintains `idx_traces_timestamp` on that column and runs five subqueries over the trace's observations, so one more entry is a small share of it, and the erasure it pays for went from 19 minutes to 22 seconds. (b) Pricing and rolling an empty hour cost next to nothing, and the chunk's hours are a set. (c) A mismatch is a programmer's mistake, and inside the writer's `apply` a panic would take the transaction down with it. (e) #22 (a) added the deletion term, and the amended specs are read on their own. |
| 26 | **2026-09-28** (found in implementing PR 2) — **How PR 2 carried out #6–#19.** (a) **The tail is rows, not a column.** The chunks store their tail in `erasure_tail (erasure_id, trace_id, arrived_from, arrived_to)`, one row per trace, instead of `tail_windows` on the erasure's row: the tail's scrub picks the erased spans out of a batch by their trace ids, and windows alone do not carry them. A trace a late span brought back and a later chunk deleted again keeps one row whose window covers both. The rows go in the transaction that ends the erasure, with the user id (#9). The data contract below is amended. (b) **A resume into the parsed phase does not know step 1's traces.** Their list is not stored, so after a resume a chunk reads the whole arrival window of every trace that changed since step 1, as it does for a trace step 1 did not know. The tail reads more batches than an uninterrupted run would, and scrubs the same spans. (c) **Two small jobs of the erasure's own.** Step 1's count is written by a job after step 1 (a resume keeps the first), and the move from `raw` to `parsed` by a job after step 2, because the raw phase can end without a job when no batch held a span. The move to `tail` is in the last chunk's transaction. (d) **#16 retries jobs.** A job failing with a database condition is retried with backoff for up to two minutes; a read outside the writer that fails ends the erasure failed, like any failure that is not a condition. (e) **A stop answers the waiting requests.** The worker's stop is the first thing a stop does (#17), and a request holding its answer under `?wait` is answered `202` at once when the worker stops, rather than holding the drain for the rest of its wait. (f) `Location` is sent with the `200` as well. The two `GET` routes, like the `DELETE`, reach a live project only. The chunk size moved from the request (`UserErasure.Chunk`) to the worker's options, since the worker is what runs the chunks. (g) **The error sentence never names the user**: an error that quotes the user id — a refusal quotes the echo — has it replaced by "the user" before it is stored. (h) The user page asks an editor's dry run on the way in, to find `running` (#13) and follow it; a viewer is offered no erasure and asks nothing. | (a) A tail that could name windows but not traces would scrub nothing, or would have to guess whose spans a batch holds. (b) Storing step 1's list would keep up to every trace id of the person on the row for the length of the erasure, to save reads of batches the erasure already cleaned. (c) The phase has to move when its work is whole, and a phase that did no work has no job to move it in. (d) A condition is a property of the writer's transaction, which a retry repeats whole; a read is repeated by the resume. (e) Thirty seconds of a stop's ten would be spent waiting for an answer the stop is about to make unchangeable. (g) #9 applies to what the row keeps, whatever the error says. |
| 27 | **2026-09-28** (found in the first review of PR #134) — **The edges of the task.** (a) **A clean stop gives its start back.** A run the worker's stop ends submits one small job, bounded to a second, that takes back the start `erasureBegin` counted, so `attempts` counts the starts a crash cut off. Amends #12's "`attempts` counts starts". (b) **The last start does only the tail.** After three of those, the next start records "interrupted by 3 restarts", moves to `tail` and scrubs what the committed chunks stored, then ends `failed`; a tail that is cut off too is dropped at the start after it, with an error line giving the erasure's id and how many windows were dropped. Amends #12's "a third interrupted start ends the erasure `failed`". (c) **An error that names the user is not kept.** When the error's text holds the user id, as it is or escaped as `%q` escapes it, the stored sentence is only the phase that failed; #26 (g)'s replacement missed the escaped form and, for a short id, took unrelated words with it. (d) **The freeze clock goes forward with the chunks.** A chunk freezes by the later of the stored `now` and the wall clock: an erasure that waited in the queue or across a stop judges a swept hour by now, and a clock that went back still keeps the first start's view (Edge cases). Amends #12. (e) **No answer is not a refusal.** The interface's confirmed request that times out, or that a proxy answers 502 or 504, says the server may have accepted the erasure and where to see whether it did, and reads the listing again; the CLI already did. Amends the Interface's "a timeout of the `202` request itself is shown as a failure to reach the server". (f) **The user page finds a running erasure in the listing**, not in a dry run: the listing names a user only while that user's erasure runs (#9), so one indexed read answers it, where the dry run counted every store of the user on each visit. Amends the Interface's "from the dry run's `running` (#13)"; the dry run's `running` stays for the dialog and the CLI. (g) An erasure that is gone (404) is no longer followed or shown as running; `users rm-data` reads a running erasure again after a read that failed without an answer or with a 5xx. (h) **`UI_BUDGET` rises from 22,200 to 22,700.** `main` measured 22,234 when PR #134 was opened, already 34 over; this PR measured 268 lines after its first review (the erase card's progress line and listing, the user page's banner, the erasure module), and one more review cycle needs room — the raise rule of spec 045 #17. The owner approved the raise in advance. | (a) #12's cap is there for an erasure that brings the server down, not for one that outlasts a deploy. (b) Giving up by dropping the tail would leave the person's late spans in the archive with the user id already forgotten, the one state no repeat can repair. (c) #9 governs what the row keeps whatever an error says, and a sentence that loses its detail loses less than one that names the person. (d) The freeze (spec 013 #11) protects an hour past the window from being recomputed from what the sweep left, and only the present can say which hours are past it. (e) The server records the erasure before it answers, so a lost answer says nothing about whether it ran. (f) A courtesy banner should not cost a full preview on every visit. |
| 28 | **2026-09-28** (found in the second review of PR #134) — **Failures that leave the erasure to the next start, and the listing's order.** (a) **A tail that fails does not end the erasure.** A read or a scrub of step 4 that fails — a busy or failing database outside the writer's retry, a batch that stays in conflict — leaves the erasure running in its tail, its windows kept, for the next start; before, it ended `failed`, and ending drops the windows. The starts it takes count, and #27 (b)'s last start still gives up. A failure of a chunk (#16) still ends the erasure `failed` after its tail. (b) **A run that failed waits.** After a run that stopped without ending the erasure for any reason but the worker's stop, the worker waits its full poll interval, a minute, before it looks for work again, and a new request does not wake it: the failed erasure is taken first at every look, and at every wake it would spend a start on the same failure and hold every queued erasure behind it. The cap's sentence says what it counts: "3 starts ended before the erasure did", not "interrupted by 3 restarts" (amends #27 b). (c) **The pause is registered before the start.** A stop can end the wait for `erasureBegin` while the writer still commits it; the writer's queue is in order, so a pause submitted after lands after it, and it does nothing to an erasure that is not running (amends #27 a). (d) **The listing puts erasures under way first**: queued and running, then the rest newest first, still at most 100 (amends #14's "newest first"). The user page (#27 f) and the Settings card look for a running erasure there, and one waiting behind a hundred newer records was cut off. (e) An erasure that ends failed is logged by the state it ended in, not by the run's own view; the listing reads the pre-migration backup once, not once a row; the Settings card does not read the listing while it follows its own erasure; the user page forgets the erasure it followed when it is reused for another user. | (a) The windows name batches that hold spans of traces already gone, and ending forgets the user; a failure that may pass must not be the one that makes the tail unrecoverable. (b) A queue is fair only if a failing head does not take every turn. (d) A cut that hides what is running defeats the listing's one purpose on those screens; ordering costs nothing at this size. |
| 29 | **2026-09-28** (found in the third review of PR #134) — **What a failed tail said, the start a pause takes back, and one bound for retries.** (a) **An erasure that gives up says what its tail failed with.** A tail that fails records its sentence (never naming the user, #27 c) in `last_failure`, which no answer shows; the last start's sentence becomes "3 starts ended before the erasure did; its tail last failed with: …" when one did. It does not go in `error`, which a later tail that succeeds would end the erasure `failed` by. (b) **A pause takes back its own start.** `erasureBegin` writes a random token of its run in `run`, and the pause of a clean stop matches it: a stop that ends the wait for room in a full queue, before the start was handed to the writer, ended a run that counted nothing, and the count on the row is the crashes before it (amends #28 c). (c) **A worker that could not read the next erasure started none**: it logs that, and a request still wakes it; only a run that failed waits out its poll (#28 b). (d) **One bound for an erasure job's retries.** A full queue and a condition of the database are retried in one loop within #16's two minutes; before, a loop for the queue inside a loop for the conditions allowed up to twice that. (e) **The end's log line gives each phase's time again** — `raw_took`, `parsed_took`, `tail_took` — for the phases this run went through, beside `took`; a phase an earlier start finished is left out. (f) **The screens forget another project's erasure and keep their own.** The Settings card forgets what it followed when its project changes; it and the user page follow the project's id and role as values, so reading the account's projects again does not drop the banner. (g) **The user page's lost answer says where to look**: the page shows the erasure while it runs, and an erasure that already ended names no one (#9), so the page reads the user again and shows the data gone. Not changed: the listing's order stays an expression sort over the project's erasures of 30 days (#28 d); counting crashes by a process token was weighed and not taken. | (a) The give-up sentence is what an operator reads, and the log line with the cause is long gone by then. (b) #28 (c) closed the stop after the start was queued and opened the stop before it was. (c) A read error is not a run that failed, and waiting a minute on it turned requests with `?wait` into `202`s. (d) #16 promised two minutes. (e) The phase timings are how #5 and #21–#24 were measured, and PR 1 logged them. (f) Each screen's effect forgot the erasure whenever it ran again, and it ran again whenever the account's projects were read. (g) "This page shows it if it did" was not true of an erasure that ended within the proxy's wait. A process token for counting crashes still needs a write at a clean stop to tell a deploy from a crash, so it keeps the compensating write it was meant to remove; the per-run token of (b) closes the edge. A project's erasures in 30 days are each a confirmed request, and sorting them costs less than a second query. |
| 30 | **2026-09-28** (found in the fourth review of PR #134) — **A failed erasure goes after the others, an older server is asked without `wait`, and the screen keeps up with the end.** (a) **The give-up says what the last tail said.** The start that runs out of starts writes #27 (b)'s sentence before its own tail runs; when that tail fails too, the give-up at the next start writes the sentence again with it, rather than keeping the one without it. A chunk's failure (#16) stays what the erasure ends with. Amends #29 (a). (b) **An erasure whose run failed goes after every other.** The worker keeps, for as long as it runs, the erasures whose run failed and when each may be taken again, a poll interval later. It takes every other erasure, running or queued, before them, at once and on a request's wake; a failed one is taken again when it is due, or when nothing else waits. Amends #28 (b), after which the worker took the same failed erasure first again once its minute was out, so one erasure that kept failing held every other for five starts. (c) **The worker's look for work names its states as literals**, so SQLite uses the partial `idx_erasures_state`; with bound parameters it read every record of the last 30 days. (d) **`users rm-data` asks an older server again without `wait`.** A server from before this spec refuses an unknown query parameter with a `400` before it erases anything; the command repeats the request without `wait` and prints the answer as the end, since such a server answers the end, `--no-wait` or not. (e) **The user page reads the user again when the erasure it follows ends**, and shows the data gone once it was done; the lost answer's look for the erasure follows it, and reads again, only if the page is still that user's. Not changed: `recordProgress` decodes and encodes the erasure's counts in each job. | (a) #29 (a) wrote the cause when a later start ended the erasure and missed the start that ends it. (b) #28 (b)'s fairness held for one minute. (c) A partial index matches a query only on its condition's literals. (d) The CLI already read an answer without a state as an older server's end, and the server it meant refused the request first. (e) The banner said "Erased" over the charts of the traces it erased. The counts are a dozen keys, microseconds a job beside a batch rewrite or a chunk of milliseconds, and columns would be another schema change. |
| 31 | **2026-09-28** (found in the fifth review of PR #134) — **An answer belongs to the screen that asked, the dialog says an erasure is under way, and a tail that ran leaves nothing behind.** (a) **A screen that moved on follows nothing it did not ask for.** Both erase dialogs send the confirmed request through one helper, which holds the user and the project it was asked for. An answer, or a lost one, that comes back after the user page was reused for another user, or after the Settings card moved to another project, is said in the dialog and not followed: the screen never names its user with another's erasure, and the user page does not leave for `/users` on it. (b) **The dialog shows the dry run's `running`**, before the counts: "An erasure of this user is under way — <phase>, <id>. The counts below shrink as it goes, and confirming follows it rather than starting another." #13 kept the block for the dialog, and no screen read it. (c) **A tail that ran to its end drops its windows and what an earlier tail failed with**, in one job before the end is written. An end that is then not written leaves the erasure in its tail with nothing to scrub; a give-up after it drops no windows and says only #28 (b)'s sentence. (d) **The interface's watch stops on a 4xx**, as the CLI does (#27 g): a refusal, such as a role taken away, is the answer again, and was read every two seconds for as long as the screen was open. An answer that arrives after the watch stopped or moved to another erasure is dropped. (e) **The CLI's follow-up commands carry the operator's `--project` and `--url`**: `--no-wait`'s line and a watch that ends before the erasure print `tracepad users erasure` with them, as the lost answer's `tracepad users erasures` already did. Not changed: the stored error is still kept unless it names the user, raw or as `%q` escapes it (#27 c); no error on the erasure's path formats the user id, and its sources (the database, the writer, a batch conflict, the decoder) carry no escaped or folded form of it. Whether to keep only a fixed sentence and an error class instead is the owner's call. The lookups of one erasure stay separate queries. | (a) Twenty seconds is long enough to move to another user or project, and the user page is one component reused for every id. (b) The spec's reason for #13 is the person looking up a user whose erasure runs, and that person is on the interface. (c) #29 (a) and #30 (a) made the give-up say what the tail failed with; a tail that did not fail must not be described by an older one. (d) The CLI and the interface should read the same answer the same way. (e) A hint that drops the project fails for a key that reaches several, or goes to the default server. The owner decides between checking the error for the id and not keeping its text: the second loses "the disk is full", which is what an operator acts on. `user_id` is set only while an erasure is under way, and the unique index holds exactly that, so the two lookups of a running erasure cannot mean different things. |
| 32 | **2026-09-28** (decided by the owner after the fifth review of PR #134) — **The record says a failure as its phase and a cause from a fixed list; the error's text goes only to the server's log.** Refines #9 and #27 (c). `error` is "the <phase> phase failed: <cause>", or, for an erasure that ran out of starts, #28 (b)'s sentence with "; its tail last failed with: <cause>" when a tail did; the tail's failure that #29 (a) keeps is the cause alone. The causes, one for each source of failure on the erasure's path: "the disk is full" (`SQLITE_FULL`), "the database is busy" (`SQLITE_BUSY`, `SQLITE_LOCKED`), "the disk could not be read or written" (`SQLITE_IOERR`), "the server ran out of memory" (`SQLITE_NOMEM`), "the database file could not be opened" (`SQLITE_CANTOPEN`), "the database is read-only" (`SQLITE_READONLY`), "the database file is damaged" (`SQLITE_CORRUPT`, `SQLITE_NOTADB`) — the last two are not conditions, since they do not pass, but an operator acts on them — "the write queue stayed full" (the writer's queue past #16's two minutes), "a raw batch could not be rewritten" (a scrub that failed after its retries, one refused as changed every time, or a batch that could not be read to plan it), and "an unexpected error, which the server's log has" for anything else. A condition inside a raw batch's failure is said as the condition. A stop of the server is not a cause, though it was among the owner's examples: a run the stop ends records no failure and resumes, so the record never says it. Nor is a deadline, which nothing on the path has. The error itself is logged once, where it is met, with its cause: the raw phase's before the end is written, a chunk's when its tail begins, the tail's when it fails — the worker's line then says only "the tail failed: <cause>" — and any other of a run that stopped before the end on the worker's line. The end's line gives the record's sentence as `err`, as before. The writer does not log an erasure's jobs: a chunk and a scrub of the task, and the task's own jobs, report their failures themselves, and neither the writer's "write commit failed" line nor the line of a window such a job took down gives their error. A log line still never names the user (044 #15): an error whose text holds the id is logged as "an error that named the user, which is not logged" — the id as it is, quoted as `%q` quotes it, with its letters past ASCII escaped, escaped for a URL path or query, or encoded in a JSON string, its letters past ASCII escaped or not and those past the first plane as surrogate pairs. Letters are compared without their case, which an escape's hex digits may be written in. The id counts where it stands as a word of its own: an end of it that is a letter or a digit has none beside it, and an end that is neither, as in `@bob`, is a boundary itself. So a short id does not withhold every error that holds its letters; a short id that is a whole word of an error, `1` in "sqlite error 1", still withholds it, which is the safe side. | #27 (c) kept the error's text unless it held the id in one of two forms. That is a check for the forms someone thought of, and the record outlives the erasure by 30 days after it has forgotten the user (#9). A fixed list is a guarantee: nothing an error carries can reach the record. The list keeps what an operator acts on, a full disk or a busy database, and the log keeps the rest for whoever reads it next to the erasure's id. The log is now the only place the error is kept, which makes the forms it checks and a short id's reach matter there. |

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
  callers. In PR 2 it gains `Erasure` (the erasure's id, step 1's moment and
  the traces step 1 read): when set, `apply` adds its counts and
  `traces_deleted` to the `erasures` row, and its tail to `erasure_tail`, in
  the same transaction (#12, #26 a).
- `RawScrub`: gains `ErasureID`, and adds its tally to the row the same way.
- `EraseUserData` becomes `(*Store).runErasure(ctx, writer, id, options)`,
  run by the worker, `(*Store).NewEraser(writer, EraserOptions)` with
  `Start` and `Close`. `StartErasure(ctx, writer, UserErasure) (Erasure, error)` inserts
  or finds (#11) and wakes the worker. `Erasure(ctx, projectID, id)` and
  `Erasures(ctx, projectID)` read, `RunningErasure(ctx, projectID, userID)`
  answers the dry run's `running`, and `AwaitErasure(ctx, projectID, id,
  wait)` holds a request for its end.
- `TraceDelete` is unchanged. The server's round builder cuts its rows with
  one new store call, `DeletionChunks(ctx, projectID, hours, deletes, limit,
  budget, chunks)`, in the round's read slot, pricing only the hours of the
  chunks a round runs (#4, #22, #23).
- The sweeper removes finished erasures past 30 days (#15), as its last
  job of a pass.

## Data contract

`0029_user_hour_order` (PR 1):

```sql
DROP INDEX idx_traces_user;
CREATE INDEX idx_traces_user ON traces(project_id, user_id, timestamp);
```

`0030_erasures` (PR 2):

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
    attempts        INTEGER NOT NULL DEFAULT 0,          -- starts a crash cut off (#27)
    run             TEXT,                                -- #29 (b): the start that counted last
    traces_at_start INTEGER,
    counts          TEXT NOT NULL DEFAULT '{}',          -- the `deleted` keys
    compaction      INTEGER NOT NULL DEFAULT 0,
    error           TEXT,
    last_failure    TEXT                                 -- #29 (a): what a failed tail said
) STRICT;
CREATE INDEX idx_erasures_project ON erasures(project_id, created_at DESC);
CREATE UNIQUE INDEX idx_erasures_running ON erasures(project_id, user_id)
    WHERE user_id IS NOT NULL;                           -- #11, enforced
CREATE INDEX idx_erasures_state ON erasures(state, created_at)
    WHERE state IN ('queued', 'running');
-- #26 (a): the tail, one row per trace, gone when the erasure ends.
CREATE TABLE erasure_tail (
    erasure_id   TEXT NOT NULL REFERENCES erasures(id) ON DELETE CASCADE,
    trace_id     TEXT NOT NULL,
    arrived_from INTEGER NOT NULL,
    arrived_to   INTEGER NOT NULL,
    PRIMARY KEY (erasure_id, trace_id)
) STRICT, WITHOUT ROWID;
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
  user_id=?)` in index order with no temp B-tree. An hour's cost reads the
  hour's range of `idx_traces_timestamp`.
- A user whose traces arrived round-robin across three hours, each hour
  rolled: the chunks take whole hours, and each hour is in exactly one
  chunk's `Hours`, so it is rolled once. A client
  hang-up after chunk k leaves no hour counting an erased trace (023 #19's
  test, kept green).
- The budget: with `RollBudget` set low on the job, a chunk of dense hours
  takes one hour, and one of sparse hours takes several. The first hour is
  always taken.
- A trace whose start moves earlier between chunks is still erased, and its
  new hour rolled.
- An hour's cost is its traces and every observation they hold, counted from
  the rows before any roll (#22); an hour at the watermark costs nothing; a
  cost is asked only of the hours a chunk considers.
- Bulk deletion: sixty light hours delete in one chunk, and dense ones still
  stop at `deleteRoundChunks` with `more`.
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
