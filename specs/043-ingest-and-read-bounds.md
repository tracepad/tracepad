# Spec 043 — Ingest and read bounds: what one request may cost everyone else

**Status:** 🚧 IN PROGRESS
**Sprint:** September 2026

> Tracepad is one process with one SQLite writer, one connection pool and one
> heap, shared by every project it serves. Probing the ingest and read paths
> with hostile but authenticated requests showed how little bounds what one
> request may cost the others. An export of tens of thousands of small spans
> is one transaction that holds the only writer for seconds; while it does,
> every connection the pool opens runs `PRAGMA auto_vacuum`, which wants the
> write lock, gives up after the busy timeout, and turns another project's
> export into a `401` — which an exporter drops rather than retries. Reads
> have no deadline and no concurrency limit, `?tag=` repeated a thousand
> times is a scan nobody can cancel (and a `500` past the expression-depth
> limit), and a chain of a few thousand spans renders in seconds because
> every level of the tree re-validates the JSON of the levels below it; past
> five thousand the encoder's nesting limit fails the response after the
> `200` is on the wire, so the trace answers an empty body for ever. Numbers
> are no better: a cost of `1.5e308` sent on two observations makes the
> trace's total infinite, and every listing, statistic and user page of the
> project answers `200` with an empty body; a cost total sent as a string
> stops the project's statistics rollup for good and makes the hours it sits
> in undeletable. This spec gives each of these a bound, a status an exporter
> or a client can act on, and a test that fails without it.

---

## Overview

Deliverables, three PRs in this order (the last commit of the third flips
the status):

1. **Honest failures and finite numbers** — no new settings. ✅ Implemented (#112).
   - A credential that could not be checked is `503`, not `401`; `auto_vacuum`
     leaves the connection string; the pool keeps the connections it opens
     (Decision 1).
   - A storage failure that is the database's condition rather than the
     request's content is `503` on ingest (Decision 2).
   - Every JSON response is encoded before its status is written (Decision 3).
   - One counting rule for costs and token counts, shared by ingest, the
     rollup and the live scans; finite totals from the mapper; instants that
     cannot overflow; saturating sums; a render backstop (Decisions 4–7).
   - A rollup pass that one failing hour cannot stop (Decision 8).
   - Migration 0025 repairs aggregates that are already non-finite
     (Decision 9).
2. **Ingest bounds** — `TRACEPAD_MAX_SPANS_PER_REQUEST`,
   `TRACEPAD_BODY_BUDGET_BYTES` (Decisions 10–14).
3. **Read bounds** — `TRACEPAD_READ_TIMEOUT`, `TRACEPAD_READ_CONCURRENCY`;
   the `?tag=` cap; a tree with a node, depth and size ceiling, rendered in
   linear time; the interface's notice for a trace shown in part
   (Decisions 15–20).
   ✅ Implemented: Decisions 15–20, the read half of 21 (`read_slots`,
   `reads_timed_out`, `reads_refused_busy`) and 22 for the two read settings;
   the details as built are Decision 25.

Decision 21 (what `GET /api/v1/system` reports) lands with PRs 2 and 3,
each with its own half. Docs: `docs/ingest.md` (statuses, the two ingest
settings, the collector's batch settings), `docs/api.md` (read statuses,
`observations_omitted`, the tag cap), `docs/docker.md` (the four settings),
`openapi.json`, the MCP tools' output schemas, `schema.d.ts`.

Builds on spec 002 Decision 27 as merged: `TRACEPAD_MAX_BODY_BYTES` bounds
the decompressed body. What that decision left open — how many such bodies
the server holds at once — is Decision 13 here.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-26** — **A credential the server could not check is `503`, never `401`.** A failed or timed-out key lookup, session lookup, project lookup or membership lookup in the guard answers `503` with `Retry-After: 1` and `{"error": "cannot check credentials right now; retry shortly"}`; `401` stays the answer for a credential that was checked and is unknown. Each lookup runs under a deadline of its own, **5 s**. And the cause of the observed failures goes: `auto_vacuum(INCREMENTAL)` leaves the DSN — the mode is a property of the file: it is set once, as a fresh file is created, and on an existing file `ensureIncrementalVacuum` already checks it at every open (spec 005 #5) — and the pool keeps what it opens (`SetMaxIdleConns` equal to the open limit, `SetConnMaxIdleTime(5m)`), so per-connection pragmas run once per connection rather than once per burst | Setting `auto_vacuum` to a full or incremental mode writes the header even when the mode is unchanged, so SQLite begins a write transaction for it. In the DSN it runs on every new pool connection, and the pool closes all but two idle connections (the `database/sql` default), so every burst of concurrent requests opens connections that queue for the writer's lock; behind a long commit they wait out `busy_timeout` (5 s), fail, and the guard answered that failure `401` (`auth.go`, `identify`). An exporter treats `401` as final: it drops the batch and its operator starts checking a key that was never wrong. `503` with `Retry-After` is what OTLP exporters retry, and it tells a person the truth. The deadline keeps a lookup that waits for a pool connection (Decision 16 bounds the pool) from holding an ingest request indefinitely. |
| 2 | **2026-09-26** — **Ingest answers `503` for a failure of the database, `500` for anything else.** `SQLITE_BUSY`, `SQLITE_LOCKED`, `SQLITE_NOMEM`, `SQLITE_IOERR`, `SQLITE_FULL` and `SQLITE_CANTOPEN` (primary codes, extended codes folded) are `503`, `Retry-After: 1`, `{"error": "storage is temporarily unavailable; retry shortly"}`, logged at error once per minute per code with a count. Everything else keeps `500 failed to store spans` | A full disk, an I/O error or a lock that did not clear is a condition that passes: retrying the same batch later is exactly right, and `500` made every exporter drop it (only `429`, `502`, `503` and `504` are retryable in OTLP/HTTP). A constraint failure or an encoding error is the batch's own and would fail again on every retry, so it stays a `500`, which exporters do not retry — a retry loop over a poison batch would be worse than the loss. After this spec's number rules no known client input reaches the second class; a new one is a bug to fix, not a condition to wait out. |
| 3 | **2026-09-26** — **A response is encoded before its status is written.** `writeJSON` marshals into a buffer, appends the newline `json.Encoder` wrote, and only then writes the header and the bytes; a value that does not encode is `500` `{"error": "failed to render the response"}`, logged with the type of the value and the encoder's error. The bytes of every response that encoded before are unchanged | `writeJSON` wrote `200` and then encoded (`api.go`, `writeJSON`), so any encode error — a non-finite number, a nesting limit — left a `200` with an empty body. A client cannot tell that from an empty answer, a cache may keep it, and the interface shows a blank page with no error to report. The buffer costs one copy of a response that the page cap and Decision 18 bound; a streaming endpoint, when one exists, is not built on `writeJSON`. |
| 4 | **2026-09-26** — **One counting rule for each number the store adds up.** A cost counts when `cost_details.total` is a JSON number (`json_type` `integer` or `real`) with magnitude at most **10^12**; a token count counts when it is a JSON number from **0 to 10^9**. Anything else in those places is stored as sent (spec 002 #11) and counted as *no data* — `NULL`, not zero (spec 002 #14). Each rule is one SQL expression built by one Go function in the store, and every reader uses it: the trace aggregate (`refreshAggregates`), the statistics rollup, the users rollup and the live statistics scans for cost; `tokenExprs` for the three token classes (amends spec 031 #1: "present but not a count" now includes a number outside the range, and the class is still `NULL` rather than read from the next spelling) | Cost was read with a bare `json_extract` in four places and scanned into a `float64`: a string `total` failed the scan and with it the hour's roll, and two totals near the largest double summed to `+Inf` in SQLite, which stored it and handed it to an encoder that cannot write it. Token counts were cast to integers — `1e300` saturates to `MaxInt64` — and then summed: SQL's `SUM` raised `integer overflow` in the live scan while the rollup's Go addition wrapped negative. A domain rule removes overflow instead of chasing it: 10^12 is beyond any price one call costs in any currency, and a sum of such values needs ~10^296 rows to overflow a double; 10^9 is two orders beyond the largest context any model offers, and a sum of such counts needs ~9×10^9 observations to overflow an `int64`. Keeping the value as sent keeps the evidence; counting it as no data is the answer spec 002 #14 already gives for "not a cost". |
| 5 | **2026-09-26** — **The mapper produces no number the store cannot keep, and no instant arithmetic can overflow.** (a) `mapCost` derives `total` from the components only when their sum is finite; otherwise `cost_details` is stored without a derived `total`. (b) An RFC 3339 instant outside the years 1678–2262 — where `time.Time.UnixNano` is undefined — is not an instant: it stays unclaimed and lands in metadata like every other shape `parseInstant` cannot read. (c) Only a positive completion start takes part in time-to-first-token, in the trace aggregate (`MIN` over positive values, as `start_time` already is) and in the per-observation value the tree renders | (a) Each component was already screened for finiteness, but `1e308 + 1e308` is `+Inf`, which `json.Marshal` refuses, so the whole batch failed with `500` — one bad span rejecting N−1 good ones, which spec 002 #13 forbids. (b) and (c) are the same class through a different door: a completion start of `-9223372036854775808` minus a positive start overflows to a real, and the STRICT `ttft_ms INTEGER` column refuses it, again failing the batch; a date in the year 3000 produced an arbitrary nanosecond count. A span that never said when it started already has no wait to measure (spec 012 #3); a completion start before 1970 is the same fact. |
| 6 | **2026-09-26** — **Sums in Go saturate.** `addCount` holds at `math.MaxInt64` instead of wrapping, and the cost addition of the rollups holds at the largest finite double instead of reaching `+Inf` | Decision 4 makes both unreachable from client input; this is the belt for a row written before it, and for a future reader that forgets the rule. A sum that holds at the limit is visibly absurd; one that wraps negative or becomes `Inf` breaks the page that shows it. |
| 7 | **2026-09-26** — **A non-finite number renders as `null`.** The response writer checks every `float64` and `*float64` it places in a rendered object; a NaN or an infinity is written as `null` and logged once a minute with the field name and a count | The backstop for everything Decisions 4–6 do not reach: an aggregate stored before migration 0025 ran, a score mean or a run delta computed from extreme values. `null` is the API's existing word for "no number" (spec 002 #14), which is the truth about an infinite one. It sits in the one writer every object response goes through (`render.go`), so no handler has to remember it. |
| 8 | **2026-09-26** — **One failing hour does not stop a pass.** In the dirty-hour loop a failed hour is logged and the loop goes on; if any failed, the pass does not move `last_pass`, so those hours are found again next pass. In the forward roll a failure stops the roll at that hour and the watermark still advances to it — past everything this pass rolled, never past the failure. Other projects already continue past a failing one (`Aggregator.Pass`) | A failing hour used to return from the project's pass before the watermark moved, so one poisoned hour stopped that project's statistics for good and kept the live scan answering for an ever-growing tail. With Decision 4 no known input makes a roll fail; a genuine failure — a full disk, a bug — must still cost only its hour. Holding `last_pass` re-rolls the hours that did succeed, which is idempotent (spec 013 #3); moving it would lose the failed correction for ever, which is the defect the aggregator's comments already warn about. The watermark never moves past an unrolled hour, because the read seam would then answer it as zero (spec 013 #12). Deletion and erasure re-roll inside their own transaction (spec 035, spec 023 #19) and gain from Decision 4 directly: the hour a poisoned trace sits in can be deleted again. |
| 9 | **2026-09-26** — **Migration 0025 repairs what is already stored.** It recomputes `total_cost` of every trace whose total is not finite or whose observations carry a cost the rule of Decision 4 no longer counts; it stamps `updated_at` on those traces and on every trace with an observation whose token count Decision 4 no longer counts, so the next pass re-rolls their hours through the dirty-hour mechanism (spec 013 #15); and it sets to `NULL` every non-finite `total_cost` in `stats_hourly`, `users_hourly` and `users` and every negative token sum in `stats_hourly`. The pass then rewrites the hours it can recompute; a frozen hour (spec 013 #11) keeps the `NULL` | Decision 7 makes a poisoned project readable; only a repair makes its numbers right. A trace whose raw rows exist is recomputed from them, exactly as a late span would have made it. A frozen hour has lost its raw rows, so the honest value of an infinite or wrapped sum there is "no data". The migration runs once, before the server listens, and reads the observations that carry a cost or a usage once. |
| 10 | **2026-09-26** — **At most `TRACEPAD_MAX_SPANS_PER_REQUEST` spans per export, default 20,000.** Counted after decoding, before mapping; an export with more is `413` `{"error": "this export carries N spans; the server takes at most M per request (TRACEPAD_MAX_SPANS_PER_REQUEST)"}`, counted as a rejected batch and not stored raw. Refused whole, never cut | The body cap bounds bytes, not work: 20 MiB holds a few hundred thousand minimal spans, and mapping, indexing and committing cost per span. 20,000 is far above the 512 spans OpenTelemetry's batch processors send by default (spec 002 #16) and above the Collector's default batch of 8,192, which a Collector may exceed by one incoming request when no maximum is set — the docs name `send_batch_max_size` for that deployment. A refusal is loud and whole: the exporter logs it, and no trace is stored with an arbitrary half of its spans, which a cut accepted through `partial_success` would do — and a partial success is no more retryable than a `413`. |
| 11 | **2026-09-26** — **An export commits in slices of at most 1,000 rows.** The handler splits the mapped batch — traces with their observations, a trace larger than a slice split across consecutive slices — and submits the slices one after another, each a job of its own; the export is answered after the last one commits. The first slice carries the media check and the media bodies and refs (spec 041 #9, #23); the last carries the raw body and its media refs. A failure answers the export as Decisions 2 and 13 say, with the slices before it committed | One export was one transaction, so its size was how long the only writer was unavailable to every other project. A slice bounds that to a fraction of a second, and submitting the next slice only after the previous commits puts every other request's job between them in the FIFO queue. A partly committed export is safe because ingest is an upsert by natural key (spec 002 #5): the retry rewrites the same rows. Aggregates stay exact because each slice recomputes the traces it touched from their own rows (spec 002 #22). Raw goes last so that a failed export leaves no raw body and its retry stores exactly one. `200` still means everything is on disk (spec 002 #15). |
| 12 | **2026-09-26** — **A commit window closes at 1,000 rows as well as at 64 jobs.** Every job reports a weight — an ingest slice its rows, any other job 1 — and the writer stops collecting when the window's weight reaches 1,000 | Slicing alone lets 64 concurrent big exports put 64 slices in one window, one transaction of 64,000 rows. With the weight rule a window stays under 2,000 rows — what it held before the job that crossed the line, plus that job — while a window of small jobs — scores, prompt writes, small exports — still groups up to 64 for the fsync economy of spec 002 #15. |
| 13 | **2026-09-26** — **Request bodies share a budget, `TRACEPAD_BODY_BUDGET_BYTES`, default four times `TRACEPAD_MAX_BODY_BYTES` (80 MiB).** Every body read into memory counts — ingest, the JSON API, the Langfuse media upload — as decompressed bytes, reserved in 64 KiB steps as they are read and held until the handler returns. A request whose next step does not fit fails at once with `429`, `Retry-After: 1`, `{"error": "the server is holding as many request bodies as it can; retry shortly"}`. A budget smaller than the body cap refuses to start. The public routes' 8 KiB bodies (spec 028 #26) are not counted | Spec 002 #27 bounds one body; handler concurrency was unbounded, so N simultaneous exports held N bodies and their decoded, mapped and re-encoded copies. Counting what was read, not what was declared, covers gzip, whose size is unknown until it is inflated. Failing at once rather than waiting cannot deadlock (no request holds half a body while waiting for the rest). `429` with `Retry-After` is the status the writer already uses for "full, come back" (spec 002 #15), and every OTLP exporter retries it. The reservation is held until the handler returns because the decoded export lives that long — through its wait for the writer, which is the backpressure. The budget counts body bytes, not heap; the docs state the measured ratio of peak heap to body so that an operator can size it against a container's memory limit. |
| 14 | **2026-09-26** — **The labels a listing shows are cut at 1,000 characters at ingest, and a trace keeps at most 50 distinct tags.** The labels are the trace's name, user id, session id, environment, release and version, each tag, and an observation's name and model; a longer value is cut at a character boundary, a longer tag list keeps its first 50 distinct entries in order. The raw body keeps what was sent (spec 002 #9), and a remap applies the same rule | These are the fields every row of a listing, a facet or a statistics key carries, and each was bounded only by the body cap: a 20 MiB trace name is 20 MiB in every one of up to 500 rows of a page. Refusing the span would break spec 002 #13, and leaving the value unclaimed would empty the field that identifies the trace; a cut keeps the field useful and the full value recoverable. 1,000 characters is five times the score-name limit (`maxScoreNameLength`), room for a name that is a sentence. |
| 15 | **2026-09-26** — **Reads have a deadline, `TRACEPAD_READ_TIMEOUT`, default 20 s.** Every `GET` route of the route table except the public ones runs under a context with that deadline, and every store read takes a `context.Context` and runs with `QueryContext` / `QueryRowContext`; the driver interrupts a statement whose context ends. A read stopped by the deadline is `503` `{"error": "the read took longer than 20s and was stopped; narrow the time range or the filters"}`, without `Retry-After`. Background jobs pass their own contexts and are not bound by it | Nothing stopped a read: a client that hung up left its query running, and a filter that scans every row (spec 009 #12: the cap bounds the answer, not the work) ran as long as it took. 20 s answers before the interface's own 30 s clock gives up, so a person sees the server's reason rather than a network error. `503` because the server declined to finish; no `Retry-After`, because the same request would be stopped again. `504` belongs to a gateway. The deadline covers the wait for a read slot (Decision 16) as well, so one number bounds the whole read. |
| 16 | **2026-09-26** — **At most `TRACEPAD_READ_CONCURRENCY` reads at once, default twice `GOMAXPROCS` and at least 4.** A read route takes a slot before its handler runs and gives it back when the response's status is written, or when the handler returns without writing one — so a slot covers the store work and the rendering (Decision 3 renders before the status), never the client's download; a request that gets no slot before its deadline is `503`, `Retry-After: 1`, `{"error": "the server is busy; retry shortly"}`. MCP tools reach the read API through the same routes (spec 004 #16) and take the same slots. The pool opens at most twice the slots plus 8 connections — the writer, the background jobs, credential lookups and handlers that hold two statements at once — and keeps them (Decision 1) | SQLite reads are CPU-bound; more of them at once than there are cores only makes each slower and holds more memory and more WAL snapshots, which keep checkpoints from finishing. A slot is taken per request, not per statement, so a handler never waits for a second slot while holding the first; it is given back at the status, so a client that reads a large media body or a large tree slowly holds a socket, not a slot. Waiting within the deadline rather than refusing at once is for the interface, whose dashboard asks several questions at the same moment. The pool bound keeps a flood of anything — ingest credential lookups included — from opening connections without limit; the headroom beyond the slots keeps writes and lookups from queueing behind reads. |
| 17 | **2026-09-26** — **`?tag=` takes at most 50 distinct values.** Values are deduplicated first; more than 50 is `400` `{"error": "tag: at most 50 values"}`, on every route that reads the trace listing's filters | Tags are ANDed (one `EXISTS` over the trace's tag array per value), and a trace keeps at most 50 distinct tags (Decision 14), so a filter naming more can match nothing. Without a cap each repetition multiplied the scan's cost, and a thousand of them passed SQLite's expression-depth limit and came back `500` — an internal error answering a malformed request, the case spec 027 #20 already closed for the other list filters. |
| 18 | **2026-09-26** — **A trace's tree holds at most 10,000 observations and 32 MiB of skeleton, and `?expand=io` holds one payload at a time.** `GET /traces/{id}` and `/traces/last` build the tree from the longest prefix of the trace's observations, in `(start_time, id)` order, that fits both limits — the store reads at most 10,000 rows, and the 32 MiB counts the observations' own fields, the trace's fields and metadata being rendered whole as spec 004 #24 says — and say what they left out with `"observations_omitted": N`, N being `observation_count` minus the observations rendered, present only when N > 0. An observation whose parent was left out renders at the root with its `parent_observation_id`, as an orphan does. Under `?expand=io` each payload is read, cut to its share (spec 004 #6) and released while the tree is written, instead of every payload being loaded before the first is budgeted; the bytes of the answer are unchanged. The interface says above the tree how many observations are not shown (amends spec 004's edge case "structure is never truncated") | Structure was exempt from every budget because a partial tree seemed a wrong answer rather than a smaller one, but its size had no bound at all: the number of observations grows across exports without limit, and each observation's skeleton carries unbudgeted maps (`usage`, `model_parameters`, `cost_details`). A prefix in start order is the part of a run that happened first, the same order the tree already sorts siblings by, and the count tells every consumer that the answer is partial and by how much. 10,000 is well past any agent run a person reads as one tree; 32 MiB is the bound for a trace whose observations are few but crafted to be heavy. Fixed numbers, not settings: they shape the answer a client parses, which should not depend on the server it asked. The payloads were the same problem one step later: a 50 KiB answer was built by decoding every input, output and metadata of the trace into memory first, so its peak was the sum of the trace's payloads; read one at a time it is the skeleton plus the largest one. |
| 19 | **2026-09-26** — **The tree is at most 100 levels deep.** An observation at depth 101 is detached from its parent and rendered at the root with its `parent_observation_id`, keeping its own children — the rule spec 004 #30 applies to a cycle's entry — so a deeper chain becomes several shallower ones and every observation still appears once | Nesting is two JSON levels per observation, and parsers have limits: Go's decoder stops at 10,000 levels, Python's at its recursion limit of about 1,000. A chain five thousand deep was a trace no client could read even when the server could render it. 100 levels is far deeper than any real call stack of an application's steps. |
| 20 | **2026-09-26** — **Rendering is linear in the size of the response.** A rendered object writes its nested objects and lists of objects straight into one buffer instead of marshalling each level separately; leaves still go through `encoding/json`, so every response's bytes are unchanged | `object.MarshalJSON` called `json.Marshal` on each child, and the standard encoder re-validates whatever a `MarshalJSON` returns — so each level re-scanned everything beneath it, quadratic in the depth of the tree. Decision 19 bounds the depth, and this removes the multiplier altogether, so the skeleton's cost is proportional to its bytes and Decision 18's 32 MiB is a bound on time as well as on memory. |
| 21 | **2026-09-26** — **`GET /api/v1/system` shows the new limits.** Beside `writer_queue` it reports `body_budget` (`held_bytes`, `capacity_bytes`) and `read_slots` (`busy`, `capacity`), deployment-wide like the queue; the project's `counters` gain `exports_over_span_cap`, `bodies_refused_for_budget`, `reads_timed_out` and `reads_refused_busy`, since the process started (spec 004 #10, #33) | An operator who sees `429` or `503` in an exporter's log needs to know which limit said it and how often, and the limits are only worth their settings if someone can see them bind. The gauges are the process's, like the queue depth; the counts are the project's, like the ingest counters, so no project learns another's traffic. |
| 22 | **2026-09-26** — **Capacity is a setting; shape is a constant.** The four settings above size the machine's work — spans per request, bytes in memory, time and parallelism of reads — and each has a default that needs no tuning on a laptop or a small server, validated at start (out of range refuses to start, like `TRACEPAD_RESPONSE_BUDGET_BYTES`). The limits that shape an answer or a stored value — label length, tags per trace, the tag filter, the tree's nodes, depth and size, the slice and window weights, the counting ranges — are constants | A client written against one server must read the same shapes from another; an operator sizing a deployment needs the knobs that trade memory and latency for throughput and no others. |
| 23 | **2026-09-26** (found in implementation) — **Three details of #1 and #3 as built.** (a) A fresh file is created by a connection of its own that sets `auto_vacuum=INCREMENTAL` and then `journal_mode=WAL`, before the pool opens it. (b) Until #16 bounds the pool, the connections it keeps are the number #16 gives it — twice the default read slots, plus 8 — and opening beyond them is not yet bounded. (c) A response that does not render is logged with the type of the value, not the route | (a) WAL writes the file's header, after which the mode cannot change without a VACUUM; the old DSN named `journal_mode` before `auto_vacuum`, so on a fresh file the pragma never took and every new database was rewritten by `ensureIncrementalVacuum` on its first open. (b) #1's rule is that the pool keeps what it opens, and the open limit it names arrives with #16; the idle limit is set to that number now so the rule holds from this PR. (c) `writeJSON` does not see the request, and wrapping the `ResponseWriter` to carry the route hides from `http.MaxBytesReader` the method through which it closes a connection after an over-long body. |
| 24 | **2026-09-27** (found in the first five reviews of PR #112) — **What #8 stops for, how long a failing hour holds the rollup back, and the edges of #1–#9.** (a) A failure that is not the hour's own — the pass cancelled or out of time, the writer closed or its queue full, a database condition of #2 — ends the project's pass at the first hour it meets, before the watermark and `last_pass` move; the next pass starts over. (b) A failing hour holds the rollup back for at most **3 passes in a row**, counted against what is held rather than per hour: `last_pass` at one value, for the dirty hours behind the watermark, and the watermark at one hour, for a closed hour ahead of it. After that the pass gives up — logged — and moves on: `last_pass` moves past the failed dirty hours, which keep their previous numbers until a span lands in them again; the forward roll goes on past the failed closed hour, which answers from whatever the rollup holds for it. The counts live in memory; a restart grants the passes again. (c) A cost that is not a finite number adds nothing to a sum, and a sum of nothing else is no data — not a NaN, and not the zero two opposite infinities made. (d) The mapper sums cost components in key order. (e) A response that does not render also loses `ETag` and `Last-Modified` and says `Cache-Control: no-store`. (f) Migration 0025 also nulls a token sum larger than 10⁹ per row its cell counts. (g) The `503` of #1 is documented on every guarded operation, as one shared response. (h) A commit a database condition failed is logged by the writer once a minute per condition, with the number of failures it stands for, and the handlers do not log it again; every JSON API write answers it `503` with `Retry-After: 1`, as ingest does. (i) A client that hangs up during a credential lookup gets no answer and no error line. (j) The pool keeps `max(4, GOMAXPROCS)` idle connections, not the size #16 will give it (amends #23 b) (k) That line is at error, whichever line it was — the window's warning or a job's error. (l) A failed hour is logged once, where it failed; the pass reports how many. (m) Migration 0025 finds the traces it repairs in one pass over the observations, parsing each `usage` once, and every migration is logged before it starts and after it ends, with the rows it changed and the time it took. (n) What is held for a project that is no longer there is forgotten. (o) A `total` is a number when it is a JSON number or a string whose whole text is one (`"0.25"`, strict JSON: not `1e5e5`, not `Infinity`) — amending #4; the mapper stores such a string as the number, and the repair keeps its cost. (p) The repair nulls a token sum only where no honest one can be that large: a negative sum, the largest int64 itself, and on a model cell a sum past 10⁹ for each observation it counts — amending (f), whose bound was wrong for a cell with no model. (q) A failed hour's one line is the aggregator's, which names the project and the hour; the writer does not log a job whose caller does. (r) The writer logs a database condition only for a write it lost, at error, counting lost writes since the last line; a window the condition failed whose jobs then commit one by one logs nothing. (s) A guard lookup a database condition failed is logged once a minute per condition, and any lookup — in the guard or in a handler — whose client hung up is neither answered nor logged. (t) The guard reads a session's project and its role in one query, under one deadline. (u) Found in the fifth review: a string `total` is a number only when it is a strict JSON number, in the mapper as in the counting rule — `.5`, `+1`, `007`, `0x1p-2` stay as sent (amends o); a guard lookup that ran out of its deadline is logged once a minute, as a condition is; a handler's own lookup of the project or account its route names answers a database condition `503` with `Retry-After`, logged once a minute, and anything else `500`; a session slide a condition failed is not logged by the handler; ingest and the JSON API answer a failed write through one mapping; a migration's line reports `rows_written`, every row its statements wrote, a scratch table's included (amends m); a cost summed sample by sample is a value, not an allocation per sample. | (a) Every hour after it would meet the same failure, and going on logged an error for each and rolled none — one line per dirty hour, thousands after a repair. (b) Held for good, `last_pass` made every later pass re-roll everything that changed since, for one hour that cannot be rolled, and a stuck watermark left every later hour to the live scan — the "one poisoned hour stops the statistics for good" #8 set out to remove. Moving `last_pass` to just before the failed hour's oldest stamp bounds nothing: an hour that always fails keeps that stamp. Counted per hour, two failing hours held it for ever by taking turns — one giving up as the other came back. A correction is still never dropped silently — it is dropped loudly, and the next span retries it; giving up on a closed hour trades one hour's numbers for every later hour's, which is the better loss. (c) A stored infinity is not a cost (#4), and a NaN is not a number. (d) Near the largest double, whether the sum overflows depends on the order it is taken in, and a map's order is random: the same span stored a total on one delivery and none on the next. (e) A handler may have set how long its answer keeps — a prompt read sets a minute — and a failure must not be kept at all. (f) `CAST(1e300 AS INTEGER)` is the largest int64, a positive sum the negative check missed; the bound assumes 10⁹ tokens per trace on a trace-unit cell, three orders past any context. (g) The guard answers it on every such route; a client generated from the document has to know it can. (h) The condition is the same news on every commit until it passes; the writer logged each failed commit, whatever the handler did. A score or a prompt written while the disk is full is lost to a `500` exactly as an export was. (i) The lookup failed because the client left, not because storage did; the log line was a false alarm and the `503` went to nobody. (j) Each kept connection keeps its own page cache: the size #16 names was 264 idle connections on a 64-core host, hundreds of megabytes held after one burst. The number of processors is the work the machine can do at once; until #16 bounds opening, a burst past it opens and closes connections as before. (k) An alert on errors has to see a full disk, and the pacing let the window's warning take the minute's one line. (l) The hour's line and the pass's joined error said the same thing twice, every tick. (m) The first version scanned the observations twice and read each of nine token keys with two JSON calls: about 8 s for a million observations that all carry a cost and a usage, against about 3 s in one pass (measured on a laptop, synthetic data); on a larger store it runs before the server listens, and without a line it looked like a hang. (n) A deleted project's counts would otherwise live as long as the process. (o) SQLite's `SUM` always read `"0.25"` as 0.25, and a numeric-only rule would have taken those costs away from every stored trace through the repair — data a client sent and the store had counted. (p) A cell with no model counts traces and sums every observation of them: an agent's trace of three thousand calls at half a million tokens honestly sums to 1.5 × 10⁹ in a cell whose count is one, and nothing stamps its hour, so the NULL would have stood for ever. `CAST(1e300 AS INTEGER)` stores exactly the largest int64, which no honest sum reaches; a wrapped sum is negative or on a trace the repair stamps. (q) The writer's line said the same failure again without the hour. (r) A window's warning took the minute's one line and was promoted to error even when every write committed on its retry; the count counted log calls, not failures. (s) The same condition failed every lookup as it failed every write, and a hang-up was a false alarm wherever a lookup ran with the request's context. (t) Behind a held lock a session request could wait for three lookups in a row, about fifteen seconds, before its `503`. (u) Go's parser takes texts JSON does not, so such a cost counted when it arrived and not once a repair or a late span re-rolled its trace — one trace, two totals. A lock held past five seconds fails every exporter's retry alike, as a condition does. The guard already answered a condition `503`; the handler behind it answered the same condition `500`, which a client does not retry. The writer logs the condition once a minute; a line per signed-in request buried it. Two copies of one status mapping drift. The count included the repair's temporary table, which is not what "changed" says. A statistics answer over raw rows took one allocation per sample. |
| 25 | **2026-09-27** (found in implementing PR 3) — **Details of #15–#20 as built.** (a) The read gate stands after the guard: a credential lookup keeps its own 5 s deadline (#1) and takes no slot, and the read deadline starts when the guard admits the request, covering the wait for a slot and the handler. "The public ones" are the routes whose policy is `public`; the Langfuse SDK's `GET /api/public/media/{id}` is an `ingest` route and is gated like every other `GET`. (b) The gate answers a read the deadline stopped: a store read whose context ended writes nothing, a `5xx` a handler writes after the deadline is held back, and the gate answers `503` in its place, without the `Retry-After`, `ETag` or `Last-Modified` the handler may have set and with the caller's `Cache-Control: private, no-store` (spec 001 #17) in place of whatever it said instead; a read whose client hung up is answered by nobody and logged by nobody. Every read handler answers a failed store read through one helper, which also makes a database condition of #2 a `503` with `Retry-After` on reads, as #24 (u) made it for lookups. (c) The pool opens at most twice the slots plus 8 connections and keeps `max(4, GOMAXPROCS)` of them idle between bursts (#24 j), not all it opened — amending #16's "and keeps them". (d) The 32 MiB of #18 is the sum of the observations' own fields as rendered — each observation's object without its payloads and children; the scan stops at the first observation that does not fit, so no observation after it is decoded. (e) `observations_omitted` comes before `observations` in the answer. (f) A read refused or stopped for a caller with no project — an account's own routes, the admin token's listings — counts in no project's counters. The payload slots of `?expand=io` are counted from the payload references an observation carries, not from the payloads read: they differ only for a metadata payload that is not an object or a stored JSON `null`, neither of which ingest writes, and counting what reads back would mean reading every payload before the first is budgeted. (g) Background work passes the context it already has; a read the writer makes inside its own transaction runs under `context.Background()`, as its `BeginTx` does. (h) Testing #15's `?expand=io` check uses inputs of 4 KiB, not 1 MiB: the counting seam proves one payload at a time whatever the size, and 2 GiB of payloads is minutes of suite time; the 50,000 traces of #12 and the trees of #15 are copied inside the database file rather than written through the writer, which spends about a millisecond a row. | (a) A lookup that waited for a read slot would put ingest behind reads, which #16 forbids; the route's policy is the table's word for who may call it, and a path prefix is not. (b) A handler that answers a query the deadline interrupted cannot tell that from any other failure, and ninety handlers each learning to would be ninety chances to answer `500`; the gate knows the deadline passed. A hang-up used to leave its query running and so failed nothing; logging it now would be a new false alarm. (c) #24 (j) measured what keeping every connection costs on a large host; the open bound is what #16 needed from the pool. (d) Reading all 10,000 rows before measuring would hold every map the bound is there to limit. (e) A reader of a stream learns the tree is partial before it reads the tree. (f) The counters are a project's (spec 004 #33), and a refusal about no project is nobody's traffic. (h) The bounds are what is tested; the writer's speed is not. |
| 26 | **2026-09-27** (found in the first review of PR #117) — **What the gate lets through, and which of its two answers a stopped read gets.** (a) A read the deadline stopped after spending longer waiting for its slot than running is answered as busy — `503` "the server is busy; retry shortly" with `Retry-After: 1`, counted in `reads_refused_busy` — and only one that ran longer than it waited is told to narrow the request, counted in `reads_timed_out`; one number still bounds the whole read (#15). (b) `GET /api/v1/queues/{name}/next` is not gated: it claims an item through the writer (spec 024 #5), and a claim the read deadline answered would still commit. (c) `GET /api/v1/system` runs under the deadline and takes no slot, so `read_slots.busy` counts the other reads and the gauge answers while every slot is taken. (d) A raw body's media are put back before its status is written, inside the slot and the deadline; a body the deadline cut short is not sent. (e) A tree knows it was cut from its own read — the store reads one row past the ceiling to learn whether there is one, and the 32 MiB stop is a cut — rather than from the trace's count, read before it: a cut tree says at least `"observations_omitted": 1`, and a whole one says nothing, whatever the count says. (f) Each observation's own fields are kept at their length, so the 32 MiB is what the tree holds. | (a) The edge case already said so: a read slow only because it waited is the server's condition, which passes, and telling it to narrow its filters would send a person after a fault that is not there. (b) Spec 024 #5 made claiming a `GET` on purpose; the gate is for reads. (c) The gauge is what an operator reads when reads are refused, and a gauge behind the limit it reports is unreadable exactly then. (d) The slot was given back at the status, so the reads after it ran outside the bound, and a deadline during them left references in a body promised to be the one the client sent. (e) A live trace's count, read before spans that took it past the ceiling arrived, said nothing was left out of a tree that was cut. (f) A buffer's capacity may be twice its length. |
| 27 | **2026-09-27** (found in the second review of PR #117) — **The gauge's own lane, and reads that hold one statement at a time.** (a) `GET /api/v1/system` reads in a lane of one slot of its own, under the read deadline: a second one waiting past its deadline is `503` busy, and the lane is not one of `TRACEPAD_READ_CONCURRENCY`'s, so the gauge still answers while those are taken — amending #26 (c), under which it took no slot and its counts ran without a bound. Its one connection comes out of the pool's headroom. (b) Which routes the gate bounds, and how, is decided beside the route table, by route: a `GET` that writes and one that reports the gate's gauges are named there, and every other `GET` with a credential is a read. (c) An observation the renderer cannot write is answered as a rendering failure — `500` "failed to render the trace" — not as a failed read. (d) The whole-trace and single-observation reads resolve payloads after their rows are read and the cursor closed, as the tree does: no read holds a statement open while it asks for another. (e) The interface's notice counts the observations the tree holds and adds `observations_omitted`, rather than subtracting from `observation_count`. | (a) Uncapped, a flood of gauge reads — each a count over every table of a project — took the connections the writer and credential lookups keep in reserve, the starvation #16's headroom exists to prevent; a lane of one bounds it without putting the gauge behind the limit it reports. (b) A route added to the table is judged where it is added. (c) An operator reading "failed to read" looks at the database for a renderer's bug. (d) #16 sizes the pool at two statements a read; a read that held its cursor for each row's payloads was the one way to need more. (e) The count may have been read before the spans that took the trace past the ceiling (#26 e); the tree is what is on the screen. |

## API contract

New and changed statuses. Every error body is the API's one shape,
`{"error": "…"}`.

| Where | Status | When | `Retry-After` |
|---|---|---|---|
| Every authenticated route | `503` `cannot check credentials right now; retry shortly` | A credential, project or membership lookup failed or ran past 5 s (#1) | `1` |
| `POST /v1/traces`, `POST /api/public/otel/v1/traces` | `413` `this export carries N spans; the server takes at most M per request (TRACEPAD_MAX_SPANS_PER_REQUEST)` | More spans than the setting (#10) | — |
| Same | `503` `storage is temporarily unavailable; retry shortly` | A database condition (#2) | `1` |
| Ingest, every JSON API route with a body, `PUT /api/public/media/{id}/upload` | `429` `the server is holding as many request bodies as it can; retry shortly` | The body budget is spent (#13) | `1` |
| Every non-public `GET` route | `503` `the server is busy; retry shortly` | No read slot before the deadline (#16) | `1` |
| Same | `503` `the read took longer than 20s and was stopped; narrow the time range or the filters` | The deadline ended the read (#15); the number is the setting | — |
| `GET /api/v1/traces`, `/traces/last`, `DELETE /api/v1/traces`, `POST /api/v1/queues/{name}/items/from-traces` | `400` `tag: at most 50 values` | More than 50 distinct `tag` values (#17) | — |
| Every JSON route | `500` `failed to render the response` | The response did not encode (#3); never a `200` with an empty body | — |

`GET /api/v1/traces/{id}` and `GET /api/v1/traces/last` gain
`"observations_omitted": <int>`, present only when the tree was cut (#18).
The MCP tools `get_trace` and `get_last_trace` declare it in their output
schemas.

Unchanged: `401` for an unknown credential; `413` for a body over
`TRACEPAD_MAX_BODY_BYTES`; `429` for a full writer queue.

## Data contract (migration 0025)

No schema change. `0025_repair_numbers.sql`, in outline — `COUNTED` stands
for Decision 4's cost expression over `o.cost_details`, `UNCOUNTED_TOKENS`
for "one of the three token classes is present in `o.usage` and outside
Decision 4's range", `NOW` for
`CAST(unixepoch('subsec') * 1000000000 AS INTEGER)` and `MAXREAL` for
`1.7976931348623157e308`:

```sql
-- Traces whose total is infinite, or whose observations carry a cost the rule no longer counts.
UPDATE traces
   SET total_cost = (SELECT CAST(SUM(COUNTED) AS REAL) FROM observations o
                      WHERE o.project_id = traces.project_id AND o.trace_id = traces.id
                        AND o.provided_cost = 1),
       updated_at = NOW
 WHERE abs(total_cost) > MAXREAL
    OR (project_id, id) IN (SELECT project_id, trace_id FROM observations o
                             WHERE provided_cost = 1 AND COUNTED IS NULL
                               AND json_extract(cost_details, '$.total') IS NOT NULL);
-- Traces whose token counts the rule no longer counts: only their hours move.
UPDATE traces SET updated_at = NOW
 WHERE (project_id, id) IN (SELECT project_id, trace_id FROM observations o
                             WHERE UNCOUNTED_TOKENS);
-- Sums no row can produce. The pass rewrites the hours it can recompute;
-- a frozen hour keeps NULL, which is "no data".
UPDATE stats_hourly SET total_cost = NULL WHERE abs(total_cost) > MAXREAL;
UPDATE stats_hourly SET input_tokens = NULL
 WHERE input_tokens < 0 OR input_tokens > 1000000000 * count;   -- and output, cache_read
UPDATE users_hourly SET total_cost = NULL WHERE abs(total_cost) > MAXREAL;
UPDATE users        SET total_cost = NULL WHERE abs(total_cost) > MAXREAL;
```

Stamping `updated_at` makes the next aggregator pass re-roll every hour the
stamped traces' observations start in (spec 013 #15, #16), and with them the
users' summaries those hours touch (spec 023 #3).

## Testing

Each item names the test that fails on the code before this spec.

**PR 1**

1. *Lookups (#1).* With a transaction holding the write lock, as the
   writer's commit does, and the pool forced to open a new connection, a key
   lookup answers within 1 s. Fails before: the new connection waits
   `busy_timeout` for the write lock and the request is `401`. A store whose
   lookups fail (a closed database) makes an ingest request `503` with
   `Retry-After: 1`, and a session request `503`, not a sign-out.
2. *Storage classes (#2).* A writer seam returning `SQLITE_FULL` makes an
   export `503` with `Retry-After`; one returning a constraint error keeps
   `500`.
3. *Encode first (#3).* `writeJSON` handed a value that cannot encode (an
   infinity inside a nested map, which the backstop of #7 does not reach):
   `500` with a JSON body. Fails before: `200`, empty body. Every golden
   response of the server suite is byte-identical.
4. *Counting (#4–#6).* Ingest `cost_details` `{"input": 1e308, "output":
   1e308}` → `200`, stored without `total` (fails before: `500`). Two
   observations with `total` `1.5e308` → the listing, the trace, `/stats`,
   `/users` and `/sessions` are `200` with valid JSON and no cost for that
   trace (fails before: empty `200`). A `total` of `"abc"` → the next pass
   rolls the hour and advances the watermark, and `DELETE /traces/{id}` of
   that trace succeeds (fails before: the pass fails every time and the
   delete is `500`). Usage counts of `1e300` on two observations →
   `/stats?group_by=day` answers with that class `null` in the live half and
   the rolled half alike (fails before: `500 integer overflow` live, a
   negative sum rolled). A completion start of `-9223372036854775808` → `200`
   and `ttft_ms` absent (fails before: `500`). An RFC 3339 completion start in
   the year 3000 lands in metadata. `addCount` at `MaxInt64` holds.
5. *Backstop (#7).* An object carrying `math.Inf(1)` in a `float64` and a
   `*float64` field renders both as `null`.
6. *Rollup isolation (#8).* A seam failing the roll of one hour: the dirty
   loop rolls the others, `last_pass` stays, the next pass retries the hour;
   in the forward roll the watermark stops at the failing hour, not before
   the hours rolled ahead of it in the pass. Fails before: nothing of the
   project's pass is kept.
7. *Repair (#9).* A database rolled and then left as an earlier version
   left it, migration 0025 not yet applied, with an infinite
   `total_cost`, a text `total`, an infinite frozen `stats_hourly` cell and a
   negative token sum: after migration 0025 and one pass, every number is
   the one a fresh ingest of the same spans produces, and the frozen cell is
   `NULL`.

**PR 2**

8. *Span cap (#10).* `cap` spans → `200`; `cap + 1` → `413` with the
   message, nothing stored, `rejected_batches` counted. Fails before: `200`.
9. *Slices (#11, #12).* A commit seam records rows per transaction: an
   export of 20,000 spans commits in transactions of at most 2,000 rows (a
   slice and the job that crossed the line); an export of one span from a
   second project, sent while the first is committing, is answered before
   the first finishes. Fails before: one transaction of 20,000 rows, and the
   second export waits for all of it. A failure injected in the third slice
   answers `503`, leaves the first two slices' rows and no raw body, and the
   retried export converges on the rows of an uninterrupted one with one raw
   body.
10. *Budget (#13).* With the budget at twice the body cap and the writer held
    by a seam: two exports of a full cap are admitted, a third — plain or
    gzip — is `429` with `Retry-After: 1` after reading at most one step past
    what fit; releasing the seam admits it. A JSON API body and a media
    upload count against the same budget. A budget below the cap refuses to
    start. Fails before: all three are read.
11. *Labels (#14).* A 20,000-character trace name, user id and model are
    stored at 1,000 characters, the raw body keeps them whole, a remap
    produces the same rows; 200 tags with duplicates keep the first 50
    distinct in order.

**PR 3**

12. *Deadline (#15).* With `TRACEPAD_READ_TIMEOUT=200ms` and a trace listing
    filtered by ten tags over 50,000 synthetic traces: `503` with the
    deadline message within the timeout plus a small margin, and the next
    request finds a free connection. Fails before: the scan runs to its end.
    A client that disconnects mid-read releases its connection.
13. *Slots (#16).* With two slots held by a blocking seam, a third read is
    `503` "busy" at its deadline; an ingest request and a key lookup still
    succeed meanwhile. Fails before: the third read runs. With one slot, a
    client that stops reading a 10 MiB media body after its headers does not
    keep the next read from being served.
14. *Tag cap (#17).* 50 distinct values → `200`; 51 → `400`; 60 copies of
    one value → `200`; 1,000 values → `400`. Fails before: 1,000 values is
    `500`.
15. *Tree (#18–#20).* A chain of 6,000 spans: `200`, every span present
    exactly once, the response's JSON nesting at most 2 × 100 + a few levels,
    rendered within a small multiple of the time a flat trace of 6,000 spans
    takes (a ratio, not a wall-clock number, so CI speed does not decide it).
    Fails before: `200` with an empty body.
    10,001 observations → 10,000 rendered and `"observations_omitted": 1`;
    a trace of 50 observations whose `usage` maps are 1 MiB each stops at
    32 MiB with the count. A parent left out renders its child at the root.
    `?expand=io` over 2,000 observations with 1 MiB inputs: a counting seam
    on the payload reader is never asked for a payload while an earlier one
    is still held, and the answer is byte-identical to the one built the old
    way (fails before: all 2,000 are held at once). Playwright: the interface shows the notice on such a
    trace; live check in a browser.
16. *System (#21).* The gauges move while a seam holds a body and a slot;
    each refusal above increments its counter in its own project only.

## Edge cases

- **A Collector batching without a maximum** can send one export over
  20,000 spans after a burst; it gets `413` and logs a permanent error. The
  docs name `send_batch_max_size` (or the exporter's batch `max_size`) and
  the setting.
- **A request that fails mid-slice** leaves its earlier slices visible until
  the retry rewrites them; readers already see traces fill in while their
  spans arrive, so this is not a new state.
- **Gzip bodies** reserve budget as they inflate, so a compressed export
  costs its decompressed size, exactly as spec 002 #27 counts it.
- **A body that trickles in** holds only what it has sent, and the server's
  read timeout ends it.
- **A retried export that was over the span cap** is refused again: `413`
  is not retried by exporters, and nothing about it changes on retry.
- **Budget spent by one project's burst** turns other projects' exports into
  `429` for as long as it lasts; exporters retry, and fairness between
  projects is out of scope (below).
- **A read slow only because it waited for a slot** gets the "busy" message
  with `Retry-After`; one stopped while running gets the "narrow it"
  message without.
- **A deadline in the middle of a query** interrupts the statement while it
  is still in its first step, which is where aggregates and sorts do their
  work; a scan that is already returning rows stops after the row in
  progress, when `database/sql` closes them.
- **A slow download** — a large media body, a large tree — holds its
  connection but not its read slot, which is given back at the status
  (#16).
- **`/observations/{id}/io`** stays budget-exempt (spec 004 #3); its payloads
  are bounded by the body cap, and it runs under the read deadline and a
  slot like every read.
- **A score value near the largest double** is accepted as today; a mean or
  a delta that overflows renders `null` (#7), and bounds on score values
  belong to the scores spec.
- **Labels stored before this spec** keep their length. Only a hostile
  client writes a label past 1,000 characters, and deleting its traces is
  the repair.
- **A trace whose first 10,000 observations do not include its root** shows
  the ones it has, orphans at the root; the listing row's
  `observation_count` still counts all of them.
- **Tags that repeat** in an export count once toward the 50.
- **Background work** — the sweeper, the rollup, the search backfill — runs
  under its own context and is limited by neither the read deadline nor the
  read slots.

## Config additions

| Env | Default | Meaning |
|---|---|---|
| `TRACEPAD_MAX_SPANS_PER_REQUEST` | `20000` | Spans one export may carry; over it the answer is `413` (#10). At least `1` |
| `TRACEPAD_BODY_BUDGET_BYTES` | 4 × `TRACEPAD_MAX_BODY_BYTES` | Decompressed request bodies held at once; over it the answer is `429` (#13). At least `TRACEPAD_MAX_BODY_BYTES` |
| `TRACEPAD_READ_TIMEOUT` | `20s` | Deadline of one read request, its wait for a slot included (#15). At least `1s` |
| `TRACEPAD_READ_CONCURRENCY` | 2 × `GOMAXPROCS`, at least `4` | Reads served at once (#16). At least `1` |

## Out of scope

- **Fairness between projects**: per-project shares of the body budget, the
  writer queue or the read slots, and token-bucket rate limits. The bounds
  here stop one request from costing everyone seconds and stop the process
  from running out of memory; dividing capacity between tenants needs
  traffic to size it and belongs to the rate-limiting spec deferred since
  spec 002.
- **Transport limits** of the HTTP server itself — write deadlines,
  connection caps.
- **Moving payload encoding and compression out of the write transaction**,
  which would shorten every slice further.
- **Caps on the arrays of the JSON write routes** (scores, dataset items):
  bounded by the body cap and the budget here, sized by their own specs.
- **Caching credential lookups** in memory.
- **The cost of one rollup job**, which grows with the rows of its hour.
- **Heavy reads inside writes**: the queue fill from a listing filter
  (`POST /api/v1/queues/{name}/items/from-traces`) and the dry run of
  `DELETE /api/v1/traces` scan like a listing and run under neither the read
  deadline nor a read slot. A write refuses differently — a delete goes in
  rounds, a fill is capped by `limit` — so bounding them is a decision of its
  own.
