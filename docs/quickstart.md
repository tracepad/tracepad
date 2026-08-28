# Quickstart

From nothing to a trace on screen. Everything below runs on one machine and
needs no configuration.

## 1. Run the server

```sh
tracepad
```

The first run creates the database, a project called `default`, and its key
pair — then prints them, once:

```
Project "default" created. Connect your app with either:

  # OpenTelemetry SDK
  OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://localhost:4318/v1/traces
  OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer tp-sk-…"

  # Langfuse SDK
  LANGFUSE_HOST=http://localhost:4318
  LANGFUSE_PUBLIC_KEY=tp-pk-…
  LANGFUSE_SECRET_KEY=tp-sk-…

  # Web interface, signed in with that key
  http://localhost:4318/#key=tp-sk-…
```

**Copy the secret key somewhere.** It is stored hashed, so this is the only
time it is printable; a lost key is replaced with `tracepad keys new`, not
recovered.

Data lives in `~/.local/share/tracepad` by default (`/data` in the Docker
image); `TRACEPAD_DATA_DIR` moves it.

## 2. Point an application at it

Any OpenTelemetry SDK, in any language, with the two variables above. In
Python, with the OpenTelemetry SDK's automatic exporter configuration:

```sh
export OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://localhost:4318/v1/traces
export OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer tp-sk-…"
python your_app.py
```

The Langfuse SDKs work too, against the same endpoint under their own path —
set `LANGFUSE_HOST`, `LANGFUSE_PUBLIC_KEY` and `LANGFUSE_SECRET_KEY` instead.
Both dialects of attribute naming are understood; see
[ingest.md](ingest.md) for what is mapped and how.

A `200` from the export means the spans are committed and fsynced, so a
trace is queryable the moment its exporter's batch returns.

## 3. Look at them

Three ways, all reading the same API.

**The browser.** Open the pre-authed link the first run printed and the
interface is already signed in. Later runs print the plain URL and the login
screen asks for the key. See [ui.md](ui.md).

**The terminal.** The CLI is the same binary:

```sh
export TRACEPAD_API_KEY=tp-sk-…
tracepad traces                  # the newest traces
tracepad traces last --error --full   # the last failure, payloads included
```

See [cli.md](cli.md).

**An agent.** Point an MCP client at `http://localhost:4318/mcp` with the
same key, or `curl` the API directly:

```sh
curl -H "Authorization: Bearer tp-sk-…" \
  "http://localhost:4318/api/v1/traces?status=error&limit=5"
```

`GET /api/v1` lists every endpoint with a one-line description; see
[api.md](api.md) and [mcp.md](mcp.md).

## Next

- [ingest.md](ingest.md) — endpoints, authentication, attribute conventions.
- [api.md](api.md) — the read API, its filters, and the response budget.
- [retention.md](retention.md) — how long data is kept and how to change it.
- [admin.md](admin.md) — more projects, more keys, erasing one user's data.
