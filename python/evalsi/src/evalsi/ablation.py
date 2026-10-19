"""Component ablation of an adopted harness (H7).

A refinement tool such as Harness² edits a harness made of eight components.
Given the base harness and an adopted one, this module plans one *revert
variant* per edited component (the base's content for that component, the
adopted content for everything else), and, once each variant has been run,
attributes the gain per component: the effect of a component is the full
adopted harness minus its revert variant, with the interval of the paired
difference (``evalsi.compare``). An edit whose interval includes zero is
reported as having no measurable effect. An edit that came along with the
others but does nothing shows an effect of exactly zero.

Everything here is offline and pure apart from reading and writing the harness
directories. Nothing is run.

The component map is the OpenCode surface of Harness²
(``harness2/substrate/opencode.py`` ``OPENCODE_SURFACE`` and ``harness2/core/tree.py``,
``is_allowed``), read from source and not run:

- c1 ``systemprompt.md``, c2 ``guardrails.md``, c3 ``LongTermMEMORY.md`` and
  c4 ``tool_descriptions/tool_guidance.md`` are the always-on files;
- c5 ``sub_agents/<name>/AGENT.md`` (the stub says "c5: delegated subagents");
- c6 ``skills/<name>/SKILL.md`` and ``skills/<name>/scripts/*`` (the stub says
  "c6: reusable workflows");
- c7 ``scripts/<name>.py`` ("c7: deterministic helpers");
- c8 ``plugin/<name>.ts`` ("c8: OpenCode plugins").

A path off that surface (Harness² ignores it when it reads a tree) is its own
group, ``other``, so a stray edit is not lost; the README stubs are in it too.
"""

from __future__ import annotations

import hashlib
import json
import math
import os
from collections.abc import Collection, Mapping, Sequence
from dataclasses import asdict, dataclass, field
from typing import Any

from evalsi.compare import compare_results, record_values

Tree = dict[str, bytes]


class AblationError(ValueError):
    """An input that cannot be planned or attributed; never a zero effect."""


COMPONENT_NAMES = {
    "c1": "systemprompt",
    "c2": "guardrails",
    "c3": "memory",
    "c4": "tool-guidance",
    "c5": "sub-agents",
    "c6": "skills",
    "c7": "scripts",
    "c8": "plugins",
    "other": "off-surface files",
}

_ALWAYS_ON = {
    "systemprompt.md": "c1",
    "guardrails.md": "c2",
    "LongTermMEMORY.md": "c3",
    "tool_descriptions/tool_guidance.md": "c4",
}
_PREFIXES = (("sub_agents/", "c5"), ("skills/", "c6"), ("scripts/", "c7"), ("plugin/", "c8"))


def component_of(rel: str) -> str:
    """The component (``c1``..``c8``, or ``other``) a harness file belongs to."""
    if rel in _ALWAYS_ON:
        return _ALWAYS_ON[rel]
    if rel.endswith("/README.md") and rel.startswith(tuple(p for p, _ in _PREFIXES)):
        return "other"  # Harness² writes these stubs itself and drops them on read
    for prefix, component in _PREFIXES:
        if rel.startswith(prefix):
            return component
    return "other"


GRANULARITIES = ("component", "unit")


def unit_of(rel: str) -> str:
    """The finest group a file reverts with: its component, and for the components that hold
    several independent things, which one. ``c6:fmt`` is the skill ``skills/fmt/``, ``c5:coder``
    the sub-agent ``sub_agents/coder/``, ``c7:scripts/x.py`` and ``c8:plugin/y.ts`` one file.
    The always-on files and anything off the surface are their own unit."""
    comp = component_of(rel)
    parts = rel.split("/")
    if comp in ("c5", "c6") and len(parts) >= 3:
        return f"{comp}:{parts[1]}"
    if comp in ("c7", "c8"):
        return f"{comp}:{rel}"
    return comp


def group_of(rel: str, by: str) -> str:
    if by not in GRANULARITIES:
        raise AblationError(f"granularity must be one of {', '.join(GRANULARITIES)}")
    return unit_of(rel) if by == "unit" else component_of(rel)


def _group_name(group: str) -> str:
    comp, sep, unit = group.partition(":")
    return (
        f"{COMPONENT_NAMES.get(comp, comp)}: {unit}" if sep else COMPONENT_NAMES.get(group, group)
    )


def normalize_rel(rel: str) -> str:
    """Reject paths that escape the harness (as Harness²'s ``normalize_rel`` does)."""
    r = str(rel).strip().replace("\\", "/")
    if not r or r.startswith("/") or ".." in r.split("/"):
        raise AblationError(f"path escapes the harness: {rel!r}")
    while r.startswith("./"):
        r = r[2:]
    return r.strip("/")


def read_dir(path: str | os.PathLike[str]) -> Tree:
    """``{relative path: bytes}`` for every file under a harness directory.
    A symlink (file or directory) is refused, not followed."""
    root = os.fspath(path)
    if os.path.islink(root) or not os.path.isdir(root):
        raise AblationError(f"{root}: not a harness directory")
    tree: Tree = {}
    for here, dirs, files in os.walk(root, followlinks=False):
        for name in [*dirs, *files]:
            full = os.path.join(here, name)
            if os.path.islink(full):
                raise AblationError(f"{full}: symlinks are not allowed in a harness")
        dirs.sort()
        for name in sorted(files):
            full = os.path.join(here, name)
            rel = normalize_rel(os.path.relpath(full, root))
            if "__pycache__" in rel.split("/"):
                continue
            with open(full, "rb") as handle:
                tree[rel] = handle.read()
    return tree


def read_executables(path: str | os.PathLike[str]) -> set[str]:
    """The relative paths under a harness directory that have an execute bit. The
    content is read by :func:`read_dir`; the mode is kept apart so a variant can be
    written with the permissions of the side its content came from."""
    root = os.fspath(path)
    out: set[str] = set()
    for here, _dirs, files in os.walk(root, followlinks=False):
        for name in files:
            full = os.path.join(here, name)
            if not os.path.islink(full) and os.stat(full).st_mode & 0o111:
                out.add(normalize_rel(os.path.relpath(full, root)))
    return out


@dataclass
class ComponentEdit:
    component: str
    name: str
    added: list[str] = field(default_factory=list)
    removed: list[str] = field(default_factory=list)
    modified: list[str] = field(default_factory=list)

    @property
    def files(self) -> list[str]:
        return sorted([*self.added, *self.removed, *self.modified])

    def to_dict(self) -> dict[str, Any]:
        out = asdict(self)
        out["files"] = self.files
        return out


def plan(
    base: Mapping[str, bytes], adopted: Mapping[str, bytes], by: str = "component"
) -> list[ComponentEdit]:
    """One entry per component (or, with ``by="unit"``, per skill, sub-agent, script or
    plugin) the adopted harness edits, in component order."""
    for rel in [*base, *adopted]:
        normalize_rel(rel)
    edits: dict[str, ComponentEdit] = {}

    def edit(rel: str) -> ComponentEdit:
        group = group_of(rel, by)
        return edits.setdefault(group, ComponentEdit(group, _group_name(group)))

    for rel in sorted(set(adopted) - set(base)):
        edit(rel).added.append(rel)
    for rel in sorted(set(base) - set(adopted)):
        edit(rel).removed.append(rel)
    for rel in sorted(set(base) & set(adopted)):
        if base[rel] != adopted[rel]:
            edit(rel).modified.append(rel)
    order = {c: i for i, c in enumerate(COMPONENT_NAMES)}
    return sorted(edits.values(), key=lambda e: (order[e.component.split(":")[0]], e.component))


def revert_variant(
    base: Mapping[str, bytes],
    adopted: Mapping[str, bytes],
    component: str,
    by: str = "component",
) -> Tree:
    """The adopted harness with one component (or unit) put back to the base's content."""
    out: Tree = {rel: data for rel, data in adopted.items() if group_of(rel, by) != component}
    out.update({rel: data for rel, data in base.items() if group_of(rel, by) == component})
    return out


def revert_executables(
    base: set[str],
    adopted: set[str],
    base_tree: Mapping[str, bytes],
    component: str,
    by: str = "component",
) -> set[str]:
    """The execute bits of :func:`revert_variant`'s files: the base's for the reverted
    component, the adopted harness's for the rest."""
    keep = {rel for rel in adopted if group_of(rel, by) != component}
    return keep | {rel for rel in base if rel in base_tree and group_of(rel, by) == component}


def _write_tree(tree: Mapping[str, bytes], dest: str, executable: Collection[str] = ()) -> None:
    root = os.path.realpath(dest)
    os.makedirs(root, exist_ok=True)
    for rel, data in tree.items():
        target = os.path.realpath(os.path.join(root, normalize_rel(rel)))
        if os.path.commonpath([root, target]) != root:
            raise AblationError(f"path escapes the output: {rel!r}")
        os.makedirs(os.path.dirname(target), exist_ok=True)
        with open(target, "xb") as handle:
            handle.write(data)
        if rel in executable:
            os.chmod(target, os.stat(target).st_mode | 0o111)


def _variant_dir(group: str) -> str:
    if ":" not in group:
        return group
    safe = "".join(ch if ch.isalnum() or ch in "-_." else "-" for ch in group)
    return f"{safe[:40]}-{hashlib.sha1(group.encode()).hexdigest()[:8]}"


def plan_dirs(
    base_dir: str | os.PathLike[str],
    adopted_dir: str | os.PathLike[str],
    out_dir: str | os.PathLike[str],
    by: str = "component",
) -> dict[str, Any]:
    """Write ``<out>/<variant>/`` for each edited component (or unit) and
    ``<out>/manifest.json``; return the manifest. A unit's directory is its id with the
    characters that are not safe in a name replaced, plus a short hash so two never collide."""
    base, adopted = read_dir(base_dir), read_dir(adopted_dir)
    base_x, adopted_x = read_executables(base_dir), read_executables(adopted_dir)
    out = os.fspath(out_dir)
    if os.path.islink(out):
        raise AblationError(f"{out}: the output directory is a symlink")
    if os.path.isdir(out) and os.listdir(out):
        raise AblationError(f"{out}: the output directory is not empty")
    real_out = os.path.realpath(out)
    for src in (base_dir, adopted_dir):
        real = os.path.realpath(src)
        if os.path.commonpath([real, real_out]) in (real, real_out):
            raise AblationError(f"{out}: overlaps the harness directory {src}")
    edits = plan(base, adopted, by)
    variants = []
    for e in edits:
        dirname = _variant_dir(e.component)
        _write_tree(
            revert_variant(base, adopted, e.component, by),
            os.path.join(out, dirname),
            revert_executables(base_x, adopted_x, base, e.component, by),
        )
        entry = e.to_dict()
        entry["variant_dir"] = dirname
        variants.append(entry)
    manifest = {
        "base": os.fspath(base_dir),
        "adopted": os.fspath(adopted_dir),
        "granularity": by,
        "note": "each variant is the adopted harness with that component reverted to the base",
        "components": variants,
    }
    os.makedirs(out, exist_ok=True)
    with open(os.path.join(out, "manifest.json"), "x", encoding="utf-8") as handle:
        json.dump(manifest, handle, indent=2)
        handle.write("\n")
    return manifest


# --- Attribution -------------------------------------------------------------

EFFECT, HARMFUL = "effect", "harmful"
NO_EFFECT = "no measurable effect"
INSUFFICIENT = "insufficient data"


@dataclass
class Attribution:
    component: str
    name: str
    status: str
    effect: float | None = None
    low: float | None = None
    high: float | None = None
    paired_n: int = 0

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


def _dataset(data: Mapping[str, Any]) -> str:
    manifest = data.get("manifest") or {}
    return str(
        ((manifest.get("dataset") or {}) if isinstance(manifest, Mapping) else {}).get("sha256")
        or ""
    )


def _classify(
    component: str,
    reference: Mapping[str, Any],
    full: Mapping[str, Any],
    metric: str,
    level: float,
) -> Attribution:
    name = _group_name(component)
    for c in compare_results(reference, full, level=level):
        if c.metric != metric:
            continue
        if c.diff is None or c.diff_low is None or c.diff_high is None or c.paired_n < 2:
            return Attribution(component, name, INSUFFICIENT, c.diff, paired_n=c.paired_n)
        status = NO_EFFECT
        if c.significant:
            # `regressed` follows the metric's direction; the effect stays in raw units.
            status = HARMFUL if c.regressed else EFFECT
        return Attribution(component, name, status, c.diff, c.diff_low, c.diff_high, c.paired_n)
    return Attribution(component, name, INSUFFICIENT)


def attribute(
    base: Mapping[str, Any],
    full: Mapping[str, Any],
    variants: Mapping[str, Mapping[str, Any]],
    metric: str,
    *,
    level: float = 0.95,
) -> list[Attribution]:
    """The effect of each component (the full adopted harness minus its revert
    variant, paired by record and trial), then the total (full minus base) as
    ``total``. The sign follows the metric: a positive effect means the edit
    raised the metric. A component whose interval includes zero has "no
    measurable effect", and with fewer than two pairs "insufficient data"; the
    latter is not a claim of no effect. The results must be of one dataset."""
    datasets = {_dataset(d) for d in (base, full, *variants.values())} - {""}
    if len(datasets) > 1:
        raise AblationError("the results files were not run on the same dataset")
    out = [_classify(c, v, full, metric, level) for c, v in variants.items()]
    total = _classify("total", base, full, metric, level)
    total.name = "full adopted vs base"
    out.append(total)
    return out


def attribution_table(rows: Sequence[Attribution]) -> str:
    head = ["component", "name", "effect", "interval", "n", "verdict"]
    lines = [head]
    for r in rows:
        effect = "-" if r.effect is None else f"{r.effect:+.3f}"
        interval = "-" if r.low is None else f"[{r.low:+.3f}, {r.high:+.3f}]"
        lines.append([r.component, r.name, effect, interval, str(r.paired_n), r.status])
    widths = [max(len(r[i]) for r in lines) for i in range(len(head))]
    return "\n".join(
        "  ".join(cell.ljust(w) for cell, w in zip(r, widths, strict=True)).rstrip() for r in lines
    )


# --- Cross-task harm --------------------------------------------------------


@dataclass
class TaskEffect:
    record: str
    effect: float  # full adopted minus the revert variant, averaged over the record's trials

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass
class Harm:
    component: str
    name: str
    helped: list[TaskEffect] = field(default_factory=list)
    hurt: list[TaskEffect] = field(default_factory=list)
    unchanged: int = 0

    @property
    def compared(self) -> int:
        """How many tasks the full harness and the variant could be compared on."""
        return len(self.helped) + len(self.hurt) + self.unchanged

    @property
    def mixed(self) -> bool:
        """The edit helps some tasks and hurts others, whatever its average."""
        return bool(self.helped) and bool(self.hurt)

    @property
    def sign_p(self) -> float | None:
        """Two-sided exact sign test over the tasks that changed: how surprising a split of
        this size between helped and hurt would be if the edit were as likely to hurt a
        task as to help it. None when no task changed. It speaks to the direction of the
        edit across tasks; it does not say that any one task's change is more than noise."""
        n = len(self.helped) + len(self.hurt)
        if n == 0:
            return None
        k = min(len(self.helped), len(self.hurt))
        tail = sum(math.comb(n, i) for i in range(k + 1)) / 2**n
        return float(min(1.0, 2 * tail))

    def to_dict(self) -> dict[str, Any]:
        out = asdict(self)
        out["mixed"] = self.mixed
        out["sign_p"] = self.sign_p
        out["compared"] = self.compared
        return out


def _higher_is_better(data: Mapping[str, Any], metric: str) -> bool:
    for s in data.get("summaries") or []:
        if s.get("metric") == metric:
            return bool(s.get("higher_is_better", True) is not False)
    return True


def harm_check(
    full: Mapping[str, Any],
    variants: Mapping[str, Mapping[str, Any]],
    metric: str,
    *,
    min_effect: float = 0.0,
) -> list[Harm]:
    """Per component, which tasks the edit helped and which it hurt: for each record the
    full adopted harness minus the component's revert variant, averaged over trials, in the
    metric's direction (a positive number is always an improvement). A component that
    helps some tasks and hurts others is ``mixed`` and worth a look even when its average
    effect is zero or positive. Only a difference larger than ``min_effect`` counts as help
    or harm. ``Harm.sign_p`` adds an exact sign test across the tasks that changed. Neither
    can show that a single task's change is more than noise, and a handful of tasks gives a
    sign test almost no power (5 helped and 0 hurt is p = 0.0625), so read this as where to
    look, not as a verdict."""
    if not math.isfinite(min_effect) or min_effect < 0:
        raise AblationError("min_effect must be a finite number, not negative")
    datasets = {_dataset(d) for d in (full, *variants.values())} - {""}
    if len(datasets) > 1:
        raise AblationError("the results files were not run on the same dataset")
    sign = 1.0 if _higher_is_better(full, metric) else -1.0
    full_vals = record_values(full).get(metric, {})
    out = []
    for component, variant in variants.items():
        base_vals = record_values(variant).get(metric, {})
        per_record: dict[str, list[float]] = {}
        for key in set(full_vals) & set(base_vals):
            per_record.setdefault(key.rsplit("#", 1)[0], []).append(
                sign * (full_vals[key] - base_vals[key])
            )
        harm = Harm(component, _group_name(component))
        for rid in sorted(per_record):
            effect = sum(per_record[rid]) / len(per_record[rid])
            if effect > min_effect:
                harm.helped.append(TaskEffect(rid, effect))
            elif effect < -min_effect:
                harm.hurt.append(TaskEffect(rid, effect))
            else:
                harm.unchanged += 1
        harm.hurt.sort(key=lambda t: t.effect)
        out.append(harm)
    return out


def harm_table(rows: Sequence[Harm]) -> str:
    head = ["component", "name", "helped", "hurt", "same", "sign p", "verdict", "most hurt"]
    lines = [head]
    for h in rows:
        worst = ", ".join(f"{t.record} ({t.effect:+.2f})" for t in h.hurt[:3]) or "-"
        if not h.compared:
            verdict = INSUFFICIENT  # nothing was compared: that is missing evidence, not "ok"
        elif h.mixed:
            verdict = "mixed: helps some, hurts others"
        else:
            verdict = "hurts" if h.hurt else "ok"
        p = h.sign_p
        lines.append(
            [
                h.component,
                h.name,
                str(len(h.helped)),
                str(len(h.hurt)),
                str(h.unchanged),
                "-" if p is None else f"{p:.3f}",
                verdict,
                worst,
            ]
        )
    widths = [max(len(r[i]) for r in lines) for i in range(len(head))]
    return "\n".join(
        "  ".join(cell.ljust(w) for cell, w in zip(r, widths, strict=True)).rstrip() for r in lines
    )
