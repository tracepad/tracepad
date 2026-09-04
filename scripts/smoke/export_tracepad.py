"""Export a trace with the Tracepad package itself.

The third exporter beside the plain OpenTelemetry SDK and the Langfuse SDK
(spec 017 #12). The other two are the drift detector for somebody else's
conventions; this one is the drift detector for ours — it is installed from
`sdk/python` in this checkout, so what it writes is what the mapper of this
same commit has to read.
"""

import os
import sys

import tracepad

trace_id_file = sys.argv[1]

tracepad.init(
    host=os.environ["SMOKE_HOST"],
    key=os.environ["SMOKE_SECRET_KEY"],
    environment="smoke",
    release="smoke-1",
)

RESPONSE = {
    "model": "claude-sonnet-5-2026-08-01",
    "choices": [{"message": {"role": "assistant", "content": "pong"}}],
    "usage": {"prompt_tokens": 11, "completion_tokens": 5, "cost": 0.0003},
}


@tracepad.observe(name="smoke-workflow")
def workflow(question: str) -> str:
    tracepad.update_trace(
        name="tracepad-smoke",
        user_id="smoke-user",
        session_id="smoke-session",
        tags=["smoke", "spec-017"],
        metadata={"suite": "smoke"},
    )
    with tracepad.generation(
        "smoke-generation",
        model="claude-sonnet-5",
        model_parameters={"temperature": 0.1, "max_tokens": 64},
        input=[{"role": "user", "content": question}],
    ) as call:
        trace_id.append(call.trace_id)
        call.first_token()
        call.end(response=RESPONSE)
    return "pong"


trace_id: list[str] = []
workflow("ping")
tracepad.flush()

with open(trace_id_file, "w") as f:
    f.write(trace_id[0])
print(f"tracepad trace {trace_id[0]} exported to {os.environ['SMOKE_HOST']}")
