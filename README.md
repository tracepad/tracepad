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

## License

Apache-2.0. See [LICENSE](LICENSE); third-party attributions are in
[NOTICE](NOTICE).
