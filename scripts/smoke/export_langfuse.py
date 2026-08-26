"""Export a trace with the Langfuse Python SDK.

The SDK is configured with nothing but a host and a key pair, exactly as it
would be against Langfuse itself: it exports OTLP protobuf to
`/api/public/otel/v1/traces` with Basic auth, which is the alias route and
the second auth scheme of spec 002 #2. Everything trace-level here arrives as
`langfuse.*` attributes on spans, which is what the dialect exists for.
"""

import os
import sys

import langfuse
from langfuse import Langfuse

host = os.environ["SMOKE_HOST"]
trace_id_file = sys.argv[1]

client = Langfuse(
    public_key=os.environ["SMOKE_PUBLIC_KEY"],
    secret_key=os.environ["SMOKE_SECRET_KEY"],
    host=host,
)

with langfuse.propagate_attributes(
    trace_name="smoke-trace",
    user_id="smoke-user",
    session_id="smoke-session",
    tags=["smoke", "spec-002"],
    metadata={"suite": "smoke"},
    environment="smoke",
):
    with client.start_as_current_observation(name="smoke-workflow") as span:
        trace_id = span.trace_id
        with client.start_as_current_observation(
            name="smoke-generation",
            as_type="generation",
            model="claude-sonnet-5",
            model_parameters={"temperature": 0.1, "max_tokens": 64},
            input=[{"role": "user", "content": "ping"}],
        ) as generation:
            generation.update(
                output={"role": "assistant", "content": "pong"},
                usage_details={"input": 11, "output": 5, "total": 16},
                cost_details={"input": 0.0001, "output": 0.0002},
            )

client.flush()
client.shutdown()

with open(trace_id_file, "w") as f:
    f.write(trace_id)
print(f"langfuse trace {trace_id} exported to {host}")
