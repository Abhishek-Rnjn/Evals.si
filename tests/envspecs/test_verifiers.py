# ruff: noqa: E402
"""The verifiers adapter, against verifiers itself."""

from __future__ import annotations

import asyncio
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import pytest

vf = pytest.importorskip("verifiers")
pytest.importorskip("datasets")

from conftest import task
from evalsi_harness.envspecs.verifiers import STATE_KEY, TaskVerifiersEnv, load_environment
from evalsi_harness.testing import LocalSandboxClient


def run(coro: Any) -> Any:
    return asyncio.run(coro)


def call(name: str, call_id: str, **arguments: Any) -> Any:
    return vf.AssistantMessage(
        role="assistant",
        content="",
        tool_calls=[vf.ToolCall(id=call_id, name=name, arguments=json.dumps(arguments))],
    )


def new_state(env: TaskVerifiersEnv, index: int = 0) -> Any:
    return {"info": {"task_index": index}, "trajectory": []}


async def act(env: TaskVerifiersEnv, state: Any, name: str, call_id: str, **arguments: Any) -> str:
    messages = [call(name, call_id, **arguments)]
    [reply] = await env.env_response(messages, state)
    assert reply.role == "tool"
    return str(reply.content)


def test_is_a_verifiers_environment_with_the_sandbox_tools() -> None:
    env = load_environment([task("one"), task("two")], client=LocalSandboxClient())
    assert isinstance(env, vf.StatefulToolEnv)
    assert [row["question"] for row in env.get_dataset()] == ["one", "two"]
    assert [row["info"]["task_index"] for row in env.get_dataset()] == [0, 1]
    tools = {t.name: t for t in env.tool_defs}
    assert set(tools) == {"bash", "read_file", "write_file", "submit"}
    # The episode handle is injected, never shown to the model.
    for t in tools.values():
        assert "rollout" not in t.parameters.get("properties", {})
    assert set(tools["write_file"].parameters["required"]) == {"path", "content"}


def test_a_rollout_is_an_episode_rewarded_by_the_checker() -> None:
    async def go() -> None:
        env = load_environment([task()], client=LocalSandboxClient())
        state = new_state(env)
        await env.setup_state(state)
        assert not await env.episode_over(state)
        out = await act(env, state, "bash", "c1", command="cat setup.log notes.txt")
        assert "ready" in out
        assert "draft" in out
        await act(env, state, "write_file", "c2", path="notes.txt", content="done\n")
        assert not await env.episode_over(state)
        assert await env.task_reward(state) == 0.0
        await act(env, state, "submit", "c3")
        assert await env.episode_over(state)
        assert await env.task_reward(state) == 1.0
        assert state[STATE_KEY].info["check"]["passed"] is True
        # A tool call after the end changes nothing.
        assert "over" in await act(env, state, "bash", "c4", command="echo again")
        await env.close_episode(state)
        assert STATE_KEY not in state
        assert state["evalsi_result"]["reward"] == 1.0
        assert await env.task_reward(state) == 1.0

    run(go())


def test_a_failing_end_state_scores_zero() -> None:
    async def go() -> None:
        env = load_environment([task()], client=LocalSandboxClient())
        state = new_state(env)
        await env.setup_state(state)
        await act(env, state, "submit", "c1")
        assert await env.episode_over(state)
        assert await env.task_reward(state) == 0.0
        await env.close_episode(state)

    run(go())


def test_a_checker_that_cannot_run_is_an_infra_error_not_a_zero() -> None:
    async def go() -> None:
        broken = {"command": ["sh", "-c", "exit 0"], "parser": "json"}
        env = load_environment([task(checker=broken)], client=LocalSandboxClient())
        state = new_state(env)
        await env.setup_state(state)
        with pytest.raises(vf.ToolCallError) as caught:
            await act(env, state, "submit", "c1")
        assert isinstance(caught.value.__cause__, vf.InfraError)
        await env.close_episode(state)

    run(go())


def test_rollouts_of_different_tasks_do_not_share_a_sandbox() -> None:
    async def go() -> None:
        env = load_environment([task("a"), task("b")], client=LocalSandboxClient())
        first, second = new_state(env, 0), new_state(env, 1)
        await env.setup_state(first)
        await env.setup_state(second)
        await act(env, first, "write_file", "c1", path="notes.txt", content="done\n")
        assert "draft" in await act(env, second, "read_file", "c2", path="notes.txt")
        await env.close_episode(first)
        await env.close_episode(second)

    run(go())


class ChatServer:
    """A chat-completions server in the full response shape verifiers' OpenAI
    client validates (the harness's scripted server sends only what its own client reads)."""

    def __init__(self, script: Any) -> None:
        self.requests: list[dict[str, Any]] = []
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args: Any) -> None:
                pass

            def do_POST(self) -> None:
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                outer.requests.append(body)
                name, arguments = script(body["messages"])
                call = {
                    "id": f"call_{len(outer.requests)}",
                    "type": "function",
                    "function": {"name": name, "arguments": json.dumps(arguments)},
                }
                reply = {
                    "id": f"chatcmpl-{len(outer.requests)}",
                    "object": "chat.completion",
                    "created": 0,
                    "model": body["model"],
                    "choices": [
                        {
                            "index": 0,
                            "finish_reason": "tool_calls",
                            "message": {"role": "assistant", "content": None, "tool_calls": [call]},
                        }
                    ],
                    "usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
                }
                data = json.dumps(reply).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        self._server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.base_url = f"http://127.0.0.1:{self._server.server_address[1]}/v1"

    def __enter__(self) -> ChatServer:
        threading.Thread(target=self._server.serve_forever, daemon=True).start()
        return self

    def __exit__(self, *exc: object) -> None:
        self._server.shutdown()
        self._server.server_close()


def test_a_full_rollout_against_a_model_server(monkeypatch: pytest.MonkeyPatch) -> None:
    """verifiers drives the environment itself: it calls a chat-completions
    server, the model's tool calls run in the sandbox, and the rubric scores."""

    def model(messages: list[dict[str, Any]]) -> tuple[str, dict[str, Any]]:
        turn = sum(1 for m in messages if m["role"] == "assistant")
        script = [("write_file", {"path": "notes.txt", "content": "done\n"}), ("submit", {})]
        return script[min(turn, len(script) - 1)]

    monkeypatch.setenv("EVALSI_TEST_KEY", "k")
    with ChatServer(model) as server:
        env = load_environment([task()], client=LocalSandboxClient())
        config = vf.ClientConfig(api_base_url=server.base_url, api_key_var="EVALSI_TEST_KEY")
        out = run(env.evaluate(config, "scripted", num_examples=1, rollouts_per_example=1))
    [rollout] = out["outputs"]
    assert rollout["reward"] == 1.0
    assert rollout["error"] is None
    offered = {t["function"]["name"] for t in server.requests[0]["tools"]}
    assert offered == {"bash", "read_file", "write_file", "submit"}
    assert len(server.requests) == 2  # write, then submit: the episode ended the rollout
