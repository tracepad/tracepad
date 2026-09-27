# Media: images and files in traces

A multimodal call sends the model a picture, and the trace of that call keeps
what was sent. Tracepad takes images and files out of the JSON at ingest,
stores each distinct one **once**, leaves a small reference where it was, and
shows images as images in the interface. It does this for every client — the
Tracepad packages, the Langfuse SDK and plain OpenTelemetry — because it
happens on the server, at the one door every span comes through.

## What is recognised

Four shapes, anywhere in an observation's input, output or metadata or in a
trace's metadata, and nothing else:

| Shape | Who sends it | Example |
|---|---|---|
| A **data URL** — the whole string | OpenAI `image_url`, `file_data`, and most wrappers | `"data:image/png;base64,iVBORw0…"` |
| Anthropic's **base64 source** | Anthropic messages | `{"type": "base64", "media_type": "image/jpeg", "data": "…"}` |
| Gemini's **inline data** | Gemini, Vertex | `{"inline_data": {"mime_type": "image/png", "data": "…"}}` (or `inlineData` / `mimeType`) |
| The GenAI conventions' **blob part** | OpenTelemetry GenAI instrumentations | `{"type": "blob", "mime_type": "image/png", "content": "…"}` |

The scheme, `;base64` and the MIME type are matched in any case, and the type
is stored lower-case. A match is extracted when its decoded size is **at least
4 KiB**. Smaller ones
— icons, tiny thumbnails — stay where they are, because a reference would cost
more than it saves.

What is **not** recognised, on purpose:

- A data URL inside a longer string (`"see data:image/png;base64,… here"`).
  The whole string has to be the URL; a sentence that quotes one is text.
- A base64 string with no MIME type beside it. Guessing what bytes are is how a
  trace's text gets mangled.
- A URL (`https://…/cat.png`). A URL is a string and stays one.

A recognised shape whose data is not valid base64 is left inline, with a
warning in the server log naming the trace; the batch is never rejected for it.

## The reference

Where the media was, the payload now holds:

```json
{"tracepad_media": "3f2a…c91e", "mime_type": "image/png", "size": 48213}
```

`tracepad_media` is the lower-case hex SHA-256 of the decoded bytes, and
`size` is their length. For a data URL the reference replaces the string. For
the three object shapes it replaces the base64 inside the object and nothing
else, so every other field the client sent stays — a blob part's `modality`, a
Gemini part's `thought_signature`. An Anthropic image becomes:

```json
{"type": "image", "source": {"type": "base64", "media_type": "image/jpeg",
  "data": {"tracepad_media": "3f2a…c91e", "mime_type": "image/jpeg", "size": 48213}}}
```

Wherever media was, a reference is.

Every reader returns the reference as it is: the read API, `tracepad traces
show`, and the MCP tools. None of them fetches the bytes — an agent reading a
trace needs to know a picture was sent, not to swallow three megabytes of it.

Existing traces are not rewritten. Media that was stored inline before this
version stays inline until retention takes it.

## Stored once

Bodies live in the same database file as everything else (one file is still
the whole backup), as decoded bytes — three quarters of their base64 — and
**one row per distinct content across the whole server**. A photograph a
pipeline sends to five generations, or a retry sends twice, is one body.

The raw archive is stored the same way: the body kept for replay is the export
as sent, **with the media factored out** — each extracted value replaced by
the same reference — so a picture is not kept a second time for the raw
window. Only those values change: a JSON body keeps its fields, its key order
and its spacing, including inside a JSON document an attribute holds, and
fields a newer client sends that this server does not know yet. See [ingest.md](ingest.md#what-is-kept-and-for-how-long).

Measured on one trace with a 1 MiB photograph sent to five generations:

| | Before | After |
|---|---|---|
| Database growth | 8.6 MB | 1.05 MB |
| Payload rows | 7.0 MB | 1.0 KB |
| Raw body, compressed | 1.38 MB | 536 B |
| Media | — | one 1 MiB body |

The bytes are shared, but what a project sees of them is its own: the type is
the one **this project** first stored the body under, and the Langfuse record's
`uploadedAt` is when this project's hold began. Another project sending the
same bytes first, under another type, changes neither. Within one project the
first stored type is the one served; each reference still carries the type its
own client declared, and the interface draws it under that.

## Reading the bytes

```
GET /api/v1/media/{sha256}
```

answers the bytes with the `Content-Type` this project stored them under,
cached for a year and
`immutable` (the address is the content), and keyed in the browser's cache by
the credential and the project that asked. It answers only a project that
points at the body — from one of its traces or one of its raw batches — and
`404` for any other hash, so a hash that appeared in a log is not a way into
another project's pictures. Whether a project holds a body is looked up by the
hash and the project together; another project's rows are never read, so a
hash only another project holds costs the same to refuse as one nobody holds.
See [api.md](api.md#media).

Every body is served with `X-Content-Type-Options: nosniff` and a sandboxing
`Content-Security-Policy`, and anything that is not an image, audio or video is
sent as an attachment: the bytes are whatever a client declared them to be, and
none of them is ever run as a page of this server.

## Not keeping them: the placeholder setting

A project that must not keep pictures — faces, documents, anything under a
policy — sets `media` to `placeholder`:

```sh
curl -X PATCH "$TRACEPAD_URL/api/v1/projects/$PROJECT" \
  -H "Authorization: Bearer $TRACEPAD_API_KEY" \
  -d '{"media": "placeholder"}'
# or
tracepad retention set --media placeholder
```

or chooses *Keep a placeholder only* in Settings. Media is still taken out of
the JSON, but no body is written, and the reference says so:

```json
{"tracepad_media": "3f2a…c91e", "mime_type": "image/png", "size": 48213, "stored": false}
```

The type and size stay, so a reader still sees that a picture was sent and how
large it was. Switching is not destructive either way: `placeholder` stops
keeping new bodies and deletes none, and `store` starts keeping them again from
the next export.

## How long they are kept

Media follows its traces; there is no separate window. A body lives while a
trace or a raw batch points at it, and goes with the last one:

- the retention sweep deletes a trace's refs with the trace, and a raw
  batch's with the batch, each in the same transaction;
- erasing a user's data and deleting traces delete the refs of the traces they
  take; an erasure also rewrites the raw batches that held the user's spans,
  and a rewritten batch keeps refs only to the bodies its new body still
  points at;
- purging a deleted project deletes every ref it still has.

Each of those collects the bodies nothing points at any more, in the same
transaction. A body another project still points at survives — refs carry the
project, bodies are shared. Every dry run and every confirmed deletion names
the bodies the project would stop holding (`media`, `media_bytes`) — whether
or not another project keeps the same bytes on disk, so a count never says
what any other project holds — and `GET /api/v1/system` reports what the
project's media costs:

```json
"media": {"setting": "store", "count": 212, "bytes": 318455112}
```

Raw bodies outlive traces by design (see [retention.md](retention.md#what-outlives-what)),
so a picture a deleted trace pointed at stays while a raw batch still points
at it, and goes when that batch expires — the same archive position the rest
of the raw body takes. An erasure is the exception: it takes the user's spans
out of the raw batches too, so a picture only their spans pointed at goes in
the same request.

## The Langfuse SDK's media channel

The Langfuse SDK takes media out of a payload itself and uploads it through
its own channel before it exports the span. Tracepad serves that channel, so a
picture sent through the bridge is kept like any other:

| Call | What it does |
|---|---|
| `POST /api/public/media` | The SDK asks where to upload, for a `traceId` of 32 lower-case hex digits. `mediaId` is the SDK's own derivation of the hash, which it checks. `uploadUrl` is `null` when this project already holds the body — the second identical picture sends nothing. For a trace that has not arrived its ref then waits for the trace's spans like an upload's, counted as old as the bytes are, not from the ask. `429` with `Retry-After: 60` when the project has 10,000 uploads waiting for their traces and this one would be another; a trace the project already has is never refused. `403` "this trace was deleted or erased within the hour; its media is not stored" for a trace deleted or erased within the hour that is not stored now. |
| `PUT` the `uploadUrl` | The bytes. The URL is presigned: the SDK sends no credential with this request, so the URL carries a signed token instead, good for an hour — across a restart, because the key it is signed with is kept in the database. The body must match the declared length and SHA-256, or nothing is stored. `403` when the key that asked for the URL has been revoked since, or its trace was deleted or erased within the hour; `429` at the cap above, for an upload whose trace has not arrived — not for the retry of one already stored. Both are decided before the body is read and answered at once; the body is then read to its end, up to the length the `POST` declared, and dropped — as it is after a `503` when the check could not be made — so that the SDK reads the status instead of a reset connection, on a connection it can keep. A client that sent `Expect: 100-continue` hears the refusal before it sends anything. |
| `PATCH /api/public/media/{mediaId}` | The SDK's report on the upload; a failure is logged. |
| `GET /api/public/media/{mediaId}` | The Langfuse record of a body, with a `url` to `GET /api/v1/media/{sha256}` — which, like every read, needs a key of the project. |

The span arrives carrying the SDK's reference string —
`@@@langfuseMedia:type=image/png|id=…|source=base64_data_uri@@@` — and ingest
rewrites it to Tracepad's reference when the body is stored, so a bridged trace
looks like any other in the interface.

A span can overtake its own upload — a script that sets an input and flushes
at once exports the span before the SDK's upload thread has PUT the bytes. The
span is then stored with the string, and once the upload lands every read —
the API, the interface, the CLI, MCP — answers the reference in its place, for
the trace the upload was made for. The stored payload is not rewritten, and the
raw archive and the export keep the string as the client sent it.

Under the `placeholder` setting nothing is uploaded and no string is resolved
— not even to a picture the project stored before it switched, which would
otherwise gain a new trace to live for — and the string is left as the client
wrote it, as the evidence of what the client meant.

A body only another project holds is still asked for: skipping the upload on a
hash alone would let any project adopt another's picture by naming it. If the
SDK uploads for a trace whose spans never arrive, the ref is dropped by the
hourly sweep a day after the upload, and the body with it — naming the same
hash again for another trace that has not arrived writes a ref as old as the
bytes — or, for bytes the project has had for most of a day or more, one that
has an hour left — so it keeps the body an hour longer at most. That ref keeps
the body while the trace's spans are on their way, even if the trace that held
it is deleted meanwhile. The sweep looks only at the refs the channel wrote
that are still waiting for their trace, so its cost does not grow with the
pictures a project keeps, and a project may have at most 10,000 of them. A
waiting ref leaves the count when its trace arrives, or when the sweep
drops it a day after its upload; so a client whose spans go somewhere else
meets the cap and stays at it — its uploads for new traces answered `429` —
until the spans come or its oldest uploads age out, however often the SDK
retries.

An upload URL names the key that asked for it. Revoking the key voids the URLs
it obtained, and deleting or erasing a trace voids the URLs for that trace for
the next hour, the time a URL lives, and refuses new ones for it — a picture that landed after its trace
was gone would be stored under a ref to nothing. The SDK asks for a URL and
PUTs it in one go, so what this refuses is the uploads in transit for the
removed traces; the SDK logs them. Uploads for every other trace go on, during
a deletion as before and after it. A trace sent again under a deleted id is a
trace like any other once it is stored, and its uploads are taken from then —
but the Langfuse SDK uploads a span's pictures before it exports the span, so
within the hour after the deletion the pictures of such a trace are refused
while it is not here yet, and its spans show their references unresolved: for
a trace that is not here, a trace coming back cannot be told from one just
erased, and the erasure's promise is kept. URLs issued before the
upgrade that introduced this are refused once.

## In the interface

In an observation's input and output panel, a reference to an image is a
**thumbnail** — click it to open the full image in a new tab — with its type
and size under it, drawn under the type its own reference declared. Any other
file is a **chip** with its type and size that downloads it, named by the
start of its hash with an extension only for common image, audio, video, PDF
and text types: anything else, `text/html` and `image/svg+xml` included, is
saved with none, so a file that declared itself something a desktop would run
is never saved as one. A reference with `"stored": false` is a muted chip
reading *not stored (project setting)*. The JSON below still shows every
reference as data. See [ui.md](ui.md#payloads).

## The way out

`tracepad export --otlp`, and `GET /api/v1/raw/{id}` beneath it, put the bytes
back, each in the shape it came in: the base64 in an object's slot, a data URL
where a whole string was. A trace moved to another backend is whole, and
posted into another Tracepad it is extracted again to the same reference. See [export.md](export.md).
