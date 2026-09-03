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
| `PUT` | `/api/v1/score-configs/{name}` | Pin what a score name means — see [Score configs](#score-configs) |
| `GET` | `/api/v1/score-configs` | List the configs |
| `GET` | `/api/v1/score-configs/{name}` | One config |
| `DELETE` | `/api/v1/score-configs/{name}` | Remove a config |

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
| `timestamp` | no | RFC 3339, between 1678 and 2262 (what Unix nanoseconds can represent). **Event time**: when the graded interaction happened. Defaults to receive time. |

`data_type` is inferred when you leave it out: a `value` makes the score
`numeric`, a `string_value` makes it `text`. `boolean` and `categorical` are
claims about meaning that only you can make, so they must be stated. A score
carries `value` or `string_value` — never both.

Unknown fields are rejected with a `400` naming the field. That is deliberate:
an unattended agent that writes `commet` should be told, not silently lose the
comment.

A score whose `name` has a [config](#score-configs) is checked against it —
type, range, categories — and a violation is a `400` naming the item and the
rule. A name without a config is as free as the table above.

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

An empty array is a `400`: nothing to write is a client bug, not a no-op. So is
an array that gives the same `id` to two items — writes upsert by id, so the
response would promise more rows than were stored.

### Scoring a trace that has not arrived yet

Nothing checks that the target exists. Evals run asynchronously and may grade a
trace whose spans are still in flight; refusing the score would force you to
poll. A score whose trace never arrives stays visible in listings and is
removed by retention on its own `timestamp`.

## Score configs

Two harnesses that both post `accuracy` — one as a 0–1 float, one as a 0–100
integer — produce a trend line that means nothing. A config binds a name to
what it means, and from then on every score posted under that name must
comply:

```sh
curl -X PUT -H "Authorization: Bearer tp-sk-…" http://localhost:4318/api/v1/score-configs/accuracy -d '{
  "data_type": "numeric",
  "direction": "higher",
  "min": 0, "max": 1,
  "description": "LLM judge, 0–1"
}'
```

```json
{"name": "accuracy", "data_type": "numeric", "direction": "higher",
 "min": 0, "max": 1, "categories": null, "description": "LLM judge, 0–1",
 "created_at": "2026-09-03T10:00:00Z", "updated_at": "2026-09-03T10:00:00Z"}
```

| Field | Notes |
|---|---|
| `data_type` | Required: `numeric`, `boolean`, `categorical` or `text`. |
| `direction` | Which way is better: `higher`, `lower` or `none`. **Required** for `numeric` and `boolean`, **forbidden** for `categorical` and `text`. `none` is for the informational number — a token count, a retrieval depth — that you want typed and bounded but not judged. |
| `min`, `max` | `numeric` only, either or both, `min ≤ max`. |
| `categories` | `categorical` only, and required there: a non-empty list of distinct strings. |
| `description` | Free text. |

The binding is **by name**, and by name only. There is no config id to opt
into: the promise that names do not drift is only kept if the name is the
binding. What a config checks, inside the write transaction:

- the score's `data_type` equals the config's — `"accuracy" is numeric in its
  config, got categorical`;
- a numeric `value` lies inside `min`/`max` when set — `value 1.5 is above the
  config's max 1`;
- a categorical `string_value` is one of the categories — `"maybe" is not
  among the config's categories (pass, fail)`.

One violating item fails its whole batch with a `400` naming it, and nothing
is written. The check runs inside the write rather than in the handler
because a config can be replaced between a read and the commit.

Configs are **declarative**: `PUT` creates or replaces the whole config, a
re-`PUT` of the same body is a no-op, `DELETE` removes it. There are no
versions and no partial updates. A harness declares its configs at the top of
every run the way it declares its dataset — idempotent, in a file under
version control.

A config is a rule for what comes next. **Scores already stored are never
re-validated**: replacing a config changes what is accepted from now on and
nothing else, a config for a name that scores already used with another type
is accepted, and deleting a config touches no score.

`GET /api/v1/score-configs` returns them **all**, in name order, with no
`limit` and no cursor — a project has as many configs as it has score names,
and a loop over a list that only a person can grow would be ceremony.

`direction` is also what a run comparison will read to say *improved* or
*regressed* rather than merely *changed* — see [datasets.md](datasets.md).

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
fields. So is a known one sent without a value (`?name=`): that is a template
with an unset variable, and reading it as "no filter" would quietly answer a
different question than the one asked.

## Responses

| Status | Meaning |
|---|---|
| `201` | Written, committed and fsynced to disk. |
| `200` | The read succeeded. |
| `400` | Validation: a bad field, an unknown field, an unknown parameter, an invalid cursor, a score its name's config refuses. The message says which. |
| `401` | Unknown credentials. |
| `404` | No score — or no config — with that id or name in this project. |
| `413` | The body is over `TRACEPAD_MAX_BODY_BYTES`. |
| `429` | The write queue is saturated; retry after the `Retry-After` delay. |

A `201` means the score is on disk, not merely queued — the same guarantee a
`200` gives on the ingest endpoint.
