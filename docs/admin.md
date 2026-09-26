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
| Create, list and revoke its keys | ❌ | ❌ | ✅ | ✅ | ✅ |
| Erase a user's data in it | ✅ | ❌ | ✅ | ✅ | ✅ |
| Anything in another project | ❌ | ❌ | ❌ | ✅ | ✅ |
| List every project | ❌ | ❌ | ❌ | ✅ | ✅ |
| Create a project | ❌ | ❌ | ❌ | ✅ | ✅ |
| Rename a project | ❌ | ❌ | ❌ | ✅ | ✅ |
| **Delete or restore a project** | ❌ | ❌ | ❌ | ✅ | ✅ |

A project's secret key is still the administrator of its own project. That is
what keeps "retention changes without a restart" true in the default install,
which has no admin token at all.

Its keys are the exception: **no project key lists, mints or revokes keys**,
and it is answered `403` on those three routes. A key that could mint keys
would turn one lost key into as many credentials as its finder wanted, each of
them outliving the revocation of the first. Issuing a credential is a person's
act — an owner's or an editor's, signed in — or the operator's, with the
admin token.

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
identity of what it destroys — the project's **name**, the **user id**, or the
**trace id** when one trace is deleted. Without it, the endpoint answers `200`
with a preview:

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
| `GET` | `/api/v1/projects/{id}/keys` | Public keys, who minted each and when it was last used. Not with a project key. |
| `POST` | `/api/v1/projects/{id}/keys` | Mint a pair, optionally `{"name": …}`. Not with a project key. |
| `DELETE` | `/api/v1/projects/{id}/keys/{public_key}` | Revoke one. Not with a project key. |
| `DELETE` | `/api/v1/projects/{id}/users/{user_id}/data` | Erase one user's parsed data. |
| `DELETE` | `/api/v1/traces/{id}` | Delete one trace — an editor's route, on the data plane. See [below](#deleting-traces). |
| `DELETE` | `/api/v1/traces?…&to=` | Delete every trace a listing filter matches before `to`, in rounds. |
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
and `restore` — take the admin token; the rest take a project key. The `keys`
commands take the admin token too.

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
zero-downtime. An owner or editor does it in the interface, under **Settings →
Project → API keys**; from a terminal it takes the admin token, because no
project key manages keys:

```sh
export TRACEPAD_API_KEY=$TRACEPAD_ADMIN_TOKEN
tracepad projects ls                                      # the project's id
tracepad keys create --project $ID --name "checkout api"  # mint the new pair
# move your SDKs onto it
tracepad keys ls --project $ID                            # wait until the old one goes quiet
tracepad keys rm tp-pk-old… --project $ID                 # revoke it
```

The token reaches every project, so it needs `--project` as soon as there is
more than one; with a single project the command finds it by itself.

There is never a window where ingest `401`s. The secret half of a pair is
returned exactly once, at creation; the store keeps only its SHA-256 hash, so
there is nothing to show later even to an administrator. The name is optional,
at most 64 characters, free of control characters and not unique: it is there
to say which program holds
the key.

The listing says, for each key, who minted it and when it was last used:

```
PUBLIC KEY           NAME          SCOPES             CREATED              CREATED BY                   LAST USED
tp-pk-3f9a…          -             ingest,read,write  2026-09-01 08:00:00  server                       2026-09-26 17:41:12
tp-pk-81c0…          checkout api  ingest,read,write  2026-09-26 17:30:05  ed@example.com (editor)      never
```

- **Created by** is the account that minted it, with its email as it was then
  and its standing in the project now — `owner`, `editor`, `viewer`, `removed`
  (no role here any more), `disabled` or `deleted`; or `admin token`; or
  `server`, for the key of the first-start project and those
  `TRACEPAD_PROJECTS` declares; or `unknown`, for a key older than this
  record. If a key was ever lost, the `unknown` keys created after it are the
  ones to rotate first.
- **Last used** is the last request the key authenticated, admitted or
  refused. It is written to the database once a minute; the listing includes
  uses not yet written, and a crash loses at most that minute. `never` means not since the server started
  recording it.

Taking somebody's access away — removing a membership, demoting an editor,
disabling or deleting the account — revokes no key: the person who minted a
key is often the one who wired production with it, and ingest stopping by
surprise is the worse failure. The Keys card marks a key whose minter can no
longer manage keys in the project, and deleting an account shows the keys it
minted before it asks for the email. Rotate those deliberately: mint, move,
revoke.

Revoking a project's **last** key leaves a project that cannot ingest. That is
allowed — an owner or editor can mint a new pair in the interface, and so can
the admin token — but it asks for the confirmation first. `TRACEPAD_PROJECTS`
does not re-add it: on restart it creates the projects it names that do not
exist and leaves an existing project's keys as they are.

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

The same `PATCH /api/v1/projects/{id}` carries the project's media setting,
`"media": "store"` (the default) or `"placeholder"` — whether ingest keeps the
images and files it takes out of payloads, or only a reference saying it did
not ([media.md](media.md#not-keeping-them-the-placeholder-setting)). It is not
destructive either way and needs no confirmation: `tracepad retention set
--media placeholder`.

Every dry run of a deletion — a window that shrinks, an erasure, a trace
deletion, a project's — and every confirmed answer names the media bodies the
project would stop holding as `media` and their decoded bytes as
`media_bytes`: the bodies none of the project's traces or raw batches staying
behind points at. The figure is the project's own, like the one
`GET /api/v1/system` reports: a body another project also holds is counted,
although its bytes stay on disk for that project, and a project's deletion no
longer says how much of what it counts leaves the disk.

## Erasing a user's data

```sh
tracepad users rm-data user-4711
```

The echo is the user id. The response reports what went, per store. When an
eval run holds any of the user's traces, the preview names it under
`affected_runs` — erasure outranks the pin, and the run shows those items as
missing afterwards ([datasets.md](datasets.md#what-a-run-keeps)).

The confirmed answer also says when the rest of the job is done, and what it
will not reach:

```json
{
  "dry_run": false,
  "deleted": {"traces": 12, "observations": 240, "scores": 30, "payloads": 480, …},
  "user_id": "user-4711",
  "compaction": {"requested_at": "2026-09-26T10:02:11Z", "expected_by": "2026-09-26T11:00:00Z"},
  "pre_migration_backup": {"created_at": "2026-09-24T08:00:00Z", "removed_at": "2026-10-01T08:00:00Z"}
}
```

- `compaction` — the freed space is zeroed as the rows go; the search index
  and the write-ahead log are rewritten by the next sweeper pass, which
  `expected_by` names. `GET /api/v1/system` says when it finished.
- `pre_migration_backup` — present while the server keeps a copy of the
  database from before its last upgrade: the one copy an erasure does not
  rewrite, and the day the sweeper removes it. The dry run names it too.

**What it does not take.** The raw OTLP bodies are not touched — a batch holds
many traces — so the user's spans stay readable through `GET /api/v1/raw/{id}`
and `tracepad export --otlp` until the raw window takes their batches, which by
default is never; the preview says so in its note. Scores given to one of the
user's sessions rather than a trace, and dataset items cut from their traces,
stay as well, and so does the pre-migration backup until its date.
[retention.md](retention.md#what-this-means-for-a-data-subject-request) lists
each and what to do about it today.

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

The erasure is synchronous and runs in **chunks** — up to five hundred traces
of one hour — each one a transaction that leaves the store consistent on its
own: the chunk's traces go and the hour they occupied is recomputed in the
same commit, and the writer is held for one chunk at a time so ingest keeps
flowing between them. A user active in many hours takes many chunks, and a
long history can take longer than the interface waits; that is safe, because
a request cut off between chunks — a closed tab, the interface's
thirty-second clock — destroys nothing half-way: what the committed chunks
erased is erased and counted as erased, and repeating the call finishes the
rest. The counts in the answer are the request's own; a repeat reports what
it erased, not what the interrupted one did.

## Deleting traces

```sh
tracepad traces rm 4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f
tracepad traces rm --to 2026-09-17T14:02:17Z --env loadtest --since 24h
```

A trace leaves the store in exactly two ways otherwise: the retention sweep
takes it when its window runs out, or the project goes whole. In between is
where the mistakes live — an eval harness that exported under the
application's key, a load test run against production, one trace holding
something a person should not have typed — and this is the door for those.
One trace by id, or every trace a [listing filter](api.md#filters) matches:
`DELETE /api/v1/traces/{id}`, and `DELETE /api/v1/traces?<filters>&to=`. An
editor's route rather than an administrator's — it is the reader's and the
manager's gesture at the surfaces where they read and choose — and it wears
the same ceremony as everything above: a dry run until `confirm` echoes the
identity of what goes. The SDKs call both from a script, the rounds walked for
you — [Python](sdk-python.md#deleting-traces), [Node](sdk-js.md#deleting-traces),
[Go](sdk-go.md#deleting-traces).

**What goes** is what erasure takes, by the same path: the trace, its
observations, scores, payloads and search entries, the
[annotation-queue items](annotation.md) pointing at it, and the hour it
started in re-rolled in the same transaction, so the
[statistics](retention.md#what-outlives-what) stop counting it as the `200`
arrives. Raw OTLP bodies are **not** touched — a raw batch holds many traces
of many kinds, and a trace cannot be cut out of one — and the preview says so
in its `note`. A trace an eval run holds is deleted like any other; the run
then shows the item as missing, and the preview names the run under
`affected_runs` so the hole is seen before it opens
([datasets.md](datasets.md#what-a-run-keeps)).

**One trace.** The echo is the trace id, because the id is the trace's only
identity. Without `confirm` the endpoint answers the preview — `would_delete`
with `traces`, `observations`, `scores` and `annotation_items`, `oldest`,
`affected_runs`, `confirm`, `note` — and an unknown id is `404`, dry run and
confirmed alike. With `?confirm=<id>` it deletes and answers
`{"dry_run": false, "deleted": {…}, "id", "compaction"}`; `deleted` counts the
payloads too, and `compaction` says when the sweeper pass that overwrites what
the deletion unlinked is due.

**By filter.** The filters are the trace listing's own — the same names, the
same validation, `400` on an unknown one — so "delete what I am looking at"
is one call with no second grammar; an empty filter is *every trace before
`to`*. `to` is **required**, dry run and confirmed alike: the listing is
half-open on it, so a trace that starts after it can never qualify, and the
operator who previewed a thousand does not delete a thousand and forty
because ingest kept flowing in between. The CLI does not fill it in — a
script that deletes by filter should say what it means — and the interface
pins it to the moment its dialog opened, and says so. The preview counts the
match **exactly** under `matched` (the listing's own count stops at a
thousand), and the echo is the **project name**, the one retention shrinking
already uses for the same act by another door.

A confirmed request deletes **one round**: the newest `limit` matches
(1–1000, default 1000), in chunks of at most five hundred traces of one hour,
each chunk its own transaction — and at most fifty such chunks, whichever
bound comes first, so a set spread thinly over many hours does not run past
the interface's clock — and answers
`{"dry_run": false, "deleted": {…}, "more": true}` — repeat the same call
while `more` is true. A request that ran for minutes is one the interface's
thirty-second clock would cut off every time, leaving the operator with an
error over a store that is in fact fine; a round is a complete, consistent
act, and the client — the CLI, the dialog — is the one that loops and can say
*2,000 of 12,000*. A round cut off between chunks destroys nothing half-way:
what committed is gone and counted, and the next request continues. Nothing
is recounted on the way: a confirmed round takes what the filter matches at
that moment, so a set that shrank since the preview — retention, another
operator, an earlier round — is not a reason to refuse, and a repeat after
`more: false` deletes nothing and is not an error.

**The ingest race is documented, not fought.** A deletion removes what the
store holds at that moment. A span that arrives afterwards for a deleted
trace creates the trace again from what arrived, exactly as it would for a
trace never seen; nothing is remembered about deleted ids. `to=` makes this
rare for the bulk form, and an operator deleting a trace whose spans are
still arriving has a stopwatch problem, not a store problem: wait for the
export to finish, then delete.

Not here: moving traces between projects (replay the
[raw bodies](export.md) into the other project with its key), a trash or an
undo (a project has a grace window because a project is a thing with a name
and a history; a trace is a row, and a re-export recreates it), deleting one
observation. A session's traces are `?session_id=…&to=`.

## In the web interface

*Delete…* sits on the trace header beside *Add to queue*, and on the traces
listing beside *Add to queue…* — editors only, like both — each a dialog
around the same card with the server's dry run on screen
([ui.md](ui.md#deleting-traces)).

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
