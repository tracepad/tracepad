"""The binary every end-to-end module runs against."""

from __future__ import annotations

from typing import Any

import pytest
from harness import serve


@pytest.fixture(scope="module")
def store(tmp_path_factory: pytest.TempPathFactory) -> Any:
    process, running = serve(str(tmp_path_factory.mktemp("store")))
    try:
        yield running
    finally:
        process.terminate()
        process.wait(timeout=10)
