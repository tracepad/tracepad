# Tracepad documentation

Tracepad is LLM observability and evals in a single binary: traces, prompt
versions, datasets and eval runs, scores and annotation queues for LLM and
agent applications, self-hosted on one embedded database. Point any
OpenTelemetry-instrumented app at it, or one that sends with the Langfuse SDK,
and read what it did in the browser, from the CLI, or through your coding
agent over MCP.

New here? [Quickstart](quickstart.md) goes from nothing to a trace on screen,
by hand or [through your coding agent](quickstart.md#with-your-coding-agent).
Already sending with the Langfuse SDK? [Coming from the Langfuse SDK](langfuse-sdk.md)
is the switch. Weighing your options? [How Tracepad compares](compare.md) sets
it beside the tools that cover the same ground.

## Get started

- [Quickstart](quickstart.md): the first trace on screen.
- [Agent setup](agent-setup.md): the page to hand a coding agent, which sets
  Tracepad up for a project and reads the first trace back.
- [Agent upgrade](agent-upgrade.md): the page to hand it for an update, which
  backs the data up, restarts the server and checks it, with a way back.
- [Installing the binary](install.md): download, verify, run as a service,
  upgrade, back up.
- [Docker](docker.md): the image, its volume, TLS in front, upgrades.
- [Configuration](configuration.md): every environment variable.
- [How Tracepad compares](compare.md): what other tools need to run, and
  where they do more.

## Send traces

- [Sending traces](ingest.md): the endpoints, the auth schemes, the attribute
  mapping.
- [Python](sdk-python.md), [Node](sdk-js.md) and [Go](sdk-go.md): the
  packages, each a thin layer over OpenTelemetry.
- [Media](media.md): images and files in traces.
- [Coming from the Langfuse SDK](langfuse-sdk.md): what carries over, and what
  goes through Tracepad's API instead.

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
