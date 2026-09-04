"""The eval harness: a run, the block that stamps it, and the scores.

Spec 014 made an eval a loop any script can run with `curl`. This is that loop
in Python, and the one thing it owns that a `curl` recipe cannot is the
stamping: inside `run.item(case)` every span the *application* starts — its
own, a framework's, another SDK's — carries the run and the item, because a
`SpanProcessor` writes them at `on_start` rather than the harness touching a
span it does not hold (spec 018 #3).

The store executes nothing, and neither does this: running the cases is the
user's program.
"""

from __future__ import annotations

import hashlib
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any

from opentelemetry import context as otel_context
from opentelemetry.sdk.trace import SpanProcessor

from . import _config
from ._attributes import ITEM_ID, RUN_ID
from ._errors import TracepadError
from ._http import request
from ._scores import score
from ._tracing import flush

if TYPE_CHECKING:
    from ._datasets import Dataset, Item

#: Where the current attempt sits. The OTel context is one `ContextVar`, so
#: this travels exactly as the current span does — inherited by an `asyncio`
#: task, copied by `contextvars.copy_context()`, and invisible to a thread
#: started without it (spec 018 #3).
ATTEMPT_KEY = "tracepad-attempt"

PAGE = 500


class RunContextProcessor(SpanProcessor):
    """Writes the run and the item on every span started inside an item block.

    The code under test is the application, and its root span is the
    framework's, another SDK's or a decorator's — never the harness's. An
    attribute written at `on_start` lands on all of them, the way a `Resource`
    does but scoped to the block.

    It is the SDK's own base class rather than a duck: a processor is called
    through private hooks too (`_on_ending`), and inheriting is what keeps a
    minor release of the SDK from turning this into an `AttributeError` on
    every span.
    """

    def on_start(self, span: Any, parent_context: Any = None) -> None:
        attempt = otel_context.get_value(ATTEMPT_KEY, parent_context)
        if not isinstance(attempt, Attempt):
            return
        span.set_attribute(RUN_ID, attempt.run_id)
        span.set_attribute(ITEM_ID, attempt.item_id)
        # A root is where a trace begins, and the block records every one it
        # saw start (spec 018 #4).
        if span.parent is None:
            attempt.traces.append(format(span.get_span_context().trace_id, "032x"))


@dataclass
class Attempt:
    """One case, once: what the block stamped and what it produced."""

    run_id: str
    item_id: str
    #: Every root span the processor saw start inside the block, in order. A
    #: case run three times inside one block is three traces of one item, and
    #: the run's summary counts them all (spec 014 #2).
    traces: list[str] = field(default_factory=list)

    @property
    def trace_id(self) -> str | None:
        """The last trace to start — the one a harness would quote."""
        return self.traces[-1] if self.traces else None

    def attributes(self) -> dict[str, str]:
        """The two attributes, for a service the block cannot reach.

        They do not propagate with the trace context, by design (spec 014 #2),
        so a harness calling another process forwards these by its own means.
        """
        return {RUN_ID: self.run_id, ITEM_ID: self.item_id}

    def score(self, name: str, value: float | None = None, **fields: Any) -> None:
        """Score the trace this attempt produced (spec 018 #4)."""
        if self.trace_id is None:
            raise ValueError(
                f"tracepad: no trace has started inside this item block yet ({self.item_id}); "
                "score after the application has run, or pass trace_id= to tracepad.score()"
            )
        score(name, value, trace_id=self.trace_id, **fields)


class Run:
    """An open run: the version it pinned, the blocks it stamps, and its close."""

    def __init__(self, dataset: Dataset, body: dict[str, Any]) -> None:
        self.dataset = dataset
        self.id: str = body["id"]
        self.name: str = body.get("name", "")
        #: The version the harness must fetch by — one number by construction
        #: (spec 014 #7).
        self.dataset_version: int = body["dataset_version"]
        self._closed = False

    @contextmanager
    def item(self, case: Item | str) -> Iterator[Attempt]:
        """Run one case: everything started inside carries the run and the item.

        The block opens no span of its own — a root span the harness opened
        would make every eval trace look like a trace of the harness.
        """
        attempt = Attempt(self.id, getattr(case, "id", case) or "")
        token = otel_context.attach(otel_context.set_value(ATTEMPT_KEY, attempt))
        try:
            yield attempt
        finally:
            # A span started after this is not stamped, whatever the harness
            # still holds: what is left is a record, not a context (#9).
            otel_context.detach(token)

    def finish(self, timeout: float = 30.0) -> dict[str, Any]:
        """Deliver everything the run produced, then close it as finished."""
        return self._close({}, timeout)

    def fail(self, error: str, timeout: float = 30.0) -> dict[str, Any]:
        return self._close({"status": "failed", "error": error}, timeout)

    def _close(self, body: dict[str, Any], timeout: float) -> dict[str, Any]:
        # The flush comes first so that `run.get()` on the next line is over
        # every trace and score the run produced (spec 018 #5). A late span
        # still links, so a flush that timed out is a number read early.
        flush(timeout)
        self._closed = True
        return dict(request(_config.current(), "POST", f"/api/v1/runs/{self.id}/finish",
                            body=body).body or {})

    def get(self) -> dict[str, Any]:
        """The run with its summary, as the server computes it (spec 018 #8)."""
        return dict(request(_config.current(), "GET", f"/api/v1/runs/{self.id}").body or {})

    def items(self, unknown: bool = False) -> Iterator[dict[str, Any]]:
        """The run's cases with the attempts made at each."""
        params: dict[str, Any] = {"limit": PAGE}
        if unknown:
            params["unknown"] = "true"
        return pages(f"/api/v1/runs/{self.id}/items", params, "items")

    def __enter__(self) -> Run:
        return self

    def __exit__(self, kind: Any, error: BaseException | None, traceback: Any) -> bool:
        # A run left `running` is reported as such forever (spec 014 #8), and
        # the harness that crashed between the last case and `finish` is
        # exactly the one that forgot to write the `except`.
        if not self._closed:
            if error is None:
                self.finish()
            else:
                self.fail(repr(error))
        return False


@dataclass(frozen=True)
class ScoreConfig:
    """What a score name means (`docs/scores.md`)."""

    name: str
    data_type: str
    direction: str | None = None
    min: float | None = None
    max: float | None = None
    categories: list[str] | None = None
    description: str | None = None

    def body(self) -> dict[str, Any]:
        fields = ("data_type", "direction", "min", "max", "categories", "description")
        return {f: getattr(self, f) for f in fields if getattr(self, f) is not None}


def score_configs(configs: Any) -> None:
    """Declare what the run's score names mean, before it runs a case.

    Synchronous and loud (spec 018 #6): a `numeric` without a `direction`
    should stop the job at the top rather than fail every score batch quietly
    in the queue. `PUT` is idempotent, so this is safe on every CI run.
    """
    for config in configs:
        name = config["name"] if isinstance(config, dict) else config.name
        body = {k: v for k, v in config.items() if k != "name"} if isinstance(config, dict) \
            else config.body()
        try:
            request(_config.current(), "PUT", f"/api/v1/score-configs/{name}", body=body)
        except TracepadError as error:
            raise TracepadError(f"tracepad: score config {name!r}: {error}") from None


def item_id(key: str) -> str:
    """A stable item or run id from a natural key (spec 018 #7).

    The same derivation `docs/scores.md` shows for score ids, so that two
    harnesses hashing the same key agree.
    """
    return hashlib.sha256(key.encode()).hexdigest()[:32]


def compare(a: str, b: str) -> dict[str, Any]:
    """Two runs side by side, exactly as the server computes it (spec 014 #18)."""
    return dict(request(_config.current(), "GET", f"/api/v1/runs/{a}/compare/{b}").body or {})


def pages(path: str, params: dict[str, Any], key: str) -> Iterator[dict[str, Any]]:
    """Walk a cursor-paged listing to its end.

    The loop lives here because this is where `docs/datasets.md` warns a
    hand-written harness goes wrong: a pass that silently stopped at the first
    page would be recorded as a whole run over a fraction of the cases. The
    first request omits `cursor` rather than sending it empty, which is a 400
    everywhere in this API.
    """
    query = dict(params)
    while True:
        answer = request(_config.current(), "GET", path, params=query).body or {}
        yield from answer.get(key) or []
        cursor = answer.get("next_cursor")
        if not cursor:
            return
        query["cursor"] = cursor
