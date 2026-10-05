"""Record and replay of model calls and tool I/O.

Recording captures every model turn and every tool result of a task trial in
a cassette (JSON lines). Replaying serves them back, so a trajectory can be
re-scored with new evaluators without re-running the agent, debugged
deterministically, or branched: with ``branch_at_step=N`` the first N model
turns (and their tool results) are replayed and the run continues live from
there, for counterfactual evaluation.


Tools that change the sandbox are re-executed during replay, so the
environment ends in the same state the recording did.
"""

from __future__ import annotations

import json
from collections import defaultdict, deque
from pathlib import Path
from typing import Any

from evalsi.types import Usage
from evalsi_harness.models import ChatModel, Message, ModelTurn, ToolCallRequest, ToolSpec
from evalsi_harness.tools import Tool, ToolResult


class CassetteError(RuntimeError):
    pass


def _args_key(args: dict[str, Any]) -> str:
    return json.dumps(args, sort_keys=True, separators=(",", ":"))


def _turn_to_dict(turn: ModelTurn) -> dict[str, Any]:
    return {
        "text": turn.text,
        "tool_calls": [
            {"id": c.id, "name": c.name, "arguments": c.arguments} for c in turn.tool_calls
        ],
        "usage": turn.usage.to_dict(),
        "stop_reason": turn.stop_reason,
    }


def _turn_from_dict(data: dict[str, Any]) -> ModelTurn:
    return ModelTurn(
        text=data.get("text", ""),
        tool_calls=[ToolCallRequest(**c) for c in data.get("tool_calls") or []],
        usage=Usage.from_dict(data.get("usage") or {}),
        stop_reason=data.get("stop_reason", ""),
    )


class Cassette:
    """One task trial's recording, and the replay cursor over it."""

    def __init__(self, path: Path, mode: str, *, branch_at_step: int = 0) -> None:
        if mode not in ("record", "replay"):
            raise ValueError(f"recording mode must be off, record or replay, not {mode!r}")
        self.path = path
        self.mode = mode
        self.branch_at = branch_at_step
        self.entries: list[dict[str, Any]] = []
        self._turns: deque[dict[str, Any]] = deque()
        self._tools: dict[tuple[str, str], deque[dict[str, Any]]] = defaultdict(deque)
        self.turns_served = 0
        self.live = mode == "record"
        if mode == "replay":
            if not path.exists():
                raise CassetteError(f"no cassette to replay at {path}")
            for line in path.read_text(encoding="utf-8").splitlines():
                if not line.strip():
                    continue
                entry = json.loads(line)
                if entry["kind"] == "model":
                    self._turns.append(entry["turn"])
                elif entry["kind"] == "tool":
                    self._tools[(entry["name"], _args_key(entry["args"]))].append(entry["result"])

    def record(self, entry: dict[str, Any]) -> None:
        if self.mode == "record":
            self.entries.append(entry)

    def next_turn(self) -> ModelTurn | None:
        """The next recorded turn, or None once the run has gone live."""
        if self.live:
            return None
        if self.branch_at and self.turns_served >= self.branch_at:
            self.live = True
            return None
        if not self._turns:
            raise CassetteError(
                f"cassette {self.path.name} has only {self.turns_served} model turns; "
                "set branch_at_step to continue live"
            )
        self.turns_served += 1
        return _turn_from_dict(self._turns.popleft())

    def tool_result(self, name: str, args: dict[str, Any]) -> ToolResult | None:
        if self.live:
            return None
        queue = self._tools.get((name, _args_key(args)))
        if not queue:
            raise CassetteError(
                f"cassette {self.path.name} has no result for {name}({_args_key(args)})"
            )
        data = queue.popleft()
        return ToolResult(
            data["content"],
            is_error=data.get("is_error", False),
            metadata=data.get("metadata") or {},
        )

    def save(self) -> None:
        if self.mode != "record":
            return
        self.path.parent.mkdir(parents=True, exist_ok=True)
        with self.path.open("w", encoding="utf-8") as f:
            for entry in self.entries:
                f.write(json.dumps(entry) + "\n")


class CassetteModel:
    """A model that records through, or replays and then (after a branch) goes live."""

    def __init__(self, cassette: Cassette, live: ChatModel | None) -> None:
        self.cassette = cassette
        self.inner = live
        self.name = live.name if live is not None else "replay"

    async def complete(
        self, messages: list[Message], tools: list[ToolSpec], *, system: str = ""
    ) -> ModelTurn:
        turn = self.cassette.next_turn()
        if turn is None:
            if self.inner is None:
                raise CassetteError(
                    "replay reached the branch point but no live model is configured"
                )
            turn = await self.inner.complete(messages, tools, system=system)
        self.cassette.record({"kind": "model", "turn": _turn_to_dict(turn)})
        return turn

    async def aclose(self) -> None:
        if self.inner is not None:
            await self.inner.aclose()


class CassetteTool:
    """Records a tool's results, or serves recorded ones.

    Sandbox tools are re-executed during replay (``reexecute``): the agent
    sees the recorded result, but the environment is rebuilt for real, so the
    checker grades the same end state and a branch continues from it."""

    def __init__(self, cassette: Cassette, inner: Tool, *, reexecute: bool = False) -> None:
        self.cassette = cassette
        self.inner = inner
        self.spec = inner.spec
        self.reexecute = reexecute

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        result = self.cassette.tool_result(self.spec.name, args)
        if result is None:
            result = await self.inner.call(args, span=span)
        elif self.reexecute:
            await self.inner.call(args, span=span)
        self.cassette.record(
            {
                "kind": "tool",
                "name": self.spec.name,
                "args": args,
                "result": {
                    "content": result.content,
                    "is_error": result.is_error,
                    "metadata": result.metadata,
                },
            }
        )
        return result
