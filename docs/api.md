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
| `GET` | `/api/v1/sessions` | List sessions by most recent activity |
| `GET` | `/api/v1/sessions/{id}` | One session: totals and traces |
| `GET` | `/api/v1/stats` | Counts, errors, cost, latency percentiles |
| `GET` | `/api/v1/prompts/{name}/diff` | Unified diff between two prompt versions |
| `GET` | `/api/v1/system` | Version, uptime, database size, ingest counters |
| `GET` | `/api/v1` | This endpoint map |
| `GET` | `/api/v1/openapi.json` | The OpenAPI document |

Scores and prompts have their own pages: [scores.md](scores.md),
[prompts.md](prompts.md). So does administration — projects, keys, retention
windows and user-data erasure — under `/api/v1/projects`:
[admin.md](admin.md) and [retention.md](retention.md).

Everything under `/api/v1/projects` that destroys something is a dry run until
`?confirm=` echoes the name of what it destroys. That contract is described
once, in [admin.md](admin.md#dry-run-by-default).

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
      "release": "2026.8.30",
      "version": "support-v9",
      "tags": ["beta", "support"],
      "timestamp": "2026-09-01T10:00:00Z",
      "total_cost": 0.001,
      "latency_ms": 820,
      "ttft_ms": 388,
      "error_count": 1,
      "observation_count": 7
    }
  ],
  "next_cursor": "MTc4ODIyMDgwMDAwMDAwMDAwMDo0Zjhj…"
}
```

Rows carry the aggregate columns and never a payload: a listing is for
choosing what to fetch.

`release` and `version` are what the deployment called itself and what the
trace's own logic called itself; `ttft_ms` is the wait before the first token
— the earliest completion start among the trace's observations, minus when the
trace began. Each is absent when nothing reported it.

### Filters

| Parameter | Meaning |
|---|---|
| `from`, `to` | RFC 3339. Half-open — `from` inclusive, `to` exclusive — so walking a timeline never reports a trace twice. |
| `environment` | Exact match. |
| `user_id`, `session_id`, `name` | Exact match. |
| `tag` | Repeatable; a trace must carry **every** tag given. |
| `status` | `error` (at least one failed observation) or `ok`. |
| `min_cost` | Traces whose total cost is at least this. A trace whose client provided no cost has none and never matches. |
| `q` | Full-text search over what the observations carried. See [Search](#search). |
| `release`, `version` | Exact match on the deployment, and on the version of the trace's own logic. |
| `type` | Traces with at least one observation of this kind — one of `span`, `generation`, `event`, `agent`, `tool`, `chain`, `retriever`, `guardrail`, `evaluator`, `embedding`. Exact: `generation` does not match `embedding`. Anything else is a `400`. |
| `prompt` | `name`, or `name@version`: traces with at least one observation that ran this prompt, at any version or at that one. A version is a **run of digits after the last `@`, with a name in front of it**; every other string is a name, `@` included — `@acme/support`, `team@acme/answer`, `name@latest` and `svc@-1` all filter as names. Labels are not versions: `name@latest` matches a prompt literally called that, and otherwise returns nothing. |
| `fields` | Comma-separated subset of the row fields. |
| `limit` | 1–500, default 50. |
| `cursor` | The `next_cursor` or `prev_cursor` of a previous page. |
| `direction` | `next` (default) or `prev`. See [Paging](#paging). |
| `count` | `1` adds `total` and `total_capped`. |

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

Cursors are opaque; pass one back as `?cursor=`. They are a keyset over
`(timestamp, id)`, so a trace ingested mid-walk cannot make a page skip or
repeat a row — and page four hundred costs what page one costs, which no
`OFFSET` can promise.

Every listing answers with two of them:

- **`next_cursor`** — the page after this one, towards older rows. `null` on
  the oldest page.
- **`prev_cursor`** — the page before it, towards newer rows, fetched with
  `?direction=prev`. `null` on the newest page.

`direction` also names the two ends, because **with no cursor it is an
anchor**:

```sh
curl … "/api/v1/traces?limit=50"                  # the newest page
curl … "/api/v1/traces?limit=50&direction=prev"   # the oldest page
```

The oldest page is a full page ending at the oldest row, not the remainder
that walking forward happens to stop on: with five rows and pages of two,
walking forward ends on one row and `direction=prev` answers with two. Both
end on the same row.

Rows always come back newest first, whichever direction the page was fetched
in: the direction is how a page was found, not how it is read.

### Counting

`?count=1` adds two fields to a listing:

```json
{ "traces": [ … ], "total": 847, "total_capped": false }
```

`total` counts what the **filters** match — not the page, and not what is
left after the cursor. It stops at 1000: `total_capped: true` means the real
number is larger and the count did not go looking for it.

That cap bounds the **answer**. It bounds the work only where matches are
plentiful, because `LIMIT` ends a scan once that many rows have *matched*: a
selective filter over a column no index covers (`name`, `tag`, `status`,
`min_cost`) is read to the end, and the session count — which has to form its
groups before it can count them — is barely bounded at all. Measured on
500 000 rows: an unfiltered trace count 1 ms, one matching nothing 260 ms; an
unfiltered session count 23 ms, one inside a 1 % time window 719 ms.

Worth the perspective, though: the listing beside it reads the same rows and
then sorts them — 864 ms for that same 260 ms count. A count is a fraction of
a screen that is already expensive for that filter, never the reason it is.

It is off by default because the answer changes with the filters and not with
the page: a client asks for it when the filters move, and pages without it.

`GET /api/v1/sessions/{id}` pages the same way but takes no `count`: its
`trace_count` is that number already, and exactly.

## Search

Every filter above asks about a trace's labels — who, when, where, how much.
`q` asks about what was *said*: the error message somebody pasted, a sentence
the model should not have produced, an order id from a support ticket.

```sh
curl -H "Authorization: Bearer tp-sk-…" \
  "http://localhost:4318/api/v1/traces?q=%22refund+failed%22&status=error"
```

It searches, per observation, the `input`, `output` and `metadata` payloads,
the observation's `name` and its `status_message` — and the trace's own `name`.
A trace matches when **one field of one of its observations** matches.

Of a payload it searches the **values**, not the JSON around them: the strings,
numbers and booleans it carries, wherever they are nested. `role`, `content`
and every other key of a message array are structure, not text, and they match
nothing.

Each row then carries `match`: where the hit was, and the text around it.

```json
{
  "id": "4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f",
  "name": "support-chat",
  "match": {
    "observation_id": "2b3c4d5e6f7a8b9c",
    "field": "output",
    "snippet": "…the refund failed for the order because the card issuer declined the…"
  }
}
```

`field` is one of `input`, `output`, `metadata`, `name`, `status_message` or
`trace_name`; with `trace_name` the `observation_id` is `null` and the trace
itself is what matched. The snippet is at most 160 characters, cut on word
boundaries around the first term and out of the same text the index searched —
so it reads as a sentence rather than as the JSON it arrived in — and it is
**plain text**: the hit is not marked up, because the API answers with data and
a client that highlights folds the query terms itself. `match` is a row field like the others: it
answers to `?fields=`, and it is never present without a `q`.

### What is and is not matched

- **Words, not substrings.** `error` does not find `errors`; `err*` finds both.
- **Case and diacritics are folded.** `Refund`, `refund` and `réfund` are one
  word.
- **Several words are ANDed, in any order.** `refund order` finds a field
  containing both; `"refund order"` finds them adjacent, in that order.
- **Identifiers split on punctuation** and are found whole or by part:
  `user_id_42` is found by `user_id_42`, by `user` and by `42`.
- **A payload's keys and structure are not searched.** `{"role": "user",
  "content": "the refund failed"}` is found by `refund` and by `user`, and not
  by `role` or `content`. A number is found by its digits: an `order_id` of
  `12345` answers to `12345`. A payload that is not JSON is searched whole.
  The values of one field are one text, so a `"quoted phrase"` can run from the
  end of one value into the start of the next.
- **Only the first 64 KiB of each payload's text is indexed** — the text inside
  the JSON, not the JSON. A word past that is stored and readable but not
  findable; `/observations/{id}/io` still returns the whole thing. See
  [retention.md](retention.md#what-search-costs).
- **All the words must occur in the same field of the same observation.** Two
  words in two different observations of one trace are not a match.

There is no operator syntax and nothing to escape: words, `"quoted phrases"`
and a trailing `*` are the whole language, and everything else — `AND`, `OR`,
`NOT`, parentheses, `:`, `^`, `-` — is literal text. The only `q` this endpoint
refuses is one with no word in it, or one over 512 characters, and both are a
`400` rather than an empty listing.

`q` changes what is listed and nothing else: the rows stay newest first, the
cursors are the same keyset, and the count is the count with the search. A
cursor taken with a `q` is valid only with the same `q`, which is already how
every other filter behaves. There is no relevance ordering — the useful order
for "when did this last happen?" is the one the listing already has.

`GET /api/v1/traces/last` takes `q` too, which is "the last trace that said
this", whole, in one request. It returns the trace as `GET /traces/{id}` does,
without a `match`: the payloads are in the response already.

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
          "completion_start_time": "2026-09-01T10:00:00.428Z",
          "ttft_ms": 388,
          "model": "claude-sonnet-5",
          "model_parameters": {"temperature": 0.2},
          "level": "DEFAULT",
          "usage": {"input": 128, "output": 41, "total": 169},
          "cost_details": {"input": 0.00038, "output": 0.00062},
          "prompt": {"name": "support-answer", "version": 7},
          "input_bytes": 12480,
          "output_bytes": 3011
        }
      ]
    }
  ]
}
```

An observation whose parent has not arrived yet renders at the root with its
`parent_observation_id` intact — a trace still being written looks incomplete,
not empty. `children` is absent for a leaf.

`prompt` is the prompt your client said this observation ran, recorded as sent
and resolved against no registry: this store may not manage that prompt at
all, and a label pointing at nothing is still a label. Its `version` is null
when the client named a prompt without a whole-number version.
`input_bytes` and `output_bytes` are the uncompressed payload sizes — present
whether or not `?expand=io` inlined the payloads themselves, because they are
what you decide with before asking for the rest.

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

A session is not a stored entity: it is the set of traces that named it, and
both endpoints below aggregate those traces on the way out.

```sh
curl … "http://localhost:4318/api/v1/sessions?environment=production&limit=2"
```

```json
{
  "sessions": [
    {
      "id": "session-77",
      "trace_count": 12,
      "error_count": 1,
      "total_cost": 0.043,
      "first_seen": "2026-09-01T10:00:00Z",
      "last_seen": "2026-09-01T10:14:22Z"
    }
  ],
  "next_cursor": "MTc4ODIyMDgwMDAwMDAwMDAwMDpzZXNzaW9uLTc3"
}
```

Most recent activity first (`last_seen DESC`, with the session id as the
tie-break), paginated by the same opaque cursor as every other listing.

| Filter | Meaning |
|---|---|
| `from`, `to` | RFC 3339, half-open, on the **traces**: a session appears when any of its traces falls in the window, and its totals then describe those traces. |
| `environment`, `user_id` | Exact match on the session's traces. |

A trace that named no session is not a session of one and never appears.

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

`group_by` is `hour`, `day`, `model`, `environment` or `release` (default
`day`), and `unit` says what a bucket counts. Grouping by hour, day,
environment or release counts **traces**; grouping by model counts
**observations**, because a trace has no model. The two counts are not
comparable, which is why the response says which one you are looking at.

Grouped by release, the traces that named none fall in the bucket whose `key`
is the empty string — that is a group, not a gap, and dropping it would make
the numbers stop adding up.

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
