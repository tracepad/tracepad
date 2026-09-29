"""Fixtures: a clean process for every test.

`init`, the global `TracerProvider` and the score queue are all process-wide
by design — the OTel API refuses to set a provider twice on purpose — so the
suite resets them between tests rather than pretending they are not. The reset
and the capture are the public `tracepad.testing` (spec 040 #7): the suite
exercises the helpers an application tests with, and the environment scrub
is all that stays local.
"""

from __future__ import annotations

from collections.abc import Iterator

import pytest

from tracepad import testing

# `pytester` runs the suites that check the opt-in plugin of `tracepad.testing`.
pytest_plugins = ["pytester"]


@pytest.fixture(autouse=True)
def fresh(monkeypatch: pytest.MonkeyPatch) -> Iterator[None]:
    for variable in (
        "TRACEPAD_URL",
        "TRACEPAD_HOST",
        "TRACEPAD_API_KEY",
        "TRACEPAD_ENVIRONMENT",
        "TRACEPAD_RELEASE",
        "OTEL_SERVICE_NAME",
        "OTEL_RESOURCE_ATTRIBUTES",
        # A developer's own bound would move every test that times an export.
        "TRACEPAD_EXPORT_TIMEOUT",
        "OTEL_EXPORTER_OTLP_TRACES_TIMEOUT",
        "OTEL_EXPORTER_OTLP_TIMEOUT",
    ):
        monkeypatch.delenv(variable, raising=False)
    # A proxy would stand between the suite and the hosts it watches or
    # times; `NO_PROXY` also keeps urllib off the macOS system proxy.
    for variable in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"):
        monkeypatch.delenv(variable, raising=False)
        monkeypatch.delenv(variable.lower(), raising=False)
    monkeypatch.setenv("NO_PROXY", "*")
    monkeypatch.setenv("no_proxy", "*")
    # The deprecated-host warning is once a process; every test starts unwarned.
    monkeypatch.setattr("tracepad._config._host_warned", False)
    testing.reset()
    yield
    testing.reset()


@pytest.fixture
def spans() -> Iterator[testing.Capture]:
    """A provider of our own, adopted by `init`, recording into memory."""
    with testing.capture() as captured:
        yield captured
