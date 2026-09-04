"""Reading an OpenAI-compatible response (spec 017 #5).

One reader, one shape. OpenRouter puts the charge the provider actually made
in `usage.cost`, and OpenAI's envelope is what every proxy and most providers
speak, so this carries the common case; every other provider is carried by the
explicit arguments of `Generation.end`, which win over anything read here.

Reading a response is not wrapping a client: the call stays the application's,
made however it likes. And the cost is never estimated — what this cannot
read, it does not send, so the store shows *no data* rather than `$0`.
"""

from __future__ import annotations

from collections.abc import Mapping
from typing import Any


def read_response(response: Any) -> dict[str, Any]:
    """The fields of an OpenAI-compatible response, by our own names.

    The object may be a `dict` or anything with the attributes — the OpenAI
    client's pydantic models qualify. Absent fields are absent from the
    result rather than present and empty.
    """
    fields: dict[str, Any] = {}
    model = _get(response, "model")
    if isinstance(model, str) and model:
        fields["model"] = model

    usage = _get(response, "usage")
    if usage is not None:
        counts: dict[str, Any] = {}
        _count(counts, "input_tokens", _get(usage, "prompt_tokens"))
        _count(counts, "output_tokens", _get(usage, "completion_tokens"))
        _count(counts, "cache_read_input_tokens",
               _get(_get(usage, "prompt_tokens_details"), "cached_tokens"))
        _count(counts, "reasoning_tokens",
               _get(_get(usage, "completion_tokens_details"), "reasoning_tokens"))
        if counts:
            fields["usage"] = counts
        cost = _get(usage, "cost")
        if _is_number(cost):
            fields["cost"] = cost

    content = _content(response)
    if content is not None:
        fields["output"] = content
    return fields


def _content(response: Any) -> Any:
    """`choices[0].message.content`, the answer in the common envelope."""
    choices = _get(response, "choices")
    if not isinstance(choices, (list, tuple)) or not choices:
        return None
    return _get(_get(choices[0], "message"), "content")


def _get(obj: Any, name: str) -> Any:
    if obj is None:
        return None
    if isinstance(obj, Mapping):
        return obj.get(name)
    return getattr(obj, name, None)


def _count(counts: dict[str, Any], name: str, value: Any) -> None:
    if _is_number(value):
        counts[name] = value


def _is_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool)
