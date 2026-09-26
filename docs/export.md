# Taking the data out

Tracepad keeps every export body it accepts, byte for byte, exactly as the
client sent it. `tracepad export --otlp` replays that archive into any OTLP
receiver — another Tracepad, an OpenTelemetry Collector, a vendor's endpoint —
or writes it to a directory you can carry away.

The bytes that leave are the bytes that arrived. Every dialect, every attribute
this server's mapper never claimed, every quirk of the SDK that sent them: all
of it goes, because none of it was thrown away.

```sh
# Into a collector
tracepad export --otlp --to http://collector:4318/v1/traces

# Onto disk, to move or to keep
tracepad export --otlp --dir ./tracepad-export
```

## What it can and cannot carry

The archive is bounded by the **raw retention window**
(`raw_retention_days`, see [retention.md](retention.md)). A trace older than
that window still exists — its rows are in the database and the UI shows it —
but the body it arrived in is gone, and this command never invents one. It
replays what it has and tells you what it could not:

```
$ tracepad export --otlp --to http://collector:4318/v1/traces
12400 batches, 3.1 GiB
  covering   2026-08-06 04:12:19 .. 2026-09-05 09:44:02
  not covered 214 traces started before the archive begins
--after MTc4NzczODQwMDAwMDAwMDAwMDoxMjQwMA
```

`not covered` is a lower bound, and deliberately so: it counts the traces whose
earliest span started before the oldest batch arrived. A trace exported late —
recorded on Monday, flushed on Wednesday — lands after the batch line and is
not counted, so the real gap can only be smaller than the number shown.

`tracepad system` shows the same numbers without sending anything, and
`--dry-run` shows them beside how many batches the window holds:

```
$ tracepad export --otlp --to http://collector:4318/v1/traces --dry-run
12400 batches in the window
  nothing was sent
  not covered 214 traces started before the archive begins
```

Run that before pointing the command at a receiver, and before shortening
`raw_retention_days`.

The number is the **window's**, not the remainder. `--since`/`--until` narrow
it; `--after` does not, because the API counts what the filters match and never
what is left after a cursor — one meaning for `count` across the whole read API.
A dry run resumed with `--after` says so rather than letting the number be read
as what it is about to send:

```
$ tracepad export --otlp --to … --after MTc4… --dry-run
12400 batches in the window; the resume starts inside it, so fewer will be sent
```

With `--json` the field is `matching_window` for the same reason, beside
`resuming: true`. An archive larger than the count's cap reports `100000+`.

If the server has never kept bodies — `TRACEPAD_STORE_RAW` off since day one —
the command exits 1 saying so, and reports every trace as uncovered. There is
nothing to replay and no flag that changes it.

**The archive is what arrived, and nothing since has changed it.** Deleting
traces and erasing a user's data remove parsed rows and leave the batches
alone, so the export still carries the spans of a trace deleted yesterday and
of a user erased this morning, until the raw window takes the batches that
held them. An export taken to answer "give me my data" is right to include
them; one taken after an erasure, to seed another store, carries the erased
person with it. [retention.md](retention.md#what-this-means-for-a-data-subject-request)
says how to bound that today.

## Into a receiver

```sh
tracepad export --otlp \
  --to https://otlp.example.com/v1/traces \
  --header "authorization=Bearer $RECEIVER_TOKEN" \
  --gzip
```

Each batch is posted under the `Content-Type` it was received in, which is what
makes it acceptable to a receiver that took it once. `--header k=v` repeats,
and its value is sent as written. Header names are not case-sensitive: two
`--header`s naming the same one in different case are one header, and the last
one on the command line wins.

The receiver's credentials go in `--header` and nowhere else.
`OTEL_EXPORTER_OTLP_HEADERS` is **not** read: on a machine that sends traces to
Tracepad it holds your Tracepad key, and that key is not the receiver's
business. Whenever it is set, the command says it ignored it.

`Content-Type`, `Content-Encoding`, `Content-Length` and `Host` are not yours to
set: each batch goes out under the type and encoding it has, and `--gzip` is how
to compress it.

Into Langfuse, whose OTLP endpoint takes a Langfuse project's public and secret
key as Basic auth:

```sh
LF_PUBLIC_KEY=pk-lf-…   # from Langfuse's project settings
LF_SECRET_KEY=sk-lf-…
tracepad export --otlp \
  --to https://cloud.langfuse.com/api/public/otel/v1/traces \
  --header "authorization=Basic $(printf '%s' "$LF_PUBLIC_KEY:$LF_SECRET_KEY" | base64 | tr -d '\n')"
```

These are **Langfuse's** keys, `pk-lf-…` and `sk-lf-…`. On a machine whose
Langfuse SDK sends traces to Tracepad, `LANGFUSE_PUBLIC_KEY` and
`LANGFUSE_SECRET_KEY` hold *Tracepad's* keys — do not use them here. `tr -d
'\n'` is there because GNU `base64` wraps its output, and a line break is not
something a header can carry.

`--gzip` compresses on the wire. It is off by default because whether the
receiver supports it is the one thing this command cannot know, and the bytes
are on a local link more often than not.

**The receiver is not authenticated by your project key.** The key opens *this*
server's archive; the receiver's credentials are whatever it wants, and they go
in `--header`. A Tracepad key on its way to the receiver is refused before
anything is sent. The command reads every header name and value and the `--to`
URL whole — as written, percent-decoded, and with anything base64 decoded — so
the scheme does not matter: a bearer token, a Basic pair, a query parameter, a
path segment. The keys of your Tracepad this machine holds are refused
outright: the one the command reads with (`--key` or `TRACEPAD_API_KEY`),
`TRACEPAD_API_KEY` even when `--key` overrides it, `TRACEPAD_ADMIN_TOKEN`,
`LANGFUSE_SECRET_KEY`, and any `tp-sk-…` in `OTEL_EXPORTER_OTLP_HEADERS`:

```
$ tracepad export --otlp --to https://otlp.example.com/v1/traces \
    --header "authorization=Bearer $TRACEPAD_API_KEY"
tracepad: --header Authorization carries a key of your Tracepad that this machine holds (--key, TRACEPAD_API_KEY, TRACEPAD_ADMIN_TOKEN, LANGFUSE_SECRET_KEY or OTEL_EXPORTER_OTLP_HEADERS); the receiver would get admin access to your project. --allow-tracepad-key does not change that: give the receiver its own credentials
```

An admin token shorter than 16 characters is looked for only as a whole word
in the text as written, and not in the `--to` host: an operator's short token
would otherwise match a compose service of the same name, or a few bytes of a
receiver's base64. A `--header` typed wrong — curl's `Name: value`, say — is
refused without repeating it, so a key in it does not reach a CI log.

Any other key starting `tp-sk-` is refused the same way, with a message that
names the one way through.

The one receiver whose own credentials are such a key is another Tracepad —
moving to a new server, say. Give it *that* server's key, and say so with
`--allow-tracepad-key`. The flag lets through a `tp-sk-` that is none of the
keys above; those, the source's, are refused with or without it, since no
receiver has a use for them:

```sh
tracepad export --otlp \
  --to https://new-tracepad.example.com/v1/traces \
  --header "authorization=Bearer $NEW_SERVER_KEY" \
  --allow-tracepad-key
```

### When a receiver says no

| The receiver answers | What happens |
|---|---|
| `2xx` | The batch is delivered. |
| `2xx` with `partialSuccess` | Delivered. The receiver's message is logged and counted — it has the bytes, and what it did with them is its business. |
| `429`, `5xx`, or nothing at all | Retried, backing off 1s → 2s → 4s → … capped at 30s, six attempts. |
| any other `4xx` | The export **stops**. |

A receiver that says `503` is asking for time, and a nightly export that died at
03:14 for a fifteen-second blip is the wrong outcome. A receiver that says `400`
or `413` is describing the batch, and sending the next one would leave a hole
you would not find until you went looking for a trace.

So it stops, with the batch id, the receiver's status and body, and the cursor
to continue from:

```
$ tracepad export --otlp --to http://collector:4318/v1/traces
tracepad: batch 8814 was refused: 413 payload too large
1204 batches, 512.4 MiB
  covering   2026-08-06 04:12:19 .. 2026-08-09 22:03:55
  not covered 214 traces started before the archive begins
  stopped at batch 8814: 413 payload too large
--after MTc4NzczODQwMDAwMDAwMDAwMDo4ODEz
```

Fix what the receiver complained about, then:

```sh
tracepad export --otlp --to http://collector:4318/v1/traces \
  --after MTc4NzczODQwMDAwMDAwMDAwMDo4ODEz
```

The cursor resumes **at** the batch that failed, not after it. Re-sending a
batch a receiver already took is safe — OTLP receivers upsert by span id, this
one included — so the resume point errs towards sending twice rather than
towards a gap.

Exit codes: `0` finished, `1` stopped, `2` you typed something wrong.

## Onto disk

```sh
tracepad export --otlp --dir ./tracepad-export
```

One file per batch, named `<received_at_ms>-<id>.pb` — or `.json` for a batch
that arrived in the OTLP/JSON encoding — so that a plain `ls` is in replay
order. Beside them, `manifest.jsonl`: one line per batch, the listing row as the
API returns it, appended as each file lands.

```
$ ls tracepad-export | head -3
1788220800000-1.pb
1788220800001-2.pb
1788220800002-3.json

$ head -1 tracepad-export/manifest.jsonl
{"id":1,"received_at":"2026-09-01T00:00:00Z","dialect":"langfuse",
 "content_type":"application/x-protobuf","content_encoding":"","size_bytes":1274}
```

The names carry the order and the extension carries the encoding, so replaying
the files by hand needs no parsing at all:

```sh
for file in tracepad-export/*.pb tracepad-export/*.json; do
  [ -e "$file" ] || continue
  case "$file" in
    *.json) type=application/json ;;
    *)      type=application/x-protobuf ;;
  esac
  curl -sS -X POST http://collector:4318/v1/traces \
    -H "Content-Type: $type" -H "Authorization: Bearer $TOKEN" \
    --data-binary "@$file" >/dev/null
done
```

That replays `.pb` before `.json` rather than in strict arrival order. When the
order matters — a receiver that does not upsert — walk the manifest, which is in
arrival order by construction:

```sh
jq -r '"\(.id) \(.content_type)"' tracepad-export/manifest.jsonl |
while read -r id type; do
  curl -sS -X POST http://collector:4318/v1/traces \
    -H "Content-Type: $type" -H "Authorization: Bearer $TOKEN" \
    --data-binary "@$(ls tracepad-export/*-"$id".*)" >/dev/null
done
```

The directory must be empty unless you pass `--after`, so that two exports never
interleave one manifest. A resume appends to the manifest it left.

`--header` and `--gzip` apply to `--to` and are refused with `--dir`: a file
holds the body as it is.

## Windows and pieces

```sh
# Yesterday's traffic only
tracepad export --otlp --dir ./monday --since 2026-09-01T00:00:00Z --until 2026-09-02T00:00:00Z

# The last hour
tracepad export --otlp --to http://collector:4318/v1/traces --since 1h
```

`--since` and `--until` take a duration counted back from now (`1h`, `30m`) or
an RFC 3339 instant. The window is half-open — `since` inclusive, `until`
exclusive — so walking a timeline a day at a time never replays a batch twice.

A cursor from a previous run and a `--since` that contradicts it are refused
rather than reconciled: the cursor says where to start and the window says where
to start, and guessing which one you meant is worse than asking.

## Moving to another Tracepad

The receiver of a replay is `POST /v1/traces`, and this server is one:

```sh
TRACEPAD_URL=http://old:4318 TRACEPAD_API_KEY=tp-sk-old… \
  tracepad export --otlp \
    --to http://new:4318/v1/traces \
    --header "authorization=Bearer tp-sk-new…"
```

Everything re-resolves through the receiving server's mapper: the sessions, the
columns, the payloads, the trace and span ids. Two things to know:

- **Datasets and runs do not move.** The export moves traces. A trace stamped
  with `tracepad.run_id` whose run does not exist on the new server is an orphan
  there, counted as such in `GET /api/v1/system` (see
  [datasets.md](datasets.md)). Push the datasets and open the runs first if you
  want the links to survive.
- **Scores and prompts do not move either.** They were never in an OTLP body.
  `tracepad scores ls --json` and `tracepad prompts get --json` are their export;
  see [cli.md](cli.md).

## The endpoints underneath

The command is a client of two endpoints, and so can anything else be — a
backup job, a script, a second Tracepad. They are project-scoped reads under the
project's own keys, with no admin token involved — and, like every other read,
open to every member of the project signed in to the interface, viewers
included:

| Method | Path | |
|---|---|---|
| `GET` | `/api/v1/raw` | The batches, oldest first, keyset-paged |
| `GET` | `/api/v1/raw/{id}` | One body, in the `Content-Type` it arrived in |

See [api.md#the-raw-archive](api.md#the-raw-archive).

**Media comes back inline.** Ingest keeps each image or file once and leaves a
reference in the archived body ([media.md](media.md)); the body endpoint puts
the bytes back where each reference is, so a batch leaving for another backend
is whole: the base64 in the slot of an Anthropic source, a Gemini part or a
GenAI blob, and a data URL where a whole string was — each shape as the client
sent it, which a second Tracepad extracts to the same reference again. A reference whose
body was not stored (the `placeholder` setting) stays a reference. `size_bytes`
in the listing is the stored body's length, before the media is put back.

## A receiver that only takes protobuf

If some of your batches arrived as OTLP/JSON (see [ingest.md](ingest.md)) and
the receiver you are replaying into takes only protobuf, those batches will be
refused and the export will stop on the first one. The archive keeps what
arrived and never converts it, because a conversion at ingest would bake a
converter's bugs into the only copy.

The answer is `--dir`, and a conversion of the `.json` files by hand — the
manifest says which they are — before replaying them.

## What is *not* here

- **Traces older than the raw window.** Their rows survive; their bodies do
  not, and nothing here synthesizes an OTLP body from parsed rows. That would be
  a second, lossy exporter, and mixing the two in one command would hand the
  receiver a corpus that means two different things by row.
- **An `import` command.** The receiver of a replay is `POST /v1/traces`.
- **OTLP over gRPC.**
