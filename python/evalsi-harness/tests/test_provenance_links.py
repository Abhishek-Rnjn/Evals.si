"""Connector relationships must survive the normalized trajectory and scoring."""

from __future__ import annotations

import asyncio
import json
from typing import Any

import httpx

from evalsi.packs.agent_checks import citation_grounding, handoff_correctness
from evalsi.types import Content, Record, Trajectory
from evalsi.v1alpha1 import agent_pb2
from evalsi_harness.connectors import A2AConnector
from evalsi_harness.context import ContextConfig
from evalsi_harness.events import StepEvent
from evalsi_harness.loop import AgentLoop, LoopConfig
from evalsi_harness.models import ModelTurn, ToolCallRequest
from evalsi_harness.readers import parse_claude
from tests.test_evidence import DOC


def test_evidence_snapshot_is_exported_and_can_be_scored() -> None:
    class Model:
        name = "fake"

        def __init__(self) -> None:
            self.turns = iter(
                [
                    ModelTurn(
                        text="",
                        tool_calls=[
                            ToolCallRequest(
                                "1",
                                "evidence",
                                json.dumps(
                                    {
                                        "action": "add",
                                        "statement": "Signed in 1847",
                                        "source": "task",
                                        "quote": DOC,
                                    }
                                ),
                            )
                        ],
                    ),
                    ModelTurn(text="Signed in 1847 [E1]."),
                ]
            )

        async def complete(self, *args: Any, **kwargs: Any) -> ModelTurn:
            return next(self.turns)

        async def aclose(self) -> None:
            pass

    events: list[Any] = []
    loop = AgentLoop(
        Model(),
        [],
        LoopConfig(context=ContextConfig(strategy="evidence-state", window_tokens=8000)),
        events.append,
    )
    final = asyncio.run(loop.run([{"role": "user", "content": DOC}]))
    snapshot = final.extra["evidence_state"]
    assert snapshot["version"] == 1
    assert snapshot["evidence"][0]["quote"] == DOC
    record = Record(
        id="r",
        input=Content(text=DOC),
        output=final.output,
        trajectory=Trajectory(steps=[e.step for e in events if isinstance(e, StepEvent)]),
        metadata={"agent": final.extra},
    )
    assert citation_grounding(record).passed
    assert loop.evidence is not None
    assert loop.evidence.retract("E1", "wrong document") is None
    # Snapshots are copies, not references to mutable live state.
    assert snapshot["evidence"][0]["status"] == "active"
    record.metadata["agent"]["evidence_state"] = loop.evidence.snapshot()
    assert not citation_grounding(record).passed


def test_claude_child_before_parent_resolves_to_explicit_handoff() -> None:
    events = [
        {
            "type": "assistant",
            "parent_tool_use_id": "task",
            "message": {"id": "child", "content": [{"type": "text", "text": "subtask done"}]},
        },
        {
            "type": "assistant",
            "message": {
                "id": "parent",
                "content": [{"type": "tool_use", "id": "task", "name": "Task", "input": {}}],
            },
        },
    ]
    parsed = parse_claude("\n".join(json.dumps(e) for e in events))
    record = Record(id="r", trajectory=Trajectory(steps=parsed.steps))
    assert handoff_correctness(record).passed
    missing = parse_claude(json.dumps(events[0]))
    assert not handoff_correctness(
        Record(id="r", trajectory=Trajectory(steps=missing.steps))
    ).passed


def test_a2a_explicit_task_relationships_link_across_messages() -> None:
    async def go() -> None:
        connector = A2AConnector(agent_pb2.A2AAgent(url="https://agent.invalid"))
        await connector.client.aclose()
        tasks = iter(
            [
                {"kind": "task", "id": "parent", "status": {"state": "input-required"}},
                {
                    "kind": "task",
                    "id": "child",
                    "status": {"state": "completed"},
                    "metadata": {"evalsi": {"parent_task_id": "parent"}},
                },
            ]
        )
        connector.client = httpx.AsyncClient(
            transport=httpx.MockTransport(
                lambda req: httpx.Response(200, json={"result": next(tasks)})
            )
        )
        try:
            first = await connector.send("delegate")
            second = await connector.send("continue")
            steps = first.steps + second.steps
            assert handoff_correctness(Record(id="r", trajectory=Trajectory(steps=steps))).passed
            assert len({s.span_id for s in steps}) == len(steps)
        finally:
            await connector.aclose()

    asyncio.run(go())


def test_a2a_references_do_not_imply_a_handoff() -> None:
    async def go() -> None:
        connector = A2AConnector(agent_pb2.A2AAgent(url="https://agent.invalid", stream=True))
        await connector.client.aclose()
        payload = {
            "result": {
                "kind": "task",
                "id": "child",
                "status": {"state": "completed"},
                "history": [{"referenceTaskIds": ["parent"]}],
            }
        }
        connector.client = httpx.AsyncClient(
            transport=httpx.MockTransport(
                lambda req: httpx.Response(200, text="data: " + json.dumps(payload) + "\n\n")
            )
        )
        try:
            reply = await connector.send("read prior result")
            assert not any(step.type == "handoff" for step in reply.steps)
        finally:
            await connector.aclose()

    asyncio.run(go())


def test_a2a_stream_links_explicit_parent_and_child_tasks() -> None:
    async def go() -> None:
        connector = A2AConnector(agent_pb2.A2AAgent(url="https://agent.invalid", stream=True))
        await connector.client.aclose()
        events = [
            {"kind": "task", "id": "parent", "status": {"state": "working"}},
            {
                "kind": "task",
                "id": "child",
                "status": {"state": "working"},
                "metadata": {"evalsi": {"parent_task_id": "parent"}},
            },
            {"kind": "status-update", "taskId": "child", "status": {"state": "completed"}},
            {
                "kind": "status-update",
                "taskId": "parent",
                "status": {"state": "completed"},
                "final": True,
            },
        ]
        stream = "".join("data: " + json.dumps({"result": event}) + "\n\n" for event in events)
        connector.client = httpx.AsyncClient(
            transport=httpx.MockTransport(lambda req: httpx.Response(200, text=stream))
        )
        try:
            reply = await connector.send("delegate")
            record = Record(id="r", trajectory=Trajectory(steps=reply.steps))
            assert handoff_correctness(record).passed
            assert len([step for step in reply.steps if step.type == "handoff"]) == 1
        finally:
            await connector.aclose()

    asyncio.run(go())
