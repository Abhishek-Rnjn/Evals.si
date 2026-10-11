"""PyTorchCheckpointHook, against a stand-in ``torch`` (the real one is in tests/training_hooks)."""

from __future__ import annotations

import os
import stat
import sys
import types
from pathlib import Path
from typing import Any

import pytest

from evalsi.training import PyTorchCheckpointHook
from evalsi.training.loop import StepResult


class Evaluator:
    def __init__(self) -> None:
        self.steps: list[int] = []

    def evaluate(self, checkpoint: str, step: int) -> StepResult:
        self.steps.append(step)
        return StepResult("cpu", str(step), checkpoint)


class Model:
    def state_dict(self) -> dict[str, Any]:
        return {"w": 1}


def fake_torch(monkeypatch: pytest.MonkeyPatch, rank: int | None) -> None:
    torch = types.ModuleType("torch")
    torch.save = lambda state, file: file.write(b"checkpoint")  # type: ignore[attr-defined]
    torch.distributed = types.SimpleNamespace(  # type: ignore[attr-defined]
        is_available=lambda: rank is not None,
        is_initialized=lambda: rank is not None,
        get_rank=lambda: rank,
    )
    monkeypatch.setitem(sys.modules, "torch", torch)


def test_checkpoint_is_readable_like_any_file_the_trainer_writes(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake_torch(monkeypatch, None)
    umask = os.umask(0o022)
    try:
        evaluator = Evaluator()
        with PyTorchCheckpointHook(evaluator, wait=True) as hook:
            hook.save(Model(), tmp_path / "step-1.pt", 1)
    finally:
        os.umask(umask)
    mode = stat.S_IMODE((tmp_path / "step-1.pt").stat().st_mode)
    # Not owner-only: a separate evaluation pod under another user can read it.
    assert mode == 0o644
    assert evaluator.steps == [1]
    assert [item.name for item in tmp_path.iterdir()] == ["step-1.pt"]


def test_only_rank_zero_saves_and_evaluates(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    fake_torch(monkeypatch, 1)
    evaluator = Evaluator()
    with PyTorchCheckpointHook(evaluator, wait=True) as hook:
        hook.save(Model(), tmp_path / "step-1.pt", 1)
    assert not list(tmp_path.iterdir())
    assert evaluator.steps == []

    fake_torch(monkeypatch, 0)
    with PyTorchCheckpointHook(evaluator, wait=True) as hook:
        hook.save(Model(), tmp_path / "step-1.pt", 1)
    assert (tmp_path / "step-1.pt").exists()
    assert evaluator.steps == [1]
