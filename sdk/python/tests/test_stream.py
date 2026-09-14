"""The streaming pass-through over a generation (spec 031 #7)."""

from __future__ import annotations

import asyncio
import itertools
import logging
from collections.abc import AsyncIterator, Iterator
from typing import Any

import pytest
from opentelemetry.trace import StatusCode

import tracepad
from tracepad import _attributes as attrs
from tracepad import _tracing

MODEL = "gpt-4o-mini-2026-04-01"
USAGE = {
    "prompt_tokens": 42,
    "completion_tokens": 7,
    "prompt_tokens_details": {"cached_tokens": 30},
    "cost": 0.00041,
}


def chunk(content: str | None = None, *, role: str | None = None, usage: Any = None,
          choices: bool = True) -> dict[str, Any]:
    """One chunk the way OpenAI cuts them: `usage` is `null` until the last."""
    delta: dict[str, Any] = {}
    if role is not None:
        delta["role"] = role
    if content is not None:
        delta["content"] = content
    return {
        "model": MODEL,
        "choices": [{"index": 0, "delta": delta, "finish_reason": None}] if choices else [],
        "usage": usage,
    }


# The first chunk names the role with an empty content, the deltas follow, and
# a last chunk with no choices carries the usage — what `stream_options=
# {"include_usage": True}` makes OpenAI send, and OpenRouter's shape too.
CHUNKS = [
    chunk("", role="assistant"),
    chunk("po"),
    chunk("ng"),
    chunk(usage=USAGE, choices=False),
]


class Attribute:
    """A chunk that answers by attribute, the way the OpenAI client's models do."""

    def __init__(self, source: Any) -> None:
        for name, value in source.items():
            if isinstance(value, dict):
                value = Attribute(value)
            elif isinstance(value, list):
                value = [Attribute(v) if isinstance(v, dict) else v for v in value]
            setattr(self, name, value)


@pytest.fixture
def ticking(monkeypatch: pytest.MonkeyPatch) -> None:
    """A clock that moves a whole second per reading (see test_generation)."""
    ticks = itertools.count(1)
    monkeypatch.setattr(_tracing, "time_ns", lambda: 1787738400_000_000_000 + next(ticks) * 10**9)


def test_a_stream_ends_the_generation_with_what_it_carried(spans: Any) -> None:
    with tracepad.generation("chat", model="gpt-4o-mini") as call:
        seen = list(call.stream(CHUNKS))

    assert seen == CHUNKS  # every chunk through, unchanged
    attributes = spans.attributes("chat")
    assert attributes[attrs.RESPONSE_MODEL] == MODEL
    assert attributes["gen_ai.usage.input_tokens"] == 42
    assert attributes["gen_ai.usage.output_tokens"] == 7
    assert attributes["gen_ai.usage.cache_read_input_tokens"] == 30
    assert attributes[attrs.USAGE_COST] == 0.00041
    assert attributes[attrs.OUTPUT] == "pong"
    assert attrs.COMPLETION_START_TIME in attributes


def test_the_first_token_is_the_first_chunk_with_content(spans: Any, ticking: None) -> None:
    # The role-only chunk is not a token, so the stamp is the clock's first
    # reading, and the third chunk does not move it.
    with tracepad.generation("chat") as call:
        for _ in call.stream(CHUNKS):
            pass
    assert spans.attributes("chat")[attrs.COMPLETION_START_TIME] == "2026-08-26T10:00:01.000Z"


def test_objects_stream_like_dicts(spans: Any) -> None:
    with tracepad.generation("chat") as call:
        for _ in call.stream(Attribute(c) for c in CHUNKS):
            pass

    attributes = spans.attributes("chat")
    assert attributes[attrs.RESPONSE_MODEL] == MODEL
    assert attributes["gen_ai.usage.output_tokens"] == 7
    assert attributes[attrs.USAGE_COST] == 0.00041
    assert attributes[attrs.OUTPUT] == "pong"


def test_a_stream_without_usage_records_the_model_and_the_output(spans: Any) -> None:
    with tracepad.generation("chat") as call:
        for _ in call.stream(CHUNKS[:3]):
            pass

    attributes = spans.attributes("chat")
    assert attributes[attrs.RESPONSE_MODEL] == MODEL
    assert attributes[attrs.OUTPUT] == "pong"
    assert attrs.USAGE_COST not in attributes
    assert not [key for key in attributes if key.startswith(attrs.USAGE_PREFIX)]


def test_an_explicit_end_mid_stream_wins_and_nothing_ends_twice(
    spans: Any, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.WARNING, logger="opentelemetry")
    with tracepad.generation("chat") as call:
        for i, _ in enumerate(call.stream(CHUNKS)):
            if i == 1:
                call.end(cost=9.5, usage={"input_tokens": 1})

    attributes = spans.attributes("chat")
    assert attributes[attrs.USAGE_COST] == 9.5
    assert attributes["gen_ai.usage.input_tokens"] == 1
    assert "gen_ai.usage.output_tokens" not in attributes  # the last chunk came too late
    assert attributes[attrs.OUTPUT] == "po"  # what the stream had read by then
    assert not caplog.records  # no "Calling end() on an ended span"


def test_an_abandoned_stream_is_ended_by_the_block(
    spans: Any, caplog: pytest.LogCaptureFixture
) -> None:
    caplog.set_level(logging.WARNING, logger="opentelemetry")
    with tracepad.generation("chat") as call:
        left = call.stream(CHUNKS)
        next(left)
        next(left)

    span = spans.one("chat")
    assert span.end_time is not None
    assert dict(span.attributes)[attrs.OUTPUT] == "po"
    assert attrs.USAGE_COST not in span.attributes

    # Draining it afterwards changes nothing and ends nothing twice.
    assert len(list(left)) == 2
    assert not caplog.records


def test_breaking_out_of_the_loop_keeps_what_was_read(spans: Any) -> None:
    with tracepad.generation("chat") as call:
        for i, _ in enumerate(call.stream(CHUNKS)):
            if i == 1:
                break

    attributes = spans.attributes("chat")
    assert attributes[attrs.OUTPUT] == "po"
    assert attributes[attrs.RESPONSE_MODEL] == MODEL


def test_a_stream_that_raises_is_an_error_with_what_was_read(spans: Any) -> None:
    def cut() -> Iterator[dict[str, Any]]:
        yield CHUNKS[0]
        yield CHUNKS[1]
        raise ConnectionError("the provider hung up")

    with pytest.raises(ConnectionError), tracepad.generation("chat") as call:
        for _ in call.stream(cut()):
            pass

    span = spans.one("chat")
    assert span.status.status_code is StatusCode.ERROR
    assert [e.name for e in span.events] == ["exception"]
    assert dict(span.attributes)[attrs.OUTPUT] == "po"


def test_a_consumer_that_raises_is_an_error_too(spans: Any) -> None:
    # The loop's body fails: the generator is closed while the exception is in
    # flight, and the block — not the wrapper — must be the one to end the
    # span, or the exception lands on an ended span and is lost.
    with pytest.raises(ValueError), tracepad.generation("chat") as call:
        for i, _ in enumerate(call.stream(CHUNKS)):
            if i == 1:
                raise ValueError("cannot parse the chunk")

    span = spans.one("chat")
    assert span.status.status_code is StatusCode.ERROR
    assert [e.name for e in span.events] == ["exception"]
    assert dict(span.attributes)[attrs.OUTPUT] == "po"


def test_capture_output_off_keeps_the_usage_and_drops_the_text(spans: Any) -> None:
    @tracepad.observe(type="generation", capture_output=False)
    def answer() -> str:
        call = _tracing._current.get()
        assert isinstance(call, tracepad.Generation)
        return "".join(c["choices"][0]["delta"].get("content", "")
                       for c in call.stream(CHUNKS) if c["choices"])

    assert answer() == "pong"
    attributes = spans.attributes("answer")
    assert attrs.OUTPUT not in attributes
    assert attributes["gen_ai.usage.input_tokens"] == 42


def test_the_async_twin(spans: Any, ticking: None) -> None:
    async def cut() -> AsyncIterator[dict[str, Any]]:
        for c in CHUNKS:
            await asyncio.sleep(0)
            yield c

    async def main() -> list[Any]:
        with tracepad.generation("chat") as call:
            return [c async for c in call.astream(cut())]

    assert asyncio.run(main()) == CHUNKS
    attributes = spans.attributes("chat")
    assert attributes[attrs.RESPONSE_MODEL] == MODEL
    assert attributes["gen_ai.usage.output_tokens"] == 7
    assert attributes[attrs.USAGE_COST] == 0.00041
    assert attributes[attrs.OUTPUT] == "pong"
    assert attributes[attrs.COMPLETION_START_TIME] == "2026-08-26T10:00:01.000Z"


def test_the_async_twin_abandoned_is_ended_by_the_block(spans: Any) -> None:
    async def cut() -> AsyncIterator[dict[str, Any]]:
        for c in CHUNKS:
            yield c

    async def main() -> None:
        with tracepad.generation("chat") as call:
            async for i, _ in aenumerate(call.astream(cut())):
                if i == 1:
                    break

    async def aenumerate(source: AsyncIterator[Any]) -> AsyncIterator[tuple[int, Any]]:
        i = 0
        async for value in source:
            yield i, value
            i += 1

    asyncio.run(main())
    attributes = spans.attributes("chat")
    assert attributes[attrs.OUTPUT] == "po"
    assert attrs.USAGE_COST not in attributes
