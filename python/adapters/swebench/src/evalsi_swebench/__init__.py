"""SWE-bench as Evals.si agent tasks.

The importer ``swebench://`` turns SWE-bench instances (in the SWE-bench 5
format, which carries each instance's image, eval script, log parser and
evaluation type) into records whose environment is the instance's image with
the repository at ``/testbed``, and whose checker is the instance's eval
script graded by the official SWE-bench code::

    dataset: {uri: "swebench://verified.jsonl?instances=django__django-11099,astropy__astropy-12907"}

The agent, built-in or bring-your-own, works in ``/testbed``. The checker then
resets the test files, applies the test patch, runs the tests and reports the
instance resolved when every FAIL_TO_PASS test passes and every PASS_TO_PASS
test still does, exactly as the SWE-bench harness decides. Grading never
depends on the sandbox rung: a Firecracker microVM and a bubblewrap sandbox
run the same image and the same script.

Options (query string): ``instances`` (comma-separated ids), ``split``,
``workdir`` (default /testbed), ``network`` (default deny), ``timeout``
(checker timeout, default 30m), ``image`` (override; ``none`` uses the host's
system directories, for fixtures), ``oracle=true`` (adds the gold patch as
``.evalsi-gold.patch`` in the workdir, to validate the setup with an agent
that just applies it).

An instance may carry ``evalsi_environment`` (environment fields, merged
over the defaults): datasets and test fixtures without prebuilt images use it
to place the repository and set it up.
"""

from __future__ import annotations

import json
import tempfile
from pathlib import Path
from typing import Any

from evalsi.types import TaskCheck

REQUIRED = (
    "instance_id",
    "image",
    "eval_script",
    "log_parser",
    "eval_type",
    "FAIL_TO_PASS",
    "PASS_TO_PASS",
)

INSTRUCTION = """\
<issue>
{problem}
</issue>

The repository is checked out in {workdir} at the commit the issue was reported against.
Change the code (not the tests) so that the issue is resolved. Do not commit your changes.
"""


def _tests(value: Any) -> list[str]:
    return json.loads(value) if isinstance(value, str) else list(value or [])


def load(path: str, **options: str) -> list[dict[str, Any]]:
    """Records for ``swebench://<file.json|.jsonl|.parquet or HF dataset>?options``."""
    from swebench.harness.utils import load_swebench_dataset, make_test_spec

    wanted = [i for i in options.get("instances", "").split(",") if i]
    raw = load_swebench_dataset(path, options.get("split", "test"), wanted or None)
    workdir = options.get("workdir", "/testbed")
    oracle = options.get("oracle", "false").lower() in ("1", "true", "yes")
    rows = []
    for inst in raw:
        missing = [k for k in REQUIRED if k not in inst]
        if missing:
            raise ValueError(
                f"{inst.get('instance_id', '?')}: missing {', '.join(missing)}; the importer needs "
                "SWE-bench 5 instances, which carry their image, eval script and log parser"
            )
        image = options.get("image", inst["image"])
        # The official script, with the test command's exit status recorded
        # after the end marker; run with stderr folded into stdout, in order,
        # as the SWE-bench harness captures it.
        script = "\n".join(
            ["#!/bin/bash", "set -uxo pipefail", *make_test_spec(dict(inst)).eval_script_list]
        )
        environment: dict[str, Any] = {
            "sandbox": {
                "workdir": workdir,
                "network": options.get("network", "deny"),
                "min_isolation": "namespaced",
            },
            "checker": {
                "command": ["bash", "-c", "bash .evalsi-eval.sh 2>&1"],
                "files": {".evalsi-eval.sh": script + "\n"},
                "timeout": options.get("timeout", "30m"),
                "parser": "evalsi_swebench:grade",
            },
            "command_timeout": "10m",
        }
        if image and image != "none":
            environment["image"] = image
        extra = inst.get("evalsi_environment") or {}
        for key, value in extra.items():
            if isinstance(value, dict) and isinstance(environment.get(key), dict):
                environment[key] = {**environment[key], **value}
            else:
                environment[key] = value
        if oracle:
            environment["files"] = {
                **environment.get("files", {}),
                ".evalsi-gold.patch": inst.get("patch", ""),
            }
        rows.append(
            {
                "id": inst["instance_id"],
                "input": INSTRUCTION.format(
                    problem=inst.get("problem_statement", "").strip(), workdir=workdir
                ),
                "reference": {
                    "FAIL_TO_PASS": _tests(inst["FAIL_TO_PASS"]),
                    "PASS_TO_PASS": _tests(inst["PASS_TO_PASS"]),
                },
                "metadata": {
                    "repo": inst.get("repo", ""),
                    "version": inst.get("version", ""),
                    "base_commit": inst.get("base_commit", ""),
                    "swebench": {k: inst[k] for k in (*REQUIRED, "repo", "version") if k in inst},
                    "environment": environment,
                },
            }
        )
    return rows


def grade(stdout: str, stderr: str, exit_code: int, task: Any) -> TaskCheck:
    """The checker's parser: the official SWE-bench verdict on the eval log."""
    from swebench.harness.constants import FAIL_TO_PASS, PASS_TO_PASS
    from swebench.harness.grading import get_eval_report
    from swebench.harness.utils import make_test_spec

    instance = dict(task.record.metadata["swebench"])
    instance.setdefault("repo", task.record.metadata.get("repo", ""))
    instance.setdefault("version", task.record.metadata.get("version", ""))
    spec = make_test_spec(instance)
    with tempfile.TemporaryDirectory() as tmp:
        log = Path(tmp) / "eval.log"
        log.write_text(stdout + "\n" + stderr, encoding="utf-8")
        report = get_eval_report(
            spec,
            {"instance_id": spec.instance_id, "model_patch": "(applied in the sandbox)"},
            str(log),
            True,
        )[spec.instance_id]
    status = report.get("tests_status") or {}
    tests: dict[str, str] = {}
    for kind in (FAIL_TO_PASS, PASS_TO_PASS):
        for outcome in ("success", "failure"):
            for name in status.get(kind, {}).get(outcome, []):
                tests[name] = "passed" if outcome == "success" else "failed"
    f2p = status.get(FAIL_TO_PASS, {})
    fixed, broken = len(f2p.get("success", [])), len(f2p.get("failure", []))
    details = "resolved" if report.get("resolved") else "not resolved"
    if not report.get("patch_successfully_applied"):
        details = "the tests did not run: " + str(
            report.get("infra_failure_reason") or "no test output"
        )
    return TaskCheck(
        passed=bool(report.get("resolved")),
        score=fixed / (fixed + broken) if fixed + broken else None,
        details=details,
        tests=tests,
        error="the environment could not run the tests" if report.get("infra_failure") else "",
    )


__all__ = ["grade", "load"]
