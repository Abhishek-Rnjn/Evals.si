"""Protected-file and interjection checks against an actual sandbox pool.

EVALSI_SANDBOX_ADDR/TLS select a real pool. The model is scripted; no model-quality
claim follows. Run separately for pod and bubblewrap backends.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import os
from pathlib import Path
from typing import Any

from google.protobuf.json_format import ParseDict

from evalsi.runspec import normalize_durations
from evalsi.sandbox.client import SandboxClient
from evalsi.types import Content, Record
from evalsi.v1alpha1 import run_pb2
from evalsi_harness import HarnessContext, Task, load_harness, run_task
from evalsi_harness.testing import ScriptedModelServer

FILES = {
    "calc.py": "def add(a,b): return a-b\n",
    "tests/test_calc.py": "from calc import add\nassert add(2,3)==5\n",
}
FIX = "printf 'def add(a,b): return a+b\\n' > calc.py"
CHEAT = "printf 'pass\\n' > tests/test_calc.py"


async def checks(image: str, driver: str) -> dict[str, Any]:
    async with SandboxClient(os.environ["EVALSI_SANDBOX_ADDR"]) as client:

        async def get() -> SandboxClient:
            return client

        async def run(data: dict[str, Any]) -> Record:
            data.setdefault("environment", {}).update(
                {
                    "image": image,
                    "sandbox": {"network": "deny", "min_isolation": "namespaced", "memory_mb": 128},
                }
            )
            spec = ParseDict(
                normalize_durations(data, run_pb2.RunSpec.DESCRIPTOR), run_pb2.RunSpec()
            )
            harness = load_harness(spec, HarnessContext(sandboxes=get))
            try:
                outcome = await run_task(
                    Task.build(spec, Record(id="live", input=Content(text="fix add"))), harness
                )
                assert not outcome.error, outcome
                assert outcome.record is not None
                return outcome.record
            finally:
                await harness.aclose()

        results = []
        for label, script, mode, passes in [
            ("honest", FIX, "fail", True),
            ("tamper", CHEAT, "fail", False),
            ("restore-cheat", CHEAT, "restore", False),
            ("restore-fix", FIX + "; " + CHEAT, "restore", True),
            ("restore-symlink", FIX + "; rm -rf tests; ln -s . tests", "restore", True),
        ]:
            rec = await run(
                {
                    "target": {"agent": {"cli": {"command": ["sh", "-c", script]}}},
                    "environment": {
                        "files": FILES,
                        "checker": {
                            "command": ["sh", "-c", "PYTHONPATH=. python3 tests/test_calc.py"],
                            "protect": ["tests/**"],
                            "on_change": mode,
                        },
                    },
                }
            )
            assert rec.check is not None
            assert not rec.check.error, rec.check
            assert rec.check.passed == passes, (label, rec.check)
            if label != "honest":
                assert rec.metadata["policy_events"][0]["kind"] == "protected_file_modified"
            assert rec.trajectory is not None
            assert rec.metadata["isolation"]["driver"] == driver, rec.metadata
            results.append({"case": label, "passed": rec.check.passed})
            print(f"PASS {driver}: {label}", flush=True)

        note = "Use hello.txt instead."

        def reply(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
            if any(m.get("role") == "tool" for m in messages):
                assert note in str(messages), messages
                return {"text": "read the correction"}
            return {"tool_calls": [("bash", {"command": "echo hello > hello.txt"})]}

        with_server = ScriptedModelServer(reply)
        try:
            rec = await run(
                {
                    "target": {
                        "connector": "openai-compatible",
                        "base_url": with_server.base_url,
                        "model": "scripted",
                    },
                    "harness": {"builtin": {"interjections": [{"at_step": 1, "message": note}]}},
                    "environment": {"checker": {"command": ["sh", "-c", "test -f hello.txt"]}},
                }
            )
            assert rec.check is not None, rec
            assert rec.check.passed, rec
            assert rec.trajectory is not None
            assert any(s.name == "user-interjection" for s in rec.trajectory.steps)
            assert len(with_server.requests) == 2
            results.append({"case": "interjection", "passed": True})
            print(f"PASS {driver}: interjection", flush=True)
        finally:
            with_server.close()
        return {"driver": driver, "model": "scripted", "cases": results}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", default="")
    parser.add_argument("--driver", choices=["pod", "bwrap"], required=True)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    args.report.write_text(
        json.dumps(asyncio.run(checks(args.image, args.driver)), indent=2) + "\n"
    )


if __name__ == "__main__":
    main()
