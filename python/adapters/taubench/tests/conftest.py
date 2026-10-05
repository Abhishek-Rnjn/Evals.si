"""tau2 reads its data directory at import: the tests need TAU2_DATA_DIR
(the data/ directory of a tau2-bench v0.2.0 checkout); CI fetches it."""

from __future__ import annotations

import os
from pathlib import Path

import pytest


def pytest_collection_modifyitems(config: pytest.Config, items: list[pytest.Item]) -> None:
    data = os.environ.get("TAU2_DATA_DIR", "")
    if data and (Path(data) / "tau2" / "domains" / "mock").is_dir():
        return
    skip = pytest.mark.skip(reason="set TAU2_DATA_DIR to tau2-bench's data directory")
    for item in items:
        item.add_marker(skip)
