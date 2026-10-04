# How Tracepad compares

Tracepad is one way to keep the traces and evals of an LLM application. This
page puts it next to other tools that cover the same ground and does not
grade them. It lists what each one needs in order to run, under which licence,
and where the other tools do more than Tracepad does.

**Every fact about another product was checked on 2026-10-04** against the
source linked under the table. These products change quickly, and the date
matters more than any single cell. If something here is out of date, an issue
or a pull request is welcome.

## Running it yourself

| | Licence | Self-hosting | What a self-hosted install runs | OTLP ingest |
|---|---|---|---|---|
| **Tracepad** | Apache-2.0 | Free, no features gated | One binary with SQLite inside, under 30 MB of memory at rest | HTTP (protobuf, JSON); no gRPC |
| Langfuse | MIT, plus commercial `ee` directories | Free; some features need a licence key, among them data retention policies, project-level roles and audit logs | Web and worker containers, PostgreSQL, ClickHouse, Redis and an S3-compatible store. The Docker Compose guide recommends at least 4 cores and 16 GiB of memory | HTTP (protobuf, JSON); no gRPC |
| Arize Phoenix | Elastic License 2.0 | Free, no features gated | One container, with SQLite by default or PostgreSQL | gRPC and HTTP |
| Opik | Apache-2.0 | Free; the self-hosted edition has no user management | Backend, Python backend and frontend containers, MySQL, ClickHouse, ZooKeeper, Redis and MinIO | HTTP (protobuf); no gRPC |
| Laminar | Apache-2.0 | Free | Frontend and app server, PostgreSQL, ClickHouse and Quickwit; RabbitMQ in the full setup | gRPC and HTTP |
| LangSmith | Proprietary | Enterprise plan only | Kubernetes with ClickHouse, PostgreSQL, Redis and blob storage; the guide asks for at least 16 vCPUs and 64 GB of memory | HTTP (protobuf) |
| Pydantic Logfire | SDK MIT; the platform is closed source | Enterprise plan only | Kubernetes with PostgreSQL, object storage, Redis and an identity provider | gRPC and HTTP |

Sources, checked 2026-10-04:
Langfuse — [Docker Compose](https://langfuse.com/self-hosting/deployment/docker-compose),
[licence key](https://langfuse.com/self-hosting/license-key),
[OpenTelemetry](https://langfuse.com/integrations/native/opentelemetry);
Phoenix — [self-hosting](https://arize.com/docs/phoenix/self-hosting),
[architecture](https://arize.com/docs/phoenix/self-hosting/architecture),
[licence](https://github.com/Arize-ai/phoenix/blob/main/LICENSE);
Opik — [self-hosting](https://www.comet.com/docs/opik/self-host/overview),
[docker-compose.yaml](https://github.com/comet-ml/opik/blob/main/deployment/docker-compose/docker-compose.yaml),
[OpenTelemetry](https://www.comet.com/docs/opik/tracing/opentelemetry/overview);
Laminar — [Docker Compose](https://laminar.sh/docs/self-hosting/docker-compose);
LangSmith — [self-hosted](https://docs.langchain.com/langsmith/self-hosted),
[Kubernetes](https://docs.langchain.com/langsmith/kubernetes),
[OpenTelemetry](https://docs.langchain.com/langsmith/trace-with-opentelemetry);
Logfire — [self-hosted deployment](https://pydantic.dev/docs/logfire/deploy/self-hosted-deployment/overview/).
Tracepad's own figures are in the [README](../README.md#what-it-is-built-for).

## What each one does

| | Tracepad | Langfuse | Phoenix | Opik | Laminar | LangSmith | Logfire |
|---|---|---|---|---|---|---|---|
| Traces, sessions, users, cost | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Prompt versions and labels | ✓ | ✓ | ✓ | ✓ | — | ✓ | ✓ |
| Datasets and run comparison | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Human review and annotation queues | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Built-in LLM-as-judge evaluators | — | ✓ | ✓ | ✓ | — | ✓ | ✓ |
| Online evaluation of live traffic | — | ✓ | — | ✓ | — | ✓ | ✓ |
| Prompt playground | — | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| Alerts | — | ✓ | — | ✓ | ✓ (licence key) | ✓ | ✓ |
| An official MCP server | ✓ read-only | ✓ | ✓ | ✓ | ✓ | ✓ | ✓ |
| A managed cloud | — | ✓ | ✓ (Arize AX) | ✓ | ✓ | ✓ | ✓ |

A dash means the feature was not found in that product's documentation on
2026-10-04. It does not mean the feature can't be built on top. On Tracepad,
for example, an eval harness can call any judge model you choose and post its
verdicts as scores ([datasets.md](datasets.md), [scores.md](scores.md)).

## Where Tracepad is the better fit

- **The whole install is one process.** There is no database server, cache or
  object store to run, upgrade or back up. A backup is a copy of one file
  ([install.md](install.md)).
- **Nothing is behind a licence key.** Retention windows, erasure of one
  user's data, roles and scoped keys are in the Apache-2.0 binary
  ([retention.md](retention.md), [accounts.md](accounts.md)).
- **The exact bytes come back out.** Every export body is kept as it arrived,
  and `tracepad export --otlp` replays it into any OTLP receiver
  ([export.md](export.md)).
- **Agents read it safely.** The MCP server cannot change anything, responses
  have a byte budget, and the agent skill ships inside the binary
  ([agents.md](agents.md)).
- **Traces from the Langfuse SDK arrive unchanged** ([langfuse-sdk.md](langfuse-sdk.md)).

## Where another tool is the better fit

- **More than about a million spans a day, or high availability.** Tracepad is
  one process on one machine
  ([what it is built for](../README.md#what-it-is-built-for)). The tools built
  on ClickHouse are designed for that scale.
- **Evaluation without writing code:** built-in judges, online evaluation of
  live traffic, a playground.
- **Single sign-on, SCIM or audit logs.** Tracepad has email and password
  accounts with roles.
- **A hosted service you never run yourself.** Tracepad has no cloud.
- **gRPC-only exporters, or the OpenInference conventions** that Phoenix's
  instrumentations write. Tracepad accepts OTLP over HTTP, and it stores
  OpenInference spans without reading model or token counts from them yet
  ([What beta means](../README.md#what-beta-means)).
