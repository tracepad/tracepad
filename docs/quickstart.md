# Quickstart

From nothing to a trace on screen. Everything below runs on one machine and
needs no configuration.

## 1. Run the server

```sh
tracepad
```

Or in Docker, which needs nothing installed but Docker:

```sh
docker run -d --name tracepad -v tracepad:/data -p 4318:4318 \
  ghcr.io/tracepad/tracepad
docker logs tracepad
```

Everything below is the same either way; the container prints to its log what
the binary prints to your terminal. See [docker.md](docker.md) for the volume,
the permissions and upgrades.

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
```

**Copy the secret key somewhere.** It is stored hashed, so this is the only
time it is printable; a lost key is replaced with `tracepad keys create`, not
recovered.

Underneath it is a second link, which is how you get into the browser
interface:

```
This server has no owner yet. Create the first one — it takes an email and a
password, and nothing is written down anywhere but this database:

  http://localhost:4318/setup#token=…

The link is good until this process stops. Restart to have a new one printed.
```

The key is for your application; the account is for you. Open the link, pick a
password, and that is the last credential you type into a browser here — see
[accounts.md](accounts.md). The token in it is minted per start and held in
memory, so if you lose the link, restart and a new one is printed.

Data lives in `~/.local/share/tracepad` by default (`/data` in the Docker
image, which is where the volume goes); `TRACEPAD_DATA_DIR` moves it.

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
Every dialect of attribute naming is understood; see [ingest.md](ingest.md)
for what is mapped and how.

**In Python, with a decorator instead.** The `tracepad` package is a thin
layer over the same OpenTelemetry SDK — one call to point the process at the
store, and the shapes a person writes every time:

```sh
pip install tracepad
export TRACEPAD_HOST=http://localhost:4318
export TRACEPAD_API_KEY=tp-sk-…
```

```python
import tracepad

tracepad.init()

@tracepad.observe
def answer(question: str) -> str:
    tracepad.update_trace(user_id="u-42", tags=["support"])
    with tracepad.generation("chat", model="gpt-4o-mini", input=question) as call:
        response = client.chat.completions.create(model="gpt-4o-mini", messages=…)
        call.end(response=response)          # model, usage, and the cost as charged
    tracepad.score("helpful", 1)             # against the trace in flight
    return response.choices[0].message.content
```

It adds an exporter to a `TracerProvider` your application already has rather
than replacing it, so it sits beside FastAPI instrumentation and the Langfuse
SDK instead of competing with them. See [sdk-python.md](sdk-python.md).

A `200` from the export means the spans are committed and fsynced, so a
trace is queryable the moment its exporter's batch returns.

## 3. Look at them

Three ways, all reading the same API.

**The browser.** Open the setup link the first run printed and create your
account; after that it is `http://localhost:4318/` and an email and a password.
See [ui.md](ui.md) and [accounts.md](accounts.md).

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
- [sdk-python.md](sdk-python.md) — the `tracepad` package: `init`, `@observe`,
  generations, prompts and scores.
- [api.md](api.md) — the read API, its filters, and the response budget.
- [retention.md](retention.md) — how long data is kept and how to change it.
- [admin.md](admin.md) — more projects, more keys, erasing one user's data.
- [accounts.md](accounts.md) — inviting somebody, and what a `viewer` may do.
