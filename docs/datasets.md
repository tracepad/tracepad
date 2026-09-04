# Datasets and runs

An eval is a set of cases, a pass over them, and a comparison against last
time. The traces a pass produces already land in Tracepad; this is where the
cases and the passes land too, so that "did this change make it better" is
answered by the tool that holds the evidence.

Three nouns:

- A **dataset** is a named, versioned set of test cases (**items**). Items are
  append-only: an edit is a new row, a delete is an archive, and every version
  the dataset ever had can be read back.
- A **run** is a container a harness opens around one pass over a dataset. It
  pins the dataset version it ran, carries whatever the harness wants to say
  about what was tried, and is closed by the harness — never inferred.
- A **score config** pins what a score's name means — its type, which
  direction is better, its range — so that names and scales do not drift
  between harnesses. See [scores.md](scores.md#score-configs).

Tracepad never executes an eval. Your harness fetches the items, runs its own
function, exports the traces over OTLP exactly as in production, and posts
scores exactly as today. Two span attributes tie each trace to its run and to
the item it answered. Everything here is a JSON API that works with `curl`.

The reading side is here too: a run's summary, its items with the attempts
they got, and the comparison of two runs. Every number in it is computed by
the server, so the CLI, the MCP tools and anything else you write cannot
disagree about what "improved" means.

## Endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/api/v1/datasets` | List datasets by name |
| `PUT` | `/api/v1/datasets/{name}` | Create a dataset or replace its description and metadata |
| `GET` | `/api/v1/datasets/{name}` | One dataset: version and counts |
| `DELETE` | `/api/v1/datasets/{name}` | Delete it with its items and runs; dry run until `?confirm=` |
| `POST` | `/api/v1/datasets/{name}/items` | Add or edit items, one or an array |
| `GET` | `/api/v1/datasets/{name}/items` | The items at a version, whole |
| `GET` | `/api/v1/datasets/{name}/items/{id}` | One item as of a version |
| `GET` | `/api/v1/datasets/{name}/items/{id}/versions` | Every row of one item's history |
| `DELETE` | `/api/v1/datasets/{name}/items/{id}` | Archive an item at a new version |
| `POST` | `/api/v1/datasets/{name}/runs` | Open a run |
| `GET` | `/api/v1/datasets/{name}/runs` | List a dataset's runs, newest first |
| `GET` | `/api/v1/runs` | List the project's runs across every dataset, newest first |
| `GET` | `/api/v1/runs/{id}` | One run with its summary |
| `GET` | `/api/v1/runs/{id}/items` | The run's cases with the attempts it made at each |
| `GET` | `/api/v1/runs/{a}/compare/{b}` | Two runs of one dataset side by side |
| `POST` | `/api/v1/runs/{id}/finish` | Close a run as finished or failed |
| `DELETE` | `/api/v1/runs/{id}` | Delete a run, releasing its traces |

Authentication is the same as everywhere: `Authorization: Bearer <secret key>`
or `Basic base64(<public key>:<secret key>)`. Everything lives inside one
project; nothing here crosses a project boundary.

The `metadata` of a **dataset** and of a **run** must be a JSON object when it
is sent at all — these are your own dimensions, keyed, and a bare string or
number would be a label with nowhere to put the next one. An **item**'s three
bodies are the exception: `input`, `expected_output` and `metadata` are any
JSON value, because a test case is whatever your function takes.

## The whole loop

The recipe, in the order that makes the numbers trustworthy: declare the
configs, push the cases, open the run, fetch the items **at the version the
run pinned**, stamp every trace, post the scores, close the run.

```sh
TP=http://localhost:4318
AUTH="Authorization: Bearer tp-sk-…"

# 1. Declare what your score names mean. Idempotent; put this in the harness.
curl -X PUT -H "$AUTH" $TP/api/v1/score-configs/accuracy \
  -d '{"data_type": "numeric", "direction": "higher", "min": 0, "max": 1}'
curl -X PUT -H "$AUTH" $TP/api/v1/score-configs/verdict \
  -d '{"data_type": "categorical", "categories": ["pass", "fail"]}'

# 2. Push the cases. Re-running with the same cases changes nothing.
curl -H "$AUTH" $TP/api/v1/datasets/support-golden/items -d '[
  {"id": "a1b2c3d4e5f60718293a4b5c6d7e8f90",
   "input": {"question": "how do I reset my password?"},
   "expected_output": {"answer": "Settings, then Reset."},
   "metadata": {"tags": ["auth"]}},
  {"id": "b2c3d4e5f60718293a4b5c6d7e8f90a1",
   "input": {"question": "can I get a refund?"},
   "expected_output": {"answer": "Within 30 days, yes."}}
]'
# → {"ids": ["a1b2…", "b2c3…"], "version": 1, "changed": 2}

# 3. Open the run. The response says which dataset version it pinned.
RUN=$(curl -H "$AUTH" $TP/api/v1/datasets/support-golden/runs \
  -d '{"name": "prompt v7 / claude-sonnet-5", "metadata": {"prompt": "support-answer@7"}}')
RUN_ID=$(echo "$RUN" | jq -r .id)
VERSION=$(echo "$RUN" | jq -r .dataset_version)

# 4. Fetch the items at that version — not "the current one" — page by page.
PAGE_URL="$TP/api/v1/datasets/support-golden/items?version=$VERSION&limit=500"
while :; do
  PAGE=$(curl -sH "$AUTH" "$PAGE_URL")
  echo "$PAGE" | jq -c '.items[]'          # your harness runs these
  CURSOR=$(echo "$PAGE" | jq -r '.next_cursor // empty')
  [ -n "$CURSOR" ] || break
  PAGE_URL="$TP/api/v1/datasets/support-golden/items?version=$VERSION&limit=500&cursor=$CURSOR"
done
```

`limit` caps at 500, so a dataset larger than that needs the loop: a pass that
silently stopped at the first page would be recorded as a whole run over a
fraction of the cases. The first request omits `cursor` rather than sending it
empty — a parameter given without a value is a `400` everywhere in this API,
so the shorter loop that always appends `&cursor=$CURSOR` fails on its first
page.

Then, for each item, run your function under a trace whose **root span**
carries two attributes:

```
tracepad.run_id  = <RUN_ID>
tracepad.item_id = <item id>
```

With the OpenTelemetry SDK that is one `span.set_attribute` per attribute on
the span you start for the case; with the Langfuse SDK it is the same call on
the trace's root observation. Both must be the 32-hex ids the API handed out —
a run *name* or a case *label* in their place stays visible in the trace's
metadata and links nothing. The item goes only where the run goes: an item id
on a trace that carries no run id anywhere links nothing and is left where you
put it, in the span's metadata.

The two need not sit on the same span. Both are trace-level and may be set at
any level — span, scope or resource — so a run id in the OTel resource of an
eval process and an item id on the span that answered the case link the same
way the recipe above does.

```sh
# 5. Post the scores against the trace ids your exporter produced, as always.
curl -H "$AUTH" $TP/api/v1/scores -d '[
  {"trace_id": "'$TRACE'", "name": "accuracy", "value": 1},
  {"trace_id": "'$TRACE'", "name": "verdict", "data_type": "categorical", "string_value": "pass"}
]'

# 6. Close the run.
curl -H "$AUTH" $TP/api/v1/runs/$RUN_ID/finish -d '{}'
# or, when the harness died:
curl -H "$AUTH" $TP/api/v1/runs/$RUN_ID/finish -d '{"status": "failed", "error": "judge timed out"}'
```

Steps 1 and 2 are declarative and safe to run at the top of every CI job.
Step 3 is safe to retry too if you supply the run's `id` yourself: a second
`POST` with a known id returns the existing run unchanged with a `200`.

## Items and versions

A dataset has one **version clock**. It advances by exactly one on every write
that changes the item set — a `POST` of one or many items, a `DELETE` of one —
and never otherwise.

- A batch is **one tick**, however many items it carries. `version` in the
  response is the dataset's version after the write, `changed` is how many
  items produced a row.
- A `POST` whose items are all byte-equal to what is stored writes nothing
  and leaves the version where it was: `"changed": 0`. Equality is on the
  compacted JSON of `input`, `expected_output` and `metadata` plus the source
  pair, so key order and whitespace are not changes. A retry after a lost
  response is therefore idempotent.
- An **edit** is a `POST` with the same `id` and a different body: a new row,
  one tick. The old row is still readable at every earlier version.
- A **delete** is an archive: `DELETE …/items/{id}` writes a row marked
  archived at a new version. The item is gone from the current version and
  still there at every earlier one. Deleting an item that is unknown, or
  already archived at the current version, is a `404`.
- **Order** is first appearance. Every item gets a `seq` when it is first
  written, keeps it through every edit, and returns to its place if it is
  archived and posted again.

"The dataset at version V" is, per item, the row with the greatest version at
or below V, minus the archived ones. Every read resolves it that way:

```sh
curl -H "$AUTH" "$TP/api/v1/datasets/support-golden/items?version=3"
curl -H "$AUTH" "$TP/api/v1/datasets/support-golden/items/a1b2…?version=3"
curl -H "$AUTH" "$TP/api/v1/datasets/support-golden/items/a1b2…/versions"
```

The last one lists every row of one item, newest first, archived rows
flagged, and it lists them all: an item has as many rows as the dataset had
ticks it took part in, so there is nothing to page. Version `0` is the dataset
before its first item; a version above the current one is a `400`.

### Item fields

| Field | Required | Notes |
|---|---|---|
| `id` | no | 32 lower-case hex characters. Omitted, the server generates one. The same id again is an edit of the same item — hash your case's natural key into it to stay idempotent. |
| `input` | yes | Any JSON value. Tracepad never reads inside it. |
| `expected_output` | no | Any JSON value. |
| `metadata` | no | Any JSON value. |
| `source_trace_id`, `source_observation_id` | no | Where the case came from, when it was cut from a production trace. Strings, not references: the source lives under retention and may be gone, and a case does not stop being a case when its origin does. |

Unknown fields are a `400` naming the field; so is a batch that gives the
same `id` to two items, and an empty batch.

### The listing is whole

`GET …/items` returns item bodies **whole**. It is the one read endpoint
beside `/observations/{id}/io` that ignores the response budget, because its
consumer is the harness and a truncated test case is a different test case.
Its bound is the page (`limit` up to 500, cursors both ways) and the body cap
the store applied at write time (`TRACEPAD_MAX_BODY_BYTES`).

## Runs

```sh
curl -H "$AUTH" $TP/api/v1/datasets/support-golden/runs -d '{
  "id": "optional 32-hex",
  "name": "prompt v7 / claude-sonnet-5",
  "metadata": {"prompt": "support-answer@7", "temperature": 0.2},
  "dataset_version": 12
}'
```

```json
{
  "id": "0e5a7c1d2b3f4a6980c1d2e3f4a5b6c7",
  "dataset": "support-golden",
  "dataset_version": 12,
  "name": "prompt v7 / claude-sonnet-5",
  "metadata": {"prompt": "support-answer@7", "temperature": 0.2},
  "status": "running",
  "error": null,
  "created_at": "2026-09-03T10:00:00Z",
  "finished_at": null
}
```

- `dataset_version` defaults to the dataset's version at that instant. Name an
  older one when the harness fetched its items before it created the run; a
  version above the current is a `400`. The version the harness *fetched* and
  the version it *ran* must be the same number, and handing it out here is
  what makes that true.
- `metadata` is free-form: the dimensions of an experiment are yours. What
  the run actually *did* — which models answered, which prompts ran — will be
  derived from its traces when the summary lands, and is not declared here.
- `status` is `running` until you say otherwise. A run left `running` is
  reported as such with its age; Tracepad never guesses from a timeout,
  because a slow judge and a crashed harness look the same from inside.
- `finish` closes it once: `{}` for `finished`, `{"status": "failed",
  "error": "…"}` for `failed`. A second `finish` is a `409`. A run with zero
  traces can be closed — a harness that failed before its first case still
  closes it, as failed, with the reason.
- Linking does not stop at `finish`: a late span of a trace that started
  inside the run still belongs to it.

`GET /api/v1/datasets/{name}/runs` lists newest first, with the run object
above and no summary: a page of runs is for choosing one.

`GET /api/v1/runs` is the same listing across **every dataset** of the
project — "what ran lately", whichever set it ran — in the same order and
with the same rows, which already say which dataset each belongs to. It takes
two filters, `dataset` (an exact name) and `status` (`running`, `finished` or
`failed`), pages both ways with the same cursor shape, and answers `?count=1`
with the capped total the filters match, as the trace and session listings do
([api.md](api.md#counting)):

```sh
curl -H "$AUTH" "$TP/api/v1/runs?status=running&count=1"
```

A `dataset` that does not exist is an empty page, not a `404`: here the name
is a filter, not an address.

## Reading a run back

`GET /api/v1/runs/{id}` adds the summary — how the run went:

```json
{
  "id": "0e5a…", "dataset": "support-golden", "dataset_version": 12,
  "status": "finished",
  "summary": {
    "items":  {"total": 200, "covered": 198, "missing": 2, "unknown": 0},
    "traces": {"count": 214, "attempts_max": 3, "error_count": 3,
               "total_cost": 1.42, "latency_ms": {"p50": 812, "p95": 2410}},
    "scores": {
      "accuracy": {"data_type": "numeric", "direction": "higher",
                   "count": 214, "mean": 0.81, "min": 0, "max": 1},
      "verdict":  {"data_type": "categorical", "direction": null,
                   "count": 214, "distribution": {"fail": 44, "pass": 170}}
    },
    "models":  ["gpt-5", "gpt-5-mini"],
    "prompts": [{"name": "support-answer", "version": 7}]
  }
}
```

- `covered` is how many of the version's cases got at least one trace, and
  `missing` the rest. `unknown` counts **traces**, not cases: the run's traces
  that no case of its version accounts for — a harness that ran newer cases, a
  mistyped id, or a trace that named the run and no case. `?unknown=true` on
  the items view lists exactly those.
- `error_count` counts **traces** that failed, the same meaning the session
  roll-up gives the word. `attempts_max` is the largest number of traces one
  case got, so a run that retried something three times says so.
- Percentiles are **exact** over the run's traces — a run is hundreds of them,
  and at that size the exact number beats the ±12% of the bucketed ones in
  [`/stats`](api.md#stats).
- `total_cost` is `null` when no attempt carried a cost. Absent cost is not
  zero.
- Score names report `mean`/`min`/`max` when they are numeric or boolean, a
  `distribution` when categorical, and `count` alone for text. `data_type` is
  the type the run's scores under that name actually used — the commonest one
  if they used more than one — which is not always what the name's
  [config](scores.md#score-configs) declares: a config governs what is accepted
  from now on and never re-types a score already stored. `direction` does come
  from the config, and only for a numeric or boolean name; it is `null`
  otherwise, which is what makes a comparison say *changed* rather than
  *improved*.
- `models` and `prompts` are derived from the run's observations — what it
  actually ran, not what the harness declared.

Everything is computed at read time. A run keeps taking late spans after it is
finished, and a stored summary would be wrong in exactly the window somebody
is watching.

### The cases and what was answered

`GET /api/v1/runs/{id}/items` puts the expected output beside the produced one:

```json
{"run": "0e5a…", "dataset": "support-golden", "dataset_version": 12,
 "items": [{
   "id": "a1b2…", "seq": 3,
   "input": {"question": "how do I reset my password?"},
   "expected_output": {"answer": "Settings, then Reset."},
   "attempts": [{
     "trace_id": "4f8c…", "timestamp": "2026-09-03T10:00:02Z",
     "error_count": 0, "total_cost": 0.007, "latency_ms": 640,
     "output": {"answer": "Open Settings and choose Reset."},
     "scores": [{"id": "…", "name": "accuracy", "data_type": "numeric", "value": 1}]
   }]
 }], "next_cursor": null, "prev_cursor": null}
```

- `output` is the trace's **root observation's** output — the earliest starting
  observation with no parent, which is where a harness puts the answer for the
  case. A trace whose root carried no output shows `null` rather than reaching
  down the tree for another span's business.
- `expected_output` and `output` are budgeted: one too large for the response
  arrives as a marker with `trace_id` and `observation_id`, which
  `/api/v1/observations/{id}/io` takes. The scores ride whole.
- A case with no attempt is still a row, with an empty `attempts`: that is what
  "missing" looks like from here.
- `?unknown=true` appends the run's unaccounted traces, grouped by the id they
  named, each row marked `"unknown": true` and carrying no case body. They come
  after every known case, so a cursor walk stays a walk.

### Comparing two runs

`GET /api/v1/runs/{a}/compare/{b}` is the question the whole feature exists
for:

```json
{
  "a": {"id": "0e5a…", "name": "prompt v7", "dataset_version": 12,
        "status": "finished", "created_at": "…"},
  "b": {"id": "1f6b…", "name": "prompt v8", "dataset_version": 12, "…": "…"},
  "dataset": "support-golden", "same_version": true,
  "metadata": {"prompt": {"a": "support-answer@7", "b": "support-answer@8"}},
  "traces": {"count": {"a": 214, "b": 200}, "error_count": {"a": 3, "b": 0},
             "total_cost": {"a": 1.42, "b": 1.10, "delta": -0.32},
             "latency_ms": {"p50": {"a": 812, "b": 700}, "p95": {"a": 2410, "b": 2100}}},
  "scores": [{"name": "accuracy", "data_type": "numeric", "direction": "higher",
              "a": {"mean": 0.81, "count": 214}, "b": {"mean": 0.86, "count": 200},
              "delta": 0.05, "improved": 14, "regressed": 3, "same": 181}],
  "items": [{"id": "a1b2…", "seq": 3, "in": "both",
             "scores": {"accuracy": {"a": 0.5, "b": 1, "delta": 0.5, "verdict": "improved"}}}],
  "next_cursor": null, "prev_cursor": null
}
```

- A case answered **N times** has one value per score name: the **mean** of its
  attempts for a number, so the item rows and the header cannot disagree; for a
  category or a piece of text, the **newest** attempt's value, because there is
  no mean of a word.
- **Equality is exact.** A tolerance would be a number Tracepad invented, so
  `0.7999999` and `0.8` are a change. Round in your judge if that matters.
- `verdict` is `improved` or `regressed` only for names whose config gives a
  direction; without one it is `changed` or `same`, and the header row counts
  `changed` instead of `improved`/`regressed`. A name only one run carried has
  no verdict at all — there is nothing to compare it against.
- `metadata` lists only the keys the two runs disagree about, both values.
- `in` says which run had the case: `both`, `a`, `b` — or
  `only_in_version_a` / `only_in_version_b` when the dataset moved between the
  runs, which `same_version: false` also announces. The version labels win:
  a case missing from the other run's version was never that run's to answer.
- Runs of **different datasets** are a `400`, and so is a run compared with
  itself. Two runs where one is still `running` compare fine — its numbers
  will move.

The header counts are about the whole pair, so they do not change as you page
through `items`.

### What a run keeps

A trace that belongs to a **live run is not swept by retention**. The run is
the evidence for a number somebody will act on, and the interesting run is the
old one — the baseline from before the regression. The exception is stated in
[retention.md](retention.md); its size is in `GET /api/v1/system` under
`runs.pinned_traces`.

`DELETE /api/v1/runs/{id}` is the release valve. It removes one row of
bookkeeping and answers with how many traces it released to the ordinary
retention window — the traces themselves are not deleted:

```json
{"id": "0e5a…", "released_traces": 214}
```

A harness that creates a run per CI job prunes old ones with it, from a
script, with no preview round-trip.

Two things do outrank the pin. Erasing a user's data
(`tracepad users rm-data`) deletes the user's traces whether or not a run
holds them; the dry run names the runs affected. And deleting the dataset
takes its runs with it.

## Orphans

A trace naming a run that does not exist in the project is **stored as any
trace is**, with its columns filled — refusing it would drop the one artifact
that shows what went wrong. It is counted, and each unknown run id is logged
once per process:

```json
"runs": {"pinned_traces": 214, "orphan_traces": 3}
```

in `GET /api/v1/system`. A harness that stamps a wrong id finds out there,
before it reads an empty run. An orphan is not pinned: it lives and dies on
the ordinary retention window. The same happens to the traces of a run whose
dataset was deleted mid-ingest.

A trace re-delivered with a different `run_id` moves: per-field upsert, last
delivery wins, the same rule as every other trace field.

## Deleting a dataset

The one destructive act here. It cascades every run and releases every
pinned trace, so it is a dry run until `?confirm=` echoes the name:

```sh
curl -X DELETE -H "$AUTH" $TP/api/v1/datasets/support-golden
```

```json
{"dry_run": true, "dataset": "support-golden", "items": 200, "runs": 14,
 "pinned_traces": 2996, "confirm": "support-golden"}
```

```sh
curl -X DELETE -H "$AUTH" "$TP/api/v1/datasets/support-golden?confirm=support-golden"
```

Every version of every item and every run go. The traces stay, unpinned.
Deleting a run or a score config needs no confirmation: one row each, and
nothing else is touched.

## The same loop from a shell

Every step above is a command, and the CLI is nothing but a client of the API
([cli.md](cli.md)). This is the whole of it:

```sh
# 1. Declare what the score names mean. Idempotent.
tracepad score-configs push accuracy --file accuracy.json

# 2. Push the cases: a .jsonl (one case per line) or a .json array, one batch,
#    one version tick. Re-running an unchanged file prints "unchanged at
#    version 12" and writes nothing.
tracepad datasets push support-golden --file cases.jsonl
# → version 12: 3 items changed, 200 in the batch

# 3. Open the run and read back the version it pinned.
RUN=$(tracepad runs create support-golden --name "prompt v8" --json)
RUN_ID=$(echo "$RUN" | jq -r .id)
VERSION=$(echo "$RUN" | jq -r .dataset_version)

# 4. Fetch the cases at that version. --json walks every page, so this is the
#    dataset's export as well as the harness's input.
tracepad datasets show support-golden --version "$VERSION" --json > cases.json

# 5. Run your function per case, stamping tracepad.run_id and
#    tracepad.item_id on the trace, and post the scores as always.

# 6. Close the run, then read it back.
tracepad runs finish "$RUN_ID"
tracepad runs show "$RUN_ID"
tracepad runs show "$RUN_ID" --items          # the cases and what was answered

# 7. The question this was all for.
tracepad runs compare "$BASELINE" "$RUN_ID"
```

`runs compare` prints the header, one line per score name with the delta and
how many cases moved, and then the cases whose verdict is not `same`; `--all`
lists every case. A run that failed closes with its reason:
`tracepad runs finish "$RUN_ID" --failed "judge timed out"`.

## The same loop from the web interface

The screens are the third client of these endpoints and add nothing to them
([ui.md](ui.md), *Evals*). What the browser is good for is the half of the
loop a person does by hand:

1. **Declare the score names.** *Evals → Score configs → New score config*:
   the type, which direction is better, the bounds or the categories. It
   `PUT`s the whole config, so editing one later is the same form.
2. **Build the set.** *Datasets → New dataset*, then *New item* for a case
   typed by hand — or, far more often, **open a trace where the model got it
   right (or nearly right), pick the observation, and press *Add to
   dataset***. The editor opens with that observation's input as the case and
   its output as the expected answer, both whole, with the trace and
   observation recorded as the source. Correct the expected answer and save.
   The dataset's version ticks once per save that changed something, and
   *unchanged* is what a save that changed nothing says.
   For a set that already exists as a file, `datasets push` is still the
   import — the browser writes one case at a time.
3. **Run it.** From the harness, exactly as above: the interface opens no run
   and closes none. The run page shows the two attributes to stamp while
   nothing has arrived, and follows the run as its traces land.
4. **Read it back.** The run page for one pass, *Compare with…* or two ticked
   checkboxes for two of them.

Deleting is where the browser is deliberately not quicker than `curl`:
deleting a dataset shows the same dry run and asks for the same echo, and
deleting a run says what happens to its traces before it happens.

## Responses

| Status | Meaning |
|---|---|
| `200` | The read succeeded, the envelope or config was written, the run was closed, or a known run id was returned unchanged. |
| `201` | Items written (or found unchanged), or a run created. On disk, not merely queued. |
| `400` | Validation: a missing `input`, a bad id, a version above the current, a run pinned above the current, an unknown field or query parameter, a wrong `confirm`. The message says which. |
| `401` | Unknown credentials. |
| `404` | No such dataset, item, or run in this project; an item already archived at the current version. |
| `409` | A run closed twice; a run id that already exists in another dataset. |
| `413` | The body is over `TRACEPAD_MAX_BODY_BYTES`. |
| `429` | The write queue is saturated; retry after the `Retry-After` delay. |
