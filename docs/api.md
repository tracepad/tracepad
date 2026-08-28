# Read API

Everything tracepad knows is readable over HTTP. The UI, the CLI and the MCP
server are thin clients over exactly these endpoints — a feature that is not
here does not exist.

The API is written for agents and for `curl`, in that order: flat snake_case
JSON, RFC 3339 timestamps, opaque cursors, and a byte budget so that a
response never quietly eats a context window.

Authentication is the same as for ingest — `Authorization: Bearer <secret
key>` or `Basic base64(<public key>:<secret key>)`. See
[ingest.md](ingest.md#authentication). No `Content-Type` is required.

## Finding your way around

```sh
curl -H "Authorization: Bearer tp-sk-…" http://localhost:4318/api/v1
```

returns every endpoint with a one-line description. The machine-readable
version is [`/api/v1/openapi.json`](#self-description), an OpenAPI 3.1
document served without authentication.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/traces` | List traces, filtered and paginated |
| `GET` | `/api/v1/traces/{id}` | One trace, observations as a tree |
| `GET` | `/api/v1/traces/last` | The newest trace matching the filters, whole |
| `GET` | `/api/v1/observations/{id}/io` | One observation's payloads, whole |
| `GET` | `/api/v1/sessions/{id}` | One session: totals and traces |
| `GET` | `/api/v1/stats` | Counts, errors, cost, latency percentiles |
| `GET` | `/api/v1/prompts/{name}/diff` | Unified diff between two prompt versions |
| `GET` | `/api/v1/system` | Version, uptime, database size, ingest counters |
| `GET` | `/api/v1` | This endpoint map |
| `GET` | `/api/v1/openapi.json` | The OpenAPI document |

Scores and prompts have their own pages: [scores.md](scores.md),
[prompts.md](prompts.md).

## The one command

```sh
curl -H "Authorization: Bearer tp-sk-…" \
  "http://localhost:4318/api/v1/traces/last?status=error&expand=io"
```

"The last failed trace, with its payloads" — one request instead of list,
filter, get, and one get per observation.

## Listing traces

```sh
curl -H "Authorization: Bearer tp-sk-…" \
  "http://localhost:4318/api/v1/traces?environment=production&status=error&limit=20"
```

```json
{
  "traces": [
    {
      "id": "4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f",
      "name": "support-chat",
      "user_id": "user-4821",
      "session_id": "session-77",
      "environment": "production",
      "tags": ["beta", "support"],
      "timestamp": "2026-09-01T10:00:00Z",
      "total_cost": 0.001,
      "latency_ms": 820,
      "error_count": 1,
      "observation_count": 7
    }
  ],
  "next_cursor": "MTc4ODIyMDgwMDAwMDAwMDAwMDo0Zjhj…"
}
```

Rows carry the aggregate columns and never a payload: a listing is for
choosing what to fetch.

### Filters

| Parameter | Meaning |
|---|---|
| `from`, `to` | RFC 3339. Half-open — `from` inclusive, `to` exclusive — so walking a timeline never reports a trace twice. |
| `environment` | Exact match. |
| `user_id`, `session_id`, `name` | Exact match. |
| `tag` | Repeatable; a trace must carry **every** tag given. |
| `status` | `error` (at least one failed observation) or `ok`. |
| `min_cost` | Traces whose total cost is at least this. A trace whose client provided no cost has none and never matches. |
| `fields` | Comma-separated subset of the row fields. |
| `limit` | 1–500, default 50. |
| `cursor` | The `next_cursor` of the previous page. |

A parameter the endpoint does not know is a `400`, and so is one sent without
a value (`?environment=` is what an unset shell variable expands to, and
answering it with the whole project would be a wider answer to a narrower
question).

`?fields=` keeps the row's own field order and drops the rest:

```sh
curl … "http://localhost:4318/api/v1/traces?fields=id,name,error_count"
```

A field the trace never carried stays absent rather than coming back empty —
absent means "this trace said nothing", which is not the same as zero.

### Paging

`next_cursor` is opaque; pass it back as `?cursor=`. It is a keyset over
`(timestamp, id)`, so a trace ingested mid-walk cannot make the next page skip
or repeat a row. `null` means there is no next page.

## One trace

```sh
curl … "http://localhost:4318/api/v1/traces/4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f"
```

Returns the trace's fields, its `metadata`, and `observations` as a **nested
tree**: children inside their parents, siblings ordered by start time.

```json
{
  "id": "4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f",
  "name": "support-chat",
  "environment": "production",
  "observation_count": 2,
  "metadata": {"channel": "web"},
  "observations": [
    {
      "id": "1a2b3c4d5e6f7a8b",
      "type": "span",
      "name": "handle-request",
      "start_time": "2026-09-01T10:00:00Z",
      "end_time": "2026-09-01T10:00:00.82Z",
      "level": "DEFAULT",
      "children": [
        {
          "id": "2b3c4d5e6f7a8b9c",
          "parent_observation_id": "1a2b3c4d5e6f7a8b",
          "type": "generation",
          "name": "chat-completion",
          "model": "claude-sonnet-5",
          "model_parameters": {"temperature": 0.2},
          "level": "DEFAULT",
          "usage": {"input": 128, "output": 41, "total": 169},
          "cost_details": {"input": 0.00038, "output": 0.00062}
        }
      ]
    }
  ]
}
```

An observation whose parent has not arrived yet renders at the root with its
`parent_observation_id` intact — a trace still being written looks incomplete,
not empty. `children` is absent for a leaf.

### `?expand=io`

Adds each observation's `input`, `output` and `metadata`. Without it, a trace
of hundreds of observations still fits the response budget; with it, you get
the shape of every payload in one round trip.

## The response budget

Every response spends at most **50 KiB** on payloads (`TRACEPAD_RESPONSE_BUDGET_BYTES`
to change the default, `?budget=` to override per request, between 4096 and
5242880). The structure of a response is never truncated — only payloads are.

Each expanded observation gets an equal share of what is left. A payload that
does not fit is cut on a UTF-8 boundary and replaced by a marker:

```json
{
  "truncated": true,
  "size": 3200000,
  "preview": "[{\"role\":\"user\",\"content\":\"here is the whole log file…",
  "trace_id": "4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f",
  "observation_id": "2b3c4d5e6f7a8b9c",
  "full": "/api/v1/observations/2b3c4d5e6f7a8b9c/io?trace_id=4f8c…"
}
```

`full` is the URL that returns it whole. `trace_id` and `observation_id` are
the same target for a consumer that speaks tools rather than URLs — they are
exactly what the MCP `get_observation_io` tool takes.

Markers cost bytes too. A trace with more payloads than the budget can carry
markers for gets none of them and one line saying so:

```json
"expansion": {
  "expanded": false,
  "payloads": 600,
  "budget_needed": 178000,
  "reason": "a budget of 51200 bytes cannot carry markers for 600 payloads; retry with a larger ?budget=, or read one payload at a time from /api/v1/observations/{id}/io"
}
```

Nothing is lost: the tree already carries every observation id, so any payload
is one `/observations/{id}/io` call away, and `budget_needed` is the number to
retry `?budget=` with. The key is absent when the expansion happened normally.

## One observation's payloads

```sh
curl … "http://localhost:4318/api/v1/observations/2b3c4d5e6f7a8b9c/io?trace_id=4f8c…"
```

The one endpoint no budget applies to: it exists to be the `full` target of
every marker, and a budget here would recurse. Payload size is bounded at
ingest by `TRACEPAD_MAX_BODY_BYTES`.

`trace_id` is optional. A span id is unique only inside its trace, so if the
same id appears in more than one trace of the project the answer is a `409`
listing the candidates — never a guess. Truncation markers always carry the
pair, so following a marker never lands there.

## Sessions

```sh
curl … "http://localhost:4318/api/v1/sessions/session-77"
```

```json
{
  "id": "session-77",
  "trace_count": 12,
  "total_cost": 0.043,
  "error_count": 1,
  "first_seen": "2026-09-01T10:00:00Z",
  "last_seen": "2026-09-01T10:14:22Z",
  "traces": [ … ],
  "next_cursor": null
}
```

Every number counts traces: `error_count` is how many of the session's traces
failed, not how many spans did. `traces` pages with the same `limit`/`cursor`
as the listing.

## Statistics

```sh
curl … "http://localhost:4318/api/v1/stats?group_by=day&from=2026-09-01T00:00:00Z"
```

```json
{
  "group_by": "day",
  "unit": "trace",
  "buckets": [
    {
      "key": "2026-09-01",
      "count": 412,
      "error_count": 7,
      "total_cost": 1.82,
      "latency_ms": {"p50": 640, "p95": 2310}
    }
  ]
}
```

`group_by` is `hour`, `day`, `model` or `environment` (default `day`), and
`unit` says what a bucket counts. Grouping by hour, day or environment counts
**traces**; grouping by model counts **observations**, because a trace has no
model. The two counts are not comparable, which is why the response says which
one you are looking at.

Percentiles are exact — nearest rank over the real samples, computed on the
fly. `total_cost` is summed only over rows whose client provided a cost and is
absent when none did. A range with nothing in it comes back with no buckets
rather than with fabricated zeroes.

## Prompt version diff

```sh
curl … "http://localhost:4318/api/v1/prompts/support/diff?from=1&to=3"
```

```json
{
  "name": "support",
  "from": 1,
  "to": 3,
  "diff": "--- prompt (version 1)\n+++ prompt (version 3)\n@@ -1 +1 @@\n-\"Be brief.\"\n+\"Be brief and cite the source.\"\n"
}
```

A unified patch over the pretty-printed `prompt` **and** `config` of the two
versions — a run can change because the text changed or because the parameters
did, so both are diffed. Empty when the two versions are identical.

## Self-description

`GET /api/v1` returns the endpoint map. `GET /api/v1/openapi.json` returns a
hand-authored OpenAPI 3.1 document — the contract itself, not a rendering of
the code, kept honest by a test that fails when the document and the router
disagree in either direction. Neither needs a key.

## System

```sh
curl … "http://localhost:4318/api/v1/system"
```

Version, uptime, the database's size on disk, row counts, the writer queue's
depth, and — since this process started — how many batches and spans arrived
per attribute dialect, how many were skipped, and every distinct
`x-langfuse-ingestion-version` seen. The counters are in memory and say so:
`counters.since` is when they started.

The row counts and the ingest counters are **your project's**: a project key is
a tenant credential, so this does not report how much data anybody else holds,
how much traffic they send, which SDK versions they run, or how many keys they
have. `payloads` is absent because that table has no project to attribute a row
to, and `projects` is a bare count of how many tenants share this process — it
names none of them.

`size_bytes` is the one deployment-wide number: it is the file on disk, which
is the operator question this endpoint exists to answer, and payloads and
compression are shared so it cannot be split per project.

This is the endpoint to read first when something looks wrong, and the one to
paste into a bug report.

## Errors

One shape everywhere:

```json
{"error": "unknown query parameter \"trace\" (accepted: from, to, environment, …)"}
```

| Status | Meaning |
|---|---|
| `400` | The request cannot mean what it says: an unknown parameter, a malformed value, a `limit` out of range. |
| `401` | The credentials do not resolve to a project. |
| `404` | No such thing in this project. On `traces/last`, the message names the filters that found nothing. |
| `409` | An observation id that is ambiguous without a `trace_id`. |
