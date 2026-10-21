from __future__ import annotations

import asyncio

import pytest

from conftest import make_record
from evalsi import EvalContext, Score, SkipRecord, TaskCheck
from evalsi.packs import agent
from evalsi.runner import evaluate


def check(**tests: str) -> TaskCheck:
    return TaskCheck(passed=all(v != "failed" for v in tests.values()), tests=tests)


def run(assertion: str, task_check: TaskCheck | None) -> list[Score]:
    record = make_record("done")
    record.check = task_check
    return asyncio.run(
        agent.state_assertion.bind({"assertion": assertion}).run(record, EvalContext())
    )


def test_a_passed_or_failed_assertion_is_scored() -> None:
    assert run("refund-amount", check(**{"refund-amount": "passed"}))[0].passed is True
    failed = run("refund-amount", check(**{"refund-amount": "failed"}))[0]
    assert failed.passed is False
    assert "refund-amount: failed" in failed.explanation


@pytest.mark.parametrize("tests", [{"refund-amount": "skipped"}, {"handoff-correct": "passed"}, {}])
def test_an_assertion_that_does_not_apply_is_skipped_not_passed(tests: dict[str, str]) -> None:
    with pytest.raises(SkipRecord):
        run("refund-amount", check(**tests))


def test_no_checker_is_skipped_and_a_broken_checker_is_an_error() -> None:
    with pytest.raises(SkipRecord):
        run("refund-amount", None)
    with pytest.raises(ValueError, match="did not run"):
        run("refund-amount", TaskCheck(passed=False, error="timed out"))


def test_an_alias_gives_each_assertion_its_own_metric() -> None:
    records = []
    for rid, tests in [
        ("a", {"refund-amount": "passed", "handoff-correct": "failed"}),
        ("b", {"refund-amount": "passed", "handoff-correct": "skipped"}),
    ]:
        record = make_record("done", id=rid)
        record.check = check(**tests)
        records.append(record)
    result = evaluate(
        records,
        [
            {"ref": "state-assertion", "name": name, "params": {"assertion": name}}
            for name in ("refund-amount", "handoff-correct")
        ],
        cache=False,
    )
    amount, handoff = result.metric("refund-amount"), result.metric("handoff-correct")
    assert (amount.mean, amount.n) == (1.0, 2)
    assert amount.higher_is_better is True
    # The skipped task is left out of the mean instead of counting as a pass.
    assert (handoff.mean, handoff.n, handoff.skipped) == (0.0, 1, 1)
