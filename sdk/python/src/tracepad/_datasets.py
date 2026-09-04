"""Datasets and their items (spec 018 #1).

A `Dataset` has made no request: `tracepad.dataset("x")` is a name, not a
network call. Everything below is synchronous and raises `TracepadError` —
the harness is a script, and a script wants a value or an exception.
"""

from __future__ import annotations

from collections.abc import Iterator
from dataclasses import dataclass, fields
from typing import Any

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
        self._path = f"/api/v1/datasets/{name}"

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
        (`changed == 0`), so this belongs at the top of every CI run.
        """
        body = [item if isinstance(item, dict) else item.body() for item in items]
        answer = self._call("POST", "/items", body=body)
        return int(answer["version"]), int(answer["changed"])

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


def dataset(name: str) -> Dataset:
    """A dataset by name. No request is made here."""
    return Dataset(name)
