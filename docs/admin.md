# Administration

Projects, API keys, retention windows and user-data erasure, over the same
[JSON API](api.md) as everything else and through the same binary's CLI. No
admin console, no SQL.

Two rules run through all of it: **who may ask**, and **nothing destructive
happens until it is confirmed by name**.

## Who may ask

Three kinds of caller reach this surface: a project key, a person signed in
with an [account](accounts.md), and `TRACEPAD_ADMIN_TOKEN`.

| | Project key (`tp-sk-…`) | `viewer` | `editor` | Owner | `TRACEPAD_ADMIN_TOKEN` |
|---|---|---|---|---|---|
| Read a project and its windows | ✅ | ✅ | ✅ | ✅ | ✅ |
| Change its retention | ✅ | ❌ | ✅ | ✅ | ✅ |
| Create, list and revoke its keys | ✅ | ❌ | ✅ | ✅ | ✅ |
| Erase a user's data in it | ✅ | ❌ | ✅ | ✅ | ✅ |
| Anything in another project | ❌ | ❌ | ❌ | ✅ | ✅ |
| List every project | ❌ | ❌ | ❌ | ✅ | ✅ |
| Create a project | ❌ | ❌ | ❌ | ✅ | ✅ |
| Rename a project | ❌ | ❌ | ❌ | ✅ | ✅ |
| **Delete or restore a project** | ❌ | ❌ | ❌ | ✅ | ✅ |

A project's secret key is still the administrator of its own project. That is
what keeps "retention changes without a restart" true in the default install,
which has no admin token at all.

Creating, deleting, restoring and renaming a project need an owner or the
token, for one reason: an `sk` lives in application config and in CI, and a
leaked application credential must not be able to move a project in or out of
existence. A project's name is also the echo every destructive confirmation is
typed against, which is why renaming sits with them.

A server with no owner and no `TRACEPAD_ADMIN_TOKEN` answers those rows `403`
with a message that names both, rather than `401`-ing a perfectly good key.

The token is sent in the same header as a key, so the CLI takes it as `--key`
or `TRACEPAD_API_KEY`:

```sh
TRACEPAD_API_KEY=$TRACEPAD_ADMIN_TOKEN tracepad projects ls
```

A person's session is a cookie rather than a header, and names the project it
is asking about with `X-Tracepad-Project` on the routes that do not carry one
in the path. See [accounts.md](accounts.md#what-a-session-may-ask).

## Dry run by default

Every destructive endpoint changes nothing until `?confirm=` echoes the
identity of what it destroys — the project's **name**, or the **user id**.
Without it, the endpoint answers `200` with a preview:

```json
{
  "dry_run": true,
  "would_delete": {"traces": 41203, "observations": 180114, "scores": 96,
                   "prompts": 4, "raw_batches": 812, "api_keys": 2,
                   "annotation_queues": 3, "annotation_items": 1408},
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
| `GET` | `/api/v1/projects` | What the caller can reach: all with the token or an owner, its own with a key, its memberships with a member's session. `?include=deleted` (owner or token). `?activity=24h` adds `traces_24h` to every row — the traces of the last 24 hours, counted the way `/api/v1/stats` counts them; a soft-deleted project carries `0`. Any other value is a `400`. |
| `POST` | `/api/v1/projects` | Create; the secret is in the response and nowhere else. |
| `GET` | `/api/v1/projects/{id}` | One project with its windows. |
| `PATCH` | `/api/v1/projects/{id}` | `name` (owner or token), `retention_days`, `raw_retention_days`, `stats_retention_days`. |
| `DELETE` | `/api/v1/projects/{id}` | Soft delete; `202` with the purge date. |
| `POST` | `/api/v1/projects/{id}/restore` | Undo it inside the grace window. |
| `GET` | `/api/v1/projects/{id}/keys` | Public keys and their creation dates. |
| `POST` | `/api/v1/projects/{id}/keys` | Mint a pair. |
| `DELETE` | `/api/v1/projects/{id}/keys/{public_key}` | Revoke one. |
| `DELETE` | `/api/v1/projects/{id}/users/{user_id}/data` | Erase one user's parsed data. |
| `GET` | `/api/v1/projects/{id}/members` | Who has a role in this project. See [accounts.md](accounts.md#managing-accounts). |

The people who sign in, their roles and their invitations are
[accounts.md](accounts.md), and `tracepad accounts …` manages them from a
terminal on this same token ([cli.md](cli.md#accounts)).

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

The four that move a project in or out of existence — `create`, `rename`, `rm`
and `restore` — take the admin token; the rest take a project key.

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
absence: it means "keep forever" and "follow the trace window", and it is the
only way to say it: a day count is bounded at 36500. A window that
**grows** applies immediately. A window that **shrinks** destroys data on the
next sweep, so it is previewed and confirmed like any other destruction, even
when nothing is old enough to be affected yet: what is being changed is the
policy.

## Erasing a user's data

```sh
tracepad users rm-data user-4711
```

The echo is the user id. The response reports what went, per store. When an
eval run holds any of the user's traces, the preview names it under
`affected_runs` — erasure outranks the pin, and the run shows those items as
missing afterwards ([datasets.md](datasets.md#what-a-run-keeps)). Raw OTLP
bodies are deliberately not touched — see
[retention.md](retention.md#what-this-means-for-a-data-subject-request) for
what that means for a data-subject request and how to deploy if it is not
acceptable.

The **annotation-queue items** pointing at the erased traces go with them
([annotation.md](annotation.md)) — an item is a pointer, and the queues keep
their shape with shorter lists. Both the dry run and the answer count them
under `annotation_items`, beside the traces and the scores. The verdicts
already given are scores on those traces and go with the traces.

The user's rows in the **per-user rollup** ([users.md](users.md)) go in the
same request, outright rather than by recomputation: they are about the user,
and for an hour past the trace-retention window there would be nothing left to
recompute them from. So the account leaves `/api/v1/users` immediately, and
`GET /api/v1/users/{id}` answers `404`. The project-wide statistics are
corrected where they can be, which is the rule
[retention.md](retention.md#what-outlives-what) states.

The erasure is synchronous and runs in **chunks of five hundred traces**, each
one a transaction that leaves the store consistent on its own: the chunk's
traces go and the hours they occupied are recomputed in the same commit. A
request that is cut off between chunks — a closed tab, the interface's
thirty-second clock — therefore destroys nothing half-way: what the committed
chunks erased is erased and counted as erased, and repeating the call finishes
the rest. The counts in the answer are the request's own; a repeat reports
what it erased, not what the interrupted one did.

## In the web interface

The same card sits on the user's own page (`/users/{id}` → *Erase data*), with
the id already filled in; on success the page leaves for the listing. It is
the Settings card with one field fewer — one contract, rendered twice.

The Settings screen renders this whole contract (see [ui.md](ui.md#settings-and-administration)):
a **Project** tab with the project's own management, and a **Server** tab —
owners only — with the project lifecycle and the accounts. Every destructive
card is the dry run above, shown, with the same echo typed into a field: the
browser is not a softer path to destruction than `curl`.

`TRACEPAD_ADMIN_TOKEN` is not entered in the interface at all. It was there
because one credential unlocked the management plane in a browser; an owner
[account](accounts.md) is that credential now, and the token stays what it is
for — the CLI, and the recovery when every owner's password is lost
([cli.md](cli.md#accounts)).

## Not on the MCP surface

None of this is reachable as an MCP tool, and that is structural rather than an
oversight. An agent should not hold destructive capability at all, so that a
hallucinated tool call has nothing to destroy. See [mcp.md](mcp.md).
