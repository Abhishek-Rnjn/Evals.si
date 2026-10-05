"""Environment checkers: grade the end state of a task in its sandbox.

After the agent finishes, the checker's files (hidden tests the agent never
saw) are copied in and its command runs in the same sandbox. Parsers:

- ``exit-code`` (default): exit 0 passes.
- ``json``: the last non-empty stdout line is
  ``{"passed": bool, "score": number, "tests": {...}, "details": str}``.
- ``junit:<path>``: a JUnit XML report; passes when no test failed or erred.
- ``module:function``: a Python function ``(stdout, stderr, exit_code, task)``
  returning a ``TaskCheck``, a mapping like the JSON form, or a bool.

A checker that cannot run (timeout, broken sandbox, unparsable output) yields
a check with ``error`` set: the task is reported as an error, not a failure.
"""

from __future__ import annotations

import importlib
import json
from collections.abc import Mapping
from typing import Any
from xml.etree import ElementTree

from evalsi.sandbox import SandboxError
from evalsi.types import TaskCheck
from evalsi_harness.environment import TaskEnvironment
from evalsi_harness.task import CheckerConfig, Task


def _tail(text: str, limit: int = 2000) -> str:
    text = text.strip()
    return text if len(text) <= limit else "..." + text[-limit:]


def _from_mapping(data: Mapping[str, Any]) -> TaskCheck:
    if "passed" not in data:
        raise ValueError("checker output has no 'passed'")
    return TaskCheck.from_dict(data)


def parse_json(stdout: str) -> TaskCheck:
    lines = [line for line in stdout.splitlines() if line.strip()]
    if not lines:
        raise ValueError("the checker printed nothing")
    data = json.loads(lines[-1])
    if not isinstance(data, Mapping):
        raise ValueError("the checker's last line is not a JSON object")
    return _from_mapping(data)


def parse_junit(xml: bytes) -> TaskCheck:
    root = ElementTree.fromstring(xml)
    tests: dict[str, str] = {}
    for case in root.iter("testcase"):
        name = "::".join(p for p in (case.get("classname"), case.get("name")) if p)
        if case.find("failure") is not None:
            tests[name] = "failed"
        elif case.find("error") is not None:
            tests[name] = "error"
        elif case.find("skipped") is not None:
            tests[name] = "skipped"
        else:
            tests[name] = "passed"
    ran = [s for s in tests.values() if s != "skipped"]
    passed = sum(s == "passed" for s in ran)
    return TaskCheck(
        passed=bool(ran) and passed == len(ran),
        score=passed / len(ran) if ran else None,
        details=f"{passed} of {len(ran)} tests passed",
        tests=tests,
    )


def _python_parser(ref: str) -> Any:
    module, _, name = ref.partition(":")
    if not module or not name:
        raise ValueError(f"checker parser {ref!r} must be module:function")
    return getattr(importlib.import_module(module), name)


async def run_checker(env: TaskEnvironment, checker: CheckerConfig, task: Task) -> TaskCheck:
    try:
        if checker.files:
            await env.sandbox.write_files(dict(checker.files))
        result = await env.sandbox.exec(
            checker.command, timeout_s=checker.timeout_s, env=env.config.env
        )
    except SandboxError as exc:
        return TaskCheck(passed=False, error=f"checker could not run: {exc}")
    if result.outcome == "timeout":
        return TaskCheck(passed=False, error=f"checker timed out after {checker.timeout_s:g}s")
    parser = checker.parser or "exit-code"
    try:
        if parser == "exit-code":
            check = TaskCheck(
                passed=result.ok,
                details="" if result.ok else _tail(result.stdout + "\n" + result.stderr),
            )
        elif parser == "json":
            check = parse_json(result.stdout)
        elif parser.startswith("junit:"):
            path = parser.removeprefix("junit:")
            files = await env.sandbox.read_files([path])
            if path not in files:
                return TaskCheck(passed=False, error=f"the checker wrote no JUnit report at {path}")
            check = parse_junit(files[path])
        else:
            value = _python_parser(parser)(result.stdout, result.stderr, result.exit_code, task)
            if isinstance(value, TaskCheck):
                check = value
            elif isinstance(value, Mapping):
                check = _from_mapping(value)
            else:
                check = TaskCheck(passed=bool(value))
    except (ValueError, ElementTree.ParseError, ImportError, AttributeError) as exc:
        return TaskCheck(
            passed=False,
            error=f"could not read the checker's result ({parser}): {exc}",
            details=_tail(result.stdout + "\n" + result.stderr, 800),
        )
    return check
