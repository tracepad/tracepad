# Annotation queues

Scoring the trace you happen to be reading is one thing. A review programme is
the other way round: somebody decides *which* traces deserve a human verdict,
several people work through them, and the team can see what is done and what
is left.

Two nouns:

- A **queue** is a named list and the **score names** a reviewer must set on
  every item in it. The names are score configs — the same ones an eval
  harness declares (see [scores.md](scores.md#score-configs)) — so a queue and
  a chart never disagree about what `accuracy` means.
- An **item** is one trace, or one observation of a trace. In an agent trace
  the thing to judge is often one generation, so an item can point at it.

The verdicts themselves are not stored here. They are **scores**, written
through the endpoint everything else writes scores through, and *completing*
an item is the server checking that the scores the queue asked for are
actually on the target. Delete a queue and the scores stay: they are the work,
and the queue was only the list of what to do.

Everything below is a plain JSON API that works with `curl`, and every step
has a CLI command and a screen.

## Endpoints

| Method | Path | Purpose | A key needs |
|---|---|---|---|
| `GET` | `/api/v1/queues` | Every queue with its progress | `read` |
| `PUT` | `/api/v1/queues/{name}` | Create a queue or replace it whole | `write` |
| `GET` | `/api/v1/queues/{name}` | One queue: its score names and counts | `read` |
| `DELETE` | `/api/v1/queues/{name}` | Delete it with its items; dry run until `?confirm=` | `write` |
| `POST` | `/api/v1/queues/{name}/items` | Add one target or an array of them | `write` |
| `POST` | `/api/v1/queues/{name}/items/from-traces` | Add every trace a listing filter matches, capped | `write` |
| `GET` | `/api/v1/queues/{name}/items` | The items oldest first, filtered and paginated | `read` |
| `GET` | `/api/v1/queues/{name}/next` | The next item to annotate, claimed | `write` |
| `GET` | `/api/v1/queues/{name}/items/{id}` | One item | `read` |
| `POST` | `/api/v1/queues/{name}/items/{id}/complete` | Mark it done | `write` |
| `POST` | `/api/v1/queues/{name}/items/{id}/skip` | Mark it skipped, with the reason | `write` |
| `POST` | `/api/v1/queues/{name}/items/{id}/reopen` | Back to pending — or just release the claim | `write` |
| `DELETE` | `/api/v1/queues/{name}/items/{id}` | Take one item out of the list | `write` |

Authentication is the same as everywhere: `Authorization: Bearer <secret key>`
(see [ingest.md](ingest.md#authentication)). The last column is the
[scope](api.md#scopes) a key must hold: `next` is `write`, because it claims
the item it hands out, and the verdicts themselves are scores, which a key
writes with `ingest`.

## Declaring a queue

A queue is declarative, like a score config: the `PUT` is the whole queue, and
sending the same body twice writes nothing. Keep it in a script beside the
configs it names.

```sh
curl -X PUT -H "Authorization: Bearer tp-sk-…" \
  http://localhost:4318/api/v1/queues/weekly-review -d '{
    "description": "Did support answer the question, and how did it sound?",
    "score_configs": ["accuracy", "tone"]
  }'
```

```json
{
  "name": "weekly-review",
  "description": "Did support answer the question, and how did it sound?",
  "score_configs": ["accuracy", "tone"],
  "counts": {"pending": 0, "completed": 0, "skipped": 0},
  "created_at": "2026-09-08T09:00:00Z",
  "updated_at": "2026-09-08T09:00:00Z"
}
```

`201` on create, `200` on a replace or a no-op. Every name in `score_configs`
must already be a declared config, or the call is a `400` naming the missing
one — a queue that asked for a name nothing defines would be a form nobody can
fill.

The list can be changed later, and it then governs the items that are not yet
completed. Nothing re-validates what is already done.

The name follows the prompt grammar: one URL path segment, starting with a
letter or a digit, at most 200 characters.

## Filling it

### One target at a time

```sh
curl -H "Authorization: Bearer tp-sk-…" \
  http://localhost:4318/api/v1/queues/weekly-review/items \
  -d '{"trace_id": "4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f"}'
```

```json
{"ids": ["c0ffee00c0ffee00c0ffee00c0ffee00"], "added": 1, "existing": 0}
```

An array works too, all-or-nothing, up to 1,000 targets: an add of more is a
`400` (`an add takes at most 1000 targets; use items/from-traces for a filter`),
refused before any target is looked at, and a filter is what `from-traces` is
for. Adding a target the queue already holds answers with the item it already
is and counts it as `existing`, whatever its status — which is what makes a
script safe to retry. `observation_id` beside `trace_id` queues that step
rather than the whole run.

The target does not have to exist yet: an item may be queued for a trace whose
spans are still in flight, exactly as a score may be written about one.

### Everything a filter matches

```sh
curl -H "Authorization: Bearer tp-sk-…" -X POST \
  "http://localhost:4318/api/v1/queues/weekly-review/items/from-traces?status=error&environment=production&limit=200"
```

```json
{"matched": 431, "added": 200, "existing": 0, "capped": true}
```

The query parameters are the **trace listing's own** — the same names, the
same meanings, the same `400` on one that does not exist (see
[api.md](api.md#filters)). "Queue what I am looking at" is one call with no
second grammar.

The newest `limit` matches are added (1–1000, 100 by default). `matched` is
how many there were, counted exactly; `capped` says more matched than were
taken. To continue, run it again with `to=` at the oldest one you took — the
window is half-open, so nothing is queued twice, and the dedupe above would
catch it anyway.

With no filter at all it takes the newest `limit` traces of the project.

## Annotating

### Taking an item

```sh
curl -H "Authorization: Bearer tp-sk-…" \
  "http://localhost:4318/api/v1/queues/weekly-review/next?annotator=ada"
```

```json
{
  "item": {
    "id": "c0ffee00c0ffee00c0ffee00c0ffee00",
    "trace_id": "4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f",
    "status": "pending",
    "seq": 1,
    "added_at": "2026-09-08T09:01:00Z",
    "claimed_by": "ada",
    "claimed_until": "2026-09-08T09:11:00Z"
  },
  "pending": 200
}
```

Being handed an item **claims** it for ten minutes, so two people at one desk
do not review one trace twice by accident. The window is a constant, not a
setting.

Asking again gives you the same item back: a reload must not hand a reviewer a
different trace mid-verdict. Somebody else asking gets the next one nobody
holds. When nothing is claimable the answer is `{"item": null, "pending": N}`,
and `pending` is then what other people are holding — come back, the claims
expire.

`annotator` is a name the client sends, 1–200 characters: a signature, so
that a team can read "who said this" on the queue. The web desk sends the
signed-in account's name and asks for nothing. A program working a queue with
a key has a name but no account, so it stays free text. Who wrote each verdict
is also on the score itself, as its [author](scores.md#who-wrote-a-score),
which the server records and nobody types.

### Posting the verdict

The verdict is a score, written exactly as any other (see
[scores.md](scores.md#writing-a-score)). The interface stamps where it came
from, and a script should too:

```sh
curl -H "Authorization: Bearer tp-sk-…" http://localhost:4318/api/v1/scores -d '{
  "trace_id": "4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f",
  "name": "accuracy",
  "value": 0.9,
  "metadata": {"source": "annotation", "queue": "weekly-review", "annotator": "ada"}
}'
```

### Completing

```sh
curl -H "Authorization: Bearer tp-sk-…" \
  http://localhost:4318/api/v1/queues/weekly-review/items/c0ffee00c0ffee00c0ffee00c0ffee00/complete \
  -d '{"annotator": "ada"}'
```

The queue promised a shape, and *completed* means the shape was filled. The
server checks it against the scores actually stored:

```json
{"error": "item c0ffee… is missing a score for tone", "missing": ["tone"]}
```

— `409`, and nothing changes. Post the missing score and try again.

Two rules are worth stating:

- **Whoever wrote it counts.** A judge's verdict already on the trace is a
  verdict; the reviewer confirms or edits it rather than repeating it. The
  desk prefills the form from exactly those scores.
- **The target has to match.** A trace item needs scores that name no
  observation; an observation item needs scores that name *its* observation.
  They are different verdicts about different things.

A second completion is a `409` naming who got there first, so a race between
two reviewers is visible to the one who lost it.

### Skipping, reopening, releasing

```sh
curl … /items/{id}/skip   -d '{"annotator": "ada", "reason": "not a support conversation"}'
curl … /items/{id}/reopen -d '{"annotator": "ada"}'
```

Not every trace deserves a verdict, and the reason is what makes that readable
afterwards. Skipping an item somebody has already **completed** is a `409`
naming them: a skip writes the same columns a completion filled, so it would
destroy the record of who decided what rather than add to it. Reopen it first.

`reopen` puts a completed or skipped item back to pending — for a manager who
disagrees with a verdict, or wants a skip looked at again; the scores stay
where they are, because reopening asks for another look and not for a
retraction.

`reopen` on an item that is *already* pending is not refused: what it does to
one is release the claim. That is how the desk's *Later* puts an item back
without waiting out the ten minutes.

## Reading the results

The queue page is the bookkeeping — who completed what, what was skipped and
why:

```sh
curl -H "Authorization: Bearer tp-sk-…" \
  "http://localhost:4318/api/v1/queues/weekly-review/items?status=completed&annotator=ada"
```

Items come back oldest first, in the order they were added, which is the order
they are worked in. `?status=` and `?annotator=` narrow it; paging is the same
keyset both ways as every other listing ([api.md](api.md#paging)).

`annotator=` is "what has this person got": the items they completed or
skipped, and the pending ones they are holding a claim on. So
`?status=pending&annotator=ada` is ada's desk right now, and
`?status=completed&annotator=ada` is her work.

The **verdicts** are read where every score is read — filtered by name, by
target, or by time:

```sh
curl -H "Authorization: Bearer tp-sk-…" \
  "http://localhost:4318/api/v1/scores?name=accuracy&trace_id=4f8c…"
```

A score written from a queue carries `metadata.queue` and
`metadata.annotator`, so "everything ada decided in the weekly review" is a
filter over the scores you already have — and the chip on the trace says
`annotation` where a judge's says `api` and a reader's says `web`.

## The same loop from the command line

```sh
tracepad score-configs push accuracy --file accuracy.json
tracepad score-configs push tone     --file tone.json
tracepad queues put weekly-review --config accuracy --config tone \
  --description "Did support answer the question, and how did it sound?"

# Fill it: one trace, or everything a filter matches.
tracepad queues add weekly-review --trace 4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f
tracepad queues add weekly-review --from-traces --error --env production --limit 200

# Work it. `next` answers with the whole item, so one call gives both ids.
NEXT=$(tracepad queues next weekly-review --annotator ada --json)
ITEM=$(echo "$NEXT" | jq -r .item.id)
TRACE=$(echo "$NEXT" | jq -r .item.trace_id)
tracepad scores add --trace "$TRACE" --name accuracy --value 0.9
tracepad scores add --trace "$TRACE" --name tone --string warm
tracepad queues complete weekly-review "$ITEM" --annotator ada

# Or not.
tracepad queues skip weekly-review "$ITEM" --annotator ada --reason "not a support conversation"

# The bookkeeping.
tracepad queues ls
tracepad queues items weekly-review --status completed
```

The same loop with a judge model in place of `ada` is the same commands: the
CLI can do nothing the API cannot, and the API can do nothing the desk cannot.
See [cli.md](cli.md#queues).

## The same loop from the web interface

*Evals → Queues*. A queue's page shows what is done, by whom and what was
skipped; *Start annotating* opens the desk, which is the loop above with
nothing to type: the trace on the left, one control per score on the right,
*Complete & next*.

Traces reach a queue from two places — the *Add to queue* button on a trace
header or an observation panel, and *Add to queue…* beside the filter bar on
*Traces*, which queues what the current filters match. See
[ui.md](ui.md#queues).

## What happens to items

- **Items follow their trace.** The retention sweep that deletes a trace
  deletes its items in the same job, erasing a user's data
  ([retention.md](retention.md)) takes the items of their traces, and so does
  deleting a trace by hand — one by id, or a filter's worth
  ([admin.md](admin.md#deleting-traces)). An item is a pointer, and a pointer
  to a deleted trace is a desk showing an empty page. Every one of these
  destructive endpoints counts what it takes under `annotation_items` in the
  dry run and in the answer, and deleting a **project** names the queues
  themselves too ([admin.md](admin.md#dry-run-by-default)). A trace deleted
  from the desk itself takes its item with it, and the desk moves on to the
  next.
- **Deleting a queue takes its items and nothing else.** The scores written
  while annotating stay on their traces and expire on their own timestamps,
  like every other score.
- Deleting a **score config** a queue names touches neither the queue nor the
  scores. The queue keeps asking for that name, and the desk shows it as a
  free-typed score with a note; `PUT` the queue again to drop it.

## Edge cases

- **An item whose trace has not arrived.** The desk shows the trace page's
  not-found state, with *Skip* and *Later* available. `complete` is a `409`
  listing every name, because none of them can be on a trace that is not
  there.
- **An observation id that is not in the trace.** The desk shows the trace with
  a note; scores go to the observation id as given, exactly as
  `POST /api/v1/scores` would take them.
- **A claim that expires while the form is open.** The save still posts the
  scores — they are scores — and `complete` still succeeds if nobody else
  completed the item meanwhile. The claim gates `next`, not completion.
- **Two queues over the same trace.** Independent items, shared scores. So
  completing the second may need nothing new: the desk prefills and *Complete*
  is enabled at once.
