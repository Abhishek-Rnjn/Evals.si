"""On-disk cache of judge responses, keyed by a content hash of everything that
determines the answer (judge config, prompt, schema)."""

from __future__ import annotations

import json
import os
import sqlite3
from pathlib import Path
from typing import Any


def default_cache_path() -> Path:
    root = os.environ.get("EVALSI_CACHE_DIR") or os.path.join(
        os.environ.get("XDG_CACHE_HOME") or os.path.expanduser("~/.cache"), "evalsi"
    )
    return Path(root) / "judge-cache.sqlite3"


class JudgeCache:
    def __init__(self, path: str | Path | None = None) -> None:
        self.path = Path(path) if path is not None else default_cache_path()
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._db = sqlite3.connect(self.path, check_same_thread=False)
        self._db.execute(
            "CREATE TABLE IF NOT EXISTS judge_cache (key TEXT PRIMARY KEY, value TEXT NOT NULL)"
        )
        self._db.commit()

    def get(self, key: str) -> dict[str, Any] | None:
        row = self._db.execute("SELECT value FROM judge_cache WHERE key = ?", (key,)).fetchone()
        if row is None:
            return None
        value: dict[str, Any] = json.loads(row[0])
        return value

    def put(self, key: str, value: dict[str, Any]) -> None:
        self._db.execute(
            "INSERT OR REPLACE INTO judge_cache (key, value) VALUES (?, ?)",
            (key, json.dumps(value, ensure_ascii=False)),
        )
        self._db.commit()

    def close(self) -> None:
        self._db.close()
