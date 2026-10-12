"""Finding ``evalsid``.

Platform wheels carry the ``evalsid`` server (and the static bubblewrap beside
it, which ``evalsid`` prefers over the host's) in ``evalsi/_bin``, the same
release as this package. The pure-Python wheel and the sdist carry none.

Lookup order, first match wins:

1. ``EVALSID``, the path you set: always honoured, so a development build or a
   different release can be used on purpose;
2. the binary bundled in this install, which matches this package's version;
3. ``evalsid`` on ``PATH``.
"""

from __future__ import annotations

import os
import shutil
from pathlib import Path

BIN_DIR = Path(__file__).parent / "_bin"


def bundled_evalsid() -> str | None:
    """The ``evalsid`` shipped in this install, or ``None`` (a pure-Python install)."""
    path = BIN_DIR / "evalsid"
    return str(path) if path.is_file() and os.access(path, os.X_OK) else None


def evalsid_path() -> str | None:
    """The ``evalsid`` to run, or ``None`` when there is none."""
    return os.environ.get("EVALSID") or bundled_evalsid() or shutil.which("evalsid")
