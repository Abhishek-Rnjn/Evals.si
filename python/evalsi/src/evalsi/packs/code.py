"""The ``code`` pack: execute generated code against tests, in the sandbox.

``unit-tests`` runs the output (code, or the first fenced code block in it)
followed by the tests, HumanEval style, under the Evals.si sandbox: no
network, a private workspace, and resource limits. Passing means the program
exited 0. Timeouts and policy denials are the code's own failures; a missing
or broken sandbox is an infrastructure error and is never scored.
Use trials for pass@k.
"""

from __future__ import annotations

import re

from evalsi.evaluator import MetricSpec, Requirements, ScoreType, SkipRecord, evaluator
from evalsi.packs.core import _output_text
from evalsi.registry import Pack
from evalsi.sandbox import run_sandboxed
from evalsi.types import Record, Score

PASSED = ScoreType.PASSED
_FENCE = re.compile(r"```[ \t]*([\w+-]*)[^\n]*\n(.*?)```", re.DOTALL)


def extract_code(text: str, language: str = "python") -> str:
    """The first fenced block in ``language`` (or untagged), else the whole text."""
    blocks = _FENCE.findall(text)
    for tag, body in blocks:
        if tag.lower() in (language, "py" if language == "python" else language, ""):
            return str(body)
    return str(blocks[0][1]) if blocks else text


def _tests(record: Record) -> str:
    for key in ("test", "tests"):
        value = record.metadata.get(key)
        if isinstance(value, str) and value.strip():
            return value
    if record.reference is not None and record.reference.as_text().strip():
        return record.reference.as_text()
    raise SkipRecord("no tests: put them in the reference or metadata['test']")


def _tail(text: str, limit: int = 600) -> str:
    text = text.strip()
    return text if len(text) <= limit else "..." + text[-limit:]


@evaluator(
    name="builtin/unit-tests",
    version="1.0.0",
    description=(
        "Runs the generated code plus its tests in the sandbox; passes when the program "
        "exits 0. Tests come from the reference or metadata['test']; with "
        "metadata['entry_point'], check(<entry_point>) is called (HumanEval style)."
    ),
    requires=Requirements(output=True, sandbox=True),
    outputs=[MetricSpec("unit-tests", PASSED, higher_is_better=True)],
)
async def unit_tests(
    record: Record,
    *,
    timeout_s: float = 10.0,
    memory_mb: int = 512,
    interpreter: str = "python3",
    min_isolation: str = "confined",
) -> Score:
    tests = _tests(record)
    program = extract_code(_output_text(record)) + "\n\n" + tests + "\n"
    entry_point = record.metadata.get("entry_point")
    if isinstance(entry_point, str) and entry_point and "def check(" in tests:
        program += f"\ncheck({entry_point})\n"
    result = await run_sandboxed(
        [interpreter, "solution.py"],
        files={"solution.py": program},
        timeout_s=timeout_s,
        memory_mb=memory_mb,
        min_isolation=min_isolation,
    )
    if result.outcome == "timeout":
        explanation = f"timed out after {timeout_s}s"
    elif result.outcome == "denied":
        explanation = f"sandbox denied: {', '.join(result.denials)}; {_tail(result.stderr, 300)}"
    else:
        explanation = "" if result.ok else _tail(result.stderr or result.stdout)
    return Score(
        passed=result.ok,
        explanation=explanation,
        metadata={
            "outcome": result.outcome,
            "exit_code": result.exit_code,
            "duration_ms": result.duration_ms,
            "isolation": result.isolation,
        },
    )


@evaluator(
    name="builtin/python-syntax",
    version="1.0.0",
    description="Whether the generated Python compiles (nothing is executed, no sandbox needed).",
    requires=Requirements(output=True),
    outputs=[MetricSpec("python-syntax", PASSED, higher_is_better=True)],
)
def python_syntax(record: Record) -> Score:
    code = extract_code(_output_text(record))
    try:
        compile(code, "<output>", "exec", dont_inherit=True)
    except SyntaxError as exc:
        return Score(passed=False, explanation=f"line {exc.lineno}: {exc.msg}")
    return Score(passed=True)


PACK = Pack(
    name="code",
    description="Code: sandboxed unit tests (pass@k with trials) and syntax checks.",
    evaluators=[unit_tests, python_syntax],
)
