"""Answer checks for WebArena, GAIA and OfficeQA tasks. Every task here is synthetic."""

from __future__ import annotations

import hashlib
from pathlib import Path
from typing import Any

import pytest

from evalsi.evaluator import EvaluatorConfigError, SkipRecord
from evalsi.packs.answers import (
    gaia_final_answer,
    gaia_match,
    gaia_quasi_exact_match,
    load_officeqa_reward,
    officeqa_reward,
    webarena_string_match,
    webarena_url_match,
)
from evalsi.testing import score
from evalsi.types import Content, Record, Step, Trajectory


def wa(answers: Any, output: str, eval_types: Any = ("string_match",), **extra: Any) -> Record:
    ev = {"eval_types": list(eval_types), "reference_answers": answers, **extra}
    return Record(
        id="wa",
        input=Content(text="q"),
        output=Content(text=output),
        metadata={"webarena": {"task_id": 1, "eval": ev}},
    )


def passed(definition: Any, record: Record, **params: Any) -> bool:
    (s,) = score(definition, record, **params).values()
    assert s.passed is not None
    return s.passed


def test_webarena_exact_match_ignores_case_and_wrapping_quotes() -> None:
    ref = {"exact_match": "Quest Lumaflex Band"}
    assert passed(webarena_string_match, wa(ref, '"quest lumaflex band"'))
    assert passed(webarena_string_match, wa(ref, "  Quest Lumaflex Band "))
    assert not passed(webarena_string_match, wa(ref, "Quest Lumaflex Band, 2 units"))


def test_webarena_must_include_needs_every_phrase_and_tokenizes_a_single_character() -> None:
    ref = {"must_include": ["Sprite", "Impulse Duffle"]}
    assert passed(webarena_string_match, wa(ref, "impulse duffle and the sprite ball"))
    assert not passed(webarena_string_match, wa(ref, "only Sprite"))
    # One reference of one character matches a word, not a digit inside another number.
    assert not passed(webarena_string_match, wa({"must_include": ["0"]}, "there are 10 of them"))
    assert passed(webarena_string_match, wa({"must_include": ["0"]}, "there are 0 of them"))
    assert passed(webarena_string_match, wa({"must_include": ["10"]}, "there are 100"))


def test_webarena_string_match_skips_what_it_cannot_check_and_names_what_it_left() -> None:
    for ref in ({"fuzzy_match": ["x"]}, {"fuzzy_match": "N/A"}, None, {}):
        with pytest.raises(SkipRecord):
            score(webarena_string_match, wa(ref, "x"))
    both = wa({"exact_match": "a"}, "a", ("string_match", "url_match"))
    (s,) = score(webarena_string_match, both).values()
    assert s.passed is True
    assert "url_match" in s.explanation
    assert s.metadata["unchecked_eval_types"] == ["url_match"]
    with pytest.raises(SkipRecord, match="not a WebArena task"):
        score(webarena_string_match, Record(id="x", output=Content(text="a")))


def browsed(url: str) -> Trajectory:
    page = f"Page URL: {url}\nPage Title: t\nSnapshot:\n- heading"
    return Trajectory(
        steps=[
            Step(type="tool", name="browser_navigate", output=Content(text="Page URL: http://x/")),
            Step(type="tool", name="bash", output=Content(text="Page URL: http://decoy/")),
            Step(type="tool", name="browser_click", output=Content(text=page)),
        ]
    )


def url_task(reference: str, url: str, **ev: Any) -> Record:
    rec = wa(None, "", ("url_match",), reference_url=reference, **ev)
    rec.trajectory = browsed(url)
    return rec


def test_webarena_url_match_follows_gold_in_pred() -> None:
    ref = "http://git.test/dashboard/todos"
    assert passed(webarena_url_match, url_task(ref, "http://git.test/dashboard/todos/"))
    assert passed(webarena_url_match, url_task(ref, "http://git.test/dashboard/todos?x=1"))
    assert not passed(webarena_url_match, url_task(ref, "http://git.test/dashboard"))
    # Query values: every reference key needs one of its values in the page's query.
    q = "http://git.test/issues/?state=opened&sort=created_asc"
    assert passed(
        webarena_url_match, url_task(q, "http://git.test/issues/?sort=created_asc&state=opened&z=1")
    )
    assert not passed(webarena_url_match, url_task(q, "http://git.test/issues/?state=closed"))
    either = "http://a.test/x |OR| http://b.test/y"
    assert passed(webarena_url_match, url_task(either, "http://b.test/y"))
    # Only a browser tool's page counts, and a run with no page fails the task.
    rec = url_task(ref, "http://git.test/dashboard/todos")
    rec.trajectory = Trajectory(
        steps=[
            Step(
                type="tool",
                name="bash",
                output=Content(text="Page URL: http://git.test/dashboard/todos"),
            )
        ]
    )
    assert not passed(webarena_url_match, rec)
    with pytest.raises(SkipRecord):
        score(webarena_url_match, wa({"exact_match": "a"}, "a"))
    with pytest.raises(SkipRecord, match="rule"):
        score(webarena_url_match, url_task(ref, "http://git.test/", url_note="EXACT"))


def test_gaia_numbers_lists_and_strings() -> None:
    assert gaia_quasi_exact_match("$1,234.50", "1234.5")
    assert gaia_quasi_exact_match("12%", "12")
    assert not gaia_quasi_exact_match("12 apples", "12")
    assert gaia_quasi_exact_match("b, a, c", "b,a,c")
    assert not gaia_quasi_exact_match("a, b", "a, b, c")
    assert gaia_quasi_exact_match("1, two; 3", "1,two,3")
    assert not gaia_quasi_exact_match("1, two", "1, three")
    assert gaia_quasi_exact_match("Saint-Petersburg", "saint petersburg")
    # In a list, punctuation inside an element still counts.
    assert not gaia_quasi_exact_match("a.b, c", "ab, c")
    assert not gaia_quasi_exact_match("", "x")


def test_gaia_reads_the_final_answer_template_and_skips_withheld_answers() -> None:
    assert gaia_final_answer("thinking...\nFINAL ANSWER: 42") == "42"
    assert gaia_final_answer("FINAL ANSWER: 1\nno, FINAL ANSWER:  two ") == "two"
    assert gaia_final_answer("just text") == "just text"
    rec = Record(
        id="g",
        output=Content(text="so\nFINAL ANSWER: Paris"),
        reference=Content(text="paris"),
    )
    assert passed(gaia_match, rec)
    rec.reference = Content(text="?")
    with pytest.raises(SkipRecord, match="withheld"):
        score(gaia_match, rec)


FAKE_REWARD = """
def score_answer(ground_truth, predicted, tolerance=0.0):
    return 1.0 if predicted.strip() == ground_truth.strip() else 0.0
"""


def officeqa_record(ref: str, out: str) -> Record:
    return Record(id="o", output=Content(text=out), reference=Content(text=ref))


def test_officeqa_runs_only_the_pinned_reward_file(tmp_path: Path) -> None:
    path = tmp_path / "reward.py"
    path.write_text(FAKE_REWARD)
    # Not the pinned file: refused, with the hash to pass after reading it.
    with pytest.raises(EvaluatorConfigError, match=r"not the reward\.py"):
        score(officeqa_reward, officeqa_record("1", "1"), reward_py=str(path))
    digest = hashlib.sha256(FAKE_REWARD.encode()).hexdigest()
    ok = score(officeqa_reward, officeqa_record("12.5", "12.5"), reward_py=str(path), sha256=digest)
    assert ok["officeqa-reward"].passed is True
    bad = score(officeqa_reward, officeqa_record("12.5", "13"), reward_py=str(path), sha256=digest)
    assert bad["officeqa-reward"].passed is False
    with pytest.raises(EvaluatorConfigError, match="reward_py"):
        score(officeqa_reward, officeqa_record("1", "1"))
    with pytest.raises(EvaluatorConfigError, match="tolerance"):
        score(
            officeqa_reward,
            officeqa_record("1", "1"),
            reward_py=str(path),
            sha256=digest,
            tolerance=2,
        )
    with pytest.raises(EvaluatorConfigError, match="cannot read"):
        load_officeqa_reward(str(tmp_path / "none.py"))
    other = tmp_path / "noscore.py"
    other.write_text("x = 1\n")
    with pytest.raises(EvaluatorConfigError, match="score_answer"):
        load_officeqa_reward(str(other), hashlib.sha256(b"x = 1\n").hexdigest())
