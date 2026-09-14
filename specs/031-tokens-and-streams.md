# Spec 031 — Tokens in the statistics, streams in the SDK, and where a price comes from

**Status:** 📝 DRAFT
**Sprint:** September 2026

> The store records the price a provider charged and never estimates one
> (spec 002 #14), the Python package reads that price from an
> OpenAI-compatible answer (spec 017 #5), and the mapping accepts it from
> any instrumentation that puts `gen_ai.usage.cost` on a span. What is
> missing is around that: the statistics know cost and latency but not a
> single token, so the one number every provider reports — and the only
> one a user of a provider without a price has — is nowhere in the charts;
> a streamed answer has to be stitched back together by hand before
> `end(response=…)` can read it; and a user of some other instrumentation
> has no page telling them how to get a price onto their spans. This spec
> closes the three, and records why the fourth — asking OpenRouter for the
> price after the fact — is not built.

---

## Overview

Deliverables, one PR per area, in this order (the last commit of the
last PR flips the status):

- **Server** — `stats_hourly` carries three token sums per cell, the
  aggregator fills them, the live scan computes them, and
  `GET /api/v1/stats` reports them per bucket (Decisions 1–5).
- **UI** — a **Tokens** chart on the Stats screen beside Cost, token
  totals in the summary line and a tokens column in the three breakdown
  tables (Decision 6).
- **SDK** — `call.stream(chunks)`: a pass-through over a streamed
  OpenAI-compatible answer that stamps the first token, gathers the
  output, reads the usage and the cost from the chunk that carries them,
  and ends the generation when the stream does (Decisions 7–8).
- **Docs** — a section in `docs/ingest.md` on where a price comes from
  for each way of exporting spans, and what to do when there is none
  (Decision 9); `docs/api.md` and `docs/sdk-python.md` updated for the
  above.

Not here: a price table (spec 002 #14 stands); fetching a price from
OpenRouter's generation endpoint after the span landed (Decision 10 says
why); tokens on the trace listing, its filters, the session page or the
per-user rollup (`users_hourly`); reasoning and cache-creation counts in
the statistics (they stay on the observation, where they are shown
today).

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-14** — Three token sums per statistics cell: **input**, **output** and **cache read**. Each is the sum over the observations in the cell of one count read from the observation's `usage` object under the first key present of a closed list: input ← `input_tokens`, `prompt_tokens`, `input`; output ← `output_tokens`, `completion_tokens`, `output`; cache read ← `cache_read_input_tokens`, `cache_read_tokens`, `input_cached_tokens`. The three lists are one constant in the store, used by the rollup and the live scan alike, and grow only by a Decision here | The store keeps usage keys as sent (spec 002 #19, spec 030 #1), so the same fact arrives under a Langfuse spelling, a `gen_ai.usage.*` suffix or a bare key, and the statistics have to pick a spelling per class or show three columns for one number. A closed list in one place is spec 030 #2's rule applied to reading: every key the statistics understand is named, and a collision with somebody's unrelated `input` is visible rather than a prefix scan's accident. Reasoning and cache-creation counts are not summed: reasoning is inside `output` for most providers and beside it for some, so a sum would double-count or under-count depending on who sent it, and cache creation is a fact about one provider's billing that the observation panel already shows. |
| 2 | **2026-09-14** — Tokens are an **observation** fact, summed into every cell the observation's trace falls in: the model cell (observation unit) and the trace-unit cell of the same environment and release. The `traces` table gains no token columns; the trace listing, its filters and the session page do not learn about tokens | `traces.total_cost` is maintained incrementally in the ingest transaction because the listing sorts and filters on it (spec 002 #7); nothing lists or filters on tokens, so a trace-level column would be a write on every span for a number only the statistics read, plus a migration that backfills it from every observation in the store. The rollup already walks the hour's observations to build the model cells and the live scan already joins them for the model grouping; adding the trace-unit cell to the same walk is the whole change on the rolled side, and one more aggregate over the same join on the live side. |
| 3 | **2026-09-14** — Migration **0018** adds `input_tokens`, `output_tokens` and `cache_read_tokens` to `stats_hourly`, `INTEGER` and nullable: NULL when nothing in the cell carried that count, which is not the claim that it carried zero. The migration ends with `UPDATE stats_rollup SET last_pass = 0` (spec 023 #15's backfill), so the first pass after the upgrade re-rolls every hour the raw data still holds. **Known limit:** an hour past the project's `retention_days` is frozen (spec 013 #14) and keeps NULL tokens for ever — its observations are gone | The same shape as `total_cost` for the same reason (spec 002 #14): a cell whose calls reported no usage must not chart as zero tokens beside a cell that reported few. The backfill is the one every rollup addition has used (0013, 0015, 0016) and costs one pass over the retained hours. The frozen hours are the price of the rollup outliving the raw data, and the limit is the one spec 027 #17 records for `names_hourly`. |
| 4 | **2026-09-14** — `GET /api/v1/stats` buckets gain a `tokens` object: `{"input": n, "output": n, "cache_read": n}`, each key present only when something in the bucket carried that count, and the object absent when none of the three is. Same on every grouping and both units; `openapi.json` and the generated UI schema follow | Three keys under one name rather than three top-level `*_tokens` fields because the bucket already groups its latencies that way, and because "no data" on the whole class is one absence to test rather than three. Absent rather than zero is the API's rule for cost and the rows behind it (`docs/api.md`, "a range with nothing in it comes back with no buckets rather than fabricated zeroes"). |
| 5 | **2026-09-14** — The live scan computes the trace-unit token sums with one additional aggregate query over `observations` joined to `traces`, grouped by the same bucket key as the trace scan, and merges it into the buckets by key; the model grouping reads the three counts in the row it already has. The rollup's `rollHour` reads the three counts in the observation query it already runs and adds them to both cells | The live scan is over the hour in progress and a bounded tail (spec 013 #5), so a second query over that window is milliseconds; a correlated subquery per trace row in the existing scan would be the same rows read once per trace instead of once. Where the seam merges the two halves nothing changes: both produce cells with the same three nullable sums, and the bucket adds them the way it adds cost — a NULL contributes nothing and does not make the sum zero. |
| 6 | **2026-09-14** — The Stats screen gets a **Tokens** chart between Cost and Latency with three lines — input, output, cache read — formatted with `count`; the summary line gains `N tokens` (input + output, cache read excluded); the three breakdown tables gain a **Tokens** column showing input + output for the row, `—` when the row carried none. `buildSeries` gains three series with the same null discipline as `cost`: a bucket with traffic but no tokens is a gap, not a zero | Input and output are what a bill is made of; cache read is what explains a bill that is smaller than the tokens suggest, so it is on the chart and out of the headline number, where adding it would count the same tokens twice for the providers that report cached tokens inside the input. A column per class in the tables would widen them past a phone; one number plus the chart is what a reader compares models by. Lines: the chart and the series are about sixty application lines against a budget with a few hundred to spare (`make ui-lines`). |
| 7 | **2026-09-14** — `Generation.stream(chunks)` in the Python package: takes an iterable of OpenAI-compatible stream chunks and returns an iterator that yields each chunk unchanged. On the first chunk that carries content it stamps the first token (`first_token()`, idempotent as today); it concatenates `choices[0].delta.content` strings into the output; it keeps the `model` of any chunk that names one and the `usage` of the chunk that carries it (the last, on every provider that sends one); when the iterator is exhausted, closed, or leaves by exception it calls `end(response=…)` with what it gathered — the same reader, the same table of fields, `usage.cost` included — unless `end` was already called. Explicit arguments given to a later `end` win as they do today; a stream that never carried `usage` records the model and the output, as the non-streamed case already does. Async streams get the same in `astream` | The reader in spec 017 #5 already works on any object with a `usage`, so a caller could hand it the last chunk — but only after collecting the chunks themselves, remembering which one had the usage, and joining the deltas, which is the code every caller would write and get slightly wrong. A wrapper that yields the chunks through is what a streaming caller can drop in without changing what they do with the chunks. Ending on exhaustion is what makes `for chunk in call.stream(response):` complete: the generation ends when the answer does, and a `with` block around it still ends the span if the stream is abandoned. |
| 8 | **2026-09-14** — Whether a stream carries usage is the provider's choice and the package does not make it: `docs/sdk-python.md` tells the reader to pass `stream_options={"include_usage": True}` to an OpenAI-compatible client and `extra_body={"usage": {"include": True}}` to OpenRouter (the cost rides in the same final chunk), and says what happens without them — a generation with a model and an output and no usage, the way the doc already describes for a stream's last chunk. `@observe(type="generation")` over a generator function keeps recording the list of chunks as the output and reading nothing (spec 017); the streaming reader is the explicit helper, not the decorator | The package does not own the request, so it cannot add the option; what it can do is say so in the one place a streaming caller looks. Changing what the decorator does with a generator would change a shipped contract for the callers who rely on it, for a case the explicit helper serves better — a decorator cannot yield the chunks through to a caller who is not there. |
| 9 | **2026-09-14** — `docs/ingest.md` gets a **Where the price comes from** section under "What Tracepad reads from your spans": one paragraph per way in — the Python package (read from the answer, or `cost=`), a Langfuse SDK (`cost_details`), your own OpenTelemetry spans (`gen_ai.usage.cost`, set on the span before it ends, with a five-line Python example), and a third-party auto-instrumentation (the span is closed by the time the answer is in your hands, so the price can only land there if the instrumentation itself reads it — name that, and point to the package as the alternative). It closes with what the store does when there is no price: `total_cost` absent, the trace shows *no data*, and the statistics chart tokens instead (this spec) | Every mechanism in the section exists today; what does not exist is the page that says which one applies to a reader, and the one fact they will hit — an auto-instrumented span cannot be priced after the call returns — is the one a reader cannot find out by reading the mapping table. Written as recipes because the alternative, a server-side fetch, is the thing Decision 10 refuses. |
| 10 | **2026-09-14** — **Not built: fetching the price from OpenRouter after the fact.** OpenRouter's `GET /api/v1/generation?id=` reports a call's exact charge given its generation id, which arrives as `gen_ai.response.id` on a span with no price only from an instrumentation that was not told to read `usage.cost`. Filling it in from the server would mean an OpenRouter key stored per project, a background fetch with its own retries and rate limits, a delay before the generation is queryable, and a rule for a span whose id is not OpenRouter's. It stays out until a live case asks for it; the recipe of Decision 9 is the answer meanwhile | The price is in the answer already for every caller that asks for it (`usage.cost` comes back without opting in on a non-streamed OpenRouter call, and with `usage.include` on a streamed one); a server-side fetch would serve the case where the caller did not ask, at the cost of a credential the server has no other use for and a write path that revises a landed observation. Recorded here so the question is not re-opened without the case. |
| 11 | **2026-09-14** — A `user_id` answer carries no `tokens`, on any grouping: the object is absent for every bucket, and `openapi.json` and `docs/api.md` say so. Refines Decision 4's "same on every grouping and both units" for the one filter whose rolled half is a different table | The per-user rollup (`users_hourly`) is out of this spec's scope and holds no token sums, while the live scan behind the watermark could compute them for a user as easily as for a project. Answering tokens from the live half alone would be a timeline whose tokens appear at the watermark and vanish behind it — the seam made visible, which spec 013 #5 forbids. Absent everywhere is the honest answer until `users_hourly` learns the three columns, which is a spec of its own. |
| 12 | **2026-09-14** — Tokens are a fact about a call to a model; a span with usage and no model is not one. Both the rollup and the live aggregate read the observations that name a model — the rows the model cells are made of — so an observation that carries `usage` and no `model` is in neither the model cell nor the trace-unit cell | Decision 5 says which query the counts are read in, and that query is the model grouping's; this makes the consequence explicit rather than an accident of the join. A usage on a span that names no model is, when it happens, a parent's sum over the generations beneath it, and adding it to the trace-unit cell would count those tokens twice. The rule is pinned by a store test so a change to it is a Decision, not a drift. |

## Application contract

- `GET /api/v1/stats` — every bucket may carry `tokens`
  (`{input?, output?, cache_read?}`); absent when nothing in the bucket
  reported a count, and always absent with `user_id` (Decision 11). `unit`
  and the groupings do not change.
- Stats screen — a **Tokens** chart (input, output, cache read); the
  summary line reads `N traces · N with errors · $X · N tokens`; the three
  breakdown tables carry a **Tokens** column. A bucket or row without
  tokens is a gap on the chart and a `—` in the table, never a zero.
- Python package — `call.stream(chunks)` and `await`-able `call.astream(chunks)`
  yield the chunks through and end the generation with the gathered
  model, output, usage and cost when the stream ends.
- The observation panel and the trace listing do not change.

## Testing

- Store: an hour with observations carrying usage under each spelling of
  Decision 1 rolls to the expected three sums in the model cell and in
  the trace-unit cell; an observation with no usage contributes NULL, not
  zero, and a cell with none stays NULL; the live scan over the same rows
  yields the same sums per bucket for every grouping; the seam merges a
  rolled half with tokens and a live half without into a bucket that
  carries the rolled sums.
- Migration: a database rolled before 0018 has `last_pass = 0` after it
  and the next pass writes tokens for every retained hour; a frozen hour
  keeps NULL (the frozen-hour test of spec 025 #21, one column further).
- API: the `tokens` object is absent on a bucket with no counts and
  carries only the keys that have them; `openapi.json` validates against
  the answer.
- UI: `buildSeries` unit tests for the three series (null on a bucket
  with no tokens, aligned with `x`); the summary line and the breakdown
  column render `—` for none; one e2e case on the Stats screen with the
  fixture's tokens in both projects.
- SDK: unit tests over a hand-written chunk list — first token stamped on
  the first content chunk and only once, deltas joined, `usage` taken from
  the last chunk, `usage.cost` written, model taken; a stream without
  `usage` records model and output; `end()` called explicitly mid-stream
  wins and the wrapper does not end twice; abandoning the iterator ends
  the span; the async twin over an async generator.
- Docs: the anchor checker passes; the `docs/api.md` example is the
  server's real answer for the fixture.
- Live: the demo data on a scratch server shows the Tokens chart with
  three lines and the model table with the column; one streamed call
  through the package against any OpenAI-compatible endpoint lands with
  usage on the observation.

## Out of scope

- A price table or any estimate of cost — spec 002 #14.
- Tokens on the trace listing (`min_tokens`, a column), the session page,
  the user page and `users_hourly`.
- Reasoning and cache-creation tokens in the statistics.
- Fetching the price from OpenRouter's generation endpoint — Decision 10.
- Reading a streamed answer through `@observe(type="generation")` on a
  generator — Decision 8.
