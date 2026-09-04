"""Tracepad — the ergonomics of tracing an LLM application, over the OTel SDK.

    import tracepad

    tracepad.init()  # TRACEPAD_HOST / TRACEPAD_API_KEY

    @tracepad.observe
    def answer(question: str) -> str:
        with tracepad.generation("chat", model="gpt-4o-mini") as call:
            response = client.chat.completions.create(...)
            call.end(response=response)
        return response.choices[0].message.content

Everything this package does is reachable with the OpenTelemetry SDK and
`curl` — see `docs/sdk-python.md` and `docs/ingest.md`. It owns no transport,
no batching, no retry and no context propagation; those are the OTel SDK's,
and it wraps no provider client.
"""

from __future__ import annotations

from ._config import VERSION as __version__
from ._errors import TracepadConfigError, TracepadError, TracepadHTTPError
from ._prompts import Prompt, prompt
from ._scores import score
from ._tracing import (
    Generation,
    Observation,
    event,
    flush,
    generation,
    init,
    observe,
    span,
    update,
    update_trace,
)

__all__ = [
    "Generation",
    "Observation",
    "Prompt",
    "TracepadConfigError",
    "TracepadError",
    "TracepadHTTPError",
    "__version__",
    "event",
    "flush",
    "generation",
    "init",
    "observe",
    "prompt",
    "score",
    "span",
    "update",
    "update_trace",
]
