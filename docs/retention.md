# Retention

A store that only grows is a store you eventually delete by hand. Tracepad
forgets on a schedule, on request, and when a project goes — without an
operator ever running SQL. One exception is chosen rather than scheduled: a
trace that belongs to a **live eval run is kept past the window**, because the
run is the evidence for a number somebody will act on, and the run you want
to look at is the old one. Retention bounds the file *plus what you chose to
keep*; `GET /api/v1/system` shows the size of that choice, and deleting the
run releases it. See [What a run keeps](#what-a-run-keeps).

Nothing is deleted by default. A new install keeps everything forever until
somebody sets a window, because a self-hosted tool must not quietly discard the
data of an operator who configured nothing. At the reference rate of a busy
single-app deployment — roughly 22 MB a month — that is the right default for
this audience, and the wrong one for a fleet, which is why the window is one
`PATCH` away.

That rate is what the default is sized for: a small deployment, where keeping
everything costs a few hundred megabytes a year. It is not sized for the top
of the envelope this is built to serve — around 10 GB of data on the disk, and
a million spans a day arriving — where that stock is spent in days and a window
is not optional but the thing that keeps the file a size your disk has. Those
two figures are a stock and a flow, and they are the README's:
[What it is built for](../README.md#what-it-is-built-for).

## The three windows

Each project has three, all counted in whole days (1 to 36500) and all
nullable:

| Setting | Applies to | `null` means |
|---|---|---|
| `retention_days` | Traces, and everything hanging off them: observations, payloads, scores, and the annotation-queue items that point at them | Keep forever (the default) |
| `raw_retention_days` | The stored OTLP bodies of `TRACEPAD_STORE_RAW` | Follow `retention_days` |
| `stats_retention_days` | The hourly statistics rollup, and the per-user one beside it ([users.md](users.md)) | Keep forever (the default) |

Setting `stats_retention_days` deletes the *stored summaries* past it; it does
not hide the traces. While the raw rows are still there, statistics for those
hours are computed from them on the fly, exactly as they were before the
rollup existed — slower, and correct. Once both windows have passed there is
nothing left to compute from, and the charts are empty because the data is.

```sh
tracepad retention show
tracepad retention set --days 90               # traces: 90 days
tracepad retention set --raw-days 14 --yes     # raw bodies: 14 days
tracepad retention set --stats-days 730 --yes  # statistics: two years
tracepad retention set --forever               # back to keeping everything
```

"Keep it essentially forever" is spelled `--forever`, not a very large number
of days: a window is turned into a nanosecond cutoff, so the day count is
capped at 36500 rather than allowed to overflow into a date in the future —
where it would match every row there is.

Raw follows the parsed window by default rather than being shorter, because raw
is the insurance policy: it is what makes a mapping bug retroactively fixable
and what [`tracepad export --otlp`](export.md) replays. A default that expired
it sooner would silently cap all of that. Operators for whom the raw bodies are
the heavy or the sensitive part shorten them deliberately.

**`raw_retention_days` is the export's reach.** A trace older than that window
still exists — its rows are in the database and every screen shows it — but the
body it arrived in is gone, and an export replays bodies. Shortening this window
is therefore shortening how far back the data can leave whole; nothing else
about the trace changes. `GET /api/v1/system` reports the archive's size and its
two ends beside `traces_before_window`, which is how many traces already fall
outside it, and `tracepad export --otlp --dry-run` prints the same before
sending anything. Read those before shortening this window, not after.

A batch is swept by its own age and never because the traces it fed were swept:
one export body feeds many traces with different fates.

## What outlives what

The statistics are **not** deleted with the traces they summarize. That is
deliberate: a month of history is a few thousand rows where the traces behind
it are millions, and deleting the cheap thing along with the expensive one is
what used to make configuring retention silently amputate the charts.

So a project with `retention_days = 30` keeps answering `/api/v1/stats` about
last year — counts, errors, cost and latency percentiles — while the traces
behind those numbers are long gone. An operator who means "no *record* older
than 30 days", which is a different promise, sets `stats_retention_days` too.

Two consequences follow, and both are stated here rather than left to be
discovered:

1. **A frozen hour cannot be corrected.** Once an hour is older than
   `retention_days`, its raw rows are gone by design, so the rollup stops
   recomputing it — a single late fragment arriving for that hour would
   otherwise replace five thousand summarized traces with itself. The hour's
   stored numbers stand as the archive of what was there.
2. **Erasing a user's data, and deleting traces, corrects the hours it can
   reach.** The rolled hours the erased or deleted traces occupied are
   recomputed in the same transaction that deletes them — both run in chunks
   of up to five hundred traces of one hour, and each chunk commits with its
   hour already corrected — so the counts drop before the request answers,
   and a request cut off between chunks leaves no hour counting traces that
   are gone. Hours already frozen are not recomputed: the aggregates
   carry no user id, no name and no text — they are counts, sums and latency
   buckets — which is the same archive posture the raw bodies have below, and
   the same reasoning regulators accept for a backup.

   The **per-user** rollup is the exception to that exception. Those rows are
   about the user by construction, so an erasure deletes them outright rather
   than recomputing them — in every hour, frozen ones included, where a
   recompute could not have run at all. The user leaves `/api/v1/users`
   immediately; only the project-wide totals for a frozen hour go on counting
   the traces.

The rollup is **four tables**, and one window governs all of them:

| Table | Holds | Swept by |
|---|---|---|
| `stats_hourly` | traffic, errors, cost, tokens and latency per hour | `stats_retention_days` |
| `users_hourly`, `users` | the same per end user, plus their summary | `stats_retention_days` |
| `scores_hourly` | score means, rates and category counts per hour ([quality.md](quality.md)) | `stats_retention_days` |
| `names_hourly` | how many traces of each name per hour, and how many failed — what [`GET /api/v1/facets`](api.md#filter-values) lists | `stats_retention_days` |

`names_hourly` is the youngest of the four, and on an upgraded install it starts
where the *traces* do rather than where the other three do: its backfill reads
the raw rows, and past `retention_days` there are none to read. If your
`stats_retention_days` is the longer of the two, the filter values for that gap
name the environments and releases and not the trace names.

They are written by one pass in one transaction and swept together, so the
dashboard, Users, Quality and filter-value answers can never disagree about
how far back the history reaches.

One consequence shows on the dashboard's summary row, whose change is
measured against the **previous window** of the same length
([ui.md](ui.md#dashboard)): with `stats_retention_days` shorter than twice
the window on screen, the previous window is partly or wholly swept, the
change is against what remains, and a previous window with nothing left
reads *new* rather than a number.

**Freezing is asked of each of the four separately**, because what a freeze
protects is the rows that already stand. An hour past the window whose rows a
table already holds is left alone, exactly as the statistics are. An hour it
holds *nothing* for has nothing to protect, so the roll may still write it —
and what it writes is whatever the raw rows say. Usually that is nothing: past
the window the sweep has taken the traces, and the scores and the per-user
traffic with them. But the window is measured against the client's timestamp
while the sweep deletes by *arrival*, so a year of history imported this
morning is "past the window" and completely intact — and it gets its per-user,
score and trace-name rows rather than a permanent gap.

## What an annotation queue keeps

Nothing. A queue item is a pointer at a trace ([annotation.md](annotation.md)),
and the sweep that deletes the trace deletes its items in the same job — a
pointer to a deleted trace is a desk showing an empty page. The queue itself
stays, with a smaller list.

The **verdicts** are not items and do not go with them: they are scores on the
trace, and they expire on their own timestamps like every other score, which
here means with the trace they are about. Deleting a queue by hand takes its
items and nothing else, for the same reason — the scores are the work, and the
queue was only the list of what to do.

## What a run keeps

A trace whose `run_id` names an existing run of its project
([datasets.md](datasets.md)) is **not swept**, however old it is. The sweep
skips it; the retention dry run leaves it out of its counts, so a preview
never promises to take what the pass would keep; and the retention window can
be shortened below the age of the pinned traces without touching them. When
the file does not shrink after a window change, this is where to look:

```json
"runs": {"pinned_traces": 2996, "orphan_traces": 0}
```

The pin is per trace and per run, not per dataset: eval traffic is hundreds of
traces per pass, not a million a day, and its payloads are worth their disk.
The size is a choice the operator makes per run rather than a window that
quietly amputates the baseline.

What releases a pinned trace:

- **Deleting the run** (`DELETE /api/v1/runs/{id}`). The traces are not
  deleted; they return to the ordinary window and go on the next pass if they
  are past it. The response says how many.
- **Deleting the dataset**, which takes its runs with it.
- **Erasing a user's data**, which outranks the pin: the user's traces go
  whether or not a run holds them, and the dry run names the runs affected.
  An eval trace ordinarily carries no user id, but a harness that replays
  production sessions under their real ids is exactly the harness that will
  receive the request.

What is not pinned:

- **Raw bodies.** A batch feeds many traces with different fates, and raw is
  the archive, not the evidence. After the raw window a pinned trace keeps its
  parsed rows and loses its raw body.
- **Orphans.** A trace naming a run that does not exist — a typo, or a run
  whose dataset was deleted — is stored and counted (`orphan_traces`), and
  lives on the ordinary window.

## The clock is arrival, not the client's

Retention counts from `ingested_at` — the server's clock when the trace row was
first created — and never from the `timestamp` a span carried.

`timestamp` is client bytes. A client whose clock is behind would have its
traces deleted on the next sweep; one that sends the future would buy itself
immortality. "How long do we keep what we received" is a question only our own
clock can answer.

Two consequences worth knowing:

- A trace's lease starts once. Later spans joining an existing trace do not
  refresh it, so a long-running conversation cannot outlive the window by
  staying chatty.
- A span that arrives after its trace was swept recreates a fragment, which
  then lives its own full window from *its* arrival. That is the honest answer
  for data we genuinely hold again.

## The sweeper

One goroutine, one pass an hour (`TRACEPAD_SWEEP_INTERVAL`), deleting per
project in chunks of about a thousand traces. Each chunk is a transaction
through the same group-commit writer that ingest uses, so a sweep serializes
with incoming exports for milliseconds at a time instead of holding a lock
against them.

Beside it runs the **statistics aggregator** (`TRACEPAD_ROLLUP_INTERVAL`,
five minutes), which rolls closed hours into the summary the charts read and
re-rolls the hours late spans touched. It writes through the same writer, in
jobs bounded to one hour of one project, and it deletes nothing except what
`stats_retention_days` says. Its first pass on an existing database is the
backfill: it walks from the oldest trace forward, logging progress, while
every query keeps being answered from the raw rows meanwhile.

Each sweep pass also:

- collects **orphaned payloads** — rows left behind when a re-delivered span
  overwrote its input, output or metadata with a new one;
- collects **orphaned search-index entries**, for the same reason: the three
  paths that delete observations take the index with them inside their own
  transactions, and this is the belt to those braces;
- **purges** projects whose seven-day deletion grace has run out (below);
- removes **expired browser sessions and invitation links**
  ([accounts.md](accounts.md)). This is housekeeping and not access control: a
  session that has run out stops working the moment it does, and an invitation
  past its seven days is refused, whether or not a pass has been by;
- runs an incremental vacuum, so the file on disk actually shrinks. Deleting
  rows without one returns nothing to the filesystem.

A retention change takes effect on the next pass, within the interval. There is
no run-now endpoint on purpose: an immediate sweep would be a destructive
trigger with none of the dry-run semantics the rest of the admin surface
insists on, and the hourly cadence *is* the margin in which a mistaken window
can be corrected before it costs anything.

What the sweeper has done is in `GET /api/v1/system`:

```json
"sweeper": {
  "enabled": true,
  "interval_seconds": 3600,
  "last_run": "2026-09-01T12:00:00Z",
  "next_run": "2026-09-01T13:00:00Z",
  "traces_deleted": 4120,
  "raw_batches_deleted": 96,
  "since": "2026-08-30T09:14:02Z"
}
```

The counts are this project's own and since this process started, like every
other counter that endpoint reports.

## What search costs

The full-text index ([api.md](api.md#search)) is the one store beside the
payloads themselves, and it is worth knowing what it is made of before a large
deployment upgrades into it.

**Each payload contributes the first 64 KiB of its text.** What is indexed of a
payload is the values inside its JSON, not the JSON around them
([api.md](api.md#what-is-and-is-not-matched)), and the 64 KiB is counted on
that text: a message array's keys and brackets cost the index nothing and take
none of the budget. The rest is stored and readable — `/observations/{id}/io`
still returns all of it — but not searched. This is a size decision, not a
quality one: a 500 KB document on one observation's input would otherwise cost
the index as much as a hundred ordinary traces, while the error messages,
refusals and answers people search for live in the first kilobytes. It is
written here rather than left to be discovered by a search that came back
empty.

**Ingest pays for it.** Measured on an Apple M1 Pro over a synthetic corpus of
one trace and twenty generations per batch, each carrying about 2.5 KB of
prompt, completion and metadata text: 3.7 ms per batch without the index and
7.2 ms with it — roughly 5 400 spans a second against 2 700. The index halves
the write path and leaves it two orders of magnitude above the ceiling this
product is built for.

**The first start after the upgrade builds the index for what is already
stored.** It runs before the server listens, because a search that answers
"nothing" because the index is half-built is worse than a start that takes a
minute, and it is resumable: a crash halfway carries on where it stopped. On
the same machine and the same corpus it indexes about **260 traces — 5 200
observations — a second**, so a store of a million observations spends roughly
three minutes there, once. The log says it is happening and reports progress.

**An upgrade that changes what a word is rebuilds the index the same way.**
Schema 0007 is such an upgrade — it is what moved the index off the JSON text
and onto the text inside it — so a store upgrading into it pays that first
start again, at the same rate, once. There is nothing to run and nothing to
decide: the start after the upgrade does it, and says so in the log.

## Deleting a user's data

```sh
tracepad users rm-data user-4711
```

`DELETE /api/v1/projects/{id}/users/{user_id}/data` erases everything the
queryable stores hold about one user — the traces filed under that id, their
observations, payloads, scores and the annotation-queue items pointing at them
— synchronously, and answers with the counts.
Like every destructive endpoint it is a dry run until confirmed; the echo here
is the user id itself. Traces an eval run is keeping go with the rest, and the
dry run lists those runs under `runs` so the hole is visible before it opens.

## Deleting traces

```sh
tracepad traces rm 4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f
tracepad traces rm --to 2026-09-17T14:02:17Z --env loadtest
```

`DELETE /api/v1/traces/{id}` and `DELETE /api/v1/traces?<filters>&to=` are
the operator's own door between the sweep and the project's deletion: one
trace by id, or every trace a listing filter matches before a moment
([admin.md](admin.md#deleting-traces)). They take exactly what an erasure
takes and by the same path — the traces, their observations, payloads,
scores, search entries and queue items, the hours re-rolled in the same
transaction, a run's pin overridden — so every promise on this page about
what outlives what holds for a deletion as it holds for an erasure. In
particular the raw OTLP bodies are **not** touched, for the structural
reason below: a raw batch holds many traces, and a trace cannot be cut out
of one. The preview says so.

A deletion removes what the store holds at that moment, and nothing is
remembered about the ids: a span that arrives afterwards for a deleted trace
creates the trace again from what arrived, as it would for a trace never
seen. The bulk form's required `to` makes that rare; for one trace whose
export is still in flight, wait for it to finish.

### What this means for a data-subject request

You are the controller; tracepad is the tool. Two things are worth stating
plainly rather than leaving to be discovered:

**Erasure covers the queryable stores immediately.** After the call, no read
endpoint, CLI command or MCP tool can return that user's traces, and no search
finds their text: the index is deleted in the same transaction as the rows.
This lands well inside the one-month response window Article 12(3) allows.

**The statistics are corrected where they can be.** The rolled hours the
erased traces occupied are recomputed in the same transaction that deletes
them, chunk by chunk, so they are right before the call returns and stay right
if the call is cut off; hours whose raw rows retention already took are frozen
and keep their totals. Those rows hold
no user id, no name and no text — see [What outlives what](#what-outlives-what).
The **per-user** rows, which do hold the id, are deleted outright in the same
request, frozen hours included.

**Raw OTLP bodies are not erased.** They are an archive: not served by any read
endpoint, not searchable, expiring on their own schedule — the same posture as
a database backup, which regulators accept. Two caveats follow from that, and
both are on you rather than on the tool:

1. The completeness of an erasure assumes a **bounded raw window**. With
   `raw_retention_days` unset and `retention_days` unset too, the raw bodies are
   kept forever, and so is the copy of the erased data inside them.
2. A future `remap` replays raw bodies into the parsed tables. Replaying a
   window that still contains an erased user resurrects their data.

If your deployment cannot live with either, run with `TRACEPAD_STORE_RAW=off`.
Nothing is archived, erasure is complete the moment it returns, and the price
is the insurance: a mapping bug becomes data loss rather than a replay away
from being fixed.

## Deleting a project

Deleting a project is the most irreversible thing in the product, so it is not
irreversible for a week:

```sh
tracepad projects rm <project-id>     # admin token required
```

- The keys stop authenticating **immediately**, and the project vanishes from
  listings.
- The data is destroyed by the sweeper **seven days later**, and the response
  says when.
- `tracepad projects restore <project-id>` undoes it until then, with the same
  credential that deleted it: an owner account or the admin token. A project's
  own key reaches neither — it lives in application config and in CI, and a
  leaked application credential must not move a project in either direction.
  The one thing such a key still reads while its project is deleted is the
  project itself, so whoever is about to restore can see what they are
  restoring and until when.
- The name stays reserved throughout, so restore always has its name to come
  back to. Creating a project with that name meanwhile is a `409` naming the
  restorable one.

Seven days, fixed, not a knob. A safety net whose size depends on how the
deployment was configured is a safety net nobody can rely on.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `TRACEPAD_SWEEP_INTERVAL` | `1h` | How often a pass runs. A Go duration; at least `1s`. |
| `TRACEPAD_ROLLUP_INTERVAL` | `5m` | How often the statistics aggregator runs, and so how long a closed hour waits before the rollup holds it. A Go duration; at least `1s`. |
| `TRACEPAD_STORE_RAW` | `on` | Keep raw OTLP bodies at all. `off` removes the archive caveats above. |

The windows themselves are per project and live in the database, so changing
them needs no restart. See [admin.md](admin.md).
