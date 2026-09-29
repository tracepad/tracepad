# Scores

A score is a quality judgement attached to a trace, an observation or a
session: a number from an eval run, a pass/fail from a guardrail, a verdict
from an LLM judge, a rating from a human reviewer.

Scores are a plain JSON API — no SDK, no protobuf, and everything below works
with `curl`.

## Endpoints

| Method | Path | Purpose | A key needs |
|---|---|---|---|
| `POST` | `/api/v1/scores` | Write one score or an array of them | `ingest` |
| `GET` | `/api/v1/scores` | List scores, filtered and paginated | `read` |
| `GET` | `/api/v1/scores/{id}` | Fetch one score | `read` |
| `DELETE` | `/api/v1/scores/{id}` | Retract one score — see [Deleting a score](#deleting-a-score) | `write` |
| `PUT` | `/api/v1/score-configs/{name}` | Pin what a score name means — see [Score configs](#score-configs) | `write` |
| `GET` | `/api/v1/score-configs` | List the configs | `read` |
| `GET` | `/api/v1/score-configs/{name}` | One config | `read` |
| `DELETE` | `/api/v1/score-configs/{name}` | Remove a config | `write` |

Authentication is the same as for ingest — `Authorization: Bearer <secret
key>` or `Basic base64(<public key>:<secret key>)`. See
[ingest.md](ingest.md#authentication).

The last column is the [scope](api.md#scopes) a key must hold. Writing a score
is `ingest`, not `write`: an end user's thumbs-up is written by the
application, with the key it sends spans with. An online judge that reads
traces and scores them holds `ingest` and `read`; retracting a score and
changing a config take `write`.

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
comment. `author` is one of them: who wrote a score is not yours to say, it is
the credential you wrote with ([below](#who-wrote-a-score)).

A score whose `name` has a [config](#score-configs) is checked against it —
type, range, categories — and a violation is a `400` naming the item and the
rule. A name without a config is as free as the table above.

### Idempotency and corrections

A score id is the idempotency key. Re-posting the same id replaces the row
whole, so retrying a failed batch is safe and a correction is just another
POST — there is nothing to delete first. The row's author goes with it: whoever
wrote the correction is now the score's author.

If your eval loop already has a natural key, hash it into the id:

```python
score_id = hashlib.sha256(f"{run_id}:{trace_id}:helpfulness".encode()).hexdigest()[:32]
```

### Deleting a score

A correction is a re-POST, but a retraction — *this verdict should not be
here at all* — is a delete:

```sh
curl -X DELETE -H "Authorization: Bearer tp-sk-…" \
  http://localhost:4318/api/v1/scores/8673743e8ca15b9213d541a703786e86
```

```json
{"id": "8673743e8ca15b9213d541a703786e86"}
```

`404` when this project has no score with that id, which is also what an id
belonging to another project answers. There is no dry run and no `?confirm=`:
a score is a single row, and posting the same id again puts it back. The row
goes through the same write queue as every other write, so a `200` means it is
off the disk.

`tracepad scores rm <id>` is the same call — see [cli.md](cli.md#scores-add-scores-rm).

Deleting a score also **corrects the quality rollup**, in the same transaction
that removes the row. A score's arrival is noticed by its `created_at`, and a
deleted row has none to notice — so the one hour it was counted in is
recomputed with the deletion, the way erasing a user's data recomputes the
hours it emptied. Only that table: the traffic statistics of the hour are not
its business. An hour past the project's retention window is frozen and stays
as it is ([quality.md](quality.md#the-lag)).

### Batches

An array is all-or-nothing: one transaction, and one `400` naming the first
item it refused. It holds at most **10,000** scores; a longer one is a `413`
(`this request carries N scores; the server takes at most 10000 per request —
send them in batches`), refused before any item is looked at, with nothing
written. The SDKs send scores in batches of 100.

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
      "created_at": "2026-08-27T10:00:02.114Z",
      "author": {"kind": "key", "id": "tp-pk-…", "name": "nightly-judge", "standing": "active"}
    }
  ],
  "next_cursor": null
}
```

Scores come back newest first. Fields that were never set are omitted, except
`author`, which is always there ([below](#who-wrote-a-score)).

| Parameter | Notes |
|---|---|
| `trace_id`, `observation_id`, `session_id`, `name`, `data_type` | Exact-match filters |
| `author` | The scores one author wrote: an account id or a public key, as a score's `author.id` gives it, or `me` for the account or key asking |
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

## Who wrote a score

Every score records the credential it was written with: the account signed
in to the web interface, or the project key a program sent. The server stamps
it; the body has no say, so a key cannot write a score in somebody else's
name. What a program is — the judge's model, the eval run — belongs in
`metadata`, as before; the key is who stands behind it.

```json
"author": {"kind": "account", "id": "3f0c…", "name": "Ada", "email": "ada@example.com", "standing": "editor"}
"author": {"kind": "key", "id": "tp-pk-…", "name": "nightly-judge", "standing": "revoked"}
"author": null
```

- **`kind`** is `account` or `key`, and **`id`** the account's id or the
  key's public key.
- **`name`** is the account's display name, or the key's name when it wrote.
  An account that is gone, or has no display name now, shows the name it had
  when it wrote. It can be empty: an account never has to set one.
- **`email`** is the account's email when it wrote, and only a signed-in
  **editor or owner** of the project sees it — the people who already see who
  minted each key. A viewer and every key get the name alone; a viewer cannot
  list the team anywhere else, and a program has no use for a person's
  address.
- **`standing`** is the author's relation to the project **now**: an account's
  `owner`, `editor` or `viewer`, or `removed` (no longer a member),
  `disabled` or `deleted`; a key's `active` or `revoked`. The name and the
  email are copies, so they survive the account's deletion and the key's
  revocation, and `standing` says that the author is gone.
- **The last writer is the author.** A correction re-posts the score whole
  (above), and its author with it: when a person corrects a judge, the value
  and the author are the person's, and `metadata.source` still says `judge`.
- **`null`** is a score written before the server recorded authors. Nothing
  on disk says who wrote those, so nothing is guessed.

Deleting an account leaves its scores where they are, with its name and email
on them ([accounts.md](accounts.md)). Erasing an end user's data takes the
scores on their traces whoever wrote them, and says nothing about the authors
([retention.md](retention.md#deleting-a-users-data)).

## From the web interface

The three endpoints above are also three buttons. A trace, an observation and
a session each carry a **Scores** block: one chip per score — the name, the
value rendered by its type, where it came from — with the comment on expand.
*Score* opens a dialog that offers the project's [configured](#score-configs)
names and builds the control the config dictates, and every chip carries
*Edit* — the same dialog, re-posting the score's own id — and *Delete*.

A score written there carries `metadata: {"source": "web"}` and no
`timestamp`, so it is stamped at receive time: a judgement made now happened
now. Nothing else is different — it is this API, called from a browser. See
[ui.md](ui.md#scores).

## Trends over time

"Did hallucination drop after 2.5.0" is not a listing question, and the listing
does not answer it: `GET /api/v1/stats/scores` does, from an hourly roll-up
beside the statistics'. One series per score name, a mean for a numeric name, a
rate for a boolean one, the distribution for a categorical one, grouped by day,
hour, environment, release or model:

```sh
curl … "http://localhost:4318/api/v1/stats/scores?name=hallucination&group_by=release"
tracepad scores trend --name hallucination --group-by release
```

A score is counted in the hour of the **trace** it names, so the curve lines up
with the traffic. Scores that name only a session, and `text` scores, are not on
a timeline at all. See [quality.md](quality.md).

A score that names only a session goes **with its session**: the retention
sweep deletes it once it is older than the project's trace window and no trace
carries its session any more, and erasing a user's data deletes the
session-only scores of every session the user's traces belonged to — a session
another user's traces share included ([retention.md](retention.md#deleting-a-users-data)).
A score that names a trace follows the trace, whatever session it also names.

## From an annotation queue

A review programme — a named list of traces and the score names a reviewer
must set on each of them — is [annotation.md](annotation.md). It stores no
verdicts of its own: a queue item is *completed* exactly when the scores it
asked for are on its target, checked here, whoever wrote them. A judge's
verdict already on the trace therefore counts, and the reviewer confirms it
rather than repeating it.

Scores written from the annotation desk carry
`metadata: {"source": "annotation", "queue": …, "annotator": …}`, so the chip
on the trace says where the verdict came from and "everything ada decided in
the weekly review" is a filter over the scores you already have.

## Responses

| Status | Meaning |
|---|---|
| `201` | Written, committed and fsynced to disk. |
| `200` | The read succeeded. |
| `400` | Validation: a bad field, an unknown field, an unknown parameter, an invalid cursor, a score its name's config refuses. The message says which. |
| `401` | Unknown credentials. |
| `403` | A key without the scope the route needs; the body and `WWW-Authenticate` name it ([api.md](api.md#scopes)). |
| `404` | No score — or no config — with that id or name in this project. |
| `413` | The body is over `TRACEPAD_MAX_BODY_BYTES`, or an array holds more than 10,000 scores. |
| `429` | The write queue is saturated; retry after the `Retry-After` delay. |

A `201` means the score is on disk, not merely queued — the same guarantee a
`200` gives on the ingest endpoint.
