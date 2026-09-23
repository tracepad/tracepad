"""What goes on the wire: the body's encoding (spec 014 #32)."""

from __future__ import annotations

import json
import urllib.request
from typing import Any, ClassVar

import pytest

from tracepad import _config
from tracepad._http import request


class Answer:
    status = 200
    headers: ClassVar[dict[str, str]] = {}

    def __enter__(self) -> Answer:
        return self

    def __exit__(self, *_: Any) -> None:
        return None

    def read(self) -> bytes:
        return b"{}"


def sent(monkeypatch: pytest.MonkeyPatch, body: Any) -> bytes:
    """The bytes `request` hands to urllib for this body."""
    calls: list[urllib.request.Request] = []

    def urlopen(call: urllib.request.Request, timeout: float) -> Answer:
        calls.append(call)
        return Answer()

    monkeypatch.setattr(urllib.request, "urlopen", urlopen)
    request(_config.Config(host="http://x", key="tp-sk-x"), "POST", "/api/v1/x", body=body)
    assert len(calls) == 1
    data = calls[0].data
    assert isinstance(data, bytes)
    return data


def test_non_ascii_goes_as_raw_utf8(monkeypatch: pytest.MonkeyPatch) -> None:
    body = {"input": {"q": "Καλημέρα κόσμε"}, "note": "café ✓"}

    data = sent(monkeypatch, body)

    assert "Καλημέρα κόσμε".encode() in data
    assert b"\\u" not in data
    assert json.loads(data) == body


def test_a_lone_surrogate_is_escaped_and_the_rest_stays_raw(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    body = {"input": "bad \udcff byte", "note": "Καλημέρα"}

    data = sent(monkeypatch, body)

    assert b"\\udcff" in data
    assert "Καλημέρα".encode() in data
    assert json.loads(data) == body
