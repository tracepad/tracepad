"""The generation helper and its OpenAI-compatible reader (spec 017 #5)."""

from __future__ import annotations

import itertools
import json
from typing import Any

import pytest

import tracepad
from tracepad import _attributes as attrs
from tracepad import _tracing

ANSWER = {
    "model": "gpt-4o-mini-2026-04-01",
    "choices": [{"message": {"role": "assistant", "content": "pong"}}],
    "usage": {
        "prompt_tokens": 42,
        "completion_tokens": 7,
        "prompt_tokens_details": {"cached_tokens": 30},
        "completion_tokens_details": {"reasoning_tokens": 3},
        "cost": 0.00041,
    },
}


class Attribute:
    """A response that answers by attribute, the way a pydantic model does."""

    def __init__(self, source: Any) -> None:
        for name, value in source.items():
            setattr(self, name, Attribute(value) if isinstance(value, dict) else value)


def test_a_dict_response(spans: Any) -> None:
    with tracepad.generation("chat", model="gpt-4o-mini",
                             model_parameters={"temperature": 0.3, "max_tokens": 128},
                             input=[{"role": "user", "content": "ping"}]) as call:
        call.end(response=ANSWER)

    attributes = spans.attributes("chat")
    assert attributes[attrs.REQUEST_MODEL] == "gpt-4o-mini"
    assert attributes[attrs.RESPONSE_MODEL] == "gpt-4o-mini-2026-04-01"
    assert attributes["gen_ai.request.temperature"] == 0.3
    assert attributes["gen_ai.request.max_tokens"] == 128
    assert attributes["gen_ai.usage.input_tokens"] == 42
    assert attributes["gen_ai.usage.output_tokens"] == 7
    assert attributes["gen_ai.usage.cache_read_input_tokens"] == 30
    assert attributes["gen_ai.usage.reasoning_tokens"] == 3
    assert attributes[attrs.USAGE_COST] == 0.00041
    assert attributes[attrs.OUTPUT] == "pong"
    assert json.loads(attributes[attrs.INPUT]) == [{"role": "user", "content": "ping"}]
    assert attributes[attrs.OBSERVATION_TYPE] == "generation"


def test_an_object_response(spans: Any) -> None:
    with tracepad.generation("chat") as call:
        call.end(response=Attribute(ANSWER))

    attributes = spans.attributes("chat")
    assert attributes[attrs.RESPONSE_MODEL] == "gpt-4o-mini-2026-04-01"
    assert attributes["gen_ai.usage.input_tokens"] == 42
    assert attributes[attrs.USAGE_COST] == 0.00041
    assert attributes[attrs.OUTPUT] == "pong"


def test_explicit_arguments_win(spans: Any) -> None:
    with tracepad.generation("chat") as call:
        call.end(response=ANSWER, model="claude-sonnet-5",
                 usage={"input_tokens": 1, "output_tokens": 2}, cost=9.5, output="rewritten")

    attributes = spans.attributes("chat")
    assert attributes[attrs.RESPONSE_MODEL] == "claude-sonnet-5"
    assert attributes["gen_ai.usage.input_tokens"] == 1
    assert attributes["gen_ai.usage.output_tokens"] == 2
    assert attributes[attrs.USAGE_COST] == 9.5
    assert attributes[attrs.OUTPUT] == "rewritten"


def test_a_response_without_usage_claims_no_cost(spans: Any) -> None:
    with tracepad.generation("chat") as call:
        call.end(response={"model": "gpt-4o-mini", "choices": [{"message": {"content": "hi"}}]})

    attributes = spans.attributes("chat")
    assert attributes[attrs.RESPONSE_MODEL] == "gpt-4o-mini"
    assert attributes[attrs.OUTPUT] == "hi"
    assert attrs.USAGE_COST not in attributes
    assert not [key for key in attributes if key.startswith(attrs.USAGE_PREFIX)]


def test_first_token_stamps_once(spans: Any, monkeypatch: pytest.MonkeyPatch) -> None:
    # A clock that moves a whole second per reading: two stamps a millisecond
    # apart would be one value however often they were written, and what is
    # being tested is that the second call does not write at all.
    ticks = itertools.count(1)
    monkeypatch.setattr(_tracing, "time_ns", lambda: 1787738400_000_000_000 + next(ticks) * 10**9)

    with tracepad.generation("chat") as call:
        call.first_token()
        call.first_token()

    assert spans.attributes("chat")[attrs.COMPLETION_START_TIME] == "2026-08-26T10:00:01.000Z"


def test_leaving_the_block_without_end(spans: Any) -> None:
    with tracepad.generation("chat", model="gpt-4o-mini"):
        pass
    assert spans.one("chat").end_time is not None


def test_the_prompt_that_ran(spans: Any) -> None:
    prompt = tracepad.Prompt(name="support-answer", version=7, type="text", text="hi")
    with tracepad.generation("chat", prompt=prompt):
        pass

    attributes = spans.attributes("chat")
    assert attributes[attrs.PROMPT_NAME] == "support-answer"
    assert attributes[attrs.PROMPT_VERSION] == 7


def test_observe_as_a_generation_reads_the_return_value(spans: Any) -> None:
    @tracepad.observe(type="generation")
    def call(question: str) -> dict[str, Any]:
        return ANSWER

    call("ping")
    attributes = spans.attributes("call")
    assert attributes[attrs.OBSERVATION_TYPE] == "generation"
    assert attributes[attrs.RESPONSE_MODEL] == "gpt-4o-mini-2026-04-01"
    assert attributes["gen_ai.usage.output_tokens"] == 7
    assert attributes[attrs.OUTPUT] == "pong"


def test_observe_as_a_generation_over_a_generator_keeps_what_it_yielded(spans: Any) -> None:
    # A generator's result is the list of its chunks, not a model's answer:
    # handing that list to the response reader recorded nothing at all
    # (found in review of PR #35).
    @tracepad.observe(type="generation")
    def stream() -> Any:
        yield "po"
        yield "ng"

    assert "".join(stream()) == "pong"
    attributes = spans.attributes("stream")
    assert attributes[attrs.OBSERVATION_TYPE] == "generation"
    assert json.loads(attributes[attrs.OUTPUT]) == ["po", "ng"]


def test_observe_as_a_generation_without_capturing_the_output(spans: Any) -> None:
    @tracepad.observe(type="generation", capture_output=False)
    def call() -> dict[str, Any]:
        return ANSWER

    call()
    attributes = spans.attributes("call")
    assert attrs.OUTPUT not in attributes
    assert attributes["gen_ai.usage.input_tokens"] == 42
