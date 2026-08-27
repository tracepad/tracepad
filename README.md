# Tracepad

A lightweight, self-hosted store and viewer for LLM and agent application
traces. Single binary, embedded database, OTLP-native ingestion — point any
OpenTelemetry-instrumented app at it and browse your traces.

**Status: pre-release.** Under active development; not ready for use yet.

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

## License

Apache-2.0. See [LICENSE](LICENSE); third-party attributions are in
[NOTICE](NOTICE).
