"""The package against a real binary (spec 017, Testing).

Everything below the package — the OTLP encoding, the transport, the auth, the
mapper's table, the columns — only breaks at a seam the unit layer cannot see.
So this boots the store on a temporary database, exports a trace through the
package's own exporter, posts a score and fetches a prompt, and then reads all
three back through the API a person would read them with.

`TRACEPAD_BINARY` names the binary; `scripts/sdk-test.sh` builds it and sets
it. Without it the suite skips, so `pytest` on its own stays a unit run.
"""

from __future__ import annotations

from pathlib import Path

import pytest
from opentelemetry import trace as otel_api
from opentelemetry.sdk.trace import TracerProvider

import tracepad
from harness import BINARY, KEY, Store, trace_of, walk

pytestmark = pytest.mark.skipif(not BINARY, reason="TRACEPAD_BINARY is not set")

ANSWER = {
    "model": "claude-sonnet-5-2026-08-01",
    "choices": [{"message": {"role": "assistant", "content": "Open Settings and choose Reset."}}],
    "usage": {"prompt_tokens": 128, "completion_tokens": 41, "cost": 0.0011},
}


def test_a_traced_call_arrives_whole(store: Store) -> None:
    store.call("POST", "/api/v1/prompts/support-answer/versions",
               {"type": "text", "prompt": "Answer {topic}.", "labels": ["production"]})
    tracepad.init(store.host, KEY, environment="e2e", release="2026.9.4")

    support = tracepad.prompt("support-answer", label="production")
    assert support.version == 1
    assert support.compile(topic="password resets") == "Answer password resets."

    @tracepad.observe(name="answer-question")
    def answer(question: str) -> str:
        tracepad.update_trace(name="support-chat", user_id="user-4821",
                              session_id="session-77", tags=["support", "beta"],
                              version="retrieval-v2")
        with tracepad.generation("chat-completion", model="claude-sonnet-5", prompt=support,
                                 model_parameters={"temperature": 0.2},
                                 input=[{"role": "user", "content": question}],
                                 metadata={"attempt": 1}) as call:
            call.first_token()
            call.end(response=ANSWER)
            trace_id.append(call.trace_id)
        tracepad.score("helpful", 0.9, comment="cited the source")
        return "done"

    trace_id: list[str] = []
    answer("how do I reset my password?")
    tracepad.flush(20.0)

    stored = trace_of(store, trace_id[0])
    assert stored["name"] == "support-chat"
    assert stored["user_id"] == "user-4821"
    assert stored["session_id"] == "session-77"
    assert sorted(stored["tags"]) == ["beta", "support"]
    assert stored["environment"] == "e2e"
    assert stored["release"] == "2026.9.4"
    assert stored["version"] == "retrieval-v2"

    observations = walk(stored["observations"])
    assert [o["name"] for o in observations] == ["answer-question", "chat-completion"]
    root, generation = observations
    assert root["input"] == {"question": "how do I reset my password?"}

    assert generation["type"] == "generation"
    assert generation["model"] == "claude-sonnet-5"
    assert generation["usage"] == {"input_tokens": 128, "output_tokens": 41}
    assert generation["model_parameters"] == {"temperature": 0.2}
    assert generation["prompt"] == {"name": "support-answer", "version": 1}
    assert generation["ttft_ms"] is not None
    assert generation["output"] == "Open Settings and choose Reset."
    assert generation["metadata"]["attempt"] == 1

    # The cost is the one the provider charged, never a computed one.
    assert abs(stored["total_cost"] - 0.0011) < 1e-12

    scores = store.call("GET", f"/api/v1/scores?trace_id={trace_id[0]}")["scores"]
    assert [(s["name"], s["value"], s["comment"]) for s in scores] == [
        ("helpful", 0.9, "cited the source")
    ]


def test_a_streamed_call_lands_with_its_usage(store: Store) -> None:
    """A stream through `call.stream` (spec 031 #7): the usage rides the last chunk."""
    tracepad.init(store.host, KEY)
    chunks = [
        {"model": ANSWER["model"], "choices": [{"delta": {"role": "assistant", "content": ""}}]},
        {"model": ANSWER["model"], "choices": [{"delta": {"content": "Open Settings "}}]},
        {"model": ANSWER["model"], "choices": [{"delta": {"content": "and choose Reset."}}]},
        {"model": ANSWER["model"], "choices": [], "usage": ANSWER["usage"]},
    ]

    with tracepad.generation("chat-completion", model="claude-sonnet-5") as call:
        trace_id = call.trace_id
        assert list(call.stream(chunks)) == chunks
    tracepad.flush(20.0)

    stored = trace_of(store, trace_id)
    (generation,) = walk(stored["observations"])
    assert generation["type"] == "generation"
    assert generation["usage"] == {"input_tokens": 128, "output_tokens": 41}
    assert generation["output"] == "Open Settings and choose Reset."
    assert generation["ttft_ms"] is not None
    assert abs(stored["total_cost"] - 0.0011) < 1e-12


def test_the_application_s_own_spans_share_the_trace(store: Store) -> None:
    """`init` under a provider somebody else set: one pipeline, one trace."""
    otel_api.set_tracer_provider(TracerProvider())
    tracepad.init(store.host, KEY)
    framework = otel_api.get_tracer("the.framework")

    with framework.start_as_current_span("GET /answer") as request:
        trace_id = format(request.get_span_context().trace_id, "032x")
        with tracepad.span("answer-question"):
            pass

    tracepad.flush(20.0)

    stored = trace_of(store, trace_id)
    assert [o["name"] for o in walk(stored["observations"])] == [
        "GET /answer",
        "answer-question",
    ]
    # The trace's name is the root span's: nothing claimed it (docs/ingest.md).
    assert stored["name"] == "GET /answer"


def test_a_failing_step_is_stored_as_an_error(store: Store) -> None:
    tracepad.init(store.host, KEY)

    @tracepad.observe
    def fails() -> None:
        raise RuntimeError("upstream timeout")

    with tracepad.span("attempt") as observation:
        trace_id = observation.trace_id
        with pytest.raises(RuntimeError):
            fails()

    tracepad.flush(20.0)

    stored = trace_of(store, trace_id)
    failed = next(o for o in walk(stored["observations"]) if o["name"] == "fails")
    assert failed["level"] == "ERROR"
    assert "upstream timeout" in failed["status_message"]
    assert stored["error_count"] == 1


def test_observation_metadata_merges_by_key(store: Store) -> None:
    """Spec 042 #5: `update(metadata=)` adds its keys and keeps the ones there."""
    tracepad.init(store.host, KEY)
    with tracepad.span("tagged", metadata={"a": 1, "request_id": "r-7"}) as step:
        trace_id = step.trace_id
        tracepad.update(metadata={"b": {"flag": True}})
        tracepad.update(metadata={"a": 3})
    tracepad.flush(20.0)

    (tagged,) = walk(trace_of(store, trace_id)["observations"])
    assert {key: tagged["metadata"][key] for key in ("a", "b", "request_id")} == {
        "a": 3,
        "b": {"flag": True},
        "request_id": "r-7",
    }


def test_the_package_is_installed_from_this_checkout() -> None:
    """A guard against testing a `tracepad` from PyPI by accident."""
    assert Path(tracepad.__file__).resolve().is_relative_to(Path(__file__).resolve().parents[3])
