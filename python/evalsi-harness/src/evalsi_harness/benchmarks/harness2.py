"""Harness² runs as Evals.si records and results (H7 run importer).

``import_namespace(<namespace dir>)`` reads one Harness² namespace directory
(``<runs>/<bench>/<subset>/<bench>-<substrate>-<mode><k>[...]``: per-task
``summary.json``, ``judged.json``, ``base_judged.json`` and the
``<task>/harnesses/<label>/`` trees) and gives one results structure per harness:

``base``
    The reference executions (``base1`` ... ``baseK``), one record per task, the
    label's number as the trial.
``<mode><k>`` (for example ``agg4``)
    The run Harness² chose for each task under that mode and k, one record per
    task, trial 0, so ``evalsi matrix`` pairs it with ``base`` by task.

The structures have the shape of ``evalsi run --output`` (``manifest``,
``records``, ``results``, ``summaries``), so ``evalsi.matrix`` and
``evalsi.compare`` read them as they read any results file; ``harness_of`` names
the harness ``harness2:base`` or ``harness2:<mode>-k<k>``. The metrics are
``h2-criteria-fraction`` (passed criteria over criteria) and ``h2-all-pass``.

A run the judge did not grade is never scored as zero. It is SKIPPED when it has
no verdict to read (no judged row, or the selection did not complete) and an
ERROR when the judge recorded a failure. Cost is in ``metadata.harness2.usd``
only: it is Harness²'s selection cost for that task, not the cost of the
rollout, so it is kept out of ``usage`` where the matrix would show it as the
cost of the run. The records carry no trajectory: the run directories hold
benchmark-specific trajectories whose format is not pinned yet, so the
trajectory evaluators skip these records.

The layout was read from the Harness² source (``core/schedule.py``,
``core/judge.py``, ``core/selection.py``, ``cli/judge.py``), not from a recorded
run, and the fixtures are written from that source. Reading is tolerant (see
``evalsi.harness2_calibration.read_namespace``): a missing key is a missing
value, and the legacy ``arm``/``arms`` keys are accepted. The importer only
reads. Symlinks that leave the namespace root are refused, and everything it
reads is untrusted text: nothing is executed, and the ``run_dir`` paths recorded
on the original machine are never opened.
"""

from __future__ import annotations

import hashlib
import json
import os
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from evalsi.harness2_calibration import (
    JudgedRow,
    NamespaceData,
    SelectionRow,
    UnsafePathError,
    read_namespace,
    safe_child,
)
from evalsi.stats import interval
from evalsi.types import Outcome, Record

FRACTION = "h2-criteria-fraction"
ALL_PASS = "h2-all-pass"
METRICS = (FRACTION, ALL_PASS)

# Limits on a harness tree read for its fingerprint.
MAX_TREE_FILES = 2000
MAX_TREE_FILE_BYTES = 4 * 1024 * 1024
MAX_TREE_BYTES = 64 * 1024 * 1024

_LABEL = re.compile(r"[A-Za-z0-9_][A-Za-z0-9_.\-]*")

# The eight components, by where Harness² keeps them in an OpenCode harness tree.
_COMPONENTS = (
    ("systemprompt.md", "system_prompt"),
    ("guardrails.md", "guardrails"),
    ("LongTermMEMORY.md", "memory"),
    ("tool_descriptions/", "tool_guidance"),
    ("skills/", "skills"),
    ("scripts/", "scripts"),
    ("sub_agents/", "subagents"),
    ("plugin/", "plugins"),
)


def component_of(relpath: str) -> str | None:
    for prefix, name in _COMPONENTS:
        if relpath == prefix or (prefix.endswith("/") and relpath.startswith(prefix)):
            return name
    return None


@dataclass
class TreeInfo:
    path: str  # relative to the namespace root
    sha256: str | None  # None when the tree could not be read completely
    components: list[str]
    files: int


@dataclass
class Execution:
    """One (task, label) execution and its verdict."""

    task_id: str
    kind: str  # "base" or "chosen"
    label: str
    mode: str | None
    k: int | None
    run_id: str
    chosen_is_base: bool | None
    n_passed: int | None
    n_criteria: int | None
    outcome: Outcome
    reason: str
    usd: float | None = None
    harness_fingerprint: str = ""
    run_dir: str = ""
    tree: TreeInfo | None = None


@dataclass
class Harness2Import:
    namespace: str
    root: Path
    executions: list[Execution] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)

    def cell_names(self) -> list[str]:
        return list(self._groups())

    def _groups(self) -> dict[str, list[Execution]]:
        groups: dict[str, list[Execution]] = {}
        for e in self.executions:
            groups.setdefault(cell_name(e), []).append(e)
        # The base first, as the baseline of a matrix; then by mode and k.
        return dict(sorted(groups.items(), key=lambda kv: (kv[0] != "base", kv[0])))

    def cells(self) -> dict[str, dict[str, Any]]:
        """``{cell name: results structure}``, ``base`` first."""
        dataset = hashlib.sha256(
            "\n".join(sorted({e.task_id for e in self.executions})).encode()
        ).hexdigest()
        return {
            name: results_for(name, rows, self.namespace, dataset, self.notes)
            for name, rows in self._groups().items()
        }


def cell_name(e: Execution) -> str:
    """``base``, or ``<mode><k>``; also a file name, so only safe characters survive."""
    if e.kind == "base":
        return "base"
    raw = f"{e.mode or 'unknown'}{e.k if e.k is not None else ''}"
    return re.sub(r"[^A-Za-z0-9_.\-]", "_", raw).lstrip(".") or "unknown"


def _profile(e: Execution) -> str:
    if e.kind == "base":
        return "harness2:base"
    return f"harness2:{e.mode or 'unknown'}" + (f"-k{e.k}" if e.k is not None else "")


# --- harness trees ---


def read_tree(ns_root: Path, tree_dir: Path, notes: list[str]) -> TreeInfo | None:
    """Fingerprint a harness tree: a SHA-256 over its sorted ``path, content`` pairs.

    ``None`` when the directory is absent. The hash is None (with a note) when
    anything was refused or over a limit, so a partial tree never gets a
    fingerprint that looks complete. Symlinked directories are not followed; a
    symlinked file is read only if it stays inside the namespace root.
    """
    rel_tree = os.path.relpath(tree_dir, ns_root)
    try:
        top = safe_child(ns_root, tree_dir)
    except UnsafePathError as exc:
        notes.append(f"{rel_tree}: refused: {exc}")
        return None
    if not top.is_dir():
        return None
    complete = True
    files: dict[str, bytes] = {}
    total = 0
    for dirpath, dirnames, filenames in os.walk(top, followlinks=False):
        base = Path(dirpath)
        for d in sorted(dirnames):
            if (base / d).is_symlink():
                notes.append(f"{rel_tree}: symlinked directory {d} not followed")
                complete = False
        dirnames[:] = sorted(d for d in dirnames if not (base / d).is_symlink())
        for name in sorted(filenames):
            path = base / name
            rel = path.relative_to(top).as_posix()
            try:
                real = safe_child(ns_root, path)
                if not real.is_file():
                    raise OSError("not a regular file")
                size = real.stat().st_size
                if len(files) >= MAX_TREE_FILES or size > MAX_TREE_FILE_BYTES:
                    raise OSError("over the size limit")
                if total + size > MAX_TREE_BYTES:
                    raise OSError("over the size limit")
                files[rel] = real.read_bytes()
                total += size
            except (UnsafePathError, OSError) as exc:
                notes.append(f"{rel_tree}/{rel}: not read: {exc}")
                complete = False
    digest = hashlib.sha256()
    for rel in sorted(files):
        digest.update(rel.encode() + b"\0" + hashlib.sha256(files[rel]).digest())
    comps = sorted({c for rel in files if (c := component_of(rel))})
    return TreeInfo(
        path=rel_tree,
        sha256=digest.hexdigest() if complete else None,
        components=comps,
        files=len(files),
    )


# --- executions ---


def _verdict(
    n_passed: int | None, n_criteria: int | None, error: str, judged: bool
) -> tuple[Outcome, str]:
    """SCORED when graded; else SKIPPED (nothing to read) or ERROR (the judge failed)."""
    if n_criteria is not None and n_criteria > 0 and n_passed is not None:
        if 0 <= n_passed <= n_criteria:
            return Outcome.SCORED, ""
        return Outcome.ERROR, f"inconsistent judge score {n_passed}/{n_criteria}"
    if not judged:
        return Outcome.SKIPPED, "not judged"
    if error.startswith("selection"):
        return Outcome.SKIPPED, error
    if error:
        return Outcome.ERROR, f"judge failed: {error}"
    return Outcome.SKIPPED, "the judge did not grade this run"


def _trial_of(label: str, used: set[int]) -> int:
    m = re.search(r"(\d+)$", label)
    trial = max(int(m.group(1)) - 1, 0) if m else 0
    while trial in used:
        trial += 1
    used.add(trial)
    return trial


def import_namespace(ns_dir: str | Path) -> Harness2Import:
    """Read a namespace directory into executions. Raises FileNotFoundError if
    ``ns_dir`` is not a directory; an empty one gives no executions and a note."""
    ns = read_namespace(ns_dir)
    imp = Harness2Import(namespace=ns.root.name, root=ns.root, notes=list(ns.notes))
    imp.executions += _base_executions(ns)
    imp.executions += _chosen_executions(ns, imp.notes)
    return imp


def _base_executions(ns: NamespaceData) -> list[Execution]:
    out = []
    for b in ns.base:
        outcome, reason = _verdict(b.n_passed, b.n_criteria, "", b.judged)
        out.append(
            Execution(
                task_id=b.task_id,
                kind="base",
                label=b.label,
                mode=None,
                k=None,
                run_id=b.run_id,
                chosen_is_base=None,
                n_passed=b.n_passed,
                n_criteria=b.n_criteria,
                outcome=outcome,
                reason=reason,
                run_dir=b.run_dir,
            )
        )
    return out


def _chosen_executions(ns: NamespaceData, notes: list[str]) -> list[Execution]:
    sels: dict[tuple[str, str, int | None], SelectionRow] = {
        (s.task_id, s.mode, s.k): s for s in ns.selections
    }
    judged: dict[tuple[str, str, int | None], JudgedRow] = {
        (j.task_id, j.mode, j.k): j for j in ns.judged
    }
    out = []
    for key in sorted(set(sels) | set(judged), key=lambda x: (x[1], x[2] or 0, x[0])):
        task_id, mode, k = key
        sel, row = sels.get(key), judged.get(key)
        label = (row.label if row and row.label else sel.chosen_label if sel else "") or ""
        outcome, reason = _verdict(
            row.n_passed if row else None,
            row.n_criteria if row else None,
            (row.error if row else "") or (sel.error if sel and sel.ok is False else ""),
            row is not None,
        )
        if row is None and sel is not None and sel.ok is False:
            outcome, reason = Outcome.SKIPPED, "selection did not complete"
        tree = None
        dirname = ns.task_dirs.get(task_id)
        if dirname and _LABEL.fullmatch(label):
            tree = read_tree(ns.root, ns.root / dirname / "harnesses" / label, notes)
        out.append(
            Execution(
                task_id=task_id,
                kind="chosen",
                label=label,
                mode=mode,
                k=k,
                run_id=(row.run_id if row and row.run_id else sel.chosen_run_id if sel else ""),
                chosen_is_base=row.chosen_is_base
                if row and row.chosen_is_base is not None
                else (sel.chosen_is_base if sel else None),
                n_passed=row.n_passed if row else None,
                n_criteria=row.n_criteria if row else None,
                outcome=outcome,
                reason=reason,
                usd=sel.usd if sel else None,
                harness_fingerprint=sel.harness_fingerprint if sel else "",
                tree=tree,
            )
        )
    return out


# --- results structures ---


def _record(e: Execution) -> Record:
    h2: dict[str, Any] = {
        "task_id": e.task_id,
        "label": e.label,
        "kind": e.kind,
        "run_id": e.run_id,
    }
    if e.kind == "chosen":
        h2.update(
            mode=e.mode,
            k=e.k,
            chosen_is_base=e.chosen_is_base,
            usd=e.usd,
            harness_fingerprint=e.harness_fingerprint,
        )
    else:
        h2["run_dir"] = e.run_dir
    if e.tree is not None:
        h2["harness_path"] = e.tree.path
        h2["tree_sha256"] = e.tree.sha256
        h2["components"] = e.tree.components
    return Record(
        id=e.task_id,
        metadata={"agent": {"profile": _profile(e)}, "harness2": h2},
    )


def results_for(
    name: str,
    rows: list[Execution],
    namespace: str,
    dataset_sha256: str,
    notes: list[str] | None = None,
) -> dict[str, Any]:
    """One results structure for the executions of one harness."""
    records: list[dict[str, Any]] = []
    results: list[dict[str, Any]] = []
    values: dict[str, list[float]] = {m: [] for m in METRICS}
    counts = {m: {"skipped": 0, "errors": 0} for m in METRICS}
    used: set[tuple[str, int]] = set()
    for e in sorted(rows, key=lambda r: (r.task_id, r.label)):
        trial = 0
        if e.kind == "base":
            taken = {t for (tid, t) in used if tid == e.task_id}
            trial = _trial_of(e.label, taken)
            used.add((e.task_id, trial))
        rec = _record(e).to_dict()
        records.append(rec)
        for metric in METRICS:
            res: dict[str, Any] = {
                "record_id": e.task_id,
                "evaluator": metric,
                "evaluator_ref": f"harness2/{metric}@1",
                "outcome": e.outcome.value,
                "trial": trial,
            }
            if e.outcome is Outcome.SCORED:
                assert e.n_passed is not None and e.n_criteria is not None
                if metric == FRACTION:
                    value = e.n_passed / e.n_criteria
                    res["scores"] = [{"name": metric, "number": value}]
                else:
                    value = 1.0 if e.n_passed == e.n_criteria else 0.0
                    res["scores"] = [{"name": metric, "passed": value == 1.0}]
                values[metric].append(value)
            else:
                res["reason"] = e.reason
                counts[metric]["skipped" if e.outcome is Outcome.SKIPPED else "errors"] += 1
            results.append(res)
    summaries = [_summary(m, values[m], counts[m]) for m in METRICS]
    return {
        "manifest": {
            "name": f"harness2:{namespace}:{name}",
            "dataset": {"sha256": dataset_sha256},
            "source": {"importer": "harness2", "namespace": namespace, "cell": name},
        },
        "summaries": summaries,
        "results": results,
        "records": records,
        "notes": list(notes or []),
    }


def _summary(metric: str, vals: list[float], count: dict[str, int]) -> dict[str, Any]:
    n = len(vals)
    mean = sum(vals) / n if n else None
    std = None
    if n > 1 and mean is not None:
        std = (sum((v - mean) ** 2 for v in vals) / (n - 1)) ** 0.5
    out: dict[str, Any] = {
        "metric": metric,
        "evaluator": metric,
        "kind": "proportion" if metric == ALL_PASS else "number",
        "n": n,
        "mean": mean,
        "std": std,
        "skipped": count["skipped"],
        "errors": count["errors"],
        "higher_is_better": True,
    }
    ci = interval(vals, proportion=metric == ALL_PASS)
    if ci is not None:
        if metric == FRACTION:
            ci = ci.clipped(0.0, 1.0)
        out["ci"] = {"low": ci.low, "high": ci.high, "level": ci.level, "method": ci.method}
    return out


def write_cells(imp: Harness2Import, out_dir: str | Path) -> list[Path]:
    """Write each cell as ``<out_dir>/<name>.json``, for ``evalsi matrix``.

    Refuses an ``out_dir`` inside the Harness² namespace directory: the importer
    does not write there.
    """
    out = Path(out_dir).resolve()
    if out == imp.root or out.is_relative_to(imp.root):
        raise ValueError(f"refusing to write inside the Harness² directory {imp.root}")
    out.mkdir(parents=True, exist_ok=True)
    paths = []
    for name, data in imp.cells().items():
        path = out / f"{name}.json"
        path.write_text(json.dumps(data, indent=2) + "\n", encoding="utf-8")
        paths.append(path)
    return paths


__all__ = [
    "ALL_PASS",
    "FRACTION",
    "Execution",
    "Harness2Import",
    "TreeInfo",
    "cell_name",
    "component_of",
    "import_namespace",
    "read_tree",
    "results_for",
    "write_cells",
]
