"""Export a trace from an application that configures nothing.

The path the docs call "nothing but environment" (README, quickstart): the
application imports only the OpenTelemetry *API* and asks for a tracer. There
is no provider, no exporter and no endpoint in this file — `run.sh` starts it
under `opentelemetry-instrument` with the `OTEL_EXPORTER_OTLP_*` lines the docs
print, and the distro's launcher builds the provider from them. If the recipe
stops being enough (a variable renamed, a protocol the server no longer
speaks), nothing arrives and `check.py` fails.

Deliberately imports no SDK and no exporter: that is what makes it the
environment's test and not a second copy of export_otel.py.
"""

import sys

from opentelemetry import trace

trace_id_file = sys.argv[1]
tracer = trace.get_tracer("tracepad.smoke.env-only")

with tracer.start_as_current_span("env-only-request") as root:
    root.set_attribute("user.id", "smoke-user")
    root.set_attribute("session.id", "smoke-session")
    context = root.get_span_context()
    if not context.is_valid:
        # No provider was configured: the launcher did not run, or it could not
        # build an exporter from the environment and said so on stderr.
        raise SystemExit("FAIL: the span is a no-op; opentelemetry-instrument configured nothing")
    trace_id = format(context.trace_id, "032x")

    with tracer.start_as_current_span("chat gpt-4o-mini") as generation:
        generation.set_attribute("gen_ai.operation.name", "chat")
        generation.set_attribute("gen_ai.request.model", "gpt-4o-mini")
        generation.set_attribute("gen_ai.usage.input_tokens", 11)
        generation.set_attribute("gen_ai.usage.output_tokens", 3)

with open(trace_id_file, "w") as f:
    f.write(trace_id)
print(f"env-only trace {trace_id}")
