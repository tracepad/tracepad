# Spec 003 — Scores & Prompts API

**Status:** ✅ SHIPPED
**Sprint:** August–September 2026

> The first native write surface beyond telemetry: attach quality scores to
> traces, observations, and sessions, and manage versioned prompts with
> movable labels. Both are designed freely (no foreign wire contracts) as
> small JSON APIs under `/api/v1`, and both write through the same durable
> group-commit pipeline as ingest. After this spec ships, an eval loop can
> grade yesterday's traces and an application can fetch its production
> prompt — from the same binary that stores its traces.

---

## Overview

Deliverables:

- Scores: `POST /api/v1/scores` (single or array), `GET /api/v1/scores`
  (filtered list), `GET /api/v1/scores/{id}`.
- Prompts: `GET /api/v1/prompts`, `GET /api/v1/prompts/{name}` (by label or
  version), `POST /api/v1/prompts/{name}/versions`,
  `GET /api/v1/prompts/{name}/versions`,
  `PUT`/`DELETE /api/v1/prompts/{name}/labels/{label}`.
- Schema 0003: `scores`, `prompts`, `prompt_labels`.
- The group-commit writer generalized to carry these writes with the same
  durability guarantee as ingest.
- Docs: `docs/scores.md`, `docs/prompts.md` (same PR).

## Decisions log

| # | Decision | Why |
|---|---|---|
| 1 | Native surface is plain JSON under `/api/v1/*`; no protobuf | These are interactive application/SDK calls with no OTLP counterpart. JSON is debuggable with curl, which is the design's agent-first bar (design §3). |
| 2 | Auth identical to ingest: `Bearer <tp-sk>` or `Basic base64(pk:sk)` against project keys | One credential story for the whole binary (spec 002 #2). The project resolved from the key scopes every read and write. |
| 3 | Score ids are client-optional; omitted ⇒ server generates 32-hex (`randomHex(16)`, like project ids); writes **upsert** by `(project_id, id)` | Retry-safe by the same logic as spec 002 #5: a re-POST with the same id is idempotent, and a correction is a re-POST, not a DELETE+POST. Server-generated ids keep the common case zero-thought. |
| 4 | A score targets at least one of `trace_id` / `session_id`; `observation_id` additionally requires `trace_id`. Target existence is **not** checked | Evals run async and may grade a trace whose spans are still in flight; refusing would force clients to poll. Dangling scores are visible in lists and get cleaned up by retention alongside their targets. |
| 5 | `data_type` ∈ numeric\|boolean\|categorical\|text. numeric/boolean carry `value` (boolean: 0 or 1), categorical/text carry `string_value`; the other field must be absent. Omitted `data_type` is inferred: `value` present ⇒ numeric, `string_value` present ⇒ text | The common cases (a float from an eval, a text verdict from a judge) need no ceremony; boolean and categorical are semantic claims the client must make explicitly. Cross-field checks keep every row internally consistent. |
| 6 | Score `metadata` is inline JSON TEXT, not a `payloads` row | Payload indirection exists for MB-scale LLM IO (spec 002 #8). Score metadata is judge config and eval context — small by nature, and inlining keeps score reads join-free. |
| 7 | An array POST is all-or-nothing: one transaction, one 400 naming the first invalid item | Partial success is OTLP's world, where the sender is a fire-and-forget exporter. Here the client is an application that can fix its batch and retry; a half-applied batch is harder to reason about than a clean failure. |
| 8 | No `config_id` column yet; `score_configs` (typed score schemas) is a later spec | Forward-only migrations make `ALTER TABLE ADD COLUMN` a one-liner later; a column referencing a table that does not exist is a trap, not future-proofing. |
| 9 | Scores and prompt writes go through the group-commit writer (generalized to accept write jobs, not just ingest batches) | Spec 002 #23 put the only `synchronous=FULL` connection in the writer; a 200 on a score must mean "on disk" for the same reason a 200 on an export does. Serializing through one goroutine also gives prompt version assignment its atomicity for free. |
| 10 | Prompt versions are append-only per `(project, name)`; `version` = current max + 1, assigned inside the write transaction; `type` (text\|chat) is fixed by the first version, later mismatch ⇒ 400 | Versions are an audit trail: nothing is edited in place, rollback is a label move (#12), not a rewrite. A name that changes shape between versions breaks every client that fetches by label, so the type is part of the name's contract. |
| 11 | `latest` is virtual: always the highest version, computed at read time, reserved as a label name | A stored `latest` label is a cache that can drift from the truth it summarizes. Reserving the name means `?label=latest` and the unqualified GET are the same code path and cannot disagree. |
| 12 | Labels are unique per `(project, name, label)` and move atomically: assignable at version creation, movable via `PUT .../labels/{label}` (the rollback path), removable via `DELETE` | Deploy-by-label is the point of prompt management: promote = move `production` to a new version, roll back = move it back — no redeploy, no new version. Uniqueness makes "which version is production" a single-row answer. |
| 13 | `GET /api/v1/prompts/{name}` resolves `?version=N` xor `?label=L`; both ⇒ 400; neither ⇒ latest | No implicit `production` default: a convention the server invents is a support question factory. Unqualified GET meaning "newest" matches every other versioned-artifact tool. |
| 14 | Prompt GET responses carry `Cache-Control: max-age=60` uniformly | Design §6.2 wants client caching hinted by the server, and 60 s bounds label-move propagation. A longer `immutable` for version-pinned GETs is not worth a second cache branch: a client that pins versions may cache forever regardless, and the docs say so. |
| 15 | No server-side templating: `prompt` is stored and returned verbatim; variable interpolation is the client's job | Template syntax is a language-runtime concern (and every framework has its own). Storing opaque JSON keeps the server out of that treadmill (design §6.5 draws the same line for the SDK). |
| 16 | JSON timestamps are RFC 3339 UTC; storage stays Unix nanoseconds | Spec 002 #4 keeps ns internally; rendering ISO at the API boundary is the layer split design §5.2 assigns. Score `timestamp` is client-optional (event time, e.g. when the graded interaction happened), defaulting to receive time. |
| 17 | Unknown JSON fields are rejected with a 400 naming the field | Agent-first cuts both ways: an unattended agent that typos `commet` must get an error, not silent data loss. Server and clients are versioned together in a self-hosted binary, so strictness costs no forward compatibility. |
| 18 | List endpoints use `limit` (default 50, max 500) + opaque `cursor` pagination and never return prompt bodies | Cursor pagination is stable under concurrent writes where offset is not; the pattern is set here exactly as the read API (spec 004) will use it. Version lists are for picking and diffing — bodies come from the single-prompt GET. |
| 19 | **2026-08-27** — `/api/v1/*` requires no `Content-Type`: a body is parsed as JSON whatever the header says (or does not say) | #1 justifies this whole surface by its debuggability with curl, and `curl -d '{…}'` sends `application/x-www-form-urlencoded`. Demanding the header would 415 exactly the command the design uses as its bar, to prevent nothing: a body that is not JSON fails at the parser with a 400 either way, and the JSON routes have no second wire format to disambiguate — unlike ingest (spec 002 #1), where the 415 tells an exporter it guessed wrong about protobuf. |
| 20 | **2026-08-27** — Validation that needs stored state — a prompt's existing type, whether a labelled version exists — runs inside the write transaction and comes back as a typed rejection the handler renders as 400/404 | The alternative is a read in the handler before the submit, which is a race: between the check and the commit another writer can create the name or move the label, so the handler would either reject a legal write or accept an illegal one. The writer already serializes every write (#9), so the transaction is the only place where "does this name exist yet" has an answer that is still true when it is acted on. Typing the refusal (rather than returning a plain error) is what keeps a client mistake out of the 500s and out of the error log: the writer logs a rejection as routine, a storage failure as an incident. |
| 21 | **2026-08-27** — An unknown query parameter, and a `limit` outside 1–500, are 400s | #17 refuses unknown JSON fields so that an unattended agent's typo surfaces instead of silently dropping data; a mistyped filter is the same failure with a worse blast radius — `?trace=abc` would silently widen a listing to the whole project and the caller would act on the wrong rows. Out-of-range `limit` follows: a client asking for 5000 is reasoning about a page size it will not get, and silently handing back 500 makes it believe it has seen everything. |
| 22 | **2026-08-27** — `scores.data_type` and `prompts.type` are `NOT NULL`, tightening the Data contract above | The contract writes them with a CHECK only, and a CHECK does not constrain NULL: `NULL IN ('numeric', …)` evaluates to NULL, which passes. That leaves the schema admitting a row the API cannot produce — #5 resolves a data type for every score before it is written, and #10 fixes a prompt's type at its first version. A column that permits an unreachable state is a trap for the first reader who trusts it (the read API, spec 004, will join on these), so the schema says what the decisions already guarantee. |
| 23 | **2026-08-27** (from PR #4 review) — A request that cannot mean what it says is refused, never reinterpreted: a timestamp outside 1678–2262, a query parameter present without a value, a batch that gives one id to two scores, an empty `content` in a chat message | Four cases of one failure, each found by review as a silent reinterpretation. `time.Time.UnixNano()` is undefined outside its int64 span, so `"3000-01-01T00:00:00Z"` was accepted and read back as 1830 — a stored lie. `?label=` is what an unset shell variable expands to, and resolving it to "the latest" would ship an unreleased prompt to a caller that believes it asked for `production`; the same shape empties a filter (`?name=`) and answers a narrower question with a wider one, which is what #21 already refuses for unknown names. A batch naming one id twice upserts once (#3) but answers with two ids, so a client counting what it wrote counts wrong. And an empty message `content` cannot be corrected afterwards — versions are append-only (#10) — so the client discovers it when the model call fails, not when the version is written. |
| 24 | **2026-08-27** (from PR #4 review) — Every transaction opens with `BEGIN IMMEDIATE` (`_txlock=immediate` in the DSN) | Spec 002's jobs write first, so their deferred transactions took the write lock at the first statement. Prompt jobs read first — they must, to number a version (#10) — which makes the transaction start as a *read* snapshot and then need an upgrade, and SQLite refuses that upgrade with `SQLITE_BUSY_SNAPSHOT` whenever another connection has written since the snapshot was taken. It is not a lock to wait out: `busy_timeout` does not apply, so the failure surfaced as a 500 on a write the caller could not have done anything about (reproduced at 18 failures in 120 runs of the concurrency test; zero after). Taking the write lock at `BEGIN` removes the upgrade entirely, and it costs nothing here because every transaction this binary opens is already a write and they are already serialized through one goroutine (#9). |
| 25 | **2026-08-27** (from PR #4 review) — The score keyset indexes carry the `id` tiebreak (`(project_id, timestamp DESC, id DESC)` and `(project_id, name, timestamp, id)`), and the cursor predicate is a row-value comparison `(timestamp, id) < (?, ?)` rather than the equivalent `OR` form | #18 chose keyset pagination over offset to avoid scanning past what the client already read; as first written it scanned anyway. `EXPLAIN QUERY PLAN` on the shipped schema: the `OR` form searches on `project_id=?` alone and adds `USE TEMP B-TREE FOR LAST TERM OF ORDER BY` — SQLite cannot seek through a top-level `OR`, and cannot satisfy `ORDER BY timestamp DESC, id DESC` from an index that stops at `timestamp`. Both halves are needed and neither is sufficient: with the index alone the temp B-tree goes away but the seek does not appear; with the row-value form alone the seek reaches `timestamp` but the sort remains. With both, every page is `SEARCH scores USING COVERING INDEX … (project_id=? AND (timestamp,id)<(?,?))`. Editing migration 0003 in place is legitimate here and only here: it is unmerged and has never been applied outside this branch's tests, so there is no database in the world that would miss the change (spec 001 #6 keeps migrations forward-only once released). |
| 26 | **2026-09-26** — Prompt reads send **`Cache-Control: private, max-age=60`** and **`Vary: Authorization, Cookie, X-Tracepad-Project`** (amends #14) | A prompt is one project's, and `max-age=60` alone let a shared cache in front of the server — a proxy, a CDN — hand one project's prompt to the next caller of the same URL, with another project's key or with none. `private` keeps it to the client that asked; the `Vary` names what decides which project a request is about — the key, the session cookie, and the project header a session sends (spec 028 #6) — for a cache that keeps private copies all the same. The sixty seconds are unchanged, so an SDK's cache behaves as before. |
| 27 | **2026-09-26** — #4's "cleaned up by retention alongside their targets" is implemented for session targets: a session-only score goes when it is older than the trace window and no trace carries its session, and with the erasure of any trace of its session (spec 044 #7, #8) | Every deletion path took scores by trace id, so a verdict on a session outlived both its traces and an erasure. See spec 044 #7, #8. |
| 28 | **2026-09-28** — **An array holds at most 10,000 scores** (owner decision; spec 043 #36). One longer is `413` `{"error": "this request carries N scores; the server takes at most 10000 per request — send them in batches"}`, answered once the body is read: its top-level array is counted before any item is decoded into a request — an unknown field, a `null`, a value of the wrong type in it change nothing — and before anything reaches the writer, so an over-long array gets the same answer whatever it holds. A body that is not one well-formed array is not counted and keeps the `400` it had. Refused whole, never cut. A single object is one score and never meets it. The cap is a constant, not a setting. Amends the API contract, where an array had no count bound but the body cap, and keeps #7: an array within the cap is still one transaction | The body cap bounds bytes, not rows: 20 MiB of minimal scores is half a million of them, one transaction by #7, and at the measured 6.6 s per 100,000 (spec 043 #35) that held the only writer for half a minute. 10,000 commits in well under a second and is a hundred times the batch every SDK sends scores in (spec 017 #6), so no client of ours meets it. `413`, as the span cap of spec 043 #10 answers: the request is too large, and the same request will be refused again — a `400` would read as a bad field. Checked before validation so the count is the answer, not whichever item happened to be invalid first. |

## API contract

Errors are `{"error": "<message>"}` with 400 (validation), 401, 404;
409 for label/version races lost inside the writer is not needed — writes are
serialized (#9). Request bodies are capped by `TRACEPAD_MAX_BODY_BYTES`.

### Scores

`POST /api/v1/scores` — body: one score object or an array of them.

```json
{
  "id": "optional 32-hex",
  "trace_id": "…", "observation_id": null, "session_id": null,
  "name": "helpfulness",
  "data_type": "numeric", "value": 0.9,
  "comment": "judge rationale…",
  "metadata": {"judge_model": "…"},
  "timestamp": "2026-08-27T10:00:00Z"
}
```

Response: `201 {"ids": ["…"]}` in input order (also for a single object).
`name`: 1–200 chars. A client-supplied `id` must match `^[0-9a-f]{32}$` — the
shape the server generates (#3); a client with a natural key hashes it.

`GET /api/v1/scores` — filters `trace_id`, `observation_id`, `session_id`,
`name`, `data_type`, `from`/`to` (RFC 3339, on `timestamp`, half-open: `from`
inclusive, `to` exclusive); newest first.
Response: `{"scores": […], "next_cursor": "…"|null}`.

`GET /api/v1/scores/{id}` — one full score object.

### Prompts

Prompt and label names: `^[A-Za-z0-9][A-Za-z0-9._-]*$`, ≤200 chars (one URL
path segment). `latest` reserved (#11).

`POST /api/v1/prompts/{name}/versions`

```json
{
  "type": "chat",
  "prompt": [{"role": "system", "content": "You are…"}],
  "config": {"model": "gpt-5", "temperature": 0.2},
  "commit_message": "tighten tone",
  "labels": ["production"]
}
```

`type` required on the first version, optional-but-checked after (#10);
`prompt` is a JSON string for `text`, an array of `{role, content}` objects
for `chat`. Response: 201 with the full prompt object (below).

`GET /api/v1/prompts/{name}?label=…|version=…` (#13, #14):

```json
{
  "name": "summarize", "version": 3, "type": "chat",
  "prompt": […], "config": {…},
  "commit_message": "tighten tone",
  "labels": ["production"],
  "created_at": "2026-08-27T09:58:11Z"
}
```

`GET /api/v1/prompts/{name}/versions` — all versions, newest first, without
`prompt`/`config` bodies (#18). Response:
`{"versions": […], "next_cursor": "…"|null}`.

`GET /api/v1/prompts` — per name: `{"name", "type", "latest_version",
"labels": {"production": 3}, "updated_at"}`; paginated, ordered by `name`
(which is also what the cursor walks), `updated_at` being the `created_at` of
the name's newest version. Response:
`{"prompts": […], "next_cursor": "…"|null}`.

`PUT /api/v1/prompts/{name}/labels/{label}` — body `{"version": N}`; moves or
creates the label; 404 if the version does not exist. `DELETE` removes the
label. Both 200 `{"label": "…", "version": N}` (DELETE: the version it
pointed at).

## Data contract (schema 0003)

```sql
scores(
  project_id TEXT, id TEXT,                   -- PK (project_id, id); 32-hex
  trace_id TEXT, observation_id TEXT, session_id TEXT,
  name TEXT NOT NULL,
  data_type TEXT CHECK (data_type IN ('numeric','boolean','categorical','text')),
  value REAL, string_value TEXT,
  comment TEXT, metadata TEXT,                -- JSON, inline (#6)
  timestamp INTEGER NOT NULL,                 -- ns, event time (#16)
  created_at INTEGER NOT NULL                 -- ns, receive time
)
prompts(
  project_id TEXT, name TEXT, version INTEGER, -- PK (project_id, name, version)
  type TEXT CHECK (type IN ('text','chat')),
  prompt TEXT NOT NULL, config TEXT,           -- JSON
  commit_message TEXT,
  created_at INTEGER NOT NULL
)
prompt_labels(
  project_id TEXT, name TEXT, label TEXT,      -- PK (project_id, name, label)
  version INTEGER NOT NULL
)
```

Indexes: `scores(project_id, timestamp DESC, id DESC)`,
`scores(project_id, trace_id)`, `scores(project_id, session_id)`,
`scores(project_id, name, timestamp, id)` (trend queries, design §5.2; the
`id` tiebreak is #25). All STRICT (spec 001 #7).

## Testing

1. **Unit** — validation matrix for #5 (data_type × value/string_value),
   name/label grammar, strict-field rejection (#17), RFC 3339 round-trip.
2. **HTTP e2e** — in-process server: score single/array/upsert/all-or-nothing,
   list filters + cursor walk, prompt version lifecycle (create → label →
   move → delete → fetch by each path), type-mismatch 400, reserved `latest`,
   404s.
3. **Concurrency** — N parallel `POST …/versions` for one name yield versions
   1..N with no gaps or duplicates (the #9/#10 guarantee); parallel label
   moves end on a single winner.
4. **Durability** — the writer-generalization keeps the existing writer tests
   green; score/prompt acks arrive only after commit (asserted like spec 002's
   writer tests).

## Edge cases

- **Score for a trace that never arrives**: stored, listed, filterable;
  removed by retention on its own `timestamp` (retention spec).
- **Re-POST with same id, different fields**: full-row replace (#3), like
  observation redelivery.
- **`DELETE` of a label that does not exist / GET of an unknown prompt**: 404.
- **First version posted with `labels: ["latest"]`**: 400, reserved (#11).
- **Empty array POST**: 400 — nothing to write is a client bug, not a no-op.
- **Writer backpressure**: 429 + `Retry-After: 1`, same as ingest (spec 002 #15).

## Config additions

None.

## Out of scope (later specs)

Native read API for traces + CLI + MCP (004), `score_configs` / typed score
schemas (#8), prompt version diff endpoint (read API/UI), deleting scores or
prompt versions, protected labels, annotation queues, retention of scores
(retention spec covers all tables at once).
