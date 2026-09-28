# Scores, datasets, runs and review queues

The pieces, and how they hang together:

- A **score** is one judgement on a trace, an observation or a session — a
  number, a boolean, a category or a text.
- A **score config** declares what a score name means: its type, its range or
  categories, and its direction (whether higher is better). Every write under
  that name is checked against it.
- A **dataset** is a versioned set of cases (`input`, `expected_output`).
- A **run** is one pass of the application over a dataset version. Its traces
  carry `tracepad.run_id` and `tracepad.item_id`, and the scores on those
  traces are its results.
- A **queue** is a list of traces waiting for a person's scores.

Tracepad stores and compares; it executes nothing. The harness that runs the
cases is the application's code, through the SDK's eval helpers.

## Reading what is there

```sh
tracepad score-configs ls
tracepad datasets ls
tracepad datasets show <dataset> --limit 5
tracepad runs ls <dataset>
tracepad runs show <run-id>
```

`runs show` summarises a run: coverage (how many cases have a trace), cost,
latency, the mean of each score, the models and prompts it used. A run still
`open` is still being written; its numbers will move.

## Did the change make it better

Compare two runs of the same dataset — the older one first:

```sh
tracepad runs compare <run-a> <run-b>
tracepad runs compare <run-a> <run-b> --all
```

Read it top down:

1. The header: both runs, the dataset version each pinned, and the metadata
   they differ in (usually the prompt or the model). Two runs over different
   versions compare different cases; say so.
2. Traces, failures, cost and latency, side by side.
3. Per score name: both means, the delta, and how many cases moved. A name
   says *improved* or *regressed* only when its config gives a direction;
   otherwise its cases are just *changed*.
4. The cases that moved, one row per case and score. `--all` adds the ones
   that did not.

Every number here is the server's. Report them; do not recompute means from
the rows, and do not call a delta an improvement when the config has no
direction.

Then read what the application actually answered for a case that regressed:

```sh
tracepad runs show <run-b> --items
tracepad traces show <trace-id> --full
```

## Trends over time

```sh
tracepad scores trend --name <score> --since 168h
tracepad scores trend --name <score> --group-by release
tracepad scores trend --name <score> --group-by model
tracepad scores ls --trace <trace-id>
```

`trend` is the mean, the rate of true or the share of each category per bucket.
Grouped by model it counts only the scores on an observation. A score on a
session, and a `text` score, are never on a timeline.

## Writing

Most writes here belong in the application's harness, not in a one-off
command: an eval is worth something because it runs the same way again.
When the human asks you to set something up, prefer writing the harness code
with the SDK ([Python](https://github.com/tracepad/tracepad/blob/main/docs/sdk-python.md#evals),
[Node](https://github.com/tracepad/tracepad/blob/main/docs/sdk-js.md#evals),
[Go](https://github.com/tracepad/tracepad/blob/main/docs/sdk-go.md#evals)).

From the command line, the loop is:

```sh
tracepad score-configs push <name> --file config.json
tracepad datasets push <dataset> --file cases.jsonl
tracepad runs create <dataset> --name "<what changed>" --json
tracepad runs finish <run-id>
```

- The bodies of `config.json` and each line of `cases.jsonl` are the request
  bodies of `PUT /api/v1/score-configs/{name}` and
  `POST /api/v1/datasets/{name}/items`; their fields are in
  `GET /api/v1/openapi.json`. Unknown fields are refused by name.
- `datasets push` is one batch and one version tick, and an unchanged file
  changes nothing. Give each case an `id` derived from its natural key, so a
  second push edits instead of duplicating. A request takes at most 10,000
  items (or scores); `push` sends a longer file as several writes, one tick
  each.
- `runs create --json` answers with the run's id and the dataset version it
  pinned. Between `create` and `finish`, the application runs each case with
  the run id and the case id stamped on its trace, and posts its scores.
- `runs finish <run-id> --failed "<reason>"` closes a run that did not
  complete, so nobody reads it as a result.

## Review queues

```sh
tracepad queues ls
tracepad queues items <queue> --status pending --limit 20
```

A queue names the score configs a reviewer must set on each item. Working a
queue — taking the next item, posting its scores, completing it — is a
person's job or a script's with somebody answerable for it. Do it only when
asked: `tracepad queues next <queue> --annotator <who>` claims an item for ten
minutes, and `tracepad queues complete` is refused until every score the queue
names is on the item. A verdict lives in the scores; completing an item only
records that it was reviewed.

## Deleting

`datasets rm` and `queues rm` are dry runs until confirmed — the rule in
SKILL.md applies. `runs rm`, `score-configs rm` and `scores rm` have no dry
run at all: each removes one row the moment it runs, so ask before running
them, naming what will go.
