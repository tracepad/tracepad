"""`@observe`, the context managers, and what they capture (spec 017 #4, #11)."""

from __future__ import annotations

import asyncio
import json
import logging
from typing import Any

import pytest
from opentelemetry import trace as otel_api
from opentelemetry.trace import StatusCode

import tracepad
from tracepad import _attributes as attrs


def test_a_sync_function_becomes_a_span(spans: Any) -> None:
    @tracepad.observe
    def add(a: int, b: int = 2) -> int:
        return a + b

    assert add(1, b=3) == 4

    span = spans.one("add")
    assert json.loads(span.attributes[attrs.INPUT]) == {"a": 1, "b": 3}
    assert span.attributes[attrs.OUTPUT] == "4"
    assert span.attributes[attrs.OBSERVATION_TYPE] == "span"


def test_the_decorator_takes_arguments(spans: Any) -> None:
    @tracepad.observe(name="renamed", type="tool")
    def work() -> str:
        return "done"

    work()
    assert spans.one("renamed").attributes[attrs.OBSERVATION_TYPE] == "tool"


def test_self_is_dropped(spans: Any) -> None:
    class Agent:
        @tracepad.observe
        def step(self, question: str) -> str:
            return "answer"

    Agent().step("why?")
    assert json.loads(spans.attributes("step")[attrs.INPUT]) == {"question": "why?"}


def test_the_opt_outs_leave_the_attributes_unset(spans: Any) -> None:
    @tracepad.observe(capture_input=False, capture_output=False)
    def quiet(secret: str) -> str:
        return secret

    quiet("s3cret")
    attributes = spans.attributes("quiet")
    assert attrs.INPUT not in attributes
    assert attrs.OUTPUT not in attributes


def test_update_inside_replaces(spans: Any) -> None:
    @tracepad.observe
    def step(question: str) -> str:
        tracepad.update(input="redacted", output="also redacted", level="WARNING",
                        status_message="degraded", metadata={"attempt": 2})
        return "answer"

    step("why?")
    attributes = spans.attributes("step")
    assert attributes[attrs.INPUT] == "redacted"
    assert attributes[attrs.OUTPUT] == "also redacted"
    assert attributes[attrs.OBSERVATION_LEVEL] == "WARNING"
    assert attributes[attrs.OBSERVATION_STATUS_MESSAGE] == "degraded"
    assert json.loads(attributes[attrs.OBSERVATION_METADATA]) == {"attempt": 2}


def test_an_exception_ends_the_span_as_an_error_and_propagates(spans: Any) -> None:
    @tracepad.observe
    def fails() -> None:
        raise ValueError("no")

    with pytest.raises(ValueError, match="no"):
        fails()

    span = spans.one("fails")
    assert span.status.status_code is StatusCode.ERROR
    assert [event.name for event in span.events] == ["exception"]


def test_an_async_function(spans: Any) -> None:
    @tracepad.observe
    async def fetch(url: str) -> str:
        await asyncio.sleep(0)
        return "body"

    assert asyncio.run(fetch("http://x")) == "body"
    attributes = spans.attributes("fetch")
    assert json.loads(attributes[attrs.INPUT]) == {"url": "http://x"}
    assert attributes[attrs.OUTPUT] == "body"


def test_a_sync_generator_ends_when_it_is_exhausted(spans: Any) -> None:
    @tracepad.observe
    def stream() -> Any:
        yield "a"
        yield "b"

    assert list(stream()) == ["a", "b"]
    assert json.loads(spans.attributes("stream")[attrs.OUTPUT]) == ["a", "b"]


def test_a_generator_closed_early_keeps_what_it_yielded(spans: Any) -> None:
    @tracepad.observe
    def stream() -> Any:
        yield "a"
        yield "b"

    for value in stream():
        assert value == "a"
        break

    assert json.loads(spans.attributes("stream")[attrs.OUTPUT]) == ["a"]


def test_an_async_generator(spans: Any) -> None:
    @tracepad.observe
    async def stream() -> Any:
        yield 1
        yield 2

    async def drain() -> list[int]:
        return [value async for value in stream()]

    assert asyncio.run(drain()) == [1, 2]
    assert json.loads(spans.attributes("stream")[attrs.OUTPUT]) == [1, 2]


def test_an_argument_that_is_not_json_is_repr_d(spans: Any) -> None:
    class Opaque:
        def __repr__(self) -> str:
            return "<opaque>"

    @tracepad.observe
    def step(thing: Any) -> str:
        return "ok"

    step(Opaque())
    assert json.loads(spans.attributes("step")[attrs.INPUT]) == {"thing": "<opaque>"}


def test_span_and_event(spans: Any) -> None:
    with tracepad.span("outer", input={"q": 1}, metadata={"k": "v"}) as observation:
        assert observation.trace_id is not None and len(observation.trace_id) == 32
        assert observation.span_id is not None and len(observation.span_id) == 16
        with tracepad.event("cache.miss"):
            pass

    outer = spans.one("outer")
    marker = spans.one("cache.miss")
    assert outer.attributes[attrs.OBSERVATION_TYPE] == "span"
    assert json.loads(outer.attributes[attrs.INPUT]) == {"q": 1}
    assert marker.attributes[attrs.OBSERVATION_TYPE] == "event"
    assert marker.start_time == marker.end_time
    assert marker.parent.span_id == outer.context.span_id


def test_a_span_takes_its_kind_when_it_opens(spans: Any) -> None:
    # Spec 038 #1: one call, not a span retyped by `update` afterwards.
    with tracepad.span("docs-search", type="retriever"):
        pass

    assert spans.attributes("docs-search")[attrs.OBSERVATION_TYPE] == "retriever"


def test_an_unknown_kind_warns_and_is_still_written(
    spans: Any, caplog: pytest.LogCaptureFixture
) -> None:
    # Written all the same: the mapper keeps it in the observation's metadata.
    # Once per spelling, whichever door: a step in a loop says it once.
    with caplog.at_level(logging.WARNING, logger="tracepad"):
        for _ in range(3):
            with tracepad.span("lookup", type="retreiver"):
                tracepad.update(type="retreiver")

        @tracepad.observe(type="retriver")
        def lookup() -> None:
            pass

        # Not at decoration: the application may not have configured its
        # logging yet. On the call.
        assert caplog.text.count("is not one of the observation types") == 1
        lookup()
        lookup()

    assert caplog.text.count("'retreiver' is not one of the observation types") == 1
    assert caplog.text.count("'retriver' is not one of the observation types") == 1
    kinds = [s.attributes[attrs.OBSERVATION_TYPE] for s in spans.all()]
    assert kinds == ["retreiver"] * 3 + ["retriver"] * 2


def test_a_kind_is_remembered_only_after_init(caplog: pytest.LogCaptureFixture) -> None:
    # Before `init` the warning is said again after it (spec 038 #5).
    with caplog.at_level(logging.WARNING, logger="tracepad"):
        with tracepad.span("early", type="retreiver"):
            pass
        with tracepad.span("early", type="retreiver"):
            pass
    assert caplog.text.count("is not one of the observation types") == 2


def test_an_empty_version_is_no_version(spans: Any) -> None:
    # As Go's `UpdateTrace` skips an empty string.
    with tracepad.span("handler"):
        tracepad.update_trace(version="")

    assert attrs.TRACE_VERSION not in spans.attributes("handler")


def test_an_empty_kind_is_the_default(spans: Any, caplog: pytest.LogCaptureFixture) -> None:
    # As Go's `WithType("")`, at every door: forwarding an optional kind opens
    # a plain step, and `update` leaves the kind where it was.
    with caplog.at_level(logging.WARNING, logger="tracepad"):
        with tracepad.span("forwarded", type=None):
            pass
        with tracepad.span("blank", type=""):
            tracepad.update(type="")

        @tracepad.observe(type="")
        def decorated() -> None:
            pass

        decorated()

    assert caplog.text == ""
    for name in ("forwarded", "blank", "decorated"):
        assert spans.attributes(name)[attrs.OBSERVATION_TYPE] == "span"


def test_span_as_a_generation_hands_out_a_generation(spans: Any) -> None:
    # Spec 038 #8: the one kind with a handle of its own, as
    # `@observe(type="generation")` does.
    with tracepad.span("chat", type="generation", metadata={"attempt": 1}) as call:
        call.first_token()
        call.end(model="gpt-4o-mini", usage={"input_tokens": 3})

    chat = spans.one("chat")
    assert chat.attributes[attrs.OBSERVATION_TYPE] == "generation"
    assert chat.attributes[attrs.RESPONSE_MODEL] == "gpt-4o-mini"
    assert json.loads(chat.attributes[attrs.OBSERVATION_METADATA]) == {"attempt": 1}


def test_update_trace_writes_the_trace_level_names(spans: Any) -> None:
    with tracepad.span("handler"):
        tracepad.update_trace(
            name="support-chat",
            user_id="u-42",
            session_id="s-7",
            tags=["support", "beta"],
            metadata={"channel": "web"},
            version="retrieval-v2",
        )

    attributes = spans.attributes("handler")
    assert attributes[attrs.TRACE_NAME] == "support-chat"
    assert attributes[attrs.USER_ID] == "u-42"
    assert attributes[attrs.SESSION_ID] == "s-7"
    assert json.loads(attributes[attrs.TRACE_TAGS]) == ["support", "beta"]
    assert json.loads(attributes[attrs.TRACE_METADATA]) == {"channel": "web"}
    assert attributes[attrs.TRACE_VERSION] == "retrieval-v2"


def test_update_outside_a_span_warns_and_writes_nothing(
    spans: Any, caplog: pytest.LogCaptureFixture
) -> None:
    with caplog.at_level(logging.WARNING, logger="tracepad"):
        tracepad.update(name="nowhere")
        tracepad.update_trace(user_id="u-1")
    assert caplog.text.count("outside a span") == 2
    assert spans.all() == []


def test_dumps_of_a_string_is_the_string() -> None:
    assert attrs.dumps("plain text") == "plain text"
    assert attrs.dumps({"a": 1}) == '{"a":1}'


def test_a_value_json_cannot_express_at_all_is_still_a_string(spans: Any) -> None:
    # `default=repr` is never consulted for a dict key, nor for a cycle, so
    # these two are what is left under it. The decorator serializes its
    # arguments *before* the call, so a raise here would break the function.
    cycle: list[Any] = []
    cycle.append(cycle)
    keyed = {("a", "b"): 1}

    assert attrs.dumps(cycle) == "[[...]]"
    assert "'a', 'b'" in attrs.dumps(keyed)

    @tracepad.observe
    def step(loop: Any, mapping: Any) -> str:
        return "ok"

    assert step(cycle, keyed) == "ok"
    assert attrs.INPUT in spans.attributes("step")


def test_a_partly_read_generator_does_not_leave_its_span_current(spans: Any) -> None:
    # A generator runs in its consumer's context: a span held current across a
    # `yield` would parent the consumer's next span to a step it merely
    # touched, and two of them would restore each other out of order.
    @tracepad.observe
    def stream(label: str) -> Any:
        yield f"{label}-1"
        yield f"{label}-2"

    first, second = stream("a"), stream("b")
    assert (next(first), next(second)) == ("a-1", "b-1")
    assert not otel_api.get_current_span().get_span_context().is_valid

    with tracepad.span("after"):
        pass
    list(first)
    list(second)

    after = spans.one("after")
    assert after.parent is None
    assert len({span.context.trace_id for span in spans.all()}) == 3


def test_a_span_started_inside_a_generator_step_is_its_child(spans: Any) -> None:
    @tracepad.observe
    def stream() -> Any:
        with tracepad.span("inner"):
            pass
        yield "a"

    list(stream())
    assert spans.one("inner").parent.span_id == spans.one("stream").context.span_id


def test_a_generator_that_raises_ends_its_span_as_an_error(spans: Any) -> None:
    @tracepad.observe
    def stream() -> Any:
        yield "a"
        raise ValueError("no")

    with pytest.raises(ValueError, match="no"):
        list(stream())

    span = spans.one("stream")
    assert span.status.status_code is StatusCode.ERROR
    assert [event.name for event in span.events] == ["exception"]
    assert json.loads(span.attributes[attrs.OUTPUT]) == ["a"]
