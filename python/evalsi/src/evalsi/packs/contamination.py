"""Contamination guard: flag trajectories that fetch benchmark rubric or answer pages (H7).

Public benchmark rubrics may be online, and an agent with web access can read them.
``contamination-fetch`` looks at the tool steps of a trajectory (web fetch and search
tools, ``curl`` or ``wget`` in a shell command, any URL in a tool's input) and flags:

* a ``raw.githubusercontent.com`` or ``github.com`` URL whose path has a word such as
  ``rubric``, ``answer``, ``solution`` or ``gold``;
* a ``huggingface.co/datasets/<name>`` URL (or ``hf://datasets/<name>``, or a
  ``dataset=<name>`` query on the datasets server) for a configured dataset name;
* a URL matching an extra ``host/path`` glob the caller passes;
* a canary string, either one the caller passes or an Evals.si canary
  (``EVALSI_CANARY_<32 hex>``, see ``canaries.py``), in a tool step's input or output.

It is a string match on recorded steps. It does not see a rubric the agent reads from a
local file, a search query that never becomes a URL, or an answer recalled from training.
A clean result is not proof of a clean run. Ordinary repository pages with those words in
the path (``solutions-architecture.md``) are flagged: read the evidence before acting.
"""

from __future__ import annotations

import fnmatch
import re
from typing import Any
from urllib.parse import parse_qs, urlsplit

from evalsi.evaluator import MetricSpec, Requirements, ScoreType, SkipRecord, evaluator
from evalsi.types import Record, Score

DEFAULT_HOSTS = ("raw.githubusercontent.com", "github.com", "gist.github.com")
DEFAULT_WORDS = (
    "rubric",
    "rubrics",
    "answer",
    "answers",
    "solution",
    "solutions",
    "gold",
)
# The prefix canaries.make_canaries writes, followed by 16 random bytes in hex.
_CANARY = re.compile(r"EVALSI_CANARY_[0-9a-f]{32}")
_URL = re.compile(
    r"""(?:(?:https?|hf)://|\b(?=(?:raw\.githubusercontent\.com|gist\.github\.com|github\.com|huggingface\.co)/))[^\s"'<>)\]\\]+""",
    re.IGNORECASE,
)


def _words(path: str) -> set[str]:
    """Lowercase words of a path, split on separators and camelCase (``GoldAnswers``)."""
    spaced = re.sub(r"([a-z0-9])([A-Z])", r"\1 \2", path)
    return {w for w in re.split(r"[^A-Za-z0-9]+", spaced.lower()) if w}


def _split(url: str) -> tuple[str, str, str]:
    """(host, path, query) of a URL with or without a scheme."""
    parts = urlsplit(url if "://" in url else "//" + url)
    return (parts.hostname or "").lower(), parts.path, parts.query


def _dataset_names(path: str, host: str, query: str, scheme_hf: bool) -> set[str]:
    segments = [s for s in path.split("/") if s]
    names: set[str] = set()
    if scheme_hf:
        # hf://datasets/org/name/file; the host part is "datasets".
        segments = [host, *segments] if host else segments
    if len(segments) >= 2 and segments[0].lower() == "datasets":
        rest = segments[1:]
        names.add(rest[0].lower())
        if len(rest) >= 2:
            names.add(f"{rest[0]}/{rest[1]}".lower())
    names.update(d.lower() for d in parse_qs(query).get("dataset", []))
    return names


def _classify(
    url: str, *, hosts: list[str], words: set[str], datasets: set[str], patterns: list[str]
) -> str | None:
    host, path, query = _split(url)
    scheme_hf = url.lower().startswith("hf://")
    if host in hosts and words & _words(path):
        return f"rubric or answer path on {host}"
    if (host.endswith("huggingface.co") or scheme_hf) and datasets:
        hit = _dataset_names(path, host, query, scheme_hf) & datasets
        if hit:
            return f"configured dataset {sorted(hit)[0]}"
    target = f"{host}{path}".lower()
    for pattern in patterns:
        if fnmatch.fnmatch(target, pattern.lower()):
            return f"matches pattern {pattern}"
    return None


@evaluator(
    name="builtin/contamination-fetch",
    version="1.0.0",
    description=(
        "Flags a trajectory whose tool steps fetch a benchmark rubric or answer page: "
        "GitHub URLs with rubric, answer, solution or gold in the path, configured "
        "Hugging Face datasets, extra host/path globs, or canary strings. Scores 1 when "
        "flagged and 0 when clean (lower is better), with the matched evidence in "
        "metadata. Skipped when the trajectory has no steps. A string match, not proof."
    ),
    requires=Requirements(output=False, trajectory=True),
    outputs=[
        MetricSpec(
            "contamination-fetch", ScoreType.NUMBER, min=0.0, max=1.0, higher_is_better=False
        )
    ],
)
def contamination_fetch(
    record: Record,
    *,
    hosts: list[str] | None = None,
    words: list[str] | None = None,
    datasets: list[str] | None = None,
    patterns: list[str] | None = None,
    canaries: list[str] | None = None,
) -> Score:
    """``hosts`` and ``words`` replace the defaults (GitHub hosts; rubric, answer, solution,
    gold). ``datasets`` are Hugging Face names, ``org/name`` or ``name``; none by default.
    ``patterns`` are ``host/path`` globs (``example.com/bench/*``). ``canaries`` are extra
    strings; Evals.si canaries are always looked for."""
    assert record.trajectory is not None
    steps = record.trajectory.steps
    if not steps:
        raise SkipRecord("the trajectory has no steps")
    host_list = [h.lower() for h in (DEFAULT_HOSTS if hosts is None else hosts)]
    word_set = {w.lower() for w in (DEFAULT_WORDS if words is None else words)}
    dataset_set = {d.lower() for d in datasets or []}
    extra = [c for c in canaries or [] if c]
    evidence: list[dict[str, Any]] = []
    for index, step in enumerate(steps):
        if step.type not in ("tool", "retrieval"):
            continue
        inp = step.input.as_text() if step.input is not None else ""
        out = step.output.as_text() if step.output is not None else ""
        for match in _URL.finditer(inp):
            url = match.group(0).rstrip(".,;:")
            reason = _classify(
                url,
                hosts=host_list,
                words=word_set,
                datasets=dataset_set,
                patterns=list(patterns or []),
            )
            if reason:
                evidence.append(
                    {"step": index, "tool": step.name, "kind": "url", "url": url, "reason": reason}
                )
        for where, text in (("input", inp), ("output", out)):
            found = sorted(set(_CANARY.findall(text)) | {c for c in extra if c in text})
            evidence += [
                {"step": index, "tool": step.name, "kind": "canary", "where": where, "canary": c}
                for c in found
            ]
    flagged = bool(evidence)
    return Score(
        number=1.0 if flagged else 0.0,
        explanation=(
            "; ".join(
                f"step {e['step']} {e.get('url') or e['canary']}: {e.get('reason', 'canary seen')}"
                for e in evidence[:5]
            )
            if flagged
            else "no tool step fetched a rubric or answer page"
        ),
        metadata={"flagged": flagged, "evidence": evidence},
    )
