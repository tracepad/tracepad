# Administration

Projects, API keys, retention windows and user-data erasure, over the same
[JSON API](api.md) as everything else and through the same binary's CLI. No
admin console, no SQL.

Two rules run through all of it: **who may ask**, and **nothing destructive
happens until it is confirmed by name**.

## Who may ask

| | Project key (`tp-sk-…`) | `TRACEPAD_ADMIN_TOKEN` |
|---|---|---|
| Read and change its own retention | ✅ | ✅ |
| Create, list and revoke its own keys | ✅ | ✅ |
| Erase a user's data in its own project | ✅ | ✅ |
| Restore its own deleted project | ✅ | ✅ |
| Anything in another project | ❌ | ✅ |
| List every project | ❌ | ✅ |
| Create a project | ❌ | ✅ |
| Rename a project | ❌ | ✅ |
| **Delete a project** | ❌ | ✅ |

A project's secret key is the administrator of its own project. That is what
keeps "retention changes without a restart" true in the default install, which
has no admin token at all.

Deleting a project is the exception, and needs the token even for one's own
project. An `sk` lives in application config and in CI; a leaked application
credential must not be able to destroy the data it was writing. In a token-less
deployment, deleting a project means setting the token first — a deliberate
speed bump on the one act with a blast radius.

A server with no `TRACEPAD_ADMIN_TOKEN` answers those rows `403` with a message
that names the variable, rather than `401`-ing a perfectly good key.

The token is sent in the same header as a key, so the CLI takes it as `--key`
or `TRACEPAD_API_KEY`:

```sh
TRACEPAD_API_KEY=$TRACEPAD_ADMIN_TOKEN tracepad projects ls
```

## Dry run by default

Every destructive endpoint changes nothing until `?confirm=` echoes the
identity of what it destroys — the project's **name**, or the **user id**.
Without it, the endpoint answers `200` with a preview:

```json
{
  "dry_run": true,
  "would_delete": {"traces": 41203, "observations": 180114, "scores": 96,
                   "prompts": 4, "raw_batches": 812, "api_keys": 2},
  "oldest": "2026-03-14T08:21:00Z",
  "confirm": "checkout-service",
  "note": "the keys stop working immediately; the data is restorable for seven days"
}
```

Sending it back is what makes it happen:

```sh
curl -X DELETE -H "Authorization: Bearer $TRACEPAD_ADMIN_TOKEN" \
  "localhost:4318/api/v1/projects/$ID?confirm=checkout-service"
```

A wrong `confirm` is a `400` that does nothing. An id is a string you paste; a
name is a thing you mean, so a typoed or hallucinated target cannot match — and
the echo is checked against the stored row inside the write transaction, so a
rename between the preview and the commit cannot turn a confirmed request into
a request for something else.

The CLI does both steps for you: it shows the preview and asks you to type the
name back. `--yes` answers that for a script — it still makes both requests and
still sends the server's own confirm value, so what it replaces is the typing,
not the check. A non-interactive run **without** `--yes` stops with the preview
on stderr and exit code 1.

## Endpoints

| Method | Path | |
|---|---|---|
| `GET` | `/api/v1/projects` | All with the token, its own with a key. `?include=deleted` (token only). |
| `POST` | `/api/v1/projects` | Create; the secret is in the response and nowhere else. |
| `GET` | `/api/v1/projects/{id}` | One project with its windows. |
| `PATCH` | `/api/v1/projects/{id}` | `name` (token only), `retention_days`, `raw_retention_days`. |
| `DELETE` | `/api/v1/projects/{id}` | Soft delete; `202` with the purge date. |
| `POST` | `/api/v1/projects/{id}/restore` | Undo it inside the grace window. |
| `GET` | `/api/v1/projects/{id}/keys` | Public keys and their creation dates. |
| `POST` | `/api/v1/projects/{id}/keys` | Mint a pair. |
| `DELETE` | `/api/v1/projects/{id}/keys/{public_key}` | Revoke one. |
| `DELETE` | `/api/v1/projects/{id}/users/{user_id}/data` | Erase one user's parsed data. |

## Projects

```sh
tracepad projects ls
tracepad projects ls --deleted           # with purge dates; admin token
tracepad projects show
tracepad projects create checkout-service
tracepad projects rename $ID orders-service
tracepad projects rm $ID
tracepad projects restore $ID
```

`projects create` answers with the project and a fresh key pair. The secret is
printed once, because only its hash is ever stored:

```
project checkout-service created (9f2c…)

  TRACEPAD_API_KEY=tp-sk-…
  LANGFUSE_PUBLIC_KEY=tp-pk-…
  LANGFUSE_SECRET_KEY=tp-sk-…
```

Deleting and restoring are described in [retention.md](retention.md#deleting-a-project):
keys die at once, data survives seven days, the name stays reserved.

Commands that take a project accept `--project <id>`. Without it, a credential
that reaches exactly one project answers the question by itself — which is the
whole of it for the single-project install most deployments are.

## Keys

Several key pairs can be active at once, which is what makes rotation
zero-downtime:

```sh
tracepad keys create              # mint the new pair
# move your SDKs onto it
tracepad keys ls
tracepad keys rm tp-pk-old…       # revoke the old one
```

There is never a window where ingest `401`s. The secret half of a pair is
returned exactly once, at creation; the store keeps only its SHA-256 hash, so
there is nothing to show later even to an administrator.

Revoking a project's **last** key leaves a project that cannot ingest. That is
allowed — a declaratively provisioned deployment re-adds its keys from
`TRACEPAD_PROJECTS` on restart — but it asks for the confirmation first.

## Retention

See [retention.md](retention.md). The short version:

```sh
tracepad retention show
tracepad retention set --days 90
tracepad retention set --raw-days 14
tracepad retention set --forever --raw-follow
```

`null` — `--forever` for traces, `--raw-follow` for raw — is a value, not an
absence: it means "keep forever" and "follow the trace window". A window that
**grows** applies immediately. A window that **shrinks** destroys data on the
next sweep, so it is previewed and confirmed like any other destruction, even
when nothing is old enough to be affected yet: what is being changed is the
policy.

## Erasing a user's data

```sh
tracepad users rm-data user-4711
```

The echo is the user id. The response reports what went, per store. Raw OTLP
bodies are deliberately not touched — see
[retention.md](retention.md#what-this-means-for-a-data-subject-request) for
what that means for a data-subject request and how to deploy if it is not
acceptable.

## Not on the MCP surface

None of this is reachable as an MCP tool, and that is structural rather than an
oversight. An agent should not hold destructive capability at all, so that a
hallucinated tool call has nothing to destroy. See [mcp.md](mcp.md).
