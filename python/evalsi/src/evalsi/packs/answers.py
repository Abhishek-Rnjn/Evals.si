"""Answer checks for web and document question-answering benchmarks (H4).

These score the answer an agent gives to a task the ``webarena://``, ``gaia://`` and
``officeqa://`` importers produced. Each reproduces, as far as it is stated below, the
benchmark's own rule, and says in its description what it leaves out.

* ``builtin/webarena-string-match``: WebArena's ``StringEvaluator`` for ``exact_match``
  and ``must_include`` reference answers (read from ``evaluation_harness/evaluators.py``
  of ``web-arena-x/webarena``, ``main`` on 2026-10-22). ``fuzzy_match`` needs an LLM judge
  and is skipped, as are tasks graded only by ``url_match`` or ``program_html``.
* ``builtin/webarena-url-match``: its ``URLEvaluator`` (rule ``GOLD in PRED``) on the
  last page the browser tool reported in the trajectory. ``program_html`` needs the live
  page's DOM and is not reproduced.
* ``builtin/gaia-match``: GAIA's quasi-exact match. Reimplemented from the published scorer
  as remembered (numbers compared as numbers after dropping ``$``, ``%`` and ``,``;
  comma or semicolon lists element by element; other strings with whitespace and
  punctuation removed). The scorer's source is on a gated Hugging Face space that was
  not reachable when this was written, so it has not been diffed against it.
* ``builtin/officeqa-reward``: Databricks's ``reward.py`` for OfficeQA, run from a copy
  you supply (it is not vendored here). The file's SHA-256 must be the pinned one, or one
  you pass after reading it, so a spec cannot make a worker import an arbitrary file.
"""

from __future__ import annotations

import hashlib
import importlib.util
import re
import string
import sys
import urllib.parse
from collections import defaultdict
from collections.abc import Mapping
from pathlib import Path
from types import ModuleType
from typing import Any

from evalsi.evaluator import (
    EvaluatorConfigError,
    MetricSpec,
    Requirements,
    ScoreType,
    SkipRecord,
    evaluator,
)
from evalsi.registry import Pack
from evalsi.types import Record, Score

PASSED = ScoreType.PASSED

# SHA-256 of databricks/officeqa reward.py as read from main on 2026-10-22.
OFFICEQA_REWARD_SHA256 = "0d91698c87df6d889339aac36f63ae0966607f169890b0bf8b472b26bfe8138f"


def _answer_text(record: Record) -> str:
    assert record.output is not None
    return record.output.as_text()


# --- WebArena ---


def _clean_answer(answer: str) -> str:
    answer = answer.strip()
    if (answer.startswith("'") and answer.endswith("'")) or (
        answer.startswith('"') and answer.endswith('"')
    ):
        answer = answer[1:-1]
    return answer.lower()


def _webarena(record: Record) -> Mapping[str, Any]:
    data = record.metadata.get("webarena")
    if not isinstance(data, Mapping) or not isinstance(data.get("eval"), Mapping):
        raise SkipRecord("the record carries no metadata.webarena.eval (it is not a WebArena task)")
    return data


def _must_include(ref: str, pred: str, *, single: bool) -> bool:
    clean_ref, clean_pred = _clean_answer(ref), _clean_answer(pred)
    # WebArena tokenizes the answer when the only reference is one character, so that "0"
    # does not match "10"; word_tokenize splits on words and punctuation like this.
    if single and len(clean_ref) == 1:
        return clean_ref in re.findall(r"\w+|[^\w\s]", clean_pred)
    return clean_ref in clean_pred


@evaluator(
    name="builtin/webarena-string-match",
    version="1.0.0",
    description=(
        "WebArena's string check on the final answer: exact_match and must_include reference "
        "answers, case-insensitive. Skips tasks whose reference is fuzzy_match (an LLM judge), "
        "url_match only or program_html only. A task that also has url_match or program_html "
        "passes this part only; the score says which parts it did not check."
    ),
    requires=Requirements(output=True),
    outputs=[MetricSpec("webarena-string-match", PASSED, higher_is_better=True)],
)
def webarena_string_match(record: Record) -> Score:
    ev = _webarena(record)["eval"]
    refs = ev.get("reference_answers")
    if not isinstance(refs, Mapping) or not refs:
        raise SkipRecord("the task has no string reference answers (url_match or program_html)")
    if "fuzzy_match" in refs:
        raise SkipRecord("fuzzy_match needs an LLM judge, which WebArena's own is not reproduced")
    pred = _clean_answer(_answer_text(record))
    ok = True
    for approach, value in refs.items():
        if approach == "exact_match":
            ok = ok and _clean_answer(str(value)) == pred
        elif approach == "must_include":
            items = [value] if isinstance(value, str) else list(value)
            ok = ok and all(_must_include(str(v), pred, single=len(items) == 1) for v in items)
        else:
            raise SkipRecord(f"unknown reference answer kind {approach!r}")
    unchecked = [t for t in ev.get("eval_types") or [] if t != "string_match"]
    return Score(
        passed=ok,
        explanation="" if not unchecked else f"not checked here: {', '.join(unchecked)}",
        metadata={"unchecked_eval_types": unchecked},
    )


def _last_page_url(record: Record) -> str | None:
    if record.trajectory is None:
        return None
    url = None
    for step in record.trajectory.steps:
        if step.type == "tool" and step.name.startswith("browser_") and step.output is not None:
            found = re.search(r"^Page URL: (\S+)", step.output.as_text(), re.MULTILINE)
            if found:
                url = found.group(1)
    return url


@evaluator(
    name="builtin/webarena-url-match",
    version="1.0.0",
    description=(
        "WebArena's URL check (rule 'GOLD in PRED'): the last page the browser tool reported "
        "must contain the reference URL's host and path and its query values. Needs the "
        "trajectory of a run that used the browser tools. program_html is not reproduced."
    ),
    requires=Requirements(output=False, trajectory=True),
    outputs=[MetricSpec("webarena-url-match", PASSED, higher_is_better=True)],
)
def webarena_url_match(record: Record) -> Score:
    ev = _webarena(record)["eval"]
    reference = ev.get("reference_url")
    if "url_match" not in (ev.get("eval_types") or []) or not isinstance(reference, str):
        raise SkipRecord("the task has no url_match reference")
    rule = ev.get("url_note") or "GOLD in PRED"
    if rule != "GOLD in PRED":
        raise SkipRecord(f"unknown URL matching rule {rule!r}")
    page = _last_page_url(record)
    if page is None:
        return Score(passed=False, explanation="the trajectory shows no browser page")

    def parse(url: str) -> tuple[str, dict[str, list[str]]]:
        parts = urllib.parse.urlparse(url.rstrip("/"))
        return parts.netloc + parts.path, urllib.parse.parse_qs(parts.query)

    pred_base, pred_query = parse(page)
    ref_bases: list[str] = []
    ref_queries: dict[str, set[str]] = defaultdict(set)
    for ref in reference.split(" |OR| "):
        base, query = parse(ref)
        ref_bases.append(base)
        for key, values in query.items():
            ref_queries[key].update(values)
    base_ok = any(base in pred_base for base in ref_bases)
    query_ok = all(
        any(v in pred_query.get(key, []) for v in values) for key, values in ref_queries.items()
    )
    return Score(passed=base_ok and query_ok, explanation=f"last page: {page}")


# --- GAIA ---


def _is_float(value: str) -> bool:
    try:
        float(value)
    except ValueError:
        return False
    return True


def _gaia_number(text: str) -> float:
    for char in ("$", "%", ","):
        text = text.replace(char, "")
    try:
        return float(text)
    except ValueError:
        return float("inf")


def _gaia_str(text: str, *, remove_punct: bool = True) -> str:
    squeezed = re.sub(r"\s", "", text).lower()
    return (
        squeezed.translate(str.maketrans("", "", string.punctuation)) if remove_punct else squeezed
    )


def gaia_quasi_exact_match(model_answer: str, ground_truth: str) -> bool:
    """GAIA's scoring rule; see the module docstring for what it was checked against."""
    if _is_float(ground_truth):
        return _gaia_number(model_answer) == float(ground_truth)
    if any(c in ground_truth for c in ",;"):
        want, got = re.split("[,;]", ground_truth), re.split("[,;]", model_answer)
        if len(want) != len(got):
            return False
        return all(
            _gaia_number(g) == float(w)
            if _is_float(w)
            else _gaia_str(g, remove_punct=False) == _gaia_str(w, remove_punct=False)
            for g, w in zip(got, want, strict=True)
        )
    return _gaia_str(model_answer) == _gaia_str(ground_truth)


_FINAL_ANSWER = re.compile(r"final answer\s*:\s*", re.IGNORECASE)


def gaia_final_answer(text: str) -> str:
    """What follows the last ``FINAL ANSWER:`` in a reply, or the whole reply."""
    parts = _FINAL_ANSWER.split(text)
    return parts[-1].strip() if len(parts) > 1 else text.strip()


@evaluator(
    name="builtin/gaia-match",
    version="1.0.0",
    description=(
        "GAIA's quasi-exact match of the answer after 'FINAL ANSWER:' against the reference: "
        "numbers as numbers, comma or semicolon lists element by element, other text with "
        "whitespace and punctuation ignored. Skips records with no reference (the test split "
        "withholds answers). Reimplemented from the published rule, not diffed against it."
    ),
    requires=Requirements(output=True, reference=True),
    outputs=[MetricSpec("gaia-match", PASSED, higher_is_better=True)],
)
def gaia_match(record: Record) -> Score:
    assert record.reference is not None
    truth = record.reference.as_text().strip()
    if truth in ("", "?"):
        raise SkipRecord("the reference answer is withheld")
    answer = gaia_final_answer(_answer_text(record))
    return Score(passed=gaia_quasi_exact_match(answer, truth), explanation=f"answer: {answer[:80]}")


# --- OfficeQA ---

_REWARD_MODULES: dict[str, ModuleType] = {}


def load_officeqa_reward(path: str, sha256: str = OFFICEQA_REWARD_SHA256) -> ModuleType:
    """Imports Databricks's ``reward.py`` from ``path`` if its SHA-256 is ``sha256``."""
    source = Path(path).expanduser()
    try:
        data = source.read_bytes()
    except OSError as exc:
        raise EvaluatorConfigError(f"cannot read reward_py {path!r}: {exc}") from None
    digest = hashlib.sha256(data).hexdigest()
    if digest != sha256.lower():
        raise EvaluatorConfigError(
            f"{path} has SHA-256 {digest}, not {sha256}: it is not the reward.py this evaluator "
            "was checked against. Read it, then pass sha256=<its hash> to use it"
        )
    if digest in _REWARD_MODULES:
        return _REWARD_MODULES[digest]
    spec = importlib.util.spec_from_loader(f"_officeqa_reward_{digest[:12]}", loader=None)
    assert spec is not None
    module = importlib.util.module_from_spec(spec)
    exec(compile(data, str(source), "exec"), module.__dict__)
    if not callable(getattr(module, "score_answer", None)):
        raise EvaluatorConfigError(
            f"{path} has no score_answer(ground_truth, predicted, tolerance)"
        )
    _REWARD_MODULES[digest] = module
    sys.modules.setdefault(spec.name, module)
    return module


@evaluator(
    name="builtin/officeqa-reward",
    version="1.0.0",
    description=(
        "OfficeQA's own scorer (reward.py: numbers within a relative tolerance with units, "
        "dates and text exactly, a direct answer only), run from the copy at reward_py, whose "
        "SHA-256 must be the pinned one (or sha256). tolerance is the relative error allowed "
        "(0.0 exact, 0.01 is 1%). The score is 1 or 0."
    ),
    requires=Requirements(output=True, reference=True),
    outputs=[MetricSpec("officeqa-reward", PASSED, higher_is_better=True)],
)
def officeqa_reward(
    record: Record,
    *,
    reward_py: str = "",
    tolerance: float = 0.0,
    sha256: str = OFFICEQA_REWARD_SHA256,
) -> Score:
    if not reward_py:
        raise EvaluatorConfigError(
            "officeqa-reward needs reward_py: the path to reward.py from the databricks/officeqa "
            "repository"
        )
    if not 0 <= tolerance <= 1:
        raise EvaluatorConfigError("tolerance must be between 0 and 1")
    assert record.reference is not None
    module = load_officeqa_reward(reward_py, sha256)
    score = float(module.score_answer(record.reference.as_text(), _answer_text(record), tolerance))
    return Score(passed=score == 1.0, explanation=f"tolerance {tolerance:g}")


PACK = Pack(
    name="answers",
    description="Answer checks for the WebArena, GAIA and OfficeQA benchmarks.",
    evaluators=[webarena_string_match, webarena_url_match, gaia_match, officeqa_reward],
)
