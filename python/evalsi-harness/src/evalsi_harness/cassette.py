"""Record and replay of model calls and tool I/O.

Recording captures every model turn and every tool result of a task trial in
a cassette (JSON lines). Replaying serves them back, so a trajectory can be
re-scored with new evaluators without re-running the agent, debugged
deterministically, or branched: with ``branch_at_step=N`` the first N model
turns (and their tool results) are replayed and the run continues live from
there, for counterfactual evaluation.

A cassette is also a session log: each entry is written as it happens, so a
trial that dies leaves everything up to its last step. ``resume`` replays that
log and carries on live, appending to it. ``fork`` replays its first N turns
and goes live into a new log that holds the prefix and the new continuation and
names the log it forked from, so logs form a tree (``session_tree``).

Tools that change the sandbox are re-executed during replay, so the
environment ends in the same state the recording did. That is also how a
resumed or forked trial gets its sandbox back.
"""

from __future__ import annotations

import json
import os
from collections import defaultdict, deque
from collections.abc import Callable
from pathlib import Path
from typing import Any

from evalsi.types import Usage
from evalsi_harness.models import (
    ChatModel,
    Message,
    ModelError,
    ModelTurn,
    ToolCallRequest,
    ToolSpec,
)
from evalsi_harness.tools import Tool, ToolResult
from evalsi_harness.user_sim import UserSimulator, UserTurn

CASSETTE_VERSION = 3
EMPTY_TOOLSET: dict[str, Any] = {"builtin": [], "delegate_steps": 0}


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
        **({"thinking": turn.thinking} if turn.thinking else {}),
        **({"cache_read_tokens": turn.cache_read_tokens} if turn.cache_read_tokens else {}),
        **({"cache_write_tokens": turn.cache_write_tokens} if turn.cache_write_tokens else {}),
        **({"reasoning_tokens": turn.reasoning_tokens} if turn.reasoning_tokens else {}),
        **({"model": turn.model} if turn.model else {}),
        **({"switched_from": turn.switched_from} if turn.switched_from else {}),
    }


def _turn_from_dict(data: dict[str, Any]) -> ModelTurn:
    return ModelTurn(
        text=data.get("text", ""),
        tool_calls=[ToolCallRequest(**c) for c in data.get("tool_calls") or []],
        usage=Usage.from_dict(data.get("usage") or {}),
        stop_reason=data.get("stop_reason", ""),
        thinking=data.get("thinking") or [],
        cache_read_tokens=data.get("cache_read_tokens", 0),
        cache_write_tokens=data.get("cache_write_tokens", 0),
        reasoning_tokens=data.get("reasoning_tokens", 0),
        model=data.get("model", ""),
        switched_from=data.get("switched_from", ""),
    )


MODES = ("record", "replay", "resume", "fork")


class Cassette:
    """One task trial's session log, and the replay cursor over it.

    ``path`` is the log. ``record`` writes it anew; ``replay`` only reads;
    ``resume`` reads it and appends what happens live; ``fork`` reads it and
    writes the new log ``fork_to``."""

    def __init__(
        self,
        path: Path,
        mode: str,
        *,
        branch_at_step: int = 0,
        context: dict[str, Any] | None = None,
        hooks: list[Any] | None = None,
        toolset: dict[str, Any] | None = None,
        fork_to: Path | None = None,
        user_simulator: dict[str, Any] | None = None,
    ) -> None:
        if mode not in MODES:
            raise ValueError(
                f"recording mode must be off, record, replay, resume or fork, not {mode!r}"
            )
        if mode == "fork" and (branch_at_step <= 0 or fork_to is None):
            raise ValueError("fork needs branch_at_step above 0 and a log to write")
        self.path = path
        self.mode = mode
        self.branch_at = branch_at_step
        self.fork_to = fork_to
        self._turns: deque[dict[str, Any]] = deque()
        self._tools: dict[tuple[str, str], deque[dict[str, Any]]] = defaultdict(deque)
        self._users: dict[str, deque[dict[str, Any]]] = defaultdict(deque)
        self.user_simulator = user_simulator
        self.turns_served = 0
        # The context strategy the recording is valid for: a replay under
        # another one would send the model different conversations.
        self.context = context if context is not None else {"strategy": "none"}
        # Hooks change what the agent sees and does, so they are part of what a
        # recording is valid for, too.
        self.hooks = hooks or []
        # Which optional tools the agent had: they change what it can do and the
        # sequence of calls, so a replay without them would rebuild another sandbox.
        self.toolset = toolset if toolset is not None else EMPTY_TOOLSET
        self._next_id = 0
        self._last_id = -1
        self._out: Any = None
        # Whether this log's file has been created: after that it is only appended to.
        self._started = False
        # A resume with no log yet is just a recording.
        self._fresh = mode == "record" or (mode == "resume" and not self._has_log())
        self.live = self._fresh
        if not self._fresh:
            self._load()

    def _has_log(self) -> bool:
        return self.path.exists() and self.path.stat().st_size > 0

    def _load(self) -> None:
        if not self.path.exists():
            raise CassetteError(f"no cassette to replay at {self.path}")
        recorded: dict[str, Any] = {"strategy": "none"}  # version 1 files have no header
        recorded_hooks: list[Any] = []
        recorded_toolset: dict[str, Any] = EMPTY_TOOLSET
        recorded_user: dict[str, Any] | None = None
        good: list[str] = []
        lines = [ln for ln in self.path.read_text(encoding="utf-8").splitlines() if ln.strip()]
        for line in lines:
            try:
                entry = json.loads(line)
            except json.JSONDecodeError:
                # A trial killed mid-write leaves a half line at the end; the
                # entries before it are good.
                if self.mode == "resume":
                    break
                raise CassetteError(
                    f"cassette {self.path.name} is damaged: {line[:80]!r}"
                ) from None
            good.append(line)
            if entry["kind"] == "header":
                recorded = entry.get("context") or {"strategy": "none"}
                recorded_hooks = entry.get("hooks") or []
                recorded_toolset = entry.get("toolset") or EMPTY_TOOLSET
                recorded_user = entry.get("user_simulator")
            else:
                self._next_id = max(self._next_id, entry.get("id", self._next_id) + 1)
                self._last_id = entry.get("id", self._last_id)
                if entry["kind"] == "model":
                    self._turns.append(entry["turn"])
                elif entry["kind"] == "tool":
                    key = (entry["name"], _args_key(entry["args"]))
                    self._tools[key].append(entry["result"])
                elif entry["kind"] == "user":
                    self._users[entry["conversation"]].append(entry["result"])
        if recorded_user != self.user_simulator:
            raise CassetteError(
                f"cassette {self.path.name} needs the same recorded user simulator; "
                "older logs without user turns cannot replay simulated-user tasks"
            )
        if self.mode == "resume":
            # A refusal at the very end is where the trial died (a provider that went away),
            # not an outcome the run handled: resuming tries that call again.
            while (
                good
                and json.loads(good[-1]).get("kind") == "model"
                and self._turns
                and "error" in self._turns[-1]
            ):
                self._turns.pop()
                dropped = good.pop()
                self._next_id = max(self._next_id - 1, 0)
                self._last_id = json.loads(good[-1]).get("id", -1) if good else -1
                del dropped
        if recorded_hooks != self.hooks:
            raise CassetteError(
                f"cassette {self.path.name} was recorded with hooks {recorded_hooks}, but the spec "
                f"lists {self.hooks}; a replay needs the same hooks in the same order"
            )
        if recorded_toolset != self.toolset:
            raise CassetteError(
                f"cassette {self.path.name} was recorded with the tools {recorded_toolset}, but "
                f"the spec has {self.toolset}; a replay needs the same tools"
            )
        if recorded != self.context:
            raise CassetteError(
                f"cassette {self.path.name} was recorded with context management "
                f"{recorded}, but the spec asks for {self.context}; "
                "a replay needs the same strategy and window"
            )
        if self.mode == "resume" and good != lines:
            # Drop a half-written last line (and a refusal the trial died on), so appending
            # continues a valid log. The repaired log replaces the old one in one step: a
            # kill in the middle leaves the whole old log, never a truncated one.
            tmp = self.path.with_suffix(self.path.suffix + ".tmp")
            tmp.write_text("\n".join(good) + "\n" if good else "", encoding="utf-8")
            os.replace(tmp, self.path)

    # --- writing ---

    def _writer(self) -> Any:
        if self._out is None:
            target = self.fork_to if self.mode == "fork" else self.path
            assert target is not None
            target.parent.mkdir(parents=True, exist_ok=True)
            appending = self._started or (self.mode == "resume" and not self._fresh)
            self._out = target.open("a" if appending else "w", encoding="utf-8")
            self._started = True
            if not appending:
                # Version 2 records thinking, reasoning items, cache and reasoning
                # tokens and model switches. Version 1 files have no header, lack
                # those fields, and replay unchanged (as context management "none").
                header: dict[str, Any] = {
                    "kind": "header",
                    "version": CASSETTE_VERSION,
                    "context": self.context,
                    "hooks": self.hooks,
                    "toolset": self.toolset,
                    "user_simulator": self.user_simulator,
                }
                if self.mode == "fork":
                    header["forked_from"] = {"session": self.path.name, "turns": self.branch_at}
                self._out.write(json.dumps(header) + "\n")
                self._out.flush()
        return self._out

    def record(self, entry: dict[str, Any], *, replayed: bool = False) -> None:
        """Appends an entry to the log. A resumed run does not write what it
        served from its own log; a fork writes everything, prefix included."""
        if self.mode == "replay" or (replayed and self.mode == "resume"):
            return
        entry = {"id": self._next_id, "parent": self._last_id, **entry}
        self._last_id = self._next_id
        self._next_id += 1
        out = self._writer()
        out.write(json.dumps(entry) + "\n")
        out.flush()  # a trial killed now still leaves this entry

    def save(self) -> None:
        if self._out is None and self.mode in ("record", "fork") and not self._started:
            self._writer()  # an empty log still has its header
        if self._out is not None:
            self._out.close()
            self._out = None

    # --- reading ---

    def next_turn(self) -> ModelTurn | None:
        """The next recorded turn, or None once the run has gone live."""
        if self.live:
            return None
        if self.branch_at and self.turns_served >= self.branch_at:
            self.live = True
            return None
        if not self._turns:
            if self.mode == "resume":
                self.live = True  # the log ends here: carry on
                return None
            raise CassetteError(
                f"cassette {self.path.name} has only {self.turns_served} model turns; "
                "set branch_at_step to continue live"
            )
        self.turns_served += 1
        entry = self._turns.popleft()
        if "error" in entry:
            # The provider refused this call when it was recorded (a raised limit that was
            # too high, say); the run handled it, so the replay raises it again.
            raise ModelError(entry["error"])
        return _turn_from_dict(entry)

    def tool_result(self, name: str, args: dict[str, Any]) -> ToolResult | None:
        if self.live:
            return None
        queue = self._tools.get((name, _args_key(args)))
        if not queue:
            if self.mode == "resume":
                return None  # the trial died before this call finished: run it
            raise CassetteError(
                f"cassette {self.path.name} has no result for {name}({_args_key(args)})"
            )
        data = queue.popleft()
        return ToolResult(
            data["content"],
            is_error=data.get("is_error", False),
            metadata=data.get("metadata") or {},
        )


def session_tree(directory: Path) -> dict[str, dict[str, Any]]:
    """The logs in a directory and how they relate: for each, its parent log
    (None for a root), the turns of the parent it replays, and its own length."""
    tree: dict[str, dict[str, Any]] = {}
    for path in sorted(directory.glob("*.jsonl")):
        parent, turns, models = None, 0, 0
        for line in path.read_text(encoding="utf-8").splitlines():
            try:
                entry = json.loads(line)
            except json.JSONDecodeError:
                break
            if entry.get("kind") == "header" and entry.get("forked_from"):
                parent = entry["forked_from"]["session"]
                turns = entry["forked_from"]["turns"]
            elif entry.get("kind") == "model":
                models += 1
        tree[path.name] = {"parent": parent, "forked_at_turn": turns, "turns": models}
    return tree


class CassetteModel:
    """A model that records through, or replays and then (after a branch) goes live."""

    def __init__(self, cassette: Cassette, live: ChatModel | None) -> None:
        self.cassette = cassette
        self.inner = live
        self.name = live.name if live is not None else "replay"

    async def complete(
        self,
        messages: list[Message],
        tools: list[ToolSpec],
        *,
        system: str = "",
        max_tokens: int = 0,
    ) -> ModelTurn:
        try:
            turn = self.cassette.next_turn()
        except ModelError as exc:
            # A refusal on the tape: a fork copies it into its own log, a resume does not.
            self.cassette.record({"kind": "model", "turn": {"error": str(exc)}}, replayed=True)
            raise
        replayed = turn is not None
        if turn is None:
            if self.inner is None:
                raise CassetteError(
                    "replay reached the branch point but no live model is configured"
                )
            try:
                turn = await self.inner.complete(
                    messages, tools, system=system, max_tokens=max_tokens
                )
            except ModelError as exc:
                self.cassette.record({"kind": "model", "turn": {"error": str(exc)}})
                raise
        self.cassette.record({"kind": "model", "turn": _turn_to_dict(turn)}, replayed=replayed)
        return turn

    async def aclose(self) -> None:
        if self.inner is not None:
            await self.inner.aclose()


class CassetteUserSimulator(UserSimulator):
    """Replays user decisions; creates the judge only when execution goes live."""

    def __init__(self, cassette: Cassette, factory: Callable[[], UserSimulator]) -> None:
        self.cassette = cassette
        self.factory = factory
        self.inner: UserSimulator | None = None
        self.turns = 0

    async def reply(self, conversation: list[dict[str, Any]]) -> UserTurn:
        tape = self.cassette
        key = json.dumps(conversation, sort_keys=True, separators=(",", ":"))
        queue = tape._users.get(key)
        replayed = not tape.live and bool(queue)
        if replayed:
            assert queue is not None
            data = queue.popleft()
            result = UserTurn(data["message"], data["done"], Usage.from_dict(data["usage"]))
        else:
            if not tape.live and (tape.mode != "resume" or tape._turns):
                raise CassetteError("cassette has no user turn for this conversation")
            if self.inner is None:
                self.inner = self.factory()
            self.inner.turns = self.turns
            result = await self.inner.reply(conversation)
        self.turns += 1
        tape.record(
            {
                "kind": "user",
                "conversation": key,
                "result": {
                    "message": result.message,
                    "done": result.done,
                    "usage": result.usage.to_dict(),
                },
            },
            replayed=replayed,
        )
        return result


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
        # tool_search's flag passes through the recording.
        self.deferred = getattr(inner, "deferred", False)

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        result = self.cassette.tool_result(self.spec.name, args)
        replayed = result is not None
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
            },
            replayed=replayed,
        )
        return result
