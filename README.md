# Tracepad

A lightweight, self-hosted store and viewer for LLM and agent application
traces. Single binary, embedded database, OTLP-native ingestion — point any
OpenTelemetry-instrumented app at it and browse your traces.

**Status: pre-release.** Under active development; not ready for use yet.

New here? [docs/quickstart.md](docs/quickstart.md) goes from nothing to a
trace on screen.

## Getting it

In Docker, which needs nothing else installed:

```sh
docker run -d --name tracepad -v tracepad:/data -p 127.0.0.1:4318:4318 \
  ghcr.io/tracepad/tracepad
docker logs tracepad          # the first run prints the keys, once
```

The port is published on this machine only: Tracepad speaks plain HTTP, and
serving it to anyone else is a job for a TLS proxy in front
([docs/docker.md](docs/docker.md#serving-over-tls)).

Or as a binary: the archives for Linux, macOS and Windows on
[Releases](https://github.com/tracepad/tracepad/releases) — one file, nothing
to install alongside it. Both carry the web interface; both keep everything in
one directory you choose. See [docs/docker.md](docs/docker.md) for the volume,
the permissions and upgrades.

From a checkout, `make build` produces the same binary and `make image` the
same image.

## What it is built for

One binary over one embedded database, sized for the traces of a team rather
than of a fleet. Up to roughly **10 GB of data and a million spans a day** is
the range Tracepad is written to serve well, on one ordinary machine and with
nothing else to run. Past that you are looking for a platform on a column
store — ClickHouse and its neighbours — and the honest answer is that this is
not that.

Rough figures, measured on a synthetic corpus of about 2,000 traces:

- **Binary** — 15.6 MiB on `darwin/arm64`, 6.2 MiB gzipped; about half a MiB
  more for `linux/amd64`.
- **Memory** — ~27 MiB resident at rest, ~63 MiB under ingest.
- **Ingest** — ~1,900 spans/s from one sequential client, which is headroom
  rather than the ceiling: the envelope above is set by what the file and the
  queries carry, not by what the intake keeps up with.
- **Disk** — ~5 KB per trace, the raw OTLP archive included
  (`TRACEPAD_STORE_RAW`, on by default).
- **Reads** — 8–23 ms per API query.

Distrust the disk figure first: that corpus has short payloads, and on real
prompts and completions it is the payloads and the raw batches that the file
is made of. The read and memory figures were taken at that corpus's size too,
and they move with it — though `/api/v1/stats` answers closed hours from an
hourly rollup, so a month's chart costs about what a day's does, and it keeps
answering about data the retention window has since deleted. Size all of it against
your own traffic, and give it a retention window — near the top of this
envelope that is not optional. The two ceilings are a stock and a flow, and
they are counted in different units: the flow is spans, the disk figure above
is per trace. At a handful of spans a trace, a million spans a day is on the
order of a gigabyte a day, and more once the payloads are real ones — so the
stock is spent in days, not months. Nothing is deleted until you set that
window, which is a default for a small deployment rather than for this range:
see [docs/retention.md](docs/retention.md).

## Sending traces

Tracepad accepts standard OTLP/HTTP on `/v1/traces`, and the same endpoint
under the Langfuse SDK's path. Connecting an instrumented application is an
endpoint and a header:

```sh
export OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://localhost:4318/v1/traces
export OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer tp-sk-…"
```

In Python and in Node there is also a package — a thin layer over the same
OpenTelemetry SDK, which adds an exporter to the provider your application
already has rather than replacing it:

```sh
pip install tracepad        # or: npm install tracepad @opentelemetry/api
```

```python
import tracepad
tracepad.init()

@tracepad.observe
def answer(question: str) -> str: ...
```

In Go, the same package with the shape of the OTel API — a context in, a
context out (`go get github.com/tracepad/tracepad/sdk/go`).

See [docs/ingest.md](docs/ingest.md) for the endpoints, the auth schemes, the
attribute conventions Tracepad understands, and the ingest configuration,
[docs/sdk-python.md](docs/sdk-python.md) for the Python package,
[docs/sdk-js.md](docs/sdk-js.md) for the Node one and
[docs/sdk-go.md](docs/sdk-go.md) for the Go one.

## Scores and prompts

The same binary takes quality scores for your traces and serves the prompts
your application runs on, over a plain JSON API — so an eval loop can grade
yesterday's traces and a deploy can be a label move:

```sh
curl -H "Authorization: Bearer tp-sk-…" localhost:4318/api/v1/scores \
  -d '{"trace_id":"4f8c…","name":"helpfulness","value":0.9}'
curl -H "Authorization: Bearer tp-sk-…" \
  "localhost:4318/api/v1/prompts/summarize?label=production"
```

See [docs/scores.md](docs/scores.md) and [docs/prompts.md](docs/prompts.md).

## Reading traces back

Everything Tracepad knows is readable over HTTP, and the read API is written
for agents first: flat JSON, cursor pagination, a byte budget so a response
never quietly eats a context window, and task shortcuts instead of only REST
listings.

```sh
curl -H "Authorization: Bearer tp-sk-…" \
  "localhost:4318/api/v1/traces/last?status=error&expand=io"
```

The question people actually arrive with is a piece of text — an error message
somebody pasted, a sentence the model should not have said — so the listing
searches what the observations carried, and every row says where it matched:

```sh
curl -H "Authorization: Bearer tp-sk-…" \
  "localhost:4318/api/v1/traces?q=%22refund+failed%22"
```

The rest of what the SDKs already send is a filter too — which deployment a
trace ran in, what kind of step it contains, which prompt produced an answer:

```sh
curl … "localhost:4318/api/v1/traces?release=2026.8.30&type=tool"
curl … "localhost:4318/api/v1/traces?prompt=support-answer@7"
```

The same question, three ways:

```sh
tracepad traces last --error --full     # the CLI, in the same binary
```

```json
{"mcpServers": {"tracepad": {"type": "http", "url": "http://localhost:4318/mcp",
  "headers": {"Authorization": "Bearer tp-sk-…"}}}}
```

The CLI and the MCP server are HTTP clients of that API and contain no logic of
their own, so all three return the same bytes. See [docs/api.md](docs/api.md),
[docs/cli.md](docs/cli.md) and [docs/mcp.md](docs/mcp.md).

The MCP surface reads and nothing else — administration is deliberately not
reachable as a tool.

A coding agent gets the order of work — start from the trace, read the payload
before guessing at the prompt, dry-run anything destructive and ask — as a
skill that ships inside the binary, so its version is always the server's:

```sh
tracepad skills install      # into ~/.claude/skills; --project or --dir for elsewhere
```

See [docs/agents.md](docs/agents.md).

## Browsing traces

The same binary serves a web interface on the same port. The first run prints
the link that creates the account you will sign in with:

```
This server has no owner yet. Create the first one — it takes an email and a
password, and nothing is written down anywhere but this database:

  http://localhost:4318/setup#token=…
```

People sign in with an email and a password; keys stay what they are, for
programs. An owner runs the server and invites everyone else into specific
projects as a `viewer` or an `editor` — see
[docs/accounts.md](docs/accounts.md).

A filterable, searchable trace list — a hit shows the text it matched under
the row and opens the panel on the observation it came from; a trace as its
observation tree with the payloads of whichever span you are looking at;
sessions rolled up from the traces that named them; volume, cost, latency and
errors over time; and settings, where a project's retention, keys and data
live — with an administration section that unlocks with the admin token for
project lifecycle.

It is a client of the read API like the CLI and the MCP server, it keeps what
you are looking at in the URL, and it makes no request to any origin but your
own server — fonts and charts included. See [docs/ui.md](docs/ui.md).

## Forgetting

Nothing is deleted until you say so, and then it is deleted on a schedule
rather than by hand:

```sh
tracepad retention set --days 90       # traces, counted from when they arrived
tracepad users rm-data user-4711       # one user's traces, not the raw archive
```

An hourly sweeper removes what has expired, in chunks through the same writer
as ingest, and hands the freed pages back so the file shrinks. The statistics
survive it: the hourly rollup keeps answering about a window whose traces are
gone, until you give it a window of its own with `--stats-days`. Projects, keys
and retention windows are managed over the same API; every destructive call is
a dry run until you echo the name of what it destroys, and deleting a project
is undoable for a week. An erasure takes the parsed data and leaves the raw
OTLP archive and the freed bytes in the file behind; the
[data-subject section](docs/retention.md#what-this-means-for-a-data-subject-request)
says what that means and what to do about it. See
[docs/retention.md](docs/retention.md) and [docs/admin.md](docs/admin.md).

## Images and files

A picture a model was sent is stored once, not as base64 in every payload that
carried it. Ingest takes data URLs, Anthropic and Gemini inline bodies and
GenAI blob parts out of the JSON — for every client, the Langfuse SDK's own
upload channel included — keeps each distinct file one time, and leaves a
small reference in its place; the trace view shows the image. A project that
must not keep pictures keeps only a placeholder. See
[docs/media.md](docs/media.md).

## Taking the data out

Every export body Tracepad accepts is kept as it arrived — with media stored
once beside it and put back on the way out — and one command replays it into any OTLP receiver — another Tracepad, a Collector, a vendor's
endpoint — or onto disk:

```sh
tracepad export --otlp --to http://collector:4318/v1/traces
tracepad export --otlp --dir ./tracepad-export
```

In arrival order, resumably, and it reports what it could not cover: a trace
older than the raw retention window has rows but no body, and nothing here
invents one. See [docs/export.md](docs/export.md).

## License

Apache-2.0. See [LICENSE](LICENSE); third-party attributions are in
[NOTICE](NOTICE). Every release archive and the image also carry
`THIRD_PARTY_NOTICES`, the licences of the Go modules and npm packages the
binary is built from (`make notices` writes it after `make ui`).
