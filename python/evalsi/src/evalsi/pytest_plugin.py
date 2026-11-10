"""pytest integration for :func:`evalsi.testing.assert_eval`.

Loaded by pytest through the ``pytest11`` entry point; it does nothing until
asked. ``pytest --evalsi-results DIR`` writes the results of every
``assert_eval`` call to ``DIR/<test id>.json`` (the format of ``evalsi eval
--output``), so a failing test can be read with ``evalsi report`` and two
commits compared with ``evalsi compare``.
"""

from __future__ import annotations

import re
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import pytest


def pytest_addoption(parser: Any) -> None:
    parser.getgroup("evalsi").addoption(
        "--evalsi-results",
        metavar="DIR",
        default=None,
        help="write each assert_eval call's results to DIR/<test id>.json",
    )


@pytest.hookimpl(hookwrapper=True)
def pytest_runtest_call(item: Any) -> Iterator[None]:
    directory = item.config.getoption("--evalsi-results", default=None)
    if not directory:
        yield
        return
    from evalsi import testing

    name = re.sub(r"[^A-Za-z0-9_.-]+", "_", item.nodeid).strip("_") or "test"
    calls = 0

    def sink(result: Any) -> None:
        nonlocal calls
        calls += 1
        suffix = "" if calls == 1 else f".{calls}"
        out = Path(directory)
        out.mkdir(parents=True, exist_ok=True)
        result.save(out / f"{name}{suffix}.json")

    previous, testing._results_sink = testing._results_sink, sink
    try:
        yield
    finally:
        testing._results_sink = previous
