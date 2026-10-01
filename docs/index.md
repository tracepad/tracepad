# Tracepad documentation

Tracepad is a lightweight, self-hosted store and viewer for LLM and agent
application traces: one binary, an embedded database, OTLP-native ingestion.
Point any OpenTelemetry-instrumented app at it and browse what it did.

New here? [Quickstart](quickstart.md) goes from nothing to a trace on screen.

## Get started

- [Quickstart](quickstart.md): the first trace on screen.
- [Installing the binary](install.md): download, verify, run as a service,
  upgrade, back up.
- [Docker](docker.md): the image, its volume, TLS in front, upgrades.
- [Configuration](configuration.md): every environment variable.

## Send traces

- [Sending traces](ingest.md): the endpoints, the auth schemes, the attribute
  mapping.
- [Python](sdk-python.md), [Node](sdk-js.md) and [Go](sdk-go.md): the
  packages, each a thin layer over OpenTelemetry.
- [Media](media.md): images and files in traces.

## Use it

- [Web interface](ui.md), [Scores](scores.md), [Prompts](prompts.md),
  [Datasets and runs](datasets.md), [Annotation queues](annotation.md),
  [Quality](quality.md) and [Users](users.md).

## Read it back

- [Read API](api.md), [CLI](cli.md) and [MCP](mcp.md): three clients of one
  API, returning the same bytes.
- [Coding agents](agents.md): which door to use, and in what order.
- [Taking the data out](export.md): the archive, and what reads it.

## Operate it

- [Accounts](accounts.md), [Administration](admin.md) and
  [Retention](retention.md).

The source is on [GitHub](https://github.com/tracepad/tracepad); the
[changelog](https://github.com/tracepad/tracepad/blob/main/CHANGELOG.md) lists
what changed in each release.
