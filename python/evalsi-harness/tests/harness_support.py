"""Shared helpers for the harness tests."""

from __future__ import annotations

import asyncio
from pathlib import Path
from typing import Any

from google.protobuf import json_format

from evalsi.judges import JudgeClient
from evalsi.runspec import normalize_durations
from evalsi.testing import TEST_JUDGE, ScriptedJudge
from evalsi.types import Content, Record
from evalsi.v1alpha1 import run_pb2
from evalsi_harness import HarnessContext, Task, TaskOutcome, load_harness, run_task
from evalsi_harness.testing import LocalSandboxClient


def spec_of(data: dict[str, Any]) -> run_pb2.RunSpec:
    data = {"dataset": {"inline": {"records": []}}, "evaluators": [{"ref": "exact-match"}], **data}
    return json_format.ParseDict(
        normalize_durations(data, run_pb2.RunSpec.DESCRIPTOR), run_pb2.RunSpec()
    )


def run(
    spec: run_pb2.RunSpec,
    record: Record,
    sandboxes: LocalSandboxClient,
    *,
    trial: int = 0,
    judge: JudgeClient | None = None,
    base_dir: Path | None = None,
) -> TaskOutcome:
    async def go() -> TaskOutcome:
        async def get() -> LocalSandboxClient:
            return sandboxes

        ctx = HarnessContext(
            sandboxes=get,
            judge=(lambda name: judge) if judge else None,
            base_dir=base_dir or Path.cwd(),
        )
        harness = load_harness(spec, ctx)
        try:
            return await run_task(Task.build(spec, record, trial=trial, run_id="run1"), harness)
        finally:
            await harness.aclose()

    return asyncio.run(go())


def record(text: str = "Write hello into out.txt", **kw: Any) -> Record:
    return Record(id=kw.pop("id", "t1"), input=Content(text=text), **kw)


def judge_answering(reply: Any) -> JudgeClient:
    return JudgeClient(TEST_JUDGE, ScriptedJudge(reply))
