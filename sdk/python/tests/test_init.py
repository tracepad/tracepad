"""`init` adapts to the provider it finds (spec 017 #2, #10)."""

from __future__ import annotations

import logging
from typing import Any

import pytest
from opentelemetry import trace as otel_api
from opentelemetry.sdk.trace import TracerProvider

import tracepad
from conftest import HOST, KEY
from tracepad import _tracing


def processors(provider: Any) -> list[Any]:
    return list(provider._active_span_processor._span_processors)


def test_creates_a_provider_when_there_is_none() -> None:
    tracepad.init(HOST, KEY)
    provider = otel_api.get_tracer_provider()
    assert isinstance(provider, TracerProvider)
    assert len(processors(provider)) == 1


def test_adds_a_processor_to_the_application_s_provider() -> None:
    application = TracerProvider()
    otel_api.set_tracer_provider(application)

    tracepad.init(HOST, KEY)

    assert otel_api.get_tracer_provider() is application
    assert len(processors(application)) == 1


def test_a_second_init_is_a_no_op() -> None:
    tracepad.init(HOST, KEY)
    provider = otel_api.get_tracer_provider()

    tracepad.init("http://elsewhere.test", "tp-sk-other")

    assert len(processors(provider)) == 1
    assert tracepad._config.current().host == HOST


def test_export_false_attaches_no_exporter() -> None:
    tracepad.init(HOST, KEY, export=False)
    assert processors(otel_api.get_tracer_provider()) == []


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
