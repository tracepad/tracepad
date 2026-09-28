"""Datasets and their items (spec 018 #1).

A `Dataset` has made no request: `tracepad.dataset("x")` is a name, not a
network call. Everything below is synchronous and raises `TracepadError` —
the harness is a script, and a script wants a value or an exception.
"""

from __future__ import annotations

from collections.abc import Iterator
from dataclasses import dataclass, fields
from typing import Any
from urllib.parse import quote

from . import _config
from ._harness import PAGE, Run, pages
from ._http import request


@dataclass(frozen=True)
class Item:
    """One case. Tracepad never reads inside `input` (`docs/datasets.md`)."""

    id: str | None = None
    input: Any = None
    expected_output: Any = None
    metadata: Any = None
    #: The version this row was written at. Read back, never sent.
    dataset_version: int | None = None
    source_trace_id: str | None = None
    source_observation_id: str | None = None

    def body(self) -> dict[str, Any]:
        """What goes on the wire: unset fields are omitted, not sent as null."""
        return {
            f.name: getattr(self, f.name)
            for f in fields(self)
            if f.name != "dataset_version" and getattr(self, f.name) is not None
        }

    @classmethod
    def read(cls, row: dict[str, Any]) -> Item:
        # The store calls it `version` — the version this row was written at.
        # Reading it under its own name is what filled `dataset_version` with
        # `None` on every item (found in review of PR #36).
        known = {f.name for f in fields(cls)} - {"dataset_version"}
        return cls(dataset_version=row.get("version"),
                   **{k: v for k, v in row.items() if k in known})


class Dataset:
    """A named set of cases, and the runs over it."""

    def __init__(self, name: str) -> None:
        self.name = name
        self._path = f"/api/v1/datasets/{quote(str(name), safe='')}"

    def create(self, description: str | None = None,
               metadata: Any = None) -> dict[str, Any]:
        """Create it, or replace its description and metadata."""
        body: dict[str, Any] = {}
        if description is not None:
            body["description"] = description
        if metadata is not None:
            body["metadata"] = metadata
        return dict(self._call("PUT", "", body=body))

    def put_items(self, items: Any) -> tuple[int, int]:
        """Push the cases whole, and report `(version, changed)`.

        The same cases again write nothing and leave the version where it was
        (`changed == 0`), so this belongs at the top of every CI run. A list
        longer than the 10,000 items one request takes is sent as consecutive
        writes of that many, each its own version (spec 018 #15): `version` is
        the last one's, `changed` the sum, and a failure leaves the writes
        before it in place.
        """
        body = [item if isinstance(item, dict) else item.body() for item in items]
        if len(body) > MAX_ITEMS_PER_WRITE:
            _refuse_repeated_ids(body)
        version = changed = 0
        # An empty list is sent all the same: the server says what is wrong.
        for start in range(0, max(len(body), 1), MAX_ITEMS_PER_WRITE):
            answer = self._call("POST", "/items", body=body[start:start + MAX_ITEMS_PER_WRITE])
            version = int(answer["version"])
            changed += int(answer["changed"])
        return version, changed

    def items(self, version: int | None = None) -> Iterator[Item]:
        """The cases at a version — the run's, not "the current one"."""
        params: dict[str, Any] = {"limit": PAGE}
        if version is not None:
            params["version"] = version
        for row in pages(f"{self._path}/items", params, "items"):
            yield Item.read(row)

    def run(self, name: str, *, metadata: Any = None, id: str | None = None,
            dataset_version: int | None = None) -> Run:
        """Open a run, pinned to a version it hands back (spec 018 #2)."""
        body: dict[str, Any] = {"name": name}
        for key, value in (("metadata", metadata), ("id", id),
                           ("dataset_version", dataset_version)):
            if value is not None:
                body[key] = value
        return Run(self, self._call("POST", "/runs", body=body))

    def runs(self) -> Iterator[dict[str, Any]]:
        """This dataset's runs, newest first."""
        return pages(f"{self._path}/runs", {"limit": PAGE}, "runs")

    def delete(self, confirm: str) -> dict[str, Any]:
        """Delete it with its items and runs. The name must be echoed."""
        return dict(self._call("DELETE", "", params={"confirm": confirm}))

    def _call(self, method: str, path: str, **kwargs: Any) -> dict[str, Any]:
        return request(_config.current(), method, self._path + path, **kwargs).body or {}


#: The most items one `POST …/items` takes (spec 014 #34).
MAX_ITEMS_PER_WRITE = 10_000


def _refuse_repeated_ids(body: list[dict[str, Any]]) -> None:
    """One request refuses an id given twice; split across two, the second
    would quietly become an edit of the first. So the whole list is checked
    before any of it is sent."""
    seen: dict[str, int] = {}
    for index, item in enumerate(body):
        id = item.get("id")
        if id is None:
            continue
        if id in seen:
            raise ValueError(f"tracepad: put_items: the item at index {index} "
                             f"repeats id {id} of the item at index {seen[id]}")
        seen[id] = index


def dataset(name: str) -> Dataset:
    """A dataset by name. No request is made here."""
    return Dataset(name)
