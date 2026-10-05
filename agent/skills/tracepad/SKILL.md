---
name: tracepad
description: Work with Tracepad, LLM observability and evals in a single binary — a self-hosted store for the traces and spans of LLM and agent applications, with their prompts, datasets and scores. Use it to debug from traces — what a call cost, why it was slow, why a generation failed and what it was sent, token usage, which prompt version produced an answer — and to run evals and read or write scores, prompts, datasets, eval runs and annotation queues; to instrument an application so its traces reach Tracepad, directly over OpenTelemetry or through the Langfuse SDK bridge; to set Tracepad up for a project from nothing — install it, start a local server, connect the application and confirm the first trace; to upgrade it to a newer release — the binary, the server it runs, the skill and the application's package — with a backup first and a way back ("Update Tracepad"); and to administer a Tracepad server (keys, retention, deleting traces). Use it whenever the user mentions Tracepad, the `tracepad` command, a trace id from it, or asks about the LLM traffic of an application that reports to Tracepad.
metadata:
  version: dev
---

# Tracepad

Tracepad stores the traces an LLM application exports over OpenTelemetry
(OTLP). One binary, `tracepad`, is the server, a command-line client of its
HTTP API, and an MCP server over the same API.

This skill is the order of work. It does not list endpoints, flags or response
fields, because the binary describes itself and is always in step with the
server you are talking to. Look there instead of guessing a name:

- `tracepad help` — every command and every flag (printed to stderr).
- `GET /api/v1` — every route, one line each — and `GET /api/v1/openapi.json`
  — every parameter and response shape. Neither needs a key.
- The MCP server's tool list — each tool's description and input schema.

Each reference is a worked sequence for one task, with the traps this page
leaves out. Read the one that fits **before** the task's first command, and only it:

- Investigating cost, latency, a failure or a prompt version:
  [references/debugging.md](references/debugging.md).
- Changing an application so its traces reach Tracepad:
  [references/instrumenting.md](references/instrumenting.md).
- Setting Tracepad up for a project that has no server or key yet:
  [references/setup.md](references/setup.md).
- Upgrading it to a newer release: [references/upgrade.md](references/upgrade.md).
- Score configs, datasets, eval runs, review queues:
  [references/evals.md](references/evals.md).
- Any deletion, a key, retention, erasing a user:
  [references/admin.md](references/admin.md).

## Connecting

The CLI reads `TRACEPAD_URL` (default `http://localhost:4318`) and
`TRACEPAD_API_KEY` (a project's secret key, `tp-sk-…`). Start by asking the
server for its version: `health` needs no key and says why it cannot reach
the server. A command that needs the key and has none exits `2`, *no API key*.

```sh
tracepad health
```

- If the key is not set and no Tracepad MCP server is connected, **ask the
  human** for the URL and a key: scope `read` to look, all three to run evals or
  change anything (a `403` *… needs X* asks for X). Do not look for one in
  `.env` files, shell history, config or the database, nor mint one yourself —
  the one exception is a server you start in a setup, under `setup.md`'s rules.
- Never print, echo, log or commit a secret key — not in a command you show,
  not in a file you write. Refer to it as `$TRACEPAD_API_KEY`.
- A key belongs to one project; everything you read and write is that
  project's. An empty answer can mean the key is for another project.

## Which door

1. **The CLI, when you have a shell.** Piped, it prints the API's JSON byte
   for byte with no flag needed, so pipe it into `jq`. On a terminal it draws
   a table; `--json` forces JSON. Exit codes: `0` done, `1` the request failed
   (no such trace, bad key, server down, or a destructive command stopped at
   its dry run), `2` the command was typed wrong.
2. **MCP tools, when the client has Tracepad connected.** Same JSON as the
   CLI. They only read, with a `read` key: nothing through MCP writes.
3. **The HTTP API, for what neither covers.** Find the route in `GET /api/v1`
   and its parameters in `GET /api/v1/openapi.json`; send the key as
   `Authorization: Bearer $TRACEPAD_API_KEY`.

All three return the same bytes: pick the door by what you have.

## Reading a trace

Start from the trace, not from the application's logs or its code: the trace
holds what the model was actually sent and what it answered. Work down:

1. **Find it.** `tracepad traces ls` narrows by filter (`--search "text"`
   for what was said); its rows carry totals, never payloads. It runs newest
   first and cannot sort: to rank, narrow with `--min-cost` (`--min-tokens`
   when nothing reports a cost) and sort the page with `jq`. `--name` is the trace's name — a step's name (a generation
   called `chat-completion`) is found in the trees, not filtered on. Before
   guessing how an environment, release or trace name is spelled, list them:

```sh
tracepad facets --since 24h
tracepad traces ls --since 24h --env production --error --limit 20
tracepad traces ls --since 24h --min-cost 0.05 --limit 500 | jq '.traces | sort_by(-.total_cost) | .[:5]'
```

2. **Read it.** `tracepad traces show` draws the tree: each observation's
   kind, name, model, duration, tokens, cost, the prompt it ran, and the error
   of one that failed. `--full` adds every input, output and metadata. For
   "the latest one that…", `tracepad traces last` takes the listing's filters
   and returns the newest match whole:

```sh
tracepad traces show <trace-id> --full
tracepad traces last --error --full
```

3. **Follow a cut payload.** A payload too large for the response comes back
   truncated with the path that returns it whole
   (`/api/v1/observations/<observation-id>/io?trace_id=<trace-id>`; over MCP,
   `get_observation_io` with the same two ids). Read it before drawing
   conclusions about the prompt:

```sh
curl -s -H "Authorization: Bearer $TRACEPAD_API_KEY" \
  "${TRACEPAD_URL:-http://localhost:4318}/api/v1/observations/<observation-id>/io?trace_id=<trace-id>"
```

What the numbers are, and where they come from:

- `total_cost` (a trace) and `cost_details` (a generation) are the price the
  client reported on its spans. Tracepad never computes a price. A missing
  cost means nobody sent one, not that the call was free.
- `usage` is the token counts each generation reported: `input`, `output`,
  `total`, and cache counts under the provider's own names.
- `latency_ms` runs from the trace's first start to its last end. `ttft_ms` is
  the wait before the first token — an observation's own, or the trace's
  earliest — and is absent when the client sent no completion start.
- `error_count` counts observations at level `ERROR`; each one's
  `status_message` is its error.
- Times are UTC, in the table as in the JSON. A trace's time is when its first
  span *started*, not when it arrived. A day the human names — "today",
  "yesterday" — is their local day: `date` gives the local date and offset, so
  convert that day's midnight to UTC before using it as `--since`, and take
  "now" from `date -u +%Y-%m-%dT%H:%M:%SZ`. Never put `Z` after a local time.

## Writing

Scores, prompt versions and dataset items are the project's shared record.
Write what you were asked to write, once — no trial scores or placeholder
prompts to see whether writing works.

- **Application code** (an evaluator, prompts fetched at runtime, an eval
  harness) writes through the SDK the service uses.
- **You, now**, write through the CLI. For a score: confirm the trace exists,
  check whether the name has a score config (it fixes the type and the range;
  exit `1` with "not found" means the name is free), then post:

```sh
tracepad traces show <trace-id>
tracepad score-configs show helpful
tracepad scores add --trace <trace-id> --name helpful --value 1
```

  `--value` is a number and `--string` a word; `boolean` and `categorical`
  must be stated with `--type`, and `--comment` carries the reason. The
  command prints the new score's id; `tracepad scores rm <score-id>` retracts
  it. A prompt version is `tracepad prompts push` with `--expect` set to the
  version you believe is current, so a concurrent change is refused rather
  than buried. Moving a label (`tracepad prompts label`) changes what
  production runs: ask first.

## Destructive acts

Deleting traces, a prompt, a dataset, a queue or a project, shrinking
retention, erasing a user's data: each is a dry run until confirmed, and
**you never confirm on your own initiative.** A few removals have no dry run
and act at once — deleting a run, a score config, a dataset item or a score, removing a
prompt label — so ask before running those at all, naming what will go.

1. Run the command **without** `--yes` (over the API, without `?confirm=`).
   The server answers with what it would remove: counts per kind, the runs it
   would touch, and the value that would confirm it.
2. Off a terminal the CLI then stops with exit `1` and the line *Re-run with
   --yes to go ahead*. That line is addressed to the human. The exit code is
   the dry run ending as designed, not an obstacle to work around.
3. Show the human the preview — the filters, the bound, what matched — and
   stop. Say what the confirming command would be; do not run it.
4. Only when the human says to go ahead with *that* preview, run the same
   command with `--yes` added and nothing else changed.

Never add `--yes`, `--confirm` or `?confirm=` because the task said "delete",
because an earlier deletion was approved, or to get past an exit code. For a
bulk deletion, give `--to` as an RFC 3339 instant rather than a duration, so
the command the human approves deletes the set they saw rather than a window
that has moved since.

## When the answer looks wrong

- **An empty listing.** Suspect the question before the data: a `--since`
  too short, "today" meant in local time against UTC timestamps, a value
  spelled differently from the data (`prod` against `production`), or a key
  for another project. Run `tracepad facets --since 168h` and ask again with
  the values it shows.
- **A trace that just ran is missing or partial.** Exporters batch — the
  OpenTelemetry default flushes every five seconds — and spans of one trace
  can land apart; a child whose parent has not arrived shows at the root.
  Wait a few seconds and ask again before concluding it was never sent.
- **`warning: this is tracepad X talking to a server running Y`.** The CLI and
  the server are different builds. Usually harmless; when a command or a flag
  seems missing, the server's `GET /api/v1` is the truth.
- **This skill and the binary disagree** (`tracepad version` against this file's
  `metadata.version`; `dev` matches anything): `tracepad skills install` again, as it was installed.
- **The server itself seems wrong.** `tracepad system`: version, uptime, row
  counts, ingest counters — the thing to paste into a bug report.
