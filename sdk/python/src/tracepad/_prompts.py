"""Prompts: fetched by label, cached for as long as the server says (spec 017 #8).

A prompt is read per request and changes per deploy. The store already says
how long a label may be trusted (`Cache-Control: max-age=60`,
`docs/prompts.md`), so the cache honours that and nothing else; when the store
is away the last answer is served stale, because the reference application
must survive a restart of its own observability. With nothing cached the call
raises: a fallback prompt baked into the code is a prompt the trace cannot
name.
"""

from __future__ import annotations

import re
import threading
import time
from dataclasses import dataclass, field
from typing import Any
from urllib.parse import quote

from . import _config
from ._errors import TracepadError, TracepadPlaceholderError
from ._http import max_age, request
from ._log import logger


@dataclass(frozen=True)
class Prompt:
    """One version of a stored prompt."""

    name: str
    version: int
    type: str
    text: str | None = None
    messages: list[dict[str, Any]] | None = None
    labels: list[str] = field(default_factory=list)
    config: dict[str, Any] = field(default_factory=dict)

    def compile(self, /, **variables: Any) -> str | list[dict[str, Any]]:
        """Substitute `{name}` placeholders, in the text or in every message.

        A placeholder with no variable raises `TracepadPlaceholderError`: a
        prompt sent with a hole in it is a worse failure than one not sent. A
        message whose content is not a string (a list of parts) is passed on
        as it is. Nothing else: a template language is a product, and what
        the store stores is plain text (spec 017 #8, #18).
        """
        if self.messages is not None:
            return [
                {**message, "content": _fill(content, variables)}
                if isinstance(content := message.get("content", ""), str) else dict(message)
                for message in self.messages
            ]
        return _fill(self.text or "", variables)


_PLACEHOLDER = re.compile(r"\{\{|\}\}|\{([^{}]*)\}")


def _fill(template: str, variables: dict[str, Any]) -> str:
    """The reading the JS package's `fill` has: `{{` and `}}` are the braces
    themselves, and what stands between two braces is a name, looked up as
    written. Never `str.format`: the text is the store's, written by whoever
    holds the project key, and a format spec or an attribute chain in it would
    run in this process (spec 017 #18)."""

    def one(match: re.Match[str]) -> str:
        name = match.group(1)
        if name is None:
            return match.group(0)[0]
        if name not in variables:
            raise TracepadPlaceholderError(
                f"tracepad: prompt placeholder {{{name}}} has no variable"
            )
        # An empty spec, never one from the text: what `str.format` gave for
        # `{name}`, which for a str- or int-mixin Enum before 3.12 is its value.
        return format(variables[name], "")

    return _PLACEHOLDER.sub(one, template)


@dataclass
class _Entry:
    prompt: Prompt
    expires_at: float


_lock = threading.Lock()
_cache: dict[tuple[str, str | None, int | None], _Entry] = {}


def prompt(name: str, *, label: str | None = None, version: int | None = None) -> Prompt:
    """Fetch a prompt by label, by version, or the latest of them."""
    key = (name, label, version)
    now = time.monotonic()
    with _lock:
        cached = _cache.get(key)
    if cached is not None and cached.expires_at > now:
        return cached.prompt

    params: dict[str, Any] = {}
    if label is not None:
        params["label"] = label
    if version is not None:
        params["version"] = version
    try:
        answer = request(_config.current(), "GET", f"/api/v1/prompts/{quote(str(name), safe='')}",
                         params=params)
    except TracepadError as error:
        if cached is None or _is_client_error(error):
            raise
        # The store restarting must not take the application down; the label
        # it moved meanwhile is late by at most the window it published.
        logger.warning("tracepad: serving prompt %r from a stale cache: %s", name, error)
        return cached.prompt

    fetched = _read(answer.body)
    with _lock:
        _cache[key] = _Entry(fetched, time.monotonic() + max_age(answer.headers))
    return fetched


def _is_client_error(error: TracepadError) -> bool:
    """A 4xx is about the request, not about the server being away: a moved
    label, a deleted prompt or a wrong key must be raised, not papered over."""
    status = getattr(error, "status", 0)
    return 400 <= status < 500


def _read(body: Any) -> Prompt:
    if not isinstance(body, dict):
        raise TracepadError(f"tracepad: unexpected prompt answer: {body!r}")
    stored = body.get("prompt")
    messages = stored if isinstance(stored, list) else None
    return Prompt(
        name=body.get("name", ""),
        version=body.get("version", 0),
        type=body.get("type", ""),
        text=stored if isinstance(stored, str) else None,
        messages=messages,
        labels=list(body.get("labels") or []),
        config=dict(body.get("config") or {}),
    )


def forget() -> None:
    """Empty the cache. For tests."""
    with _lock:
        _cache.clear()
