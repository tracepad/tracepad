# Prompts

Tracepad stores prompts as versioned, labelled artefacts. Versions are
append-only — an audit trail, never edited in place — and labels move between
them, so promoting a prompt to production and rolling it back are the same
one-line operation, with no redeploy and no new version.

Like scores, this is a plain JSON API that works with `curl`; authentication is
the [same as for ingest](ingest.md#authentication), and no `Content-Type` is
required.

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/prompts` | List names, with where their labels point |
| `GET` | `/api/v1/prompts/{name}` | Fetch one version, by label, by version, or the latest |
| `POST` | `/api/v1/prompts/{name}/versions` | Append a version |
| `GET` | `/api/v1/prompts/{name}/versions` | List a name's versions |
| `PUT` | `/api/v1/prompts/{name}/labels/{label}` | Create or move a label |
| `DELETE` | `/api/v1/prompts/{name}/labels/{label}` | Remove a label |
| `DELETE` | `/api/v1/prompts/{name}` | Delete a name with every version and label |

Prompt and label names match `^[A-Za-z0-9][A-Za-z0-9._-]*$` and are at most 200
characters — one URL path segment, no escaping. `latest` is reserved as a label
name (see [Labels](#labels)).

## Creating a version

```sh
curl -H "Authorization: Bearer tp-sk-…" \
  http://localhost:4318/api/v1/prompts/summarize/versions -d '{
  "type": "chat",
  "prompt": [{"role": "system", "content": "You are terse."}],
  "config": {"model": "claude", "temperature": 0.2},
  "commit_message": "first cut",
  "labels": ["production"]
}'
```

```json
{
  "name": "summarize",
  "version": 1,
  "type": "chat",
  "prompt": [{"role": "system", "content": "You are terse."}],
  "config": {"model": "claude", "temperature": 0.2},
  "commit_message": "first cut",
  "labels": ["production"],
  "created_at": "2026-08-27T09:58:11Z"
}
```

| Field | Required | Notes |
|---|---|---|
| `type` | on the first version | `text` or `chat`. Fixed for the name from then on. |
| `prompt` | yes | A string for `text`; an array of `{role, content}` messages for `chat`. Every message needs a non-empty `role` and `content` — a version cannot be edited afterwards, so an empty one would be permanent. |
| `config` | no | A JSON object — model, temperature, whatever your runtime reads. |
| `commit_message` | no | Why this version exists. |
| `labels` | no | Labels to point at this version as it is created. |

The version number is `current max + 1`, assigned inside the write transaction.
Concurrent creates for one name therefore produce `1..N` with no gaps and no
duplicates, however many writers race.

A name has one shape for its whole life: posting a text body to a chat name (or
declaring a `type` the name does not use) is a `400`. A name that changed shape
between versions would break every client that fetches it by label.

Nothing is interpreted inside `prompt` — `{{variables}}` and any other template
syntax are stored and returned verbatim. Interpolation belongs to your
framework, not to the store.

## Fetching a prompt

```sh
curl -H "Authorization: Bearer tp-sk-…" \
  "http://localhost:4318/api/v1/prompts/summarize?label=production"
```

| Query | Result |
|---|---|
| *(none)* | The highest version |
| `?version=3` | Exactly version 3 |
| `?label=production` | Whatever `production` currently points at |
| `?label=latest` | The highest version — `latest` is computed, never stored |
| `?version=…&label=…` | `400`: pick one |
| `?label=` (no value) | `400`, never "the latest": an unset shell variable must not silently ship an unreleased prompt |

There is no implicit `production` default: an unqualified fetch means "newest",
the way every other versioned-artefact tool works.

Responses carry `Cache-Control: max-age=60`, which bounds how long a label move
takes to reach a client that caches. If you pin a version, you may cache it for
as long as you like — a version never changes.

## Labels

A label is a single row per `(prompt, label)`, so "which version is production"
has exactly one answer. Promote by moving it forward, roll back by moving it
back:

```sh
# promote
curl -X PUT -H "Authorization: Bearer tp-sk-…" \
  http://localhost:4318/api/v1/prompts/summarize/labels/production -d '{"version": 4}'

# roll back
curl -X PUT -H "Authorization: Bearer tp-sk-…" \
  http://localhost:4318/api/v1/prompts/summarize/labels/production -d '{"version": 3}'

# retire the label
curl -X DELETE -H "Authorization: Bearer tp-sk-…" \
  http://localhost:4318/api/v1/prompts/summarize/labels/production
```

Both answer `200 {"label": "production", "version": 3}`; `DELETE` reports the
version the label pointed at. Pointing a label at a version that does not exist
is a `404`.

`latest` cannot be created, moved or deleted: it always names the highest
version, computed at read time, so it can never drift from the truth it
summarizes.

## Listing

```sh
curl -H "Authorization: Bearer tp-sk-…" http://localhost:4318/api/v1/prompts
```

```json
{
  "prompts": [
    {
      "name": "summarize",
      "type": "chat",
      "latest_version": 4,
      "labels": {"production": 3},
      "updated_at": "2026-08-27T09:58:11Z"
    }
  ],
  "next_cursor": null
}
```

Names come back alphabetically; `updated_at` is when the name's newest version
was created.

`GET /api/v1/prompts/{name}/versions` lists a name's versions newest first,
with their commit messages and labels but **without** the bodies — version
lists are for picking and diffing; bodies come from the single-prompt fetch.

Both listings take `limit` (1–500, default 50), a `cursor` and a `direction`
(`next` or `prev`), and answer with `next_cursor` and `prev_cursor`. Keep
passing `next_cursor` until it comes back `null`; with no cursor at all,
`direction=prev` is the far end — the last name alphabetically, the oldest
version of a name — which is what makes "jump to the end" cost one page.

## Deleting a prompt

A name that was a mistake, or a rename done by re-creating it under the right
name, is removed whole. There is no way to delete one version: versions are the
audit trail, and a hole in it would leave a label pointing at nothing.

Like every destructive endpoint here, it is a dry run until `?confirm=` echoes
the name (see [admin.md](admin.md#dry-run-by-default)):

```sh
# what would go
curl -X DELETE -H "Authorization: Bearer tp-sk-…" \
  http://localhost:4318/api/v1/prompts/summarize
```

```json
{
  "dry_run": true,
  "name": "summarize",
  "would_delete": {"versions": 4, "labels": 1},
  "confirm": "summarize",
  "note": "traces that ran this prompt keep the name and version they recorded; the trace filter goes on answering for it"
}
```

```sh
# and for real
curl -X DELETE -H "Authorization: Bearer tp-sk-…" \
  "http://localhost:4318/api/v1/prompts/summarize?confirm=summarize"
```

The confirmed call answers with the same shape plus `"deleted": true`, and the
versions and the labels go in one transaction. A `confirm` that does not match
is a `400` that changes nothing; an unknown name is a `404` either way.

**Traces are untouched.** An observation's `prompt` is a name and a version
the client said it ran, recorded as sent and resolved against no registry
(see [api.md](api.md#one-trace)) — not a reference into this table. So
`?prompt=summarize` on the trace listing goes on finding the runs of a prompt
whose definition is gone, which is the honest answer: they did run it.

## Using a prompt from an application

Fetch by label at start-up (or per request, behind the 60-second cache), read
`config` for your model parameters, and interpolate the body yourself:

```python
prompt = httpx.get(
    "http://localhost:4318/api/v1/prompts/summarize",
    params={"label": "production"},
    headers={"Authorization": f"Bearer {secret_key}"},
).json()

messages = [
    {"role": m["role"], "content": m["content"].format(**variables)}
    for m in prompt["prompt"]
]
response = client.messages.create(model=prompt["config"]["model"], messages=messages)
```

Deploying a new prompt is then a label move, not a release.

## Responses

| Status | Meaning |
|---|---|
| `201` | The version is committed and fsynced to disk. |
| `200` | The read, the label move, the label removal, or the deletion — or its dry run — succeeded. |
| `400` | Validation: a type mismatch, a malformed name, an unknown field or parameter, `latest` used as a label, a `confirm` that does not echo the name. |
| `401` | Unknown credentials. |
| `404` | No such prompt, version or label in this project. |
| `413` | The body is over `TRACEPAD_MAX_BODY_BYTES`. |
| `429` | The write queue is saturated; retry after the `Retry-After` delay. |
