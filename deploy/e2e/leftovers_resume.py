"""SIGKILL a harness worker mid-request, then resume its on-disk cassette.

Uses a real sandbox pool and a scripted model. Completed tool calls are replayed
into a new sandbox; external side effects are outside this check's scope.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import signal
import subprocess
import sys
import threading
from pathlib import Path
from typing import Any

from google.protobuf.json_format import ParseDict

from evalsi.sandbox.client import SandboxClient, SandboxSpec
from evalsi.types import Content, Record
from evalsi.v1alpha1 import run_pb2
from evalsi_harness import HarnessContext, Task, load_harness, run_task
from evalsi_harness.testing import ScriptedModelServer


async def child(work: Path, url: str) -> None:
    async with SandboxClient(os.environ["EVALSI_SANDBOX_ADDR"]) as client:

        class LeasedClient:
            async def create(self, spec: SandboxSpec | None = None) -> Any:
                spec = spec or SandboxSpec()
                spec.idle_timeout_s = 3
                return await client.create(spec)

        async def get() -> Any:
            return LeasedClient()

        spec = ParseDict(
            {
                "target": {"connector": "openai-compatible", "base_url": url, "model": "scripted"},
                "harness": {"builtin": {"recording": {"mode": "resume", "dir": "tapes"}}},
                "environment": {
                    "sandbox": {"min_isolation": "namespaced"},
                    "checker": {
                        "command": [
                            "sh",
                            "-c",
                            (
                                'test "$(cat log.txt)" = "one\ntwo\nthree" '
                                '&& test "$(cat out.txt)" = hello'
                            ),
                        ]
                    },
                },
            },
            run_pb2.RunSpec(),
        )
        harness = load_harness(spec, HarnessContext(sandboxes=get, base_dir=work))
        try:
            outcome = await run_task(
                Task.build(
                    spec,
                    Record(id="resume", input=Content(text="Write the three lines and hello.")),
                ),
                harness,
            )
            assert not outcome.error, outcome
            assert outcome.record is not None
            assert outcome.record.check is not None, outcome
            assert outcome.record.check.passed, outcome
            (work / "completed.json").write_text(
                json.dumps(
                    {
                        "passed": True,
                        "output": outcome.record.output.as_text(),
                        "isolation": outcome.record.metadata["isolation"],
                    }
                )
            )
        finally:
            await harness.aclose()


def verify(work: Path) -> dict[str, Any]:
    entered = threading.Event()
    release = threading.Event()
    first = True

    def reply(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        nonlocal first
        results = sum(m.get("role") == "tool" for m in messages)
        if results == 2 and first:
            first = False
            entered.set()
            release.wait(45)
        commands = [
            "echo one >> log.txt",
            "echo two >> log.txt",
            "echo three >> log.txt",
            "echo hello > out.txt",
        ]
        if results == len(commands):
            return {"text": "finished"}
        return {"tool_calls": [("bash", {"command": commands[results]})]}

    server = ScriptedModelServer(reply)
    worker = subprocess.Popen(
        [sys.executable, __file__, "--child-url", server.base_url, "--work-dir", str(work)]
    )
    try:
        assert entered.wait(60), "worker never reached its third model request"
        tapes = list((work / "tapes").glob("*.jsonl"))
        assert len(tapes) == 1
        tape = tapes[0]
        saved = [json.loads(line) for line in tape.read_text().splitlines()]
        assert [line["kind"] for line in saved] == ["header", "model", "tool", "model", "tool"]
        worker.kill()
        assert worker.wait(10) == -signal.SIGKILL
        release.set()
        before = len(server.requests)
        subprocess.run(
            [sys.executable, __file__, "--child-url", server.base_url, "--work-dir", str(work)],
            check=True,
            timeout=60,
        )
        assert len(server.requests) - before == 3
        completed = json.loads((work / "completed.json").read_text())
        assert completed["passed"]
        saved = [json.loads(line) for line in tape.read_text().splitlines()]
        assert [line["kind"] for line in saved] == ["header", *["model", "tool"] * 4, "model"]
        ids = [line["id"] for line in saved[1:]]
        assert ids == list(range(len(ids))), ids
        sent = len(server.requests)
        subprocess.run(
            [sys.executable, __file__, "--child-url", server.base_url, "--work-dir", str(work)],
            check=True,
            timeout=60,
        )
        assert len(server.requests) == sent, "finished replay called the model"
        return {
            "worker_exit": "SIGKILL",
            "sandbox": completed["isolation"]["driver"],
            "model": "scripted",
            "completed_tool_turns_before_kill": 2,
            "new_model_turns_on_resume": 3,
            "unique_cassette_events": len(ids),
            "finished_replay_model_calls": 0,
            "checker": "passed",
        }
    finally:
        release.set()
        if worker.poll() is None:
            worker.kill()
        worker.wait()
        server.close()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--work-dir", type=Path, required=True)
    parser.add_argument("--report", type=Path)
    parser.add_argument("--child-url")
    args = parser.parse_args()
    args.work_dir.mkdir(parents=True, exist_ok=True)
    if args.child_url:
        asyncio.run(child(args.work_dir, args.child_url))
    else:
        assert args.report
        result = verify(args.work_dir)
        args.report.write_text(json.dumps(result, indent=2) + "\n")
        print("PASS " + json.dumps(result))
