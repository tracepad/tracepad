"""`tracepad.testing`: the capture, the reset, the opt-in plugin (spec 040)."""

from __future__ import annotations

import logging
import subprocess
import sys
import time
import urllib.request
from typing import Any

import pytest
import requests
from opentelemetry import trace as otel_api
from opentelemetry.sdk.trace import TracerProvider

import tracepad
from tracepad import _scores, _tracing, testing


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


def test_a_tracer_taken_at_import_records_in_every_capture() -> None:
    tracer = otel_api.get_tracer("app")  # before any provider, as a module does

    for name in ("first", "second"):
        with testing.capture() as captured, tracer.start_as_current_span(name):
            with tracepad.span("step"):
                pass
        assert [span.name for span in captured.spans] == ["step", name]
        step, root = captured.spans
        assert step.parent is not None and step.parent.span_id == root.context.span_id


def test_a_tracer_first_used_under_init_still_records_in_later_captures() -> None:
    # What `trace.get_tracer()` hands out at import, before any provider: a
    # proxy that binds to the global provider at its first span, for good.
    tracer = otel_api._PROXY_TRACER_PROVIDER.get_tracer("app")
    tracepad.init("http://tracepad.test:4318", "tp-sk-test", export=False)
    with tracer.start_as_current_span("under init") as span:
        assert span.is_recording()
    testing.reset()
    assert not tracer.start_span("off").is_recording()
    with testing.capture() as captured, tracer.start_as_current_span("captured"):
        pass
    assert [span.name for span in captured.spans] == ["captured"]


def test_a_capture_holds_the_global_provider_as_a_process_does() -> None:
    # The reset reaches into `opentelemetry.trace`'s private globals (spec 040
    # #3): a release that renames them fails here, not in an application's suite.
    with testing.capture() as captured:
        otel_api.set_tracer_provider(TracerProvider())  # an app factory's, refused
        with tracepad.span("answer"):
            pass
    assert [span.name for span in captured.spans] == ["answer"]
    testing.reset()
    own = TracerProvider()
    otel_api.set_tracer_provider(own)  # a reset process takes one again
    assert otel_api.get_tracer_provider() is own


def test_the_reset_shuts_down_the_provider_init_built(monkeypatch: pytest.MonkeyPatch) -> None:
    tracepad.init("http://tracepad.test:4318", "tp-sk-test")  # exporting, batched
    built = _tracing._built
    shut: list[bool] = []
    monkeypatch.setattr(built, "shutdown", lambda: shut.append(True))
    testing.reset()
    assert shut == [True]


def test_the_reset_waits_for_that_shutdown_only_so_long(monkeypatch: pytest.MonkeyPatch) -> None:
    tracepad.init("http://tracepad.test:4318", "tp-sk-test")
    monkeypatch.setattr(_tracing._built, "shutdown", lambda: time.sleep(2))  # a store away
    monkeypatch.setattr(testing, "_SHUTDOWN_WAIT", 0.05)
    started = time.monotonic()
    testing.reset()
    assert time.monotonic() - started < 1


def test_an_init_before_the_first_reset_does_not_keep_the_tracers() -> None:
    # A session fixture that calls `init` before any capture, in a process of
    # its own: the plugin's import put the follower in first.
    script = """
from opentelemetry import trace
from tracepad import testing
import tracepad
tracer = trace.get_tracer("app")
tracepad.init("http://tracepad.test:4318", "tp-sk-test", export=False)
tracer.start_span("under init").end()
with testing.capture() as captured:
    tracer.start_span("captured").end()
print([span.name for span in captured.spans])
"""
    out = subprocess.run([sys.executable, "-c", script], capture_output=True, text=True, check=True)
    assert out.stdout.strip() == "['captured']"


def test_a_capture_reads_nothing_from_the_environment(
    monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    monkeypatch.setenv("TRACEPAD_ENVIRONMENT", "ci")
    monkeypatch.setenv("TRACEPAD_RELEASE", "1.2.3")
    with caplog.at_level(logging.WARNING, logger="tracepad"), testing.capture():
        pass
    assert caplog.records == []


def test_a_queue_the_reset_replaced_sends_nothing_after_it() -> None:
    sent: list[list[dict[str, Any]]] = []
    _scores.reset(_scores.ScoreQueue(sent.append, interval=0.05))
    tracepad.score("pending", 1, trace_id="a" * 32)
    testing.reset()
    time.sleep(0.2)
    assert sent == []


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
