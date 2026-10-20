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
import re
import shlex
import time
import uuid
from collections.abc import AsyncIterator, Mapping
from dataclasses import dataclass, field
from typing import Any, Protocol
from urllib.parse import quote

import httpx

from evalsi.sandbox import SandboxError
from evalsi.targets import api_key
from evalsi.types import Content, Message, Step, ToolCall, Trajectory, Usage
from evalsi.v1alpha1 import agent_pb2
from evalsi_harness.environment import TaskEnvironment
from evalsi_harness.events import Emit, FinalEvent, StepEvent, span_id
from evalsi_harness.formats import parse_output
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


def _usage(data: Any) -> Usage:
    """Token usage as an agent reported it, in Evals.si or OpenAI Chat names."""
    if not isinstance(data, Mapping):
        return Usage()
    try:
        usage = Usage.from_dict(data)
        if usage.input_tokens is None and data.get("prompt_tokens") is not None:
            usage.input_tokens = int(data["prompt_tokens"])
        if usage.output_tokens is None and data.get("completion_tokens") is not None:
            usage.output_tokens = int(data["completion_tokens"])
    except (TypeError, ValueError) as exc:
        raise TaskError(f"http agent: the usage is malformed: {exc}") from exc
    return usage


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
MAX_STREAM_EVENTS = 10_000


async def _sse_events(response: httpx.Response) -> AsyncIterator[str]:
    """Server-sent events: each is its ``data:`` lines, joined with newlines, up
    to a blank line (or the end of the stream)."""
    data_lines: list[str] = []
    async for raw in response.aiter_lines():
        if raw == "":
            if data_lines:
                yield "\n".join(data_lines)
                data_lines.clear()
        elif raw.startswith("data:"):
            data_lines.append(raw[5:].removeprefix(" "))
    if data_lines:
        yield "\n".join(data_lines)


# The HTTP+JSON binding speaks A2A 0.3's proto JSON: ROLE_USER, TASK_STATE_*,
# parts without "kind", and responses wrapped as {"task": ...} or {"message":
# ...}. Its payloads are put in the JSON-RPC binding's shape, so one reader
# serves both.


def _rest_state(state: Any) -> str:
    out = str(state or "").lower().removeprefix("task_state_").replace("_", "-")
    return "canceled" if out == "cancelled" else out


def _rest_parts(parts: Any) -> list[dict[str, Any]]:
    out: list[dict[str, Any]] = []
    for p in parts or []:
        if not isinstance(p, Mapping):
            continue
        if "kind" in p:
            out.append(dict(p))
        else:
            kind = next((k for k in ("text", "data", "file") if k in p), "")
            if kind:
                out.append({"kind": kind, kind: p[kind]})
    return out


def _rest_message(m: Mapping[str, Any]) -> dict[str, Any]:
    role = str(m.get("role", "")).lower().removeprefix("role_")
    parts = m.get("parts", m.get("content"))
    return {**m, "kind": "message", "role": role, "parts": _rest_parts(parts)}


def _rest_status(status: Mapping[str, Any]) -> dict[str, Any]:
    out = {**status, "state": _rest_state(status.get("state"))}
    message = status.get("message", status.get("update"))
    if isinstance(message, Mapping):
        out["message"] = _rest_message(message)
    return out


def _rest_artifact(artifact: Mapping[str, Any]) -> dict[str, Any]:
    return {**artifact, "parts": _rest_parts(artifact.get("parts"))}


def _rest_task(task: Mapping[str, Any]) -> dict[str, Any]:
    out = {**task, "kind": "task"}
    if isinstance(task.get("status"), Mapping):
        out["status"] = _rest_status(task["status"])
    out["artifacts"] = [
        _rest_artifact(a) for a in task.get("artifacts") or [] if isinstance(a, Mapping)
    ]
    out["history"] = [_rest_message(m) for m in task.get("history") or [] if isinstance(m, Mapping)]
    return out


def _from_rest(payload: Any) -> Any:
    """A SendMessageResponse, StreamResponse or Task in the HTTP+JSON binding,
    as the JSON-RPC binding's result."""
    if not isinstance(payload, Mapping):
        return payload
    if isinstance(payload.get("task"), Mapping):
        return _rest_task(payload["task"])
    for key in ("message", "msg"):
        if isinstance(payload.get(key), Mapping):
            return _rest_message(payload[key])
    update = payload.get("statusUpdate", payload.get("status_update"))
    if isinstance(update, Mapping):
        out = {**update, "kind": "status-update"}
        if isinstance(update.get("status"), Mapping):
            out["status"] = _rest_status(update["status"])
        return out
    update = payload.get("artifactUpdate", payload.get("artifact_update"))
    if isinstance(update, Mapping):
        out = {**update, "kind": "artifact-update"}
        if isinstance(update.get("artifact"), Mapping):
            out["artifact"] = _rest_artifact(update["artifact"])
        return out
    if "status" in payload:  # a bare Task, as GET /v1/tasks/{id} returns it
        return _rest_task(payload)
    if "role" in payload:
        return _rest_message(payload)
    return payload


def _rest_request(message: Mapping[str, Any]) -> dict[str, Any]:
    """A user message in the HTTP+JSON binding."""
    out: dict[str, Any] = {
        "messageId": message["messageId"],
        "role": "ROLE_USER",
        "content": [{"text": p["text"]} for p in message["parts"]],
    }
    for key in ("contextId", "taskId"):
        if key in message:
            out[key] = message[key]
    return out


class A2AConnector:
    """Agent2Agent: ``message/send``, following the task until it settles, over
    the JSON-RPC binding or the HTTP+JSON one."""

    def __init__(self, cfg: agent_pb2.A2AAgent) -> None:
        binding = cfg.binding.upper() or "JSONRPC"
        if binding not in ("JSONRPC", "HTTP+JSON"):
            raise TaskError(f"a2a binding {cfg.binding!r}: use JSONRPC or HTTP+JSON")
        self.rest = binding == "HTTP+JSON"
        self.url = cfg.url
        self.name = cfg.url
        self.client = httpx.AsyncClient(timeout=_timeout(cfg), headers=_headers(cfg.headers_env))
        self.stream = cfg.stream
        self.context_id: str | None = None
        self.task_id: str | None = None
        self._task_roots: dict[str, Step] = {}
        self._linked_tasks: set[str] = set()

    def _link_tasks(self, steps: list[Step]) -> list[Step]:
        """A2A has no standard parent task field. Honor an explicit metadata
        extension only: metadata.evalsi.parent_task_id; never infer a handoff
        from referenceTaskIds, which can be ordinary references."""
        added: list[Step] = []
        for step in steps:
            task = step.attributes.get("a2a.task_id")
            if isinstance(task, str) and task and task not in self._task_roots:
                self._task_roots[task] = step
        for step in steps:
            task = step.attributes.get("a2a.task_id")
            if not isinstance(task, str) or task not in self._task_roots:
                continue
            root = self._task_roots[task]
            if step is not root:
                step.parent_span_id = root.span_id
            parent = step.attributes.get("a2a.parent_task_id")
            if isinstance(parent, str) and parent and task not in self._linked_tasks:
                source = self._task_roots.get(parent)
                handoff = Step(
                    type="handoff",
                    name="a2a:handoff",
                    span_id=span_id(),
                    parent_span_id=source.span_id if source else "",
                    attributes={"a2a.task_id": task, "a2a.parent_task_id": parent},
                )
                root.parent_span_id = handoff.span_id
                self._linked_tasks.add(task)
                added.append(handoff)
        return [*added, *steps]

    @staticmethod
    def _task_attributes(result: Mapping[str, Any], task_id: str) -> dict[str, Any]:
        attrs: dict[str, Any] = {"a2a.task_id": task_id}
        metadata = result.get("metadata")
        ext = metadata.get("evalsi") if isinstance(metadata, Mapping) else None
        parent = ext.get("parent_task_id") if isinstance(ext, Mapping) else None
        if isinstance(parent, str) and parent:
            attrs["a2a.parent_task_id"] = parent
        return attrs

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

    async def _call(self, method: str, params: dict[str, Any]) -> Any:
        """``message/send`` or ``tasks/get`` over the configured binding."""
        if not self.rest:
            return await self._rpc(method, params)
        base = self.url.rstrip("/")
        try:
            if method == "message/send":
                body = {
                    "message": _rest_request(params["message"]),
                    "configuration": params.get("configuration") or {},
                }
                response = await self.client.post(f"{base}/v1/message:send", json=body)
            else:
                task = quote(str(params["id"]), safe="")
                response = await self.client.get(f"{base}/v1/tasks/{task}")
        except httpx.HTTPError as exc:
            raise TaskError(f"A2A agent {self.url}: {exc}") from exc
        if response.status_code >= 400:
            raise TaskError(
                f"A2A agent {self.url}: HTTP {response.status_code}: {response.text[:300]}"
            )
        return _from_rest(response.json())

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
        if self.stream:
            return await self._stream(message, text)
        result = await self._call(
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
                result = await self._call("tasks/get", {"id": result["id"]})
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
            for step in steps:
                step.attributes.update(self._task_attributes(result, str(result.get("id", ""))))
            return AgentReply(
                reply,
                steps=self._link_tasks(steps),
                finished=state in TERMINAL,
                extra={"a2a_state": state},
            )
        if isinstance(result, Mapping):
            self.context_id = result.get("contextId") or self.context_id
            reply = _a2a_text(result.get("parts"))
            return AgentReply(reply, steps=[_agent_step("a2a", reply, input_text=text)])
        raise TaskError(f"A2A agent {self.url}: unexpected result {result!r:.200}")

    async def _stream(self, message: dict[str, Any], text: str) -> AgentReply:  # noqa: PLR0915
        """``message/stream``: every status update is a step, artifact chunks are
        put back together, and the stream's last event settles the task."""
        url = self.url
        body: dict[str, Any] = {
            "jsonrpc": "2.0",
            "id": str(uuid.uuid4()),
            "method": "message/stream",
            "params": {"message": message},
        }
        if self.rest:
            url = f"{self.url.rstrip('/')}/v1/message:stream"
            body = {"message": _rest_request(message)}
        steps: list[Step] = []
        artifacts: dict[str, list[str]] = {}
        state = ""
        last_text = ""
        task_id = ""
        final = False
        events = 0
        try:
            async with self.client.stream(
                "POST", url, json=body, headers={"Accept": "text/event-stream"}
            ) as response:
                if response.status_code >= 400:
                    detail = (await response.aread()).decode(errors="replace")[:300]
                    raise TaskError(f"A2A agent {self.url}: HTTP {response.status_code}: {detail}")
                async for block in _sse_events(response):
                    events += 1
                    if events > MAX_STREAM_EVENTS:
                        raise TaskError(
                            f"A2A agent {self.url}: more than {MAX_STREAM_EVENTS} events"
                        )
                    try:
                        payload = json.loads(block)
                    except json.JSONDecodeError:
                        continue
                    if not isinstance(payload, Mapping):
                        continue
                    if "error" in payload:
                        err = payload["error"]
                        detail = err.get("message", err) if isinstance(err, Mapping) else err
                        raise TaskError(f"A2A agent {self.url}: {detail}")
                    result = _from_rest(payload) if self.rest else payload.get("result")
                    if not isinstance(result, Mapping):
                        continue
                    self.context_id = result.get("contextId") or self.context_id
                    kind = result.get("kind", "")
                    if kind == "artifact-update":
                        artifact = result.get("artifact") or {}
                        chunk = _a2a_text(artifact.get("parts"))
                        key = str(artifact.get("artifactId", ""))
                        parts = artifacts.setdefault(key, [])
                        if result.get("append"):
                            parts.append(chunk)
                        else:
                            parts[:] = [chunk]
                        continue
                    if kind == "message":
                        last_text = _a2a_text(result.get("parts")) or last_text
                        steps.append(_agent_step("a2a:message", last_text, input_text=text))
                        final = True
                        continue
                    status = result.get("status") if kind in ("status-update", "task") else None
                    if not isinstance(status, Mapping):
                        continue
                    task_id = str(result.get("taskId") or result.get("id") or task_id)
                    state = str(status.get("state", state))
                    last_text = _a2a_text((status.get("message") or {}).get("parts")) or last_text
                    step = _agent_step(
                        f"a2a:{state}",
                        last_text if status.get("message") else "",
                        input_text=text if not steps else "",
                        error=state if state == "failed" else "",
                    )
                    step.attributes.update(
                        {"a2a.state": state, **self._task_attributes(result, task_id)}
                    )
                    steps.append(step)
                    if kind == "task":  # a snapshot may carry the artifacts so far
                        for art in result.get("artifacts") or []:
                            artifacts[str(art.get("artifactId", ""))] = [
                                _a2a_text(art.get("parts"))
                            ]
                    if result.get("final") or state in (*TERMINAL, "input-required"):
                        final = True
        except httpx.HTTPError as exc:
            raise TaskError(f"A2A agent {self.url}: {exc}") from exc
        if not final:
            raise TaskError(f"A2A agent {self.url}: the stream ended before the task settled")
        self.task_id = task_id if state == "input-required" else None
        chunks = ["".join(p) for p in artifacts.values()]
        reply = "\n".join(c for c in chunks if c) or last_text
        if steps and not (steps[-1].output and steps[-1].output.as_text()):
            steps[-1].output = Content(text=reply)
        return AgentReply(
            reply,
            steps=self._link_tasks(steps),
            finished=state in TERMINAL or not state,
            extra={"a2a_state": state or "completed", "a2a_events": events},
        )

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
        key = api_key(cfg.api_key_env, "OPENAI_API_KEY")
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
    """Any JSON-over-HTTP agent, described by a body template and response paths.
    With ``stream``, the response is server-sent events, each a JSON object."""

    def __init__(self, cfg: agent_pb2.HTTPAgent, task: Task) -> None:
        if not cfg.url:
            raise TaskError("the http agent needs a url")
        if cfg.delta_path and not cfg.stream:
            raise TaskError("the http agent's delta_path reads a stream: set stream too")
        if not cfg.output_path and not cfg.delta_path:
            raise TaskError(
                "the http agent needs output_path (where the answer is in the response), "
                "or stream with delta_path"
            )
        self.cfg = cfg
        self.task = task
        self.name = cfg.url
        self.client = httpx.AsyncClient(timeout=_timeout(cfg), headers=_headers(cfg.headers_env))
        self.history: list[dict[str, str]] = []
        # One conversation per task and trial: trials must not share an agent thread.
        self.session_id = f"{task.run_id or 'run'}/{task.id}/{task.trial}"
        # The agent's own session id, once it reports one; later turns may not.
        self.reported_session = ""

    async def send(self, text: str) -> AgentReply:
        self.history.append({"role": "user", "content": text})
        template = self.cfg.body_template or '{"input": {{input}}}'
        values = {
            "input": text,
            "task_id": self.task.id,
            "messages": self.history,
            "trial": self.task.trial,
            "session_id": self.session_id,
        }
        raw = re.sub(
            r"\{\{(input|task_id|messages|trial|session_id)\}\}",
            lambda match: json.dumps(values[match[1]]),
            template,
        )
        try:
            body = json.loads(raw)
        except json.JSONDecodeError as exc:
            raise TaskError(
                f"http agent: body_template is not JSON after substitution: {exc}"
            ) from exc
        events = await (self._stream(body) if self.cfg.stream else self._request(body))
        answer = self._last(events, self.cfg.output_path) if self.cfg.output_path else None
        if answer is None and self.cfg.delta_path:
            chunks = [_get_path(e, self.cfg.delta_path) for e in events]
            if any(isinstance(c, str) for c in chunks):
                answer = "".join(c for c in chunks if isinstance(c, str))
        if answer is None:
            where = self.cfg.output_path or self.cfg.delta_path
            raise TaskError(f"http agent: nothing at {where!r} in the response")
        reply = answer if isinstance(answer, str) else json.dumps(answer)
        self.history.append({"role": "assistant", "content": reply})
        steps = []
        if self.cfg.messages_path:
            messages = self._last(events, self.cfg.messages_path)
            if isinstance(messages, list):
                steps = steps_from_messages(messages)
        extra: dict[str, Any] = {}
        if self.cfg.trajectory_path:
            reported = [_get_path(e, self.cfg.trajectory_path) for e in events]
            # Not every event in a stream reports steps, but one must.
            if not any(v is not None for v in reported):
                raise TaskError(
                    f"http agent: no trajectory at {self.cfg.trajectory_path!r} in the response"
                )
            for value in reported:
                if value is None:
                    continue
                trajectory = self._trajectory(value)
                steps.extend(trajectory.steps)
                self.reported_session = trajectory.session_id or self.reported_session
        if self.reported_session:
            extra["session_id"] = self.reported_session
        usage = Usage()
        if self.cfg.usage_path:
            usage = _usage(self._last(events, self.cfg.usage_path))
        if self.cfg.stream:
            extra["http_events"] = len(events)
        steps.append(_agent_step("http", reply, input_text=text))
        return AgentReply(reply, steps=steps, usage=usage, extra=extra)

    @staticmethod
    def _last(events: list[Any], path: str) -> Any:
        """The value at path in the last event that has one."""
        for event in reversed(events):
            value = _get_path(event, path)
            if value is not None:
                return value
        return None

    def _trajectory(self, value: Any) -> Trajectory:
        """Steps the agent reported: a list of steps, {"steps": [...]} or one step."""
        if isinstance(value, list):
            value = {"steps": value}
        elif isinstance(value, Mapping) and "steps" not in value and "type" in value:
            value = {"steps": [value]}
        if not isinstance(value, Mapping):
            raise TaskError(
                f"http agent: no trajectory at {self.cfg.trajectory_path!r} in the response"
            )
        try:
            trajectory = Trajectory.from_dict(value)
        except (AttributeError, TypeError, ValueError) as exc:
            raise TaskError(f"http agent: the trajectory is malformed: {exc}") from exc
        for step in trajectory.steps:
            step.span_id = step.span_id or span_id()
        return trajectory

    async def _request(self, body: Any) -> list[Any]:
        try:
            response = await self.client.request(self.cfg.method or "POST", self.cfg.url, json=body)
        except httpx.HTTPError as exc:
            raise TaskError(f"http agent {self.cfg.url}: {exc}") from exc
        if response.status_code >= 400:
            raise TaskError(
                f"http agent {self.cfg.url}: HTTP {response.status_code}: {response.text[:300]}"
            )
        return [response.json()]

    async def _stream(self, body: Any) -> list[Any]:
        events: list[Any] = []
        blocks = 0
        try:
            async with self.client.stream(
                self.cfg.method or "POST",
                self.cfg.url,
                json=body,
                headers={"Accept": "text/event-stream"},
            ) as response:
                if response.status_code >= 400:
                    detail = (await response.aread()).decode(errors="replace")[:300]
                    raise TaskError(
                        f"http agent {self.cfg.url}: HTTP {response.status_code}: {detail}"
                    )
                async for block in _sse_events(response):
                    blocks += 1  # every event counts, kept or not
                    if blocks > MAX_STREAM_EVENTS:
                        raise TaskError(
                            f"http agent {self.cfg.url}: more than {MAX_STREAM_EVENTS} events"
                        )
                    try:
                        payload = json.loads(block)
                    except json.JSONDecodeError:
                        continue  # a sentinel such as [DONE]
                    if isinstance(payload, Mapping):
                        events.append(payload)
        except httpx.HTTPError as exc:
            raise TaskError(f"http agent {self.cfg.url}: {exc}") from exc
        return events

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
        steps: list[Step] = []
        usage = Usage()
        try:
            parsed = parse_output(self.cfg.output_format, result.stdout)
        except ValueError as exc:
            raise TaskError(f"cli agent: {exc}") from exc
        if parsed is not None:
            # The agent reported its run: its steps are the trajectory, and the
            # raw event stream is not repeated as the answer.
            if parsed.wrong_format and not parsed.steps:
                # A failed process may exit before it emits any structured events.
                raise TaskError(f"cli agent: {error or parsed.error}")
            output, steps, usage = parsed.answer, parsed.steps, parsed.usage
            error = error or parsed.error
        steps.append(
            Step(
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
        )
        return AgentReply(
            output,
            steps=steps,
            usage=usage,
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
                if reply.usage.cost_usd is not None:  # unknown stays unknown, zero is a price
                    usage.cost_usd = (usage.cost_usd or 0.0) + reply.usage.cost_usd
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
