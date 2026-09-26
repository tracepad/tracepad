# Spec 019 — The way out: `tracepad export --otlp`, the raw API and OTLP/JSON in

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> Every accepted export has been kept byte for byte since spec 002 (#9),
> and the README promises what that buys: the data can leave, whole, into
> any OTLP receiver. Nothing yet honours the promise — the raw rows have no
> reader but the sweeper. This spec adds the reader: a raw-batch listing in
> the API, a body endpoint that answers with what the client sent, and a
> CLI command that replays the archive into a receiver or onto disk, in
> order, resumably, and says what it could not cover. It also closes the
> other half of "standard OTLP": `POST /v1/traces` accepts the JSON
> encoding, stored as it arrived.

---

## Overview

Deliverable, in one PR (which flips the status):

- **`GET /api/v1/raw`** — the project's raw batches, oldest first, keyset
  by `(received_at, id)`, filtered by `since`/`until`; **`GET
  /api/v1/raw/{id}`** — one body, with the `Content-Type` it was received
  in (Decisions 2, 3). **`GET /api/v1/system`** gains a `raw` block: the
  window, the count, the bytes, and the traces older than the window
  (Decision 4).
- **`tracepad export --otlp`** — replays the batches into `--to <url>` or
  writes them under `--dir <path>`, in received order, with retry, and
  with a resume cursor on stop (Decisions 1, 5, 6).
- **OTLP/JSON ingest** — `POST /v1/traces` with `Content-Type:
  application/json`, decoded per the OTLP/JSON encoding, mapped by the
  same mapper, stored as received; schema 0012 adds
  `raw_batches.content_type` (Decisions 7, 8).
- `docs/export.md` (new), `docs/ingest.md` (JSON, the `raw` block),
  `docs/api.md` (the raw section), `docs/cli.md`, `docs/retention.md` (the
  window is now the export's reach), `openapi.json` + `schema.d.ts`,
  the CLI usage parity test, AGENTS.md.

Not here: synthesizing OTLP from parsed rows for traces whose raw body is
gone (a later spec), `tracepad remap` (the other reader of the archive;
its own spec), OTLP over gRPC, an import command (the receiver of a replay
is any OTLP endpoint, this server included).

---

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-04** — The export replays **raw bodies only** — the bytes of `raw_batches`, as received — and **reports what it did not cover**: the number of traces whose `start_time` precedes the oldest raw batch's `received_at`, and a refusal with a message when `TRACEPAD_STORE_RAW` is off and the table is empty. It never synthesizes OTLP from the parsed rows (owner decision 2026-09-04) | The promise is "the data as it arrived can leave"; byte-for-byte replay is what makes the receiver see exactly what this server saw — every dialect, every unmapped attribute, every quirk the mapper skipped. A synthesis from the parsed rows is a second, lossy exporter with an inverse mapper behind it, and mixing the two in one command would hand the receiver a corpus that means two different things by row. The uncovered count is the honest edge of the promise: a trace older than the raw window (`raw_retention_days`, `docs/retention.md`) is parsed rows only, and the operator must learn that from the export, not from the receiver's gap. `start_time` against `received_at` is a lower bound — a late export of an old trace lands after the batch line — and the number is named as such. |
| 2 | **2026-09-04** — The export runs **through the API**, not against the database: `GET /api/v1/raw` lists a project's batches and `GET /api/v1/raw/{id}` serves one body; the CLI is a client of both, authenticated by the project's keys like every command (owner decision 2026-09-04: through the API) | The CLI has never opened the database (`docs/cli.md`: one command, one server), and an offline mode would be a second reader of a SQLite file the server is writing, with a second answer to every question about locking and WAL. Through the API the export works from anywhere the keys work, for one project at a time — which is what a key is — and the same two endpoints serve any other client that wants the archive: a script, a backup job, a second Tracepad. |
| 3 | **2026-09-04** — `GET /api/v1/raw` is keyset-paged by `(received_at, id)` **oldest first** — the one listing in the API whose natural order is forward, because a replay must preserve arrival order — with `since`/`until` on `received_at` (RFC 3339, the trace listing's grammar), `limit` ≤ 500, cursors both ways (spec 003 #18), `count`. Rows: `id`, `received_at`, `dialect`, `content_type`, `content_encoding`, `size_bytes`. `GET /api/v1/raw/{id}` answers the **decoded** body — gzip was removed at ingest (`store.RawBatch`, `internal/store/ingest.go:40-48`) — under the stored `Content-Type`, **budget-exempt** like `/observations/{id}/io` (spec 004 #3) and `…/items` (spec 014 #19), with `X-Tracepad-Received-At` and `X-Tracepad-Dialect` headers | A batch is the unit of the archive and of the replay; listing them is listing what arrived, and the columns are the ones the row already has plus its size, which a script wants before it fetches. Forward order because a receiver that upserts by span id does not care, but a receiver that does not — a file, a stream — must see the spans in the order the world produced them, and the archive's `received_at` is the only order this server knows. The body endpoint is exempt from the budget for the reason the other two are: a cut body is not a smaller batch, it is a broken one. The two headers carry what the CLI needs to name a file and label a manifest without a second request. **2026-09-05: RFC 3339 only — the Unix-ms mention was a drafting error; the CLI parses RFC3339Nano (Decision 13).** |
| 4 | **2026-09-04** — `GET /api/v1/system` gains `raw: {enabled, batches, bytes, oldest_received_at, newest_received_at, traces_before_window}`; `traces_before_window` is Decision 1's count. `tracepad system` prints the block; `tracepad export --otlp --dry-run` prints it and the batch count the filters match, and sends nothing | The export's report needs these numbers at its end, and an operator deciding whether to shorten `raw_retention_days` needs them before; `system` is where a project's counters already live (`runs.orphan_traces`, spec 014 #3). A dry run that answers "12,400 batches, 3.1 GB, 214 traces older than the window" is what a person runs before pointing the command at a receiver. |
| 5 | **2026-09-04** — `tracepad export --otlp` takes **one destination**: `--to <url>` (an OTLP/HTTP traces endpoint, `--header k=v` repeatable, `OTEL_EXPORTER_OTLP_HEADERS` honoured, `--gzip` to compress on the wire) or `--dir <path>` (one file per batch, `<received_at_ms>-<id>.pb` or `.json` by content type, plus `manifest.jsonl` with the listing row per line, appended as each file lands). Filters `--since`/`--until`, resume with `--after <cursor>`, `--dry-run`, `--json` for the summary. Progress on stderr, one line per 100 batches; the summary — batches sent, bytes, the last cursor, Decision 1's uncovered count — on stdout (owner decision 2026-09-04: both destinations) | A receiver is the ordinary destination and the promise's literal form; a directory is the promise kept when there is no receiver yet — the operator takes the files and the manifest and is not locked in by the absence of a collector. Files are named so that `ls` is in replay order and the manifest is what a script needs to replay them with `curl`, content type included. `--gzip` is off by default because the receiver's support for it is the one thing the CLI cannot know, and the bytes are already on a local link more often than not. |
| 6 | **2026-09-04** — Failures during replay: `429`, `5xx` and transport errors are **retried with exponential backoff** (1 s, doubling, capped at 30 s, six attempts); any other `4xx` — and the retries' exhaustion — **stops the export** with a non-zero exit, the offending batch id, the receiver's status and body, and the cursor to pass as `--after` to continue from the batch that failed. Nothing is skipped; the order has no holes (owner decision 2026-09-04) | A receiver that says 429 or 503 is asking for time, and a nightly export that died at 03:14 for a fifteen-second blip is the wrong outcome; a receiver that says 400 or 413 is describing the batch, and sending the next one would leave a hole the operator would not find until they looked for the trace. Stopping with a cursor makes the retry a command line rather than a diff of two listings. Re-sending a batch a receiver already took is safe against this server (per-span upsert, spec 002 #6) and against the OTLP collectors, which is why the resume point is "the batch that failed", inclusive. |
| 7 | **2026-09-04** — `POST /v1/traces` accepts **`Content-Type: application/json`** as the OTLP/JSON encoding of `ExportTraceServiceRequest`: `traceId`, `spanId`, `parentSpanId` and link ids **hex-encoded** as the OTLP specification prescribes (not protobuf-JSON's base64), 64-bit integers as strings or numbers, unknown fields rejected as they are for protobuf. Gzip applies as it does today. The response mirrors the request's encoding: a JSON `ExportTraceServiceResponse` with `partialSuccess` (spec 002 #13). Mapping is the same mapper over the same decoded message; `dialectOf` sees no difference | The OTLP specification defines the JSON encoding and every SDK can emit it (`OTEL_EXPORTER_OTLP_PROTOCOL=http/json`); `docs/ingest.md` has said "not implemented" since spec 002, and design §6.1 deferred it to demand. The hex-id rule is the one place OTLP/JSON departs from `protojson`, and it is the departure every collector implements; accepting base64 as well would make a body mean two things. One mapper because the encoding is transport, not meaning. |
| 8 | **2026-09-04** — A JSON batch is stored **as received** — the decoded (un-gzipped) JSON bytes, zstd at rest like the rest of the table — and schema 0012 adds `raw_batches.content_type TEXT` (`NULL` = `application/x-protobuf` for every row written before this migration). `GET /api/v1/raw/{id}` and the export replay it as JSON under its own content type; `dialect` and `content_encoding` are recorded as before (owner decision 2026-09-04: as received) | Raw-first means the archive is what arrived, and a conversion at ingest would make the archive the converter's output — a bug in the hex-id rule would be baked into every stored row, unfixable by a remap because the original is gone. A column rather than sniffing because the first byte of a protobuf message can be `{`. The replay keeps the encoding because the receiver accepted it once; a receiver that takes only protobuf is the one case where a `--dir` export and a conversion by hand is the answer, and `docs/export.md` says so. |
| 9 | **2026-09-04** — The raw endpoints are **project-scoped reads under the project's keys**, with no admin token involved, and absent from MCP | The archive is the project's data as the traces are; a key that reads traces reads the batches they came from. MCP tools are for a model reading traces (spec 004), and a body of protobuf is not a thing a model reads. |
| 10 | **2026-09-05** — `system.raw.bytes` is the **stored** size — what the archive occupies on disk, compressed — while the listing's `size_bytes` stays the decoded length the API contract names. Both are documented as what they are | The two numbers answer two questions. A script deciding whether to fetch a batch is asking what a fetch will return, which is the decoded length; an operator deciding whether to shorten `raw_retention_days` — which is the question Decision 4 says this block exists for — is asking what the archive costs on disk, and a decoded total would overstate that by the compression ratio, which is an order of magnitude on OTLP. Summing decoded lengths would also mean reading every row of the table to answer `/system`, where the stored total is a `SUM(length(body))` the page cache already holds. |
| 11 | **2026-09-05** — An unknown field in an OTLP/JSON body is **ignored**, not refused, exactly as it is on the protobuf path (`protojson` with `DiscardUnknown`). Decision 7's "unknown fields rejected as they are for protobuf" is read as "handled as they are for protobuf" | Protobuf does not reject unknown fields — `DecodeExportRequest` skips them, "so a newer exporter that adds a field still ingests" — so the literal reading would have made the JSON door *stricter* than the protobuf one and broken every SDK on a newer OTLP version the moment it added a field. The OTLP specification says receivers should ignore what they do not know. The two encodings behaving differently here is the one thing Decision 7 rules out. |
| 12 | **2026-09-05** — `GET /api/v1/raw/{id}` answers **uncompressed**; the API contract's "`Accept-Encoding: gzip` is honoured by the server's usual middleware" describes middleware this server does not have | There is no response-compression middleware anywhere in `internal/server`, and adding one for this endpoint alone would be a deployment-wide behaviour change smuggled in under a spec line that assumed it already existed. The export's on-the-wire compression is `--gzip` on the POST side, which is the half that matters: the fetch is from a server on the operator's own machine more often than not, and a receiver on the far side of a link is what `--gzip` is for. A general response-compression layer is worth its own decision, in its own spec. |
| 13 | **2026-09-05** — `--since`/`--until` in the CLI now render **RFC3339Nano** rather than RFC3339, for every command that takes them | `run.instant` parsed a sub-second timestamp and then formatted it away, silently widening the window by up to a second. For the trace listing that was invisible; for the archive, whose arrivals are milliseconds apart, it is the difference between a window and a wrong one. Fixed where it lives rather than worked around here, because the truncation was never right anywhere. |
| 14 | **2026-09-26** — **Supersedes #5 on headers: `export` reads no `OTEL_*` variable, and never sends a Tracepad key unless told the receiver is a Tracepad of the same owner.** `OTEL_EXPORTER_OTLP_HEADERS` is no longer read; it was the only `OTEL_*` variable `export` ever read — the endpoint is `--to`, which is required, each body goes out under the `Content-Type` it arrived in so there is no protocol to choose, and compression is `--gzip` — so nothing else changes. The receiver's headers come from `--header k=v` alone, sent as written (the percent-decoding was the variable's encoding, not the flag's). Whenever the variable is set, one line on stderr says it is not read, without its value. Header names are canonicalized (`http.CanonicalHeaderKey`), so `authorization` and `AUTHORIZATION` are one header with one value, and the last `--header` naming it wins — decided by the command line's order, not a map's; a name that is not an RFC 7230 token, or a value holding a control character other than tab, is a usage error at once rather than a transport error the retry loop would spend half a minute on. `Content-Type`, `Content-Encoding`, `Content-Length` and `Host` are refused as `--header` names: the export sets them per batch, and overriding them would relabel every body. Everything bound for the receiver is read for a Tracepad key before **any request** to either server: each header value, and `--to` whole and raw — user info, path and query as they go out, not a parser's view of the query, which drops malformed pairs. The detector reads no authorization scheme; it looks at the text in three forms — as given, percent-decoded, and every run that decodes as base64 (standard or URL-safe, padded or not) decoded, one level — so it is a **superset of what the server accepts as a credential** and has nothing to keep in step with the server's parsing. In any of the three: (1) one of the command's own keys — the one it reads the archive with (`TRACEPAD_API_KEY` or `--key`) and `TRACEPAD_ADMIN_TOKEN` when it is set — is refused **always**, because no receiver has a use for the source's credentials; a key of 16 characters or more is found anywhere, a shorter one only as a whole token (bounded by characters no key is made of), so an admin token `dev` is not found in `development`; (2) `tp-sk-` in any case is refused unless `--allow-tracepad-key` says the receiver is a Tracepad server of the same owner whose own key it is (a migration, a round trip). User info in `--to` is not refused as such, since collectors behind Basic auth are reached that way. Both refusals exit 1 and name where the key was, never the key. It is not the default, and it is not inferred from `--to` matching `--url`: the same server behind two names, a proxy or a port-forward would make that comparison wrong in the direction that leaks | The docs tell every machine that sends traces here to put `authorization=Bearer tp-sk-…` in `OTEL_EXPORTER_OTLP_HEADERS`, and #5 copied that variable into every POST to `--to`: a vendor's endpoint, a collector or somebody else's Tracepad received a key with admin rights over the project. With the variable and a `--header` naming the same header in different case, both entries rode in a map and the one set last on the request — random per batch — won, so some batches carried the Tracepad key even when a `--header` meant to replace it. The variable is for an application's exporter, not for moving data between backends; a receiver's credentials belong on the command line that names the receiver. A key is still a secret on a command line (the process list, shell history); a header file is a later change, not needed to close the leak. |

---

## Data contract (schema 0012)

```sql
ALTER TABLE raw_batches ADD COLUMN content_type TEXT;
```

`NULL` reads as `application/x-protobuf`. No index changes:
`idx_raw_batches_received (project_id, received_at)` already serves the
listing; the keyset needs `id` as the tiebreak, which the primary key
supplies.

`store.RawBatch` gains `ContentType`; `sweepRawBatches` is untouched.

---

## API contract

**`GET /api/v1/raw`**

| Parameter | |
|---|---|
| `since`, `until` | On `received_at`; the trace listing's time grammar. `until` is exclusive. |
| `limit` | 1–500, default 100. |
| `cursor`, `direction` | Keyset over `(received_at ASC, id ASC)`; `direction=prev` walks back. Cursor grammar as spec 009. |
| `count` | Adds `total` / `total_capped` (cap 100,000). |

Response: `{"batches": [{"id", "received_at", "dialect", "content_type",
"content_encoding", "size_bytes"}], "next_cursor", "prev_cursor"}`.
`size_bytes` is the decoded length, not the zstd length.

**`GET /api/v1/raw/{id}`** — the body; `Content-Type` as stored;
`X-Tracepad-Received-At` (RFC 3339), `X-Tracepad-Dialect`; `404` for
another project's id or a swept one. Budget-exempt (Decision 3).
`Accept-Encoding: gzip` is honoured by the server's usual middleware.

**`GET /api/v1/system`** — the `raw` block of Decision 4. `enabled` is
`TRACEPAD_STORE_RAW`; with it off and the table empty, the other fields are
zero and `oldest_received_at`/`newest_received_at` are `null`.

**`POST /v1/traces`** — Decision 7. The `400` for a JSON body that is not
OTLP/JSON names the field, as the protobuf path's decode error does.

`openapi.json` grows the two paths and the `raw` block; `schema.d.ts`
regenerates in the same commit; the parity test covers the new routes.

---

## CLI contract

```
tracepad export --otlp (--to <url> | --dir <path>)
                [--header k=v]... [--gzip]
                [--since <t>] [--until <t>] [--after <cursor>]
                [--dry-run] [--json]
```

- Exactly one of `--to` / `--dir`; `--header` and `--gzip` only with
  `--to`. `--dir` is created if absent and must be empty unless `--after`
  is given (a resume appends to the manifest).
- `--to` posts each body with its stored `Content-Type`, the headers, and
  `Content-Encoding: gzip` when `--gzip`; a `2xx` with a `partialSuccess`
  in the response is logged with the receiver's message and counted, not
  treated as failure (that is the receiver's business).
- Exit codes: `0` done; `1` stopped (Decision 6), with the cursor on
  stdout as the last line (`--json`: in the summary); `2` usage.
- Summary (`--json`): `{"sent", "bytes", "first_received_at",
  "last_received_at", "last_cursor", "traces_before_window", "stopped_at":
  null | {"id", "status", "message"}}`.
- `tracepad system` prints the `raw` block after `runs`.

The usage parity test (`internal/cli`) covers every flag.

---

## Testing

**Store / server (Go)**:

- Migration 0012 on a database with rows: `content_type` `NULL` reads as
  protobuf through `GET /api/v1/raw`.
- Listing: forward order across three pages with `next_cursor`, back with
  `prev_cursor`, `since`/`until` boundaries (exclusive `until`), `count`
  and its cap, another project's batches invisible.
- Body: bytes equal to what the fixture sent (protobuf and JSON), headers
  present, `404` across projects, budget middleware bypassed (a body larger
  than the budget comes back whole).
- `system.raw`: counts and window on a seeded project;
  `traces_before_window` counts a trace with `start_time` before the oldest
  batch and not one after; `enabled: false` with `TRACEPAD_STORE_RAW=off`.
- JSON ingest: the fixture corpus re-encoded as OTLP/JSON (a helper in
  `internal/otlptest` that converts a built `.pb` to the JSON encoding with
  hex ids) ingests to the **same mapped rows** as the protobuf body —
  golden equality per fixture; base64 ids are a `400` naming the field;
  the response is JSON with `partialSuccess` for fixture 004; gzip + JSON.
- Raw storage of a JSON batch: `content_type` recorded, body equal to the
  decoded request.

**CLI (Go, against a test server)**:

- `--to` replays N batches in `received_at` order with the stored content
  types; a receiver stub records order and headers; `--gzip` sets the
  encoding and the stub decompresses to equal bytes.
- Retry: the stub answers `503` twice then `200` — three attempts, the
  batch sent once successfully; `429` likewise; a `400` stops with exit 1,
  the id, and a cursor that, passed as `--after`, resends that batch first.
- `--dir`: files named and ordered, `manifest.jsonl` one row per batch,
  a resume appends and does not duplicate.
- `--dry-run` sends nothing and prints the counts; `--json` summary shape.
- **Round trip**: export from server A with `--to` server B's `/v1/traces`;
  B's traces, observations, payloads and metadata equal A's (the read API
  compared field by field, ids included).

**Mutation** — each invariant above by reverting its line; the table in
the PR.

**E2e (Playwright)**: none — no screen changes. `make e2e` must stay green
(the fixture corpus is unchanged).

**Measurements in the PR**: replay throughput of the fixture corpus
against a local receiver (batches/s, MB/s); the listing's page time at
10,000 batches (seeded), under the read-API budget the README states.

---

## Edge cases

- **`TRACEPAD_STORE_RAW=off` since day one**: `export` exits 1 with
  "raw storage is off; nothing to replay" and the uncovered count equal to
  every trace; `--dry-run` says the same with exit 0.
- **Raw window shorter than the traces**: the export covers the window
  and reports `traces_before_window`; `docs/retention.md` gains the
  sentence that the raw window is the export's reach.
- **A batch swept between the listing page and the body fetch**: `404`;
  the CLI stops as for a `4xx` from the receiver? No — the archive moved,
  not the receiver: the CLI logs the id as swept, counts it, and continues
  (the only skip in the command, named in the summary as `swept`).
- **A receiver that answers `2xx` with `partialSuccess.rejectedSpans`**:
  logged and counted, not retried; the receiver has the bytes.
- **A JSON body with `traceId` in base64**: `400` naming `traceId`; the
  raw row is not written (nothing was accepted, spec 002 #13's "accepted
  request body").
- **Replaying into this server** (a migration between two Tracepads): the
  run link, the session, every column re-resolves through the receiving
  mapper; `tracepad.run_id` on a span whose run does not exist there is
  an orphan (spec 014 #3), counted as such — the export moves traces, not
  datasets.
- **`--dir` on a non-empty directory without `--after`**: exit 2, so that
  two exports never interleave one manifest.
- **A cursor from `GET /api/v1/raw` passed to `--after` after `--since`
  changed**: the cursor wins for position, the filters still bound the
  end; a cursor before `since` is a `400` from the listing, surfaced as
  exit 2.

---

## Config additions

None. `TRACEPAD_STORE_RAW` is read by the `raw` block; no new variable.

---

## Out of scope

- `tracepad remap` (the other reader; a spec of its own, over this
  spec's `content_type`).
- Synthesizing OTLP for traces without a raw body.
- OTLP over gRPC; OTLP/JSON for anything but traces.
- An `import` command: the receiver of a replay is `POST /v1/traces`.
- Exporting scores, prompts, datasets — the CLI's `--json` listings
  already are their export (`docs/cli.md`).
