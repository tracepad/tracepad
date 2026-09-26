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

A browser sends a session cookie instead, and names the project it is asking
about with `X-Tracepad-Project`; every route says which of the two — and which
role — it takes, in one word. See [accounts.md](accounts.md#who-may-do-what).
An `Authorization` header always wins over a cookie, so nothing below changes
for a script.

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
| `DELETE` | `/api/v1/traces/{id}` | Delete one trace; a dry run until confirmed with its id |
| `DELETE` | `/api/v1/traces?…&to=` | Delete every trace the filters match before `to`, in rounds; a dry run until confirmed with the project name |
| `GET` | `/api/v1/observations/{id}/io` | One observation's payloads, whole |
| `GET` | `/api/v1/sessions` | List sessions by most recent activity |
| `GET` | `/api/v1/sessions/{id}` | One session: totals and traces |
| `GET` | `/api/v1/users` | List users by last seen, traffic, cost or errors |
| `GET` | `/api/v1/users/{id}` | One user: traffic, sessions, cost, errors, latency |
| `GET` | `/api/v1/runs` | List the project's eval runs, newest first, across datasets |
| `GET` | `/api/v1/queues` | List the annotation queues with their progress |
| `GET` | `/api/v1/queues/{name}/next` | The next item to annotate, claimed for ten minutes |
| `GET` | `/api/v1/stats` | Counts, errors, cost, latency percentiles |
| `GET` | `/api/v1/prompts/{name}/diff` | Unified diff between two prompt versions |
| `DELETE` | `/api/v1/prompts/{name}` | Delete a prompt name whole; a dry run until confirmed |
| `DELETE` | `/api/v1/scores/{id}` | Retract one score; no dry run, a re-POST puts it back |
| `GET` | `/api/v1/system` | Version, uptime, database size, ingest counters |
| `GET` | `/api/v1` | This endpoint map |
| `GET` | `/api/v1/openapi.json` | The OpenAPI document |

Scores and prompts have their own pages: [scores.md](scores.md) (score
configs included), [prompts.md](prompts.md). So do datasets and runs — the
cases an eval ran and the container that groups the traces one pass produced,
under `/api/v1/datasets` and `/api/v1/runs`: [datasets.md](datasets.md). So
does administration — projects, keys, retention windows and user-data erasure
— under `/api/v1/projects`: [admin.md](admin.md) and
[retention.md](retention.md). A project key administers its own project there,
but for its keys: `GET`, `POST` and `DELETE` under `/api/v1/projects/{id}/keys`
answer a key `403` — `a project key cannot list, mint or revoke keys; that
needs an owner or editor signed in, or the admin token` — and the listing says
who minted each key and when it was last used ([admin.md](admin.md#keys)). And
so do the annotation queues — what a team
has decided deserves a human verdict, and who has given one — under
`/api/v1/queues`: [annotation.md](annotation.md).

Everything under `/api/v1/projects` that destroys something is a dry run until
`?confirm=` echoes the name of what it destroys, and so is deleting a dataset,
an annotation queue, or [traces](#deleting-traces).
That contract is described once, in [admin.md](admin.md#dry-run-by-default).
The ceremony is for what cannot be undone: deleting one score takes no
`?confirm=`, because writing its id again recreates it.

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

A trace produced by an eval also carries `run_id` and `item_id` — the run it
belongs to and the case it answered, from the `tracepad.run_id` and
`tracepad.item_id` attributes the harness stamped. Both are absent on ordinary
traffic, and `item_id` never appears without `run_id`. See
[Datasets and runs](datasets.md).

### Filters

| Parameter | Meaning |
|---|---|
| `from`, `to` | RFC 3339. Half-open — `from` inclusive, `to` exclusive — so walking a timeline never reports a trace twice. |
| `environment` | The environment a trace ran in, or a comma-separated list matching **any** of them: `?environment=production,staging`. See [Lists](#lists). |
| `user_id`, `session_id` | Exact match. |
| `name` | The trace name, or a comma-separated list matching any of them. A trace with no name never matches. See [Lists](#lists). |
| `tag` | Repeatable; a trace must carry **every** tag given. |
| `status` | `error` (at least one failed observation) or `ok`. |
| `min_cost` | Traces whose total cost is at least this. A trace whose client provided no cost has none and never matches. |
| `q` | Full-text search over what the observations carried. See [Search](#search). |
| `release` | The deployment the trace ran in, or a comma-separated list matching any of them. See [Lists](#lists). |
| `version` | Exact match on the version of the trace's own logic. |
| `type` | Traces with at least one observation of this kind — one of `span`, `generation`, `event`, `agent`, `tool`, `chain`, `retriever`, `guardrail`, `evaluator`, `embedding`. Exact: `generation` does not match `embedding`. Anything else is a `400`. |
| `prompt` | `name`, or `name@version`: traces with at least one observation that ran this prompt, at any version or at that one. A version is a **run of digits after the last `@`, with a name in front of it**; every other string is a name, `@` included — `@acme/support`, `team@acme/answer`, `name@latest` and `svc@-1` all filter as names. Labels are not versions: `name@latest` matches a prompt literally called that, and otherwise returns nothing. |
| `run_id`, `item_id` | The traces of one eval run, and the attempts at one case ([datasets.md](datasets.md)). Both take the 32-hex ids this API issues; another shape is a `400`, because nothing else can be in those columns and an empty listing would report a typo as a fact. |
| `fields` | Comma-separated subset of the row fields. |
| `limit` | 1–500, default 50. |
| `cursor` | The `next_cursor` or `prev_cursor` of a previous page. |
| `direction` | `next` (default) or `prev`. See [Paging](#paging). |
| `count` | `1` adds `total` and `total_capped`. |

A parameter the endpoint does not know is a `400`, and so is one sent without
a value (`?environment=` is what an unset shell variable expands to, and
answering it with the whole project would be a wider answer to a narrower
question).

#### Lists

`environment`, `release` and `name` take **one value or a comma-separated
list**, and a trace matches when its column equals any item:

```sh
curl … "http://localhost:4318/api/v1/traces?environment=production,staging"
```

One value behaves exactly as it always did. Items are trimmed, so
`a, b` is the pair it looks like, and duplicates collapse. An **empty item**
— `a,,b`, `a,` — is a `400` for the reason an empty value is: it is a template
that did not fill in, and reading it as "just the rest" would answer a broken
request with a well-formed listing.

**Repeating the parameter** is a `400` too: `?environment=a&environment=b` is
one list spelled the way `tag` is spelled, and answering it with the traces of
`a` alone would be a listing narrower than the one that was asked for. One
list, one parameter.

A list carries **at most 100 items**; beyond that the request is a `400`
(`environment: at most 100 values in a list`). That is the same hundred
[`/facets`](#filter-values) offers per column, so it is not a limit the
interface can reach — and out of range is an error rather than a silent
truncation, exactly as it is for `limit`.

A value that **contains a comma** is not expressible through these parameters.
An environment, a release or a trace name is an identifier, and an identifier
with a comma in it is a choice its owner made against every tool that will
ever list it.

`tag` is not a list of this kind: it stays repeatable and stays an **AND**, so
`?tag=a&tag=b` keeps the traces carrying both. Two spellings for two semantics
is clearer than one spelling for both.

[`GET /api/v1/facets`](#filter-values) lists what these three can be set to.

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

## Deleting traces

```sh
curl -X DELETE … "http://localhost:4318/api/v1/traces/4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f"
curl -X DELETE … "http://localhost:4318/api/v1/traces/4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f?confirm=4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f"
```

```json
{
  "dry_run": true,
  "would_delete": {"traces": 1, "observations": 7, "scores": 2, "annotation_items": 1},
  "oldest": "2026-09-01T10:00:00Z",
  "affected_runs": [],
  "confirm": "4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f",
  "note": "raw OTLP bodies are not deleted; they expire on the raw retention window"
}
```

`DELETE /api/v1/traces/{id}` removes one trace and everything attached to it
— observations, scores, payloads, search entries, annotation-queue items —
with the hour it started in re-rolled in the same transaction. An editor's
route. Without `confirm` it answers the dry run above; the echo is the trace
id, its only identity. With it: `{"dry_run": false, "deleted": {"traces": 1,
"observations": 7, "scores": 2, "payloads": 9, "annotation_items": 1}, "id":
"4f8c…"}`. An unknown id is `404` either way, a wrong echo `400`. When an
eval run holds the trace it is deleted all the same, and `affected_runs`
names the run in the dry run ([datasets.md](datasets.md#what-a-run-keeps)).

```sh
curl -X DELETE … "http://localhost:4318/api/v1/traces?environment=loadtest&to=2026-09-17T14:02:17Z"
curl -X DELETE … "http://localhost:4318/api/v1/traces?environment=loadtest&to=2026-09-17T14:02:17Z&confirm=checkout-service&limit=1000"
```

`DELETE /api/v1/traces` takes every [filter of the listing](#filters), with
the same validation, and **requires `to`** — dry run and confirmed alike, a
`400` without it — so the set is closed: what the preview counted is what is
deleted, however much ingest flows in between. The dry run adds `matched`,
the exact count (the listing's own stops at a thousand), and its echo is the
**project name**. A confirmed request deletes **one round** — the newest
`limit` matches, 1–1000 and 1000 by default, in chunks of one hour, and at
most fifty chunks — and answers `{"dry_run": false, "deleted": {…}, "more":
true}`; repeat the same call while `more` is true. Nothing is recounted on the way, a filter that
matches nothing is a successful dry run of zero and a successful deletion of
nothing, and a repeat after `more: false` is harmless. The whole of it — what
goes, what stays, the ingest race — is in
[admin.md](admin.md#deleting-traces).

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

## The raw archive

Every export body Tracepad accepts is kept as it arrived, and these two
endpoints are how it leaves. They are the project's own reads under the
project's own keys — a key that reads traces reads the batches they came from —
and they are what [`tracepad export --otlp`](export.md) is a client of. Anything
else can be one too: a backup job, a script, a second Tracepad.

```sh
curl … "http://localhost:4318/api/v1/raw?limit=2&count=1"
```

```json
{
  "batches": [
    {"id": 1, "received_at": "2026-09-01T00:00:00Z", "dialect": "langfuse",
     "content_type": "application/x-protobuf", "content_encoding": "gzip",
     "size_bytes": 1274},
    {"id": 2, "received_at": "2026-09-01T00:00:00.001Z", "dialect": "genai",
     "content_type": "application/json", "content_encoding": "",
     "size_bytes": 3810}
  ],
  "next_cursor": "MTc4ODIyMDgwMDAwMTAwMDAwMDoy",
  "prev_cursor": null,
  "total": 12400,
  "total_capped": false
}
```

**This is the one listing here that runs forward.** Everything else in this API
is newest first, because that is how a person reads. A replay is not reading: a
receiver that does not upsert by span id — a file, a stream — has to see the
spans in the order the world produced them, and `received_at` is the only order
this server knows. So `next` walks towards *newer* batches and `prev` back
towards older ones, and rows come back oldest first either way.

| Parameter | |
|---|---|
| `since`, `until` | RFC 3339, on `received_at`. Half-open: `since` inclusive, `until` exclusive. |
| `limit` | 1–500, default 100. |
| `cursor`, `direction` | Keyset over `(received_at, id)`. |
| `count` | Adds `total` and `total_capped`, counted up to 100000 — high, because this count answers "how much is this export about to send". |

`size_bytes` is the **decoded** length, which is what a fetch of the body
returns; the row itself is compressed and smaller. `content_type` is what the
body is in, and a batch stored before schema 0012 reads as
`application/x-protobuf`, which is the only thing it can be. `dialect` is which
attribute vocabulary the mapper recognised, and is empty when it claimed
nothing.

A cursor and a `since` that contradict each other are a `400` rather than a
reconciliation: the cursor says where the page starts and so does the window,
and guessing which was meant is worse than asking.

### One body

```sh
curl … -o batch.pb "http://localhost:4318/api/v1/raw/1"
```

The bytes the client posted, with gzip already removed — the stored body is the
decoded one, so a replay does not have to unwrap two layers. The response
carries:

| | |
|---|---|
| `Content-Type` | The type it was received in, which is what a replay posts it under |
| `X-Tracepad-Received-At` | RFC 3339 |
| `X-Tracepad-Dialect` | Absent when the mapper claimed nothing |

Like `/observations/{id}/io`, this endpoint is **exempt from the response
budget**: a cut body is not a smaller batch, it is a broken one.

A `404` means the id is not this project's, or the retention sweeper has
already taken it. Both answer the same way, because a batch that is not yours
does not exist to you.

The archive stores each body with its media factored out, and this endpoint
puts it back: each reference becomes what it replaced again — the base64 in an
Anthropic, Gemini or GenAI object, the data URL of a string — so what leaves is
the batch the client sent ([media.md](media.md#the-way-out)). `size_bytes` above is
the stored length, before that.

## Media

```sh
curl … -o picture.png "http://localhost:4318/api/v1/media/3f2a…c91e"
```

Ingest takes images and files out of the payloads and leaves a reference
object in their place ([media.md](media.md)):

```json
{"tracepad_media": "3f2a…c91e", "mime_type": "image/png", "size": 48213}
```

Every read — a trace, an observation's payloads, search, the CLI, MCP —
returns that object as it is; this endpoint is where the bytes are. It answers
them in the MIME type this project stored them under, with
`Cache-Control: private, max-age=31536000, immutable` (with `Vary:
Authorization, Cookie, X-Tracepad-Project`, so a browser's cache never answers
one project with another's body), `X-Content-Type-Options: nosniff` and a
sandboxing `Content-Security-Policy`; anything that is not an
image, audio or video comes as an attachment. It answers only a project that
points at the body, from a trace or a raw batch: any other hash is `404`, the
same as one nobody holds. A reference with `"stored": false` has no bytes to
fetch.

The Langfuse SDK's media channel (`/api/public/media`) is described in
[media.md](media.md#the-langfuse-sdks-media-channel).

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
| `environment` | The environment of the session's traces, or a comma-separated list matching any of them ([Lists](#lists)). |
| `user_id` | Exact match on the session's traces. |

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

## Users

The two endpoints and everything they promise are on their own page:
[users.md](users.md). In short:

```sh
curl … "http://localhost:4318/api/v1/users?sort=cost&limit=3"
```

```json
{
  "users": [
    {
      "user_id": "user-4821",
      "traces": 312,
      "error_count": 4,
      "total_cost": 6.10,
      "sessions": 28,
      "first_seen": "2026-08-14T09:00:00Z",
      "last_seen": "2026-09-01T10:00:00Z"
    }
  ],
  "next_cursor": "Ni4xOnVzZXItNDgyMQ",
  "prev_cursor": null
}
```

`sort` is `last_seen` (the default), `traces`, `cost` or `errors`, always
descending, with the user id as the tie-break; `prefix` keeps ids starting
with it, case-sensitively. `limit`, `cursor`, `direction` and `count` are the
same as on every other listing.

The listing is answered from the per-user rollup **only**, so it trails live
traffic by the same lag the statistics do (below) — a user first seen minutes
ago is not on it yet. `GET /api/v1/users/{id}` merges the live rows and is
exact for any id, listed or not; it answers `404` when neither half has seen
it, and adds `latency_ms` to the row shape above.

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
      "tokens": {"input": 1284930, "output": 96410, "cache_read": 402118},
      "latency_ms": {"p50": 640, "p95": 2310}
    }
  ]
}
```

`group_by` is `hour`, `day`, `model`, `environment`, `release` or `total`
(default `day`), and `unit` says what a bucket counts. Grouping by hour,
day, environment, release or total counts **traces**; grouping by model
counts **observations**, because a trace has no model. The two counts are
not comparable, which is why the response says which one you are looking at.

Grouped by release, the traces that named none fall in the bucket whose `key`
is the empty string — that is a group, not a gap, and dropping it would make
the numbers stop adding up.

Grouped by `total`, the whole window is **one bucket** under the empty key,
in the same shape as a day bucket: the count, the errors, the cost and the
tokens summed, and `p50`/`p95` merged over every hour's histogram in the
window — which is not the same number as a percentile of the daily
percentiles. It takes every filter the other groupings take. A window with
nothing in it has no bucket, as everywhere. This is the figure the
dashboard's summary row shows; the change against the previous window is two
requests and a subtraction the caller does — there is no `compare`.

```sh
curl … "http://localhost:4318/api/v1/stats?group_by=total&from=2026-09-08T00:00:00Z"
```

`user_id` restricts every bucket to one end user. The shape, the groupings and
`unit` do not change; with `group_by=hour`, `group_by=day` or
`group_by=total` each bucket additionally carries `sessions` — how many of
that user's sessions *began* in it, so a sum over any range is exact. Without
the filter the key is absent rather than zero, because the statistics rollup
holds no such number. The per-user answer trails the raw data by the same lag
as the rest.

```sh
curl … "http://localhost:4318/api/v1/stats?group_by=day&user_id=user-4821"
```

Latency percentiles are **histogram-based**: accurate to a few percent, and
stable across the expiry of the rows they came from. `total_cost` is summed
only over rows whose client provided a cost and is absent when none did. A
range with nothing in it comes back with no buckets rather than with
fabricated zeroes.

`tokens` is the same idea for the one number every provider reports: the
sums of **input**, **output** and **cache-read** tokens over the generations
in the bucket, on every grouping and both units. Each count is read off an
observation's `usage` under the first spelling present of a short list —
`input_tokens`, `prompt_tokens` or `input`; `output_tokens`,
`completion_tokens` or `output`; `cache_read_input_tokens`,
`cache_read_tokens` or `input_cached_tokens` — so the OpenAI, Anthropic and
Langfuse spellings land in the same three numbers. A key is present only when
something in the bucket carried that count, and the object is absent when
none of the three is: a bucket whose calls reported no usage says nothing
rather than zero. With `user_id` the object is always absent — the per-user
rollup holds no token sums. Cache-read tokens are the input tokens a provider reported
as served from its cache; a bill is made of input and output, and cache read
is what explains one that is smaller than the tokens suggest. Reasoning and
cache-creation counts are not summed — they stay on the observation, where
the interface shows them. Hours that were rolled up before this store learned
about tokens are re-rolled on the next pass, except an hour past the
project's `retention_days`, whose observations are gone: it keeps no tokens
for ever.

### Where the numbers come from

Closed hours are rolled up in the background and answered from that rollup;
the hour in progress is always answered live. Three consequences worth
knowing:

- The statistics **trail the raw data by up to twice the rollup interval**
  (`TRACEPAD_ROLLUP_INTERVAL`, five minutes by default, so ten in the worst
  case) for hours that have closed: an hour becomes eligible one interval
  after it ends, and the pass that takes it can be a further interval away.
  The current hour is never stale — it is answered live.
- Ranges older than the project's `retention_days` **keep answering** — from
  the rollup — for as long as `stats_retention_days` allows. This is the
  point of the rollup: configuring retention no longer amputates the charts.
- A trace that arrives late for an hour already rolled is picked up by the
  next pass. Past the retention window that hour is **frozen**: the raw rows
  behind it are gone by design, so the late fragment appears in the listings
  but does not rewrite the history. See
  [retention.md](retention.md#what-outlives-what).

## Score trends

```sh
curl … "http://localhost:4318/api/v1/stats/scores?group_by=day&from=2026-09-01T00:00:00Z"
```

```json
{
  "group_by": "day",
  "targets": "any",
  "omitted": 0,
  "series": [
    {
      "name": "hallucination",
      "data_type": "numeric",
      "buckets": [
        {"key": "2026-09-01", "count": 412, "mean": 0.18, "min": 0.0, "max": 0.9}
      ]
    },
    {
      "name": "verdict",
      "data_type": "categorical",
      "buckets": [
        {"key": "2026-09-01", "count": 412, "categories": {"pass": 380, "fail": 32}}
      ]
    }
  ]
}
```

The quality curve beside the traffic one. A score is counted in the hour of the
**trace it names** — not in the hour it was graded — and takes that trace's
environment, release and model, so these buckets line up with `/api/v1/stats`'
own. A score that names only a session, and a `text` score, are not counted at
all: [quality.md](quality.md#what-is-counted-and-what-is-not) says why.

`from`, `to` and `environment` are the statistics' own filters, and `group_by`
is `hour`, `day`, `environment`, `release` or `model` (default `day`).
Without `name` every score name in the range is a series; with it, one:

```sh
curl … "http://localhost:4318/api/v1/stats/scores?name=hallucination&group_by=release"
```

What a bucket carries depends on the series' `data_type`: a `numeric` name
reports `mean`, `min` and `max`, a `boolean` name the `rate` of true values
between 0 and 1, and a `categorical` name the `categories` seen with their
counts. All three carry `count`, which is what the mean or the rate is out of.
A bucket with no scores does not exist rather than reporting zero, and one name
graded two ways is two series with the same name.

`targets` says what was counted. Grouped by model it is `observation`, because
only a score that names an observation has a model to sit under; every other
grouping counts each score once and it is `any`. It is here for the reason
`unit` is on the statistics: two counts that are not comparable must not look
alike.

Both sides of the answer are bounded, because both are unbounded client input:
a score name needs no `score_config`, and a `categorical` value is whatever the
client sent. `limit` caps how many series come back — 1 to 500, default 50, the
busiest names first — and `omitted` says how many that left out. Inside one
series only the twenty busiest values of the range are named; the rest are
summed under `other`, so a bucket's `categories` still add up to its `count`.

An unknown parameter, or a parameter given without a value, is a `400`.

The numbers come from the same seam and carry the same lag as the statistics
above, plus one addition: a score's own arrival dirties the hour of its trace,
so a judge grading yesterday's traffic is picked up by the next pass rather
than never. Deleting a score, or moving one onto another trace, corrects the
hour it leaves in the same transaction as the write itself. See
[quality.md](quality.md#the-lag).

## Filter values

```sh
curl … "http://localhost:4318/api/v1/facets?from=2026-09-01T00:00:00Z"
```

```json
{
  "from": "2026-09-01T00:00:00Z",
  "to": "2026-09-09T12:00:00Z",
  "environment": [
    {"value": "production", "count": 4656},
    {"value": "staging", "count": 218},
    {"value": "prod", "count": 1}
  ],
  "release": [{"value": "2026.9.1", "count": 3120}],
  "name": [{"value": "support-chat", "count": 2984}],
  "omitted": {"environment": 0, "release": 0, "name": 0}
}
```

What the three many-valued filters can be set to: the distinct values of
`environment`, `release` and `name` among the traces of a range, each with the
number of traces carrying it. It is what a filter panel asks before it offers a
list, and what saves a client guessing at a value it could have read.

The counts are the point as much as the values are: `prod: 1` beside
`production: 4656` is a typo, and nothing but the count says so.

`from` and `to` are the statistics' own bounds — RFC 3339, half-open. `to`
defaults to now. `from` defaults to **the oldest hour the rollup holds**, so
with no range at all the answer covers as much history as
[`stats_retention_days`](retention.md) keeps — which is what the listing covers
when its own range is unset. On a project nothing has rolled yet there is no
floor and the answer is all of history.

The `from` in the answer is the range that was actually covered, not the one
that was asked for: with no `from` given it reads back as the rollup's oldest
hour rather than the beginning of time.

That floor is also a ceiling on how far back the endpoint looks: unlike
`/stats`, it does **not** scan the raw traces for hours the rollup no longer
holds. Everywhere else that scan answers a question about a specific window; a
list of values to pick from is not worth an unbounded pass over `traces`, and
the values it would add are the ones outside the window this install chose to
keep.

The range is the *only* thing this endpoint takes: the counts do not respect
the other filters, so the list does not move as boxes are ticked. An unknown
parameter, or one given without a value, is a `400`; so is a window whose
`from` is not before its `to`.

Values are sorted by count descending and then by value ascending, and each
column carries at most **100** of them; `omitted` says how many were left out,
rarest first. A trace with no release is not a release, and a trace with no
name is not a name — neither is a value anything can be filtered by. For the
same reason the list leaves out a value that the filter could not be *given*: a
value containing a comma, or one with space at either end, cannot be spelled in
the list form, so offering it would be offering a filter that does not work.
Those traces are still reachable through `q=`.

The numbers come from the same seam as the statistics, so a range behind the
watermark is answered from the rollup and outlives the traces it summarizes.
The tail is answered live, which is what keeps the list *complete*: an
environment first seen a minute ago is already here.

One caveat, on an install that was upgraded and keeps its summaries longer than
its traces. `environment` and `release` have been in the rollup since it
existed; the trace name got a table of its own with this feature, and its
backfill can only fill an hour whose raw traces are still there. So where
`retention_days` is shorter than `stats_retention_days`, a range reaching into
the gap between the two answers the full environment and release lists beside a
name list that starts where the traces do. Everything ingested after the
upgrade has all three.

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

`GET /api/v1` returns the endpoint map: every route, a one-line description,
and the one word that says what calling it takes — `public`, `ingest`,
`member`, `editor`, `owner` or `session`
([accounts.md](accounts.md#who-may-do-what)). `GET /api/v1/openapi.json`
returns a hand-authored OpenAPI 3.1 document — the contract itself, not a
rendering of the code, kept honest by a test that fails when the document and
the router disagree in either direction. Neither needs a key.

## System

```sh
curl … "http://localhost:4318/api/v1/system"
```

Version, uptime, the database's size on disk, row counts, the writer queue's
depth, and — since this process started — how many batches and spans arrived
per attribute dialect, how many were skipped, and every distinct
`x-langfuse-ingestion-version` seen. The counters are in memory and say so:
`counters.since` is when they started.

`runs` is the link between traces and eval runs ([datasets.md](datasets.md)):
`pinned_traces` is how many traces a live run is keeping out of the retention
sweep — the size of retention's one exception — and `orphan_traces` how many
trace deliveries since start named a run this project does not have.

`media` is what the images and files ingest took out of this project's
payloads cost, beside the setting that decides whether they are kept:
`{"setting": "store", "count": 212, "bytes": 318455112}` — distinct bodies the
project's traces and raw batches point at, and their decoded size.

`raw` is the archive of export bodies — what an export can carry out, and what
it cannot:

```json
"raw": {
  "enabled": true,
  "batches": 12400,
  "bytes": 3328599654,
  "oldest_received_at": "2026-08-06T04:12:19Z",
  "newest_received_at": "2026-09-05T09:44:02Z",
  "traces_before_window": 214
}
```

Unlike the counters below it, these are on disk rather than since start.
`enabled` is `TRACEPAD_STORE_RAW`; `bytes` is what the archive occupies
compressed, which is the number an operator moving `raw_retention_days` is
deciding about; `traces_before_window` counts the traces whose earliest span
started before the oldest batch arrived, and which therefore have rows but no
body to replay. It is a lower bound — a late export of an old trace lands after
the batch line — and [export.md](export.md) says what to do about it.

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

A refusal a client has to *act* on carries what it needs beside the sentence —
a prompt append refused by `expect_version` says which version the name is
actually at ([prompts.md](prompts.md#appending-to-the-version-you-meant)):

```json
{"error": "prompt \"summarize\" is at version 9, not 7: it changed while this one was being written", "version": 9}
```

| Status | Meaning |
|---|---|
| `400` | The request cannot mean what it says: an unknown parameter, a malformed value, a `limit` out of range. |
| `401` | The credentials do not resolve to a project. |
| `404` | No such thing in this project. On `traces/last`, the message names the filters that found nothing. |
| `409` | An observation id that is ambiguous without a `trace_id`; a prompt append whose `expect_version` disagrees with the name's current state. |
