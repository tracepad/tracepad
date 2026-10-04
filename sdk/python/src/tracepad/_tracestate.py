"""What `update_trace` has said on a span so far (spec 017 #24).

OpenTelemetry keeps one value per attribute, so a second call that wrote
`tracepad.trace.tags` or `tracepad.trace.metadata` again replaced the first
call's. The package remembers what it has written on each span, in memory and
under a lock, merges each call into that, and writes both attributes whole:
tags in the order first seen, metadata by top-level key with the later value
winning. The wire is what it always was — a JSON array and a JSON object.

Only what this package wrote is merged. A span's attributes cannot be read back
through the OpenTelemetry API, so tags another writer set on the same span are
replaced by the first call, as they were before.
"""

from __future__ import annotations

import threading
import weakref
from collections.abc import Mapping
from typing import Any

from . import _attributes as attrs
from ._log import logger

# The server's own bounds (spec 043 #14, spec 002 #32): beyond them it drops
# what it was sent, so the package stops sending it.
MAX_TAGS = 50
MAX_KEYS = 512
MAX_BYTES = 1 << 20


class _State:
    def __init__(self) -> None:
        self.tags: list[Any] = []
        self.metadata: dict[Any, Any] = {}


_lock = threading.Lock()
_states: weakref.WeakKeyDictionary[Any, _State] = weakref.WeakKeyDictionary()
_warned = False


def update(span: Any, tags: Any, metadata: Any) -> dict[str, str]:
    """Merge a call into the span's state; the attributes to write, whole."""
    written: dict[str, str] = {}
    with _lock:
        state = _states.setdefault(span, _State())
        if tags is not None:
            for tag in tags:
                if tag not in state.tags and len(state.tags) < MAX_TAGS:
                    state.tags.append(tag)
            written[attrs.TRACE_TAGS] = attrs.dumps(state.tags)
        if metadata is not None:
            encoded = _merge(state.metadata, metadata)
            if encoded is not None:
                written[attrs.TRACE_METADATA] = encoded
    return written


def _merge(held: dict[Any, Any], new: Any) -> str | None:
    """Add a call's keys to `held`, replacing the ones it names, within the
    bounds; the object to write, encoded once, or nothing to write."""
    if not isinstance(new, Mapping):
        _warn("update_trace(metadata=) takes a mapping; this one was ignored")
        return None
    candidate = dict(held)
    for key, value in new.items():
        if key in held or len(candidate) < MAX_KEYS:
            candidate[key] = value
        else:
            _warn(
                f"trace metadata is bounded at {MAX_KEYS} keys; the new keys past it were dropped"
            )
    encoded = attrs.dumps(candidate)
    if len(encoded.encode()) > MAX_BYTES:
        candidate = dict(held)
        for key, value in new.items():
            trial = {**candidate, key: value}
            if (key in held or len(candidate) < MAX_KEYS) and len(
                attrs.dumps(trial).encode()
            ) <= MAX_BYTES:
                candidate = trial
            else:
                _warn(
                    f"trace metadata is bounded at {MAX_BYTES} bytes; the keys past it were dropped"
                )
        encoded = attrs.dumps(candidate)
    held.clear()
    held.update(candidate)
    return encoded


def _warn(message: str) -> None:
    """Once per process: a bound that bites is said, not repeated on every call."""
    global _warned
    if not _warned:
        _warned = True
        logger.warning("tracepad: %s", message)
