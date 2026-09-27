# Spec 041 — Media: images and files in traces, stored once

**Status:** ✅ SHIPPED
**Sprint:** September 2026

> A multimodal call sends the model a picture, and the trace of that call
> keeps what was sent. Today it keeps it verbatim: an OpenAI `image_url` part
> carrying a base64 data URL lands whole in the observation's input — in a
> payload row, where zstd barely shrinks base64 — and a second time in the
> raw OTLP body kept for replay, for the whole retention window. An
> application that sends a few photographs per request writes megabytes per
> trace into one SQLite file and shows the operator a wall of base64 where
> the picture should be. The Langfuse SDK avoids this by uploading media
> through its own channel, which Tracepad does not serve, so on the bridge
> the picture is simply lost and a dangling reference is left in its place.
> This spec takes media out of the JSON at ingest, stores each distinct file
> once, shows images as images, and serves the Langfuse media channel so the
> bridge keeps them too.

---

## Overview

Deliverables, one PR (the last commit flips the status):

- Schema 0021: `media` (content-addressed bodies) and `media_refs` (who
  points at them) (Decisions 2–3).
- Extraction at ingest from every payload the mapper writes, and from the
  raw body before it is stored (Decisions 1, 4, 5, 19).
- A project setting, `media: store | placeholder` (Decision 6).
- `GET /api/v1/media/{sha256}`; the reference object in every read path;
  the OTLP export re-inlines (Decisions 7–8).
- The Langfuse media channel: `POST /api/public/media`, the upload `PUT`,
  `PATCH /api/public/media/{id}`, `GET /api/public/media/{id}` (Decision 9).
- Images as thumbnails in the observation's input/output panel; other media
  as a chip (Decision 10).
- `docs/media.md` (new); `docs/ingest.md`, `docs/retention.md`,
  `docs/admin.md`, `docs/api.md`, `docs/export.md`, `docs/ui.md`,
  `openapi.json`, `schema.d.ts`.

Not here: media in scores, prompts or dataset items; transcoding or
thumbnails generated on the server; URLs (`https://…` images) — a URL is a
string and stays one.

## Decisions log

| # | Decision | Rationale |
|---|----------|-----------|
| 1 | **2026-09-23** — **Extraction happens on the server, at ingest**, for every client: Tracepad's packages, the Langfuse bridge, plain OpenTelemetry (owner decision, grilling 2026-09-23). The mapper walks each JSON payload it is about to store (observation input, output, metadata; trace input, output, metadata) and recognises four shapes: a **data URL** string (`data:<mime>;base64,<…>`) anywhere a string may be; Anthropic's `{"type": "base64", "media_type", "data"}` source; Gemini's `{"inline_data" \| "inlineData": {"mime_type" \| "mimeType", "data"}}`; and the GenAI semantic conventions' blob part `{"type": "blob", "mime_type", "content"}`. A match whose decoded size is **at least 4 KiB** is extracted; smaller ones stay inline | The packages could strip media before it leaves the process, but the bridge and plain OTel would not, and the operator's disk does not care which client filled it; one rule at the one door every span passes is the only place that covers all of them. The four shapes are the ones the three providers and the conventions actually emit; a free-form base64 string with no MIME type is not recognised, because guessing what bytes are is how a trace's text gets mangled. 4 KiB keeps icons and tiny thumbnails where they are, where a reference would cost more than it saves. |
| 2 | **2026-09-23** — **Storage is a table in the same database**, `media(sha256 TEXT PRIMARY KEY, mime TEXT, size INTEGER, body BLOB, created_at)`, the body stored as decoded bytes, **one row per distinct content** across the whole store (owner decision) | One file stays the backup story (the design's single-file promise). Decoded bytes are three quarters of their base64, and content addressing is what makes a photograph sent to five generations — the common case, a retry or a multi-step pipeline — one row instead of five. SQLite holds megabyte blobs without complaint; a directory beside the database would split backups, retention and the delete paths in two. |
| 3 | **2026-09-23** — **Media follows its traces**: `media_refs(sha256, project_id, trace_id)` records every trace that points at a body; every path that deletes traces — the retention sweep, user-data erasure (spec 005 #7), trace deletion (spec 035), project purge — deletes their refs in the same transaction, and a body with no ref left is deleted by the same payload GC that already runs there. No separate media window (owner decision) | The payload GC is the model: a body lives while something points at it. A second retention knob would be one more thing to explain for no case anyone has asked for; the placeholder setting (#6) is the answer for "do not keep pictures". Refs carry the project so that one project's purge never deletes a body another project still points at, while the body itself is shared. |
| 4 | **2026-09-23** — **The reference** left in the JSON is an object, `{"tracepad_media": "<sha256 hex>", "mime_type": "…", "size": N}`, replacing the matched *string* for a data URL and the matched *object* for the other three shapes | An object cannot be mistaken for text by a reader or a search, and it carries what a reader needs to decide whether to fetch it. Replacing the whole provider object (not only its `data` field) keeps the rule one rule: wherever media was, a reference is. |
| 5 | **2026-09-23** — **Raw bodies are stored with the media factored out** (owner decision): the stored raw batch has each extracted value replaced by the same reference, before compression. Replay (spec 002's reason for raw) resolves references from `media` as it re-maps. `docs/ingest.md` states that the raw body is *as sent, media factored out* | Keeping raw verbatim would store every picture twice for the raw window, which is the cost this spec exists to remove. Replay loses nothing: the bytes are in `media` for exactly as long as a trace or a raw body points at them — raw bodies write refs too (with the trace ids the batch carried). |
| 6 | **2026-09-23** — **A project setting, `media`**: `store` (default) or `placeholder`. Under `placeholder` nothing is written to `media`; the reference object carries `"stored": false` and no body can be fetched. It is set with `PATCH /api/v1/projects/{id}` like the retention windows, and shown in Settings (owner decision) | An application that sends faces, documents or anything else it must not keep gets a switch on the server instead of a code change in every client. The placeholder keeps the type and size, so a reader still sees that a picture was sent and how large it was. |
| 7 | **2026-09-23** — **Reading**: `GET /api/v1/media/{sha256}` returns the bytes with their `Content-Type`, `Cache-Control: private, max-age=31536000, immutable` (content-addressed), for a caller whose project holds a ref to it — `404` otherwise, so a hash is not a capability across projects. Every read path (API, CLI, MCP) returns the reference object as it is in the JSON; the CLI and MCP never fetch bytes (owner decision) | An agent reading a trace needs to know that an image was sent, not to swallow three megabytes of it; an application that needs the bytes asks for them by hash. Scoping by ref is what stops one project reading another's pictures by guessing a hash that appeared in a log. |
| 8 | **2026-09-23** — **The OTLP export** (spec 019) **re-inlines**: an exported span carries a data URL where the reference is, so a trace moved to another backend is whole | Export is the one reader whose consumer does not know Tracepad's references; for it the reference would be a hole. |
| 9 | **2026-09-23** — **The Langfuse media channel** (owner decision), verified against the Langfuse Python SDK 4.7.1: `POST /api/public/media` `{traceId, observationId?, contentType, contentLength, sha256Hash, field}` answers `{mediaId, uploadUrl}` — `mediaId` is the SDK's own derivation (the first 22 characters of the base64url SHA-256), which the SDK checks, and `uploadUrl` is `null` when the body is already stored; the `uploadUrl` points at Tracepad (`PUT /api/public/media/{mediaId}/upload`, Basic or Bearer auth like the other bridge routes, the checksum verified against `sha256Hash`); `PATCH /api/public/media/{mediaId}` records the SDK's upload report and answers `204`; `GET /api/public/media/{mediaId}` answers the Langfuse shape with a `url` to `GET /api/v1/media/{sha256}`. At ingest, the reference string the SDK leaves (`@@@langfuseMedia:type=…\|id=…\|source=…@@@`) is rewritten to Tracepad's reference object when its body is stored, and left as it is otherwise | The bridge's promise is that an application keeps its Langfuse SDK; today that promise silently drops every picture, with an upload error logged per picture. Serving the four calls on the same table closes it, and deriving the id the SDK's way is what its integrity check requires. Rewriting the reference string makes a bridged trace look like any other in the interface; leaving an unresolved one as it is keeps the evidence of what the client meant. |
| 10 | **2026-09-23** — **The interface**: in the observation's input and output panel a reference renders as a **thumbnail** for `image/*` (bounded box, click opens the full image in a new tab via the media URL, the MIME type and size under it) and as a **chip** for anything else (type, size, download) (owner decision); a reference with `stored: false` renders as a muted chip saying *not stored (project setting)*. The JSON view shows the reference object as data | The picture is the point of a multimodal trace; a hash is not. Other media — audio, PDF — have no useful inline preview in a trace reader and are one click away. |
| 11 | **2026-09-23** — **Accounting**: the system page's storage figures (spec 029) gain `media` bytes and count, and a project's dry-run counts for deletion paths name the media bodies that would be freed | An operator deciding on the placeholder setting needs to see what media costs; a deletion preview that hides a hundred megabytes of pictures is a preview that lies by omission. |
| 12 | **2026-09-23** — **A raw batch's refs are rows of their own**, `media_raw_refs(sha256, raw_batch_id)`, not `media_refs` rows under the trace ids the batch carried (implementation; amends #5) | A ref keyed by trace id goes when its trace goes, and a raw batch outlives its traces by design: erasure and trace deletion leave it (spec 005 #7, spec 035 #3), and its window is its own (spec 005 #6). Under #5's wording the picture an erased trace pointed at would be collected while the batch that replays it still names it, which is the loss #5 says replay does not suffer. Keyed by the batch, the ref goes with the batch — in the raw sweep's own transaction, and by cascade — and the read scope of #7 counts a raw ref as the project's own. |
| 13 | **2026-09-23** — **`media_refs` gains `created_at` and `pending`**, with a partial index on `created_at WHERE pending = 1`. The Langfuse channel writes its ref pending when the project does not have the trace yet; ingest writes its refs settled, and settles a pending one it writes again. The hourly sweep reads the pending refs older than a day: one whose trace has arrived since is settled, one whose trace never came is dropped and the body it leaves collected. A project's purge drops every ref it still has (implementation; amends the data contract) | The Langfuse channel (#9) records the ref when the SDK asks to upload, before the trace's spans arrive, and a trace can fail to arrive at all — an exporter that gave up, a sampled-out span. Without an age there is no telling "not yet" from "never", and the body would stay until the project is purged. A day is far past any exporter's retry window. Ingest's refs are written in the transaction that writes their trace and can never be orphans, so the flag keeps the hourly look to the few refs that can: a seek on the partial index, where an age alone would read every ref a project has on every pass. |
| 14 | **2026-09-23** — **The upload URL is presigned**: `PUT /api/public/media/{mediaId}/upload` is a public route whose `?token=` is an HMAC-signed grant — project, trace, hash, type, length, an hour — under a key minted at each start (kept in the database since #22); `uploadUrl` is `null` only when **this project** already holds the body, and a Langfuse id resolves at ingest only to a body this project holds (implementation; amends #9's "Basic or Bearer auth") | The SDK PUTs the upload URL with its bare HTTP client (`media_manager.py`, `_httpx_client.put`), with no credential — against Langfuse the URL is an object store's and the signature is in its query — so an authenticated PUT would refuse every upload. A key per start is the setup token's bargain: an upload URL from before a restart fails, the SDK logs it, and the picture is lost as it would be against an unreachable Langfuse. "Already stored" is asked of the project rather than of the table because a hash any project could name would otherwise adopt another project's picture — the bytes are the proof of possession, and #7's scope would be moot. `GET /api/public/media/{id}` answers a `url` that needs a key, like every read. |
| 15 | **2026-09-23** — **Serving and drawing bytes safely**: `GET /api/v1/media/{sha256}` adds `X-Content-Type-Options: nosniff` and a sandboxing `Content-Security-Policy`, and sends anything but `image/*`, `audio/*` and `video/*` as an attachment, and answers `Vary: Authorization, Cookie, X-Tracepad-Project`; the interface fetches the bytes with its own credentials and draws a blob in an `<img>`, and opens the full image in a new tab as an `<img>` in a blank page (implementation; amends #10's "via the media URL") | The type is whatever a client declared, and a body declared `text/html` or `image/svg+xml` opened as a page of this origin would run its script beside the session cookie. A session names its project in a header (spec 028 #6) that an `<img src>` cannot send, so the media URL cannot be linked directly; a blob drawn by an `<img>` or saved by a download is never a document. #7's cache headers would otherwise let a browser answer a project with a body it cached under another, which #7's scope says is a `404`; `Vary` keys the cache by who asked. |
| 16 | **2026-09-23** — **The walk runs over the decoded export before the mapper**, rewriting attributes in place — span, event, scope and resource attributes, a JSON string attribute re-encoded only when something in it was replaced — and the raw body is that export re-encoded, with only the ResourceSpans the walk changed re-written and everything else kept byte for byte (implementation; how #1 and #5 are met) | One pass feeds both halves: the mapper maps a body whose payloads already carry references, and the archive is the same body, so a replay maps to exactly the rows the first delivery produced and there is no second walker to disagree with the first about what was media. A whole-string data URL attribute becomes an object attribute, which the mapper reads as the reference object. |
| 17 | **2026-09-23** — **The re-inlining of #8 happens in `GET /api/v1/raw/{id}`**, which the export and any other reader of the archive call; each reference comes back as what it replaced (#19), and the listing's `size_bytes` stays the stored length (implementation) | The CLI is a client with no logic of its own (spec 004), and the archive's reader is where "as sent" is owed. Computing the inlined length for the listing would mean decoding every batch to list it. |
| 18 | **2026-09-23** — **`tracepad retention set --media store\|placeholder`**, and `retention show` prints the setting (implementation; the CLI half of #6) | The setting is set where the retention windows are, as #6 says, and the CLI is where the windows are set from a terminal. |
| 19 | **2026-09-23** — **In the three object shapes the reference replaces the base64, not the object** (amends #4): the Anthropic source's `data`, the GenAI blob's `content`, the Gemini part's `inline_data.data`; every other field stays as the client sent it. An Anthropic image is `{"type": "image", "source": {"type": "base64", "media_type": "image/jpeg", "data": {"tracepad_media": …}}}`. A data URL string is still replaced whole. The way out (#8, #17) puts back what each reference replaced: the base64 in an object's slot, the data URL of a whole string. The archive's JSON splice keeps the untouched elements and the envelope as their source bytes (code review) | Replacing the whole object dropped what rode beside the bytes — the blob part's `modality`, which the conventions require, a Gemini part's `thought_signature` or `display_name` — from the payload and, because the raw archive is the same rewrite, from replay and export, beyond recovery; #5 promises the archive loses the media and nothing else. Writing into the slot keeps the rule "wherever media was, a reference is" (the reference is still the one object readers and the interface look for) and makes the round trip exact: an export is the client's payload, where a data URL in place of an Anthropic source was a request the provider would refuse. |
| 20 | **2026-09-23** — **The application-line ceiling (`UI_BUDGET`) rises from 21,500 to 21,800** (owner decision 2026-09-23) | Measured the way spec 035 #17 raised it: `main` stood at 21,484; this spec's interface — the strip, the pure module, the client's call and the Settings control — came to ~206 lines, and the review round's teardown fix to 8 more, landing at 21,690. The ceiling is raised once for the known set, with room for the review cycle, rather than left as a standing warning. |
| 21 | **2026-09-23** — **A Langfuse reference string whose upload landed after its span reads as the reference** (owner decision; amends #9's "left as it is otherwise"): when a stored payload holds `@@@langfuseMedia:…\|id=X\|…@@@` and the project holds a ref from *that trace* to the body whose Langfuse id is X, every read (`/io`, a trace's metadata, a run's output, and so the interface, CLI and MCP) answers the reference object in its place — the same one ingest writes. The stored payload is not rewritten; the raw archive and the export keep the string | The SDK uploads on a thread of its own, and a script that sets an input and flushes at once exports the span first; the ingest-time rewrite then finds no body and the picture is a string forever, although the bytes arrive a moment later. Resolving on read closes the race without a second write path: the refs table already says which trace the upload was for, the check runs only on a payload whose bytes contain the marker, and a read that does not rewrite needs no transaction, no recount of payload sizes and no second rule for the archive, which is owed the batch as sent. |
| 22 | **2026-09-23** — **The upload key is kept in the database** (owner decision; amends #14's "minted at each start"): `server_keys(name, key, created_at)` in schema 0021, `media_upload` minted with 32 bytes of `crypto/rand` at the first open that finds it missing, read at every open after | A key per start lost every picture whose upload URL was issued before a restart — an upgrade, a crash, a `docker restart` in the middle of a run. The URL is already bounded by its hour and its exact grant (project, trace, hash, type, length), so outliving a restart widens nothing; the database is what a restart keeps, and a backup that carries the key restores a server whose URLs still work. The setup token stays per start: it opens an account, and a log line from last week must not. |
| 23 | **2026-09-23** — **Second review round** (implementation; how #5, #6 and #9 are met): (a) a JSON body is spliced by value, at both levels — the walk records the JSON path of every value it replaces, in the export and inside a JSON document an attribute holds, and one pass over the source replaces those bytes and no others; (b) under `placeholder` ingest resolves no Langfuse string, even to a body the project already holds; (c) a batch whose Langfuse strings were resolved to a body collected before its write is refused inside the write and taken again with the strings kept; (d) the channel's `traceId` must be 32 lower-case hex digits | (a) JSON decoding drops the fields it does not know, and a re-encoded element or document sorts its keys and re-escapes its strings; #5 promises the archive loses the media and nothing else, and the archive exists for a replay by a later version that may know those fields. (b) Resolving adds a ref for the new trace, which keeps a picture alive under a setting that says not to keep one. (c) The resolution is read outside the write; without the check the payload and the archive would point at a body that is gone and the SDK's string — the only evidence of the picture — would be lost with it. (d) A ref under any other spelling of the id names a trace no span can arrive for. |
| 24 | **2026-09-23** — **Third review round** (implementation): (a) a MIME type is case-insensitive, and so are a data URL's `data:` and `;base64` — a match is extracted in any case and its type stored and referenced lower-case, from ingest and from the Langfuse channel alike; (b) the upload PUT answers `413` only for a body over the declared length, and `400` for one that cannot be read; (c) the hourly look for bodies no ref names reads one page of the primary key per pass behind a cursor; (d) the interface's clock covers a media body's headers, not its bytes; (e) a batch's distinct base64 values are decoded and hashed once each | (a) `image/PNG` and `data:…;BASE64,` are legal (RFC 2045, RFC 2397) and were missed or drawn as a chip; the interface and the serving headers test the lower-case prefix. (b) The SDK logs what it is told, and a client that hung up has no size problem. (c) Every write and deletion of refs is one transaction, so only a hand-edited database leaves such a body; the belt must not cost a scan of every body every hour. (d) A 20 MiB body on a slow link outlasts the thirty seconds meant for a server that does not answer. (e) A conversation re-sends its history each turn, and one picture in it was decoded once per generation. |
| 25 | **2026-09-26** — **Each project has its own view of a body** (amends #2's single type, #7, #9's `GET`, #15, and the edge case "the first stored type wins"): `media_holders(sha256, project_id, mime_type, first_at)` holds, for every project with a ref to a body — from a trace or a raw batch — the type that project first stored it under and when its hold began; the row is written with the project's first ref to the body and deleted with its last, in the same transaction, and goes with the body by cascade. `GET /api/v1/media/{sha256}` answers `Content-Type`, and decides attachment, by the caller's type; `GET /api/public/media/{mediaId}` answers the caller's `contentType` and, as `uploadedAt`, its `first_at`. The bytes stay one row per content; `media.mime_type` is kept as the type they were first stored under and no answer reads it — a hold written with no type of its own in hand is `application/octet-stream`, never another project's type. The interface draws every thumbnail and full image as a blob of the *reference's* `mime_type` | A type is a client's claim, and stored once per content it let the first project to send a file decide how every other project was served it: bytes another project had declared `text/plain` came back `text/plain` to a project that sent them as `image/svg+xml`, which then would not draw; and the `Content-Type`, with the Langfuse record's `uploadedAt`, told a project that some other project had held the same file first, and since when. Langfuse keeps one media record per project and hash; so does this now, while the bytes stay shared (#2). A row per holder rather than a type on every ref is one row per project and body however many traces point at it, and gives `uploadedAt` a time without a scan of the project's refs. Within a project the first type still wins — one project's clients are one trust domain — and each reference keeps the type its own client declared, which the export already re-inlines (#8) and the interface now draws. |
| 26 | **2026-09-26** — **A project's hold is found by `(sha256, project_id)` alone, before the body is read** (amends #7, #12, #14, #23(c)): `media_raw_refs` gains `project_id`, filled from its batch and indexed with the hash, and `media_holders` is indexed by `(project_id, sha256)`, so that a Langfuse id — a hash range — is sought among the project's own rows; every "does this project hold this body" — the read endpoint, the Langfuse `GET`, the channel's `uploadUrl: null`, ingest's resolution of a Langfuse id, the export's re-inlining — is the project's `media_holders` row and a ref of the project in `media_refs` or `media_raw_refs`, each a seek on the pair, and the `media` row is read only once the answer is yes. The in-write check of #23(c) asks whether the project still holds each resolved body, not whether the body exists, and so does the write behind the channel's `uploadUrl: null`, which otherwise asks for the bytes. Both halves of the check stay: the hourly look for bodies with no ref also takes a holder row with no ref of its project behind it and restores a missing one behind a ref, as `application/octet-stream`, and a raw ref written without its project is refused by the database | #7 answered `404` alike for a hash nobody held and for one only another project held, but did not do the same work: the first stopped at the missing `media` row; the second found the row and then tested the raw scope by walking the caller's raw batches (`idx_raw_batches_received` on `project_id`, then a probe of `media_raw_refs` per batch), so its time grew with the caller's archive and the latency said what the status withheld. Seeks on the pair make both answers the same few missed probes, and serve a project's own raw-only body — an erased trace's picture still in the archive (#12) — without the walk as well. The in-write check closes the last way to gain a hold without the bytes (#14): a Langfuse id resolved while the project held the body and written after its last ref went, while another project's kept the bytes alive. |
| 27 | **2026-09-26** — **Deletion counts are the project's** (amends #11): `media` and `media_bytes` in every dry run and every confirmed answer — a shrinking window, an erasure, a trace deletion, a project's deletion — count the bodies the project would stop holding (no ref of this project, trace or raw, left) and their decoded bytes, whether or not another project keeps the bytes on disk. Collection is unchanged: a body goes when no project and no raw batch points at it | The figure answered the project that asked with a fact about the others: the same deletion counted a body when no other project held it and did not when one did, so a project holding a file learned, by previewing the deletion of its own trace, whether anybody else on the server held that file. What a project frees is what it stops holding — the figure `GET /api/v1/system` already gives per project (#11), where a body two projects share is in both — and a server with one project, the common case, reads the same numbers as before. |
| 28 | **2026-09-26** — *Accepted; implemented 2026-09-27* — **An upload URL names the key that asked for it** (amends #14, #22): the grant gains the public key of the key that authenticated the `POST` and the instant it was issued; the `PUT` answers `403` "this upload URL is not valid; ask for a new one" when that key is no longer one of the project's keys — checked before a byte of the body is read, and again inside the write. A token without the key, issued before this change, is refused the same way *Refined 2026-09-27, in the sixth review:* the grant carries the key and not the instant it was issued — the instant served the per-project cut-off of #29's first design, which the removed-trace rows replaced; a URL's hour is its signed expiry. | Revoking a key is how an operator stops what a leaked key can do, and an upload URL the key had obtained went on writing for the rest of its hour: bytes of the holder's choosing, stored under the project after the key was gone. The Langfuse SDK asks for the URL and PUTs it in one job (`media_manager.py`, `_process_upload_media_job`), so the URLs alive at the instant of a revocation are the few in transit; refusing them costs those uploads, which the SDK reports through `PATCH` and logs. The route that issues grants admits only a project key (`ingest`), so every grant has a key to name. Refusing the old token shape loses at most the uploads in transit across the upgrade's restart — the loss #22 removed for ordinary restarts, taken once. |
| 29 | **2026-09-26** — *Accepted; implemented 2026-09-27* — **A trace's deletion or erasure voids its upload URLs** (amends #14; the channel's half of spec 035 #10): every transaction that deletes traces or erases a user's traces writes the ids it removes to `media_voided(project_id, trace_id, at)`, and the `PUT` answers `403` — before the body, and again in the write — for a grant whose trace is there and not stored now. The same transaction drops the refs the traces already had, so an upload is either in before the removal and goes with its trace, or refused after it; the null answer writes no ref for such a trace either. The hourly sweep forgets a row once it is older than an upload URL lives, an hour, so no URL issued before the removal outlives it. The retention sweep writes none. A URL for any other trace uploads, before, during and after the deletion. *Refined 2026-09-27, in three reviews:* first one instant per project, set by every such transaction, voiding every URL issued before it — a wall clock stepped back refused every upload until it caught up; then a generation counter per project — which, moved at every chunk of a deletion that runs many, refused every upload that crossed any chunk's commit for as long as the request ran, and, moved only at its two ends, let an upload for a trace a later chunk took land in between. Both voided uploads for traces nobody deleted. The removed ids are what is being voided, so they are what is recorded. *Refined 2026-09-27, in the fifth review:* the `POST` answers `403` too for a removed trace that is not here, on both of its paths — a URL issued after the removal lived an hour from the ask, past the row's hour, and once the sweep had forgotten the row it uploaded into a pending ref for the erased trace; the URL's hour is counted from a clock read before the check; and the sweep forgets a row a minute after the hour, which covers a removal stamped as its chunk runs, before it commits, and the expiry's whole seconds. The rows are written for the traces the transaction removes, read from `traces` before they go *Refined again 2026-09-27, in the sixth review:* a row voids only while it is younger than the window and the slack, whatever the sweep's cadence — the sweep removes rows that void nothing, and a longer `TRACEPAD_SWEEP_INTERVAL` or a restart no longer stretches the hour; and the refusal of the `POST` and the `PUT` for a removed trace says what it is: "this trace was deleted or erased within the hour; its media is not stored". *Refined again 2026-09-27, in the seventh review:* a removal that lands between the `POST`'s check and its null answer's write is refused in the write with the same `403`, not answered as held; and the pass forgets the rows on the wall clock they are stamped and read on, not on its own. *Refined again 2026-09-27, in the eighth review:* the rows a chunk writes as it begins are stamped again as it ends, so an ask that read the pool while the chunk ran is dated no later than the row's hour starts; and the slack is five minutes rather than one, which covers the rest of a group commit after the chunk and any writer slow enough to take minutes. Its price: a trace sent again under a removed id is refused its pictures for an hour and five minutes, four minutes longer. | An upload that landed after its trace was deleted stored the body under a pending ref to a trace that was not there: in no listing, readable by hash for a day (#13), and outliving an erasure that had answered that it was complete. Spec 035 #10 lets a late span re-create a deleted trace because what it creates is a trace — seen, and deletable again; a late upload creates nothing anyone can see. 035 #10 declined to remember deleted ids for good, as one more thing that grows without end and must be swept; these rows live an hour — the lifetime of the URLs they answer for — and the sweep that removes them is the hourly pass that already runs. Their price is one row per removed trace, written in the removing transaction: measured on a chunk of the deletion's own size, about 1 ms on a 100-trace chunk of 22 ms and 18 ms on a full 500-trace chunk of 290 ms, some 5%, and one indexed probe per upload. A trace sent again under the same id within the hour — spec 035 #10's late span — is stored, seen and deletable like any other, and once it is stored its uploads are taken: what is refused is a removed trace that is not here. The Langfuse SDK uploads a span's media before it exports the span, though, so the pictures of a trace sent again within the hour reach the channel while the trace is not here yet, and are refused, and its spans' references resolve to nothing. For a trace that is not here the channel cannot tell one about to come back from one just erased; the erasure's promise is kept (owner decision 2026-09-27). The row is stamped as the removing transaction commits, not as the request began, so a URL issued while a long erasure ran lives no longer than the row that answers for it. Retention is left out: it removes traces older than the window, for which no live upload is in transit. The `POST`'s refusal costs nothing the null answer gave: it wrote no ref for such a trace either. |
| 30 | **2026-09-26** — *Accepted; implemented 2026-09-27* — **Only delivered bytes date a pending ref** (amends #9 and #13): when the project already holds the body, the channel's `POST` answers `uploadUrl: null` as before and writes the ref, settled, if the named trace is stored. If it is not stored yet the ref is pending, as the upload's would be, but dated as the bytes are in the project — the newest of its refs to the body, or its hold's first instant — never from the ask, and no earlier than an hour inside the grace; the spans, when they come, settle it. The `PUT` writes its ref as before — pending until the trace arrives, from the moment the bytes did. *Refined 2026-09-27, in review:* as first written the `null` answer wrote nothing for a trace not yet here, which left the body held by nothing between the answer and the spans: a deletion, an erasure or the retention of the project's last trace to hold it in that window collected it, and the SDK, told it need not send the bytes, never did. Dated as the bytes, the ref closes that window without reopening the one below. *Refined again 2026-09-27, in the second review:* dated as the bytes alone, a ref for bytes the project had held longer than the grace was past it on arrival, and a sweep seconds later could drop it — with the body, once the trace that held it was gone — before the spans came. The floor gives every such ref an hour, longer than the sweep's period. Its price is that naming a hash extends a body no trace claims by that hour: kept alive only by an ask every hour, from a holder of a live key, who could as well send traces that keep it in view *Refined again 2026-09-27, in the sixth review:* the `null` answer for a hash whose pending ref for the same trace is already written gives that ref the same hour, raising its date to an hour inside the grace when it is older — a ref near the end of its grace went, with the body, right after the answer that told the SDK not to send the bytes. *Refined again 2026-09-27, in the seventh review:* the sweep's job deletes a pending ref only if it is still past the grace in its own transaction — a null answer between the pass's read and its job gives the ref its hour, and the ref and the body stay — and settles one only if its trace is stored. | A ref written on the `null` answer for a trace not yet here was pending with a fresh `created_at`, and the sweep drops a pending ref a day after that (#13): naming the same hash for a new trace id once a day kept a body with no trace alive for as long as anyone liked, without sending it again — stored outside every listing and every retention window. Dated only by delivered bytes, a pending ref ages from the moment the bytes arrived, and a body no trace ever claims goes one grace after its last upload, however often its hash is named since. The `null` answer's pending ref has one job — to keep the body until the spans come — and that job takes a ref, not a fresh date. |
| 31 | **2026-09-26** — *Accepted; implemented 2026-09-27* — **A project has at most 10,000 pending refs** (amends #13, #14): a `POST` that would issue an upload URL for a trace the project does not have, and a `PUT` whose ref would be pending, answer `429` with `Retry-After: 60` and "too many media uploads are waiting for their traces" when the project already has that many — the `PUT` before reading the body and again in the write. A trace the project has is never refused: its ref is settled. A constant, not a setting *Refined again 2026-09-27, in the sixth review:* schema 0026 settles the pending refs whose trace is already stored, which before it waited for the day's sweep, so they do not count toward the cap from the upgrade on; and an ingest settles a trace's refs only when it inserts the trace, the only time the trace can have any. | A pending ref is an upload whose spans have not come. The steady state is the uploads of the last few seconds, which the next export settles, and the Langfuse SDK uploads nothing for a span that is not sampled (`span.py`, the `is_recording()` guard). Thousands of them is a client whose spans go somewhere else, or nowhere, while its pictures come here and live a day each out of every listing. The cap turns that into a refusal the SDK retries — `429` is the one client error it retries — and then logs, instead of storage nobody sees. It is not a quota: a key can store bytes through ingest, visibly; it bounds the part nobody sees. Ten thousand is two orders of magnitude over a hundred pictures a second held for the seconds an export takes. *Refined 2026-09-27, in review:* a ref already written — the SDK's retry of an upload that was stored — is not a new one and is not refused; the `null` answer's pending ref (#30) counts like the upload's; and a refusal of a grant this server signed reads the body to its end and drops it before it answers, up to 8 MiB and ten seconds, past which the connection is closed — a client still sending a body the server stopped reading gets a reset rather than the `429` the SDK retries. A token that is missing or forged is still refused having read nothing (#14). *Refined again 2026-09-27, in the second and third reviews:* the refusal is written and flushed first and the body drained after it, so a client hears it before it has sent its body, and the connection is kept; a token no retry can make good — expired, for another id, or from before grants named their key — is refused having read nothing, like a forged one; and a client that sent `Expect: 100-continue` hears the refusal with no `100`, net/http taking its body as never sent. At the cap the refusals — of an upload URL, of the null answer and of the `PUT` — are reads of the pool: no job reaches the writer, so the count runs in the writer only for a ref the pre-check let through. The cap drains as the spans arrive or as the sweep drops refs a day old: a client whose spans never come stays at it until then, `Retry-After: 60` notwithstanding. *Refined 2026-09-27, in the fifth review:* a trace's arrival settles every pending ref to it, those its spans name and those they do not (amends #13, where the sweep did it a day later) — a ref whose trace is here is outside the cap, and the cap no longer fills with the uploads of traces that arrived with no reference to their pictures; and the drain reads up to the length the grant declared, which the ask bounded by the API's body limit, rather than 8 MiB, and follows a failed lookup's `503` or `500` as it follows a refusal |
| 32 | **2026-09-26** — **A download's file name has an extension only from a fixed list** (amends #10): the interface names a downloaded body by the first 12 hex digits of its hash plus the extension its reference's type maps to under *Download names* — images, audio, video, PDF, plain text and JSON — and no extension for any other type: `text/html`, `image/svg+xml`, `application/hta`, `application/x-msdownload`, and everything not listed; the file is saved from a neutral `application/octet-stream` blob, so that a browser adds no extension of its own from the type | The name was built from the type, and the type is the client's claim: a reference declared `application/hta` downloaded as `….hta`, which Windows runs with `mshta` on a double-click — the one place the sandbox of #15 does not reach, because the file has left the browser. The list keeps types whose usual desktop handler is a viewer or a player; HTML and SVG open in a browser from disk, and CSV opens in a spreadsheet that evaluates formulas. A file with no extension is opened by nothing without asking, which is what the server's own attachment name (#15) already does. |
| 33 | **2026-09-26** — A raw batch an erasure rewrites keeps refs only to the media its new body references, and a body left unreferenced is collected (spec 044 #2) | A picture only the erased spans pointed at must go with them. See spec 044 #2. |

## Data contract (schema 0021)

```sql
CREATE TABLE media (
    sha256     TEXT PRIMARY KEY,       -- hex of the decoded bytes
    mime_type  TEXT NOT NULL,
    size       INTEGER NOT NULL,       -- decoded bytes
    body       BLOB NOT NULL,
    created_at INTEGER NOT NULL
) STRICT;

CREATE TABLE media_refs (
    sha256     TEXT NOT NULL REFERENCES media(sha256),
    project_id TEXT NOT NULL,
    trace_id   TEXT NOT NULL,
    PRIMARY KEY (sha256, project_id, trace_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX idx_media_refs_trace ON media_refs(project_id, trace_id);

ALTER TABLE projects ADD COLUMN media TEXT NOT NULL DEFAULT 'store'
    CHECK (media IN ('store', 'placeholder'));
```

Existing traces are not rewritten: the migration adds tables and a column,
and media already inline stays inline until retention takes it. `docs/media.md`
says so.

As shipped the schema is amended by three decisions, and
`internal/store/migrations/0021_media.sql` is its text: `media_refs` gains
`created_at` and `pending`, with a partial index on `created_at WHERE pending
= 1` (#13); a raw batch's refs are `media_raw_refs(sha256, raw_batch_id)`,
dropped with the batch (#12); and `server_keys(name, key, created_at)` keeps
the upload key (#22).

Decisions 25–31 add two more migrations. The first, schema 0022 (#25, #26),
creates `media_holders(sha256, project_id, mime_type, first_at)` — one row per
body and project that holds it, cascading from `media` — and gives
`media_raw_refs` a `project_id` with an index on `(sha256, project_id)`,
backfilling both from the refs, their batches and each body's stored type;
`media_holders` is also indexed on `(project_id, sha256)` for the Langfuse
id's range and a project's own rows.
The second, schema 0026 (#29, #31), adds `media_voided` — the traces removed
within an upload URL's lifetime, keyed by project and trace, indexed by age —
and a partial index of pending refs by project and trace. No body is rewritten by either.

## Download names (#32)

The interface names a downloaded body by the first 12 hex digits of its hash
plus the extension its reference's type — lower-cased and cut at the first
`;` — maps to here, and no extension for any other type.

| Type | Extension |
|---|---|
| `image/png` | `.png` |
| `image/jpeg`, `image/jpg` | `.jpg` |
| `image/gif` | `.gif` |
| `image/webp` | `.webp` |
| `image/avif` | `.avif` |
| `image/heic`, `image/heif` | `.heic`, `.heif` |
| `image/bmp` | `.bmp` |
| `image/tiff` | `.tiff` |
| `audio/mpeg`, `audio/mp3` | `.mp3` |
| `audio/wav`, `audio/x-wav`, `audio/wave` | `.wav` |
| `audio/ogg`, `audio/oga` | `.ogg` |
| `audio/opus` | `.opus` |
| `audio/flac` | `.flac` |
| `audio/aac` | `.aac` |
| `audio/mp4` | `.m4a` |
| `audio/webm`, `video/webm` | `.webm` |
| `video/mp4` | `.mp4` |
| `video/quicktime` | `.mov` |
| `video/mpeg` | `.mpeg` |
| `video/ogg` | `.ogv` |
| `application/pdf` | `.pdf` |
| `text/plain` | `.txt` |
| `application/json` | `.json` |

Deliberately absent: `image/svg+xml`, `text/html`, `application/xhtml+xml`,
`text/xml`, `application/xml`, `text/csv`, scripts and source types,
archives, office formats, `application/octet-stream`, and every type not
listed. The server's attachment name (#15) is unchanged: the first 16 hex
digits, no extension.

## Testing

- Mapper: each of the four shapes extracted at 4 KiB and kept inline below;
  a data URL inside a longer string is not a match (the whole string must be
  the URL); the reference object's fields; two generations sending the same
  image write one `media` row and two refs.
- Raw: a stored raw body carries references; replay re-maps it to the same
  observations.
- Deletion: retention, erasure, trace deletion and purge remove refs and GC
  bodies with no ref left; a body shared by two projects survives one
  project's purge.
- Setting: `placeholder` writes no body and a `stored: false` reference.
- API: `GET /api/v1/media/{sha}` bytes and headers, `404` without a ref in
  the caller's project; the export re-inlines.
- Bridge: the Langfuse SDK itself (the e2e harness already runs it) sends an
  image; the upload round trip succeeds, the trace's reference is rewritten,
  the picture is fetchable; a second identical image gets `uploadUrl: null`.
- UI: thumbnail, chip, not-stored chip (vitest); Playwright on a trace with
  an image; live check in a browser, both themes, 375 px.

Added by the decisions: a raw batch's refs outliving its traces (#12); a
pending ref settled or dropped after the grace, through its index (#13); the
presigned upload refusing a forged token and other bytes (#14); the serving
headers (#15); an object shape keeping its other fields, and extract → inline
giving back the client's payload (#19); a span that overtook its upload read
as the reference (#21); an upload URL accepted after a restart (#22); a JSON
body and the document inside an attribute kept byte for byte, `placeholder`
resolving nothing, a resolved body gone before the write (#23); a body's type
and Langfuse record answered per project, the strip drawing a reference under
its own type, holders matching refs on every path (#25); the scope query's
plan seeking on `(sha256, project_id)` only, a raw-only hold, a resolved hold
gone before the write (#26); deletion counts scoped to the project with the
bytes kept for another (#27); an upload URL refused, decided before the
body, after its key is revoked (#28), and for a trace deleted or erased
within the hour, whether asked for before or after it, and an upload for
another trace accepted; a deletion and an erasure in three chunks refusing a
URL for a trace a later chunk takes and passing one for a trace they leave, an
upload landing between chunks refused or dropped with its trace, the removed
ids remembered an hour and five minutes from their chunk's end, a slow
chunk stamped as it ends, and forgotten after, the
ask for a removed trace refused with a refusal that names the removal, a row
past the hour and the slack voiding nothing before any sweep, and a trace
sent again under a deleted id, once stored, taking its uploads and its null
answer's ref (#29); naming a held hash for a trace not yet
here writing a pending ref dated as the bytes, which keeps the body across the
deletion of the trace that held it, an hour at least for bytes past the
grace, the null answer for its own pending ref giving it that hour too, and
the body going a day after its last upload however often its hash
is named (#30); the pending cap on the ask, the null answer and the upload,
the retry of a stored upload let through, the refusals at the cap — the null
answer's too — never reaching the writer; over a real connection, a refusal heard before the body
is sent, the connection kept once the body is drained, and no `100` for a
client waiting on one; an expired, keyless or other id's token refused having
read nothing; a trace's arrival settling its refs its spans do not name, with
a seek; a body larger than 8 MiB drained whole; a failed lookup's answer
drained like a refusal; 0026 settling the pending refs of stored traces
(#31);
download names (#32).

## Edge cases

- A malformed base64 body under a recognised shape: left inline, a warning
  in the log with the trace id, never a rejected batch.
- An image over the body cap (`TRACEPAD_MAX_BODY_BYTES`) never arrives;
  nothing new here.
- The same content under two MIME types: the first stored type wins; the
  reference carries the type the client declared — per project since #25:
  each project is served the first type it stored, and every reference is
  drawn under its own; within one payload, the first reference of a body is
  the one drawn.
- A browser keeps a body it cached for a project (`immutable`, #7) under the
  type it was served. Only a project that dropped the body and took it again
  under another type can see the old one, and the interface draws by the
  reference's type regardless (#25).
- A span that overtakes its own Langfuse upload: stored with the SDK's
  string, read as the reference once the upload lands (#21).
- A Langfuse upload collected between its resolution and the ingest's write:
  the batch is taken again with the string kept (#23).
- A MIME type is case-insensitive (RFC 2045), and so is a data URL's
  `data:` and `;base64` (#24): `data:Image/PNG;BASE64,…` is extracted, and
  stored and referenced as `image/png`.
- Storing bytes the server already has skips the blob write, so an upload's
  latency can depend on whether the bytes were on the server; observing it
  needs the bytes, and closing it would take a body per project, which #2
  declined (#26).
- An upload URL in transit when its key is revoked, or when its trace is
  deleted or erased, is refused and logged by the SDK, and so is an ask for
  one for that trace within the hour (#28, #29). A trace sent again under a
  deleted id is stored, and once it is its uploads are taken — but the
  Langfuse SDK uploads before it exports the spans, so within the hour the
  pictures of such a trace are refused, and its references show as
  unresolved (#29). URLs
  issued before the upgrade that added #28 are refused once.
- A held hash named for a trace that has not arrived writes a pending ref
  dated as the bytes, with an hour left at least; the trace's spans settle it
  (#30). A body no trace claims goes a day after its last upload, or an hour
  after the last time its hash was named, whichever is later.
- Past 10,000 pending refs the channel answers `429` (#31). A ref counts
  until it is settled — by its trace's arrival — or dropped by the sweep.
