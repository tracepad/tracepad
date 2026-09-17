"""Deleting traces: the dry run, the echo, the rounds (spec 036)."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from typing import Any

import pytest

import tracepad
from test_harness import Store
from tracepad import _config, _traces
from tracepad._errors import TracepadHTTPError

TRACE = "4f8c1d2e3a5b6c7d8e9f0a1b2c3d4e5f"


@pytest.fixture
def store(monkeypatch: pytest.MonkeyPatch) -> Any:
    fake = Store()
    monkeypatch.setattr(_traces, "request", fake)
    _config.adopt(_config.Config(host="http://x", key="tp-sk-x"))
    return fake


def test_the_single_dry_run_sends_no_confirm(store: Store) -> None:
    store.answers[f"/api/v1/traces/{TRACE}"] = {"dry_run": True, "confirm": TRACE}
    assert tracepad.delete_trace(TRACE)["dry_run"] is True
    assert store.calls == [("DELETE", f"/api/v1/traces/{TRACE}", None, {})]


def test_the_single_confirm_echoes_the_id(store: Store) -> None:
    store.answers[f"/api/v1/traces/{TRACE}"] = {"dry_run": False, "deleted": {"traces": 1}}
    assert tracepad.delete_trace(TRACE, confirm=True)["deleted"] == {"traces": 1}
    assert store.calls[0][3] == {"confirm": TRACE}


def test_the_bulk_dry_run_passes_the_filters_through(store: Store) -> None:
    store.answers["/api/v1/traces"] = {"dry_run": True, "matched": 3}
    answer = tracepad.delete_traces(
        to=datetime(2026, 9, 17, 14, 2, 17),  # naive: UTC
        from_="2026-09-01T00:00:00Z", environment="staging", tag=["a", "b"], limit=5,
    )
    # The preview as the API gave it; one call, and neither `confirm` nor
    # `limit` on it — the dry run has no rounds.
    assert answer == {"dry_run": True, "matched": 3}
    assert store.calls == [("DELETE", "/api/v1/traces", None, {
        "to": "2026-09-17T14:02:17Z", "from": "2026-09-01T00:00:00Z",
        "environment": "staging", "tag": ["a", "b"],
    })]


def test_an_aware_time_is_sent_in_utc(store: Store) -> None:
    tracepad.delete_traces(to=datetime(2026, 9, 17, 16, 0, tzinfo=timezone(timedelta(hours=2))))
    assert store.calls[0][3] == {"to": "2026-09-17T14:00:00Z"}


def test_the_confirmed_bulk_walks_the_rounds_and_sums(store: Store) -> None:
    store.answers["/api/v1/traces"] = [
        {"deleted": {"traces": 1000, "observations": 4000, "payloads": 9}, "more": True},
        {"deleted": {"traces": 1000, "observations": 3000, "payloads": 0}, "more": True},
        {"deleted": {"traces": 12, "observations": 30, "payloads": 1}, "more": False},
    ]
    total = tracepad.delete_traces(to="2026-09-17T14:02:17Z", environment="staging",
                                   confirm="my-project", limit=1000)
    assert total == {"deleted": {"traces": 2012, "observations": 7030, "payloads": 10}, "rounds": 3}
    assert [params for _, _, _, params in store.calls] == [{
        "to": "2026-09-17T14:02:17Z", "environment": "staging",
        "confirm": "my-project", "limit": 1000,
    }] * 3


def test_a_round_that_finds_nothing_is_one_round(store: Store) -> None:
    store.answers["/api/v1/traces"] = {"dry_run": False, "deleted": {"traces": 0}, "more": False}
    assert tracepad.delete_traces(to="2026-09-17T14:02:17Z", confirm="my-project") == {
        "deleted": {"traces": 0}, "rounds": 1,
    }


def test_an_unknown_trace_is_raised_not_none(store: Store) -> None:
    store.answers[f"/api/v1/traces/{TRACE}"] = TracepadHTTPError(404, '{"error":"not found"}')
    with pytest.raises(TracepadHTTPError) as raised:
        tracepad.delete_trace(TRACE, confirm=True)
    assert raised.value.status == 404


def test_a_wrong_echo_stops_at_the_first_round(store: Store) -> None:
    store.answers["/api/v1/traces"] = [TracepadHTTPError(400, "confirm"), {"more": False}]
    with pytest.raises(TracepadHTTPError, match="400"):
        tracepad.delete_traces(to="2026-09-17T14:02:17Z", confirm="wrong")
    assert len(store.calls) == 1
