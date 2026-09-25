"""The prompt cache: fresh, stale, and nothing at all (spec 017 #8)."""

from __future__ import annotations

import json
import logging
import re
from enum import Enum, IntEnum
from pathlib import Path
from typing import Any

import pytest

import tracepad
from tracepad import _config, _prompts
from tracepad._errors import TracepadError, TracepadHTTPError, TracepadPlaceholderError
from tracepad._http import Response
from tracepad._prompts import Prompt

CHAT = {
    "name": "support-answer",
    "version": 7,
    "type": "chat",
    "prompt": [{"role": "system", "content": "You answer about {product}."}],
    "config": {"model": "claude-sonnet-5"},
    "labels": ["production"],
}
TEXT = {"name": "summarize", "version": 2, "type": "text",
        "prompt": "Summarise {document} in one sentence.", "config": {}, "labels": []}


class Store:
    """A fake `_http.request` that records calls and answers what it is told."""

    def __init__(self, *answers: Any) -> None:
        self.answers = list(answers)
        self.calls: list[tuple[str, dict[str, Any]]] = []

    def __call__(self, config: Any, method: str, path: str, **kwargs: Any) -> Response:
        self.calls.append((path, kwargs.get("params") or {}))
        answer = self.answers.pop(0) if len(self.answers) > 1 else self.answers[0]
        if isinstance(answer, Exception):
            raise answer
        return answer


def store(monkeypatch: pytest.MonkeyPatch, *answers: Any) -> Store:
    fake = Store(*answers)
    monkeypatch.setattr(_prompts, "request", fake)
    _config.adopt(_config.Config(host="http://x", key="tp-sk-x"))
    return fake


def answer(body: dict[str, Any], age: int = 60) -> Response:
    return Response(200, body, {"cache-control": f"max-age={age}"})


def test_a_prompt_is_fetched_and_cached(monkeypatch: pytest.MonkeyPatch) -> None:
    fake = store(monkeypatch, answer(CHAT))

    first = tracepad.prompt("support-answer", label="production")
    second = tracepad.prompt("support-answer", label="production")

    assert first is second
    assert fake.calls == [("/api/v1/prompts/support-answer", {"label": "production"})]
    assert (first.name, first.version, first.type) == ("support-answer", 7, "chat")
    assert first.labels == ["production"]
    assert first.config == {"model": "claude-sonnet-5"}
    assert first.text is None


def test_the_cache_expires_when_the_server_said_it_would(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    fake = store(monkeypatch, answer(CHAT), answer({**CHAT, "version": 8}))
    moment = [1000.0]
    monkeypatch.setattr(_prompts.time, "monotonic", lambda: moment[0])

    assert tracepad.prompt("support-answer").version == 7
    moment[0] += 59
    assert tracepad.prompt("support-answer").version == 7
    moment[0] += 2
    assert tracepad.prompt("support-answer").version == 8
    assert len(fake.calls) == 2


def test_a_version_and_a_label_are_different_cache_entries(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    fake = store(monkeypatch, answer(CHAT))
    tracepad.prompt("support-answer", label="production")
    tracepad.prompt("support-answer", version=3)

    assert [params for _, params in fake.calls] == [{"label": "production"}, {"version": 3}]


def test_a_transport_failure_is_served_stale(
    monkeypatch: pytest.MonkeyPatch, caplog: pytest.LogCaptureFixture
) -> None:
    fake = store(monkeypatch, answer(CHAT, age=0), TracepadError("connection refused"))

    fresh = tracepad.prompt("support-answer")
    with caplog.at_level(logging.WARNING, logger="tracepad"):
        stale = tracepad.prompt("support-answer")

    assert stale is fresh
    assert "stale cache" in caplog.text
    assert len(fake.calls) == 2


def test_a_client_error_is_raised_even_with_something_cached(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    store(monkeypatch, answer(CHAT, age=0), TracepadHTTPError(404, "no such prompt"))
    tracepad.prompt("support-answer")

    with pytest.raises(TracepadHTTPError):
        tracepad.prompt("support-answer")


def test_nothing_cached_raises(monkeypatch: pytest.MonkeyPatch) -> None:
    store(monkeypatch, TracepadError("connection refused"))
    with pytest.raises(TracepadError, match="connection refused"):
        tracepad.prompt("support-answer")


def test_compile_substitutes_in_messages(monkeypatch: pytest.MonkeyPatch) -> None:
    store(monkeypatch, answer(CHAT))
    compiled = tracepad.prompt("support-answer").compile(product="Tracepad")
    assert compiled == [{"role": "system", "content": "You answer about Tracepad."}]


def test_compile_substitutes_in_text(monkeypatch: pytest.MonkeyPatch) -> None:
    store(monkeypatch, answer(TEXT))
    prompt = tracepad.prompt("summarize")
    assert prompt.messages is None
    assert prompt.compile(document="the changelog") == "Summarise the changelog in one sentence."


# The Node and Go packages run the same table (sdk/js/test/prompts.test.ts,
# sdk/go/prompts_test.go): one stored text, compiled with string variables,
# must be one prompt, whichever package reads it. Read from the repository,
# so a test run from an unpacked sdist, which has no testdata/, skips it.
TABLE = Path(__file__).parents[3] / "testdata" / "prompts" / "compile.json"
CASES = json.loads(TABLE.read_text())["cases"] if TABLE.is_file() else []


@pytest.mark.skipif(not TABLE.is_file(), reason="no testdata/: not a checkout of the repository")
@pytest.mark.parametrize("case", CASES, ids=[case["name"] for case in CASES])
def test_compile_reads_what_the_node_package_reads(case: dict[str, Any]) -> None:
    prompt = Prompt(name="p", version=1, type="chat" if "messages" in case else "text",
                    text=case.get("text"), messages=case.get("messages"))
    if "error" in case:
        with pytest.raises(TracepadError, match=re.escape(case["error"])):
            prompt.compile(**case["variables"])
    else:
        assert prompt.compile(**case["variables"]) == case["compiled"]


def test_a_placeholder_with_no_variable_is_a_key_error_too() -> None:
    with pytest.raises(KeyError) as raised:
        Prompt(name="p", version=1, type="text", text="{missing}").compile()
    assert isinstance(raised.value, TracepadPlaceholderError)
    assert str(raised.value) == "tracepad: prompt placeholder {missing} has no variable"


class User:
    """An object an application might well pass, with something behind it."""

    email = "someone@example.com"

    def __init__(self) -> None:
        self.api_key = "sk-not-for-the-prompt"


@pytest.mark.parametrize("text", [
    "{user.api_key}",
    "{user.__class__.__init__.__globals__[os].environ}",
    "{user.email:>40}",
    "{user!r}",
])
def test_compile_evaluates_nothing_in_the_stored_text(text: str) -> None:
    with pytest.raises(TracepadError) as raised:
        Prompt(name="p", version=1, type="text", text=text).compile(user=User())
    assert "sk-not-for-the-prompt" not in str(raised.value)
    assert "someone@example.com" not in str(raised.value)


def test_compile_hands_a_variable_to_str_and_nothing_more() -> None:
    compiled = Prompt(name="p", version=1, type="text", text="Hello {user}").compile(user=User())
    assert compiled.startswith("Hello <")
    assert "sk-not-for-the-prompt" not in compiled


class Tone(str, Enum):
    FRIENDLY = "friendly"


class Size(IntEnum):
    LARGE = 3


@pytest.mark.parametrize("value", [Tone.FRIENDLY, Size.LARGE, 2.5, None, True])
def test_a_value_renders_as_str_format_rendered_it(value: Any) -> None:
    # Before 3.12 a mixin Enum formats as its value and prints as its name.
    compiled = Prompt(name="p", version=1, type="text", text="[{v}]").compile(v=value)
    assert compiled == f"[{value}]"


def test_a_message_whose_content_is_not_a_string_is_passed_on_as_it_is() -> None:
    parts = [{"type": "text", "text": "Hi {q}"}, {"type": "image_url", "image_url": {"url": "x"}}]
    prompt = Prompt(name="p", version=1, type="chat", messages=[
        {"role": "system", "content": "About {q}."},
        {"role": "user", "content": parts},
    ])
    assert prompt.compile(q="refunds") == [
        {"role": "system", "content": "About refunds."},
        {"role": "user", "content": parts},
    ]
