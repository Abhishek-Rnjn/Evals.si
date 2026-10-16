"""Contract tests against tau2 v0.2.0's mock domain: the importer, and the
harness end to end with a scripted agent model and a scripted simulated
user, graded by tau2's own evaluator."""

from __future__ import annotations

import asyncio
from collections.abc import Iterator
from pathlib import Path
from typing import Any

import pytest
from google.protobuf import json_format

from evalsi.judges import JudgeClient
from evalsi.runspec import normalize_durations
from evalsi.testing import TEST_JUDGE, ScriptedJudge
from evalsi.types import Content, Record
from evalsi.v1alpha1 import run_pb2
from evalsi_harness import HarnessContext, Task, TaskOutcome, load_harness, run_task
from evalsi_harness.testing import Script, ScriptedModelServer
from evalsi_taubench import load


def test_the_importer_makes_one_record_per_task() -> None:
    rows = load("mock")
    ids = [r["id"] for r in rows]
    assert "mock/create_task_1" in ids
    # Its NL assertions are not in its reward basis: graded without them.
    assert "mock/create_task_1_nl_eval" in ids
    one = load("mock", tasks="create_task_1")
    assert [r["id"] for r in one] == ["mock/create_task_1"]
    meta = one[0]["metadata"]["taubench"]
    assert meta["domain"] == "mock"
    assert meta["task"]["evaluation_criteria"]["actions"][0]["name"] == "create_task"
    assert "Important Meeting" in one[0]["input"]
    with pytest.raises(ValueError, match="unknown tau2 domain"):
        load("nowhere")
    with pytest.raises(ValueError, match="no tasks"):
        load("mock", tasks="missing")


@pytest.fixture
def model() -> Iterator[Any]:
    servers: list[ScriptedModelServer] = []

    def make(script: Script) -> ScriptedModelServer:
        servers.append(ScriptedModelServer(script))
        return servers[-1]

    yield make
    for s in servers:
        s.close()


def steps(answers: list[dict[str, Any]]) -> Script:
    it = iter(answers)
    last: dict[str, Any] = {}

    def script(messages: list[dict[str, Any]], tools: list[str]) -> dict[str, Any]:
        nonlocal last
        last = next(it, last)
        return last

    return script


def run(
    server: ScriptedModelServer,
    task_id: str,
    user: list[dict[str, Any]],
    domain: str = "mock",
    *,
    recording: dict[str, Any] | None = None,
    no_judge: bool = False,
    inspect_prompt: Any = None,
) -> TaskOutcome:
    spec = json_format.ParseDict(
        normalize_durations(
            {
                "target": {
                    "connector": "openai-compatible",
                    "model": "scripted",
                    "base_url": server.base_url,
                },
                "dataset": {"inline": {"records": []}},
                "evaluators": [{"ref": "task-success"}],
                "harness": {"external": {"python": "evalsi_taubench:TauBenchHarness"}},
            },
            run_pb2.RunSpec.DESCRIPTOR,
        ),
        run_pb2.RunSpec(),
    )
    row = load(domain, tasks=task_id)[0]
    record = Record(id=row["id"], input=Content(text=row["input"]), metadata=row["metadata"])
    if recording is not None:
        json_format.ParseDict({"recording": recording}, spec.harness.external.config)
    replies = iter(user)

    def answer(prompt: str, schema: dict[str, Any]) -> dict[str, Any]:
        if inspect_prompt is not None:
            inspect_prompt(prompt)
        return next(replies)

    judge = JudgeClient(TEST_JUDGE, ScriptedJudge(answer))

    async def go() -> TaskOutcome:
        async def no_sandboxes() -> Any:
            raise AssertionError("tau-bench tasks need no sandbox")

        ctx = HarnessContext(
            sandboxes=no_sandboxes,
            judge=None if no_judge else lambda name: judge,
            base_dir=Path.cwd(),
        )
        harness = load_harness(spec, ctx)
        try:
            return await run_task(Task.build(spec, record, trial=0, run_id="r"), harness)
        finally:
            await harness.aclose()

    return asyncio.run(go())


USER = [
    {"message": "Please create a task called 'Important Meeting' for user_1.", "done": False},
    {"message": "Thanks!", "done": True},
]


def test_an_agent_that_follows_the_policy_is_rewarded(model: Any) -> None:
    server = model(
        steps(
            [
                {
                    "tool_calls": [
                        ("create_task", {"user_id": "user_1", "title": "Important Meeting"})
                    ]
                },
                {"text": "Done: the agent confirmed the task was created successfully."},
            ]
        )
    )
    out = run(server, "create_task_1", USER)
    assert out.error == ""
    rec = out.record
    assert rec is not None
    assert rec.check is not None
    assert rec.check.passed, rec.check.details
    assert rec.check.score == 1.0
    assert rec.check.tests["database"] == "passed"
    assert rec.check.tests["action 0: create_task"] == "passed"
    steps_ = rec.trajectory.steps  # type: ignore[union-attr]
    assert [s.type for s in steps_] == ["user", "llm", "tool", "llm", "user"]
    # The agent got tau2's prompt with the domain policy and the domain's tools.
    first = server.requests[0]
    assert "Mock Domain Policy" in first["messages"][0]["content"]
    assert {t["function"]["name"] for t in first["tools"]} >= {"create_task", "get_users"}


def test_a_wrong_database_state_fails(model: Any) -> None:
    server = model(
        steps(
            [
                {"tool_calls": [("create_task", {"user_id": "user_1", "title": "Meeting"})]},
                {"text": "The agent confirmed the task was created successfully."},
            ]
        )
    )
    rec = run(server, "create_task_1", USER).record
    assert rec is not None
    assert rec.check is not None
    assert not rec.check.passed
    assert rec.check.tests["database"] == "failed"
    assert (
        rec.check.tests["communicate: The agent confirmed the task was created successfully"]
        == "passed"
    )


def test_tool_errors_and_unknown_tools_still_grade(model: Any) -> None:
    server = model(
        steps(
            [
                {"tool_calls": [("no_such_tool", {})]},
                {"tool_calls": [("create_task", {"user_id": "nobody", "title": "x"})]},
                {
                    "tool_calls": [
                        ("create_task", {"user_id": "user_1", "title": "Important Meeting"})
                    ]
                },
                {"text": "The agent confirmed the task was created successfully."},
            ]
        )
    )
    rec = run(server, "create_task_1", USER).record
    assert rec is not None
    assert rec.check is not None
    assert rec.check.passed, rec.check.details


def test_the_task_history_is_replayed(model: Any) -> None:
    server = model(
        steps(
            [
                {
                    "tool_calls": [
                        ("update_task_status", {"task_id": "task_2", "status": "completed"})
                    ]
                },
                {
                    "text": "The agent acknowledged the previous context. "
                    "The agent confirmed the task status was updated successfully."
                },
            ]
        )
    )
    out = run(
        server,
        "update_task_with_initialization_actions",
        [
            {"message": "Please mark task_2 as completed.", "done": False},
            {"message": "ok", "done": True},
        ],
    )
    rec = out.record
    assert rec is not None, out.error
    assert rec.check is not None
    assert rec.check.passed, rec.check.details


TELECOM = "[service_issue]airplane_mode_on|unseat_sim_card[PERSONA:None]"


def phone_user(fix: bool) -> list[dict[str, Any]]:
    def say(message: str, done: bool = False) -> dict[str, Any]:
        return {"message": message, "tool": "", "arguments": "{}", "done": done}

    def act(tool: str) -> dict[str, Any]:
        return {"message": "", "tool": tool, "arguments": "{}", "done": False}

    acts = [act("toggle_airplane_mode"), act("reseat_sim_card")] if fix else []
    return [
        say("My phone has no service."),
        *acts,
        say("I did both, and I have service now. ###STOP###"),
    ]


def telecom_agent() -> Script:
    return steps([{"text": "Please turn airplane mode off and reseat your SIM card."}])


@pytest.mark.parametrize("fix", [True, False])
def test_the_user_acts_on_their_own_device(model: Any, fix: bool) -> None:
    out = run(model(telecom_agent()), TELECOM, phone_user(fix), domain="telecom")
    assert out.error == ""
    rec = out.record
    assert rec is not None
    assert rec.check is not None
    # Graded on the device's end state, which only the user's tool calls change.
    assert rec.check.passed is fix, rec.check.details
    assert "assertion 0: assert_service_status" in rec.check.tests


def test_user_tool_calls_are_replayed_in_order() -> None:
    from tau2.data_model.message import AssistantMessage, ToolMessage, UserMessage

    from evalsi_taubench import _events_to_tau

    class FakeUser:
        def __init__(self) -> None:
            self.batches = [
                [
                    UserMessage(role="user", content=None),
                    ToolMessage(id="u1", role="tool", content="ok", requestor="user"),
                ]
            ]

        def take(self) -> list[Any]:
            return self.batches.pop(0) if self.batches else []

        def take_rest(self) -> list[Any]:
            return []

    from evalsi.types import Content, Step
    from evalsi_harness.events import StepEvent

    events = [
        StepEvent(Step(type="user", name="u", output=Content(text="hi"), span_id="1")),
        StepEvent(Step(type="user", name="u", output=Content(text="again"), span_id="2")),
    ]
    out = _events_to_tau(events, set(), FakeUser())  # type: ignore[arg-type]
    kinds = [(type(m).__name__, m.content) for m in out]
    assert kinds == [
        ("UserMessage", None),
        ("ToolMessage", "ok"),
        ("UserMessage", "hi"),
        ("UserMessage", "again"),
    ]
    assert not any(isinstance(m, AssistantMessage) for m in out)


@pytest.mark.parametrize("domain", ["mock", "telecom"])
def test_default_simulator_replays_without_a_judge(model: Any, tmp_path: Path, domain: str) -> None:
    if domain == "mock":
        server = model(
            steps(
                [
                    {
                        "tool_calls": [
                            ("create_task", {"user_id": "user_1", "title": "Important Meeting"})
                        ]
                    },
                    {"text": "The agent confirmed the task was created successfully."},
                ]
            )
        )
        task_id, user = "create_task_1", USER
    else:
        server = model(telecom_agent())
        task_id, user = TELECOM, phone_user(True)
    before = run(server, task_id, user, domain, recording={"mode": "record", "dir": str(tmp_path)})
    sent = len(server.requests)
    after = run(
        server,
        task_id,
        [],
        domain,
        recording={"mode": "replay", "dir": str(tmp_path)},
        no_judge=True,
    )
    assert before.error == after.error == ""
    assert before.record is not None
    assert after.record is not None
    assert before.record.check is not None
    assert after.record.check is not None
    assert before.record.check.passed
    assert after.record.check == before.record.check
    assert len(server.requests) == sent


@pytest.mark.parametrize("mode", ["resume", "fork"])
def test_device_actions_and_user_history_survive_continuation(
    model: Any,
    tmp_path: Path,
    mode: str,
) -> None:
    import json

    server = model(
        steps(
            [
                {"text": "Please turn airplane mode off and reseat your SIM card."},
                {"text": "Is service back?"},
            ]
        )
    )
    user = phone_user(True)
    user[-1] = {**user[-1], "message": "I did both, and I have service now.", "done": False}
    final = {"message": "Thanks! ###STOP###", "done": True}
    control = run(
        server,
        TELECOM,
        [*user, final],
        "telecom",
        recording={"mode": "record", "dir": str(tmp_path)},
    )
    assert control.record is not None
    assert control.record.check is not None
    assert control.record.check.passed
    tape = next(tmp_path.glob("*.jsonl"))
    if mode == "resume":
        lines = tape.read_text().splitlines()
        assert json.loads(lines[-1])["kind"] == "user"
        tape.write_text("\n".join(lines[:-1]) + "\n")
    server.script = steps([{"text": "Is service back?"}])
    prompts = []

    def inspect(prompt: str) -> None:
        prompts.append(prompt)
        assert "YOUR TOOL CALL toggle_airplane_mode" in prompt
        assert "YOUR TOOL CALL reseat_sim_card" in prompt
        assert "I did both" in prompt
        assert "AGENT: Is service back?" in prompt

    rec: dict[str, Any] = {"mode": mode, "dir": str(tmp_path)}
    if mode == "fork":
        rec["branch_at_step"] = 1
    result = run(server, TELECOM, [final], "telecom", recording=rec, inspect_prompt=inspect)
    assert result.error == ""
    assert result.record is not None
    assert result.record.check is not None
    assert result.record.check == control.record.check
    assert len(prompts) == 1
    if mode == "fork":
        child = next(tmp_path.glob("*.fork-1.jsonl"))
        tape.write_text(child.read_text())
    replay = run(
        server,
        TELECOM,
        [],
        "telecom",
        recording={"mode": "replay", "dir": str(tmp_path)},
        no_judge=True,
    )
    assert replay.error == ""
    assert replay.record is not None
    assert replay.record.check == control.record.check
