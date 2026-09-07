# CLI

The same binary that runs the server is also its client. For an agent working
in a terminal that is zero integration: the tool is already on `PATH`.

The CLI is an HTTP client of the [read API](api.md) and nothing else. It never
opens the database — the server holds the SQLite writer, and a second process
reading the live file is the kind of cleverness that ends in a corrupted
database. It also cannot do anything the API cannot: if a command here would
need a workaround, that is an API gap, and the gap gets fixed in the API.

## Connecting

```sh
export TRACEPAD_URL=http://localhost:4318      # the default
export TRACEPAD_API_KEY=tp-sk-…
```

`--url` and `--key` override the environment on any command.

## Output

On a terminal you get a table. Anywhere else — a pipe, a file, a subprocess —
you get the API's JSON, byte for byte, with no flags at all. `--json` forces
JSON on a terminal too.

```sh
tracepad traces ls                # a table, if you are looking at it
tracepad traces ls | jq '.traces' # JSON, because you are not
```

That is the whole reason for the TTY check: an agent shelling out to
`tracepad` should not have to know a flag to get machine output.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | It worked. |
| `1` | The request failed: no such trace, bad key, server unreachable. |
| `2` | The command was typed wrong: unknown flag, missing argument, a `--limit` out of range. |

The split is what lets a script tell "there is no such trace" from "you
typoed a flag".

## Commands

### `traces ls`

```sh
tracepad traces ls --env production --error --since 1h
```

| Flag | Meaning |
|---|---|
| `--search` | Find traces by what was said in them. See [Searching](#searching). |
| `--env` | Environment. |
| `--error` | Only traces with a failed observation. |
| `--since` | A Go duration (`1h`, `30m`) or an RFC 3339 instant. |
| `--until` | The other end of the range, same spellings. |
| `--user`, `--session`, `--name` | Exact matches. |
| `--tag` | A tag the trace must carry. |
| `--min-cost` | Traces costing at least this much. |
| `--release`, `--version` | The deployment, and the version of the trace's own logic. Exact matches. |
| `--type` | Traces containing a step of this kind: `span`, `generation`, `event`, `agent`, `tool`, `chain`, `retriever`, `guardrail`, `evaluator`, `embedding`. Exact — `generation` does not match `embedding`. |
| `--prompt` | Traces that ran a prompt: `name`, or `name@7` for one version of it. A version is a number, so an `@` in a name is just part of it — `@acme/support` and `team@acme/answer` work as written, and `name@latest` is read as a name rather than as a label. |
| `--fields` | Comma-separated subset of the row fields. |
| `--limit` | 1–500, default 50. |
| `--cursor` | Continue from a previous page. |
| `--oldest` | Start at the far end of the listing instead of the newest page. Takes no `--cursor`. |
| `--newer` | Walk back towards newer traces from a `--cursor`, which it requires. |
| `--total` | Also print how many traces match, counted up to 1000 (`847`, or `1000+`). |

The table ends with the commands that continue the walk, in whichever
direction there is one left:

```
older: --cursor MTc4ODIy…
newer: --newer --cursor MTc4ODIx…
```

`--oldest` costs what any other page costs: pagination is a keyset, so "the
end" is a direction to read the index in and not a count of rows to skip.
It is also not a dead end — the `newer:` line is how you come back up.

The two flags are the same direction under different names, so the two
combinations that would quietly mean the other one are refused: `--oldest`
with a `--cursor` (the cursor would win and the jump never happen), and
`--newer` without one (which is the far end, not a step back).

A `--cursor` passed with nothing in it is refused for the same reason, on every
listing that takes one: `--cursor "$NEXT"` with `NEXT` unset is a script that
lost its place, and answering it with the newest page would restart the walk
instead of continuing it — a loop that never ends.

### Searching

```sh
tracepad traces ls --search "refund failed"
```

```
TIME                 ID                                NAME          ENV         OBS  ERR  LATENCY  TTFT   COST
2026-09-01 10:00:00  4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f  support-chat  production  2    0    820ms    388ms  $0.001000
    2b3c4d5e6f7a8b9c output: …the refund failed for the order because the card issuer…
```

The second line under a row says where the trace matched — the observation and
the field — and what the text says around the hit. It is dimmed on a terminal
and plain in a pipe; with `--json` the row carries the same thing as a `match`
object.

`--search` maps to the API's `q`, so the rules are the API's:
[docs/api.md](api.md#search). Words, not substrings; `"quoted phrases"` for
adjacent words; a trailing `*` for a prefix; everything else is literal text.
A search with no word in it is refused rather than answered with an empty
table.

`traces last --search "…"` is the same search, answered with the newest
matching trace and its whole tree. `tail` does not take it: it follows the
newest page, which is not a question about text.

### `traces show`

```sh
tracepad traces show 4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f
tracepad traces show 4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f --full
```

The observation tree. `--full` adds every payload, asking for the largest
budget the API allows; a payload still too large for it prints its preview and
the URL that returns the rest.

Each observation's line names its kind, and — where the client sent them —
`ttft` and the prompt it ran:

```
· generation  chat-completion  claude-sonnet-5  740ms  ttft 388ms  169 tokens  $0.001000  prompt support-answer@7  [2b3c4d5e6f7a8b9c]
```

The header above the tree carries the trace's release and version when it
named them, beside its latency, TTFT and cost. `ttft` is the wait before the
first token; the trace's is the earliest one among its observations.

### `traces last`

```sh
tracepad traces last --error --full
```

"Why did the last run fail" — every filter of `traces ls`, `--search`
included, the newest match, the whole tree. One request.

### `tail`

```sh
tracepad tail --env production --error
```

Follows the trace listing, printing each new trace on one line. It polls the
public API (every 2 s, `--interval` to change it), so it needs no new server
surface and inherits authentication, filters and budgets.

Two of them it does not take. `--until`: a follow has no far end — the newest
page is never past it — so the flag would either do nothing or stop the
following without saying so, and a bounded range of traces is `traces ls
--until`. And `--search`: following the newest page is not a question about
text. Both are a `flag provided but not defined` rather than a flag that
quietly means something else.

A trace is timestamped by when its earliest span *started*, not by when it was
stored, and exporters batch — the OpenTelemetry SDK's default processor flushes
every five seconds. So a follow looks a minute back rather than only forward,
and skips what it has already printed: a run that started before one you have
already seen still appears when it lands, exactly once.

Each poll reads one page, so on a server producing more than `--limit` traces
per interval you see the newest page of each poll rather than everything.
Raise `--limit`, or shorten `--interval`.

In a pipe it prints one JSON row per line. Ctrl-C stops it.

### `sessions ls`

```sh
tracepad sessions ls --since 24h --env production
```

One row per session, most recent activity first: last seen, id, how many
traces, how many of those failed, cost and when the session started.

Filters: `--since`, `--until`, `--env`, `--user`, `--limit`, `--cursor`,
`--oldest`, `--newer`, `--total`.
`--since` and `--until` bound the traces, so a session appears when any of
its traces falls in the window and its totals then describe those traces.
Paging works like `traces ls`: the last line prints the `--cursor` for the
next page.

### `sessions show`

```sh
tracepad sessions show session-77
```

The session's totals — traces, how many failed, cost, the window it
spans — and its traces.

### `scores ls`

```sh
tracepad scores ls --name helpfulness --since 24h
```

Filters: `--trace`, `--observation`, `--session`, `--name`, `--type`,
`--since`.

Newest first, and it pages: `--limit` (1–500, default 50) and `--cursor`, with
the last line printing the command that continues the walk.

```
older: --cursor MTc4ODEyOTQ5ODQ0…
```

One direction only — there is no `--oldest` or `--newer` here, because the
endpoint has no other end to jump to.

### `prompts`

```sh
tracepad prompts ls
tracepad prompts get support --label production
tracepad prompts push support --file prompt.json --label staging
tracepad prompts diff support --from 1 --to 3
tracepad prompts label support production --version 4
tracepad prompts label support production --rm
tracepad prompts rm support --yes
```

`push` sends the file as the version's request body, so the prompt stays JSON
and out of shell quoting:

```json
{"type": "text", "prompt": "You are a support agent.", "config": {"temperature": 0.2}}
```

`--label`, `--message` and `--expect` fill in `labels`, `commit_message` and
`expect_version` when the file does not already set them — the file wins, so a
script that sets both is never silently overruled.

`--expect N` says which version you believe the name is at (`--expect 0` for a
name you believe is new). A name that moved under you is then a `409` naming
where it actually is, rather than a version quietly appended onto somebody
else's work — see
[prompts.md](prompts.md#appending-to-the-version-you-meant).

`ls` pages: `--limit` (1–500, default 50) and `--cursor`, with the last line
printing the command that continues the walk.

```
more: --cursor c3VwcG9ydA
```

It says `more` rather than `older` because this listing is alphabetical by
name, not newest-first. The command walks that way only: the endpoint does page
in both directions, and the web interface uses it, but a terminal walk that
started at `a` has nothing to go back to that it has not just printed.

`label` is the deploy path (see [prompts.md](prompts.md#labels)): promote by
pointing `production` at a newer version, roll back by pointing it at an older
one, retire it with `--rm`. Neither writes a version, and both print where the
label ended up — a removal printing the version it was taken from, which is
what you point it back at.

`rm` deletes a name with every version and every label it has, so it wears the
same ceremony as the administrative commands: without `--yes` it prints what
would go and asks you to type the name back, and off a terminal it refuses
outright. Traces that ran the prompt keep the name and version they recorded.

### `stats`

```sh
tracepad stats --group-by day --since 168h
tracepad stats --group-by model
tracepad stats --group-by release
```

`--since` takes Go durations (`1h`, `30m`, `168h`) or an RFC 3339 instant.
There is no day unit — `7d` is a usage error, not a week. `--until` closes the
other end, in the same two spellings, so a duration there is also counted back
from now: `--since 48h --until 24h` is the day before yesterday.

The table's second column names what is being counted: grouping by hour, day,
environment or release counts **traces**, grouping by model counts
**observations**, because a trace has no model. Grouped by release, the traces
that named none share one bucket with an empty key.

### `datasets`, `runs`, `score-configs`

The eval commands. The loop they make — declare, push, run, close, compare —
is written out end to end in [datasets.md](datasets.md#the-same-loop-from-a-shell).

```sh
tracepad datasets ls
tracepad datasets show support-golden --version 12
tracepad datasets push support-golden --file cases.jsonl --description "the golden set"
tracepad datasets rm-item support-golden a1b2c3d4e5f60718293a4b5c6d7e8f90
tracepad datasets rm support-golden --yes

tracepad runs ls                                   # every dataset's runs, newest first
tracepad runs ls support-golden
tracepad runs create support-golden --name "prompt v8" --metadata-file run.json
tracepad runs show 0e5a7c1d2b3f4a6980c1d2e3f4a5b6c7 --items
tracepad runs finish 0e5a7c1d2b3f4a6980c1d2e3f4a5b6c7 --failed "judge timed out"
tracepad runs compare 0e5a… 1f6b… --all
tracepad runs rm 0e5a7c1d2b3f4a6980c1d2e3f4a5b6c7

tracepad score-configs ls
tracepad score-configs push accuracy --file accuracy.json
tracepad score-configs rm accuracy
```

`datasets push` takes a `.jsonl` — one case per line, which is the shape a
dataset is edited by hand in — or a `.json` array, which is the shape a script
generates. Either way it is **one** batch on the wire, because the dataset's
version advances once per write: a file sent case by case would leave a version
per case and no number that names the file. It prints where it landed:

```
version 12: 3 items changed, 200 in the batch
unchanged at version 12
```

`runs ls` without a dataset reads `GET /api/v1/runs` — the whole project's
runs, newest first, with a dataset column the per-dataset table has no need
of. With one it reads that dataset's own listing.

`runs create --json` answers with the whole run, so a script reads the id and
the version it pinned from one call:

```sh
RUN=$(tracepad runs create support-golden --json)
```

`datasets show --json` walks every page rather than printing the first: it is
the dataset's export, and an export that looks complete and is not would be
worse than none. It is the one command whose JSON stitches pages; every other
`--json` prints the endpoint's own bytes, cursors included, so a script pages
exactly the way an HTTP client does.

The `runs compare` **table** walks every page too — the cases worth reading are
the ones that moved, and the regression may be on page two. `runs compare
--json` does not: it prints the one page the endpoint returned, next to counts
(`improved`, `regressed`, `same`) that are always about the whole pair, and
`next_cursor` for the rest of the cases.

`runs show --items` is the case-by-case view: expected output beside what was
produced, with the scores each attempt got. Add `--unknown` to see the run's
traces that name a case its dataset version does not have.

`datasets rm` is destructive — it takes the dataset's runs with it and releases
every trace they were keeping out of retention — so it shows the preview and
asks you to type the name back, and needs `--yes` off a terminal. `runs rm` and
`score-configs rm` do not: one row each, and the traces of a deleted run are
released, not deleted.

### `system`

```sh
tracepad system
```

Version, uptime, database size, row counts, the raw archive, the writer queue,
the ingest counters and what the retention sweeper has done since the server
started. The first thing to run when something looks wrong, and the thing to
paste into a bug report.

The `raw archive` block is what an export can carry out, and how far back:

```
raw archive in this project
  storage       on
  batches       12400
  on disk       3.1 GiB
  covering      2026-08-06 04:12:19 .. 2026-09-05 09:44:02
  not covered   214 traces started before it begins
```

### `export`

```sh
tracepad export --otlp --to http://collector:4318/v1/traces
tracepad export --otlp --dir ./tracepad-export
```

Replays the archive — every export body as it arrived — into any OTLP receiver
or onto disk, in arrival order, resumably. `--dry-run` prints what would go and
sends nothing; a receiver that answers `429` or `5xx` is retried, anything else
`4xx` stops the export with the cursor to pass as `--after`. The summary ends
with how many traces started before the archive begins, which are the ones no
export can carry.

It has a page of its own: [export.md](export.md).

## Administration

`projects`, `keys`, `retention` and `users rm-data` manage the server itself.
They are clients of the same API as everything else, and they are covered in
[admin.md](admin.md) and [retention.md](retention.md); what matters here is how
they behave at a terminal.

```sh
tracepad projects ls
tracepad keys create
tracepad retention set --days 90
tracepad users rm-data user-4711
```

Every destructive command asks the server what it would do, prints that, and
asks you to type the name of what is being destroyed:

```
$ tracepad projects rm 9f2c…
this would delete project 9f2c…:
  api_keys       2
  observations   180114
  raw_batches    812
  traces         41203
  oldest         2026-03-14 08:21:00
the keys stop working immediately; the data is restorable for seven days
type "checkout-service" to confirm:
```

`--yes` answers that for a script. It is not a bypass: the command still asks
the server first and still sends back the confirm value the server named — what
`--yes` replaces is the typing. Run non-interactively **without** it and the
command stops with the preview on stderr and exit code 1, because a script that
deletes a project by default is a script that deletes a project by accident.

Commands that act on a project take `--project <id>`; with one reachable
project, the credential answers that by itself. `projects show` also takes the
id as a positional, which is the same argument under a second spelling — so
passing both is a usage error rather than one of them quietly winning, and that
holds when the two agree as well. The commands marked as needing
the admin token in [admin.md](admin.md) take it as `--key` or
`TRACEPAD_API_KEY`, since it rides in the same header as a project key.

### `health`

```sh
tracepad health [--url URL] [--json]
```

Is the server up, and which build is it? Exit `0` and the version on stdout;
exit `1` and the reason on stderr for anything else — a refused connection, a
non-200, or a 200 whose body carries no version, which is what a proxy's splash
page on the wrong port looks like.

```sh
$ tracepad health
0.2.0
$ tracepad health --json
{"version": "0.2.0", "ok": true}
$ tracepad health --url http://localhost:9999 ; echo $?
tracepad: cannot reach http://localhost:9999: … connection refused
1
```

**It is the one command that needs no key**, because `/health` is the one route
that needs none. That is the point of it: a container's health check, a systemd
unit or a load balancer can ask whether the process is alive without holding a
project secret to do it. It is what the [Docker image](docker.md) declares as
its `HEALTHCHECK`.

It is also the one command that will look for the server rather than assume it.
With neither `--url` nor `TRACEPAD_URL`, it reads **`TRACEPAD_LISTEN`** — the
variable that told the server where to bind — and probes that, treating a
wildcard bind as `127.0.0.1`; only with neither of those does it fall back to
`http://localhost:4318`. A probe usually runs beside the process it is asking
about, and on a server moved to another port it should move too.

```sh
TRACEPAD_LISTEN=:8080 tracepad health     # probes http://127.0.0.1:8080
```

The `--listen` **flag** is not consulted, because a flag on the server's command
line is not visible to a second process. Where both a server and its probe read
the configuration, put the port in the environment.

## Version skew

Every API response carries the server's build. When it differs from the CLI's,
the first command of a session prints a warning to stderr and carries on — a
skew is usually fine, and a client that refused to run would be worse than one
that says what it noticed.
