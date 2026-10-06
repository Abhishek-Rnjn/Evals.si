"""Finding checkpoints a trainer has written.

Checkpoints are directories named ``checkpoint-<step>`` (Hugging Face
Trainer, TRL), ``global_step<step>`` or ``global_step_<step>`` (verl,
DeepSpeed) or ``step-<step>`` / ``step_<step>``. A checkpoint is ready once
it holds a model (``config.json``, ``adapter_config.json`` or a
``*.safetensors`` file) and nothing in it has changed for ``settle_s``
seconds, so a half-written save is never evaluated.

Local directories work out of the box; ``s3://``, ``gs://``, ``hf://`` and
other remote locations work when ``fsspec`` (and the protocol's package) is
installed, and are downloaded into a local cache before serving. Versions
of a model in the MLflow Model Registry work too (:func:`find_mlflow`).
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


# --- MLflow Model Registry ---------------------------------------------------

MLFLOW_PREFIX = "mlflow:"


def _mlflow_client(tracking_uri: str) -> Any:
    import os

    import httpx

    headers = {}
    if token := os.environ.get("MLFLOW_TRACKING_TOKEN"):
        headers["Authorization"] = f"Bearer {token}"
    return httpx.Client(base_url=tracking_uri.rstrip("/"), headers=headers, timeout=60)


def find_mlflow(model: str, tracking_uri: str, *, step_tag: str = "step") -> list[Checkpoint]:
    """Versions of a registered model as checkpoints: the step is the
    version's ``step_tag`` tag, else its version number, and the location
    its artifact URI (``mlflow:<name>/<version>`` when the registry serves
    the artifacts itself)."""
    with _mlflow_client(tracking_uri) as client:
        response = client.get(
            "/api/2.0/mlflow/model-versions/search",
            params={"filter": f"name='{model}'", "max_results": 1000},
        )
        response.raise_for_status()
        out = []
        for v in response.json().get("model_versions", []):
            if v.get("status", "READY") != "READY":
                continue
            tags = {t["key"]: t.get("value", "") for t in v.get("tags", [])}
            step = tags.get(step_tag, "")
            step_n = int(step) if step.isdigit() else int(v["version"])
            uri = client.get(
                "/api/2.0/mlflow/model-versions/get-download-uri",
                params={"name": model, "version": v["version"]},
            )
            uri.raise_for_status()
            location = str(uri.json().get("artifact_uri") or v.get("source", ""))
            if location.startswith("mlflow-artifacts:"):
                location = (
                    f"{MLFLOW_PREFIX}{location.removeprefix('mlflow-artifacts:').lstrip('/')}"
                )
            out.append(Checkpoint(step_n, location))
    return sorted(out, key=lambda c: c.step)


def localize_mlflow(checkpoint: Checkpoint, tracking_uri: str, cache_dir: str | Path) -> str:
    """Download an artifact the MLflow server proxies (mlflow-artifacts)."""
    path = checkpoint.location.removeprefix(MLFLOW_PREFIX)
    target = Path(cache_dir) / f"step-{checkpoint.step}"
    if (target / ".complete").exists():
        return str(target)
    with _mlflow_client(tracking_uri) as client:

        def fetch(rel: str) -> None:
            listing = client.get("/api/2.0/mlflow-artifacts/artifacts", params={"path": rel})
            listing.raise_for_status()
            for f in listing.json().get("files", []):
                child = f"{rel}/{f['path']}" if rel else f["path"]
                if f.get("is_dir"):
                    fetch(child)
                    continue
                data = client.get(f"/api/2.0/mlflow-artifacts/artifacts/{child}")
                data.raise_for_status()
                dest = target / child.removeprefix(path).lstrip("/")
                dest.parent.mkdir(parents=True, exist_ok=True)
                dest.write_bytes(data.content)

        fetch(path)
    target.mkdir(parents=True, exist_ok=True)
    (target / ".complete").write_text("")
    return str(target)
