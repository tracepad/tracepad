# Quickstart

From nothing to a trace on screen. Everything below runs on one machine and
needs no configuration.

## 1. Run the server

```sh
tracepad
```

Or in Docker, which needs nothing installed but Docker:

```sh
docker run -d --name tracepad -v tracepad:/data -p 127.0.0.1:4318:4318 \
  ghcr.io/tracepad/tracepad
docker logs tracepad
```

Everything below is the same either way; the container prints to its log what
the binary prints to your terminal. See [docker.md](docker.md) for the volume,
the permissions and upgrades.

The `127.0.0.1:` keeps the port on this machine, as `tracepad --listen
127.0.0.1:4318` does for the binary. Tracepad speaks plain HTTP, and the binary
warns at start while other machines can reach it that way (the container, which
cannot see where its port is published, says it as a note);
[docker.md](docker.md#serving-over-tls) says how to put TLS in front before you
open it up.

The first run creates the database, a project called `default`, and its key
pair — then prints them, once:

```
Project "default" created. Connect your app with either:

  # OpenTelemetry SDK (Tracepad has no gRPC: the protocol line is required if your SDK defaults to it)
  OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
  OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://localhost:4318/v1/traces
  OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer tp-sk-…"

  # Langfuse SDK
  LANGFUSE_HOST=http://localhost:4318
  LANGFUSE_PUBLIC_KEY=tp-pk-…
  LANGFUSE_SECRET_KEY=tp-sk-…
```

**Copy the secret key somewhere.** It is stored hashed, so this is the only
time it is printable; a lost key is replaced in Settings → Project → API keys,
or with `tracepad keys create` and the admin token — not recovered.

This first key holds all three of a key's [scopes](api.md#scopes) — `ingest`
to send, `read` to look, `write` to change — so it works for everything on
this page.

Underneath it is a second link, which is how you get into the browser
interface:

```
This server has no owner yet. Create the first one — it takes an email and a
password, and nothing is written down anywhere but this database:

  http://localhost:4318/setup#token=…

The link is good for 24 hours, or until this process stops. Restart to have a
new one printed.
```

The key is for your application; the account is for you. Open the link, pick a
password, and that is the last credential you type into a browser here — see
[accounts.md](accounts.md). The token in it is minted per start and held in
memory and works for 24 hours, so if you lose the link, restart and a new one
is printed.

Data lives in `~/.local/share/tracepad` by default (`/data` in the Docker
image, which is where the volume goes); `TRACEPAD_DATA_DIR` moves it.

## 2. Point an application at it

Any OpenTelemetry SDK, in any language, with the three variables above. **The
protocol line is required wherever an SDK defaults to gRPC** — Python's
auto-configured exporter does — since Tracepad speaks OTLP over HTTP and has no gRPC
receiver, and an exporter left to choose for itself may pick gRPC — the
application runs, reports nothing, and the trace list stays empty. In Python,
with the OpenTelemetry distro's automatic configuration (`pip install
opentelemetry-distro opentelemetry-exporter-otlp-proto-http`):

```sh
export OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
export OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://localhost:4318/v1/traces
export OTEL_EXPORTER_OTLP_HEADERS="authorization=Bearer tp-sk-…"
opentelemetry-instrument python your_app.py
```

A plain `python your_app.py` configures nothing; the distro's launcher (or
your own code) is what reads these variables.

The Langfuse SDKs work too, against the same endpoint under their own path —
set `LANGFUSE_HOST`, `LANGFUSE_PUBLIC_KEY` and `LANGFUSE_SECRET_KEY` instead.
Every dialect of attribute naming is understood; see [ingest.md](ingest.md)
for what is mapped and how.

**In Python, with a decorator instead.** The `tracepad` package is a thin
layer over the same OpenTelemetry SDK — one call to point the process at the
store, and the shapes a person writes every time:

```sh
pip install tracepad
export TRACEPAD_URL=http://localhost:4318
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

**In Node, the same package.** `tracepad` on npm is the same surface with
promises where Python has context managers — the same names on the wire, the
same rules about what throws — for Node 22 and newer:

```sh
npm install tracepad @opentelemetry/api
export TRACEPAD_URL=http://localhost:4318
export TRACEPAD_API_KEY=tp-sk-…
```

```ts
import * as tracepad from 'tracepad';

tracepad.init();

const answer = tracepad.observe(async (question: string) => {
  tracepad.updateTrace({ userId: 'u-42', tags: ['support'] });
  const reply = await tracepad.generation('chat', { model: 'gpt-4o-mini', input: question }, async (call) => {
    const response = await client.chat.completions.create({ model: 'gpt-4o-mini', messages: […] });
    call.end(response);                    // model, usage, and the cost as charged
    return response.choices[0].message.content;
  });
  tracepad.score('helpful', 1);            // against the trace in flight
  return reply;
});
```

See [sdk-js.md](sdk-js.md).

**In Go, the same shape as the OTel API.** The `tracepad` module is the same
thin layer over the OpenTelemetry Go SDK — a context in, a context out:

```sh
go get github.com/tracepad/tracepad/sdk/go
```

```go
import tracepad "github.com/tracepad/tracepad/sdk/go"

func main() {
	shutdown, err := tracepad.Init(ctx)      // TRACEPAD_URL, TRACEPAD_API_KEY
	if err != nil {
		log.Fatal(err)
	}
	defer shutdown(ctx)
	...
}

func answer(ctx context.Context, question string) (string, error) {
	ctx, step := tracepad.Span(ctx, "answer", tracepad.WithInput(question))
	defer step.End()
	tracepad.UpdateTrace(ctx, tracepad.WithUserID("u-42"), tracepad.WithTags("support"))

	ctx, call := tracepad.Generation(ctx, "chat", tracepad.WithModel("gpt-4o-mini"))
	response, err := client.Chat.Completions.New(ctx, params)
	if err != nil {
		call.Fail(err)
		return "", err
	}
	call.End(tracepad.Result{Model: response.Model, Usage: usage(response), Output: text(response)})
	tracepad.Score(ctx, "helpful", tracepad.WithValue(1))   // against the trace in flight
	return text(response), nil
}
```

It registers an exporter on the `TracerProvider` your application already has
— `otelhttp`, `otelgrpc` — rather than replacing it. See [sdk-go.md](sdk-go.md).

A `200` from the export means the spans are committed and fsynced, so a
trace is queryable the moment its exporter's batch returns.

**Before it goes to production, give the application a key of its own** that
holds `ingest` and nothing else. Every exporter above, the `tracepad` packages'
scores and prompt fetch included, needs no more — and a key that cannot read
is a key whose leak exposes nothing your users sent. In the interface it is
Settings → Project → API keys → mint, where `ingest` is ticked by default;
from a terminal, with the admin token:

```sh
TRACEPAD_API_KEY=$TRACEPAD_ADMIN_TOKEN tracepad keys create --scope ingest --name "checkout api"
```

It prints the lines to paste: `TRACEPAD_API_KEY` for the `tracepad` packages,
and the `LANGFUSE_*` pair for a Langfuse SDK; an OpenTelemetry exporter takes
the same secret as `authorization=Bearer …`. The first key
stays for you, the CLI and the eval harness — or give each of those a key of
its own as well ([admin.md](admin.md#keys)).

## 3. Look at them

Three ways, all reading the same API.

**The browser.** Open the setup link the first run printed and create your
account; after that it is `http://localhost:4318/` and an email and a password.
The root lands on your project's dashboard at `/p/{project id}/dashboard` —
the exporter settings above, until the first trace arrives, then the
traffic, the cost, the errors and the latency of the window with their
movement against the window before; *Traces* in the sidebar is the listing.
Every screen carries its project in the address, and the switcher at the top
of the sidebar moves between projects. See [ui.md](ui.md) and
[accounts.md](accounts.md).

**The terminal.** The CLI is the same binary:

```sh
export TRACEPAD_API_KEY=tp-sk-…
tracepad traces                  # the newest traces
tracepad traces last --error --full   # the last failure, payloads included
```

See [cli.md](cli.md).

**An agent.** Point an MCP client at `http://localhost:4318/mcp` with the
same key — or, better, a key that holds `read` alone, which can look at
everything and change nothing — or `curl` the API directly:

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
- [sdk-js.md](sdk-js.md) — the same package for Node.
- [sdk-go.md](sdk-go.md) — the same package for Go: `Init`, `Span`,
  `Generation`, prompts and scores, in the shape of the OTel API.
- [api.md](api.md) — the read API, its filters, and the response budget.
- [agents.md](agents.md) — the skill that teaches a coding agent to work with
  all of this: `tracepad skills install`.
- [retention.md](retention.md) — how long data is kept and how to change it.
- [admin.md](admin.md) — more projects, more keys, erasing one user's data.
- [accounts.md](accounts.md) — inviting somebody, and what a `viewer` may do.
