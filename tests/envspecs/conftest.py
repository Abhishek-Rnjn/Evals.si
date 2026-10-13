from __future__ import annotations

from typing import Any

from google.protobuf import json_format

from evalsi.runspec import normalize_durations
from evalsi.types import Content, Record
from evalsi.v1alpha1 import run_pb2

# A task: mark notes.txt done, graded by a command on the sandbox's end state.
ENVIRONMENT: dict[str, Any] = {
    "files": {"notes.txt": "draft\n"},
    "setup": ["echo ready > setup.log"],
    "checker": {"command": ["sh", "-c", "grep -q done notes.txt"]},
}


def spec_of(data: dict[str, Any]) -> run_pb2.RunSpec:
    data = {"dataset": {"inline": {"records": []}}, "evaluators": [{"ref": "exact-match"}], **data}
    return json_format.ParseDict(
        normalize_durations(data, run_pb2.RunSpec.DESCRIPTOR), run_pb2.RunSpec()
    )


def task(text: str = "Mark the notes done.", checker: dict[str, Any] | None = None) -> Any:
    env = {**ENVIRONMENT, **({"checker": checker} if checker else {})}
    return Record(id="t1", input=Content(text=text)), spec_of({"environment": env})
