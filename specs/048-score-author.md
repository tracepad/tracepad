# Spec 048 — Who scored it: the author of a score

**Status:** 🚧 IN PROGRESS
**Sprint:** September 2026

> A score says what someone thought of a trace, and since spec 028 the server
> always knows who that someone is: every write to `/api/v1/scores` arrives
> with a signed-in account or a project key. It writes neither down. Spec 022
> left the reviewer out because "the store has no users"; spec 028 left it for
> "a later spec once accounts exist"; spec 024 asked the annotation desk for a
> typed name, kept in the browser, that anybody can type. So a team that
> reviews traces together ends with judgements nobody can attribute, and a
> judge's verdict and a person's correction of it look the same once the
> person saved it. This spec stamps every score with the credential that wrote
> it, keeps a copy of how that credential was named at the time, and shows it
> where the score is shown. It is a column that cannot be filled in later: a
> score written before it has no author and never will, which is why it lands
> before the schema is frozen by a public release.

---

## Overview

Deliverables, two PRs (the last commit of PR 2 flips the status):

- **PR 1 — the author on the row** (Decisions 1–9): schema 0031 adds four
  columns to `scores`. `POST /api/v1/scores` stamps them from the caller. The
  read API renders an `author` object with the credential's standing today.
  `GET /api/v1/scores?author=` narrows to one author. The CLI shows and
  filters it, the MCP `list_scores` tool returns and filters it, the account
  deletion dry run counts it. `openapi.json`, `schema.d.ts` and the docs
  follow.
- **PR 2 — the author on screen** (Decisions 10–11, 14–19): the scores block
  on the trace, observation and session pages names the author beside the
  source chip. A signed-in reviewer's claim and completion are held by the
  account (schema 0032), and the desk names nobody.

Not here (Decision 12 and *Out of scope*): grouping quality trends by author,
agreement between annotators, who may overwrite whose score, an audit trail of
earlier values.

Builds on spec 003 (scores, their upsert by id), spec 022 #3–#4 and #10 (the
source chip and `metadata.source`), spec 024 #6 (the annotator name), spec 028
#3 and #12 (the `member` policy on score writes; deleting an account takes
nothing else with it), spec 045 #7–#10 (a key's `created_by`, the email kept
"as it was at minting", standing computed at read) and specs 044/047 (erasing
an end user).

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-29** — **The author is the credential that wrote the row, stamped by the server.** A write by a signed-in account records that account. A write by a project key records that key. The body cannot name an author: `author` stays an unknown field, and strict decoding keeps answering it `400`. Nothing else writes a score, so every row written from now on has an author | The server is the only party that knows who is calling. An author the client could choose would be `metadata.annotator` again: a name anybody can type. A key writes for a program, and the key is what the program can be held to. The program's own identity (an LLM judge's model, a harness's run) is the score's `metadata`, as it is today. Spec 028 #3 already admits exactly two kinds of caller to the route; the admin token and the MCP never reach it. |
| 2 | **2026-09-29** — **Four columns, plain text, no foreign key.** `author_kind` (`account` or `key`, `CHECK`ed), `author_id` (the account's id, or the key's public key), `author_name` (the account's display name, or the key's name, at the moment of writing), `author_email` (the account's email at the moment of writing; `''` for a key). All four are `NULL` on a row written before this spec (#6) | A key is hard-deleted when it is revoked (spec 045), so a foreign key to it would either block revocation or forget the author. An account is hard-deleted too (spec 028 #12), and #12's promise that deletion "takes nothing else with it" holds only if no row points at it. A plain id plus a copy of the names is what spec 045 #8 chose for `api_keys.created_by`, for the same reason: deletion is exactly the moment the answer matters, and the row it would come from is gone by then. The public key is an identifier, not a secret, and the keys screen shows it already. |
| 3 | **2026-09-29** — **The last writer is the author.** A score is upserted whole by its id (spec 003). A rewrite of the same id stamps the new caller, as it replaces the value, the comment and `created_at`. `metadata.source` keeps saying where the score came from, and the interface keeps resending it on an edit (spec 022 #10) | The row holds one value, and the author answers "whose value is this". When a person corrects a judge, the value is the person's, and the chip still says *judge*: the two facts are separate on purpose. Keeping the first writer would put the judge's key beside the person's number. Keeping both (`created_by` and `updated_by`) is the start of an audit log, which spec 028 leaves out, and it still would not say who wrote the value in between. |
| 4 | **2026-09-29** — **Rendered as `author`, with its standing today.** Every score the read API returns carries `author: {kind, id, name, email?, standing}` or `author: null`. `standing` is computed at read, as spec 045 #9 does for a key's minter. An account is `owner`, `editor`, `viewer`, `removed` (no longer a member of this project), `disabled` or `deleted`. A key is `active` or `revoked`. `name` is the account's current display name while the account exists, else the name copied at writing. A key's `name` is always the copy. The score listing reads the standing with one `LEFT JOIN` to `accounts` and `memberships`, and one to `api_keys`, per page | A copy says who it was; the standing says whether that person or program is still around, which is what a reviewer weighing an old judgement wants to know. A renamed account shows its new name, because the name is how the team knows the person now. A key's name is shown as it was, because a revoked key has no row to read it from and an active one is rarely renamed. |
| 5 | **2026-09-29** — **The email is shown to editors and owners only.** `author.email` is present when the caller is a signed-in account with the `editor` role in the project, or an owner. For a `viewer`, and for any project key, it is absent, and `name` is all there is. When `name` is empty (an account that never set one), the interface shows *a member* to a viewer | Today a viewer cannot learn a teammate's email anywhere: the member list is the owner's (spec 028), the key list with its minters is the editor's (spec 045 #9). Scores are read by every member, so an email on every score would be a new way to list the team. The editor tier is the one that already sees who minted a key. A key reads for a program, and a program has no need for a person's address. |
| 6 | **2026-09-29** — **A score written before this spec has no author.** The four columns stay `NULL` and the API renders `author: null`; the interface shows nothing where the author would be. No backfill | Nothing on disk says who wrote them. `metadata.annotator` is a name typed into a browser, and putting it in `author` would give an unverifiable string the standing of a credential. Attributing every old score to the only account of a one-person deployment would be wrong for every score an SDK wrote. Before a public release the stores that hold such rows are the maintainers' own. |
| 7 | **2026-09-29** — **Deleting an account keeps its scores and their copies.** Scores it wrote keep `author_id`, `author_name` and `author_email`. Their standing reads `deleted`. The deletion's dry run (spec 028 #12) gains `scores_authored`, the number of scores in all projects whose author is the account, beside the keys it minted. Removing the account from a project, or disabling it, changes the standing and nothing else | Spec 045 #8 kept a deleted minter's email on the key for the reason that applies here word for word: the judgement is still in the data, and "who said this" matters most once the person has left. The count tells the owner what they are about to leave behind, as the key list does. A team that must forget a former member's address has #2's columns to clear by hand; a switch to do it is *Out of scope*. |
| 8 | **2026-09-29** — **Erasing an end user (specs 044/047) does not look at authors.** A score on an erased trace, or on a session only erased traces carried, is deleted as it is today, author and all. A score an account wrote is not the end user's data because of who wrote it, and nothing in an erasure reads, counts or logs an author | Spec 044 erases a person the application observed, identified by the trace's `user_id`. The author is a person on the team that operates the application, a different subject in a different table. Erasing one must not reach the other, and must not grow a new place where it records anything about either (spec 044 #15). |
| 9 | **2026-09-29** — **`GET /api/v1/scores?author=` narrows to one author.** The value is an account id, a public key, or `me`: the caller itself, account or key. A partial index `idx_scores_author(project_id, author_id, timestamp DESC, id DESC) WHERE author_id IS NOT NULL` serves it. CLI: `scores ls --author ID\|me` and an `AUTHOR` column (the name, or `—`). MCP: `list_scores` gains an `author` argument and the field in its output schema | "What have I scored" and "what did this key write" are the two questions an author exists to answer. The index is partial so that the rows without an author cost nothing, and it is keyed like `idx_scores_timestamp` so the filter pages in the same order. |
| 10 | **2026-09-29** — **On screen, the author is named beside the source chip.** The scores block (trace, observation, session) shows `by NAME` after the chip; the email, when present (#5), is its tooltip, and a standing other than a role is shown muted (`removed`, `disabled`, `deleted`, `revoked`). A key's author reads `key NAME`. Nothing changes on the run and comparison screens, whose attempt scores stay the compact shape spec 016 gave them | The chip says what kind of judgement it is (spec 022 #3), the author says whose, and together they read as one line. Muting a gone credential says "this person or program is no longer here" without hiding the judgement. The eval screens compare scores by name across attempts; authorship there is a later question. |
| 11 | **2026-09-29** — **The desk takes a signed-in person's name from the account.** When the interface is signed in, the annotation desk sends the account's display name (or its email, if the name is empty) as `annotator` and does not ask for one; the browser-kept name (spec 024 #6) is still asked for only when the display name and the email are both unavailable. `claimed_by`, `completed_by` and `metadata.annotator` keep their meaning and their free text. The server does not change | The score's author is now the answer to "who said this", and it cannot be typed. Asking the same person to type what the server already knows is friction and a second source of truth. The queue's own fields stay free text because a program working a queue with a key has a name but no account, and spec 024's filters (`?annotator=`) are built on them. |
| 12 | **2026-09-29** — **Quality trends do not group by author.** `scores_hourly` gains no author dimension and `group_by=author` is not offered (spec 025's out of scope stands) | A per-author series multiplies the rollup by the team's size for a question nobody has asked yet. When it is asked, the scores themselves are kept, so the dimension can be added and re-rolled like any rollup column (spec 013 #4) without losing history. |
| 13 | **2026-09-29** — **Cost, measured.** On a copy of a demo store (1,574 scores beside 34,176 observations), every score given an author, half accounts and half keys, then vacuumed: the table grows from 270 KB to 377 KB, **68 bytes a score** on a row of about 172; `idx_scores_author` is 184 KB, **117 bytes a score**, since it carries the project, the author and the listing's whole key. About 185 bytes a score in all: a million scores cost about 185 MB more. The write reads nothing new, the caller being resolved already. A page of 50 from the listing, which gains three `LEFT JOIN`s by primary key and keeps its index (`idx_scores_timestamp`, `idx_scores_name`, `idx_scores_author` by filter), went from 85 to 155 µs at the median and from 0.58 to 0.84 ms at p99 | Scores are few next to spans, and the index is the price of paging one author's scores in the listing's order. What matters more than the bytes is that the columns can only ever be filled at write time. |
| 14 | **2026-09-29** — **A gone author's tooltip says why.** On the scores block, the tooltip of a muted author (#10) is the standing — `removed`, `disabled`, `deleted`, `revoked` — after the email where the reader may see one. The desk's two end-to-end tests about naming the reviewer (changing the name mid-item, dismissing the name dialog) go with the dialog they tested (#16) | Muted alone says "something is different" and not what; the word is the reason, and it costs nothing. |
| 15 | **2026-09-29** — *Owner decision; amends #11 and spec 024 #6.* **A signed-in reviewer is held by the account.** Schema 0032 adds `claimed_by_account` and `completed_by_account` to `annotation_items`, plain account ids beside the free-text `claimed_by` and `completed_by`, one of each pair per row (`CHECK`). A session's `next`, `complete`, `skip` and `reopen` send no `annotator`: the server takes the account from the caller, and a session that sends one is refused with a `400` that says why. A key still sends `annotator`, required, and is held by that name. Reads render `claimed_by` and `completed_by` of an account for the reader, by #5's rule — the display name, else the email for an editor or an owner, else `a member`, or `a deleted account` — and carry the id as `claimed_by_account` and `completed_by_account`. A second completion's `409` names the first finisher the same way, never by email. The listing takes `account=ID\|me` beside `annotator=`; `me` needs a session. The CLI's `queues items` takes `--account` | The review of #11 found what keying a claim on a display name costs: two accounts called the same shared one claim; an account with no name signed with its email, which then sat in `claimed_by`, `completed_by` and the scores' `metadata.annotator` for every member to read, against #5; a name over 200 bytes was a `400` and a dead desk; a rename left the item held under the old name for ten minutes. All four come from the client making up the identity the server already knows. A key has no account, so its reviewer stays the name it sends. |
| 16 | **2026-09-29** — **The desk writes no `annotator` into a score's metadata, and keeps no name of its own.** The desk's scores carry `{"source": "annotation", "queue": …}`; who wrote them is their author (#1). The browser-kept name, its dialog and the *needs a name* screen go: every page of the interface requires a session, so they were unreachable. One `accountName` helper names an account for the shell and for the author chip | The author is the answer to "who said this", recorded by the server; a second, client-written copy in the metadata was the place the email leaked. Code nobody can reach is code nobody tests, and with the claim on the account there is nothing left for it to do. |
| 17 | **2026-09-29** — **No backfill for work done before schema 0032; a known limit.** Claims and completions a signed-in desk wrote before 0032 stay under the display name it sent, in `claimed_by` and `completed_by`. After the upgrade a claim in flight is not resumed by the account: it expires within its ten minutes (#5) and the item is handed out again. `account=me` does not see what the person completed before the upgrade; `annotator=` with the name they typed still finds it. A desk tab left open across the upgrade sends `annotator` and is told to reload by the session's `400` | Before a public release no store holds real review work, and a backfill would have to guess which account a typed name meant — the very guess #15 removes. Ten minutes is the claim's own bound on how long anything can be stuck. |
| 18 | **2026-09-29** — **One rule names a reviewer everywhere, and says when they are gone.** The listing, the item, the answer to a write and the `409` of a second completion or a skip all name an account through the server's one `reviewer` function, for the reader who asked — the store builds no names (`CompletedRefusal` takes the name it is given). Items carry `claimed_by_standing` and `completed_by_standing`, the account's standing now as a score author's (#4); the queue page mutes a reviewer who is removed, disabled or deleted, with the reason in the tooltip, as the scores block does (#10, #14). The caller's own account is named from the session the guard resolved, so the desk's own writes read nothing more. The store refuses a second completion or a skip with `AlreadyCompleted`, which carries the item; the handler names the finisher and renders the `409`, whose words stay the store's. Two differences are kept on purpose: a deleted reviewer is `a deleted account` in the queue, which copies no name, where a score keeps the one it copied (#2, #7) — both say `deleted`; and the rule is written twice, once on the server for the queue and once in the interface for a score's author (`accountName`), both over the name and email the server has already filtered by #5 | Two copies of the naming rule had already drifted (`a member` against `another member`), and the store does not know who is reading. The scores block and the queue page show the same people and should agree whether they are still here. A name copied onto every item would be a column and a migration for bookkeeping, where on a score it is provenance; one rule for both surfaces would change the score's contract to a server-rendered name, which nothing needs yet. |
| 19 | **2026-09-29** — *Amends spec 024 #20.* **Holding an item means a claim that has not expired.** `account=` and `annotator=` list a pending item as the reviewer's only while `claimed_until` is still ahead; an expired claim is anybody's `next` (#5), and listing it under *Mine* would say otherwise | Spec 024 #20 made the filter answer "what has this person got", and an item whose claim ran out is no longer theirs to get. The review found the listing and `next` disagreeing about the same row. |

## Schema

Migration `0031_score_author.sql`:

```sql
ALTER TABLE scores ADD COLUMN author_kind  TEXT CHECK (author_kind IN ('account', 'key'));
ALTER TABLE scores ADD COLUMN author_id    TEXT;
ALTER TABLE scores ADD COLUMN author_name  TEXT;
ALTER TABLE scores ADD COLUMN author_email TEXT;
CREATE INDEX idx_scores_author ON scores(project_id, author_id, timestamp DESC, id DESC)
    WHERE author_id IS NOT NULL;
```

Either all four are `NULL` (a row from before, #6) or all four are set, the
two names possibly empty. Each column after the first carries a `CHECK` that
it is `NULL` exactly when `author_kind` is, which SQLite accepts on `ADD
COLUMN` without rebuilding the table, and `ScoreWrite` refuses an author that
is neither an account nor a key before the row is written.

## API contract

| Where | Change |
|---|---|
| `POST /api/v1/scores` | No new request field; `author` in the body stays `400 unknown field`. Every written row is stamped (#1). The response is unchanged. |
| `GET /api/v1/scores`, `GET /api/v1/scores/{id}` | Each score gains `author` (#4, #5). The listing takes `author=` (#9). |
| `DELETE /api/v1/accounts/{id}` (dry run) | Gains `scores_authored` (#7). |

`author`:

```json
{"kind": "account", "id": "3f…", "name": "Ada", "email": "ada@example.com", "standing": "editor"}
{"kind": "key", "id": "tp-pk-…", "name": "ci-judge", "standing": "revoked"}
null
```

`openapi.json` (`Score`, the list's parameters, the account dry run) and
`ui/src/lib/api/schema.d.ts` declare them.

## Application contract

- `internal/store/scores.go`: `Score.Author` (`*ScoreAuthor{Kind, ID, Name,
  Email, Standing}`, built by `AccountAuthor` and `KeyAuthor`) is written in
  the upsert's `INSERT` and `SET` lists (#3). `Scores` and `Score` read it with
  the standing joins (#4); `ScoreFilter` gains `AuthorID`;
  `ScoresAuthoredBy` counts an account's scores for the deletion's dry run
  (#7). The run screens' `attachScores` reads no author (#10).
- `internal/server/scores.go`: `handleCreateScores` stamps every score with
  `scoreAuthor(caller)`; the reads render it through `renderAuthor`, which
  applies #5 through `seesAuthorEmail`; `author=me` is resolved by
  `authorFilter`.
- `internal/server/accounts.go`: the deletion's dry run answers
  `scores_authored`.
- `internal/cli`: `scores ls` gains `AUTHOR` and `--author`.
- `internal/mcpserver/tools.go`: `list_scores` gains the argument and the
  output field.
- `ui/`: `ScoresBlock.svelte` (#10), the desk's annotator (#11),
  `lib/annotator.svelte.ts`.
- SDKs (Python, TypeScript, Go): **no change**. They write scores and never
  read them; the server stamps the key. Their test fakes ignore the new
  response field. The docs for each SDK say that a score written through it is
  authored by its key.

## Testing

- **Stamping** (#1): a score written with a session is authored by that
  account, with its name and email as they were; one written with a key is
  authored by the key, with its name and an empty email; a body carrying
  `author` is a `400` and writes nothing.
- **Last writer** (#3): a judge's score written by a key, then edited in the
  interface by an account, reads the account as author and keeps
  `metadata.source` `judge`.
- **Standing** (#4, #7): for one score each, the author's account removed
  from the project, disabled, deleted, and the key revoked; each reads the
  standing it should, and the copied name survives the deletion and the
  revocation. A renamed account shows the new name.
- **Who sees the email** (#5), as a table over the permission matrix: owner,
  editor, viewer, a key with each scope.
- **No author before** (#6): a store migrated from 0030 with scores reads
  `author: null` for every old row and stamps every new one.
- **Erasure** (#8): erasing an end user deletes the scores on their traces
  whatever their author, and the erasure's counts and log lines are
  byte-identical to a run without authors.
- **Filter** (#9): `author=me` for a session and for a key; an unknown id is
  an empty page, not an error; paging with the cursor; the query plan uses
  `idx_scores_author`.
- **Account deletion dry run** (#7) counts across two projects.
- **Queue reviewers (#15)**: two accounts with one display name take two
  items and each gets its own back; a rename keeps the item held; an account
  with no name completes an item, and a viewer reads `a member` — never the
  email — in the listing, the item and the second completion's `409`, while
  an editor reads the email; a session sending `annotator` is a `400` on
  `next`, `skip` and `reopen`; `account=me` for a session and a `400` for a
  key; a deleted reviewer reads `a deleted account`; a store from 0031 keeps
  its names, and the `CHECK` refuses a name and an account on one row.
- **E2E (PR 2)**: an editor scores a trace and sees `by NAME`; a viewer sees
  the name without the email; the desk completes an item without asking for a
  name when signed in.

## Edge cases

- **A key used from the interface's session?** Cannot happen: a request with
  `Authorization` is the key's (spec 028 #5), whatever cookie comes with it.
- **The same account writes through a key it minted**: the author is the key.
  The key's minter is on the keys screen for whoever needs the link.
- **An owner who is not a member** writes a score: the standing reads
  `owner`.
- **A score whose trace is deleted** goes with the trace, as now.
- **A retry of a key's write with the same id** stamps the same author again.
- **An account renamed to empty**: `name` falls back to the copy made at
  writing; if that is empty too, `name` is `""` and #5's placeholder applies.

## Docs to touch

- `docs/scores.md`: *Who wrote a score* — the `author` field, who is the
  author, who sees the email, the last-writer rule; the `author` filter; the
  unknown-field rule naming `author` (PR 1).
- `docs/accounts.md`: "a score does not name its author" is replaced by what
  deleting an account leaves on its scores (#7) (PR 1).
- `docs/cli.md` (`scores ls`), `docs/mcp.md` (`list_scores`), and one
  paragraph in each of `docs/sdk-python.md`, `docs/sdk-js.md` and
  `docs/sdk-go.md`: a score written through the package is authored by its key
  (PR 1).
- `openapi.json` and `schema.d.ts` (PR 1).
- `docs/annotation.md`: the desk takes the signed-in name (#11); the
  annotator stays free text for keys (PR 2). `docs/ui.md`: the author on the
  scores block (PR 2).

## Out of scope

- Grouping quality trends by author; agreement or throughput per annotator
  (spec 024's out of scope stands) (#12).
- Who may overwrite or delete whose score; a score locked to its author.
- The history of a score's earlier values and authors (an audit log, spec
  028's out of scope).
- A switch that clears the copied name and email when an account is deleted
  (#7).
- An author on datasets, runs, prompts, score configs or queues.
- A client-supplied author or "on behalf of" (#1).
