"""The harness against a real binary (spec 018, Testing).

The loop `docs/datasets.md` prints, run end to end: declare the configs, push
the cases, open the run, fetch the items at the version it pinned, run each
case inside its block, score it, close the run — then read back through the
API what the store made of it.
"""

from __future__ import annotations

from typing import Any

import pytest
from opentelemetry import trace as otel_api
from opentelemetry.sdk.trace import TracerProvider

import tracepad
from harness import BINARY, KEY, Store, trace_of

pytestmark = pytest.mark.skipif(not BINARY, reason="TRACEPAD_BINARY is not set")

CASES = [
    {"id": tracepad.item_id("reset"), "input": {"question": "where does a span land?"},
     "expected_output": {"answer": "In the trace you are reading."}},
    {"id": tracepad.item_id("refund"), "input": {"question": "what does a run pin?"},
     "expected_output": {"answer": "A dataset version."}},
]

ANSWER = {
    "model": "claude-sonnet-5-2026-08-01",
    "choices": [{"message": {"content": "In the trace you are reading."}}],
    "usage": {"prompt_tokens": 18, "completion_tokens": 9, "cost": 0.0002},
}


@tracepad.observe(name="answer-case")
def answer(question: str) -> str:
    """The application under test: its own span, with a generation inside."""
    with tracepad.generation("chat", model="claude-sonnet-5", input=question) as call:
        call.end(response=ANSWER)
    return str(ANSWER["choices"][0]["message"]["content"])  # type: ignore[index]


def a_run(name: str) -> tuple[dict[str, Any], list[str]]:
    """One pass of the whole loop, returning the run and the trace ids it made."""
    tracepad.score_configs([
        {"name": "accuracy", "data_type": "numeric", "direction": "higher",
         "min": 0, "max": 1},
        tracepad.ScoreConfig(name="verdict", data_type="categorical",
                             categories=["pass", "fail"]),
    ])

    golden = tracepad.dataset("support-golden")
    golden.put_items(CASES)

    traces: list[str] = []
    with golden.run(name, metadata={"prompt": "support-answer@7"}) as run:
        for case in golden.items(version=run.dataset_version):
            with run.item(case) as attempt:
                produced = answer(case.input["question"])
                traces.append(attempt.trace_id or "")
                attempt.score("accuracy", 1 if produced else 0)
                attempt.score("verdict", string_value="pass", data_type="categorical")

    return run.get(), traces


def test_the_whole_loop(store: Store) -> None:
    tracepad.init(store.host, KEY, environment="eval")

    first, traces = a_run("prompt v7 / claude-sonnet-5")

    assert first["status"] == "finished"
    assert first["dataset"] == "support-golden"
    summary = first["summary"]
    assert summary["items"] == {"total": 2, "covered": 2, "missing": 0, "unknown": 0}
    assert summary["traces"]["count"] == 2
    assert summary["traces"]["error_count"] == 0
    assert set(summary["scores"]) == {"accuracy", "verdict"}
    assert summary["scores"]["accuracy"]["count"] == 2
    assert summary["scores"]["accuracy"]["direction"] == "higher"
    assert summary["scores"]["verdict"]["distribution"] == {"pass": 2}
    # Derived from the run's observations, not from what the harness declared.
    assert summary["models"] == ["claude-sonnet-5"]

    # Every trace carries the run and the item, on spans the harness never
    # touched: the processor wrote them at `on_start`.
    for trace_id in traces:
        stored = trace_of(store, trace_id, expand="")
        assert stored["run_id"] == first["id"]
        assert stored["item_id"] in {case["id"] for case in CASES}

    cases = store.call("GET", f"/api/v1/runs/{first['id']}/items")["items"]
    assert len(cases) == 2
    assert all(len(case["attempts"]) == 1 for case in cases)
    assert all(case["attempts"][0]["scores"] for case in cases)

    # A second run over the same cases, and the comparison the store computes.
    second, _ = a_run("prompt v8 / claude-sonnet-5")
    verdicts = tracepad.compare(first["id"], second["id"])
    assert verdicts["a"]["id"] == first["id"]
    assert [row["in"] for row in verdicts["items"]] == ["both", "both"]
    assert all("accuracy" in row["scores"] for row in verdicts["items"])


def test_the_same_cases_again_change_nothing(store: Store) -> None:
    tracepad.init(store.host, KEY)
    golden = tracepad.dataset("idempotent")

    first_version, changed = golden.put_items(CASES)
    again_version, again = golden.put_items(CASES)

    assert changed == 2
    assert (again_version, again) == (first_version, 0)

    stored = list(golden.items())
    assert [item.id for item in stored] == [case["id"] for case in CASES]
    # The version each row was written at, under the name the store sends it.
    assert {item.dataset_version for item in stored} == {first_version}


def test_more_items_than_a_request_takes_are_written_in_two(store: Store) -> None:
    # The package's chunk is the number the server takes (spec 018 #15).
    tracepad.init(store.host, KEY)

    version, changed = tracepad.dataset("over-the-cap").put_items(
        [{"input": n} for n in range(10_001)])

    assert (version, changed) == (2, 10_001)


def test_a_run_that_raised_is_closed_as_failed(store: Store) -> None:
    tracepad.init(store.host, KEY)
    golden = tracepad.dataset("failing")
    golden.put_items(CASES[:1])

    with pytest.raises(RuntimeError, match="judge"):
        with golden.run("doomed") as run:
            run_id = run.id
            raise RuntimeError("judge timed out")

    closed = store.call("GET", f"/api/v1/runs/{run_id}")
    assert closed["status"] == "failed"
    assert "judge timed out" in closed["error"]


def test_another_sdk_s_spans_are_stamped_without_our_exporter(store: Store) -> None:
    """`init(export=False)` under somebody else's provider still stamps."""
    application = TracerProvider()
    application.add_span_processor(exporting_processor(store))
    otel_api.set_tracer_provider(application)
    tracepad.init(store.host, KEY, export=False)

    golden = tracepad.dataset("borrowed")
    golden.put_items(CASES[:1])
    framework = otel_api.get_tracer("the.framework")

    with golden.run("over another exporter") as run:
        case = next(iter(golden.items(version=run.dataset_version)))
        with run.item(case) as attempt:
            with framework.start_as_current_span("GET /answer"):
                pass
            trace_id = attempt.trace_id

    application.force_flush(10_000)
    stored = trace_of(store, trace_id or "", expand="")
    assert stored["run_id"] == run.id
    assert stored["item_id"] == case.id
    assert stored["name"] == "GET /answer"


def exporting_processor(store: Store) -> Any:
    from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
    from opentelemetry.sdk.trace.export import SimpleSpanProcessor

    return SimpleSpanProcessor(
        OTLPSpanExporter(endpoint=f"{store.host}/v1/traces",
                         headers={"Authorization": f"Bearer {KEY}"})
    )
