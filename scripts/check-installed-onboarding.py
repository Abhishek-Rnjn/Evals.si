#!/usr/bin/env python3
"""Exercise a built SDK wheel in a fresh environment outside the source tree."""

from __future__ import annotations

import argparse
import html
import json
import os
import subprocess
import tempfile
import venv
from pathlib import Path


def invoke(
    command: list[str],
    *,
    cwd: Path,
    env: dict[str, str],
    expected: int = 0,
    timeout: int = 180,
) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(
        command, cwd=cwd, env=env, capture_output=True, text=True, timeout=timeout, check=False
    )
    if result.returncode != expected:
        raise RuntimeError(
            f"{Path(command[0]).name} returned {result.returncode}; expected {expected}\n"
            f"{result.stdout[-4000:]}\n{result.stderr[-4000:]}"
        )
    return result


def verify(wheel: Path) -> None:
    env = {
        key: value
        for key, value in os.environ.items()
        if key not in {"PYTHONPATH", "PYTHONHOME", "VIRTUAL_ENV"}
        and not key.startswith(("EVALSI", "ANTHROPIC_", "OPENAI_"))
    }
    with tempfile.TemporaryDirectory(prefix="evalsi-installed-") as directory:
        root = Path(directory)
        venv.create(root / "venv", with_pip=True)
        scripts = root / "venv" / ("Scripts" if os.name == "nt" else "bin")
        python = str(scripts / ("python.exe" if os.name == "nt" else "python"))
        cli = str(scripts / ("evalsi.exe" if os.name == "nt" else "evalsi"))
        env["PATH"] = str(scripts) + os.pathsep + env.get("PATH", "")
        env["EVALSID"] = str(root / "no-server-is-required")
        invoke([python, "-I", "-c", "import evalsi"], cwd=root, env=env, expected=1)
        invoke(
            [python, "-m", "pip", "install", "--disable-pip-version-check", str(wheel)],
            cwd=root,
            env=env,
            timeout=600,
        )
        invoke(
            [
                python,
                "-I",
                "-c",
                "import importlib.util, pathlib, sys, evalsi; "
                "assert pathlib.Path(sys.prefix).resolve() in "
                "pathlib.Path(evalsi.__file__).resolve().parents; "
                "assert importlib.util.find_spec('evalsi_harness') is None; "
                "assert importlib.util.find_spec('grpc') is None",
            ],
            cwd=root,
            env=env,
        )
        doctor = invoke([cli, "doctor", "--format", "json"], cwd=root, env=env)
        readiness = json.loads(doctor.stdout)
        assert readiness["ready"] is True
        assert {check["name"] for check in readiness["checks"]} == {"python", "evalsi"}
        scoring_controls(cli, root, env)
        application_controls(python, cli, root, env)
        custom_application_controls(python, cli, root, env)
        print("PASS: fresh-wheel onboarding without source imports, harness, gRPC or evalsid")


def scoring_controls(cli: str, root: Path, env: dict[str, str]) -> None:
    records = [
        {"id": "correct", "input": "Return window?", "output": "30 days", "reference": "30 days"},
        {"id": "regression", "input": "Return window?", "output": "7 days", "reference": "30 days"},
    ]
    (root / "cases.jsonl").write_text(
        "\n".join(json.dumps(record) for record in records) + "\n", encoding="utf-8"
    )
    invoke(
        [
            cli,
            "eval",
            "--data",
            "cases.jsonl",
            "--evaluators",
            "exact-match",
            "--output",
            "results.json",
        ],
        cwd=root,
        env=env,
    )
    result = json.loads((root / "results.json").read_text(encoding="utf-8"))
    assert len(result["records"]) == 2
    assert len(result["results"]) == 2
    assert result["summaries"][0]["mean"] == 0.5
    assert result["summaries"][0]["errors"] == 0
    invoke([cli, "report", "results.json", "-o", "report.html"], cwd=root, env=env)
    assert "regression" in (root / "report.html").read_text(encoding="utf-8")
    (root / "invalid.jsonl").write_text("not valid JSON\n", encoding="utf-8")
    failure = invoke(
        [
            cli,
            "eval",
            "--data",
            "invalid.jsonl",
            "--evaluators",
            "exact-match",
            "--output",
            "invalid-results.json",
        ],
        cwd=root,
        env=env,
        expected=1,
    )
    assert "error:" in failure.stderr
    assert "Traceback" not in failure.stderr
    assert not (root / "invalid-results.json").exists()
    print("PASS: scoped doctor, recorded scoring, failed check, report and input error")


def application_controls(python: str, cli: str, root: Path, env: dict[str, str]) -> None:
    invoke([cli, "init", "app-evals", "--template", "application"], cwd=root, env=env)
    project = root / "app-evals"
    invoke([python, "evaluate.py", "--limit", "1", "--output", "first.json"], cwd=project, env=env)
    first = json.loads((project / "first.json").read_text(encoding="utf-8"))
    assert len(first["records"]) == len(first["results"]) == 1
    assert first["summaries"][0]["mean"] == 1
    invoke([python, "evaluate.py", "--output", "results.json"], cwd=project, env=env)
    result = json.loads((project / "results.json").read_text(encoding="utf-8"))
    assert len(result["records"]) == len(result["results"]) == 3
    assert result["summaries"][0]["mean"] == 1
    invoke(
        [python, "evaluate.py", "--callable", "application:regressed", "--output", "bad.json"],
        cwd=project,
        env=env,
        expected=3,
    )
    regressed = json.loads((project / "bad.json").read_text(encoding="utf-8"))
    assert regressed["summaries"][0]["mean"] == 2 / 3
    case = next(record for record in regressed["records"] if record["id"] == "japan")
    hint = case["metadata"]["evalsi_application_reproduction"]
    assert hint == {
        "version": 1,
        "callable": "application:regressed",
        "cases": "cases.jsonl",
        "case": "japan",
    }
    argv = [
        "python",
        "evaluate.py",
        "--callable",
        hint["callable"],
        "--cases",
        hint["cases"],
        "--case",
        hint["case"],
    ]
    invoke(
        [python, *argv[1:], "--output", "one-case.json"],
        cwd=project,
        env=env,
        expected=3,
    )
    isolated = json.loads((project / "one-case.json").read_text(encoding="utf-8"))
    assert len(isolated["records"]) == 1
    assert isolated["summaries"][0]["mean"] == 0
    invoke([cli, "report", "bad.json", "-o", "report.html"], cwd=project, env=env)
    rendered = html.unescape((project / "report.html").read_text(encoding="utf-8"))
    assert json.dumps(argv, separators=(",", ":")) in "".join(rendered.split())
    failure = invoke(
        [python, "evaluate.py", "--callable", "no_such_application:answer"],
        cwd=project,
        env=env,
        expected=1,
    )
    assert "Traceback" not in failure.stderr
    assert "error:" in failure.stderr.lower()
    print("PASS: first case, application success, regression, exact case reproduction and errors")


def custom_application_controls(python: str, cli: str, root: Path, env: dict[str, str]) -> None:
    invoke(
        [
            cli,
            "init",
            "custom-evals",
            "--template",
            "application",
            "--callable",
            "customer_app:answer",
        ],
        cwd=root,
        env=env,
    )
    project = root / "custom-evals"
    (project / "customer_app.py").write_text(
        "from pathlib import Path\n\n"
        "def answer(prompt: str) -> str:\n"
        "    with Path('calls.txt').open('a', encoding='utf-8') as calls:\n"
        "        calls.write(prompt + '\\n')\n"
        "    print('PRIVATE_APPLICATION_OUTPUT')\n"
        "    if prompt == 'explode':\n"
        "        raise ValueError('PRIVATE_APPLICATION_EXCEPTION')\n"
        "    return prompt.upper()\n",
        encoding="utf-8",
    )
    cases = [
        {"id": "customer-one", "input": "refund", "reference": "REFUND"},
        {"id": "customer-two", "input": "release", "reference": "RELEASE"},
    ]
    (project / "customer.jsonl").write_text(
        "\n".join(json.dumps(case) for case in cases) + "\n", encoding="utf-8"
    )
    invoke(
        [
            python,
            "evaluate.py",
            "--cases",
            "customer.jsonl",
            "--limit",
            "1",
            "--output",
            "customer-first.json",
        ],
        cwd=project,
        env=env,
    )
    assert (project / "calls.txt").read_text(encoding="utf-8").splitlines() == ["refund"]
    completed = invoke(
        [python, "evaluate.py", "--cases", "customer.jsonl", "--output", "customer.json"],
        cwd=project,
        env=env,
    )
    result = json.loads((project / "customer.json").read_text(encoding="utf-8"))
    assert len(result["records"]) == 2
    assert result["summaries"][0]["mean"] == 1
    assert result["summaries"][0]["errors"] == 0
    assert (project / "calls.txt").read_text(encoding="utf-8").splitlines() == [
        "refund",
        "refund",
        "release",
    ]
    assert "PRIVATE_APPLICATION_OUTPUT" not in completed.stdout + completed.stderr
    (project / "broken.jsonl").write_text(
        json.dumps({"id": "broken", "input": "explode", "reference": "unused"}) + "\n",
        encoding="utf-8",
    )
    failure = invoke(
        [python, "evaluate.py", "--cases", "broken.jsonl", "--output", "execution-error.json"],
        cwd=project,
        env=env,
        expected=1,
    )
    assert "Traceback" not in failure.stderr
    errored = json.loads((project / "execution-error.json").read_text(encoding="utf-8"))
    errors = [result for result in errored["results"] if result["outcome"] == "error"]
    assert len(errors) == 1
    assert not errors[0].get("scores")
    assert "application execution failed" in errors[0]["reason"]
    evidence = failure.stdout + failure.stderr + json.dumps(errored)
    assert "PRIVATE_APPLICATION_OUTPUT" not in evidence
    assert "PRIVATE_APPLICATION_EXCEPTION" not in evidence
    print(
        "PASS: configured external callable, custom cases, explicit errors and private diagnostics"
    )


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--wheel", type=Path, required=True, help="pure SDK wheel, or directory containing one"
    )
    args = parser.parse_args()
    wheel = args.wheel.resolve()
    if wheel.is_dir():
        candidates = list(wheel.glob("evalsi-*-py3-none-any.whl"))
        if len(candidates) != 1:
            parser.error("wheel directory must contain exactly one pure SDK wheel")
        wheel = candidates[0]
    if not wheel.is_file() or not wheel.name.endswith("-py3-none-any.whl"):
        parser.error("--wheel must name an existing pure SDK wheel")
    verify(wheel)


if __name__ == "__main__":
    main()
