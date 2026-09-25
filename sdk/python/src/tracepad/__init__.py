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
from ._datasets import Dataset, Item, dataset
from ._errors import (
    TracepadConfigError,
    TracepadError,
    TracepadHTTPError,
    TracepadPlaceholderError,
)
from ._harness import Attempt, Run, ScoreConfig, compare, item_id, score_configs
from ._prompts import Prompt, prompt
from ._scores import score
from ._traces import delete_trace, delete_traces
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
    "Attempt",
    "Dataset",
    "Generation",
    "Item",
    "Observation",
    "Prompt",
    "Run",
    "ScoreConfig",
    "TracepadConfigError",
    "TracepadError",
    "TracepadHTTPError",
    "TracepadPlaceholderError",
    "__version__",
    "compare",
    "dataset",
    "delete_trace",
    "delete_traces",
    "event",
    "flush",
    "generation",
    "init",
    "item_id",
    "observe",
    "prompt",
    "score",
    "score_configs",
    "span",
    "update",
    "update_trace",
]
