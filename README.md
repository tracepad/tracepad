# Tracepad

A lightweight, self-hosted store and viewer for LLM and agent application
traces. Single binary, embedded database, OTLP-native ingestion — point any
OpenTelemetry-instrumented app at it and browse your traces.

**Status: pre-release.** Under active development; not ready for use yet.

New here? [docs/quickstart.md](docs/quickstart.md) goes from nothing to a
trace on screen.

## What it is built for

One binary over one embedded database, sized for the traces of a team rather
than of a fleet. Up to roughly **10 GB of data and a million spans a day** is
the range Tracepad is written to serve well, on one ordinary machine and with
nothing else to run. Past that you are looking for a platform on a column
store — ClickHouse and its neighbours — and the honest answer is that this is
not that.

Rough figures, measured on a synthetic corpus of about 2,000 traces:

- **Binary** — 15.6 MiB, 6.2 MiB gzipped.
- **Memory** — ~27 MiB resident at rest, ~63 MiB under ingest.
- **Ingest** — ~1,900 spans/s from one sequential client.
- **Disk** — ~5 KB per trace, the raw OTLP archive included.
- **Reads** — 8–23 ms per API query.

Distrust the disk figure first: that corpus has short payloads, and on real
prompts and completions it is the payloads and the raw batches that the file
is made of. Size it against your own traffic, and give it a retention
window — see [docs/retention.md](docs/retention.md).

## Sending traces

Tracepad accepts standard OTLP/HTTP on `/v1/traces`, and the same endpoint
under the Langfuse SDK's path. Connecting an instrumented application is an
endpoint and a header:

```sh
export OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://localhost:4318/v1/traces
export OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer tp-sk-…"
```

See [docs/ingest.md](docs/ingest.md) for the endpoints, the auth schemes, the
attribute conventions Tracepad understands, and the ingest configuration.

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

## Browsing traces

The same binary serves a web interface on the same port. The first run prints
a link that is already signed in:

```
  # Web interface, signed in with that key
  http://localhost:4318/#key=tp-sk-…
```

A filterable trace list; a trace as its observation tree with the payloads of
whichever span you are looking at; sessions rolled up from the traces that
named them; volume, cost, latency and errors over time; and settings, where a
project's retention, keys and data live — with an administration section that
unlocks with the admin token for project lifecycle.

It is a client of the read API like the CLI and the MCP server, it keeps what
you are looking at in the URL, and it makes no request to any origin but your
own server — fonts and charts included. See [docs/ui.md](docs/ui.md).

## Forgetting

Nothing is deleted until you say so, and then it is deleted on a schedule
rather than by hand:

```sh
tracepad retention set --days 90       # traces, counted from when they arrived
tracepad users rm-data user-4711       # one user, everywhere it is queryable
```

An hourly sweeper removes what has expired, in chunks through the same writer
as ingest, and hands the freed pages back so the file shrinks. Projects, keys
and retention windows are managed over the same API; every destructive call is
a dry run until you echo the name of what it destroys, and deleting a project
is undoable for a week. See [docs/retention.md](docs/retention.md) and
[docs/admin.md](docs/admin.md).

## License

Apache-2.0. See [LICENSE](LICENSE); third-party attributions are in
[NOTICE](NOTICE).
