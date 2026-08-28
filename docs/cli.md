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
| `--env` | Environment. |
| `--error` | Only traces with a failed observation. |
| `--since` | A Go duration (`1h`, `30m`) or an RFC 3339 instant. |
| `--until` | The other end of the range, same spellings. |
| `--user`, `--session`, `--name` | Exact matches. |
| `--tag` | A tag the trace must carry. |
| `--min-cost` | Traces costing at least this much. |
| `--fields` | Comma-separated subset of the row fields. |
| `--limit` | 1–500, default 50. |
| `--cursor` | Continue from a previous page. |

The table ends with the cursor for the next page when there is one.

### `traces show`

```sh
tracepad traces show 4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f
tracepad traces show 4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f --full
```

The observation tree. `--full` adds every payload, asking for the largest
budget the API allows; a payload still too large for it prints its preview and
the URL that returns the rest.

### `traces last`

```sh
tracepad traces last --error --full
```

"Why did the last run fail" — every filter of `traces ls`, the newest match,
the whole tree. One request.

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
```

`--since` takes Go durations (`1h`, `30m`, `168h`) or an RFC 3339 instant.
There is no day unit — `7d` is a usage error, not a week.

The table's second column names what is being counted: grouping by hour, day
or environment counts **traces**, grouping by model counts **observations**,
because a trace has no model.

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
