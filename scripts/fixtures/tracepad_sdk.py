#!/usr/bin/env python3
"""Write `testdata/otlp/010-tracepad-sdk.pb` from the package's own exporter.

Every other fixture in the corpus is built by hand in `internal/otlptest`,
which is the right shape for reproducing somebody else's SDK. This one is the
export our *own* package produces (spec 017, Testing): the bytes come off the
wire of a real `BatchSpanProcessor` pointed at a fake collector, so the golden
beside them is evidence that the attribute names the package writes are the
ones the mapper's table reads. Run it through `make fixtures`, which then
regenerates the golden from these bytes like any other body.

Two things are replaced so that a re-run is a no-op rather than a diff:

* the ids, by a fixed generator on the provider — this is also the
  adaptation path of Decision 2, an application that already has a provider;
* the clocks, by ranking every timestamp in the finished export and laying the
  ranks out on a fixed grid, which preserves the order and the zero-duration
  event exactly. The completion start, which is an attribute rather than a
  span field, is re-stamped from the same grid with the package's own
  formatter.

Everything else — the attribute names, their values, the resource, the scope,
the protobuf framing — is what the package and the OTel SDK actually emitted.
"""

from __future__ import annotations

import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import Any, ClassVar

from opentelemetry import trace as otel_api
from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.id_generator import IdGenerator

import tracepad
from tracepad import _attributes as attrs

# 2026-08-26T10:00:00Z, the base every fixture in the corpus shares.
BASE = 1787738400_000_000_000
STEP = 10_000_000  # 10 ms between one instant and the next
ROOT = Path(__file__).resolve().parents[2]
TARGET = ROOT / "testdata" / "otlp" / "010-tracepad-sdk.pb"

ANSWER = {
    "model": "claude-sonnet-5-2026-08-01",
    "choices": [{"message": {"role": "assistant", "content": "In the trace you are reading."}}],
    "usage": {
        "prompt_tokens": 128,
        "completion_tokens": 41,
        "prompt_tokens_details": {"cached_tokens": 96},
        "completion_tokens_details": {"reasoning_tokens": 12},
        "cost": 0.0011,
    },
}

# The second trace's answer. Its model name is deliberately unrelated to every
# other name in the corpus: the interface's end-to-end suite matches a model
# breakdown's rows by substring, and a name that contained another's would make
# two rows one (found running that suite).
REWRITTEN = {
    "model": "claude-haiku-4-5",
    "choices": [{"message": {"content": "Where does a span land?"}}],
    "usage": {"prompt_tokens": 18, "completion_tokens": 9},
}


class FixedIds(IdGenerator):
    """Ids counted up from one, so the fixture names the same spans forever."""

    def __init__(self) -> None:
        self.traces = 0
        self.spans = 0

    def generate_trace_id(self) -> int:
        self.traces += 1
        return 0xA0B1C2D3E4F5A6B7C8D9EA0000000000 + self.traces

    def generate_span_id(self) -> int:
        self.spans += 1
        return 0x1A2B3C4D5E6F0000 + self.spans


class Collector(BaseHTTPRequestHandler):
    bodies: ClassVar[list[bytes]] = []

    # `do_POST` is BaseHTTPRequestHandler's spelling, not ours.
    def do_POST(self) -> None:
        length = int(self.headers.get("Content-Length", "0"))
        Collector.bodies.append(self.rfile.read(length))
        assert self.headers.get("Authorization") == "Bearer tp-sk-fixture"
        self.send_response(200)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def log_message(self, *args: Any) -> None:
        pass


def application() -> None:
    """The two traces the fixture is of: every row of the Ingest contract.

    The question, the answer, the prompt, the observation kind, the user and
    the session are all this fixture's own. The corpus is seeded whole into the
    interface's end-to-end suite, which searches it for one fixture's phrase,
    filters it by another's prompt and counts the traces of a third's kind —
    so a value shared with a neighbour would silently change what that suite
    counts (found running it).
    """

    @tracepad.observe(type="retriever", name="docs-search")
    def search(query: str) -> list[str]:
        tracepad.update(level="WARNING", status_message="one result", metadata={"attempt": 2})
        return ["the ingest page"]

    @tracepad.observe(type="generation", name="rewrite-question")
    def rewrite(question: str) -> dict[str, Any]:
        """The decorator reading a response, rather than a block ending one."""
        return REWRITTEN

    docs = tracepad.Prompt(name="tracepad-answer", version=3, type="text", text="Answer {topic}.")
    question = "where does a span land?"

    with tracepad.span("answer-question", input={"question": question}):
        tracepad.update_trace(
            name="docs-chat",
            user_id="user-9001",
            session_id="session-91",
            tags=["docs", "beta"],
            metadata={"channel": "web"},
        )
        search("span")
        with tracepad.event("cache.miss", metadata={"key": "tracepad-answer"}):
            pass
        with tracepad.generation(
            "chat-completion",
            model="claude-sonnet-5",
            prompt=docs,
            model_parameters={"temperature": 0.2, "max_tokens": 512},
            input=[{"role": "user", "content": question}],
        ) as call:
            call.first_token()
            call.end(response=ANSWER)

    # A second trace, on the decorator's own path.
    with tracepad.span("prepare-question"):
        tracepad.update_trace(name="docs-rewrite", user_id="user-9001", session_id="session-91")
        rewrite("where does span land")


def flatten(export: ExportTraceServiceRequest) -> list[Any]:
    return [
        span
        for resource in export.resource_spans
        for scope in resource.scope_spans
        for span in scope.spans
    ]


def fix_clocks(export: ExportTraceServiceRequest) -> None:
    """Lay every instant in the export out on a fixed grid, in order."""
    spans = flatten(export)
    instants = {t for span in spans for t in (span.start_time_unix_nano, span.end_time_unix_nano)}
    grid = {instant: BASE + rank * STEP for rank, instant in enumerate(sorted(instants))}
    for span in spans:
        span.start_time_unix_nano = grid[span.start_time_unix_nano]
        span.end_time_unix_nano = grid[span.end_time_unix_nano]
        for attribute in span.attributes:
            if attribute.key == attrs.COMPLETION_START_TIME:
                # Half a step into the call: the fixture's time to first token.
                attribute.value.string_value = attrs.rfc3339(
                    span.start_time_unix_nano + STEP // 2
                )


def main() -> int:
    server = HTTPServer(("127.0.0.1", 0), Collector)
    threading.Thread(target=server.serve_forever, daemon=True).start()

    # The application's own provider, which `init` adapts to rather than
    # replaces (spec 017 #2). Its resource is written out rather than
    # discovered, so that a random instance id and an SDK version do not churn
    # the golden on every regeneration.
    otel_api.set_tracer_provider(
        TracerProvider(
            resource=Resource(
                {
                    "service.name": "support-bot",
                    "service.version": "2026.9.4",
                    "deployment.environment.name": "production",
                    "telemetry.sdk.language": "python",
                    "telemetry.sdk.name": "opentelemetry",
                }
            ),
            id_generator=FixedIds(),
        )
    )
    tracepad.init(f"http://127.0.0.1:{server.server_port}", "tp-sk-fixture")

    application()
    tracepad.flush(10.0)
    otel_api.get_tracer_provider().shutdown()
    server.shutdown()

    if len(Collector.bodies) != 1:  # one batch, or the spans would not be one body
        print(f"expected one export, got {len(Collector.bodies)}", file=sys.stderr)
        return 1

    export = ExportTraceServiceRequest()
    export.ParseFromString(Collector.bodies[0])
    fix_clocks(export)
    TARGET.write_bytes(export.SerializeToString())
    print(f"wrote {TARGET.relative_to(ROOT)} ({len(flatten(export))} spans)")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
