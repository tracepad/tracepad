# Scores

A score is a quality judgement attached to a trace, an observation or a
session: a number from an eval run, a pass/fail from a guardrail, a verdict
from an LLM judge, a rating from a human reviewer.

Scores are a plain JSON API — no SDK, no protobuf, and everything below works
with `curl`.

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/api/v1/scores` | Write one score or an array of them |
| `GET` | `/api/v1/scores` | List scores, filtered and paginated |
| `GET` | `/api/v1/scores/{id}` | Fetch one score |

Authentication is the same as for ingest — `Authorization: Bearer <secret
key>` or `Basic base64(<public key>:<secret key>)`. See
[ingest.md](ingest.md#authentication).

No `Content-Type` is required: `curl -d '{…}'` works as it is.

## Writing a score

```sh
curl -H "Authorization: Bearer tp-sk-…" http://localhost:4318/api/v1/scores -d '{
  "trace_id": "4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f",
  "name": "helpfulness",
  "value": 0.9,
  "comment": "answered the question and cited the source",
  "metadata": {"judge_model": "claude", "run": "nightly-2026-08-27"}
}'
```

```json
{"ids": ["8673743e8ca15b9213d541a703786e86"]}
```

The response always carries an array, in input order, for a single object as
well as for a batch.

### Fields

| Field | Required | Notes |
|---|---|---|
| `id` | no | 32 lower-case hex characters. Omitted, the server generates one. Writing the same id again **replaces** the score. |
| `trace_id` | — | At least one of `trace_id` and `session_id` is required. |
| `observation_id` | no | Requires `trace_id`: an observation is addressed inside its trace. |
| `session_id` | — | See `trace_id`. |
| `name` | yes | 1–200 characters. The name you will filter and chart by. |
| `data_type` | no | `numeric`, `boolean`, `categorical` or `text`. Inferred when absent — see below. |
| `value` | depends | The number, for `numeric` and `boolean` (`0` or `1`). |
| `string_value` | depends | The string, for `categorical` and `text`. |
| `comment` | no | Free text — a judge's rationale, a reviewer's note. |
| `metadata` | no | Any JSON value, stored inline and returned verbatim. |
| `timestamp` | no | RFC 3339. **Event time**: when the graded interaction happened. Defaults to receive time. |

`data_type` is inferred when you leave it out: a `value` makes the score
`numeric`, a `string_value` makes it `text`. `boolean` and `categorical` are
claims about meaning that only you can make, so they must be stated. A score
carries `value` or `string_value` — never both.

Unknown fields are rejected with a `400` naming the field. That is deliberate:
an unattended agent that writes `commet` should be told, not silently lose the
comment.

### Idempotency and corrections

A score id is the idempotency key. Re-posting the same id replaces the row
whole, so retrying a failed batch is safe and a correction is just another
POST — there is nothing to delete first.

If your eval loop already has a natural key, hash it into the id:

```python
score_id = hashlib.sha256(f"{run_id}:{trace_id}:helpfulness".encode()).hexdigest()[:32]
```

### Batches

An array is all-or-nothing: one transaction, and one `400` naming the first
item it refused.

```sh
curl -H "Authorization: Bearer tp-sk-…" http://localhost:4318/api/v1/scores -d '[
  {"trace_id": "4f8c…", "name": "helpfulness", "value": 0.9},
  {"trace_id": "4f8c…", "name": "grounded", "data_type": "boolean", "value": 1},
  {"trace_id": "4f8c…", "name": "verdict", "string_value": "cites its sources"}
]'
```

An empty array is a `400`: nothing to write is a client bug, not a no-op.

### Scoring a trace that has not arrived yet

Nothing checks that the target exists. Evals run asynchronously and may grade a
trace whose spans are still in flight; refusing the score would force you to
poll. A score whose trace never arrives stays visible in listings and is
removed by retention on its own `timestamp`.

## Reading scores

```sh
curl -H "Authorization: Bearer tp-sk-…" \
  "http://localhost:4318/api/v1/scores?name=helpfulness&from=2026-08-01T00:00:00Z&limit=100"
```

```json
{
  "scores": [
    {
      "id": "8673743e8ca15b9213d541a703786e86",
      "trace_id": "4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f",
      "name": "helpfulness",
      "data_type": "numeric",
      "value": 0.9,
      "comment": "answered the question and cited the source",
      "metadata": {"judge_model": "claude"},
      "timestamp": "2026-08-27T10:00:00Z",
      "created_at": "2026-08-27T10:00:02.114Z"
    }
  ],
  "next_cursor": null
}
```

Scores come back newest first. Fields that were never set are omitted.

| Parameter | Notes |
|---|---|
| `trace_id`, `observation_id`, `session_id`, `name`, `data_type` | Exact-match filters |
| `from`, `to` | RFC 3339, on `timestamp`. `from` is inclusive, `to` is exclusive, so walking day by day never counts a score twice. |
| `limit` | 1–500, default 50 |
| `cursor` | The `next_cursor` of the previous page |

Pagination is by cursor, not offset: a score written while you are walking
cannot make the next page skip or repeat a row. Keep passing `next_cursor`
until it comes back `null`.

An unknown query parameter is a `400` — the same reasoning as unknown JSON
fields.

## Responses

| Status | Meaning |
|---|---|
| `201` | Written, committed and fsynced to disk. |
| `200` | The read succeeded. |
| `400` | Validation: a bad field, an unknown field, an unknown parameter, an invalid cursor. The message says which. |
| `401` | Unknown credentials. |
| `404` | No score with that id in this project. |
| `413` | The body is over `TRACEPAD_MAX_BODY_BYTES`. |
| `429` | The write queue is saturated; retry after the `Retry-After` delay. |

A `201` means the score is on disk, not merely queued — the same guarantee a
`200` gives on the ingest endpoint.
