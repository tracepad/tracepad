"""Deleting traces against a real binary (spec 036 #7): one by id, one by filter."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import pytest
from harness import BINARY, KEY, Store, trace_of

import tracepad

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
