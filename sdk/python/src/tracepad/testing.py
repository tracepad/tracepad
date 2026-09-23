"""Testing an application's instrumentation (spec 040).

    from tracepad.testing import capture

    def test_the_answer_is_traced():
        with capture() as captured:
            answer("why is the sky blue")
        assert captured.one("answer").attributes["tracepad.observation.type"] == "span"
        assert captured.scores[0]["name"] == "helpful"

`capture()` gives a fresh, initialised process that records instead of
exporting; `reset()` gives one that never initialised — tracing off, as spec
039 defines it. A suite on pytest opts into the two as fixtures with
`pytest_plugins = ["tracepad.testing"]` in its `conftest.py`.

Everything the package keeps process-wide is reset here and nowhere else,
OpenTelemetry's global provider included — which the API lets a process set
once, and a test suite has to set once per test (spec 040 #3). The tracing
path never imports this module.
"""

from __future__ import annotations

from collections.abc import Iterator
from typing import Any

from opentelemetry import trace as otel
from opentelemetry.sdk.trace import ReadableSpan, TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from . import _config, _prompts, _scores, _tracing

# A reserved name (RFC 2606), so nothing resolves it: export is off and the
# queue does not post, and a REST call a test forgot to stub fails loudly.
_HOST = "http://tracepad.test:4318"
_KEY = "tp-sk-test"

__all__ = ["Capture", "capture", "reset"]


def reset() -> None:
    """Return the process to never initialised: no `init`, no global provider,
    no cached configuration or prompts, no score queue (spec 040 #2)."""
    # The API's own "set once" guard, private to `opentelemetry.trace`; the
    # package's test of this function fails if a release moves it.
    otel._TRACER_PROVIDER = None
    otel._TRACER_PROVIDER_SET_ONCE = otel.Once()
    _tracing._initialized = False
    _tracing._warned_kinds.clear()
    _config.forget()
    _prompts.forget()
    _scores.reset()


class _Kept(_scores.ScoreQueue):
    """A queue that keeps each score's body where the test can read it."""

    def __init__(self, kept: list[dict[str, Any]]) -> None:
        super().__init__(kept.extend)

    def submit(self, score: dict[str, Any]) -> None:
        self._send([score])


class Capture:
    """What the code under test traced and scored, since `capture()`."""

    def __init__(self) -> None:
        reset()
        self._exporter = InMemorySpanExporter()
        provider = TracerProvider()
        provider.add_span_processor(SimpleSpanProcessor(self._exporter))
        otel.set_tracer_provider(provider)
        _tracing.init(_HOST, _KEY, export=False)
        # The bodies `score()` would have posted, in order.
        self.scores: list[dict[str, Any]] = []
        _scores.reset(_Kept(self.scores))

    @property
    def spans(self) -> list[ReadableSpan]:
        """Every finished span, in the order it ended."""
        return list(self._exporter.get_finished_spans())

    def one(self, name: str) -> ReadableSpan:
        """The single finished span of that name; fails naming what there was."""
        found = [s for s in self.spans if s.name == name]
        if len(found) != 1:
            names = [s.name for s in self.spans]
            raise AssertionError(f"want one span named {name!r}, got {names}")
        return found[0]

    def attributes(self, name: str) -> dict[str, Any]:
        return dict(self.one(name).attributes or {})

    def __enter__(self) -> Capture:
        return self

    def __exit__(self, *_: object) -> None:
        reset()


def capture() -> Capture:
    """Reset the process, then trace into memory until the `with` block ends
    (spec 040 #1). The spans are OpenTelemetry's own `ReadableSpan`."""
    return Capture()


try:
    import pytest
except ImportError:  # the helpers serve a suite on `unittest` too
    pass
else:

    @pytest.fixture
    def tracepad_capture() -> Iterator[Capture]:
        """A `Capture`, reset around the test."""
        with capture() as captured:
            yield captured

    @pytest.fixture
    def tracepad_off() -> Iterator[None]:
        """A process that never initialised: tracing off (spec 039)."""
        reset()
        yield
        reset()
