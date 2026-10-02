# Configuration

Every variable the server, the CLI and the packages read, in one place. The
server takes two flags as well — `--listen` and `--data-dir` — and a flag beats
its variable. `tracepad help` prints the server's list; a variable the server
does not know is logged as a warning at start, which catches a typo.

Sizes are bytes, durations are Go's (`30m`, `1h`), and "on/off" variables take
`on`/`off`, `true`/`false`, `yes`/`no` or `1`/`0`.

## The server

| Variable | Default | What it does |
|---|---|---|
| `TRACEPAD_LISTEN` | `localhost:4318` | Address to serve on. The default is this machine only: a `localhost` host is both loopback addresses, `127.0.0.1` and `::1`. `:4318` is every interface, over plain HTTP, and warns at start. The Docker image sets `:4318`. See [install.md](install.md#first-run). |
| `TRACEPAD_DATA_DIR` | `$XDG_DATA_HOME/tracepad`, else `~/.local/share/tracepad` (`/data` in the image) | The one directory everything is kept in. |
| `TRACEPAD_URL` | unset | The address people reach the server at, for the links it prints — the setup and invitation links. Set it behind a proxy. The CLI reads the same variable as its target. See [accounts.md](accounts.md). |
| `TRACEPAD_PROJECTS` | unset | `name:public_key:secret_key,…` — projects and keys declared at start, so a deployment never has one printed. See [docker.md](docker.md). |
| `TRACEPAD_ADMIN_TOKEN` | unset | Bearer token for administration across projects, at least 32 characters (`openssl rand -hex 32`); a shorter one stops the server from starting. See [admin.md](admin.md). |
| `TRACEPAD_ADMIN_TOKEN_FILE` | unset | Read the admin token from this file instead; readable by the server's user and nobody else. |
| `TRACEPAD_SETUP` | `on` | `off` mints and prints no setup link — for a deployment that makes its first owner with the admin token. |
| `TRACEPAD_SESSION_DAYS` | `30` | How long a browser sign-in lasts, sliding; at least `1`. |
| `TRACEPAD_TRUSTED_PROXIES` | loopback | Peers whose `X-Forwarded-For`, `-Proto` and `-Host` are believed: addresses and CIDR ranges, or `none`. See [docker.md](docker.md#serving-over-tls). |
| `TRACEPAD_STORE_RAW` | `on` | Keep every accepted export body, so the archive can be replayed ([export.md](export.md)). Costs disk. |
| `TRACEPAD_MAX_BODY_BYTES` | `20971520` (20 MiB) | Request body cap, applied to the wire bytes and again to what they decompress to. See [ingest.md](ingest.md). |
| `TRACEPAD_MAX_SPANS_PER_REQUEST` | `20000` | Spans one export may carry; at least `1`. |
| `TRACEPAD_BODY_BUDGET_BYTES` | four times the body cap | Request bodies the server holds in memory at once; at least the body cap. Past it a write waits and then gets `429`. |
| `TRACEPAD_RESPONSE_BUDGET_BYTES` | `51200` | The default byte budget of a read response ([api.md](api.md)); a request may ask for another. From 4 KiB to 5 MiB. |
| `TRACEPAD_READ_TIMEOUT` | `20s` | Deadline of one read request, its wait for a slot included; from `1s` to `4m`. |
| `TRACEPAD_READ_CONCURRENCY` | two per CPU, at least 4 | Reads served at once; at least `1`. |
| `TRACEPAD_MAX_CONNECTIONS` | `1024` | Client connections held at once; at least `16`. Past it a new connection waits to be accepted, and the connection idle longest, or else one that has sent nothing for two seconds, is closed to make room for it. |
| `TRACEPAD_MAX_CONNECTIONS_PER_SOURCE` | a quarter of `TRACEPAD_MAX_CONNECTIONS` | Connections one address (an IPv6 /64) may hold; from `1` to `TRACEPAD_MAX_CONNECTIONS`. Past it the source's own idle ones make room, and a new one is refused when all are busy. A proxy in `TRACEPAD_TRUSTED_PROXIES` is exempt. |
| `TRACEPAD_MCP` | `on` | Serve MCP at `/mcp` ([mcp.md](mcp.md)). |
| `TRACEPAD_SWEEP_INTERVAL` | `1h` | Cadence of the retention sweep; at least `1s`. See [retention.md](retention.md). |
| `TRACEPAD_ROLLUP_INTERVAL` | `5m` | Cadence of the statistics rollup; at least `1s`. |
| `TRACEPAD_IN_CONTAINER` | unset | Set to `1` by the image so a wildcard bind is read as the design rather than a warning. Not for you to set. |

## The CLI and the MCP server

| Variable | Default | What it does |
|---|---|---|
| `TRACEPAD_URL` | `http://localhost:4318` | The server to talk to. |
| `TRACEPAD_API_KEY` | unset | The key, or the admin token where a command says so. Prefer it to `--key`, which shows in the process list. |

`tracepad health` with neither `--url` nor `TRACEPAD_URL` looks at
`TRACEPAD_LISTEN`. [cli.md](cli.md) has the rest.

## The Python, Node and Go packages

| Variable | Default | What it does |
|---|---|---|
| `TRACEPAD_URL` | unset | Where the store is. `TRACEPAD_HOST`, the packages' first name for it, still works and warns once. |
| `TRACEPAD_API_KEY` | unset | A secret key, sent as `Bearer`. An `ingest` key is enough to export. |
| `TRACEPAD_ENVIRONMENT` | unset | The deployment this process is. |
| `TRACEPAD_RELEASE` | unset | The version of this deployment. |
| `TRACEPAD_EXPORT_TIMEOUT` | `5` | Seconds one export may take, retries included. |

**`TRACEPAD_URL` is one variable with two readers.** The server takes it as the
public, browser-facing address (for the links it prints), the CLI and the
packages as the address to talk to. In a `.env` shared between the server and an
application, the application therefore sends its spans to the public address; if
it should use an internal one (`http://tracepad:4318`), give `init` the host
explicitly or keep a separate `.env` for it.

Options passed to `init` win over these. The OpenTelemetry variables
(`OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES`, the exporter's and the batch
processor's own) are the OpenTelemetry SDK's business, and it honours them as
they are. [sdk-python.md](sdk-python.md), [sdk-js.md](sdk-js.md) and
[sdk-go.md](sdk-go.md) have the details.

## Sending from any OpenTelemetry SDK

These are the OpenTelemetry SDK's, not Tracepad's, and this is what to set:

| Variable | Value |
|---|---|
| `OTEL_EXPORTER_OTLP_PROTOCOL` | `http/protobuf` — required wherever the SDK defaults to gRPC — Tracepad has no gRPC receiver. `http/json` works too. |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | `http://<server>/v1/traces` |
| `OTEL_EXPORTER_OTLP_HEADERS` | `authorization=Bearer <secret key>` |

The Langfuse SDKs take `LANGFUSE_HOST`, `LANGFUSE_PUBLIC_KEY` and
`LANGFUSE_SECRET_KEY` instead ([ingest.md](ingest.md#connecting-an-application)).
