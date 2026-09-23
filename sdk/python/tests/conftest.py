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
        "TRACEPAD_HOST",
        "TRACEPAD_API_KEY",
        "TRACEPAD_ENVIRONMENT",
        "TRACEPAD_RELEASE",
        "OTEL_SERVICE_NAME",
        "OTEL_RESOURCE_ATTRIBUTES",
    ):
        monkeypatch.delenv(variable, raising=False)
    testing.reset()
    yield
    testing.reset()


@pytest.fixture
def spans() -> Iterator[testing.Capture]:
    """A provider of our own, adopted by `init`, recording into memory."""
    with testing.capture() as captured:
        yield captured
