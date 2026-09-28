# Administering a Tracepad server

Everything here changes what a project keeps or who can reach it. Read first,
change only what the human asked for, and put every destructive act through
the dry-run rule of SKILL.md: run it without `--yes`, show the preview, stop,
and add `--yes` only on the human's word about that preview.

## Who may do what

- A **project key** (`tp-sk-…`) reaches one project, and does there what its
  scopes allow: `ingest`, `read`, `write`, any combination. Reading the state
  below takes any key (`system` needs `read`); changing retention, erasing a
  user and deleting traces take a key with `write`. Not the keys: no project
  key lists, mints or revokes keys, whatever its scopes.
- The **admin token** (`TRACEPAD_ADMIN_TOKEN` on the server) reaches every
  project, and is the only credential for creating, renaming or deleting a
  project and for managing accounts. It rides where a key does:
  `TRACEPAD_API_KEY="$TRACEPAD_ADMIN_TOKEN" tracepad projects ls`. Do not ask
  for it unless the task needs it.
- `tracepad help` marks the commands that need the admin token.

## Reading the state

```sh
tracepad projects show
tracepad retention show
tracepad system
```

`projects show` with a project key is that key's project: its id, name and
retention windows, and a last line naming the key and its scopes. `retention show` is the three windows — traces, raw
bodies, statistics — in days, or forever.

## Keys

**An agent does not mint or revoke keys.** Issuing a credential is a person's
act: the `keys` commands answer a project key `403`, and they take the admin
token, which you do not ask for to get round that. When a task needs a new key
or a key revoked — a rotation, a leaked key, a new application — tell the human
to do it in the web interface, under Settings → Project → API keys, where the
listing also says what each key may do, who minted it and when it was last
used. Say which scopes the key should hold: `ingest` alone for a production
application, `read` alone for an agent or an MCP client, all three for an eval
harness or an operator's script. If they choose to run it themselves from a
terminal:

```sh
TRACEPAD_API_KEY="$TRACEPAD_ADMIN_TOKEN" tracepad keys create --project <project-id> --scope ingest --name "checkout api"
TRACEPAD_API_KEY="$TRACEPAD_ADMIN_TOKEN" tracepad keys rm <public-key> --project <project-id>
```

`--scope` is required. A key's scopes never change: a key that needs more, or
less, is a new key minted beside it and the old one revoked. Rotation is mint,
move the applications, then revoke — revoking first stops whatever still sends
with the old key; revoking the last key that carries `ingest` stops ingest,
and asks for the project's name first. `keys create` prints the new secret
**once**; it is theirs, not something to write into a tracked file, a log or a
commit message.

## Retention

```sh
tracepad retention set --days 30
```

Shortening a window deletes what falls outside it on the next sweep, so the
command is a dry run first: the preview counts the traces, observations,
scores and raw bodies that would go. Lengthening deletes nothing. `--raw-days`
and `--stats-days` move the other two windows; `tracepad help` has the rest.

## Erasing one user's data

```sh
tracepad users show <user-id>
tracepad users rm-data <user-id>
```

Everything stored about one end user — their traces with everything attached,
and their rows in the statistics. It is a request someone is entitled to make;
it is also irreversible. Look at the user first, then the dry run, then stop.
Confirmed, the server runs it as a task and the command waits for its end,
printing progress; `tracepad users erasures` lists what ran and what is still
running.

## Deleting traces

One trace by id, or every trace a listing filter matches before a bound. What
goes: the traces, their observations, payloads, scores, search entries and
queue items, with the statistics corrected. What stays: the raw OTLP bodies
they arrived in, which expire on the raw retention window. A trace an eval run
holds is deleted like any other, and the preview names the run.

The sequence, for "delete the traces of environment `test` from today":

1. **Pin the window.** "Today" is a date the human means; the data is in
   UTC. Work out the start as an RFC 3339 instant, and take the end as a fixed
   instant too — now, written out:

```sh
date -u +%Y-%m-%dT%H:%M:%SZ
```

2. **Look at the set** with the listing's own filters — `--until` here is the
   same bound `--to` will be:

```sh
tracepad traces ls --env test --since <start> --until <end> --total --limit 5
```

3. **The dry run.** The same filters, `--to` for the bound, no `--yes`:

```sh
tracepad traces rm --env test --since <start> --to <end>
```

   It prints what would go — `matched`, then the counts per kind — and exits
   `1` saying *it was not done*. That is the end of this step.

4. **Stop and report**: the filters, the bound, how many traces match, what
   else goes with them, that raw bodies stay. Give the confirming command —
   the same line with `--yes` — for the human to approve. Do not run it.

5. **Only on the human's go-ahead**, run exactly that line with `--yes`. The
   CLI deletes in rounds and loops until nothing matches; an interrupted run
   has lost nothing but its tally, and the same command continues it.

One trace is the same with its id: `tracepad traces rm <trace-id>` previews,
and the confirming line adds `--yes`. Spans that are still arriving for a
deleted trace create it again, so delete after the application has finished
exporting.

## Projects and accounts

Creating, renaming, deleting and restoring a project, and every `accounts`
command, need the admin token and are rarely an agent's job. A deleted
project is restorable for seven days (`tracepad projects restore`); nothing
else in this file is. When the human asks for one of these, read the command's
help, show them the preview it prints, and let them confirm.

## From application code

The Python, Node and Go packages can delete traces too, with the same
preview-then-confirm shape; the same rule holds for code you write: the code
previews and a person confirms. Do not write a job that confirms its own
deletions unless the human asked for exactly that.
