"""Bring-your-own agents: A2A, MCP, OpenAI Responses-compatible, HTTP and
command-line agents run inside the task's sandbox.

Every connector is a conversation: ``send`` gives the agent a user message
and returns its reply with whatever trajectory the protocol exposes. A
simulated user, when configured, keeps the conversation going; otherwise the
agent gets the task's instruction once. The environment's checker then
grades what the agent did, exactly as for the built-in agent.
"""

from __future__ import annotations

import asyncio
import json
import os
import shlex
import time
import uuid
from collections.abc import Mapping
from dataclasses import dataclass, field
from typing import Any, Protocol

import httpx

from evalsi.sandbox import SandboxError
from evalsi.types import Content, Message, Step, ToolCall, Usage
from evalsi.v1alpha1 import agent_pb2
from evalsi_harness.environment import TaskEnvironment
from evalsi_harness.events import Emit, FinalEvent, StepEvent, span_id
from evalsi_harness.task import Task, TaskError

DEFAULT_TIMEOUT_S = 600.0


@dataclass
class AgentReply:
    text: str
    steps: list[Step] = field(default_factory=list)
    usage: Usage = field(default_factory=Usage)
    # The agent ended the conversation on its side (a terminal A2A state).
    finished: bool = False
    extra: dict[str, Any] = field(default_factory=dict)


class RemoteAgent(Protocol):
    name: str

    async def send(self, text: str) -> AgentReply: ...

    async def aclose(self) -> None: ...


def _timeout(msg: Any) -> float:
    return msg.timeout.ToNanoseconds() / 1e9 if msg.HasField("timeout") else DEFAULT_TIMEOUT_S


def _headers(headers_env: Mapping[str, str]) -> dict[str, str]:
    out = {}
    for header, env in headers_env.items():
        value = os.environ.get(env)
        if value is None:
            raise TaskError(f"header {header} needs the environment variable {env} on the worker")
        out[header] = value
    return out


def _get_path(data: Any, path: str) -> Any:
    for part in [p for p in path.split(".") if p]:
        if isinstance(data, list) and part.isdigit() and int(part) < len(data):
            data = data[int(part)]
        elif isinstance(data, Mapping):
            data = data.get(part)
        else:
            return None
    return data


def _agent_step(name: str, text: str, *, input_text: str = "", error: str = "") -> Step:
    return Step(
        type="agent",
        name=name,
        input=Content(text=input_text) if input_text else None,
        output=Content(text=text),
        error=error,
        span_id=span_id(),
    )


def steps_from_messages(messages: list[Any]) -> list[Step]:
    """OpenAI-style messages (as an agent returned them) as LLM and tool steps."""
    steps: list[Step] = []
    calls: dict[str, ToolCall] = {}
    for m in messages:
        if not isinstance(m, Mapping):
            continue
        role = m.get("role")
        if role == "assistant":
            tcs = [
                ToolCall(
                    id=str(c.get("id", "")),
                    name=str((c.get("function") or {}).get("name", "")),
                    arguments=str((c.get("function") or {}).get("arguments", "")),
                )
                for c in m.get("tool_calls") or []
            ]
            for c in tcs:
                calls[c.id] = c
            steps.append(
                Step(
                    type="llm",
                    name="agent",
                    output=Content(
                        messages=[
                            Message(
                                role="assistant",
                                content=str(m.get("content") or ""),
                                tool_calls=tcs,
                            )
                        ]
                    ),
                    span_id=span_id(),
                )
            )
        elif role == "tool":
            call = calls.get(str(m.get("tool_call_id", "")))
            steps.append(
                Step(
                    type="tool",
                    name=call.name if call else str(m.get("name", "tool")),
                    input=Content(text=call.arguments) if call else None,
                    output=Content(text=str(m.get("content") or "")),
                    span_id=span_id(),
                )
            )
    return steps


# --- A2A ---


def _a2a_text(parts: Any) -> str:
    out = []
    for p in parts or []:
        if isinstance(p, Mapping) and (p.get("kind") or p.get("type")) == "text":
            out.append(str(p.get("text", "")))
    return "".join(out)


TERMINAL = {"completed", "failed", "canceled", "rejected"}


class A2AConnector:
    """Agent2Agent: JSON-RPC ``message/send``, following the task until it settles."""

    def __init__(self, cfg: agent_pb2.A2AAgent) -> None:
        self.url = cfg.url
        self.name = cfg.url
        self.client = httpx.AsyncClient(timeout=_timeout(cfg), headers=_headers(cfg.headers_env))
        self.context_id: str | None = None
        self.task_id: str | None = None

    async def _rpc(self, method: str, params: dict[str, Any]) -> Any:
        body = {"jsonrpc": "2.0", "id": str(uuid.uuid4()), "method": method, "params": params}
        try:
            response = await self.client.post(self.url, json=body)
        except httpx.HTTPError as exc:
            raise TaskError(f"A2A agent {self.url}: {exc}") from exc
        if response.status_code >= 400:
            raise TaskError(
                f"A2A agent {self.url}: HTTP {response.status_code}: {response.text[:300]}"
            )
        data = response.json()
        if "error" in data:
            raise TaskError(f"A2A agent {self.url}: {data['error'].get('message', data['error'])}")
        return data.get("result")

    async def send(self, text: str) -> AgentReply:
        message: dict[str, Any] = {
            "role": "user",
            "kind": "message",
            "messageId": str(uuid.uuid4()),
            "parts": [{"kind": "text", "text": text}],
        }
        if self.context_id:
            message["contextId"] = self.context_id
        if self.task_id:
            message["taskId"] = self.task_id
        result = await self._rpc(
            "message/send", {"message": message, "configuration": {"blocking": True}}
        )
        if (
            isinstance(result, Mapping)
            and result.get("kind", "task") == "task"
            and "status" in result
        ):
            deadline = time.monotonic() + 600
            while (result.get("status") or {}).get("state") not in (*TERMINAL, "input-required"):
                if time.monotonic() > deadline:
                    raise TaskError(f"A2A agent {self.url}: task {result.get('id')} did not settle")
                await asyncio.sleep(1)
                result = await self._rpc("tasks/get", {"id": result["id"]})
            self.context_id = result.get("contextId") or self.context_id
            state = result["status"]["state"]
            self.task_id = result.get("id") if state == "input-required" else None
            texts = [_a2a_text(a.get("parts")) for a in result.get("artifacts") or []]
            reply = "\n".join(t for t in texts if t) or _a2a_text(
                (result["status"].get("message") or {}).get("parts")
            )
            steps = [
                _agent_step("a2a", _a2a_text(m.get("parts")))
                for m in result.get("history") or []
                if m.get("role") == "agent" and _a2a_text(m.get("parts"))
            ]
            steps.append(
                _agent_step("a2a", reply, input_text=text, error=state if state == "failed" else "")
            )
            return AgentReply(
                reply, steps=steps, finished=state in TERMINAL, extra={"a2a_state": state}
            )
        if isinstance(result, Mapping):
            self.context_id = result.get("contextId") or self.context_id
            reply = _a2a_text(result.get("parts"))
            return AgentReply(reply, steps=[_agent_step("a2a", reply, input_text=text)])
        raise TaskError(f"A2A agent {self.url}: unexpected result {result!r:.200}")

    async def aclose(self) -> None:
        await self.client.aclose()


# --- MCP ---


class MCPConnector:
    """An agent exposed as a tool on an MCP server. Each message is one call."""

    def __init__(self, cfg: agent_pb2.MCPAgent) -> None:
        self.cfg = cfg
        self.name = cfg.url
        self.client: Any = None
        self.tool = cfg.tool

    async def send(self, text: str) -> AgentReply:
        from evalsi_harness.mcp import MCPClient, MCPError

        try:
            if self.client is None:
                self.client = await MCPClient.connect(
                    url=self.cfg.url,
                    headers=_headers(self.cfg.headers_env),
                    timeout_s=_timeout(self.cfg),
                )
                if not self.tool:
                    tools = await self.client.list_tools()
                    if len(tools) != 1:
                        raise TaskError(
                            f"MCP agent {self.cfg.url} has {len(tools)} tools; set mcp.tool"
                        )
                    self.tool = tools[0].name
        except MCPError as exc:
            raise TaskError(str(exc)) from exc
        result = await self.client.call_tool(self.tool, {self.cfg.argument or "input": text})
        step = _agent_step(
            self.tool,
            result.content,
            input_text=text,
            error=result.content[:300] if result.is_error else "",
        )
        return AgentReply(result.content, steps=[step])

    async def aclose(self) -> None:
        if self.client is not None:
            await self.client.aclose()


# --- OpenAI Responses ---


class ResponsesConnector:
    """``POST /responses``; later turns continue with ``previous_response_id``."""

    def __init__(self, cfg: agent_pb2.ResponsesAgent) -> None:
        if not cfg.base_url or not cfg.model:
            raise TaskError("the responses agent needs base_url and model")
        self.cfg = cfg
        self.name = cfg.model
        headers = {"Content-Type": "application/json"}
        key = os.environ.get(cfg.api_key_env or "OPENAI_API_KEY")
        if key:
            headers["Authorization"] = f"Bearer {key}"
        self.client = httpx.AsyncClient(timeout=_timeout(cfg), headers=headers)
        self.previous: str | None = None

    async def send(self, text: str) -> AgentReply:
        body: dict[str, Any] = {"model": self.cfg.model, "input": text}
        if self.previous:
            body["previous_response_id"] = self.previous
        try:
            response = await self.client.post(
                self.cfg.base_url.rstrip("/") + "/responses", json=body
            )
        except httpx.HTTPError as exc:
            raise TaskError(f"responses agent: {exc}") from exc
        if response.status_code >= 400:
            raise TaskError(f"responses agent: HTTP {response.status_code}: {response.text[:300]}")
        data = response.json()
        self.previous = data.get("id")
        steps: list[Step] = []
        texts: list[str] = []
        for item in data.get("output") or []:
            kind = item.get("type", "")
            if kind == "message":
                part = "".join(
                    c.get("text", "")
                    for c in item.get("content") or []
                    if c.get("type") in ("output_text", "text")
                )
                texts.append(part)
                steps.append(_agent_step(self.cfg.model, part))
            elif kind == "function_call":
                steps.append(
                    Step(
                        type="tool",
                        name=item.get("name", ""),
                        input=Content(text=item.get("arguments", "")),
                        span_id=span_id(),
                    )
                )
            elif kind.endswith("_call"):
                steps.append(
                    Step(
                        type="tool",
                        name=kind.removesuffix("_call"),
                        input=Content(text=json.dumps(item)),
                        span_id=span_id(),
                    )
                )
        usage = data.get("usage") or {}
        return AgentReply(
            "".join(texts),
            steps=steps,
            usage=Usage(
                input_tokens=usage.get("input_tokens"), output_tokens=usage.get("output_tokens")
            ),
            finished=data.get("status") == "failed",
        )

    async def aclose(self) -> None:
        await self.client.aclose()


# --- HTTP ---


class HTTPConnector:
    """Any JSON-over-HTTP agent, described by a body template and response paths."""

    def __init__(self, cfg: agent_pb2.HTTPAgent, task: Task) -> None:
        if not cfg.url:
            raise TaskError("the http agent needs a url")
        if not cfg.output_path:
            raise TaskError(
                "the http agent needs output_path (where the answer is in the response)"
            )
        self.cfg = cfg
        self.task = task
        self.name = cfg.url
        self.client = httpx.AsyncClient(timeout=_timeout(cfg), headers=_headers(cfg.headers_env))
        self.history: list[dict[str, str]] = []

    async def send(self, text: str) -> AgentReply:
        self.history.append({"role": "user", "content": text})
        template = self.cfg.body_template or '{"input": {{input}}}'
        raw = (
            template.replace("{{input}}", json.dumps(text))
            .replace("{{task_id}}", json.dumps(self.task.id))
            .replace("{{messages}}", json.dumps(self.history))
        )
        try:
            body = json.loads(raw)
        except json.JSONDecodeError as exc:
            raise TaskError(
                f"http agent: body_template is not JSON after substitution: {exc}"
            ) from exc
        try:
            response = await self.client.request(self.cfg.method or "POST", self.cfg.url, json=body)
        except httpx.HTTPError as exc:
            raise TaskError(f"http agent {self.cfg.url}: {exc}") from exc
        if response.status_code >= 400:
            raise TaskError(
                f"http agent {self.cfg.url}: HTTP {response.status_code}: {response.text[:300]}"
            )
        data = response.json()
        answer = _get_path(data, self.cfg.output_path)
        if answer is None:
            raise TaskError(f"http agent: nothing at {self.cfg.output_path!r} in the response")
        reply = answer if isinstance(answer, str) else json.dumps(answer)
        self.history.append({"role": "assistant", "content": reply})
        steps = []
        if self.cfg.messages_path:
            messages = _get_path(data, self.cfg.messages_path)
            if isinstance(messages, list):
                steps = steps_from_messages(messages)
        steps.append(_agent_step("http", reply, input_text=text))
        return AgentReply(reply, steps=steps)

    async def aclose(self) -> None:
        await self.client.aclose()


# --- CLI in the sandbox ---


class CLIConnector:
    """A command-line agent run in the task's sandbox, in its workdir."""

    def __init__(self, cfg: agent_pb2.CLIAgent, env: TaskEnvironment | None) -> None:
        if not cfg.command:
            raise TaskError("the cli agent needs a command")
        if env is None:
            raise TaskError(
                "a CLI agent runs inside the task's sandbox, so the run needs an environment"
            )
        self.cfg = cfg
        self.env = env
        self.name = cfg.command[0]

    async def send(self, text: str) -> AgentReply:
        placed = any("{instruction}" in a for a in self.cfg.command)
        argv = [a.replace("{instruction}", text) for a in self.cfg.command]
        extra = {}
        for inside, outside in self.cfg.env_from.items():
            value = os.environ.get(outside)
            if value is None:
                raise TaskError(f"the cli agent needs {outside} on the worker (env_from {inside})")
            extra[inside] = value
        try:
            result = await self.env.sandbox.exec(
                argv,
                stdin="" if placed else text,
                env={**self.env.config.env, **extra},
                timeout_s=_timeout(self.cfg),
            )
        except SandboxError as exc:
            raise TaskError(f"cli agent: {exc}") from exc
        for denial in result.denials:
            from evalsi_harness.events import PolicyEvent

            kind = "egress_denied" if denial.startswith("egress denied") else "denial"
            self.env.policy_events.append(PolicyEvent(kind=kind, detail=denial))
        output = result.stdout.strip()
        error = ""
        if result.outcome == "timeout":
            error = f"timed out after {_timeout(self.cfg):g}s"
        elif result.exit_code != 0:
            error = f"exit {result.exit_code}: {result.stderr.strip()[-500:]}"
        step = Step(
            type="agent",
            name=self.name,
            input=Content(text=shlex.join(argv) if placed else text),
            output=Content(text=output),
            error=error,
            span_id=span_id(),
            duration_ms=result.duration_ms or None,
            attributes={
                "evalsi.agent.exit_code": result.exit_code,
                "evalsi.agent.outcome": result.outcome,
            },
        )
        return AgentReply(
            output,
            steps=[step],
            finished=True,
            extra={"exit_code": result.exit_code, "cli_outcome": result.outcome},
        )

    async def aclose(self) -> None:
        return None


def connector_for(task: Task, env: TaskEnvironment | None) -> RemoteAgent:
    agent = task.spec.target.agent
    kind = agent.WhichOneof("kind")
    if kind == "a2a":
        return A2AConnector(agent.a2a)
    if kind == "mcp":
        return MCPConnector(agent.mcp)
    if kind == "responses":
        return ResponsesConnector(agent.responses)
    if kind == "http":
        return HTTPConnector(agent.http, task)
    if kind == "cli":
        return CLIConnector(agent.cli, env)
    raise TaskError("target.agent needs one of a2a, mcp, responses, http or cli")


async def drive_agent(
    task: Task,
    env: TaskEnvironment | None,
    emit: Emit,
    *,
    timeout_s: float = 0.0,
    user: Any = None,
) -> FinalEvent:
    """Gives a bring-your-own agent the task, and the simulated user's turns if any."""
    agent = connector_for(task, env)
    usage = Usage(input_tokens=0, output_tokens=0)
    reply: AgentReply | None = None
    turns = 0
    try:
        async with asyncio.timeout(timeout_s or None):
            text = task.instruction
            conversation: list[dict[str, Any]] = [{"role": "user", "content": text}]
            while True:
                reply = await agent.send(text)
                turns += 1
                for step in reply.steps:
                    emit(StepEvent(step))
                usage.input_tokens = (usage.input_tokens or 0) + (reply.usage.input_tokens or 0)
                usage.output_tokens = (usage.output_tokens or 0) + (reply.usage.output_tokens or 0)
                conversation.append({"role": "assistant", "content": reply.text})
                if user is None or reply.finished:
                    break
                turn = await user.reply(conversation)
                if turn.message:
                    emit(
                        StepEvent(
                            Step(
                                type="user",
                                name="simulated-user",
                                output=Content(text=turn.message),
                                span_id=span_id(),
                            )
                        )
                    )
                if turn.done or not turn.message:
                    break
                text = turn.message
                conversation.append({"role": "user", "content": text})
    except TimeoutError:
        return FinalEvent(
            output=Content(text=reply.text) if reply else None, usage=usage, stop_reason="timeout"
        )
    finally:
        await agent.aclose()
    assert reply is not None
    extra = {"steps": turns, **reply.extra}
    return FinalEvent(
        output=Content(text=reply.text), usage=usage, stop_reason="completed", extra=extra
    )
