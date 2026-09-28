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

`retention show` reads the project, which any key may do. `retention set` is
`PATCH /api/v1/projects/{id}`, and a key needs the `write`
[scope](api.md#scopes) for it, as a person needs the `editor` role.

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

**Media follows its traces**, with no window of its own: an image or a file
ingest stored ([media.md](media.md)) lives while a trace or a raw batch points
at it, and every path below — the sweep, erasure, trace deletion, a project's
purge — deletes the refs of what it deletes and collects the bodies nothing
points at any more, in the same transaction. A body another project also sent
survives. The project setting `media: placeholder` is the answer for "do not
keep pictures at all".

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
   of up to five hundred traces in whole hours, and each chunk commits with
   its hours already corrected — so the counts drop before the request answers,
   and a request cut off between chunks leaves no hour counting traces that
   are gone. Hours already frozen are not recomputed: the project-wide
   aggregates carry no user id and no prompt or completion text — they are
   counts, sums and latency buckets, plus, in `names_hourly`, the **trace
   names** they count. Trace names are operation names by convention; a
   deployment that puts personal data into them should know a frozen hour
   keeps it.

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
- deletes the **session-only scores** past the trace window: a score that
  names a session and no trace, received more than `retention_days` ago, whose
  session no trace of the project carries any more — the one its traces were
  swept with, or one whose session never arrived. A session a pinned run keeps
  alive keeps its verdicts. The retention dry run counts them as
  `session_scores`;
- runs the **compaction** an explicit deletion asked for — an erasure, a trace
  deletion, a project's purge — when one is pending: the search index merged,
  the free pages drained, the write-ahead log truncated
  ([below](#what-this-means-for-a-data-subject-request));
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
tracepad users rm-data user-4711     # a key with write
```

`DELETE /api/v1/projects/{id}/users/{user_id}/data` erases everything the
store holds about the traces filed under one user id:

- the traces, their observations, payloads, scores, search entries and the
  annotation-queue items pointing at them, and the user's per-user
  statistics;
- the scores given to the **sessions** those traces belonged to — a score
  with a `session_id` and no `trace_id` ([scores.md](scores.md)) — including
  a session another user's traces share;
- every version of the **dataset items** cut from those traces — an item any
  row of which names one of them in `source_trace_id`
  ([datasets.md](datasets.md)) — each affected dataset's version advancing by
  one;
- the user's **spans inside the raw OTLP bodies**: every batch that held one
  is rewritten without it, or deleted when nothing else was in it, and the
  pictures only those spans pointed at go with them.

What it unlinks is overwritten in the file by the next sweep rather than when
the call returns, and a few things are out of its reach by construction;
[What this means for a data-subject request](#what-this-means-for-a-data-subject-request)
lists them. Like every destructive endpoint it is a dry run until confirmed;
the echo here is the user id itself. Traces an eval run is keeping go with the
rest, and the dry run lists those runs under `affected_runs`, and the datasets
that lose items under `affected_datasets`, so the hole is visible before it
opens.

**How the raw bodies are found.** No index leads from a trace to the batches
that carried it, so the erasure reads the batches that arrived inside each of
the user's traces' arrival windows — from the trace's first arrival to its
last update, the one clock both are stamped with — decodes each and checks it
for the user's spans; a batch holding none is left as it was. The dry run's
`raw.batches_to_scan` is that count. The order is raw first, then the parsed
rows, then the batches that arrived for the user's traces while the request
ran: once the parsed rows are gone nothing names the batches any more. A
rewritten batch is no longer what the client sent,
and says so: `scrubbed_at` on the listing, `X-Tracepad-Scrubbed-At` on the body
and `scrubbed` in the export summary ([export.md](export.md)). Nothing records
whose spans went.

**An erasure is accepted, then runs.** The confirmed request checks what can
refuse it, records the erasure and answers `202 Accepted` with where to watch
it; `?wait=` up to 30 seconds answers `200` instead when the erasure ends in
that time, which a user of a few traces does. The server runs one erasure at a
time, and its record says how far it is: the phase, and how many of the
traces it found are gone. Watch it with `GET
/api/v1/projects/{id}/erasures/{erasure_id}`, `tracepad users erasure <id>`,
or the Settings screen's erase card, which lists the erasures of the last 30
days. A **restart** does not lose it: every step commits its progress with
it, and the next start resumes the erasure from its phase — the batches that
arrived for traces it had already deleted included, which a repeat of the
request could not find. The **record forgets the user**: it holds the user id
only while the erasure is queued or running, clears it in the statement that
ends it, and is itself removed by the sweeper 30 days later. What stays in
between is that an erasure ran, when, and what it took.

## Deleting traces

```sh
tracepad traces rm 4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f     # a key with write
tracepad traces rm --to 2026-09-17T14:02:17Z --env loadtest
```

`DELETE /api/v1/traces/{id}` and `DELETE /api/v1/traces?<filters>&to=` are
the operator's own door between the sweep and the project's deletion: one
trace by id, or every trace a listing filter matches before a moment
([admin.md](admin.md#deleting-traces)). They take what an erasure takes of
the traces themselves, by the same path — the traces, their observations,
payloads, scores, search entries and queue items, the hours re-rolled in the
same transaction, a run's pin overridden — so every promise on this page about
what outlives what holds for a deletion as it holds for an erasure. What an
erasure takes *beside* the traces — the session scores, the dataset items, the
spans in the raw bodies — a deletion does not. Both void
the media upload URLs for the traces they remove, and refuse new ones, for the
hour such a URL lives ([media.md](media.md#the-langfuse-sdks-media-channel)): an upload in
transit for an erased trace would otherwise store its picture after the
erasure had answered. The bulk
form works in rounds of at most a thousand traces and fifty chunks of whole hours,
each chunk a transaction of its own, so a round cut off leaves nothing
half-deleted and the next request continues. In
particular the raw OTLP bodies are **not** touched, for the structural
reason below: a raw batch holds many traces, and a trace cannot be cut out
of one. The preview says so. A picture a deleted trace pointed at goes with
it unless a raw batch still points at it, and then when that batch expires
([media.md](media.md#how-long-they-are-kept)); the preview counts the bodies
the project would stop holding as `media` and `media_bytes`.

**The first start after the upgrade builds four indexes** over the columns
that reference stored payloads (migration 0020): deleting a payload is a
foreign-key check in each of them, and without the indexes every check was a
scan — the reason erasure and the sweep were slower than they needed to be.
The migration runs before the server listens, once; on a store of a few
hundred thousand observations it is seconds, and the log names the migration
as it runs, after the backup every migration takes beside the database — kept
seven days ([what that means](#what-this-means-for-a-data-subject-request)).

A deletion removes what the store holds at that moment, and nothing is
remembered about the ids: a span that arrives afterwards for a deleted trace
creates the trace again from what arrived, as it would for a trace never
seen. The bulk form's required `to` makes that rare; for one trace whose
export is still in flight, wait for it to finish.

### What this means for a data-subject request

You are the controller; tracepad is the tool. This is what an erasure does,
and where its reach ends.

**What goes when the call returns.** Every row the store keeps about the
traces filed under the user id: the traces, their observations, payloads,
scores, search entries and annotation-queue items; the scores given to the
sessions those traces belonged to; the dataset items cut from those traces,
with their history; the per-user statistics; and the user's spans inside the
raw OTLP bodies — each batch that held them is rewritten without them, or
deleted when nothing else was in it. From then on no endpoint, CLI command or
MCP tool returns them, the export does not replay them, and no search finds
them. The project-wide statistics are corrected where they can be: the hours
the erased traces occupied are recomputed in the same transaction that
deletes them, chunk by chunk, so they are right before the call returns and
stay right if it is cut off; hours whose rows retention already took are
frozen and keep their totals — see [What outlives what](#what-outlives-what).
This lands well inside the one-month response window Article 12(3) allows.

**What is overwritten by the next sweep.** Deleting a row unlinks it; its
bytes stay in the file until something overwrites them. Tracepad zeroes the
freed space as rows go, and two things still hold the deleted text afterwards:
the search index, whose segments keep a deleted document's words until they
are merged, and the write-ahead log, which keeps the pages as they were until
it is checkpointed. So an erasure asks for a **compaction**, and the next
sweeper pass — within `TRACEPAD_SWEEP_INTERVAL`, an hour by default — merges
the index, drains the free pages and truncates the log. The erasure's answer
says when that pass is due (`compaction.expected_by`); `GET /api/v1/system`
says when it last completed. Deleting traces and a project's purge ask for one
too; the retention sweep zeroes what it frees but does not, since rewriting the
index every hour would cost more than it protects, and the words of swept
traces leave the index with its ordinary merges. The cost is per pass, not per
deletion: an explicit deletion, however small — one trace — has the next pass
compact the whole store, and on a large one that is a rewrite of the search
index. Deletions made within one interval share one compaction, so a store
where something is deleted every hour rewrites its index once an hour.

**What erasure cannot reach.**

1. **Copies outside the database file**: your own backups, volume and
   filesystem snapshots, replicas, an export taken before the erasure, the
   logs of your application or proxy. Expire those with your own process.
2. **The backup the server writes before an upgrade.** Before every start that
   applies a migration the server writes `tracepad.db.pre-<migration>.bak`
   beside the database — a complete copy, readable by its owner only. Once
   that upgrade's migrations have committed it removes the older ones, and the
   sweeper removes the newest at its first pass seven days or more after it
   was written. While one exists, the erasure's dry run and answer name it and
   that date (`pre_migration_backup.remove_after`);
   [docker.md](docker.md#upgrading-and-backing-up-first) says how to remove it
   sooner.
3. **Raw bodies older than the trace window.** With `raw_retention_days`
   longer than `retention_days`, the traces that said whose spans a batch
   holds are gone, and nothing can attribute them; the dry run counts those
   batches as `raw.unattributable_batches` — an estimate, not an exact count
   ([admin.md](admin.md#erasing-a-users-data)). If you answer erasure requests,
   keep the raw window no longer than the trace window, or run with
   `TRACEPAD_STORE_RAW=off`.
4. **Anything not linked by id**: a name typed into another user's prompt, a
   dataset item copied without its `source_trace_id`, a score about the user
   attached to someone else's trace, a trace deleted by hand earlier (its
   spans stay in the raw bodies, linked to nobody), spans the ingest could not
   decode (a `ResourceSpans` block that did not decode is kept as it arrived).
5. **What the disk keeps below the file.** A program can overwrite its own
   file, not the blocks a filesystem or an SSD has already released; full-disk
   or volume encryption is the layer for that.
6. **What deletions before this version left.** Until this version the freed
   space was not zeroed. The first sweeper pass after the upgrade compacts
   once — the index rewritten, the free pages drained — and what older
   deletions left inside pages still in use stays until those pages are
   rewritten. A full `VACUUM` of the stopped database rewrites every page,
   using the `sqlite3` shell (3.43 or newer) on the database file:

   ```sh
   sqlite3 tracepad.db "INSERT INTO search_fts(search_fts) VALUES('optimize'); VACUUM;"
   ```

   The first statement rewrites the search index without the deleted text,
   the second every page of the file.

The dry run is where these are counted before anything happens: the sessions'
scores (`session_scores`), the datasets that lose items
(`affected_datasets`), the batches it will read (`raw.batches_to_scan`) and the
ones it cannot attribute (`raw.unattributable_batches`), and the backup.

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
| `TRACEPAD_STORE_RAW` | `on` | Keep raw OTLP bodies at all. `off` stops keeping new ones; those already kept leave by the raw window. |

The windows themselves are per project and live in the database, so changing
them needs no restart. See [admin.md](admin.md).
