# MCP

Tracepad serves the Model Context Protocol so an agent can read traces as
tools rather than as HTTP. Eight tools, all reads, each a thin wrapper over one
[read API](api.md) endpoint.

**The MCP surface cannot modify or delete anything.** Not "does not today" —
cannot: the only thing a tool can reach the API with is a `GET`, and the
administrative surface of [admin.md](admin.md) deliberately added nothing here.
An agent should not hold destructive capability at all, so that a hallucinated
tool call has nothing to destroy. Deleting a project, moving a retention window
and erasing a user's data live in the HTTP API and the CLI, where a human
confirms them by name.

The tools call the HTTP API — in process when the server serves them itself,
over the network in stdio mode — and never the database. That is what makes a
tool result and a `curl` of the corresponding endpoint the same bytes: budgets,
truncation, authentication and JSON shape have exactly one implementation.

## Connecting

The running server serves MCP at `/mcp` on the same port as everything else,
over streamable HTTP. Authentication is the same project key:

```json
{
  "mcpServers": {
    "tracepad": {
      "type": "http",
      "url": "http://localhost:4318/mcp",
      "headers": {"Authorization": "Bearer tp-sk-…"}
    }
  }
}
```

Set `TRACEPAD_MCP=off` to stop serving it.

For clients that cannot speak remote HTTP, the same binary runs the same tools
over stdio against a running server:

```json
{
  "mcpServers": {
    "tracepad": {
      "command": "tracepad",
      "args": ["mcp"],
      "env": {
        "TRACEPAD_URL": "http://localhost:4318",
        "TRACEPAD_API_KEY": "tp-sk-…"
      }
    }
  }
}
```

`--url` and `--key` work as flags too.

## Protocol

Tracepad targets protocol revision **2026-07-28**, which is stateless: no
handshake, no `Mcp-Session-Id`, the protocol version and the client's
capabilities riding in each request's `_meta`. These tools are stateless read
wrappers, so the new core fits exactly — any load balancer works, and there is
no session bookkeeping to get wrong. Clients on 2025-11-25 negotiate the older
stateful session transparently; tool behaviour is identical.

Deliberately not adopted: roots, sampling and logging (deprecated in
2026-07-28), the tasks extension (every tool here answers in milliseconds),
elicitation, and MCP resources or prompts. Authentication is the pre-shared
project key, not OAuth: the OAuth framework in the MCP spec targets
multi-tenant public servers, and this one is self-hosted.

A `traceparent` on a request's `_meta` is recorded in the server log, once per
request, when it is well-formed W3C trace context: lowercase hex, neither the
trace id nor the parent id all zeros, and a version other than `ff`. Version
`00` must be exactly 55 characters; a later version may be longer, as the W3C
spec allows, when a dash follows its first 55 characters, and only those 55
are logged. Anything else is dropped without being logged. Nothing more
happens with it — this is a tracing product, so it should at least not be the
tool that drops trace context on the floor, but instrumenting tracepad with
tracepad is a later question.

## The tools

| Tool | Endpoint | Use it when |
|---|---|---|
| `list_traces` | `GET /api/v1/traces` | Finding runs by filter |
| `search` | `GET /api/v1/traces` with `q` | Finding runs by what was said in them |
| `get_trace` | `GET /api/v1/traces/{id}` | Reading one run whole |
| `get_last_trace` | `GET /api/v1/traces/last` | "Why did the last run fail" |
| `get_observation_io` | `GET /api/v1/observations/{id}/io` | Following a truncation marker |
| `list_sessions` | `GET /api/v1/sessions` | Finding conversations by filter |
| `get_session` | `GET /api/v1/sessions/{id}` | Summarizing a conversation |
| `list_users` | `GET /api/v1/users` | "Who are my heaviest users" |
| `get_user` | `GET /api/v1/users/{id}` | "What does this account cost me" |
| `get_prompt` | `GET /api/v1/prompts/{name}` | "What prompt is in production" |
| `list_scores` | `GET /api/v1/scores` | Reading eval results |
| `get_stats` | `GET /api/v1/stats` | Counts, cost, latency, trends |
| `get_score_trends` | `GET /api/v1/stats/scores` | "Did hallucination drop after 2.5.0", "which model scores best" |
| `get_facets` | `GET /api/v1/facets` | "Which environments exist", "what are the traces called" — before guessing at a filter value |
| `list_datasets` | `GET /api/v1/datasets` | "What test sets are there" |
| `get_dataset_items` | `GET /api/v1/datasets/{name}/items` | Reading the cases in one |
| `list_runs` | `GET /api/v1/datasets/{name}/runs`, or `GET /api/v1/runs` without `dataset` | "What has been tried" |
| `get_run` | `GET /api/v1/runs/{id}` | How one eval run went |
| `get_run_items` | `GET /api/v1/runs/{id}/items` | Which cases failed, and what was said |
| `compare_runs` | `GET /api/v1/runs/{a}/compare/{b}` | "Did this change make it better" |
| `list_queues` | `GET /api/v1/queues` | "What is being reviewed, and how far has it got" |
| `get_queue_items` | `GET /api/v1/queues/{name}/items` | What is left to review, who did what, why something was skipped |

`search` and `list_traces` are the same endpoint under two descriptions, and
that is the point: "the user quotes text they saw" is a different question from
"the user asks what ran", and a model choosing tools by description is better
served by two than by one with a mode. `search` requires its `q`, takes every
filter `list_traces` takes, and its rows carry `match` — which observation and
field the hit was in, and a snippet of the text — so the next call can be
`get_observation_io` on that observation. The matching rules are the API's:
[api.md](api.md#search).

The eval tools read [datasets and runs](datasets.md) and write nothing —
a run is opened by a harness or a person, and creating one from a model's guess
would leave a container in the store that nobody meant. `compare_runs` is the
one to reach for when the question is whether a change helped: it returns both
runs' aggregates per score name with the delta and how many cases improved,
regressed or stayed, then the cases themselves with their verdicts. A name says
*improved* only when its config gives it a direction; otherwise the verdict is
*changed*. `get_run_items` is the follow-up — what the model actually said for
a case — and its payload markers feed `get_observation_io` like every other.

The two annotation tools read [the review queues](annotation.md) and, for the
same reason, write nothing: reading what is left to review is an agent
question, and posting a verdict — or completing an item on the strength of
one — is a person's or a script's, with somebody to answer to. What a queue
holds is pointers; the verdicts themselves are scores, so `list_scores` with a
`trace_id` is the follow-up, and a score written from a queue carries
`metadata.queue` and `metadata.annotator`.

`get_score_trends` is the quality question `list_scores` cannot answer: it
returns one series per score name over the asked buckets — a mean for a numeric
name, the rate of true for a boolean one, the count of each value for a
categorical one — where `list_scores` returns the individual judgements. Its
`targets` field is the honesty `unit` is on `get_stats`: grouped by model it
counts only the scores that name an observation, because a trace-level score has
no model. Scores that name only a session, and `text` scores, are never on a
timeline. It returns the fifty busiest names by default — `limit` raises that
to 500, and `omitted` says how many are still not there — so a model reading
the list never mistakes it for the whole list. See [quality.md](quality.md).

`get_facets` is what to call before guessing at a value for `list_traces`. It
returns the environments, releases and trace names of a range with the number
of traces carrying each, busiest first — and the counts are the point as much
as the values: `prod: 1` beside `production: 4656` is a typo, and nothing but
the count says so. It takes the range and nothing else, so the answer does not
change with the other filters. At most a hundred values per column, with
`omitted` saying how many were left out.

Its answers go straight back in: `environment`, `release` and `name` each take
**one value or a comma-separated list**, and a trace matches when its column
equals any item — `{"environment": "production,staging"}` keeps both. `tag` is
the exception and stays an AND: a trace must carry every tag listed.

There are no administrative tools, for the reason at the top of this page: not
a gap, a guarantee.

Tool inputs mirror their endpoint's query parameters — same names, same
meanings — with the constraints stated in the schema: enums for `status`,
`type`, `group_by` and `data_type`, hex patterns for ids, `limit` bounded at
500. Every tool declares an `outputSchema` and returns the endpoint's JSON as
`structuredContent`, with a one-line summary in `content`.

`list_traces`, `search` and `get_last_trace` take four filters for what the
wire already carries:

| Parameter | Reach for it when |
|---|---|
| `release`, `version` | The user names a deployment, or asks whether a release changed something. |
| `type` | The user asks about a kind of step — a tool call, a guardrail, a retrieval — and wants the traces that contain one. One of ten values, exact: `generation` does not match `embedding`. |
| `prompt` | The user names a prompt and wants what it produced. `"support-answer"` for every version, `"support-answer@7"` for one. A version is a number, so a name that contains an `@` is passed as it stands. |

The rows they return carry `release`, `version` and `ttft_ms`; the tree from
`get_trace` and `get_last_trace` carries each observation's kind,
`completion_start_time`, its own `ttft_ms`, the `prompt` it ran and the sizes
of its payloads — the last of these whether or not `expand` inlined them, so
"is this worth fetching" is answerable without fetching it. `get_stats` takes
`release` in `group_by`, and `total` — the whole window as one bucket, whose
p95 is merged over every hour rather than averaged, which is the answer to
"how much did it cost this week" in one call; last week beside this week is
the tool called twice ([api.md](api.md#statistics)).

Every tool is annotated `readOnlyHint: true` with a display title, so a host
can auto-approve reads instead of prompting for them. `tools/list` is returned
in a fixed order with `ttlMs: 3600000` and `cacheScope: "private"` — the list
changes only when the binary does, and it is a per-project authenticated
surface.

## Following a truncated payload

A response spends a bounded byte budget on payloads. A payload that does not
fit comes back as a marker:

```json
{
  "truncated": true,
  "size": 3200000,
  "preview": "[{\"role\":\"user\",\"content\":\"here is the whole log…",
  "trace_id": "4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f",
  "observation_id": "2b3c4d5e6f7a8b9c",
  "full": "/api/v1/observations/2b3c4d5e6f7a8b9c/io?trace_id=4f8c…"
}
```

The `trace_id`/`observation_id` pair is exactly what `get_observation_io`
takes. That is why the tool exists: without it the markers would be dead ends
for a consumer that has tools but no URL fetcher.

## A worked example

"Why did the last production run fail?"

1. `get_last_trace` with `{"status": "error", "environment": "production", "expand": "io"}`
   → the whole failed trace, tree and payloads, in one call.
2. If a payload came back truncated, `get_observation_io` with the marker's
   id pair → that payload whole.
3. `get_stats` with `{"group_by": "day"}` → whether this is new or has been
   happening all week.
