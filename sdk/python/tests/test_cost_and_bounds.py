"""What the package costs when nothing records, and how long it waits (spec 042)."""

from __future__ import annotations

import logging
import threading
import time
import uuid
from collections.abc import Iterator, Sequence
from contextlib import contextmanager
from datetime import datetime, timezone
from typing import Any

import pytest
from opentelemetry import context as otel_context
from opentelemetry import trace as otel_api
from opentelemetry.sdk.trace import ReadableSpan, SpanProcessor
from opentelemetry.sdk.trace.export import BatchSpanProcessor, SpanExporter, SpanExportResult

import tracepad
from tracepad import _attributes as attrs
from tracepad import _tracing, testing

HOST, KEY = "http://tracepad.test:4318", "tp-sk-test"


class Counted:
    """A value whose every serialisation is counted: JSON reaches it through `repr`."""

    dumped = 0

    def __repr__(self) -> str:
        Counted.dumped += 1
        return "<counted>"


def serialise_everything() -> None:
    """Every door that serialises, each handed one counted value."""

    @tracepad.observe
    def step(thing: Any) -> Any:
        return thing

    with tracepad.span("s", input=Counted(), metadata={"k": Counted()}) as observation:
        observation.update(output=Counted(), metadata={"j": Counted()})
        tracepad.update(input=Counted())
        tracepad.update_trace(metadata={"t": Counted()}, tags=[Counted()])  # type: ignore[list-item]
        step(Counted())
        with tracepad.generation("g", model="m", model_parameters={"p": Counted()},
                                 input=Counted()) as call:
            call.end(output=Counted())


@contextmanager
def sampled_out() -> Iterator[None]:
    """Under a caller that chose not to sample: the default sampler drops every span."""
    parent = otel_api.SpanContext(0xA * 2**100, 0xB, is_remote=True,
                                  trace_flags=otel_api.TraceFlags(0))
    token = otel_context.attach(otel_api.set_span_in_context(otel_api.NonRecordingSpan(parent)))
    try:
        yield
    finally:
        otel_context.detach(token)


@pytest.fixture(autouse=True)
def uncounted() -> None:
    Counted.dumped = 0


def test_a_span_sampled_out_serialises_nothing() -> None:
    with testing.capture() as captured, sampled_out():
        serialise_everything()
    assert captured.spans == []
    assert Counted.dumped == 0


def test_with_tracing_off_nothing_is_serialised() -> None:
    serialise_everything()
    assert Counted.dumped == 0


def test_a_span_that_records_serialises_each_value_once() -> None:
    with testing.capture() as captured:
        serialise_everything()
    assert Counted.dumped == 12  # the twelve values above, each once
    assert captured.attributes("s")[attrs.INPUT] == '"<counted>"'


def test_a_sampler_still_sees_the_cheap_attributes_at_start() -> None:
    seen: list[dict[str, Any]] = []

    class Watching(SpanProcessor):
        def on_start(self, span: Any, parent_context: Any = None) -> None:
            seen.append(dict(span.attributes))

    with testing.capture():
        otel_api.get_tracer_provider().add_span_processor(Watching())  # type: ignore[attr-defined]
        with tracepad.generation("chat", model="gpt-4o-mini", input="hello", metadata={"a": 1}):
            pass
    assert seen == [{attrs.OBSERVATION_TYPE: "generation", attrs.REQUEST_MODEL: "gpt-4o-mini"}]


# --- update and update_trace (Decision 2) ---------------------------------------


def test_with_tracing_off_update_in_a_block_says_so_at_debug(
    caplog: pytest.LogCaptureFixture,
) -> None:
    with caplog.at_level(logging.DEBUG, logger="tracepad"), tracepad.span("off"):
        tracepad.update(output="x")
        tracepad.update_trace(user_id="u-1")
    assert [r.levelname for r in caplog.records] == ["DEBUG", "DEBUG"]


def test_under_a_sampled_out_span_update_says_so_at_debug(
    caplog: pytest.LogCaptureFixture,
) -> None:
    with testing.capture() as captured, sampled_out():
        with caplog.at_level(logging.DEBUG, logger="tracepad"), tracepad.span("dropped"):
            tracepad.update(output="x")
            tracepad.update_trace(user_id="u-1")
    assert [r.levelname for r in caplog.records] == ["DEBUG", "DEBUG"]
    assert captured.spans == []


def test_with_tracing_off_update_outside_every_block_is_quiet(
    caplog: pytest.LogCaptureFixture,
) -> None:
    with caplog.at_level(logging.WARNING, logger="tracepad"):
        tracepad.update(output="x")
    assert caplog.records == []


def test_an_initialised_process_still_warns_outside_every_block(
    caplog: pytest.LogCaptureFixture,
) -> None:
    with testing.capture(), caplog.at_level(logging.WARNING, logger="tracepad"):
        tracepad.update(output="x")
        tracepad.update_trace(user_id="u-1")
    assert [r.getMessage() for r in caplog.records] == [
        "tracepad.update() outside a span: nothing was written",
        "tracepad.update_trace() outside a span: nothing was written",
    ]


# --- export_timeout (Decision 3) -------------------------------------------------


def exporter_timeout() -> float:
    processors = _tracing._built._active_span_processor._span_processors
    batch = next(p for p in processors if isinstance(p, BatchSpanProcessor))
    return float(batch.span_exporter._timeout)


def test_the_export_timeout_is_five_seconds_by_default() -> None:
    tracepad.init(HOST, KEY)
    assert exporter_timeout() == 5.0


def test_the_environment_names_it(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("TRACEPAD_EXPORT_TIMEOUT", "2.5")
    tracepad.init(HOST, KEY)
    assert exporter_timeout() == 2.5


def test_the_argument_wins_over_the_environment(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("TRACEPAD_EXPORT_TIMEOUT", "2.5")
    tracepad.init(HOST, KEY, export_timeout=1.5)
    assert exporter_timeout() == 1.5


def test_opentelemetry_s_own_variable_works_when_neither_is_given(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("OTEL_EXPORTER_OTLP_TRACES_TIMEOUT", "7")
    tracepad.init(HOST, KEY)
    assert exporter_timeout() == 7.0


def test_a_variable_that_is_not_seconds_is_ignored_with_a_warning(
    monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    monkeypatch.setenv("TRACEPAD_EXPORT_TIMEOUT", "5s")
    with caplog.at_level(logging.WARNING, logger="tracepad"):
        tracepad.init(HOST, KEY)
    assert exporter_timeout() == 5.0
    assert "TRACEPAD_EXPORT_TIMEOUT='5s'" in caplog.text


def test_it_is_ignored_with_a_warning_when_nothing_is_exported(
    caplog: pytest.LogCaptureFixture,
) -> None:
    with caplog.at_level(logging.WARNING, logger="tracepad"):
        tracepad.init(HOST, KEY, export=False, export_timeout=1.0)
    assert "export_timeout is ignored with export=False" in caplog.text


# --- flush (Decision 4) ----------------------------------------------------------


class Hanging(SpanExporter):
    """A store that never answers, until the test lets it go."""

    def __init__(self) -> None:
        self.released = threading.Event()

    def export(self, spans: Sequence[ReadableSpan]) -> SpanExportResult:
        self.released.wait(5)
        return SpanExportResult.SUCCESS

    def shutdown(self) -> None:
        self.released.set()


def test_flush_returns_within_its_timeout_against_an_export_that_hangs(
    caplog: pytest.LogCaptureFixture,
) -> None:
    tracepad.init(HOST, KEY, export=False)
    hanging = Hanging()
    _tracing._built.add_span_processor(BatchSpanProcessor(hanging))
    try:
        with tracepad.span("pending"):
            pass
        started = time.monotonic()
        with caplog.at_level(logging.WARNING, logger="tracepad"):
            tracepad.flush(0.2)
        assert time.monotonic() - started < 0.2 + 0.3
        assert "not exported within the 0.2s budget" in caplog.text
    finally:
        hanging.released.set()


def test_a_flush_that_finishes_in_time_says_nothing(caplog: pytest.LogCaptureFixture) -> None:
    with testing.capture(), caplog.at_level(logging.WARNING, logger="tracepad"):
        with tracepad.span("done"):
            pass
        tracepad.flush(1.0)
    assert caplog.records == []


# --- metadata by key (Decision 5) ------------------------------------------------


def test_update_adds_metadata_keys_and_replaces_only_its_own(spans: testing.Capture) -> None:
    with tracepad.span("step", metadata={"a": 1, "keep": "x"}) as step:
        step.update(metadata={"b": {"nested": True}})
        tracepad.update(metadata={"a": 3, "keep": None})

    attributes = spans.attributes("step")
    prefix = attrs.OBSERVATION_METADATA
    assert {k: v for k, v in attributes.items() if k.startswith(prefix)} == {
        f"{prefix}.a": 3,
        f"{prefix}.keep": "x",  # None writes nothing, and deletes nothing
        f"{prefix}.b": '{"nested":true}',
    }


# --- found in review of PR #83 ---------------------------------------------------


def test_flushes_that_run_out_of_time_share_one_export(monkeypatch: pytest.MonkeyPatch) -> None:
    # A request that flushes in its `finally`, against a store that is away:
    # one export left running, not one thread per request.
    tracepad.init(HOST, KEY, export=False)
    released = threading.Event()
    calls: list[int] = []

    def hanging(timeout_millis: int) -> bool:
        calls.append(timeout_millis)
        released.wait(5)
        return True

    monkeypatch.setattr(_tracing._built, "force_flush", hanging)
    try:
        for _ in range(3):
            tracepad.flush(0.05)
        assert len(calls) == 1
    finally:
        released.set()


@pytest.mark.parametrize("given", [0, -1, float("nan")])
def test_an_argument_that_is_not_seconds_is_ignored_with_a_warning(
    given: float, caplog: pytest.LogCaptureFixture
) -> None:
    with caplog.at_level(logging.WARNING, logger="tracepad"):
        tracepad.init(HOST, KEY, export_timeout=given)
    assert exporter_timeout() == 5.0
    assert "export_timeout=" in caplog.text


def test_metadata_values_keep_their_type_or_become_plain_strings(spans: testing.Capture) -> None:
    moment = datetime(2026, 9, 23, 10, 0, tzinfo=timezone.utc)
    ident = uuid.UUID(int=1)
    with tracepad.span("step", metadata={"big": 2**64, "at": moment, "id": ident, "n": -(2**63)}):
        pass

    attributes = spans.attributes("step")
    prefix = attrs.OBSERVATION_METADATA
    assert attributes[f"{prefix}.big"] == str(2**64)  # past OTLP's 64 bits: JSON, not a lost batch
    assert attributes[f"{prefix}.at"] == repr(moment)  # the string, without its quotes
    assert attributes[f"{prefix}.id"] == repr(ident)
    assert attributes[f"{prefix}.n"] == -(2**63)


@pytest.mark.parametrize("given", [["a", "b"], "nightly"])
def test_metadata_that_is_no_mapping_is_written_whole(given: Any, spans: testing.Capture) -> None:
    with tracepad.span("step", metadata=given):
        pass

    metadata = {k: v for k, v in spans.attributes("step").items()
                if k.startswith(attrs.OBSERVATION_METADATA)}
    assert metadata == {attrs.OBSERVATION_METADATA: attrs.dumps(given)}
