"""Wasm evaluator plugins, run sandboxed by evalsid.

A Wasm plugin is a directory with ``evalsi-plugin.yaml`` and the module it
pins by sha256 (see the plugins guide). Plugins are found under each
directory of ``EVALSI_PLUGIN_PATH`` (``os.pathsep``-separated; empty turns
discovery off), or by default under ``~/.evalsi/plugins``, where
``evalsi plugins install`` puts them.

Discovery reads only the manifests. Scoring goes through one long-lived
``evalsid wasm serve`` process per Python process (found through
``EVALSID`` or ``PATH``), which verifies and compiles each module on first
use; Wasm evaluators therefore score the same here as on a server.
"""

from __future__ import annotations

import asyncio
import inspect
import json
import os
import shutil
import subprocess
import threading
from concurrent.futures import Future
from pathlib import Path
from typing import IO, Any

from google.protobuf import json_format

from evalsi.evaluator import (
    EvaluatorConfigError,
    EvaluatorDef,
    EvaluatorSpec,
    MetricSpec,
    Requirements,
    Scope,
    ScoreType,
    SkipRecord,
)
from evalsi.types import Record, Score

MANIFEST = "evalsi-plugin.yaml"


class WasmError(RuntimeError):
    """A Wasm evaluator failed (the module crashed, timed out or answered badly)."""


def plugin_dirs() -> list[Path]:
    env = os.environ.get("EVALSI_PLUGIN_PATH")
    if env is not None:
        return [Path(p).expanduser() for p in env.split(os.pathsep) if p]
    return [default_plugin_dir()]


def default_plugin_dir() -> Path:
    return Path(os.environ.get("EVALSI_HOME", "~/.evalsi")).expanduser() / "plugins"


def find_manifests(dirs: list[Path] | None = None) -> list[Path]:
    out: list[Path] = []
    for d in plugin_dirs() if dirs is None else dirs:
        if d.is_dir():
            out.extend(sorted(d.rglob(MANIFEST)))
    return out


def read_manifest(path: Path) -> dict[str, Any]:
    import yaml

    doc = yaml.safe_load(path.read_text())
    if not isinstance(doc, dict):
        raise EvaluatorConfigError(f"{path}: expected a mapping")
    return doc


def _params(schema: dict[str, Any] | None) -> dict[str, Any]:
    props = (schema or {}).get("properties") or {}
    required = set((schema or {}).get("required") or [])
    return {
        name: inspect.Parameter.empty if name in required else (prop or {}).get("default")
        for name, prop in props.items()
    }


def _spec(doc: dict[str, Any], plugin: dict[str, Any]) -> EvaluatorSpec:
    req = doc.get("requires") or {}
    outputs = tuple(
        MetricSpec(
            name=o["name"],
            type=ScoreType(str(o.get("type", "number")).lower()),
            min=o.get("min"),
            max=o.get("max"),
            higher_is_better=o.get("higher_is_better"),
            description=o.get("description", ""),
        )
        for o in doc.get("outputs") or []
    )
    return EvaluatorSpec(
        name=doc["name"],
        version=str(doc.get("version") or plugin["version"]),
        description=doc.get("description", ""),
        scope=Scope(str(doc.get("scope", "record")).lower()),
        requires=Requirements(
            input=bool(req.get("input")),
            output=bool(req.get("output")),
            reference=bool(req.get("reference")),
            context=bool(req.get("context")),
            trajectory=bool(req.get("trajectory")),
        ),
        outputs=outputs,
        params=_params(doc.get("params_schema")),
        pack=plugin["name"],
        tier=plugin.get("tier") or "community",
        runtime="wasm",
    )


def load_plugin(path: Path) -> Any:
    """The plugin at ``path`` (its manifest) as a Pack of evaluators."""
    from evalsi.registry import Pack

    doc = read_manifest(path)
    defs = []
    for ev in doc.get("evaluators") or []:
        spec = _spec(ev, doc)
        defs.append(EvaluatorDef(spec=spec, fn=_caller(path.resolve(), spec), wants_ctx=False))
    return Pack(
        name=doc["name"],
        description=doc.get("description", ""),
        evaluators=defs,
        tier=doc.get("tier") or "community",
    )


def discover_wasm_packs() -> list[Any]:
    import logging

    packs = []
    for path in find_manifests():
        try:
            packs.append(load_plugin(path))
        except Exception:
            logging.getLogger(__name__).exception("could not read Wasm plugin %s", path)
    return packs


def _caller(manifest: Path, spec: EvaluatorSpec) -> Any:
    from evalsi.convert import record_to_proto, score_from_proto

    def to_json(r: Record) -> dict[str, Any]:
        out: dict[str, Any] = json_format.MessageToDict(record_to_proto(r))
        return out

    def scores(raw: list[dict[str, Any]]) -> list[Score]:
        from evalsi.v1alpha1 import score_pb2

        return [score_from_proto(json_format.ParseDict(s, score_pb2.Score())) for s in raw]

    if spec.scope == Scope.DATASET:

        async def reduce(records: list[Record], **params: Any) -> list[Score]:
            body = {
                "evaluator": spec.name,
                "params": params,
                "records": [to_json(r) for r in records],
            }
            resp = await asyncio.to_thread(service().call, manifest, "reduce", body)
            return scores(resp.get("scores", []))

        return reduce

    async def evaluate(record: Record, **params: Any) -> list[Score]:
        body = {"evaluator": spec.name, "params": params, "records": [to_json(record)]}
        resp = await asyncio.to_thread(service().call, manifest, "evaluate", body)
        result = (resp.get("results") or [{}])[0]
        outcome = result.get("outcome", "OUTCOME_SCORED")
        if outcome == "OUTCOME_SKIPPED":
            raise SkipRecord(result.get("reason", ""))
        if outcome == "OUTCOME_ERROR":
            raise WasmError(f"{spec.name}: {result.get('reason', '')}")
        return scores(result.get("scores", []))

    return evaluate


class Service:
    """A client for ``evalsid wasm serve``: concurrent calls over one process."""

    def __init__(self, command: list[str]) -> None:
        self._proc = subprocess.Popen(
            command,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
        )
        self._lock = threading.Lock()
        self._next = 0
        self._pending: dict[int, Future[dict[str, Any]]] = {}
        self._reader = threading.Thread(target=self._read, daemon=True)
        self._reader.start()

    def _read(self) -> None:
        out: IO[str] = self._proc.stdout  # type: ignore[assignment]
        for line in out:
            msg = json.loads(line)
            fut = self._pending.pop(msg.get("id", -1), None)
            if fut is None:
                continue
            if msg.get("error"):
                fut.set_exception(WasmError(msg["error"]))
            else:
                fut.set_result(msg.get("response") or {})
        err = self._proc.stderr.read() if self._proc.stderr else ""
        for fut in list(self._pending.values()):
            fut.set_exception(WasmError(f"evalsid wasm serve exited: {err.strip()}"))
        self._pending.clear()

    def call(
        self, manifest: Path, verb: str, request: dict[str, Any] | None = None
    ) -> dict[str, Any]:
        fut: Future[dict[str, Any]] = Future()
        with self._lock:
            if self._proc.poll() is not None:
                raise WasmError("evalsid wasm serve is not running")
            self._next += 1
            self._pending[self._next] = fut
            line = {"id": self._next, "manifest": str(manifest), "verb": verb}
            if request is not None:
                line["request"] = request
            stdin: IO[str] = self._proc.stdin  # type: ignore[assignment]
            stdin.write(json.dumps(line) + "\n")
            stdin.flush()
        return fut.result()

    def close(self) -> None:
        if self._proc.stdin:
            self._proc.stdin.close()
        self._proc.wait(timeout=10)


_service: Service | None = None
_service_lock = threading.Lock()


def service() -> Service:
    global _service  # noqa: PLW0603 - one evalsid per process
    with _service_lock:
        if _service is None or _service._proc.poll() is not None:
            binary = os.environ.get("EVALSID") or shutil.which("evalsid")
            if not binary:
                raise WasmError("Wasm evaluators need evalsid: put it on PATH, or set EVALSID")
            _service = Service([binary, "wasm", "serve"])
        return _service
