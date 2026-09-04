"""Fixtures: a clean process for every test.

`init`, the global `TracerProvider` and the score queue are all process-wide
by design — the OTel API refuses to set a provider twice on purpose — so the
suite resets them between tests rather than pretending they are not.
"""

from __future__ import annotations

from typing import Any

import pytest
from opentelemetry import trace as otel_api
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

import tracepad
from tracepad import _config, _prompts, _scores, _tracing

HOST = "http://tracepad.test:4318"
KEY = "tp-sk-test"


def reset_otel() -> None:
    """Forget the global provider, the way a fresh process has none."""
    otel_api._TRACER_PROVIDER = None
    otel_api._TRACER_PROVIDER_SET_ONCE = otel_api.Once()


@pytest.fixture(autouse=True)
def fresh(monkeypatch: pytest.MonkeyPatch) -> Any:
    for variable in (
        "TRACEPAD_HOST",
        "TRACEPAD_API_KEY",
        "TRACEPAD_ENVIRONMENT",
        "TRACEPAD_RELEASE",
        "OTEL_SERVICE_NAME",
        "OTEL_RESOURCE_ATTRIBUTES",
    ):
        monkeypatch.delenv(variable, raising=False)
    reset_otel()
    _tracing._initialized = False
    _config.forget()
    _prompts.forget()
    _scores.reset()
    yield
    reset_otel()
    _tracing._initialized = False
    _scores.reset()


@pytest.fixture
def spans() -> Any:
    """A provider of our own, adopted by `init`, exporting into memory."""
    exporter = InMemorySpanExporter()
    provider = TracerProvider()
    provider.add_span_processor(SimpleSpanProcessor(exporter))
    otel_api.set_tracer_provider(provider)
    tracepad.init(HOST, KEY, export=False)
    return Spans(exporter)


class Spans:
    """The exported spans, by name."""

    def __init__(self, exporter: InMemorySpanExporter) -> None:
        self._exporter = exporter

    def all(self) -> list[Any]:
        return list(self._exporter.get_finished_spans())

    def one(self, name: str | None = None) -> Any:
        found = [s for s in self.all() if name is None or s.name == name]
        assert len(found) == 1, f"want one span named {name!r}, got {[s.name for s in self.all()]}"
        return found[0]

    def attributes(self, name: str | None = None) -> dict[str, Any]:
        return dict(self.one(name).attributes or {})
