# Spec 041 — Media: images and files in traces, stored once

**Status:** 🟡 DRAFT
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
  raw body before it is stored (Decisions 1, 4, 5).
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

## Edge cases

- A malformed base64 body under a recognised shape: left inline, a warning
  in the log with the trace id, never a rejected batch.
- An image over the body cap (`TRACEPAD_MAX_BODY_BYTES`) never arrives;
  nothing new here.
- The same content under two MIME types: the first stored type wins; the
  reference carries the type the client declared.
