# Retention

A store that only grows is a store you eventually delete by hand. Tracepad
forgets on a schedule, on request, and when a project goes — without an
operator ever running SQL.

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

## The two windows

Each project has two, both counted in whole days (1 to 36500) and both
nullable:

| Setting | Applies to | `null` means |
|---|---|---|
| `retention_days` | Traces, and everything hanging off them: observations, payloads, scores | Keep forever (the default) |
| `raw_retention_days` | The stored OTLP bodies of `TRACEPAD_STORE_RAW` | Follow `retention_days` |

```sh
tracepad retention show
tracepad retention set --days 90               # traces: 90 days
tracepad retention set --raw-days 14 --yes     # raw bodies: 14 days
tracepad retention set --forever               # back to keeping everything
```

"Keep it essentially forever" is spelled `--forever`, not a very large number
of days: a window is turned into a nanosecond cutoff, so the day count is
capped at 36500 rather than allowed to overflow into a date in the future —
where it would match every row there is.

Raw follows the parsed window by default rather than being shorter, because raw
is the insurance policy: it is what makes a mapping bug retroactively fixable
and what `export --otlp` will replay. A default that expired it sooner would
silently cap all of that. Operators for whom the raw bodies are the heavy or
the sensitive part shorten them deliberately.

A batch is swept by its own age and never because the traces it fed were swept:
one export body feeds many traces with different fates.

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

Each pass also:

- collects **orphaned payloads** — rows left behind when a re-delivered span
  overwrote its input, output or metadata with a new one;
- collects **orphaned search-index entries**, for the same reason: the three
  paths that delete observations take the index with them inside their own
  transactions, and this is the belt to those braces;
- **purges** projects whose seven-day deletion grace has run out (below);
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
observations, payloads and scores — synchronously, and answers with the counts.
Like every destructive endpoint it is a dry run until confirmed; the echo here
is the user id itself.

### What this means for a data-subject request

You are the controller; tracepad is the tool. Two things are worth stating
plainly rather than leaving to be discovered:

**Erasure covers the queryable stores immediately.** After the call, no read
endpoint, CLI command or MCP tool can return that user's traces, and no search
finds their text: the index is deleted in the same transaction as the rows.
This lands well inside the one-month response window Article 12(3) allows.

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
- `tracepad projects restore <project-id>` undoes it until then — reachable
  with the project's own key, so a deployment with no admin token can still
  recover from deleting its only project.
- The name stays reserved throughout, so restore always has its name to come
  back to. Creating a project with that name meanwhile is a `409` naming the
  restorable one.

Seven days, fixed, not a knob. A safety net whose size depends on how the
deployment was configured is a safety net nobody can rely on.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `TRACEPAD_SWEEP_INTERVAL` | `1h` | How often a pass runs. A Go duration; at least `1s`. |
| `TRACEPAD_STORE_RAW` | `on` | Keep raw OTLP bodies at all. `off` removes the archive caveats above. |

The windows themselves are per project and live in the database, so changing
them needs no restart. See [admin.md](admin.md).
