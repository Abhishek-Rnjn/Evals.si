"""Finding checkpoints a trainer has written.

Checkpoints are directories named ``checkpoint-<step>`` (Hugging Face
Trainer, TRL), ``global_step<step>`` or ``global_step_<step>`` (verl,
DeepSpeed) or ``step-<step>`` / ``step_<step>``. A checkpoint is ready once
it holds a model (``config.json``, ``adapter_config.json`` or a
``*.safetensors`` file) and nothing in it has changed for ``settle_s``
seconds, so a half-written save is never evaluated.

Local directories work out of the box; ``s3://``, ``gs://``, ``hf://`` and
other remote locations work when ``fsspec`` (and the protocol's package) is
installed, and are downloaded into a local cache before serving.
"""

from __future__ import annotations

import re
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any

_STEP = re.compile(r"^(?:checkpoint-|global_step_?|step[-_])(\d+)$")
_MODEL_FILES = ("config.json", "adapter_config.json")


@dataclass(frozen=True)
class Checkpoint:
    step: int
    # Local path or remote URL.
    location: str


def step_of(name: str) -> int | None:
    m = _STEP.match(name)
    return int(m.group(1)) if m else None


def _is_remote(location: str) -> bool:
    return "://" in location and not location.startswith("file://")


def find(location: str | Path, *, settle_s: float = 30.0) -> list[Checkpoint]:
    """Checkpoints under ``location`` that are ready, by step."""
    location = str(location)
    if _is_remote(location):
        return _find_remote(location, settle_s)
    root = Path(location.removeprefix("file://"))
    out: list[Checkpoint] = []
    if not root.is_dir():
        return out
    now = time.time()
    for child in root.iterdir():
        step = step_of(child.name)
        if step is None or not child.is_dir():
            continue
        files = [p for p in child.rglob("*") if p.is_file()]
        has_model = any(p.name in _MODEL_FILES or p.suffix == ".safetensors" for p in files)
        newest = max((p.stat().st_mtime for p in files), default=now)
        if has_model and now - newest >= settle_s:
            out.append(Checkpoint(step, str(child)))
    return sorted(out, key=lambda c: c.step)


def _fs(location: str) -> Any:
    try:
        import fsspec
    except ImportError as exc:
        raise RuntimeError(
            f"watching {location} needs fsspec (pip install fsspec and the protocol's package)"
        ) from exc
    fs, _ = fsspec.core.url_to_fs(location)
    return fs


def _find_remote(location: str, settle_s: float) -> list[Checkpoint]:
    fs = _fs(location)
    protocol = location.split("://", 1)[0]
    out: list[Checkpoint] = []
    now = time.time()
    for entry in fs.ls(location, detail=True):
        name = str(entry["name"]).rstrip("/").rsplit("/", 1)[-1]
        step = step_of(name)
        if step is None or entry.get("type") != "directory":
            continue
        files = fs.find(entry["name"], detail=True)
        names = [str(k).rsplit("/", 1)[-1] for k in files]
        if not any(n in _MODEL_FILES or n.endswith(".safetensors") for n in names):
            continue
        times = [_mtime(v) for v in files.values()]
        known = [t for t in times if t is not None]
        if known and now - max(known) < settle_s:
            continue
        out.append(Checkpoint(step, f"{protocol}://{str(entry['name']).rstrip('/')}"))
    return sorted(out, key=lambda c: c.step)


def _mtime(info: dict[str, Any]) -> float | None:
    for key in ("mtime", "LastModified", "last_modified", "updated"):
        value = info.get(key)
        if isinstance(value, int | float):
            return float(value)
        timestamp = getattr(value, "timestamp", None)
        if callable(timestamp):
            return float(timestamp())
    return None


def localize(checkpoint: Checkpoint, cache_dir: str | Path) -> str:
    """A local path for the checkpoint, downloading a remote one into ``cache_dir``."""
    if not _is_remote(checkpoint.location):
        return checkpoint.location.removeprefix("file://")
    target = Path(cache_dir) / f"step-{checkpoint.step}"
    if not (target / ".complete").exists():
        target.mkdir(parents=True, exist_ok=True)
        _fs(checkpoint.location).get(
            checkpoint.location.rstrip("/") + "/", str(target), recursive=True
        )
        (target / ".complete").write_text("")
    return str(target)
