"""Deleting traces: one by id, or every trace a listing filter matches (spec 036).

A script's act — an eval harness that exported under the wrong key, a test
suite that wants its traces gone before the next run — so both calls are
synchronous and raise, like every REST call here. The module is its own so
that the tracing path never imports it, and `urllib` stays off that path.
"""

from __future__ import annotations

from datetime import datetime, timezone
from typing import Any

from . import _config
from ._http import request

#: The most traces one confirmed round may take: the API's own bound.
ROUND = 1000


def delete_trace(id: str, *, confirm: bool = False) -> dict[str, Any]:
    """Delete one trace and everything attached to it.

    Without `confirm` this is the API's dry run, and the answer is its preview
    (`would_delete`, `affected_runs`, `confirm`, `note`). With `confirm=True`
    the id is sent as the echo the API asks for, and the answer says what went.
    An unknown id raises `TracepadHTTPError` with a 404 either way.
    """
    return _call(f"/api/v1/traces/{id}", {"confirm": id} if confirm else {})


def delete_traces(*, to: datetime | str, confirm: str = "", limit: int = ROUND,
                  **filters: Any) -> dict[str, Any]:
    """Delete every trace the listing's filters match that started before `to`.

    The filters are the listing's, by their API names (`docs/api.md`): `from_`
    for `from`, `tag` a list. `to` is required so that the set is closed; a
    time is a `datetime` or an RFC 3339 string, and a naive `datetime` is UTC.
    An unknown filter name is the API's 400, raised.

    With no `confirm` this is one dry run, and the answer is the API's preview
    (`matched`, `would_delete`, `affected_runs`, `oldest`, `confirm`, `note`).
    With the project's name as `confirm` it deletes in rounds of at most
    `limit` traces (1 to 1000), repeating while the API says `more`, and returns
    the total: `{"deleted": {"traces", "observations", "scores", "payloads",
    "annotation_items"}, "rounds": N}`.
    """
    params = {"from" if name == "from_" else name: _stamp(value)
              for name, value in filters.items()}
    params["to"] = _stamp(to)
    if not confirm:
        return _call("/api/v1/traces", params)
    params.update(confirm=confirm, limit=limit)
    deleted: dict[str, int] = {}
    rounds = 0
    while True:
        answer = _call("/api/v1/traces", params)
        rounds += 1
        for kind, count in (answer.get("deleted") or {}).items():
            deleted[kind] = deleted.get(kind, 0) + int(count)
        if not answer.get("more"):
            return {"deleted": deleted, "rounds": rounds}


def _stamp(value: Any) -> Any:
    """A `datetime` as RFC 3339 UTC — naive is UTC — and anything else as it is."""
    if not isinstance(value, datetime):
        return value
    if value.tzinfo is None:
        value = value.replace(tzinfo=timezone.utc)
    return value.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")


def _call(path: str, params: dict[str, Any]) -> dict[str, Any]:
    return dict(request(_config.current(), "DELETE", path, params=params).body or {})
