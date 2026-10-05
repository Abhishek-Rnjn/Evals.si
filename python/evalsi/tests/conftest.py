from __future__ import annotations

from pathlib import Path
from typing import Any

import pytest

from evalsi import Content, Record, Usage


def make_record(
    output: Any = "answer",
    reference: Any = None,
    *,
    id: str = "r1",
    input: Any = "question",
    context: list[Any] | None = None,
    usage: Usage | None = None,
    metadata: dict[str, Any] | None = None,
) -> Record:
    return Record(
        id=id,
        input=None if input is None else Content.from_value(input),
        output=None if output is None else Content.from_value(output),
        reference=None if reference is None else Content.from_value(reference),
        context=[Content.from_value(c) for c in context or []],
        usage=usage,
        metadata=metadata or {},
    )


@pytest.fixture(autouse=True)
def _isolated_env(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    """Keep tests away from the developer's judge config and cache."""
    for var in (
        "EVALSI_JUDGE_PROVIDER",
        "EVALSI_JUDGE_MODEL",
        "EVALSI_JUDGE_BASE_URL",
        "EVALSI_JUDGE_API_KEY_ENV",
        "EVALSI_JUDGE_EFFORT",
    ):
        monkeypatch.delenv(var, raising=False)
    monkeypatch.setenv("EVALSI_CACHE_DIR", str(tmp_path / "cache"))
