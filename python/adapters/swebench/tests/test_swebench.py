"""Contract tests against the real swebench package: the importer, the
official grading on real eval logs, and agent runs over the fixture dataset
(offline, through the harness's unconfined local test sandbox)."""

from __future__ import annotations

import asyncio
import json
from pathlib import Path
from typing import Any

import pytest

import evalsi.sandbox.client
from evalsi.datasets import load_records
from evalsi.run import execute
from evalsi.runspec import load_spec
from evalsi_harness.testing import LocalSandboxClient
from evalsi_swebench import fixture, grade, load


@pytest.fixture
def dataset(tmp_path: Path) -> Path:
    return fixture.write(tmp_path / "mini.jsonl")


def test_the_importer_makes_agent_tasks(dataset: Path) -> None:
    rows = load(str(dataset), oracle="true")
    assert [r["id"] for r in rows] == ["evalsi__calc-1", "evalsi__calc-2"]
    env = rows[0]["metadata"]["environment"]
    assert "image" not in env  # "none": the fixture runs on the host's system directories
    assert env["sandbox"]["workdir"] == "/testbed"
    assert env["checker"]["parser"] == "evalsi_swebench:grade"
    script = env["checker"]["files"][".evalsi-eval.sh"]
    assert ">>>>> Test Exit Code" in script  # the official exit-status record was added
    assert env["files"][".evalsi-gold.patch"].startswith("diff --git a/calc.py")
    assert env["setup"] == [fixture.SETUP]
    assert rows[0]["reference"]["FAIL_TO_PASS"] == ["test_add (tests.test_calc.CalcTest.test_add)"]
    assert "add(2, 3) returns -1" in rows[0]["input"]
    # Through the importer entry point, as a dataset URI.
    records = load_records(f"swebench://{dataset}?instances=evalsi__calc-2")
    assert [r.id for r in records] == ["evalsi__calc-2"]
    bad = dataset.parent / "old.jsonl"
    old = {"instance_id": "x", "repo": "a/b", "FAIL_TO_PASS": "[]", "PASS_TO_PASS": "[]"}
    bad.write_text(json.dumps(old) + "\n")
    with pytest.raises(ValueError, match="SWE-bench 5"):
        load(str(bad))


class _Task:
    def __init__(self, metadata: dict[str, Any]) -> None:
        self.record = type("R", (), {"metadata": metadata})()


LOG = """+ git apply -v -
: '>>>>> Start Test Output'
test_add (tests.test_calc.CalcTest.test_add) ... {add}
test_sub (tests.test_calc.CalcTest.test_sub) ... ok

Ran 2 tests in 0.001s
+ __EVALSI_EXIT=$?
: '>>>>> End Test Output'
>>>>> Test Exit Code: {code}
"""


def test_grading_follows_swebench(dataset: Path) -> None:
    meta = load(str(dataset))[0]["metadata"]
    resolved = grade(LOG.format(add="ok", code=0), "", 0, _Task(meta))
    assert resolved.passed
    assert resolved.score == 1.0
    assert resolved.tests["test_add (tests.test_calc.CalcTest.test_add)"] == "passed"
    failed = grade(LOG.format(add="FAIL", code=1), "", 1, _Task(meta))
    assert not failed.passed
    assert failed.score == 0.0
    nothing = grade("bash: python3: command not found\n", "", 127, _Task(meta))
    assert not nothing.passed
    assert "did not run" in nothing.details


AGENT = """\
task=$(cat)
case "$task" in
  *add*) python3 - <<'PY'
src = open("calc.py").read()
open("calc.py", "w").write(src.replace("return a - b", "return a + b", 1))
PY
  ;;
esac
echo done
"""


@pytest.fixture
def local_sandbox(monkeypatch: pytest.MonkeyPatch) -> LocalSandboxClient:
    client = LocalSandboxClient()

    async def connect(address: str | None = None) -> Any:
        return client

    monkeypatch.setattr(evalsi.sandbox.client, "connect", connect)
    return client


def run_spec(tmp_path: Path, dataset: Path, command: list[str], oracle: bool = False) -> Any:
    spec = tmp_path / "run.json"
    document = {
        "apiVersion": "evals.si/v1alpha1",
        "kind": "EvalRun",
        "metadata": {"name": "swebench-fixture"},
        "spec": {
            "target": {"agent": {"cli": {"command": command}}},
            "dataset": {"uri": f"swebench://{dataset}?oracle={str(oracle).lower()}"},
            "evaluators": [{"ref": "task-success"}],
        },
    }
    spec.write_text(json.dumps(document))
    return asyncio.run(execute(load_spec(spec)))


def test_a_cli_agent_on_the_fixture(
    local_sandbox: LocalSandboxClient, tmp_path: Path, dataset: Path
) -> None:
    # The agent fixes the add bug only: one of two instances is resolved.
    result = run_spec(tmp_path, dataset, ["sh", "-c", AGENT])
    summary = {s.metric: s for s in result.result.summaries}["task-success"]
    assert summary.mean == 0.5
    checks = {r.id: r.check for r in result.result.records}
    assert checks["evalsi__calc-1"] is not None
    assert checks["evalsi__calc-1"].passed
    assert checks["evalsi__calc-2"] is not None
    assert not checks["evalsi__calc-2"].passed
    assert (
        checks["evalsi__calc-2"].tests["test_mul (tests.test_calc.CalcTest.test_mul)"] == "failed"
    )


def test_the_gold_patches_resolve_every_instance(
    local_sandbox: LocalSandboxClient, tmp_path: Path, dataset: Path
) -> None:
    result = run_spec(
        tmp_path,
        dataset,
        ["sh", "-c", "git apply .evalsi-gold.patch && rm .evalsi-gold.patch"],
        oracle=True,
    )
    summary = {s.metric: s for s in result.result.summaries}["task-success"]
    assert summary.mean == 1.0
