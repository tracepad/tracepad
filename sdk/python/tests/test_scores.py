"""The score queue: the target, the batch, the retry (spec 017 #6, #7)."""

from __future__ import annotations

import logging
from typing import Any

import pytest

import tracepad
from tracepad import _scores, _tracing
from tracepad._errors import TracepadHTTPError
from tracepad._scores import ScoreQueue


class Clock:
    """A clock the test winds: frozen by default, `step` seconds per reading.

    Reading it is what the queue does to decide whether its interval is up, so
    a step of the interval closes every batch at the first opportunity and a
    step of zero never closes one — which is how these two tests separate the
    size rule from the time rule without waiting for a real second.
    """

    def __init__(self, step: float = 0.0) -> None:
        self.now = 0.0
        self.step = step

    def __call__(self) -> float:
        reading = self.now
        self.now += self.step
        return reading


class Sender:
    def __init__(self, failures: int = 0) -> None:
        self.batches: list[list[dict[str, Any]]] = []
        self.failures = failures

    def __call__(self, batch: list[dict[str, Any]]) -> None:
        self.batches.append(list(batch))
        if len(self.batches) <= self.failures:
            raise TracepadHTTPError(400, '{"error":"score 3: unknown field \\"commet\\""}')


@pytest.fixture
def sender() -> Any:
    return Sender()


def queue(sender: Sender, **kwargs: Any) -> ScoreQueue:
    made = ScoreQueue(sender, **kwargs)
    _scores.reset(made)
    return made


def test_a_score_inside_a_span_targets_its_trace(spans: Any, sender: Sender) -> None:
    made = queue(sender)
    with tracepad.span("handler") as observation:
        tracepad.score("helpful", 0.9, comment="cited the source")
        expected = observation.trace_id
    made.flush(2.0)

    assert sender.batches == [[{
        "name": "helpful", "trace_id": expected, "value": 0.9,
        "comment": "cited the source",
    }]]


def test_observation_true_adds_the_span_id(spans: Any, sender: Sender) -> None:
    made = queue(sender)
    with tracepad.span("handler") as observation:
        tracepad.score("grounded", 1, data_type="boolean", observation=True)
        expected = (observation.trace_id, observation.span_id)
    made.flush(2.0)

    written = sender.batches[0][0]
    assert (written["trace_id"], written["observation_id"]) == expected
    assert written["data_type"] == "boolean"


def test_an_explicit_target_needs_no_span(sender: Sender) -> None:
    made = queue(sender)
    tracepad.score("verdict", string_value="pass", data_type="categorical",
                   trace_id="a" * 32, id="b" * 32)
    made.flush(2.0)

    assert sender.batches[0][0] == {
        "name": "verdict", "trace_id": "a" * 32, "id": "b" * 32,
        "string_value": "pass", "data_type": "categorical",
    }


def test_outside_a_span_without_a_trace_id_raises(spans: Any) -> None:
    with pytest.raises(ValueError, match="no active span"):
        tracepad.score("helpful", 1)


def test_with_tracing_off_a_score_without_a_target_is_dropped(
    sender: Sender, caplog: pytest.LogCaptureFixture
) -> None:
    # Spec 039 #1: no `init`, so every span is a no-op — inside a block as
    # outside one — and the call site is not wrong.
    made = queue(sender)
    with caplog.at_level(logging.DEBUG, logger="tracepad"):
        with tracepad.span("handler") as observation:
            tracepad.score("helpful", 1)
        tracepad.score("helpful", 1, observation=True)
    made.flush(2.0)

    assert sender.batches == []
    assert caplog.text.count("tracing is off") == 2
    assert all(r.levelno == logging.DEBUG for r in caplog.records)
    # Spec 039 #3: no trace behind the span, no id.
    assert (observation.trace_id, observation.span_id) == (None, None)


def test_with_tracing_off_a_score_by_id_is_sent(sender: Sender) -> None:
    # Spec 039 #2: scoring by id is REST, not tracing.
    made = queue(sender)
    with tracepad.span("handler"):
        tracepad.score("helpful", 1, trace_id="a" * 32)
    made.flush(2.0)

    assert sender.batches == [[{"name": "helpful", "trace_id": "a" * 32, "value": 1}]]


def test_the_batch_closes_at_a_hundred(sender: Sender) -> None:
    clock = Clock()
    made = queue(sender, clock=clock)
    for index in range(150):
        tracepad.score(f"s{index}", 1, trace_id="a" * 32)
    made.flush(2.0)

    assert [len(batch) for batch in sender.batches] == [100, 50]


def test_the_batch_closes_at_the_interval(sender: Sender) -> None:
    made = queue(sender, clock=Clock(step=2.0), interval=2.0)
    tracepad.score("first", 1, trace_id="a" * 32)
    tracepad.score("second", 1, trace_id="a" * 32)
    made.flush(2.0)

    # Two batches although neither is anywhere near full: the interval is up.
    assert [[item["name"] for item in batch] for batch in sender.batches] == [["first"], ["second"]]


def test_a_rejected_batch_is_retried_once_then_dropped(
    caplog: pytest.LogCaptureFixture,
) -> None:
    sender = Sender(failures=2)
    made = queue(sender)
    with caplog.at_level(logging.WARNING, logger="tracepad"):
        tracepad.score("helpful", 1, trace_id="a" * 32)
        made.flush(2.0)

    assert len(sender.batches) == 2
    assert "1 score(s) dropped" in caplog.text
    assert "commet" in caplog.text


def test_one_failure_is_survived(sender: Any) -> None:
    sender = Sender(failures=1)
    made = queue(sender)
    tracepad.score("helpful", 1, trace_id="a" * 32)
    made.flush(2.0)

    assert len(sender.batches) == 2
    assert sender.batches[0] == sender.batches[1]


def test_a_score_after_the_queue_closed_is_logged_as_dropped(
    sender: Sender, caplog: pytest.LogCaptureFixture
) -> None:
    made = queue(sender)
    tracepad.score("early", 1, trace_id="a" * 32)
    made.flush(2.0)
    made.close()

    with caplog.at_level(logging.WARNING, logger="tracepad"):
        tracepad.score("late", 1, trace_id="a" * 32)

    assert "'late' dropped" in caplog.text
    assert [item["name"] for batch in sender.batches for item in batch] == ["early"]


def test_the_sender_survives_an_error_that_is_not_ours(
    caplog: pytest.LogCaptureFixture,
) -> None:
    # A score value the JSON encoder refuses raises a TypeError, not a
    # TracepadError; an exception that escaped would end the only thread there
    # is, losing every later score in silence (found in review of PR #35).
    refused: list[list[dict[str, Any]]] = []

    def send(batch: list[dict[str, Any]]) -> None:
        refused.append(list(batch))
        if batch[0]["name"] == "undeliverable":
            raise TypeError("Object of type Decimal is not JSON serializable")

    made = queue(send)  # type: ignore[arg-type]
    with caplog.at_level(logging.WARNING, logger="tracepad"):
        tracepad.score("undeliverable", 1, trace_id="a" * 32)
        made.flush(2.0)
        tracepad.score("later", 1, trace_id="a" * 32)
        made.flush(2.0)

    assert "Decimal" in caplog.text
    assert [batch[0]["name"] for batch in refused] == ["undeliverable", "undeliverable", "later"]


def test_flush_says_so_when_the_scores_spent_the_whole_budget(
    monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    flushed: list[int] = []

    class Provider:
        def force_flush(self, timeout_millis: int) -> None:
            flushed.append(timeout_millis)

    monkeypatch.setattr(_tracing.otel, "get_tracer_provider", Provider)
    monkeypatch.setattr(_tracing, "flush_scores", lambda timeout: -0.5)

    with caplog.at_level(logging.WARNING, logger="tracepad"):
        tracepad.flush(2.0)

    # A deadline of zero returns at once and exports nothing; saying so beats
    # reporting a flush that did not happen (found in review of PR #35).
    assert flushed == []
    assert "were not flushed" in caplog.text


def test_flush_drains_the_queue_and_the_spans(spans: Any, sender: Sender) -> None:
    made = queue(sender)
    tracepad.score("helpful", 1, trace_id="a" * 32)
    tracepad.flush(2.0)

    assert made is _scores._scores
    assert [item["name"] for item in sender.batches[0]] == ["helpful"]
