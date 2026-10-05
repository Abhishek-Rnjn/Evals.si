"""A tiny SWE-bench-format dataset, for tests and smoke runs without the real
images: a Python repository with two reported bugs, each with a test patch,
a gold patch and an eval script in the official shape (markers, test command,
reset). The repository is placed and committed by the instance's
``evalsi_environment``, so it runs in any sandbox with python3, git and bash.
"""

from __future__ import annotations

import json
import subprocess
import tempfile
from pathlib import Path
from typing import Any

BASE = {
    "calc.py": (
        "def add(a, b):\n    return a - b\n\n\n"
        "def sub(a, b):\n    return a - b\n\n\n"
        "def mul(a, b):\n    return a + b\n"
    ),
    "tests/__init__.py": "",
    "tests/test_calc.py": (
        "import unittest\n\nfrom calc import sub\n\n\n"
        "class CalcTest(unittest.TestCase):\n"
        "    def test_sub(self):\n        self.assertEqual(sub(5, 3), 2)\n"
    ),
}

BUGS: dict[str, dict[str, Any]] = {
    "evalsi__calc-1": {
        "problem": "add(2, 3) returns -1; it should return 5.",
        "fix": ("    return a - b\n\n\ndef sub", "    return a + b\n\n\ndef sub"),
        "test": (
            "    def test_add(self):\n"
            "        from calc import add\n"
            "        self.assertEqual(add(2, 3), 5)\n"
        ),
        "f2p": "test_add (tests.test_calc.CalcTest.test_add)",
    },
    "evalsi__calc-2": {
        "problem": "mul(2, 3) returns 5; it should return 6.",
        "fix": ("    return a + b\n", "    return a * b\n"),
        "test": (
            "    def test_mul(self):\n"
            "        from calc import mul\n"
            "        self.assertEqual(mul(2, 3), 6)\n"
        ),
        "f2p": "test_mul (tests.test_calc.CalcTest.test_mul)",
    },
}

SETUP = (
    "git init -q && git add -A && "
    "git -c user.email=fixture@evals.si -c user.name=fixture commit -qm base"
)


def _diff(files: dict[str, str], changed: dict[str, str]) -> str:
    with tempfile.TemporaryDirectory() as d:
        root = Path(d)
        for name, text in files.items():
            (root / name).parent.mkdir(parents=True, exist_ok=True)
            (root / name).write_text(text)
        subprocess.run(["sh", "-c", SETUP], cwd=root, check=True)
        for name, text in changed.items():
            (root / name).write_text(text)
        out = subprocess.run(["git", "diff"], cwd=root, check=True, capture_output=True, text=True)
        return out.stdout


def instances() -> list[dict[str, Any]]:
    out = []
    for iid, bug in BUGS.items():
        test_file = BASE["tests/test_calc.py"] + "\n" + bug["test"]
        test_patch = _diff(BASE, {"tests/test_calc.py": test_file})
        old, new = bug["fix"]
        gold = _diff(BASE, {"calc.py": BASE["calc.py"].replace(old, new, 1)})
        script = "\n".join(
            [
                "#!/bin/bash",
                "set -uxo pipefail",
                "git checkout -q HEAD -- tests/test_calc.py",
                "git apply -v - <<'EOF_114329324912'",
                test_patch.rstrip("\n"),
                "EOF_114329324912",
                ": '>>>>> Start Test Output'",
                "python3 -m unittest -v tests.test_calc",
                ": '>>>>> End Test Output'",
                "git checkout -q HEAD -- tests/test_calc.py",
            ]
        )
        out.append(
            {
                "instance_id": iid,
                "repo": "evalsi/calc",
                "version": "0.1",
                "base_commit": "base",
                "problem_statement": bug["problem"],
                "patch": gold,
                "test_patch": test_patch,
                "image": "none",
                "eval_script": script + "\n",
                "log_parser": "parse_log_django",
                "eval_type": "pass_and_fail",
                "FAIL_TO_PASS": json.dumps([bug["f2p"]]),
                "PASS_TO_PASS": json.dumps(["test_sub (tests.test_calc.CalcTest.test_sub)"]),
                "evalsi_environment": {"files": BASE, "setup": [SETUP]},
            }
        )
    return out


def write(path: str | Path) -> Path:
    path = Path(path)
    path.write_text("".join(json.dumps(i) + "\n" for i in instances()))
    return path


if __name__ == "__main__":  # pragma: no cover
    import sys

    write(sys.argv[1] if len(sys.argv) > 1 else "swebench-fixture.jsonl")
