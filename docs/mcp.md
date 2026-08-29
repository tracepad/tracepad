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

A `traceparent` on a request's `_meta` is recorded in the server log. Nothing
more happens with it — this is a tracing product, so it should at least not be
the tool that drops trace context on the floor, but instrumenting tracepad with
tracepad is a later question.

## The tools

| Tool | Endpoint | Use it when |
|---|---|---|
| `list_traces` | `GET /api/v1/traces` | Finding runs by filter |
| `get_trace` | `GET /api/v1/traces/{id}` | Reading one run whole |
| `get_last_trace` | `GET /api/v1/traces/last` | "Why did the last run fail" |
| `get_observation_io` | `GET /api/v1/observations/{id}/io` | Following a truncation marker |
| `list_sessions` | `GET /api/v1/sessions` | Finding conversations by filter |
| `get_session` | `GET /api/v1/sessions/{id}` | Summarizing a conversation |
| `get_prompt` | `GET /api/v1/prompts/{name}` | "What prompt is in production" |
| `list_scores` | `GET /api/v1/scores` | Reading eval results |
| `get_stats` | `GET /api/v1/stats` | Counts, cost, latency, trends |

There is no `search` tool. There is no search endpoint yet, and a tool faking
one over list filters would tell the model this server can do something it
cannot. It arrives with full-text search.

There are no administrative tools either, for the reason at the top of this
page: not a gap, a guarantee.

Tool inputs mirror their endpoint's query parameters — same names, same
meanings — with the constraints stated in the schema: enums for `status`,
`group_by` and `data_type`, hex patterns for ids, `limit` bounded at 500. Every
tool declares an `outputSchema` and returns the endpoint's JSON as
`structuredContent`, with a one-line summary in `content`.

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
