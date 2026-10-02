"""Deleting traces against a real binary (spec 036 #7): one by id, one by filter,
and never with a key whose scopes do not hold `write` (spec 045 #16)."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest

import tracepad
from harness import BINARY, KEY, Store, mint_key, trace_of

pytestmark = pytest.mark.skipif(not BINARY, reason="TRACEPAD_BINARY is not set")


def test_two_traces_go_one_by_id_and_one_by_filter(store: Store) -> None:
    tracepad.init(store.host, KEY)
    ids = []
    for name in ("first", "second"):
        with tracepad.span(name) as step:
            tracepad.update_trace(name="doomed", tags=["doomed"])
            ids.append(step.trace_id)
    tracepad.flush(20.0)
    by_id, by_filter = (trace_of(store, id)["id"] for id in ids)

    preview = tracepad.delete_trace(by_id)
    assert preview["dry_run"] is True and preview["confirm"] == by_id
    assert preview["would_delete"]["traces"] == 1
    gone = tracepad.delete_trace(by_id, confirm=True)
    assert gone["deleted"]["traces"] == 1 and gone["id"] == by_id

    to = datetime.now(timezone.utc) + timedelta(minutes=1)
    preview = tracepad.delete_traces(to=to, tag=["doomed"])
    assert preview["matched"] == 1 and preview["confirm"] == "e2e"
    total = tracepad.delete_traces(to=to, tag=["doomed"], confirm="e2e")
    assert total["deleted"]["traces"] == 1 and total["rounds"] == 1

    assert store.call("GET", "/api/v1/traces?tag=doomed")["traces"] == []
    with pytest.raises(tracepad.TracepadHTTPError, match="404"):
        tracepad.delete_trace(by_filter)


def test_an_ingest_key_covers_the_production_path_and_not_deletion(store: Store) -> None:
    """Spec 045 #16: span, score and prompt fetch are `ingest`; deleting is `write`."""
    store.call(
        "POST",
        "/api/v1/prompts/ingest-answer/versions",
        {"type": "text", "prompt": "Answer {topic}.", "labels": ["production"]},
    )
    tracepad.init(store.host, mint_key(store, "ingest"))

    assert tracepad.prompt("ingest-answer", label="production").version == 1
    with tracepad.span("ingest-only") as step:
        tracepad.update_trace(tags=["ingest-only"])
        tracepad.score("helpful", 1.0)
    tracepad.flush(20.0)
    trace_of(store, step.trace_id)
    scores = store.call("GET", f"/api/v1/scores?trace_id={step.trace_id}")["scores"]
    assert [(s["name"], s["value"]) for s in scores] == [("helpful", 1.0)]

    to = datetime.now(timezone.utc) + timedelta(minutes=1)
    with pytest.raises(tracepad.TracepadHTTPError) as refused:
        tracepad.delete_traces(to=to, tag=["ingest-only"], confirm="e2e")
    assert refused.value.status == 403
    assert "this key's scopes are ingest; DELETE /api/v1/traces needs write" in str(refused.value)
    assert trace_of(store, step.trace_id)["id"] == step.trace_id
