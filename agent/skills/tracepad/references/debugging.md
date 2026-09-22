# Debugging from traces

Worked sequences for the questions people bring. Each step says what to look
for in the answer before taking the next one. `<…>` is a value from an
earlier answer. Piped, every command prints JSON, so `jq` shapes it.

The listing's filters are the same everywhere: `--since`/`--until` (a Go
duration such as `24h` or an RFC 3339 instant — there is no day unit, a week
is `168h`), `--env`, `--name` (the **trace** name), `--error`, `--user`,
`--session`, `--release`, `--type`, `--prompt`, `--min-cost`, `--search`.
`tracepad facets` lists the values the first ones can take.

## Which trace cost the most, and why

1. **The scale.** The whole window as one row, then where the money went:

```sh
tracepad stats --group-by total --since 24h
tracepad stats --group-by model --since 24h
```

   Look at `count`, `total_cost` and `tokens`. Grouped by model the count is
   of generations, not traces. A model with tokens and no cost is one whose
   client reports no price.
   Statistics answer closed hours from a rollup that trails the listing by up
   to about ten minutes; the hour in progress is live.

2. **The candidates.** The listing runs newest first and has no sort by cost,
   so narrow it with `--min-cost` and sort the page yourself:

```sh
tracepad traces ls --since 24h --min-cost 0.05 --fields id,name,total_cost --limit 500 \
  | jq '.traces | sort_by(-.total_cost) | .[:5]'
```

   An empty page: lower `--min-cost`. A full page (500 rows and a
   `next_cursor`): raise it — the most expensive trace is on the page only
   when the page is not full.

3. **Why.** Read the top one's tree:

```sh
tracepad traces show <trace-id>
```

   Find the generations holding the cost and read their `model` and `usage`.
   The usual answers, in order of frequency: a large input (`usage.input` in
   the thousands, a large `input_bytes` — read it with `--full`); many calls in
   one trace (a loop, retries, an agent that did not stop — count the
   generations and compare their inputs); an expensive model on a step that
   did not need it. Say which one, with the numbers.

4. **New or usual.** Whether it is a spike or the trend, and whether a deploy
   started it:

```sh
tracepad stats --group-by day --since 168h
tracepad stats --group-by release --since 168h
tracepad users ls --sort cost --limit 10
```

## Why it is slow

```sh
tracepad stats --group-by day --since 168h
tracepad traces ls --since 24h --name <trace-name> --fields id,latency_ms,ttft_ms --limit 500 \
  | jq '.traces | sort_by(-.latency_ms) | .[:5]'
tracepad traces show <trace-id>
```

The first gives `latency_ms` p50 and p95 per day: is it every run or a tail?
In the tree, find the observation that holds the time. A generation with a long
`ttft` was waiting on the provider; a short `ttft` with a long duration is a
long output; a parent much longer than its children is the application's own
work between calls; many sequential generations is the shape of the agent.

## Why it failed, and what it was sent

1. **The newest failure, whole.** Add the filters that describe it:

```sh
tracepad traces last --error --full
tracepad traces last --error --env production --since 24h --full
```

2. **The failing step.** On a terminal it is marked `✗` with its status. In
   JSON, list every observation at level `ERROR`:

```sh
tracepad traces last --error --full \
  | jq '[.. | objects | select(.level? == "ERROR") | {id, name, type, status_message}]'
```

3. **What it was sent.** The `input` of that observation, in the same answer.
   A truncated input carries the path of the whole one — follow it (SKILL.md,
   "Follow a cut payload") before concluding what the prompt said.

4. **How often, and since when.** `--total` counts the matches (up to 1000):

```sh
tracepad traces ls --error --since 24h --total --limit 1
tracepad stats --group-by day --since 168h
```

A step named in the question — "the generation called X" — is an
**observation** name, and `--name` filters on the trace's name. Take the
traces that failed and contain a generation, and look for X in each tree,
newest first:

```sh
tracepad traces ls --error --type generation --since 24h --fields id,name,timestamp --limit 20
tracepad traces show <trace-id> --full \
  | jq '[.. | objects | select(.name? == "<X>" and .level? == "ERROR")]'
```

An error message somebody pasted is a search, not a filter. Words, not
substrings (`error` does not find `errors`; `err*` does), and `"a phrase"` for
words in a row:

```sh
tracepad traces ls --since 168h --search "\"rate limit\""
```

## Which prompt version produced this

1. The tree names the prompt on the observation that ran it (`prompt
   <name>@<version>` in the table, `prompt.name` and `prompt.version` in
   JSON), as the client recorded it.
2. The traces that ran one version, or any version:

```sh
tracepad traces ls --prompt <name>@<version> --since 168h
tracepad traces ls --prompt <name> --since 168h --error
```

3. The text, and what changed between two versions:

```sh
tracepad prompts get <name> --version <version>
tracepad prompts diff <name> --from <older> --to <newer>
```

4. Whether it made things better is a score question:
   `tracepad scores trend --name <score> --group-by release`.

## One conversation, one user

```sh
tracepad sessions show <session-id>
tracepad users show <user-id>
tracepad stats --group-by day --since 168h --user <user-id>
```

## The same over MCP

The tools take the API's parameter names, not the CLI's flags — `environment`,
`status`, `from`, `to`, `q` — and their schemas say so. The sequences above
map one to one: `get_stats` and `get_facets` for the scale, `list_traces` for
the candidates, `search` for a pasted message, `get_trace` or
`get_last_trace` (with `expand` for the payloads) for the tree, and
`get_observation_io` for a truncated payload.
