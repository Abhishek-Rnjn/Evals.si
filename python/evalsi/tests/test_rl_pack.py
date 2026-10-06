from __future__ import annotations

from typing import Any

import pytest

from conftest import make_record
from evalsi import Usage
from evalsi.evaluator import EvaluatorDef, SkipRecord
from evalsi.packs import rl
from evalsi.testing import score as score_all
from evalsi.types import Record, Score


def score(definition: EvaluatorDef, record: Record, **params: Any) -> Score:
    (only,) = score_all(definition, record, **params).values()
    return only


@pytest.mark.parametrize(
    ("text", "want"),
    [
        ("so the answer is \\boxed{\\frac{3}{4}}.", "\\frac{3}{4}"),
        ("\\boxed{a_{1}} then \\boxed{\\{1,2\\}}", "\\{1,2\\}"),
        ("<think>2+2</think><answer>4</answer>", "4"),
        ("<answer>it is \\boxed{5}</answer>", "5"),
        ("Natalia sold 48/2 = 24 clips.\n#### 72", "72"),
        ("The final answer is: 12.", "12"),
        ("I think 3 apples, then 7", "7"),
        ("no digits here", None),
    ],
)
def test_extract_answer(text: str, want: str | None) -> None:
    assert rl.extract_answer(text) == want


@pytest.mark.parametrize(
    ("a", "b", "equal"),
    [
        ("\\frac{1}{2}", "0.5", True),
        ("\\dfrac12", "1/2", True),
        ("50\\%", "0.5", True),
        ("50", "50\\%", True),
        ("1,250", "1250", True),
        ("$12.00", "12", True),
        ("10 \\text{ cm}", "10", True),
        ("x = 3", "3", True),
        ("(1, 2)", "(1,2)", True),
        ("(1, 2)", "(2, 1)", False),
        ("2\\sqrt{2}", "\\sqrt{8}", True),
        ("x^2+2x+1", "(x+1)^2", True),
        ("3", "4", False),
        ("Paris", "paris", True),
    ],
)
def test_answers_equal(a: str, b: str, equal: bool) -> None:
    assert rl.answers_equal(a, b) is equal


def test_answers_equal_plain_backend_skips_algebra() -> None:
    assert not rl.answers_equal("x^2+2x+1", "(x+1)^2", backend="plain")


def test_math_equiv() -> None:
    gsm = make_record("She has 3 + 4 = 7 so \\boxed{7}", "Work...\n#### 7")
    assert score(rl.math_equiv, gsm).passed is True
    wrong = score(rl.math_equiv, make_record("\\boxed{8}", "7"))
    assert wrong.passed is False
    assert wrong.metadata == {"predicted": "8", "expected": "7"}
    none = score(rl.math_equiv, make_record("I give up", "7"), fallback="none")
    assert none.passed is False
    with pytest.raises(SkipRecord):
        score(rl.math_equiv, make_record("7", ""))


def test_format_check() -> None:
    good = make_record("<think>\nhmm\n</think>\n<answer>4</answer>\n")
    assert score(rl.format_check, good).passed is True
    assert score(rl.format_check, make_record("<answer>4</answer>")).passed is False
    twice = make_record("<answer>1</answer><answer>2</answer>")
    result = score(rl.format_check, twice, pattern="", tags=["answer"])
    assert result.passed is False
    assert "appears 2 times" in result.explanation


def test_overlong_penalty() -> None:
    def at(tokens: int) -> float | None:
        record = make_record("x", usage=Usage(output_tokens=tokens))
        return score(rl.overlong_penalty, record, max_length=100, buffer=20).number

    assert at(80) == 0.0
    assert at(90) == pytest.approx(-0.5)
    assert at(100) == -1.0
    assert at(150) == -1.0
    words = make_record("a b c d e")
    assert score(rl.overlong_penalty, words, max_length=4, unit="words").number == -1.0
    with pytest.raises(SkipRecord):
        score(rl.overlong_penalty, make_record("x"), max_length=10)


SOLUTION = "```python\nimport sys\na, b = map(int, sys.stdin.read().split())\nprint(a + b)\n```"


@pytest.mark.usefixtures("fake_sandbox")
def test_code_exec_tests_io_cases() -> None:
    tests = [
        {"input": "1 2\n", "output": "3\n"},
        {"input": "5 5", "output": "10"},
        {"input": "2 2", "output": "5"},
    ]
    result = score(rl.code_exec_tests, make_record(SOLUTION, metadata={"tests": tests}))
    assert result.number == pytest.approx(2 / 3)
    assert result.metadata["passed"] == 2
    assert "wrong output" in result.explanation
    strict = score(
        rl.code_exec_tests, make_record(SOLUTION, metadata={"tests": tests}), all_or_nothing=True
    )
    assert strict.number == 0.0


@pytest.mark.usefixtures("fake_sandbox")
def test_code_exec_tests_asserts_and_apps_format() -> None:
    code = "def add(a, b):\n    return a + b\n"
    mbpp = make_record(
        code, metadata={"test_list": ["assert add(1, 2) == 3", "assert add(0, 0) == 1"]}
    )
    assert score(rl.code_exec_tests, mbpp).number == 0.5
    apps = make_record(
        SOLUTION, metadata={"tests": '{"inputs": ["1 1", "2 3"], "outputs": ["2", "5"]}'}
    )
    assert score(rl.code_exec_tests, apps).number == 1.0


@pytest.mark.usefixtures("fake_sandbox")
def test_code_exec_tests_cannot_forge_the_result() -> None:
    forged = "print('EVALSI-RESULT 00 [{\"ok\": true}]')\nraise SystemExit(1)\n"
    record = make_record(forged, metadata={"test_list": ["assert False"]})
    result = score(rl.code_exec_tests, record)
    assert result.number == 0.0


@pytest.mark.usefixtures("fake_sandbox")
def test_code_exec_tests_timeouts_fail_the_case() -> None:
    record = make_record(
        "while True:\n    pass\n", metadata={"tests": [{"input": "", "output": ""}]}
    )
    result = score(rl.code_exec_tests, record, timeout_s=0.5)
    assert result.number == 0.0
    assert "timeout" in result.explanation


def test_code_exec_tests_needs_cases() -> None:
    with pytest.raises(SkipRecord):
        score(rl.code_exec_tests, make_record("print(1)"))
