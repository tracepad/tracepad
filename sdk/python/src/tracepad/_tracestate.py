"""What `update_trace` has said on a span so far (spec 017 #24).

OpenTelemetry keeps one value per attribute, so a second call that wrote
`tracepad.trace.tags` or `tracepad.trace.metadata` again replaced the first
call's. The package remembers what it has written on each span, in memory, and
merges each call into that: tags in the order first seen, metadata by top-level
key with the later value winning, both written whole — the wire is one JSON
array and one JSON object, as it always was.

What is kept is JSON text, taken when the call is made: a value the application
changes afterwards is not changed in the trace, and a document is put together
by joining what is already encoded, so a call costs what it carries and no more.
Each span has a lock of its own, held across the merge *and* the write of the
attributes, so that two calls cannot leave the attribute of the one that merged
first.

Only what this package wrote is merged. A span's attributes cannot be read back
through the OpenTelemetry API, so tags another writer set on the same span are
replaced by the first call, as they were before.
"""

from __future__ import annotations

import json
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
        self.lock = threading.Lock()
        self.tags: dict[str, None] = {}  # JSON text of each tag, in order first seen
        self.metadata: dict[
            str, tuple[str, int]
        ] = {}  # JSON of the key -> (JSON of the value, its bytes)
        self.size = 2  # bytes of the object these make: braces, entries, commas


_registry = threading.Lock()
_states: weakref.WeakKeyDictionary[Any, _State] = weakref.WeakKeyDictionary()
_warned: set[str] = set()


def reset() -> None:
    """Forget what was warned about, and every span's state (`testing.reset`, spec 040)."""
    with _registry:
        _warned.clear()
        _states.clear()


def update(span: Any, tags: Any, metadata: Any) -> None:
    """Merge a call into the span's state and write both attributes whole."""
    state = _state_of(span)
    with state.lock:
        written: dict[str, str] = {}
        if tags is not None:
            for tag in tags:
                text = attrs.json_text(tag)
                if text in state.tags:
                    continue
                if len(state.tags) < MAX_TAGS:
                    state.tags[text] = None
                else:
                    _warn(
                        "tags",
                        f"trace tags are bounded at {MAX_TAGS}; the tags past them were dropped",
                    )
            written[attrs.TRACE_TAGS] = "[" + ",".join(state.tags) + "]"
        if metadata is not None and _merge(state, metadata):
            written[attrs.TRACE_METADATA] = (
                "{" + ",".join(f"{key}:{text}" for key, (text, _) in state.metadata.items()) + "}"
            )
        span.set_attributes(written)


def _state_of(span: Any) -> _State:
    try:
        with _registry:
            state = _states.get(span)
            if state is None:
                state = _states[span] = _State()
            return state
    except TypeError:
        # A span that cannot be a key — no weak reference, or unhashable — is a
        # span another provider made: this call is written on its own.
        _warn(
            "untracked",
            "update_trace() on a span it cannot keep state for; calls on it do not add up",
        )
        return _State()


def _merge(state: _State, new: Any) -> bool:
    """Add a call's keys, replacing the ones it names, within the bounds; whether
    there is an object to write."""
    if not isinstance(new, Mapping):
        _warn("shape", "update_trace(metadata=) takes a mapping; this one was ignored")
        return False
    for key, value in new.items():
        name = json.dumps(key if isinstance(key, str) else str(key), ensure_ascii=False)
        text = attrs.json_text(value)
        size = len(text.encode())
        held = state.metadata.get(name)
        if held is not None:
            total = state.size - held[1] + size
        elif len(state.metadata) >= MAX_KEYS:
            _warn(
                "keys",
                f"trace metadata is bounded at {MAX_KEYS} keys; the new keys past it were dropped",
            )
            continue
        else:
            total = state.size + len(name.encode()) + 1 + size + (1 if state.metadata else 0)
        if total > MAX_BYTES:
            _warn(
                "bytes",
                f"trace metadata is bounded at {MAX_BYTES} bytes; the keys past it were dropped",
            )
            continue
        state.metadata[name] = (text, size)
        state.size = total
    return True


def _warn(kind: str, message: str) -> None:
    """Once per process for each kind: a bound that bites is said, not repeated on every call."""
    with _registry:
        first = kind not in _warned
        _warned.add(kind)
    if first:
        logger.warning("tracepad: %s", message)
