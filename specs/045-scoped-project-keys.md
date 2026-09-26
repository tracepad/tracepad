# Spec 045 — Scoped project keys

**Status:** 🚧 IN PROGRESS
**Sprint:** September 2026

> A project key is its project's administrator (spec 005 #11). The same
> `tp-sk-…` string a production service exports spans with can read every
> prompt and output the project has stored, erase a user, shorten
> retention, bulk-delete traces and mint more keys — and a key minted with
> it keeps working after it is revoked. One credential is doing four jobs:
> the application's, the agent's, the eval harness's and the operator's,
> and whichever of the four is the least careful with it decides what the
> other three are exposed to. Spec 028 moved people off keys and left
> "per-key scopes, read-only keys" out of scope; this spec is that
> follow-up. A key carries a set of scopes — `ingest`, `read`, `write` —
> every route declares the one a key needs, managing keys stops being
> something a key can do, and the key listing says who minted each key and
> when it was last used. Every existing key keeps all three scopes, so
> nothing that works today stops working except minting, listing and
> revoking keys *with a key*.

---

## Overview

Deliverables, two PRs in the order below (the last commit of the second
flips the status):

**PR 1 — Provenance, and keys stop managing keys.**

- Schema 0023: `api_keys`
  rebuilt with `name`, `scopes`, `created_via`, `created_by`,
  `created_by_email` and `last_used_at` (Data contract). Every key minted
  in this PR is stored with all three scopes — which is what every key is
  today — so the column exists before anything reads it.
- The three routes under `/api/v1/projects/{id}/keys` admit no project key
  (Decision 4).
- Last use recorded in memory and flushed once a minute (Decision 9).
- The key listing grows `name`, `created_by` and `last_used_at`; the
  account-deletion dry run lists the keys the account minted
  (Decisions 8, 10).
- Interface: the Keys card's columns and the name field; the Server tab's
  account-deletion card shows the minted keys.
- CLI: `keys ls` columns, `keys create --name`, the `keys` commands marked
  as admin-token commands; `accounts rm`'s preview lists the keys.
- `docs/admin.md`, `docs/accounts.md`, `docs/cli.md`, `docs/api.md`,
  `openapi.json`, `schema.d.ts`, the agent skill's admin reference.

**PR 2 — Scopes.**

- The route table's `Scope` column, the guard's fifth step, the parity
  test, the golden list and the nine-caller matrix (Decisions 2, 3, 13).
- `POST …/keys` takes `scopes` (required); `403` with `insufficient_scope`
  (Decisions 6, 7).
- Revoking the last `ingest` key asks for the echo (Decision 11).
- A key reads its own scopes on the project routes (Decision 12).
- Interface: the scope checkboxes, the secret dialog's lines per scope
  (Decision 14). CLI: `keys create --scope`, `projects show` (Decision 15).
- The endpoint map's `scope` field, `x-tracepad-scope` in the OpenAPI
  document, and every page that tells a reader which key to use
  (Docs to touch).
- One end-to-end case per package (Decision 16).

Not here: keys that belong to an account, key expiry, IP allow-lists,
per-resource scopes, renaming a key or changing its scopes, a key revoking
itself, binding a presigned media upload URL to the key that asked for it,
throttling wrong credentials.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-26** — A project key carries a **set of scopes**, non-empty, drawn from three. **`ingest`** is what a running application does: send spans (both OTLP paths), use the Langfuse media channel, write scores, fetch one prompt. **`read`** is every read of the project's data, and nothing that changes it. **`write`** is every change a key may make to the project: prompts, datasets and their items, runs, score configs, queues and the work in them, retracting a score, deleting traces, retention windows, user-data erasure. Any combination is a valid key; one with all three can do what every key can do today, minus managing keys (Decision 4) | Four kinds of program hold keys and want different things: the production application (`ingest`), an agent, an MCP client, a dashboard or an export (`read`), an online judge that reads traces and writes scores (`ingest` + `read`), and the eval harness, a CI job or an operator's script (all three). The packages take one key per process, so a program that needs two of the three must be able to hold both in one key — which is why the scopes are a set rather than a ladder. Three words, because each is a line a lost credential should not cross: an `ingest` key cannot read what anybody else sent, and the store holds the end users' prompts and outputs; a `read` key cannot change anything, which makes spec 005 #13's "the MCP surface cannot modify or delete anything" true of the credential and not only of the tools; `write` is the rest. **Rejected:** keys that mirror the roles (`ingest`/`viewer`/`editor`) — a viewer annotates, so a "viewer key" retracts scores and claims queue items, and read-only is the property an agent's key needs; per-resource scopes (`traces:read`, `prompts:write`, …) — a dozen words times every route, every doc and a mint form nobody reads, for a tool whose typical deployment has one developer; a fourth `admin` scope for retention and erasure — `write` already bulk-deletes traces behind the same echo (spec 035), so the fourth word would split one blast radius in two without shrinking either. |
| 2 | **2026-09-26** — The route table (`routes.go`) gains a **`Scope` column**: the scope a key must hold to be admitted — `ingest`, `read` or `write` — or `any` (every key) or `none` (no key, whatever its scopes). Public routes declare `any`. The zero value is `unset`, which admits no key, and the parity test fails on it, exactly as it fails on an unset policy (spec 028 #7). `GET /api/v1` publishes the word beside the policy, and the OpenAPI document carries it on every operation as `x-tracepad-scope`, held to the table by the document's parity test | Spec 028's argument, one axis further: a permission matrix in prose is checked by nobody, a column in the table the router is built from is checked by the compiler for presence and by one test for meaning. **Declared, not derived** from the policy and the method: a derivation would put `GET …/next` (which claims an item) in `read` and `POST /api/v1/scores` in `write`, and each exception would be a rule inside the derivation where no reviewer reads it; a declared column makes every route a decision somebody saw. The policy and the scope are orthogonal: the policy says which kinds of caller a route admits, the scope narrows what a key among them may do, and sessions and the admin token never consult it. |
| 3 | **2026-09-26** — The mapping follows four rules, and the complete list is the API contract below. `any`: the public routes, and `GET /api/v1/projects`, `GET /api/v1/projects/{id}` and `GET /api/v1/prompts/{name}`. `ingest`: the five routes of the `ingest` policy, and `POST /api/v1/scores`. `read`: every other `GET` of a `member` route, except `GET /api/v1/queues/{name}/next`. `write`: every other `member` and `editor` route a key reaches, `next` included. Deleting traces, erasing a user and moving a retention window stay reachable by a key that holds `write` | `POST /api/v1/scores` is `ingest` because the packages' score call *is* the production path — an end user's thumbs-up, written by the application (specs 017, 032, 033) — and an ingest key that could not write it would push every application with a feedback button onto a key that also reads. Fetching one prompt is `any` for the same reason: an application that uses prompt management fetches its prompts at run time with the only key it has, and it already holds what it fetches; listing prompts, their versions and their diffs stays `read`. The project listing and the project read are `any` because the CLI resolves its project through the listing (`internal/cli/admin.go`, `projectID`) and because a key needs a way to learn what it holds (Decision 12). `next` is `write` because it claims an item for ten minutes (spec 024 #5): a key that changes who is handed what is not a read-only key. Trace deletion, erasure and retention stay on keys, rather than moving to sessions, because bulk deletion is a feature of the packages (spec 036) and erasure is what an application's own "delete my account" flow calls; both keep their dry run and their echo (spec 005 #8). |
| 4 | **2026-09-26** — **No project key lists, mints or revokes keys**, whatever its scopes: the three routes under `/api/v1/projects/{id}/keys` declare `none` and answer a key `403 "a project key cannot list, mint or revoke keys; that needs an owner or editor signed in, or the admin token"`. Owner and `editor` sessions and the admin token keep all three. This supersedes the key-management half of spec 005 #11 and #12, and removes "a project key" from the keys entry of spec 028 #3's `editor` row | Issuing a credential is a person's act. A key that mints keys turns one lost key into as many credentials as its finder wants, each of which outlives the revocation of the first. The obvious repair — revoking what a key minted together with it — breaks the rotation this API exists for: `keys create` with the old key, move the SDKs, `keys rm` the old one, and the new key dies with its parent. Taking the power away is the only version that closes the hole and keeps rotation whole. What it costs: `tracepad keys …` run with a project key stops working, and the Keys card in Settings or the CLI with the admin token takes its place; no deployment is left without a way, because every server has an owner who can sign in (spec 028 #2, #22). An agent that follows the skill can no longer mint or revoke keys, which is spec 005 #13's principle — an agent should hold nothing it can destroy with — applied to credentials. Listing goes with minting because Decision 8 puts the minter's email in the listing, and a program has no need of it. |
| 5 | **2026-09-26** — Every key that exists when schema 0023 runs gets **all three scopes**. So do the keys the server creates by itself — the first-start `default` project's and those `TRACEPAD_PROJECTS` declares — and the first key `POST /api/v1/projects` answers with | An upgrade must not break the running application, nor the CLI, harness or MCP client configured with the same key; every existing key already has these powers, so all three changes nothing a key does except Decision 4. A narrower default would answer `403` to a harness or an agent on the morning after an upgrade nobody asked to change anything. A new project's first key stays whole because it is the one the startup banner and the quickstart hand to the SDK, the CLI and the MCP client alike (`docs/quickstart.md`); the docs steer production to an `ingest` key minted beside it, and the listing marks every key that predates this spec (`created_by.kind: "unknown"`, Decision 8) so an owner can find the ones to rotate. |
| 6 | **2026-09-26** — `POST /api/v1/projects/{id}/keys` takes `{scopes, name?}`. `scopes` is **required**: an array of one to three of the three words, duplicates collapsed; missing, empty or an unknown word is `400` naming the three. `name` is optional, trimmed, at most 64 characters, not unique. **A key's scopes never change**: to narrow or widen one, mint a new key and revoke the old | The scopes are the one decision minting a key is about, so a request that leaves them out cannot mean what it says (spec 003 #23), and a default would decide it silently in one direction or the other. Immutable, because widening a key widens every copy of it — including the one that got away — while rotation is already zero-downtime (spec 005 #12). A name, because the listing's job is to answer "which program holds this key" before somebody revokes it, and `tp-pk-3f9a…` answers nothing. |
| 7 | **2026-09-26** — A key whose scopes do not cover a route gets **`403`**, the header `WWW-Authenticate: Bearer error="insufficient_scope", scope="<needed>"`, and the usual body, `{"error": "this key's scopes are ingest; GET /api/v1/traces needs read"}` — the route's pattern, never the request's ids. A key on a `none` route gets `403` and Decision 4's message, with no `scope` in the header because no scope would do. Sessions and the admin token are never answered in terms of scopes | `403`, not `401`: the credential is good, it is asking for something it cannot have. RFC 6750 §3.1 defines exactly this header for exactly this case, so a program can read the scope it lacks without parsing prose, and the body keeps the one error shape the packages and the CLI already surface verbatim (spec 017 #9; spec 028 #15's rule that the server's text is the one that knows why). OpenTelemetry exporters treat `403` as not retryable, which is right: a retry will not grow a scope. |
| 8 | **2026-09-26** — Every key records **who minted it**: `created_via` — `account`, `admin_token`, `startup` (the server created it: the first-start project or `TRACEPAD_PROJECTS`) or `unknown` (it existed before schema 0023) — and, for `account`, the account's id (`ON DELETE SET NULL`) and its email as it was at minting. The listing answers `created_by: {kind, account_id?, email?, standing?}`, where `standing` is the minter's relation to the project now: `owner`, `editor`, `viewer`, `removed` (no role here any more), `disabled` or `deleted`. Everyone who may list keys sees it, editors included | The question an owner asks when somebody leaves is "which keys did they see the secret of", and nothing answers it today. The email is kept because account deletion is exactly the moment the answer matters and the account row is gone by then (spec 028 #12); deleting an account still takes nothing else with it, since the reference only goes null. `standing` is computed when the listing is read, so it is never stale. Editors see the minter although the member list is owner-only (spec 028 #12): who issued a credential is part of what the credential is, and an editor who may revoke a key needs to know whose program it probably serves. Recording the *minting key* was considered and dropped: under Decision 4 it would always be empty. |
| 9 | **2026-09-26** — `last_used_at` is the last time the key **authenticated** a request, admitted or refused, to the minute. The guard records it in memory — one map, public key → time — and a ticker submits one writer job a minute that sets `last_used_at = max(last_used_at, seen)` for every key seen since the last one; `Shutdown` submits a final job after the HTTP server has drained and before the writer closes. A job that fails puts its values back for the next tick. The listing answers the later of the stored and the unflushed value | A write per request would put a row update in front of every ingest batch and turn every read into a write, on the one path everything serialises through (spec 003 #9); sessions slide at most daily for the same reason (spec 028 #4). One job a minute, touching only the keys used in that minute, is invisible next to ingest. A minute rather than a day because the question it answers during a rotation — "has the old key gone quiet since I moved the SDKs?" — is a question about minutes. Refused requests count, because the question is whether anybody still holds the key, and a lost `ingest` key probing reads is exactly the use an owner wants to see. `max`, so a late job never moves the time back. A crash loses at most a minute of last-use, and the docs say so. |
| 10 | **2026-09-26** — **No cascade.** Removing a member, demoting an editor, disabling or deleting an account revokes no key. Instead the listing shows the minter's `standing` (Decision 8), the Keys card marks every key whose minter can no longer manage keys in the project, and the dry run of `DELETE /api/v1/accounts/{id}` lists the keys the account minted that still exist, in every project | The person who mints a key is very often the person who wires production, so an automatic cascade turns an owner's routine act into an unannounced ingest outage — the failure spec 005 #12 guards against for the last key. An opt-in flag on the four calls that take access away (`DELETE` and `PATCH` of an account, `PUT` and `DELETE` of a membership) is more API than the problem: the owner who sees a departed person's keys listed can rotate them deliberately — mint, move, revoke — which is the only order that loses no spans. A key minted by a key cannot occur after Decision 4, so there is no key-to-key cascade to decide. |
| 11 | **2026-09-26** — Revoking **the last key that carries `ingest`** asks for the project's name, as revoking the last key does; the preview's note says that ingest stops until another `ingest` key is minted | Spec 005 #12 asks for the echo because "a project with zero keys cannot ingest". With scopes, a project whose remaining keys are all `read` cannot either, and the guard should follow its reason rather than its old wording. |
| 12 | **2026-09-26** — `GET /api/v1/projects` and `GET /api/v1/projects/{id}` answer a key with its own project as today, plus `key: {public_key, name, scopes}`. `tracepad projects show` prints the line | A program — or an agent following the skill — that meets a `403` needs a way to learn what it holds, and the key listing is no longer one (Decision 4). The field is additive; a key's row still has no `role` (spec 028's shape for a key). |
| 13 | **2026-09-26** — The scope check is a **fifth step** of the guard, after the project step: credential, origin, policy, project, scope (`internal/server/auth.go`, `resolve`). Last use is recorded at the first | A soft-deleted project's key keeps the answers spec 005 #10 and spec 028 #21 give it — `401` everywhere but reading the project — instead of being told which scope it lacks, and every verdict the six-caller matrix asserts today for a key holds unchanged for a key with all three scopes, the three key routes apart (#4). On the key routes, which no key reaches any more, a deleted project's key gets the same `403` as a live one. |
| 14 | **2026-09-26** — The Keys card: minting opens a small form — name, three checkboxes each with its one line from Decision 1, **`ingest` checked by default** — and the list shows name, public key, scopes, created (when and by whom, with the standing) and last used ("never" until it is). A key whose minter's standing is not `owner` or `editor` carries a line saying the person who minted it can no longer manage keys here. The secret dialog shows the lines that fit the scopes: the OpenTelemetry and Langfuse variables for `ingest`, `TRACEPAD_API_KEY` for `read` or `write`. The Server tab's account-deletion card shows the keys from the dry run | Least privilege is the default wherever a person chooses, and `ingest` is the most common reason to mint: wiring an application. Showing the minter's standing on the row, rather than behind a click, is spec 028 #14's rule for the Accounts table — the page an owner opens to answer "who can reach what". The dialog's lines follow the scopes because a `read` key pasted into an exporter's headers is a mistake the dialog can prevent by not suggesting it. |
| 15 | **2026-09-26** — CLI: `tracepad keys create --scope ingest[,read,write] [--name NAME]`, with `--scope` required and its absence a usage error that lists the three; `keys ls` shows PUBLIC KEY, NAME, SCOPES, CREATED BY, LAST USED; `tracepad help` marks the `keys` commands as needing the admin token; `projects show` prints the key's scopes; `accounts rm`'s preview lists the keys the account minted | The CLI stays a client of the API with no logic of its own (spec 004 #1), so it asks for what the API asks for, in the same words. |
| 16 | **2026-09-26** — The MCP server and the three packages change **no code**. The docs say which scopes each needs: the MCP server and the CLI's read commands `read`; a production application `ingest`; the eval harness `ingest`, `read` and `write`; `export --otlp` reads its source with `read`, and the receiving server's key needs `ingest`. Each package's end-to-end suite gains one case: an `ingest` key sends a span, writes a score and fetches a prompt, and deleting traces raises the `403` with the server's message | The MCP tools call the read API with the caller's own credential (spec 004 #16), so a `read` key is a complete MCP credential and an `ingest` key's tool calls come back with the server's message. The packages send one key and surface a non-2xx REST answer whole (spec 017 #9 and its counterparts); the tracing path logs and drops, as it does for a `401`. The end-to-end case pins the one claim the docs make about the packages — that `ingest` covers the production path — so a future package feature that needs `read` fails a test instead of a deploy. |
| 17 | **2026-09-26** — The application-line ceiling (`UI_BUDGET`) rises from 21,900 to **22,200** | `main` measured 21,863 when this spec was accepted, 37 under the ceiling. PR 1 measured 130 lines (21,993) after its review: the Keys card's table, its name field with its length check, its minter line, and the account-deletion card's list of keys. PR 2's checkboxes and the dialog's scope-dependent lines are estimated at ~150, and one review cycle needs room — the raise rule of spec 035 #17, 036 #8 and 041 #20: the measured `main`, plus the measured addition, plus a review cycle, raised once for the known set. The owner approved the raise in advance. |
| 18 | **2026-09-26** — PR 1 enforces #4 **before the `Scope` column exists**, with a policy of its own: the three routes under `/api/v1/projects/{id}/keys` declare **`issuer`** in the route table — `editor` without the project key — and the guard's policy step answers a key there with #4's `403`, before the project step, so a soft-deleted project's key gets the same answer as a live one (#13). The endpoint map names the policy `editor`, which is what it is to everyone but a key, and the OpenAPI operations declare the session and the admin token as their security. A parity test holds that every route under `…/keys` is `issuer` and nothing else is. PR 2 moves the key's half into the column's `none` and changes no answer | The column, its parity test and the golden list are PR 2's (Overview); the rule still has to live in the table the router is built from — a path comparison inside the guard would admit a key on any key route added under another pattern, and spec 041's `presigned` is the precedent for a policy the endpoint map spells as another. *(Amended in review of #114: the first version compared the path in a fifth guard step.)* |
| 19 | **2026-09-26** — Details #14 and #15 leave open, settled in PR 1: `keys ls` keeps its **CREATED** column beside the five of #15 (PUBLIC KEY, NAME, SCOPES, CREATED, CREATED BY, LAST USED), and CREATED BY reads `email (standing)`, `admin token`, `server` or `unknown`. `keys create --name` sends `{name}` and nothing when it is absent: in PR 1 the body is optional and carries only the name, until PR 2 makes `scopes` required. The Keys card's name field sits inline beside the mint button; PR 2 turns it into the form with the checkboxes. The key's identity rides on the guard's caller (`caller.key`, the stored row) for any handler that needs to name it, as spec 041 #28's grant will | The listing is ordered by creation and the date is what a rotation is read by (spec 005 #12); dropping a column the command already printed would be a regression #15 did not ask for. |
| 20 | **2026-09-27** — A key's name (amends #6) is refused with **`422`**, as an account's name is (spec 028 #26), when it is over 64 characters — counted in characters, as the interface counts it too — or holds a control character, a line or paragraph separator, or a format character — a bidirectional control such as U+202E, a zero-width space or joiner, a byte-order mark; everything else in #6 stands. The Keys card checks the length in code rather than with `maxlength`, which counts UTF-16 units | The name is printed in the listing, the Keys card, the CLI's table and an account's deletion preview: a newline or a separator opens a line of its own there, and an override or an invisible character makes one key's name read as another's. `422` rather than #6's `400`, because the request is well-formed and its value is what is refused — the distinction the account routes already draw. |

## Data contract

Schema **0023** rebuilds `api_keys` (the table is a handful of rows) by
the procedure spec 005 #16 used for `traces`, so that `scopes` and
`created_via` have **no default**: an insert that does not say what a key
may do, or who made it, fails loudly instead of inheriting whatever the
backfill needed.

```sql
CREATE TABLE api_keys_new (
    public_key       TEXT PRIMARY KEY,
    secret_hash      BLOB NOT NULL UNIQUE,
    project_id       TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    name             TEXT NOT NULL DEFAULT '',
    -- The canonical spelling of a non-empty subset, in this order (#1);
    -- until a mint can choose (#6), every key is 'ingest read write'.
    scopes           TEXT NOT NULL CHECK (scopes IN (
                         'ingest', 'read', 'write', 'ingest read', 'ingest write',
                         'read write', 'ingest read write')),
    created_via      TEXT NOT NULL CHECK (created_via IN (
                         'account', 'admin_token', 'startup', 'unknown')),
    created_by       TEXT REFERENCES accounts(id) ON DELETE SET NULL,
    created_by_email TEXT NOT NULL DEFAULT '',   -- as it was at minting (#8)
    last_used_at     INTEGER                     -- Unix ns; NULL = not since 0023 (#9)
) STRICT;

INSERT INTO api_keys_new (public_key, secret_hash, project_id, created_at, scopes, created_via)
SELECT public_key, secret_hash, project_id, created_at, 'ingest read write', 'unknown'
  FROM api_keys;

DROP TABLE api_keys;
ALTER TABLE api_keys_new RENAME TO api_keys;
CREATE INDEX idx_api_keys_project ON api_keys(project_id);
```

`created_at` keeps its RFC 3339 text, as 0001 wrote it; `last_used_at` is
nanoseconds, as every timestamp since 0002 is (spec 028 #18). The secret
hashes are copied, so every secret that authenticated before the migration
authenticates after it. The store's key lookup returns the key row — public
key and scopes — with its project, in the one indexed query it is today
(`internal/store/store.go`, `ProjectBySecret`).

## API contract

Errors are `{error}` as everywhere. New and changed answers:

| Method, path | Who | Body → answer |
|---|---|---|
| `GET /api/v1/projects/{id}/keys` | owner or `editor` session, admin token | `{keys: [{public_key, name, scopes, created_at, created_by: {kind, account_id?, email?, standing?}, last_used_at}]}`, oldest first. `kind` is `account`, `admin_token`, `startup` or `unknown`; `standing` only for `account`; `last_used_at` is `null` until the key is used |
| `POST /api/v1/projects/{id}/keys` | owner or `editor` session, admin token | `{scopes: ["ingest", …], name?}` → `201 {public_key, secret_key, name, scopes, created_at, created_by, note}`; `400` for missing, empty or unknown scopes or a name over 64 characters |
| `DELETE /api/v1/projects/{id}/keys/{public_key}` | owner or `editor` session, admin token | As spec 005 #12, with the echo also asked when the key is the last one carrying `ingest` (#11) |
| any of the three, with a key | — | `403 "a project key cannot list, mint or revoke keys; that needs an owner or editor signed in, or the admin token"` |
| `GET /api/v1/projects`, `GET /api/v1/projects/{id}` | any key (and as today for the rest) | A key's row gains `key: {public_key, name, scopes}` |
| `DELETE /api/v1/accounts/{id}` (dry run) | owner, admin token | Adds `keys: [{project_id, project_name, public_key, name, scopes, last_used_at}]` — the keys the account minted that still exist. They are not in `would_delete`, because the deletion does not delete them (#10) |
| `GET /api/v1` | public | Every endpoint gains `scope`: `any`, `ingest`, `read`, `write` or `none` |
| any route, a key without the scope | — | `403`, `WWW-Authenticate: Bearer error="insufficient_scope", scope="<needed>"`, `{error: "this key's scopes are <held>; <METHOD> <pattern> needs <needed>"}` |

The scope each route asks of a key (100 routes; the golden test holds this
list):

| Scope | Routes |
|---|---|
| `ingest` (6) | `POST /v1/traces` · `POST /api/public/otel/v1/traces` · `POST /api/public/media` · `PATCH /api/public/media/{mediaId}` · `GET /api/public/media/{mediaId}` · `POST /api/v1/scores` |
| `read` (36) | `GET` of: `/api/v1/system` · `/api/v1/traces` · `/api/v1/traces/last` · `/api/v1/traces/{id}` · `/api/v1/observations/{id}/io` · `/api/v1/media/{sha256}` · `/api/v1/raw` · `/api/v1/raw/{id}` · `/api/v1/sessions` · `/api/v1/sessions/{id}` · `/api/v1/stats` · `/api/v1/stats/scores` · `/api/v1/facets` · `/api/v1/users` · `/api/v1/users/{id}` · `/api/v1/scores` · `/api/v1/scores/{id}` · `/api/v1/prompts` · `/api/v1/prompts/{name}/versions` · `/api/v1/prompts/{name}/diff` · `/api/v1/datasets` · `/api/v1/datasets/{name}` · `/api/v1/datasets/{name}/items` · `/api/v1/datasets/{name}/items/{id}` · `/api/v1/datasets/{name}/items/{id}/versions` · `/api/v1/datasets/{name}/runs` · `/api/v1/runs` · `/api/v1/runs/{id}` · `/api/v1/runs/{id}/items` · `/api/v1/runs/{a}/compare/{b}` · `/api/v1/score-configs` · `/api/v1/score-configs/{name}` · `/api/v1/queues` · `/api/v1/queues/{name}` · `/api/v1/queues/{name}/items` · `/api/v1/queues/{name}/items/{id}` |
| `write` (27) | `DELETE /api/v1/traces/{id}` · `DELETE /api/v1/traces` · `DELETE /api/v1/scores/{id}` · `POST /api/v1/prompts/{name}/versions` · `DELETE /api/v1/prompts/{name}` · `PUT` and `DELETE /api/v1/prompts/{name}/labels/{label}` · `PUT` and `DELETE /api/v1/datasets/{name}` · `POST /api/v1/datasets/{name}/items` · `DELETE /api/v1/datasets/{name}/items/{id}` · `POST /api/v1/datasets/{name}/runs` · `POST /api/v1/runs/{id}/finish` · `DELETE /api/v1/runs/{id}` · `PUT` and `DELETE /api/v1/score-configs/{name}` · `PUT` and `DELETE /api/v1/queues/{name}` · `POST /api/v1/queues/{name}/items` · `POST /api/v1/queues/{name}/items/from-traces` · `GET /api/v1/queues/{name}/next` · `POST /api/v1/queues/{name}/items/{id}/complete` · `…/skip` · `…/reopen` · `DELETE /api/v1/queues/{name}/items/{id}` · `PATCH /api/v1/projects/{id}` · `DELETE /api/v1/projects/{id}/users/{user_id}/data` |
| `any` (11) | The eight public routes (`GET /health`, `PUT /api/public/media/{mediaId}/upload`, `GET /api/v1`, `GET /api/v1/openapi.json`, `GET` and `POST /api/v1/setup`, `POST /api/v1/auth/login`, `POST /api/v1/auth/accept-invite`) · `GET /api/v1/projects` · `GET /api/v1/projects/{id}` · `GET /api/v1/prompts/{name}` |
| `none` (20) | The five `session` routes under `/api/v1/auth` · the twelve `owner` routes (`POST /api/v1/projects`, `DELETE /api/v1/projects/{id}`, `POST /api/v1/projects/{id}/restore`, `GET /api/v1/projects/{id}/members`, the eight under `/api/v1/accounts`) · `GET` and `POST /api/v1/projects/{id}/keys` · `DELETE /api/v1/projects/{id}/keys/{public_key}` |

A route that answers both a key and a session (`member`, `editor`) asks the
scope of the key only; the session's role decides for the session, exactly
as spec 028 #3 says. `PATCH /api/v1/projects/{id}` with a new name is still
refused to a key inside the handler (spec 028 #19): `write` moves retention,
it does not rename.

## Application contract

**The guard.** `identify` resolves the key row, not only its project, and
records the key's use. `resolve` gains its fifth step (#13): for a key, the
route's `Scope` against the key's `scopes` — `any` admits, `none` refuses
with #4's message, a word admits when the key holds it and otherwise
refuses with #7's answer.

**Last use.** A mutex-guarded map in the server, touched once per
key-authenticated request; a one-minute ticker and `Shutdown` flush it
through the writer as one job (#9). No configuration: the interval is a
constant, like the session slide.

**Interface.** The Keys card and the secret dialog per #14; the account
deletion card per #10. A viewer still sees the card's read-only line and
nothing else (spec 028 #14).

**CLI.** Per #15. A key used for `keys …` gets the server's `403` on
stderr, exit 1, as every refusal does.

## Testing

1. **Route-table parity** — `TestEveryRouteDeclaresAScope`: no route is
   `unset`; every `public` route is `any`; every `owner` and `session`
   route and the three key routes are `none`; every route of the `ingest`
   policy is `ingest`; `read` appears only on `GET` routes; `next` is
   `write` by name, so the one `GET` that writes cannot drift into `read`.
2. **Golden list** — `TestKeyScopesMatchTheSpec`: the table above as test
   data, compared with the route table both ways. A route added, removed or
   re-scoped fails until the list moves with it, which is what makes this
   spec, and not the code, the oracle for a scope.
3. **The matrix** — `TestPermissionMatrix` grows from six callers to nine:
   no credential, a key with all three scopes, an `ingest` key, a `read`
   key, a `write` key, the admin token, and the viewer, editor and owner
   sessions. The verdict is computed from the policy and the scope columns;
   a scope refusal is asserted by status, message fragment and the
   `WWW-Authenticate` header.
4. **Red without the fix** — two tests that fail on `main`:
   `TestAKeyCannotMintAKey` (the harness's key `POST`s
   `/api/v1/projects/{id}/keys`: `403`, where `main` answers `201`) and
   `TestAnIngestKeyCannotRead` (an editor session mints
   `{"scopes": ["ingest"]}`; the new key posts a span, writes a score and
   fetches a prompt, and its `GET /api/v1/traces` is `403` with
   `insufficient_scope` — on `main` the body is ignored, the key is an
   administrator and the read is `200`).
5. **Migration** — 0023 on a store with projects, keys and accounts: every
   key comes out `ingest read write`, `unknown`, never used; a secret
   minted before the migration authenticates after it;
   `TestKeyLoginKeepsWorking` still passes for that key on every route but
   the three key routes, which now answer `403`.
6. **Provenance** — a key minted by an editor session records the account
   and its email with standing `editor`; by the admin token, `admin_token`;
   the first-start project's and a `TRACEPAD_PROJECTS` key, `startup`; a
   project created by an owner, that owner. Standing moves with the
   account: membership removed → `removed`, demoted → `viewer`, disabled →
   `disabled`, deleted → `deleted` with the email still answered.
7. **Last use** — requests before a tick write nothing; the tick submits
   one job for every key seen; an older value never overwrites a newer one;
   a refused request counts; the listing shows an unflushed use;
   `Shutdown` flushes (the row is read after the server is closed); a
   revoked key's pending use is dropped without error.
8. **Minting** — `scopes` missing, empty, or with an unknown word → `400`;
   duplicates collapse; a 65-character name → `400`; the answer echoes the
   scopes and the name.
9. **The last `ingest` key** — with one all-scopes key and one `read` key,
   revoking the first answers the dry run and the echo applies it;
   revoking the `read` key needs no echo.
10. **Account deletion preview** — lists the keys the account minted in
    two projects, and deleting the account leaves them working.
11. **Endpoint map and OpenAPI** — every endpoint in `GET /api/v1` carries
    its `scope`; every operation in the document carries the same word as
    `x-tracepad-scope` (the existing document parity test, extended).
12. **MCP** — with a `read` key every tool answers; with an `ingest` key a
    tool call's error carries "needs read".
13. **CLI** — `keys create` without `--scope` is a usage error (exit 2);
    `--scope ingest,read` sends both; `keys ls` renders the new columns;
    `keys ls` with a project key prints the server's `403`; `projects show`
    prints the key's scopes; `accounts rm`'s preview lists minted keys.
14. **Packages** — one end-to-end case per package (#16).
15. **Interface** — vitest: the mint form sends `ingest` unless changed,
    the dialog's lines follow the scopes, the former-minter line renders.
    Playwright: an owner mints an `ingest` key in Settings, and the test
    uses it to post a span (`200`) and to list traces (`403`). A live check
    in Chrome on a copy of the demo data, per the Definition of Done.

## Edge cases

- **A key with `write` and without `read`** is valid. Its dry runs answer
  counts, never content; it simply cannot look at what it changes.
- **A soft-deleted project's key** answers exactly as before on every route
  a key still reaches (#13); on the key routes it gets the same `403` as a
  live key.
- **`Authorization` and a cookie together**: the header wins (spec 028 #5),
  so the key's scopes decide, not the account's role.
- **`Basic` credentials** (the Langfuse shape): the secret half picks the
  row and its scopes; the public half is not checked, as today.
- **Keys that predate this spec** show `created_by.kind: "unknown"`. If a
  key was lost before the upgrade, keys minted with it are among the
  `unknown` ones created after it — the listing is ordered by creation, and
  the docs say to rotate those.
- **An account deleted and re-invited with the same email** is a new
  account: keys the old one minted stay `deleted`, matched by id, not
  by email.
- **An owner who mints a key** has no membership row (spec 028 #2); the
  standing is `owner` from the flag. A pending account cannot mint, because
  it cannot sign in.
- **An ingest key and `GET /api/v1/prompts/{name}`**: the key reads prompt
  text. Accepted (#3): the application that holds the key ships the prompt
  anyway.
- **A presigned media upload URL** outlives the key that asked for it, as
  today; binding it is out of scope.
- **Revoking the last key of a project declared in `TRACEPAD_PROJECTS`**:
  the bootstrap leaves an existing project alone, so a restart does not
  re-add the declared key (`internal/store/bootstrap.go`). `docs/admin.md`
  says otherwise today and is corrected in the first PR.
- **The writer busy or failing at a tick**: the job's values go back into
  the map and are retried; last use is late by a minute, never wrong.
- **`HEAD`** on a `GET` route asks what the `GET` asks.

## Config

No new variables. The last-use interval is a constant (#9).

## Docs to touch

In the PR that changes the behaviour, per the Definition of Done:

- **PR 1:** `docs/admin.md` (the permission table's key column, the Keys
  section's rotation via Settings or the admin token, the `TRACEPAD_PROJECTS`
  sentence), `docs/accounts.md` (the policy table: keys are an editor's and
  an owner's, never a key's), `docs/cli.md` (`keys` needs the admin token;
  the new columns), `docs/api.md` and `openapi.json` (the key routes'
  answers and refusal), `docs/quickstart.md` ("a lost key is replaced in
  Settings → Project → Keys"), `ui/src/lib/api/schema.d.ts`, the agent
  skill's `references/admin.md` (an agent does not mint or revoke keys).
- **PR 2:** `docs/api.md` (scopes, the `403` and its header, the endpoint
  map's `scope`), `openapi.json` (`x-tracepad-scope`, the `bearerKey`
  scheme's description, the mint body), `docs/admin.md` (the key column
  split into `ingest` / `read` / `write`), `docs/accounts.md`,
  `docs/quickstart.md` (mint an `ingest` key for production),
  `docs/mcp.md` (use a `read` key), `docs/ingest.md`,
  `docs/sdk-python.md`, `docs/sdk-js.md`, `docs/sdk-go.md` (which scopes
  each call needs), `docs/scores.md`, `docs/prompts.md`,
  `docs/datasets.md`, `docs/retention.md`, `docs/export.md` (the scope of
  each `curl` and command shown), `docs/cli.md` (`--scope`, `projects
  show`), the agent skill's `SKILL.md` (which key to ask the human for) and
  `references/admin.md`, `AGENTS.md`'s status block.

## Out of scope

- Keys that belong to an account and die with it.
- Key expiry, IP allow-lists, per-resource scopes.
- Renaming a key or changing its scopes after minting (#6).
- A key revoking itself.
- Binding a presigned media upload URL to the key that requested it, so
  that revoking the key voids its outstanding URLs.
- Rate-limiting wrong credentials, and the admin token's minimum strength.
- An audit log of who changed what (spec 028, out of scope there too).
