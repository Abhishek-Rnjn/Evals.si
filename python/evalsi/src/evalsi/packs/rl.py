"""The ``rl`` pack: verifiers built to serve as rewards.

These are ordinary evaluators, so they work in runs and online policies too,
but they are shaped for the training loop: they are cheap, deterministic,
and grade the final answer rather than the prose around it.

- ``format-check``: the completion matches a required layout (by default
  ``<think>...</think><answer>...</answer>``).
- ``math-equiv``: the final answer (``\\boxed{}``, ``<answer>``, ``####`` or
  "the answer is") equals the reference, numerically or symbolically.
- ``code-exec-tests``: the generated program runs against test cases in the
  sandbox; the score is the fraction that pass.
- ``overlong-penalty``: DAPO's soft penalty for completions near the length cap.

Served reward models and LLM judges come in through ``reward-model`` and the
``judge`` pack's ``llm-judge``.
"""

from __future__ import annotations

import ast
import asyncio
import json
import math
import os
import re
import secrets
from collections.abc import Callable
from fractions import Fraction
from typing import Any

import httpx

from evalsi.evaluator import MetricSpec, Requirements, ScoreType, SkipRecord, evaluator
from evalsi.packs.code import extract_code
from evalsi.packs.core import _output_text
from evalsi.registry import Pack
from evalsi.sandbox import run_sandboxed
from evalsi.types import Record, Score

PASSED = ScoreType.PASSED
NUMBER = ScoreType.NUMBER

DEFAULT_FORMAT = r"\s*<think>.*?</think>\s*<answer>.*?</answer>\s*"


@evaluator(
    name="builtin/format-check",
    version="1.0.0",
    description=(
        "The completion matches a required layout: a regex that must match the whole "
        "output (default: <think>...</think> then <answer>...</answer>), and optionally "
        "tags that must each appear exactly once."
    ),
    outputs=[MetricSpec("format-check", PASSED, higher_is_better=True)],
)
def format_check(
    record: Record, *, pattern: str = DEFAULT_FORMAT, tags: list[str] | None = None
) -> Score:
    text = _output_text(record)
    if pattern and re.fullmatch(pattern, text, flags=re.DOTALL) is None:
        return Score(passed=False, explanation="output does not match the required format")
    for tag in tags or []:
        opened, closed = text.count(f"<{tag}>"), text.count(f"</{tag}>")
        if opened != 1 or closed != 1:
            return Score(
                passed=False, explanation=f"<{tag}> appears {opened} times, </{tag}> {closed}"
            )
    return Score(passed=True)


# --- answer extraction -------------------------------------------------------

_ANSWER_TAG = re.compile(r"<answer>(.*?)</answer>", re.DOTALL)
_GSM8K = re.compile(r"####\s*(.+?)\s*$", re.MULTILINE)
_ANSWER_IS = re.compile(
    r"(?:final answer|the answer)\s*(?:is|:)\s*:?\s*(.+?)(?:\.\s*$|\n|$)", re.IGNORECASE
)
_LAST_NUMBER = re.compile(r"-?\d[\d,]*(?:\.\d+)?(?:[eE][-+]?\d+)?|-?\.\d+")


def _boxed(text: str) -> str | None:
    """The content of the last ``\\boxed{...}`` (or ``\\fbox{...}``), braces balanced."""
    start = max(text.rfind("\\boxed"), text.rfind("\\fbox"))
    if start < 0:
        return None
    brace = text.find("{", start)
    if brace < 0:
        # \boxed 42
        rest = text[start + len("\\boxed") :].strip().split()
        return rest[0].rstrip("$.") if rest else None
    depth = 0
    for i in range(brace, len(text)):
        if text[i] == "{":
            depth += 1
        elif text[i] == "}":
            depth -= 1
            if depth == 0:
                return text[brace + 1 : i]
    return None


def extract_answer(text: str, *, fallback: str = "last-number") -> str | None:
    """The final answer in ``text``: ``\\boxed{}``, then ``<answer>``, then
    ``#### x``, then "the answer is x", then (with ``fallback="last-number"``)
    the last number. ``None`` when nothing qualifies."""
    boxed = _boxed(text)
    if boxed is not None:
        return boxed.strip()
    tags = _ANSWER_TAG.findall(text)
    if tags:
        inner = str(tags[-1])
        return (_boxed(inner) or inner).strip()
    gsm = _GSM8K.findall(text)
    if gsm:
        return str(gsm[-1]).strip()
    stated = _ANSWER_IS.findall(text)
    if stated:
        return str(stated[-1]).strip().rstrip(".")
    if fallback == "last-number":
        numbers = _LAST_NUMBER.findall(text)
        if numbers:
            return str(numbers[-1])
    elif fallback == "text":
        return text.strip()
    return None


# --- answer comparison -------------------------------------------------------

_LATEX_NOISE = (
    "\\left",
    "\\right",
    "\\!",
    "\\,",
    "\\;",
    "\\:",
    "\\displaystyle",
    "\\mathrm",
    "\\textbf",
    "$",
)
_TEXT_WRAPPER = re.compile(r"\\(?:text|mbox|mathrm|operatorname)\{([^{}]*)\}")
_FRAC = re.compile(r"\\[dt]?frac\{([^{}]*)\}\{([^{}]*)\}")
_FRAC_SHORT = re.compile(r"\\[dt]?frac(\d)(\d)")
_SQRT = re.compile(r"\\sqrt\{([^{}]*)\}")
_UNITS = re.compile(
    r"\s*(?:\\?(?:text\{)?(?:dollars?|cents?|units?|degrees?|cm|m|km|kg|g|s|hours?|"
    r"minutes?|seconds?|inches|feet|ft|miles?|meters?)\}?)\s*$",
    re.IGNORECASE,
)


def normalize_answer(answer: str) -> str:
    """A canonical string for an answer: LaTeX noise, units, ``\\%`` and
    thousands separators removed, ``\\frac`` and ``\\sqrt`` made plain."""
    s = answer.strip()
    s = _TEXT_WRAPPER.sub(r"\1", s)
    for noise in _LATEX_NOISE:
        s = s.replace(noise, "")
    s = s.replace("\\%", "%").replace("^\\circ", "").replace("^{\\circ}", "").replace("°", "")
    s = s.replace("\\dfrac", "\\frac").replace("\\tfrac", "\\frac")
    s = _FRAC_SHORT.sub(r"\\frac{\1}{\2}", s)
    for _ in range(3):  # nested \frac{\frac{a}{b}}{c}
        s = _FRAC.sub(r"(\1)/(\2)", s)
    s = _SQRT.sub(r"sqrt(\1)", s)
    s = _UNITS.sub("", s)
    s = s.rstrip(".").strip()
    if re.fullmatch(r"-?\d{1,3}(,\d{3})+(\.\d+)?", s):
        s = s.replace(",", "")
    s = re.sub(r"\s+", "", s)
    if s.startswith("x=") or s.startswith("y="):
        s = s[2:]
    return s


def _parse_number(s: str) -> float | None:
    s = s.strip()
    percent = s.endswith("%")
    if percent:
        s = s[:-1]
    m = re.fullmatch(r"\(?(-?\d+)\)?/\(?(\d+)\)?", s)
    try:
        value = float(Fraction(int(m.group(1)), int(m.group(2)))) if m else float(s)
    except (ValueError, ZeroDivisionError):
        return None
    return value / 100 if percent else value


def _split_tuple(s: str) -> list[str] | None:
    if len(s) >= 2 and s[0] in "([{" and s[-1] in ")]}" and "," in s:
        return s[1:-1].split(",")
    return None


def _numbers_equal(a: float, b: float, rel_tol: float) -> bool:
    return math.isclose(a, b, rel_tol=rel_tol, abs_tol=rel_tol)


def _equal_plain(predicted: str, expected: str, rel_tol: float) -> bool:
    if predicted == expected:
        return True
    pa, pb = _parse_number(predicted), _parse_number(expected)
    if pa is not None and pb is not None:
        # 50% and 0.5 both count as the same answer, as does 50 when 50% is expected.
        if _numbers_equal(pa, pb, rel_tol):
            return True
        if predicted.endswith("%") != expected.endswith("%"):
            return _numbers_equal(pa * 100, pb, rel_tol) or _numbers_equal(pa, pb * 100, rel_tol)
        return False
    ta, tb = _split_tuple(predicted), _split_tuple(expected)
    if ta is not None and tb is not None and len(ta) == len(tb):
        return all(_equal_plain(x, y, rel_tol) for x, y in zip(ta, tb, strict=True))
    return predicted.casefold() == expected.casefold()


# What a math answer may use once sympy's tokenizer has rewritten it: number
# and symbol constructors, mathematical functions and constants. Anything else
# (attribute access, subscripts, other calls, lambdas) is refused before
# evaluation: answers are untrusted model output.
_SYMPY_CONSTRUCTORS = frozenset({"Integer", "Float", "Rational", "Symbol"})
_SYMPY_FUNCTIONS = frozenset(
    {
        "sqrt", "root", "exp", "log", "ln", "Abs", "floor", "ceiling",
        "sin", "cos", "tan", "cot", "sec", "csc", "asin", "acos", "atan",
        "sinh", "cosh", "tanh",
    }
)  # fmt: skip
_SYMPY_CONSTANTS = frozenset({"pi", "E", "I", "oo"})
_SYMBOL_NAME = re.compile(r"[A-Za-z][A-Za-z0-9_]{0,15}")
# Exponents bigger than this are refused, so an answer cannot ask for a
# number too large to compute (10**10**10).
_MAX_EXPONENT = 1000


class _UnsafeExpression(ValueError):
    pass


def _small(values: list[Any]) -> bool:
    try:
        return all(abs(float(v)) <= _MAX_EXPONENT for v in values)
    except (OverflowError, ValueError):
        return False


def _check_sympy_code(tree: ast.Expression) -> None:
    """Refuses code that is not plain arithmetic over the allowed names."""

    def literal(node: ast.expr) -> bool:
        return isinstance(node, ast.Constant) and type(node.value) in (int, float, str)

    def check(node: ast.expr, in_exponent: bool) -> None:
        match node:
            case ast.BinOp(op=ast.Pow(), left=left, right=right):
                if in_exponent:
                    raise _UnsafeExpression("a power in an exponent")
                check(left, in_exponent)
                check(right, True)
            case ast.BinOp(
                op=ast.Add() | ast.Sub() | ast.Mult() | ast.Div(), left=left, right=right
            ):
                check(left, in_exponent)
                check(right, in_exponent)
            case ast.UnaryOp(op=ast.UAdd() | ast.USub(), operand=operand):
                check(operand, in_exponent)
            case ast.Tuple(elts=elts):
                for elt in elts:
                    check(elt, in_exponent)
            case ast.Name(id=name) if name in _SYMPY_CONSTANTS:
                pass
            case ast.Call(func=ast.Name(id=name), args=args, keywords=[]) if (
                name in _SYMPY_CONSTRUCTORS
            ):
                if not args or not all(literal(a) for a in args):
                    raise _UnsafeExpression(f"{name} of a non-literal")
                values = [a.value for a in args if isinstance(a, ast.Constant)]
                if name == "Symbol" and not (
                    len(values) == 1
                    and isinstance(values[0], str)
                    and _SYMBOL_NAME.fullmatch(values[0])
                ):
                    raise _UnsafeExpression("a symbol name")
                if in_exponent and name != "Symbol" and not _small(values):
                    raise _UnsafeExpression("an exponent too large")
            case ast.Call(func=ast.Name(id=name), args=args, keywords=[]) if (
                name in _SYMPY_FUNCTIONS
            ):
                for arg in args:
                    check(arg, in_exponent)
            case _:
                raise _UnsafeExpression(type(node).__name__)

    check(tree.body, False)


def _equal_sympy(predicted: str, expected: str) -> bool | None:
    """Symbolic equivalence with sympy, when it is installed. ``None`` means
    sympy could not decide (or is missing).

    Answers are untrusted: they are tokenized by sympy without being run, the
    resulting code is checked against an allowlist of arithmetic, numbers,
    symbols and mathematical functions, and only then evaluated, with no
    builtins. Anything else counts as undecided."""
    if max(len(predicted), len(expected)) > 200:
        return None
    try:
        import sympy
        from sympy.parsing.sympy_parser import (
            convert_xor,
            implicit_multiplication_application,
            standard_transformations,
            stringify_expr,
        )
    except ImportError:
        return None
    transformations = (
        *standard_transformations,
        implicit_multiplication_application,
        convert_xor,
    )
    names: dict[str, Any] = {
        name: getattr(sympy, name)
        for name in (*_SYMPY_CONSTRUCTORS, *_SYMPY_FUNCTIONS - {"ln"}, *_SYMPY_CONSTANTS)
    }
    names["ln"] = sympy.log

    def parse(s: str) -> Any:
        s = s.replace("\\pi", "pi").replace("\\cdot", "*").replace("\\times", "*")
        s = s.replace("{", "(").replace("}", ")").replace("\\", "")
        # stringify_expr only tokenizes and rewrites; nothing runs yet.
        code = stringify_expr(s, {}, dict(names), transformations)
        tree = ast.parse(code.strip(), mode="eval")
        _check_sympy_code(tree)
        return eval(compile(tree, "<answer>", "eval"), {"__builtins__": {}}, dict(names))

    try:
        difference = sympy.simplify(parse(predicted) - parse(expected))
        return bool(difference == 0)
    except Exception:  # sympy raises many types on malformed input
        return None


def _equal_math_verify(predicted: str, expected: str) -> bool | None:
    try:
        from math_verify import parse, verify
    except ImportError:
        return None
    try:
        return bool(verify(parse(f"${expected}$"), parse(f"${predicted}$")))
    except Exception:
        return None


def answers_equal(
    predicted: str, expected: str, *, rel_tol: float = 1e-6, backend: str = "auto"
) -> bool:
    """Whether two final answers are the same. ``backend``: ``plain`` (string
    and numeric rules only), ``sympy``, ``math-verify``, or ``auto`` (plain,
    then math-verify, then sympy, using whichever is installed)."""
    a, b = normalize_answer(predicted), normalize_answer(expected)
    if _equal_plain(a, b, rel_tol):
        return True
    checks: list[Callable[[], bool | None]] = []
    if backend in ("auto", "math-verify"):
        checks.append(lambda: _equal_math_verify(predicted, expected))
    if backend in ("auto", "sympy"):
        checks.append(lambda: _equal_sympy(a, b))
    for check in checks:
        verdict = check()
        if verdict is not None:
            return verdict
    return False


@evaluator(
    name="builtin/math-equiv",
    version="1.0.0",
    description=(
        "The final answer (\\boxed{}, <answer>, '#### x', 'the answer is x', else the "
        "last number) equals the reference's, as numbers, tuples or (with sympy or "
        "math-verify installed) symbolically."
    ),
    requires=Requirements(reference=True),
    outputs=[MetricSpec("math-equiv", PASSED, higher_is_better=True)],
)
def math_equiv(
    record: Record,
    *,
    rel_tol: float = 1e-6,
    backend: str = "auto",
    fallback: str = "last-number",
) -> Score:
    if backend not in ("auto", "plain", "sympy", "math-verify"):
        raise ValueError("backend must be auto, plain, sympy or math-verify")
    assert record.reference is not None
    reference = record.reference.as_text()
    expected = extract_answer(reference, fallback="text")
    if not expected:
        raise SkipRecord("reference has no answer")
    predicted = extract_answer(_output_text(record), fallback=fallback)
    if predicted is None:
        return Score(passed=False, explanation="no final answer found")
    passed = answers_equal(predicted, expected, rel_tol=rel_tol, backend=backend)
    return Score(passed=passed, metadata={"predicted": predicted, "expected": expected})


# --- sandboxed test cases ------------------------------------------------------

# Runs inside the sandbox. Each case runs the solution in its own process, so a
# crash or a hang fails that case only. The result line carries a nonce read
# from stdin, which the solution's processes never see, so the program under
# test cannot print a forged result.
_RUNNER = r"""
import json, subprocess, sys
nonce = sys.stdin.readline().strip()
cases = json.load(open("cases.json"))
compare, timeout, stop = cases["compare"], cases["timeout_s"], cases["stop_on_failure"]
interp = cases["interpreter"]
def same(got, want):
    if compare == "exact":
        return got == want
    if compare == "lines":
        return got.rstrip().splitlines() == want.rstrip().splitlines()
    a, b = got.split(), want.split()
    if compare == "float" and len(a) == len(b):
        try:
            return all(abs(float(x) - float(y)) <= 1e-6 * max(1.0, abs(float(y)))
                       for x, y in zip(a, b))
        except ValueError:
            pass
    return a == b
results = []
for i, case in enumerate(cases["cases"]):
    if "assert" in case:
        argv, stdin = [interp, "-c", open("solution.py").read() + "\n" + case["assert"]], ""
    else:
        argv, stdin = [interp, "solution.py"], case.get("input", "")
    try:
        feed = {"input": stdin} if stdin else {"stdin": subprocess.DEVNULL}
        p = subprocess.run(argv, capture_output=True, text=True, timeout=timeout, **feed)
        if "assert" in case:
            ok, why = p.returncode == 0, (p.stderr or "")[-300:]
        else:
            ok = p.returncode == 0 and same(p.stdout, case.get("output", ""))
            why = (p.stderr or "")[-300:] if p.returncode else "wrong output"
    except subprocess.TimeoutExpired:
        ok, why = False, "timeout"
    results.append({"ok": ok, "why": "" if ok else why})
    if stop and not ok:
        break
print("EVALSI-RESULT " + nonce + " " + json.dumps(results), flush=True)
"""


def _cases(record: Record) -> list[dict[str, str]]:
    """Test cases from the record: ``metadata['tests']`` as input/output pairs,
    ``metadata['test_list']`` as assert statements (MBPP), or
    ``metadata['inputs']`` with ``metadata['outputs']`` (APPS, CodeContests)."""
    meta = record.metadata
    tests = meta.get("tests")
    if isinstance(tests, str):
        try:
            tests = json.loads(tests)
        except json.JSONDecodeError:
            tests = None
    if isinstance(tests, dict) and "inputs" in tests:
        meta = {**meta, "inputs": tests["inputs"], "outputs": tests.get("outputs", [])}
        tests = None
    if isinstance(tests, list) and tests and all(isinstance(t, dict) for t in tests):
        return [
            {"input": str(t.get("input", "")), "output": str(t.get("output", ""))} for t in tests
        ]
    asserts = meta.get("test_list")
    if isinstance(asserts, list) and asserts:
        setup = str(meta.get("test_setup_code") or "")
        return [{"assert": (setup + "\n" + str(a)).strip()} for a in asserts]
    inputs, outputs = meta.get("inputs"), meta.get("outputs")
    if isinstance(inputs, list) and isinstance(outputs, list) and len(inputs) == len(outputs):
        return [{"input": str(i), "output": str(o)} for i, o in zip(inputs, outputs, strict=True)]
    script = meta.get("test")
    if not isinstance(script, str) and record.reference is not None:
        script = record.reference.as_text()
    if isinstance(script, str) and script.strip():
        entry = meta.get("entry_point")
        if isinstance(entry, str) and entry and "def check(" in script:
            script += f"\ncheck({entry})\n"
        return [{"assert": script}]
    raise SkipRecord(
        "no test cases: set metadata 'tests' (input/output pairs), 'test_list' "
        "(asserts), 'inputs'/'outputs', or a test script"
    )


@evaluator(
    name="builtin/code-exec-tests",
    version="1.0.0",
    description=(
        "Runs the generated program against test cases in the sandbox, each in its own "
        "process, and scores the fraction that pass (1 or 0 with all_or_nothing). Cases "
        "are stdin/stdout pairs (metadata 'tests', or 'inputs'/'outputs') or asserts "
        "(metadata 'test_list', or a test script)."
    ),
    requires=Requirements(output=True, sandbox=True),
    outputs=[MetricSpec("code-exec-tests", NUMBER, min=0, max=1, higher_is_better=True)],
)
async def code_exec_tests(
    record: Record,
    *,
    timeout_s: float = 5.0,
    memory_mb: int = 1024,
    compare: str = "tokens",
    all_or_nothing: bool = False,
    max_cases: int = 0,
    interpreter: str = "python3",
    min_isolation: str = "confined",
) -> Score:
    if compare not in ("tokens", "lines", "exact", "float"):
        raise ValueError("compare must be tokens, lines, exact or float")
    cases = _cases(record)
    if max_cases > 0:
        cases = cases[:max_cases]
    nonce = secrets.token_hex(16)
    spec = {
        "cases": cases,
        "compare": compare,
        "timeout_s": timeout_s,
        "stop_on_failure": all_or_nothing,
        "interpreter": interpreter,
    }
    result = await run_sandboxed(
        [interpreter, "_evalsi_runner.py"],
        files={
            "solution.py": extract_code(_output_text(record)),
            "cases.json": json.dumps(spec),
            "_evalsi_runner.py": _RUNNER,
        },
        stdin=nonce + "\n",
        timeout_s=timeout_s * len(cases) + 10,
        memory_mb=memory_mb,
        min_isolation=min_isolation,
    )
    marker = f"EVALSI-RESULT {nonce} "
    lines = [line for line in result.stdout.splitlines() if line.startswith(marker)]
    if not lines:
        why = "timed out" if result.outcome == "timeout" else (result.stderr.strip()[-300:])
        return Score(
            number=0.0,
            explanation=f"the test runner did not finish: {why}",
            metadata={"outcome": result.outcome, "cases": len(cases), "passed": 0},
        )
    outcomes = json.loads(lines[-1][len(marker) :])
    passed = sum(1 for o in outcomes if o["ok"])
    value = passed / len(cases)
    if all_or_nothing:
        value = 1.0 if passed == len(cases) else 0.0
    first_failure = next((o["why"] for o in outcomes if not o["ok"]), "")
    return Score(
        number=value,
        explanation=f"{passed}/{len(cases)} cases passed"
        + (f"; {first_failure}" if first_failure else ""),
        metadata={
            "cases": len(cases),
            "passed": passed,
            "duration_ms": result.duration_ms,
            "isolation": result.isolation,
        },
    )


@evaluator(
    name="builtin/overlong-penalty",
    version="1.0.0",
    description=(
        "DAPO's soft overlong penalty: 0 up to max_length - buffer, falling linearly to "
        "-1 at max_length (and -1 beyond). Length is in characters, whitespace-separated "
        "words, or output tokens from the record's usage."
    ),
    outputs=[MetricSpec("overlong-penalty", NUMBER, min=-1, max=0, higher_is_better=True)],
)
def overlong_penalty(
    record: Record, *, max_length: int, buffer: int = 0, unit: str = "tokens"
) -> Score:
    if unit not in ("tokens", "words", "chars"):
        raise ValueError("unit must be tokens, words or chars")
    if buffer < 0 or buffer >= max_length:
        raise ValueError("buffer must be in [0, max_length)")
    text = _output_text(record)
    if unit == "tokens":
        if record.usage is None or record.usage.output_tokens is None:
            raise SkipRecord("record has no output token count (usage.output_tokens)")
        length = record.usage.output_tokens
    else:
        length = len(text.split()) if unit == "words" else len(text)
    soft = max_length - buffer
    if length <= soft:
        value = 0.0
    elif length >= max_length or buffer == 0:
        value = -1.0
    else:
        value = (soft - length) / buffer
    return Score(number=value, metadata={"length": length})


class _Batcher:
    """Collects reward-model requests made at about the same time (within
    ``wait_s``, up to ``max_batch``) into one HTTP call."""

    def __init__(
        self, url: str, api: str, model: str, headers: dict[str, str], timeout_s: float
    ) -> None:
        self.url, self.api, self.model, self.headers = url, api, model, headers
        self.timeout_s = timeout_s
        self.pending: list[tuple[str, asyncio.Future[float]]] = []
        self.flusher: asyncio.Task[None] | None = None
        self.client = httpx.AsyncClient(timeout=timeout_s)
        self.calls = 0
        # Sends in flight; held so they are not garbage-collected mid-call.
        self.sending: set[asyncio.Task[None]] = set()

    async def score(self, text: str, *, max_batch: int, wait_s: float) -> float:
        future: asyncio.Future[float] = asyncio.get_running_loop().create_future()
        self.pending.append((text, future))
        if len(self.pending) >= max_batch:
            self._flush_now()
        elif self.flusher is None:
            self.flusher = asyncio.create_task(self._flush_later(wait_s))
        return await future

    async def _flush_later(self, wait_s: float) -> None:
        await asyncio.sleep(wait_s)
        self.flusher = None
        self._flush_now()

    def _flush_now(self) -> None:
        batch, self.pending = self.pending, []
        if self.flusher is not None:
            self.flusher.cancel()
            self.flusher = None
        if batch:
            task = asyncio.create_task(self._send(batch))
            self.sending.add(task)
            task.add_done_callback(self.sending.discard)

    async def _send(self, batch: list[tuple[str, asyncio.Future[float]]]) -> None:
        texts = [t for t, _ in batch]
        if self.api == "openrlhf":
            body: dict[str, Any] = {"query": texts}
        else:
            body = {"input": texts} | ({"model": self.model} if self.model else {})
        try:
            self.calls += 1
            response = await self.client.post(self.url, json=body, headers=self.headers)
            response.raise_for_status()
            values = _reward_values(response.json(), self.api, len(texts))
        except Exception as exc:  # every waiter sees the failure
            for _, future in batch:
                if not future.done():
                    future.set_exception(exc)
            return
        for (_, future), value in zip(batch, values, strict=True):
            if not future.done():
                future.set_result(value)


def _reward_values(data: Any, api: str, n: int) -> list[float]:
    if api == "openrlhf":
        rewards = data.get("rewards", data.get("reward"))
        values = rewards if isinstance(rewards, list) else [rewards]
    else:
        items = sorted(data["data"], key=lambda d: int(d.get("index", 0)))
        values = []
        for item in items:
            value = item["data"]
            while isinstance(value, list):
                value = value[0]
            values.append(value)
    if len(values) != n:
        raise ValueError(f"the reward model returned {len(values)} rewards for {n} inputs")
    return [float(v) for v in values]


_batchers: dict[tuple[Any, ...], _Batcher] = {}


def _batcher(url: str, api: str, model: str, api_key_env: str, timeout_s: float) -> _Batcher:
    loop = asyncio.get_running_loop()
    key = (id(loop), url, api, model, api_key_env, timeout_s)
    found = _batchers.get(key)
    if found is None or found.client.is_closed:
        for stale in [k for k in _batchers if k[0] != id(loop)]:
            del _batchers[stale]
        headers = {}
        if api_key_env:
            headers["Authorization"] = f"Bearer {os.environ.get(api_key_env, '')}"
        found = _batchers[key] = _Batcher(url, api, model, headers, timeout_s)
    return found


@evaluator(
    name="builtin/reward-model",
    version="1.1.0",
    description=(
        "Scores the completion with a served reward model. api: 'openrlhf' posts "
        '{"query": [prompt + completion, ...]} and reads "rewards"; \'vllm\' posts to '
        "vLLM's /pooling endpoint. Requests made together (within wait_ms, up to "
        "max_batch) go in one call."
    ),
    requires=Requirements(input=True, output=True),
    outputs=[MetricSpec("reward-model", NUMBER)],
)
async def reward_model(
    record: Record,
    *,
    url: str,
    api: str = "openrlhf",
    model: str = "",
    timeout_s: float = 60.0,
    api_key_env: str = "",
    max_batch: int = 64,
    wait_ms: float = 5.0,
) -> Score:
    if api not in ("openrlhf", "vllm"):
        raise ValueError("api must be openrlhf or vllm")
    if max_batch < 1:
        raise ValueError("max_batch must be at least 1")
    assert record.input is not None
    text = record.input.as_text() + _output_text(record)
    batcher = _batcher(url, api, model, api_key_env, timeout_s)
    value = await batcher.score(text, max_batch=max_batch, wait_s=wait_ms / 1000)
    return Score(number=value)


PACK = Pack(
    name="rl",
    description=(
        "Verifiers for rewards: format checks, math answer equivalence, sandboxed code "
        "tests, overlong penalties and served reward models."
    ),
    evaluators=[format_check, math_equiv, code_exec_tests, overlong_penalty, reward_model],
)
