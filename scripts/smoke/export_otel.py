"""Export a GenAI-semconv trace with the plain OpenTelemetry Python SDK.

This is the integration spec 002 sells: no Tracepad SDK, no Langfuse SDK —
just the standard OTLP/HTTP exporter pointed at the endpoint with a bearer
token. The attributes are set by hand because real GenAI instrumentation
needs a real provider call; the wire format, the batching and the protobuf
encoding are the exporter's own.
"""

import json
import os
import sys

from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor

endpoint = os.environ["SMOKE_OTLP_ENDPOINT"]
secret = os.environ["SMOKE_SECRET_KEY"]
trace_id_file = sys.argv[1]

provider = TracerProvider(
    resource=Resource.create({"service.name": "smoke-otel", "deployment.environment.name": "smoke"})
)
provider.add_span_processor(
    BatchSpanProcessor(
        OTLPSpanExporter(endpoint=endpoint, headers={"Authorization": f"Bearer {secret}"})
    )
)
tracer = provider.get_tracer("tracepad.smoke")

with tracer.start_as_current_span("handle-request") as root:
    root.set_attribute("user.id", "smoke-user")
    root.set_attribute("session.id", "smoke-session")
    trace_id = format(root.get_span_context().trace_id, "032x")

    with tracer.start_as_current_span("chat gpt-4o-mini") as generation:
        generation.set_attribute("gen_ai.system", "openai")
        generation.set_attribute("gen_ai.operation.name", "chat")
        generation.set_attribute("gen_ai.request.model", "gpt-4o-mini")
        generation.set_attribute("gen_ai.request.temperature", 0.3)
        generation.set_attribute("gen_ai.request.max_tokens", 128)
        generation.set_attribute("gen_ai.response.model", "gpt-4o-mini-2026-04-01")
        generation.set_attribute("gen_ai.usage.input_tokens", 42)
        generation.set_attribute("gen_ai.usage.output_tokens", 7)
        generation.set_attribute(
            "gen_ai.input.messages",
            json.dumps([{"role": "user", "content": "ping"}]),
        )
        generation.set_attribute(
            "gen_ai.output.messages",
            json.dumps([{"role": "assistant", "content": "pong"}]),
        )

provider.shutdown()

with open(trace_id_file, "w") as f:
    f.write(trace_id)
print(f"otel trace {trace_id} exported to {endpoint}")
