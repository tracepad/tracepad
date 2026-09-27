"""A real binary on a temporary database, and the read API as a person calls it.

Shared by the end-to-end modules; the `store` fixture that runs it lives in
this directory's `conftest.py`, which is where pytest looks for one.
"""

from __future__ import annotations

import json
import os
import socket
import subprocess
import time
import urllib.error
import urllib.request
from typing import Any

BINARY = os.environ.get("TRACEPAD_BINARY", "")
KEY = "tp-sk-e2e-0000000000000000000000000000"
# Minting a key is a person's act or the admin token's, never a key's (spec 045 #4).
ADMIN_TOKEN = "tp-admin-e2e-00000000000000000000000000"


class Store:
    """A running binary, and the read API as a person would call it."""

    def __init__(self, host: str) -> None:
        self.host = host

    def call(self, method: str, path: str, body: Any = None, token: str = KEY) -> Any:
        request = urllib.request.Request(
            self.host + path,
            data=None if body is None else json.dumps(body).encode(),
            method=method,
            headers={"Authorization": f"Bearer {token}"},
        )
        with urllib.request.urlopen(request, timeout=10) as answer:
            raw = answer.read()
        return json.loads(raw) if raw else None


def serve(data_dir: str) -> tuple[subprocess.Popen[bytes], Store]:
    """Start the binary on a free port and wait for it to answer."""
    port = free_port()
    process = subprocess.Popen(
        [BINARY, "serve"],
        env={
            **os.environ,
            "TRACEPAD_DATA_DIR": data_dir,
            "TRACEPAD_LISTEN": f"127.0.0.1:{port}",
            "TRACEPAD_PROJECTS": f"e2e:tp-pk-e2e:{KEY}",
            "TRACEPAD_ADMIN_TOKEN": ADMIN_TOKEN,
        },
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )
    running = Store(f"http://127.0.0.1:{port}")
    try:
        await_health(running, process)
    except BaseException:
        # A server that never came up is still a process holding a port.
        process.terminate()
        process.wait(timeout=10)
        raise
    return process, running


def mint_key(store: Store, *scopes: str) -> str:
    """The secret of a new key of the project carrying only `scopes`."""
    (project,) = store.call("GET", "/api/v1/projects")["projects"]
    minted = store.call("POST", f"/api/v1/projects/{project['id']}/keys",
                        {"scopes": list(scopes)}, token=ADMIN_TOKEN)
    return str(minted["secret_key"])


def free_port() -> int:
    with socket.socket() as taken:
        taken.bind(("127.0.0.1", 0))
        return int(taken.getsockname()[1])


def await_health(store: Store, process: subprocess.Popen[bytes]) -> None:
    for _ in range(100):
        if process.poll() is not None:
            output = process.stdout.read().decode() if process.stdout else ""
            raise AssertionError(f"the server exited: {output}")
        try:
            store.call("GET", "/health")
            return
        except (urllib.error.URLError, OSError):
            time.sleep(0.1)
    raise AssertionError("the server never became healthy")


def trace_of(store: Store, trace_id: str, expand: str = "?expand=io") -> dict[str, Any]:
    """The trace, once the export has landed."""
    for _ in range(50):
        try:
            return dict(store.call("GET", f"/api/v1/traces/{trace_id}{expand}"))
        except urllib.error.HTTPError as missing:
            if missing.code != 404:
                raise
            time.sleep(0.1)
    raise AssertionError(f"trace {trace_id} never arrived")


def walk(observations: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """The observation tree, flattened depth-first."""
    out: list[dict[str, Any]] = []
    for observation in observations:
        out.append(observation)
        out.extend(walk(observation.get("children") or []))
    return out
