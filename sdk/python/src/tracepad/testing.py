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

import threading
from collections.abc import Iterator
from typing import Any

from opentelemetry import trace as otel
from opentelemetry.sdk.trace import ReadableSpan, TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from . import _config, _prompts, _scores, _tracestate, _tracing

# A reserved name (RFC 2606), so nothing resolves it: export is off and the
# queue does not post, and a REST call a test forgot to stub fails loudly.
_HOST = "http://tracepad.test:4318"
_KEY = "tp-sk-test"

__all__ = ["Capture", "capture", "reset"]

# How long a reset waits for the provider `init` built to shut down, in seconds.
_SHUTDOWN_WAIT = 5.0

# The provider the follower records into: a capture's, or the one `init` built
# after a reset.
_target: TracerProvider | None = None


class _Following(otel.Tracer):
    """A tracer that asks at every span where spans go now: the capture's
    provider, or one the application set after the reset, or nowhere."""

    def __init__(self, args: tuple[Any, ...], kwargs: dict[str, Any]) -> None:
        self._args, self._kwargs = args, kwargs
        # One tuple, swapped whole: a thread never sees a provider with the
        # tracer of the one before it.
        self._bound: tuple[Any, otel.Tracer] = (None, otel.NoOpTracer())

    def _now(self) -> otel.Tracer:
        provider = _target or otel.get_tracer_provider()
        if provider is _FOLLOW:
            return otel.NoOpTracer()
        bound = self._bound
        if bound[0] is not provider:
            bound = self._bound = (provider, provider.get_tracer(*self._args, **self._kwargs))
        return bound[1]

    def start_span(self, *args: Any, **kwargs: Any) -> otel.Span:
        return self._now().start_span(*args, **kwargs)

    def start_as_current_span(self, *args: Any, **kwargs: Any) -> Any:
        return self._now().start_as_current_span(*args, **kwargs)


class _Follow(otel.NoOpTracerProvider):
    """The global provider a reset leaves (spec 040 #14). The API binds a
    tracer taken before any provider — `trace.get_tracer(__name__)` at import
    — to the first provider set, for good; binding it to this one instead
    keeps it recording in every capture after. With no capture it is the
    no-op the package reads as tracing off."""

    def get_tracer(self, *args: Any, **kwargs: Any) -> otel.Tracer:
        return _Following(args, kwargs)

    def __getattr__(self, name: str) -> Any:
        # `resource`, `force_flush`, `add_span_processor`: the provider it
        # records into answers, as it would as the global.
        if _target is None:
            raise AttributeError(name)
        return getattr(_target, name)

    def follow(self, provider: TracerProvider) -> None:
        """Record into `provider`, and refuse another global provider as a
        process that set one does; a capture and `init` call it."""
        global _target
        _target = provider
        otel._TRACER_PROVIDER_SET_ONCE.do_once(lambda: None)


_FOLLOW = _Follow()
# Imported before anything set a provider — a conftest that names the plugin,
# before a session fixture calls `init` — the follower goes in at once: a
# tracer taken at import binds to the first provider set, for good.
if otel._TRACER_PROVIDER is None:
    otel._TRACER_PROVIDER = _FOLLOW


def reset() -> None:
    """Return the process to never initialised: no `init`, no provider of the
    package's, no cached configuration or prompts, no score queue (spec 040 #2)."""
    global _target
    # The API's own "set once" guard, private to `opentelemetry.trace`; the
    # package's test of this function fails if a release moves it.
    otel._TRACER_PROVIDER = _FOLLOW
    otel._TRACER_PROVIDER_SET_ONCE = otel.Once()
    if _target is not None and _target is not _tracing._built:
        _target.shutdown()
    if _tracing._built is not None:
        # Its spans go to the store the test configured, as at exit; a store
        # that is away does not hold the teardown for the exporter's retries.
        shutdown = threading.Thread(target=_tracing._built.shutdown, daemon=True)
        shutdown.start()
        shutdown.join(_SHUTDOWN_WAIT)
    _target = _tracing._built = None
    _tracing._initialized = False
    _tracing._warned_kinds.clear()
    _tracestate.reset()
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
        # Shut down by the next reset, not at exit: an exit hook would keep
        # every capture's spans alive until the interpreter ends.
        provider = TracerProvider(shutdown_on_exit=False)
        provider.add_span_processor(SimpleSpanProcessor(self._exporter))
        _FOLLOW.follow(provider)
        # A configuration of its own, not the environment's: nothing is sent,
        # and TRACEPAD_ENVIRONMENT would only draw `init`'s resource warning.
        _tracing._attach(provider, _config.Config(host=_HOST, key=_KEY), export=False)
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
