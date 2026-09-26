# Spec 044 — Erasure completeness and data at rest

**Status:** 🚧 IN PROGRESS
**Sprint:** September 2026

> User-data erasure (spec 005 #7) promises to take "everything the queryable
> stores hold about one user", and the docs tell a controller that after the
> call no read endpoint can return that user's traces. Both stopped being true
> when spec 019 gave the raw archive a reader: the user's prompts and outputs
> leave, byte for byte, through `GET /api/v1/raw/{id}` and `tracepad export
> --otlp` until the raw window runs out — by default, never. A score given to
> the user's session and a dataset item cut from their trace outlive the
> erasure too, and so does everything a deleted row leaves in the file: the
> freed bytes, the search index's segments, the write-ahead log, and a full
> copy of the database the server writes before every upgrade and never
> removes — all of it created readable by any local account the directory
> lets through, and the documented container setups let everyone through.
> This spec makes an erasure
> reach every copy the store itself holds, makes "deleted" mean "overwritten"
> within the sweep interval, puts the files on disk behind their owner, and
> has the docs say exactly where the store's reach ends.

---

## How this change is filed

**A new spec, plus one dated pointer Decision in each shipped spec whose
contract it changes** (the rows are under
[Amendments to shipped specs](#amendments-to-shipped-specs)), rather than
Decisions in spec 005 and spec 019 alone.

- The change touches the contracts of eleven shipped specs — 001, 002, 003,
  005, 011, 014, 019, 020, 028, 035 and 041. Filed as Decisions scattered over
  their logs, "what does an erasure take" would be answerable only by reading
  all of them; it already takes five logs today (005 #7, 014 #14, 023 #10 and
  #19, 035 #3, 041 #12).
- The byte-scan test below is one contract across all of it — a marker must
  not survive an erasure anywhere in the file — and it needs one spec that
  owns it, one Testing section, and one status flip.
- The pointer rows keep every amended spec's log true on its own, which is
  what the spec-first rule asks of a change to a shipped spec. Each lands in
  the PR that changes the behaviour it describes, not before: a shipped
  spec's log says what the code does.

The two migrations below are written `00NN`; each takes the next free number
when the PR that adds it lands.

---

## Overview

Deliverables, in three PRs (the last one flips the status):

1. **The docs say what the store does today** — the data-subject section of
   `docs/retention.md`, `docs/admin.md`, `docs/export.md`, `docs/docker.md`
   (Decision 14, against current behaviour). Docs only, with this spec.
2. **Data at rest** — `secure_delete` on (#10); the compaction after explicit
   deletions (#11) with migration `00NN_compaction`; pre-migration backups at
   0600 and on a lifecycle (#12); file modes (#13) and the image's `/data`;
   the raw endpoints moved to `editor` (#6).
3. **Erasure reaches every copy** — the raw scrub (#2–#5, #15) with the single
   arrival stamp and migration `00NN_erasure`; session-only scores in erasure
   and retention (#7, #8); dataset items (#9); the API, CLI and interface
   fields; the final docs.

## What `main` does today

Verified at `4a27a74`:

- The raw endpoints are `member` routes (`internal/server/routes.go:87-88`);
  the body endpoint serves any batch the project holds, with its media put
  back (`internal/server/raw.go:145-185`).
- Erasure deletes parsed rows only and leaves raw batches by design
  (`internal/store/admin.go:619-623`, `:668-761`;
  `internal/server/admin.go:661-666`), and its dry run says so in its note
  (`internal/server/admin.go:703-704`). A raw batch keeps its media bodies
  alive through `media_raw_refs` (schema 0021), so an erased trace's pictures
  stay while the batch does (`docs/media.md:156-159`).
- Every deletion path takes scores by `trace_id`
  (`internal/store/tracedelete.go:65-69`, `internal/store/sweep.go:554-558`);
  a score with only a `session_id` (spec 003 #4) is taken by neither erasure
  nor retention — only by a project's purge.
- `dataset_items.source_trace_id` names the trace an item was cut from and has
  no index (`internal/store/migrations/0010_datasets_runs.sql`); nothing
  reads it on erasure. Deleting an item archives it at a new version and
  keeps every earlier row (spec 014 #5).
- The DSN has no `secure_delete` (`internal/store/store.go:56`); nothing
  outside the tests runs `wal_checkpoint`; the sweep's incremental vacuum
  frees at most 2 000 pages a pass (`internal/store/sweep.go`,
  `vacuumPages`).
- The search index is FTS5 with `contentless_delete=1`
  (`internal/store/migrations/0006_search.sql:42-46`): a delete is a
  tombstone, and the deleted document's terms stay in the index's segments.
- A backup `<db>.pre-<migration>.bak` is written with `VACUUM INTO` before
  every run that applies a migration and never deleted
  (`internal/store/migrate.go:96`, `:227-248`).
- SQLite creates the database, `-wal`, `-shm` and the backup at 0644 under
  umask 022, so the directory is the only guard. `os.MkdirAll(dir, 0o700)`
  sets the mode only of a directory it creates (`internal/store/store.go:37`);
  one that already exists keeps its own — and the image makes `/data` with
  `mkdir -p` (`Dockerfile:74`), 0755, as does the bind-mount recipe
  (`docs/docker.md:134`).
- A raw batch's `received_at` and its traces' `ingested_at`/`updated_at` are
  two clock readings: the handler's (`internal/server/otlp.go:135`) and the
  writer's (`internal/store/ingest.go:78-80`), the second later by the
  writer-queue wait.

Probes quoted in the Decisions below were run with this repository's driver
(`modernc.org/sqlite` v1.57.0, SQLite 3.53) on scratch files; the PRs turn each
into a test.

---

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-26.** **What an erasure takes.** `DELETE /api/v1/projects/{id}/users/{user_id}/data` removes, for the traces whose `user_id` is the one named (the *erased traces*): everything it removes today — observations, payloads, the scores on those traces and their observations, annotation-queue items, search entries, media refs, the per-user rollup, the hours re-rolled; the **session-only scores** of every session an erased trace carried (#7); every version of the **dataset items** cut from an erased trace (#9); the erased traces' **spans inside the raw batches**, and the media bodies only those spans referenced (#2–#5). It then requests a **compaction** (#11), so the bytes are overwritten and not only unlinked. A person is found by the user id their traces carry and by ids that point at those traces — never by searching content | The docs promise "everything the queryable stores hold about one user" (`docs/retention.md:328-331`) and that "no read endpoint, CLI command or MCP tool can return that user's traces" (`:380-383`). On `main` the raw bodies stay readable through `GET /api/v1/raw/{id}` and `tracepad export --otlp` for the whole raw window — spec 005 #7 called raw an archive "not served by any read endpoint", and spec 019 gave it one without revisiting that position; a session verdict survives with its free-text comment; and a dataset item that is a verbatim copy of the user's input and output is never looked at. Content search is ruled out on purpose: an id link is a fact, a substring match is a guess that both misses and over-deletes. |
| 2 | **2026-09-26.** **Raw batches are rewritten without the erased spans, not deleted whole.** For every raw batch holding a span of an erased trace: the span is removed; a `ScopeSpans` or `ResourceSpans` left with no span goes with it; every other byte stays the client's, by the rule spec 041 #5 already applies to media — protobuf re-marshals only the `ResourceSpans` it changed and splices them into the source, JSON splices the removal into the source text. A batch left with no span is deleted. The batch's `media_raw_refs` are recomputed from the new body, and a media body no ref names any more is collected in the same transaction. The new column `raw_batches.scrubbed_at` records when; nothing records whose spans went (#15). A batch whose rewrite fails is deleted whole and counted as such | A batch is one exporter flush, and a batching span processor interleaves every user the process served in its window: deleting whole batches would take other people's spans out of the archive with the erased person's — silently, since the export reports only the archive's old edge (spec 019 #1), not holes in its middle. The rewrite keeps the archive whole for everyone else and costs the one thing it must: a rewritten batch is no longer what the client sent, and `scrubbed_at` says so on the listing, the body and the export. It is not a new kind of edit — the stored body is already the client's export re-encoded around the media ingest factored out (spec 041 #5, #16), through the same splice (`internal/mapping/exportbody.go:42-93`). The delete fallback exists because an erasure must never leave the spans behind for want of an encoder. |
| 3 | **2026-09-26.** **Which batches are read: the erased traces' arrival windows.** A trace's spans arrive in batches whose `received_at` lies within the trace's `[ingested_at, updated_at]`. To make that exact, the writer stamps `raw_batches.received_at` with the same reading it gives the batch's traces (`arrived` in `IngestBatch.apply`), instead of the handler's earlier one. The candidates are the project's batches inside the union of the erased traces' windows (`idx_raw_batches_received`); each is decoded and checked, and one holding no erased span is left untouched and unmarked. Three corrections for stamps written under an older rule, each dated by `schema_migrations.applied_at`: a trace that arrived before `00NN_erasure` has its lower bound widened by 60 s; before 0009, whose `updated_at` was backfilled from `ingested_at`, the window closes when 0009 was applied instead of at `updated_at`; before 0005, whose `ingested_at` was backfilled from the client's timestamp, the window opens at the project's oldest batch | No index leads from a trace to the batches that carried it, and building one would cost every ingest a row per trace per batch and a backfill that decodes the whole archive. The windows need none of it once there is one stamp: today the first batch of a trace can precede its `ingested_at` by the time the job waited for the writer, so a window read from the trace would miss it. The writer's reading is the one kept, not the handler's, because the rollup's dirty set is sound only when a stamp and its commit are the same moment (spec 025 #23, `internal/store/scores.go:87-98`); for the archive the difference is milliseconds, and the listing's order then follows the commit order. A window that is too wide costs decoding and never correctness; one that is too narrow loses spans — which is why each legacy stamp has the side it cannot vouch for opened, and why the margin is a generous sixty seconds: a submission waits behind at most 256 others, since a full queue answers 429 instead of growing (`internal/store/writer.go:172-190`). |
| 4 | **2026-09-26.** **Order: raw first, then the parsed rows, then a tail.** One request: (1) read the user's traces — id, `ingested_at`, `updated_at`; (2) scrub the batches of their windows, decoding on the read side and submitting one writer job per changed batch — the job carries the `scrubbed_at` it read and is refused when the row has changed since, and the handler re-reads and recomputes, up to three attempts; (3) the parsed chunks as today (spec 023 #19), which now also take session scores (#7) and dataset items (#9); (4) scrub the batches received since step 1 for every trace id step 3 deleted. The request still runs to completion, the contract spec 035 #14 kept for erasure | Once the parsed rows are gone nothing names the batches, so raw goes first: a request cut off in (2) or (3) is finished by repeating it, which finds the remaining traces and derives their windows again. The optimistic check keeps two erasures from resurrecting each other's spans by writing a body computed before the other's rewrite landed. Decoding outside the writer keeps each job a byte swap, so ingest waits milliseconds, as it does for a chunk today. A request cut off in (4) leaves only spans that arrived while it ran — the race spec 035 #10 already documents for every deletion. |
| 5 | **2026-09-26.** **What the scrub cannot attribute is counted and documented, not searched for.** (a) With `raw_retention_days` longer than `retention_days`, batches older than the trace window hold spans of traces the sweep already took, and nothing names their user; the dry run reports them as `raw.unattributable_batches`. (b) `ResourceSpans` blocks ingest could not decode are kept as they are. (c) Spans of traces deleted earlier through spec 035 are kept by its #3 and linked to nobody. The docs recommend a raw window no longer than the trace window for a deployment that answers erasure requests; `TRACEPAD_STORE_RAW=off` stays the answer for one that cannot accept (a)–(c) | Finding them would mean running the mapper over every batch the project holds on every request — the mapper, not a byte search, decides which attribute is the user id — which is the cost #3 exists to avoid, for a case the operator can close by configuration. The count in the preview turns a silent gap into a decision the operator makes before confirming. |
| 6 | **2026-09-26.** **`GET /api/v1/raw` and `GET /api/v1/raw/{id}` move from `member` to `editor`.** A project key passes both policies and is unaffected, as are owner and editor sessions; a viewer session gets `403` | The archive is the bulk way out of a project — every body whole, with the attributes the mapper skipped and, until a scrub reaches it, whatever an erasure has not — and taking a project's data out is an operator's act, like its retention windows and its keys, not a reviewer's (spec 028 #2: a viewer reads traces and annotates). Nothing a viewer does needs it: no screen reads the archive, and `tracepad export --otlp` authenticates with a key. With #2 the erasure reason is smaller; the bulk-export one stands, and the change costs no existing flow. |
| 7 | **2026-09-26.** **Erasure takes the session-only scores of the erased traces' sessions.** A score with `trace_id` NULL whose `session_id` is the session of an erased trace is deleted in the chunk that erases that trace, and counted as `session_scores` in the dry run and the answer. A session shared with another user's traces loses its session-only scores too. A score that carries both a trace and a session follows its trace, as today | Spec 003 #4 lets a score target a session alone, and every deletion path takes scores by trace id, so a verdict on a conversation — with its comment — outlives the erasure of the conversation. In a shared session the verdict is about a conversation the erased person was part of; erring toward deletion is how an erasure request reads, and the count in the preview shows it before it happens. |
| 8 | **2026-09-26.** **The sweep takes session-only scores.** A session-only score is deleted when its `created_at` — receive time, the server's clock — is older than the project's trace window **and** no trace of the project carries its session id; the retention dry run counts it under `session_scores`. A partial index serves the pass (data contract) | Spec 003 #4 promised that dangling scores "get cleaned up by retention alongside their targets", and the sweep takes scores by trace id only: a session-only score lives for ever, and once its traces are swept an erasure can no longer find it either. "No trace carries the session" is "alongside its target" for a session; `created_at` bounds the score whose session never arrived, on the clock retention counts by (spec 005 #1). A session a pinned run keeps alive keeps its verdicts by the second condition. |
| 9 | **2026-09-26.** **Erasure deletes the dataset items cut from an erased trace — every version.** An item any row of which names an erased trace in `source_trace_id` loses all its rows, its history included. The dataset's `version` advances by one for each chunk that removed items from it. A run of an older version resolves the item as absent, and its traces that answered it count under `unknown` (spec 014 #29). The dry run counts `dataset_items` and lists `affected_datasets: [{dataset, items}]`, the way it lists `affected_runs`. A partial index serves the lookup (data contract) | An item is a verbatim copy of an input and an output (spec 014 #4), and `source_trace_id` is the one fact the store has about where it came from. Reporting without deleting would leave the operator no tool: deleting an item archives it at a new version and keeps every earlier row (spec 014 #5), so the only purge the API offers is the whole dataset. Erasure already outranks the eval's convenience for runs (spec 014 #14), and an item is the same trade. A curator who anonymised a case keeps it by posting it without the source. The version tick tells a harness that caches by version that the set changed. |
| 10 | **2026-09-26.** **`secure_delete` is on for every connection.** The DSN gains `_pragma=secure_delete(ON)`; no setting turns it off | SQLite unlinks a deleted row and leaves its bytes in place until something overwrites them. Probed: without the pragma a deleted, checkpointed row stays readable in the page it lived on; `FAST` zeroes cells inside live pages but leaves freed overflow pages — where raw bodies and large payloads live — intact on the freelist; `ON` zeroes both. Its cost in the same probe (a laptop, warm page cache): deleting 80 MB of 16 KB blobs and 100 000 small rows in 1 000-row chunks took 0.38 s without, 0.53 s with `FAST`, 1.57 s with `ON` — roughly 15 ms per freed megabyte, written to the WAL and checkpointed; inserts pay nothing. At the reference rate that is invisible; at the envelope's million spans a day it is some tens of seconds of writer time a day, spread over the sweep's chunks — measured on the seeded corpus in the PR. No knob, for spec 005 #9's reason: a guarantee that depends on configuration is not one. |
| 11 | **2026-09-26.** **A compaction follows every explicit deletion.** Erasure, trace deletion (spec 035) and a project's purge record a compaction request in a one-row table; the next sweeper pass runs it: FTS5 `merge` with a negative page budget as bounded writer jobs until the index is one segment; the incremental vacuum until the freelist is empty, in the pass's 2 000-page jobs; then `PRAGMA wal_checkpoint(TRUNCATE)` as a **standalone writer step** — between commit windows, outside any transaction, with a busy wait of at most 250 ms, a `busy` answer meaning "again next pass". Requests coalesce. `GET /api/v1/system` gains `compaction: {requested_at, completed_at}`; the erasure answer carries `compaction: {requested_at, expected_by}`. Migration `00NN_compaction` records a request, so the first pass after the upgrade compacts what earlier deletions left. The retention sweep does not request one | Two residues survive #10. The search index's tombstones leave a deleted document's terms and positions in the live segments — probed: the token of a deleted document was still in `search_fts_data` after the delete, with FTS5's own `secure-delete` option set as well, and was gone after a `merge` or `optimize` — run with `secure_delete` on, followed by a checkpoint; without the pragma the rewritten segment's old pages kept it. And the WAL keeps pre-deletion page images until a checkpoint copies the zeroed pages back and truncates it. A merge rewrites the whole index — tens of milliseconds for a 3 MB index in the probe, proportionally more on a large store — so it runs in the pass, in steps, the way every background cost does (spec 005 #14); the interval is the promise's resolution, and `/system` shows when it was kept. Draining the freelist also clears what deletions before #10 left in free pages. The checkpoint goes through the writer because a truncating checkpoint blocks writers, and a second connection doing it is the contention the one-writer rule exists to prevent (spec 003 #24). The sweep's own deletions are zeroed by #10; compacting the index after every hourly sweep would rewrite it every hour, so the terms of swept traces leave with the index's ordinary merges, and the docs say so. |
| 12 | **2026-09-26.** **Pre-migration backups: mode 0600, one at a time, seven days.** The server creates `<db>.pre-<migration>.bak` empty with mode 0600 and lets `VACUUM INTO` write into it (SQLite accepts an empty existing file; probed — the mode is kept). Once the migrations it guards have committed, every older `<db>.pre-*.bak` is deleted; the sweeper deletes the remaining one seven days after it was written (its modification time). Each deletion is logged by file name. While one exists, the erasure's dry run and answer carry `pre_migration_backup: {created_at, removed_at}` | Today a backup is written before every run that applies a migration and is never deleted, at 0644, and the only mention in the docs is one clause (`docs/retention.md:367`). Each is a full copy of the database as it was — every user erased since, every trace swept since. Spec 001 #6's purpose is a file-swap rollback of the upgrade just made, which the newest file serves, for as long as a rollback is plausible; a week is spec 005 #9's safety-net size, fixed for its reason. Deleting the older ones only after the migrations commit keeps every file for exactly the case it exists for: a migration that fails. Operators who keep backups take their own (`docs/docker.md`, "Upgrading, and backing up first"). The answer's field is how an erasure names the one copy it does not rewrite and the date it goes. |
| 13 | **2026-09-26.** **The data directory is 0700 and the database files 0600, on every start.** `Open` keeps `MkdirAll(dir, 0700)`, tightens an existing directory that grants group or other access to 0700, creates a missing database file empty with mode 0600 before SQLite opens it (SQLite gives `-wal` and `-shm` the database file's mode; probed), and tightens an existing database, `-wal`, `-shm` and backup files to 0600. A `chmod` that fails is a WARN naming the path and its mode, never fatal. The image creates `/data` with mode 0700 | Probed under umask 022: the database, `-wal`, `-shm` and the `VACUUM INTO` backup are all created 0644, and both container setups start from a 0755 directory. The file holds every prompt and completion, the accounts' password hashes and the media-upload signing key (`server_keys`, schema 0021), so any local account the directory lets through can read all three — and the files themselves, not the directory someone else created, are what the server can vouch for. On every start and not only on create, because every existing install is 0644 today; a backup agent reading through group permission is the exception, and the docs give the recipe that does not need it. Never fatal, because a filesystem without Unix modes must still start. |
| 14 | **2026-09-26.** **The docs say what erasure guarantees and what it cannot.** The data-subject section of `docs/retention.md` is rewritten in three parts — what goes when the call returns, what is overwritten by the next pass, what no program can reach — and "complete" is not used without its conditions. The first PR corrects the text against current behaviour; the last one states the contract of this spec | The current text says "no read endpoint … can return that user's traces" and that raw is "not served by any read endpoint" (`docs/retention.md:380-398`), false since spec 019; and "erasure is complete the moment it returns" under `TRACEPAD_STORE_RAW=off` (`:405-408`), false for the backups and for the bytes in free space. A tool that is exact about its edges is one a controller can build a process around; one that overstates them is a liability discovered later. |
| 15 | **2026-09-26.** **An erasure records nothing about the person.** No list of erased ids, no user id in `scrubbed_at` or in the compaction request; the erasure's log lines carry counts, not the id | A record of who asked to be forgotten is a new record about them, kept by the act of forgetting. The marker and the request say *that* something went, which is all the operator and the export need. |

---

## API contract

**`DELETE /api/v1/projects/{id}/users/{user_id}/data`** (role unchanged:
editor).

Dry run (no `confirm`):

```json
{
  "dry_run": true,
  "would_delete": {
    "traces": 12, "observations": 240, "scores": 30, "session_scores": 2,
    "annotation_items": 1, "dataset_items": 3, "media": 4, "media_bytes": 812003
  },
  "oldest": "2026-06-01T09:14:02Z",
  "affected_runs": [{"id": "…", "dataset": "support-golden", "traces": 2}],
  "affected_datasets": [{"dataset": "support-golden", "items": 3}],
  "raw": {"batches_to_scan": 41, "unattributable_batches": 0},
  "pre_migration_backup": {"created_at": "2026-09-24T08:00:00Z", "removed_at": "2026-10-01T08:00:00Z"},
  "confirm": "user-4711",
  "note": "the user's spans are removed from the raw batches that hold them; …"
}
```

- `would_delete` stays numbers only: the confirmation card renders every key
  it carries (`ui/src/lib/components/ConfirmCard.svelte:97`).
- `media` counts the bodies only the erased traces point at; bodies the raw
  scrub frees are known after the rewrite and appear in the answer.
- `raw.batches_to_scan` is the candidate count of #3 (a count, not a
  decode); `raw.unattributable_batches` is #5 (a). Both `0` with raw
  storage off.
- `pre_migration_backup` is absent when no backup exists.
- `note` states, in words, the raw lines and the backup line.

Confirmed (`?confirm=<user id>`):

```json
{
  "dry_run": false,
  "deleted": {
    "traces": 12, "observations": 240, "scores": 30, "session_scores": 2,
    "payloads": 480, "annotation_items": 1, "dataset_items": 3,
    "media": 5, "media_bytes": 901442,
    "raw_spans": 252, "raw_batches_rewritten": 38, "raw_batches_deleted": 3
  },
  "user_id": "user-4711",
  "compaction": {"requested_at": "2026-09-26T10:02:11Z", "expected_by": "2026-09-26T11:00:00Z"},
  "pre_migration_backup": {"created_at": "…", "removed_at": "…"}
}
```

`raw_batches_deleted` counts both batches left empty and the fallback of #2.
`expected_by` is the sweeper's next pass.

**`DELETE /api/v1/traces/{id}`, `DELETE /api/v1/traces`** — the confirmed
answers gain `compaction: {requested_at, expected_by}` (#11). Their raw note
is unchanged: trace deletion does not scrub raw (Out of scope).

**`PATCH /api/v1/projects/{id}`** — the retention dry run's `would_delete`
gains `session_scores` (#8).

**`GET /api/v1/raw`, `GET /api/v1/raw/{id}`** — policy `editor` (#6). Listing
rows gain `scrubbed_at` (RFC 3339 or `null`); the body answers
`X-Tracepad-Scrubbed-At` when set. Nothing else changes.

**`GET /api/v1/system`** — gains the deployment-wide block
`compaction: {requested_at, completed_at}`, each RFC 3339 or `null`.

`openapi.json` and `schema.d.ts` carry every field above in the PR that adds
it; the parity tests cover them, and the six-caller matrix test asserts the
raw rows at `editor`.

## CLI contract

- `tracepad users rm-data` prints, after the counts: the raw line (`removed
  252 spans from 41 raw batches, 3 deleted`), the compaction line (`bytes
  overwritten by the next sweep, expected by …`), and the backup line when
  present. The preview prints `unattributable_batches` when non-zero.
- `tracepad system` prints the `compaction` block.
- `tracepad export --otlp` counts rewritten batches in its summary
  (`"scrubbed": N`), and `--dir` writes `scrubbed_at` into `manifest.jsonl`
  with the rest of the listing row.

The usage parity test covers the output fields.

## Interface

The erasure dialog (Settings and the user page) renders the new counts
through the existing card; `affected_datasets` gets a line beside the runs
line, in the same shape. The raw routes' new policy changes nothing on
screen: no screen reads the archive. Live check in a browser, per the
Definition of Done.

## Store contract

- `mapping.ExportBody` gains removal of the spans of a set of trace ids,
  dropping emptied `ScopeSpans` and `ResourceSpans`; `Encode` keeps its rule —
  bytes it did not change are the source's — for both encodings; the JSON
  splice gains the removal of an array element.
- `RawScrub{ProjectID, BatchID, Expect, Body, Delete}` job: refuses (conflict)
  when the row is gone or its `scrubbed_at` differs from `Expect`; otherwise
  writes the zstd body and `scrubbed_at`, replaces the batch's
  `media_raw_refs` with the references the new body carries, collects
  unreferenced bodies — or, with `Delete`, drops the batch through the path
  `rawSweep` uses.
- `UserDataErase` chunks also delete the session-only scores of the chunk's
  sessions and every row of the items cut from the chunk's traces, and tick
  the affected datasets' versions; counts in `DeleteCounts`.
- `UserDataPreview` gains the session-score and item counts, the affected
  datasets, and the raw candidate and unattributable counts.
- The sweep gains a session-score job per project (#8); the retention preview
  counts it.
- A `compaction` step in the pass (#11) and a standalone writer step for the
  checkpoint — a second job kind the writer runs alone between commit windows.
- `Open` enforces the modes (#13); `backupBefore` pre-creates the file at
  0600, and the migration runner removes superseded backups after a
  successful run (#12); the sweeper removes the last one after seven days.

## Data contract

`00NN_compaction.sql` (PR 2):

```sql
CREATE TABLE compaction (
    id           INTEGER PRIMARY KEY CHECK (id = 1),
    requested_at INTEGER,   -- Unix ns; NULL when nothing is pending
    completed_at INTEGER    -- Unix ns of the last completed compaction
) STRICT;
-- The first pass after the upgrade compacts what earlier deletions left.
INSERT INTO compaction (id, requested_at)
VALUES (1, CAST(unixepoch('subsec') * 1000000000 AS INTEGER));
```

`00NN_erasure.sql` (PR 3):

```sql
ALTER TABLE raw_batches ADD COLUMN scrubbed_at INTEGER;  -- Unix ns, NULL = as received
-- The retention pass over session-only scores (#8).
CREATE INDEX idx_scores_session_only ON scores(project_id, created_at)
    WHERE trace_id IS NULL;
-- The erasure's lookup of items cut from a trace (#9).
CREATE INDEX idx_dataset_items_source ON dataset_items(project_id, source_trace_id)
    WHERE source_trace_id IS NOT NULL;
```

The erasure's session-score delete rides the existing
`idx_scores_session`; the candidate batches ride `idx_raw_batches_received`.

## Files on disk

| File | Mode | Lifetime |
|---|---|---|
| data directory | 0700, tightened at start | — |
| `tracepad.db`, `-wal`, `-shm` | 0600, created so; tightened at start | — |
| `tracepad.db.pre-<migration>.bak` | 0600, created so; tightened at start | Superseded ones deleted once the migrations of a run commit; the newest deleted seven days after it was written |

## Config

None added. `TRACEPAD_SWEEP_INTERVAL` becomes the compaction's clock as well.

---

## Amendments to shipped specs

One row appended to each log in the PR that changes the behaviour, as the
next free number of that log; the wording below is the text it carries.

| Spec | PR | Text |
|---|---|---|
| 001 | 2 | **2026-09-26** — Pre-migration backups are created at 0600; superseded ones are deleted once the migrations of a run commit, and the newest seven days after it was written (spec 044 #12). #6's "trivially reversible by file swap" holds for that week. The data directory is tightened to 0700 and the database files to 0600 at every start (spec 044 #13), amending the edge case "Data dir missing → created with 0700". |
| 002 | 3 | **2026-09-26** — A raw batch's `received_at` is the writer's reading, the one its traces' `ingested_at` and `updated_at` get (spec 044 #3); before, it was the handler's, earlier by the queue wait. |
| 003 | 3 | **2026-09-26** — #4's "cleaned up by retention alongside their targets" is implemented for session targets: a session-only score goes when it is older than the trace window and no trace carries its session, and with the erasure of any trace of its session (spec 044 #7, #8). |
| 005 | 3 | **2026-09-26** — Erasure takes the user's spans out of the raw batches, the session-only scores of their sessions and the dataset items cut from their traces (spec 044 #1–#9), superseding #7's "does not touch raw batches". Testing 8 becomes "the user's spans gone from raw, other users' spans intact". |
| 005 | 2 | **2026-09-26** — `secure_delete` is on, and explicit deletions request a compaction the next pass runs (spec 044 #10, #11), extending #5. |
| 011 | 2 | **2026-09-26** — A delete stops matching at once (#7) and leaves the index's segments at the next compaction after an explicit deletion (spec 044 #11). |
| 014 | 3 | **2026-09-26** — Erasure deletes every row of an item cut from an erased trace and ticks the dataset's version (spec 044 #9) — the one exception to #5's append-only history, beside #14's for runs. |
| 019 | 2 | **2026-09-26** — The raw endpoints are `editor` routes (spec 044 #6); #9's "a key that reads traces reads the batches" still holds for keys. |
| 019 | 3 | **2026-09-26** — A batch an erasure rewrote is the batch as received minus the erased spans, marked by `scrubbed_at` on the listing, `X-Tracepad-Scrubbed-At` on the body and `scrubbed` in the export summary (spec 044 #2), qualifying #1 and #8's "as received". |
| 020 | 2 | **2026-09-26** — The image creates `/data` with mode 0700 (spec 044 #13). |
| 028 | 2 | **2026-09-26** — In #3's matrix, `GET /api/v1/raw` and `/api/v1/raw/{id}` move from `member` to `editor` (spec 044 #6). |
| 035 | 2 | **2026-09-26** — A confirmed deletion requests a compaction (spec 044 #11); raw bodies stay untouched as #3 says. |
| 041 | 3 | **2026-09-26** — A raw batch an erasure rewrites keeps refs only to the media its new body references, and a body left unreferenced is collected (spec 044 #2). |

---

## Docs

- `docs/retention.md` — the data-subject section (draft below); "Deleting a
  user's data" lists what goes; the sweeper's list gains the compaction and
  the session-only scores; "What outlives what" and the Configuration row of
  `TRACEPAD_STORE_RAW` lose "the archive caveats above"; line 367's backup
  clause points at the lifecycle.
- `docs/admin.md` — "Erasing a user's data": the new counts,
  `affected_datasets`, the raw lines, compaction, backup; no "raw bodies are
  deliberately not touched".
- `docs/export.md` — the endpoints are editor reads; rewritten batches and
  `scrubbed_at`; the export summary's `scrubbed`.
- `docs/api.md` — the raw section (role, field, header), the erasure shapes,
  the `compaction` block of `/system`.
- `docs/accounts.md` — a viewer reads everything in the project but the raw
  archive; the `editor` row of the matrix.
- `docs/datasets.md` — erasure deletes items cut from the user's traces, all
  versions; keep `source_trace_id` for erasure to reach an item, drop it from
  an anonymised one.
- `docs/scores.md` — session-only scores: retention and erasure.
- `docs/media.md` — the paragraph on pictures kept by raw batches
  (`:156-159`).
- `docs/docker.md` — `sudo install -d -m 0700 -o 65532 -g 65532
  ./tracepad-data` for the bind mount; the backup recipe under `umask 077`
  (`busybox sh -c 'umask 077 && tar czf …'`), noting the archive holds the
  pre-migration backup when one exists; the modes the server keeps.
- `docs/cli.md`, `README.md` (line 211: "everywhere it is queryable" becomes
  "everywhere the store holds it"), `openapi.json`, `schema.d.ts`,
  `AGENTS.md` status block.

Draft for `docs/retention.md`, "What this means for a data-subject request"
(the contract after PR 3; PR 1 writes the same three parts against current
behaviour):

> You are the controller; tracepad is the tool. This is what an erasure
> does, and where its reach ends.
>
> **What goes when the call returns.** Every row the store keeps about the
> traces filed under the user id: the traces, their observations, payloads,
> scores, search entries and annotation-queue items; the scores given to the
> sessions those traces belonged to; the dataset items cut from those traces,
> with their history; the per-user statistics; and the user's spans inside
> the raw OTLP bodies — each batch that held them is rewritten without them,
> or deleted when nothing else was in it. From then on no endpoint, CLI
> command or MCP tool returns them, the export does not replay them, and no
> search finds them.
>
> **What is overwritten by the next sweep.** Deleting a row unlinks it; its
> bytes stay in the file until something overwrites them. Tracepad zeroes the
> freed space as rows go, and the next sweeper pass — within
> `TRACEPAD_SWEEP_INTERVAL`, an hour by default — rewrites the search index
> and truncates the write-ahead log. The erasure's answer says when that pass
> is due; `GET /api/v1/system` says when it last completed.
>
> **What erasure cannot reach.**
>
> 1. Copies outside the database file: your backups, volume and filesystem
>    snapshots, replicas, an export taken before the erasure, the logs of your
>    application or proxy. Expire those with your own process.
> 2. The backup the server writes before an upgrade. It is kept seven days
>    and then deleted; while one exists, the erasure's answer names the date.
> 3. Raw bodies older than the trace window. With `raw_retention_days` longer
>    than `retention_days`, the traces that said whose spans a batch holds are
>    gone, and nothing can attribute them; the dry run counts those batches.
>    If you answer erasure requests, keep the raw window no longer than the
>    trace window, or run with `TRACEPAD_STORE_RAW=off`.
> 4. Anything not linked by id: a name typed into another user's prompt, a
>    dataset item copied without its `source_trace_id`, a score about the user
>    attached to someone else's trace, a trace deleted by hand earlier, spans
>    the ingest could not decode.
> 5. What the disk keeps below the file. A program can overwrite its own
>    file, not the blocks a filesystem or an SSD has already released;
>    full-disk or volume encryption is the layer for that.
> 6. Rows deleted before this version zeroed freed space may leave traces
>    inside pages still in use. A full `VACUUM` of the stopped database
>    rewrites every page.

## Testing

**The byte scan** (server level, over a real file in `t.TempDir()`):

1. Ingest, raw storage on, protobuf and JSON, in batches that mix two users:
   user *A* (a random `user_id`, a lowercase random marker *M* in a trace
   name, in an input payload under the 128-byte compression threshold, in an
   output payload above it, in a media body, in a session-only score's
   comment, and in a dataset item cut from *A*'s trace) and user *B* (marker
   *N*). Fixtures are sized so every zstd frame fits in one page.
2. Positive control: before the erasure the scan below finds *M*.
3. Erase *A*, confirmed; run one sweeper pass.
4. Assert, over `tracepad.db`, `-wal` and `-shm`: no occurrence of *M* or of
   *A*'s id in the raw bytes; none in any zstd frame carved from them (every
   offset of the frame magic `28 B5 2F FD`, decoded with a size-limited
   decoder, failures skipped); and *N* still readable through
   `GET /api/v1/raw/{id}` and the observations of *B*.

Red on `main` for five independent reasons; the PR's mutation table shows
each fix's own: without `secure_delete` (in-page cells), without the index
merge (the FTS segment), without the checkpoint (the WAL), without the raw
scrub (the live raw row, carved), without the score and item deletes.

**`secure_delete` alone** (store level, no compaction): a 16 KB row and a
small row carrying *M* are deleted and the WAL checkpointed through the
standalone step, with no vacuum; the file holds no *M*. Red with the pragma
off or at `FAST` (the freed overflow pages keep it) — the probe of #10 as a
test.

**Raw scrub** (`internal/mapping`, `internal/store`):

- Protobuf and JSON batches with spans of three traces of two users: after
  erasing one, the body decodes to exactly the other user's spans; the bytes
  of every untouched `ResourceSpans` equal the original's; emptied
  `ScopeSpans` and `ResourceSpans` are gone; unknown fields and a block that
  did not decode are kept.
- A batch holding only the erased user's spans is deleted; a forced encoder
  failure deletes the batch and counts it.
- Media: a body only an erased span referenced is collected; one a kept span
  references stays; `media_raw_refs` equal the new body's references.
- `scrubbed_at` on the listing, the header and the `--dir` manifest; the
  export summary's `scrubbed`.
- Two concurrent scrubs of one batch: the second is refused, recomputes, and
  the result holds neither user's erased spans.

**Windows** — with one clock reading, a batch received 1 ns before a trace's
`ingested_at` is not a candidate and one at `ingested_at` is; a legacy-stamped
trace (stamps set as before) still finds its first batch through the 60 s
margin; the 0005 and 0009 corrections open their sides.

**Order** — a test seam stops the request after step 2 and after step 3; the
repeat finishes and the final state equals an uninterrupted run; a batch
injected between steps 1 and 3 is scrubbed by step 4.

**Scores** — erasure takes session-only scores of the user's sessions,
including a session shared with another user, and leaves other sessions' and
the other user's trace scores; retention takes a session-only score past the
window whose session has no trace, keeps one whose session still has a
trace, and the retention dry run counts it.

**Dataset items** — erasure deletes every row of an item cut from an erased
trace, including an item whose later version dropped the source; ticks the
version once per chunk; leaves an item without a source; a run of an older
version counts the item's traces under `unknown`; the dry run lists
`affected_datasets`.

**Role** — the six-caller matrix: a viewer session `403` on both raw routes,
editor session and key `200`.

**Files** (unix build tag; the test sets umask 022) — a fresh `Open` leaves
the directory 0700 and the database, `-wal`, `-shm` and the backup 0600; a
0755 directory and 0644 files are tightened; a failing `chmod` (injected)
logs a WARN and the store opens.

**Backups** — a run with migrations writes the backup at 0600 and deletes
older ones only after committing; a failing migration deletes nothing; the
sweeper deletes the last backup when its time is past seven days (clock and
mtime injected); the erasure dry run names it while it exists.

**Compaction** — a request survives a restart; the next pass runs it and
stamps `completed_at`; requests coalesce; a checkpoint answered `busy` (a
reader holding a snapshot) leaves the request pending for the next pass; no
single step holds the writer longer than the bound the PR measures.

**Measurements in the PRs** — the sweep's chunk time on the seeded corpus with
and without `secure_delete`; a compaction's total time and longest step on a
seeded store of 1 GB; the erasure of a 20 000-trace user with raw on, and the
number of batches decoded.

## Edge cases

- `TRACEPAD_STORE_RAW=off`: no raw phase; the `raw` block of the dry run is
  zeros; everything else as above.
- A user whose traces were all swept or deleted earlier: the erasure finds
  nothing to attribute; session-only scores of their sessions are taken by
  the sweep (#8) once past the window, dataset items cut from those traces
  are not found (their source names a trace that no longer says whose it
  was).
- A trace whose user id arrived with a later span: the window is the
  trace's, not the attribute's, so the earlier batches are candidates too.
- The sweeper deletes a candidate batch between the read and the job: the job
  finds no row and does nothing.
- A batch received while the erasure runs, for a trace it is erasing: the
  tail step (#4) scrubs it; one that arrives after the request returns
  recreates the trace from what arrived (spec 035 #10).
- A checkpoint that keeps answering `busy` because an export streams for
  longer than a pass: `completed_at` does not move and `/system` shows the
  request pending; the next pass tries again.
- A migration that fails: the backup it wrote and every older one stay; the
  error names the newest, as today.
- A filesystem without Unix modes, or a directory the process does not own:
  WARN, the server starts.
- Two erasures of one user at once: the second finds fewer traces; the
  counts are each request's own, as today.
- A frozen hour (spec 013 #11) is not re-rolled, and `names_hourly` holds
  trace names: an erased trace's name stays counted there. Trace names are
  operation names by convention; the docs' "the aggregates carry no user id,
  no name and no text" (`docs/retention.md:108-112`) is corrected to say
  they carry trace names, so a deployment that puts personal data into them
  knows.

## Out of scope

- Scrubbing raw bodies on trace deletion (spec 035). The machinery is the
  same; the need is a later spec's to argue, and #3 of spec 035 stands.
- Searching raw bodies for a user id the parsed rows no longer carry (#5).
- Erasure by anything but the user id: an email inside text, a session id
  alone.
- An offline `VACUUM` command. The docs give the recipe for a stopped
  database.
- Encryption at rest, and anything below the file: filesystem journals,
  snapshots, SSD wear-levelling.
- Erasure across projects: one project per request, as today.
