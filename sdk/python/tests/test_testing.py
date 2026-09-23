"""`tracepad.testing`: the capture, the reset, the opt-in plugin (spec 040)."""

from __future__ import annotations

import urllib.request
from typing import Any

import pytest
import requests
from opentelemetry import trace as otel_api

import tracepad
from tracepad import testing


def test_a_capture_records_the_spans_in_order_and_the_scores() -> None:
    with testing.capture() as captured:
        with tracepad.span("handler") as handler:
            with tracepad.span("retrieve"):
                pass
            tracepad.score("helpful", 0.9)

    assert [span.name for span in captured.spans] == ["retrieve", "handler"]
    assert captured.attributes("handler")["tracepad.observation.type"] == "span"
    assert captured.scores == [{"name": "helpful", "trace_id": handler.trace_id, "value": 0.9}]


def test_one_fails_naming_the_spans_there_were() -> None:
    with testing.capture() as captured:
        for name in ("a", "a", "b"):
            with tracepad.span(name):
                pass

    with pytest.raises(AssertionError, match=r"want one span named 'a', got \['a', 'a', 'b'\]"):
        captured.one("a")
    with pytest.raises(AssertionError, match="named 'c'"):
        captured.one("c")


def test_nothing_reaches_the_network(monkeypatch: pytest.MonkeyPatch) -> None:
    def refuse(*_: Any, **__: Any) -> Any:
        pytest.fail("a capture made a network call")

    monkeypatch.setattr(urllib.request, "urlopen", refuse)
    monkeypatch.setattr(requests.Session, "request", refuse)
    with testing.capture() as captured:
        with tracepad.generation("chat", model="gpt-4o-mini") as call:
            call.end(output="hi", usage={"input": 3, "output": 1})
            tracepad.score("helpful", 1, observation=True)
        tracepad.flush(1.0)

    assert captured.one("chat").attributes is not None
    assert len(captured.scores) == 1


def test_leaving_a_capture_leaves_a_process_that_never_initialised() -> None:
    with testing.capture():
        pass

    with tracepad.span("after") as step:
        tracepad.score("helpful", 1)  # a no-op, not a raise (spec 039)
    assert (step.trace_id, step.span_id) == (None, None)


def test_two_captures_in_a_row_see_only_their_own() -> None:
    with testing.capture() as first, tracepad.span("first"):
        tracepad.score("one", 1)
    with testing.capture() as second, tracepad.span("second"):
        tracepad.score("two", 2)

    assert [span.name for span in first.spans] == ["first"]
    assert [span.name for span in second.spans] == ["second"]
    assert [score["name"] for score in second.scores] == ["two"]


def test_reset_undoes_the_global_provider_the_api_sets_once() -> None:
    # The reset reaches into `opentelemetry.trace`'s private globals (spec 040
    # #3): a release that renames them fails here, not in an application's suite.
    with testing.capture():
        assert not isinstance(otel_api.get_tracer_provider(), otel_api.ProxyTracerProvider)
    assert isinstance(otel_api.get_tracer_provider(), otel_api.ProxyTracerProvider)


def test_reset_alone_is_tracing_off() -> None:
    tracepad.init("http://tracepad.test:4318", "tp-sk-test", export=False)
    testing.reset()

    with tracepad.span("off") as step:
        tracepad.score("helpful", 1)
    assert step.trace_id is None


SUITE = """
def test_fixtures(tracepad_capture, tracepad_off):
    pass
"""


def test_a_suite_that_opts_in_gets_both_fixtures(pytester: pytest.Pytester) -> None:
    pytester.makeconftest('pytest_plugins = ["tracepad.testing"]')
    pytester.makepyfile(SUITE)
    pytester.runpytest_subprocess().assert_outcomes(passed=1)


def test_a_suite_that_does_not_is_untouched(pytester: pytest.Pytester) -> None:
    pytester.makepyfile(SUITE)
    result = pytester.runpytest_subprocess()
    result.assert_outcomes(errors=1)
    result.stdout.fnmatch_lines(["*fixture 'tracepad_capture' not found*"])
