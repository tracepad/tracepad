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
| `--prompt` | Traces that ran a prompt: `name`, or `name@7` for one version of it. |
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
`--since`, `--limit`.

### `prompts`

```sh
tracepad prompts ls
tracepad prompts get support --label production
tracepad prompts push support --file prompt.json --label staging
tracepad prompts diff support --from 1 --to 3
```

`push` sends the file as the version's request body, so the prompt stays JSON
and out of shell quoting:

```json
{"type": "text", "prompt": "You are a support agent.", "config": {"temperature": 0.2}}
```

`--label` and `--message` fill in `labels` and `commit_message` when the file
does not already set them — the file wins, so a script that sets both is never
silently overruled.

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

### `system`

```sh
tracepad system
```

Version, uptime, database size, row counts, the writer queue, the ingest
counters and what the retention sweeper has done since the server started. The
first thing to run when something looks wrong, and the thing to paste into a
bug report.

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
project, the credential answers that by itself. The commands marked as needing
the admin token in [admin.md](admin.md) take it as `--key` or
`TRACEPAD_API_KEY`, since it rides in the same header as a project key.

## Version skew

Every API response carries the server's build. When it differs from the CLI's,
the first command of a session prints a warning to stderr and carries on — a
skew is usually fine, and a client that refused to run would be worse than one
that says what it noticed.
