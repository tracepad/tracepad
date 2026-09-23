"""`init` adapts to the provider it finds (spec 017 #2, #10)."""

from __future__ import annotations

import logging
from typing import Any

import pytest
from opentelemetry import trace as otel_api
from opentelemetry.sdk.trace import TracerProvider

import tracepad
from tracepad import _tracing

HOST = "http://tracepad.test:4318"
KEY = "tp-sk-test"


def processors(provider: Any) -> list[str]:
    """What `init` attached, in the order it attached them."""
    return [type(p).__name__ for p in provider._active_span_processor._span_processors]


# The stamping processor of spec 018 goes on first, so that every span the
# exporter batches already carries the run and the item it was stamped with.
ATTACHED = ["RunContextProcessor", "BatchSpanProcessor"]


def test_creates_a_provider_when_there_is_none(monkeypatch: pytest.MonkeyPatch) -> None:
    # A process no test reset ran in: nothing has set the global provider.
    monkeypatch.setattr(otel_api, "_TRACER_PROVIDER", None)
    monkeypatch.setattr(otel_api, "_TRACER_PROVIDER_SET_ONCE", otel_api.Once())
    tracepad.init(HOST, KEY)
    provider = otel_api.get_tracer_provider()
    assert isinstance(provider, TracerProvider)
    assert processors(provider) == ATTACHED


def test_adds_its_processors_to_the_application_s_provider() -> None:
    application = TracerProvider()
    otel_api.set_tracer_provider(application)

    tracepad.init(HOST, KEY)

    assert otel_api.get_tracer_provider() is application
    assert processors(application) == ATTACHED


def test_a_second_init_is_a_no_op() -> None:
    tracepad.init(HOST, KEY)
    provider = otel_api.get_tracer_provider()

    tracepad.init("http://elsewhere.test", "tp-sk-other")

    assert processors(provider) == ATTACHED
    assert tracepad._config.current().host == HOST


def test_export_false_attaches_no_exporter_and_still_stamps() -> None:
    # The application exporting through another SDK still wants its spans
    # stamped by an eval (spec 018 #3, edge cases).
    tracepad.init(HOST, KEY, export=False)
    assert processors(otel_api.get_tracer_provider()) == ["RunContextProcessor"]


def test_the_environment_supplies_host_and_key(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("TRACEPAD_HOST", HOST + "/")
    monkeypatch.setenv("TRACEPAD_API_KEY", KEY)
    monkeypatch.setenv("TRACEPAD_ENVIRONMENT", "staging")
    monkeypatch.setenv("TRACEPAD_RELEASE", "2026.9.4")

    tracepad.init()

    config = tracepad._config.current()
    assert (config.host, config.key) == (HOST, KEY)
    assert config.traces_endpoint == HOST + "/v1/traces"
    resource = otel_api.get_tracer_provider().resource.attributes
    assert resource["deployment.environment.name"] == "staging"
    assert resource["service.version"] == "2026.9.4"


def test_no_host_and_no_key_raise() -> None:
    with pytest.raises(tracepad.TracepadConfigError) as raised:
        tracepad.init()
    assert "host" in str(raised.value) and "key" in str(raised.value)
    assert not _tracing._initialized


def test_the_service_name_falls_back_to_the_process() -> None:
    tracepad.init(HOST, KEY)
    name = otel_api.get_tracer_provider().resource.attributes["service.name"]
    assert name and name != "unknown_service"


def test_the_service_name_of_the_environment_wins(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("OTEL_SERVICE_NAME", "support-bot")
    tracepad.init(HOST, KEY)
    assert otel_api.get_tracer_provider().resource.attributes["service.name"] == "support-bot"


def test_environment_and_release_are_refused_on_a_provider_we_did_not_build(
    caplog: pytest.LogCaptureFixture,
) -> None:
    otel_api.set_tracer_provider(TracerProvider())
    with caplog.at_level(logging.WARNING, logger="tracepad"):
        tracepad.init(HOST, KEY, environment="prod", release="1.2.3")
    assert "OTEL_RESOURCE_ATTRIBUTES" in caplog.text
