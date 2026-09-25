"""What goes on the wire: the body's encoding (spec 014 #32), the key, and the path."""

from __future__ import annotations

import json
import threading
import urllib.request
from collections.abc import Iterator
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, ClassVar

import pytest

import tracepad
from tracepad import _config
from tracepad._errors import TracepadError, TracepadHTTPError
from tracepad._http import request

KEY = "tp-sk-never-leaves-the-store"


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


class Store(ThreadingHTTPServer):
    """Records every request as it arrived; `/hop/…` redirects to `hop_to`."""

    def __init__(self) -> None:
        super().__init__(("127.0.0.1", 0), Seen)
        self.seen: list[tuple[str, str, str | None]] = []
        self.hop_to = ""
        self.url = f"http://127.0.0.1:{self.server_address[1]}"
        threading.Thread(target=self.serve_forever, args=(0.01,), daemon=True).start()


class Seen(BaseHTTPRequestHandler):
    def do(self) -> None:
        store: Store = self.server  # type: ignore[assignment]
        self.rfile.read(int(self.headers.get("Content-Length") or 0))
        store.seen.append((self.command, self.path, self.headers.get("Authorization")))
        if self.path.startswith("/hop/"):
            self.send_response(302)
            self.send_header("Location", store.hop_to + self.path[4:])
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        body = json.dumps({"name": "n", "version": 1, "type": "text", "prompt": "hi",
                           "config": {}, "labels": []}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    do_GET = do_POST = do_PUT = do_DELETE = do

    def log_message(self, *_: Any) -> None:
        return None


@pytest.fixture
def servers() -> Iterator[tuple[Store, Store]]:
    """Two stores on two ports of one host: two origins, as `fetch` counts them."""
    pair = (Store(), Store())
    yield pair
    for one in pair:
        one.shutdown()
        one.server_close()


def test_a_redirect_to_another_origin_goes_without_the_key(
    servers: tuple[Store, Store],
) -> None:
    store, elsewhere = servers
    store.hop_to = elsewhere.url
    config = _config.Config(host=store.url, key=KEY)

    request(config, "GET", "/hop/api/v1/prompts/n")
    request(config, "POST", "/hop/api/v1/scores", body=[])  # urllib makes it a GET

    assert [auth for _, _, auth in store.seen] == [f"Bearer {KEY}"] * 2
    assert elsewhere.seen == [("GET", "/api/v1/prompts/n", None), ("GET", "/api/v1/scores", None)]


def test_a_redirect_within_the_origin_goes_without_the_key_too(
    servers: tuple[Store, Store],
) -> None:
    store, _ = servers
    store.hop_to = store.url
    config = _config.Config(host=store.url, key=KEY)

    request(config, "GET", "/hop/api/v1/prompts/n")
    # The POST a 302 turned into a GET reaches no listing as the key: the
    # store answers it `401`, not the page a queue would read as delivered.
    request(config, "POST", "/hop/api/v1/scores", body=[])

    assert store.seen[1::2] == [("GET", "/api/v1/prompts/n", None), ("GET", "/api/v1/scores", None)]


def test_the_key_does_not_come_back_when_the_chain_does(
    servers: tuple[Store, Store],
) -> None:
    store, elsewhere = servers
    store.hop_to, elsewhere.hop_to = elsewhere.url, store.url

    request(_config.Config(host=store.url, key=KEY), "GET", "/hop/hop/api/v1/prompts/n")

    assert elsewhere.seen == [("GET", "/hop/api/v1/prompts/n", None)]
    assert store.seen == [("GET", "/hop/hop/api/v1/prompts/n", f"Bearer {KEY}"),
                          ("GET", "/api/v1/prompts/n", None)]


def test_the_key_is_not_in_the_configs_repr() -> None:
    config = _config.Config(host="http://x", key=KEY)

    assert KEY not in repr(config)
    assert KEY not in str(config)
    assert config.key == KEY


@pytest.mark.parametrize(("name", "segment"), [
    ("x?confirm=x#", "x%3Fconfirm%3Dx%23"),
    ("a/b", "a%2Fb"),
    ("50%", "50%25"),
    ("with space", "with%20space"),
    ("#", "%23"),
    ("ünï", "%C3%BCn%C3%AF"),
])
def test_a_name_is_one_segment_whatever_it_holds(
    servers: tuple[Store, Store], name: str, segment: str,
) -> None:
    store, _ = servers
    _config.adopt(_config.Config(host=store.url, key=KEY))

    tracepad.dataset(name).delete("")
    tracepad.prompt(name, label="production")
    tracepad.score_configs([{"name": name, "data_type": "boolean"}])
    tracepad.compare(name, name)

    assert [(method, path) for method, path, _ in store.seen] == [
        ("DELETE", f"/api/v1/datasets/{segment}?confirm="),
        ("GET", f"/api/v1/prompts/{segment}?label=production"),
        ("PUT", f"/api/v1/score-configs/{segment}"),
        ("GET", f"/api/v1/runs/{segment}/compare/{segment}"),
    ]


def test_a_name_that_is_not_a_string_is_sent_as_its_text(servers: tuple[Store, Store]) -> None:
    store, _ = servers
    _config.adopt(_config.Config(host=store.url, key=KEY))

    tracepad.dataset(2024).delete("")  # type: ignore[arg-type]
    tracepad.score_configs([{"name": 5, "data_type": "boolean"}])
    tracepad.compare(1, 2)  # type: ignore[arg-type]

    assert [path for _, path, _ in store.seen] == [
        "/api/v1/datasets/2024?confirm=", "/api/v1/score-configs/5", "/api/v1/runs/1/compare/2"]


@pytest.mark.parametrize("name", ["", ".", ".."])
def test_a_name_that_is_no_segment_is_refused_before_the_wire(
    servers: tuple[Store, Store], name: str,
) -> None:
    store, _ = servers
    _config.adopt(_config.Config(host=store.url, key=KEY))

    with pytest.raises(TracepadError, match="empty or dot segment"):
        tracepad.dataset(name).delete(name)
    with pytest.raises(TracepadError, match="empty or dot segment"):
        tracepad.prompt(name)

    assert store.seen == []


def test_a_refused_redirect_is_still_an_http_error(servers: tuple[Store, Store]) -> None:
    store, elsewhere = servers
    store.hop_to = elsewhere.url

    # urllib follows no redirect of a DELETE; what it raises is the store's answer.
    with pytest.raises(TracepadHTTPError) as raised:
        request(_config.Config(host=store.url, key=KEY), "DELETE", "/hop/api/v1/datasets/x")

    assert raised.value.status == 302
    assert elsewhere.seen == []
