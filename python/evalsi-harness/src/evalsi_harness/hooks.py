"""Hooks: Python code at the built-in agent's lifecycle points.

Changing the prompt, gating a tool or watching context management used to
mean forking ``loop.py``. A hook is a class (``module:Class``, listed under
``harness.builtin.hooks``) with any of these methods, sync or async. It is
built as ``Class(config)`` with the hook's config dict.

``before_model(ctx, request)``
    Returns None, or a ``ModelRequest`` with the messages and system prompt to
    send for this call only; the conversation itself is not changed.
``before_tool(ctx, call)``
    Returns None to allow the call, ``Deny(reason)`` to refuse it (the agent
    sees the reason as the tool's error), or ``Rewrite(arguments)`` to run it
    with other arguments.
``after_tool(ctx, call, result)``
    Returns None, or a ``ToolResult`` to put in the result's place.
``on_compaction(ctx, step)``
    Called with each context-management step. Observe only.
``on_stop(ctx, final)``
    Called when the agent is about to finish normally. Returns None to let it,
    or ``Continue(message)`` to send the agent one more user message (at most
    ``MAX_CONTINUES`` times a run), for example to ask it to verify.

Every decision a hook makes (anything but None) is a step named
``hook:<name>`` with ``evalsi.hook.*`` attributes, so evaluators and replays
see what changed. An exception in a hook ends the run with the error, rather
than being ignored: a gate that silently stopped gating would be worse. A
recording replays only with the same hooks listed, in the same order.
"""

from __future__ import annotations

import importlib
import inspect
import json
from dataclasses import dataclass, field
from typing import Any

from evalsi.types import Content, Step
from evalsi_harness.events import span_id
from evalsi_harness.models import Message, ToolCallRequest
from evalsi_harness.task import TaskError
from evalsi_harness.tools import ToolResult
from evalsi_harness.trust import Trust

MAX_CONTINUES = 3


@dataclass
class HookContext:
    """What a hook can see of the run."""

    task_id: str = ""
    trial: int = 0
    step: int = 0
    tool_calls: int = 0
    # The hook's own config, from the spec.
    config: dict[str, Any] = field(default_factory=dict)
    # Free for the hook to keep state in between calls.
    memory: dict[str, Any] = field(default_factory=dict)


@dataclass
class ModelRequest:
    messages: list[Message]
    system: str


@dataclass
class Deny:
    reason: str


@dataclass
class Rewrite:
    arguments: dict[str, Any]


@dataclass
class Continue:
    message: str


EVENTS = ("before_model", "before_tool", "after_tool", "on_compaction", "on_stop")


class Hook:
    def __init__(self, name: str, obj: Any, config: dict[str, Any]) -> None:
        self.name = name
        self.obj = obj
        self.ctx = HookContext(config=config)

    def has(self, event: str) -> bool:
        return callable(getattr(self.obj, event, None))

    async def call(self, event: str, *args: Any) -> Any:
        value = getattr(self.obj, event)(self.ctx, *args)
        if inspect.isawaitable(value):
            value = await value
        return value


class HookError(RuntimeError):
    """A hook raised: the run ends with this, as an error."""


def _step(hook: str, event: str, parent: str, **attributes: Any) -> Step:
    decision = {k: v for k, v in attributes.items() if v not in (None, "")}
    return Step(
        type="agent",
        name=f"hook:{hook}",
        output=Content(text=json.dumps({"event": event, **decision}, sort_keys=True, default=str)),
        span_id=span_id(),
        parent_span_id=parent,
        attributes={
            "evalsi.hook.event": event,
            **{f"evalsi.hook.{k}": v for k, v in decision.items()},
        },
    )


class Hooks:
    """The hooks of a run, called in the order the spec lists them."""

    def __init__(self, hooks: list[Hook] | None = None) -> None:
        self.hooks = hooks or []
        self.steps: list[Step] = []  # decisions waiting to be emitted

    def __bool__(self) -> bool:
        return bool(self.hooks)

    def refs(self) -> list[str]:
        return [h.name for h in self.hooks]

    def bind(self, task_id: str, trial: int) -> None:
        for h in self.hooks:
            h.ctx.task_id, h.ctx.trial = task_id, trial

    def drain(self) -> list[Step]:
        steps, self.steps = self.steps, []
        return steps

    async def _run(self, hook: Hook, event: str, *args: Any) -> Any:
        try:
            return await hook.call(event, *args)
        except Exception as exc:
            raise HookError(
                f"hook {hook.name} failed in {event}: {type(exc).__name__}: {exc}"
            ) from exc

    def _note(self, hook: Hook, event: str, parent: str, **attributes: Any) -> None:
        self.steps.append(_step(hook.name, event, parent, **attributes))

    def _each(self, event: str) -> list[Hook]:
        return [h for h in self.hooks if h.has(event)]

    async def before_model(self, request: ModelRequest, *, step: int, parent: str) -> ModelRequest:
        for hook in self._each("before_model"):
            hook.ctx.step = step
            out = await self._run(hook, "before_model", request)
            if out is None:
                continue
            if not isinstance(out, ModelRequest):
                raise HookError(
                    f"hook {hook.name}: before_model must return a ModelRequest or None"
                )
            self._note(
                hook,
                "before_model",
                parent,
                decision="rewrite",
                messages_before=len(request.messages),
                messages_after=len(out.messages),
                system_changed=out.system != request.system,
            )
            request = out
        return request

    async def before_tool(
        self, call: ToolCallRequest, *, step: int, parent: str
    ) -> tuple[ToolCallRequest, Deny | None]:
        for hook in self._each("before_tool"):
            hook.ctx.step = step
            out = await self._run(hook, "before_tool", call)
            if out is None:
                continue
            if isinstance(out, Deny):
                self._note(
                    hook, "before_tool", parent, decision="deny", tool=call.name, reason=out.reason
                )
                return call, out
            if isinstance(out, Rewrite):
                self._note(
                    hook,
                    "before_tool",
                    parent,
                    decision="rewrite",
                    tool=call.name,
                    original_arguments=call.arguments,
                )
                call = ToolCallRequest(call.id, call.name, json.dumps(out.arguments))
                continue
            raise HookError(f"hook {hook.name}: before_tool must return Deny, Rewrite or None")
        return call, None

    async def after_tool(
        self, call: ToolCallRequest, result: ToolResult, *, step: int, parent: str
    ) -> ToolResult:
        for hook in self._each("after_tool"):
            hook.ctx.step = step
            out = await self._run(hook, "after_tool", call, result)
            if out is None:
                continue
            if not isinstance(out, ToolResult):
                raise HookError(f"hook {hook.name}: after_tool must return a ToolResult or None")
            self._note(hook, "after_tool", parent, decision="replace", tool=call.name)
            result = out
        return result

    async def on_compaction(self, step: Step, *, parent: str) -> None:
        for hook in self._each("on_compaction"):
            await self._run(hook, "on_compaction", step)

    async def on_stop(self, final: Any, *, step: int, parent: str) -> Continue | None:
        for hook in self._each("on_stop"):
            hook.ctx.step = step
            out = await self._run(hook, "on_stop", final)
            if out is None:
                continue
            if not isinstance(out, Continue) or not out.message.strip():
                raise HookError(f"hook {hook.name}: on_stop must return Continue(message) or None")
            self._note(hook, "on_stop", parent, decision="continue", message=out.message[:500])
            return out
        return None


def load_hooks(refs: list[tuple[str, dict[str, Any]]], trust: Trust) -> Hooks:
    """Hooks from ``(module:Class, config)`` pairs. On a server only the
    classes the config lists under ``agents.trusted_python`` load."""
    hooks: list[Hook] = []
    for ref, config in refs:
        trust.check_python(ref, "hook")
        module, _, name = ref.partition(":")
        if not module or not name:
            raise TaskError(f"hook {ref!r} must be module:Class")
        try:
            cls = getattr(importlib.import_module(module), name)
        except (ImportError, AttributeError) as exc:
            raise TaskError(f"cannot load hook {ref}: {exc}") from exc
        try:
            obj = cls(config) if _takes_config(cls) else cls()
        except Exception as exc:
            raise TaskError(f"hook {ref} failed to start: {type(exc).__name__}: {exc}") from exc
        if not any(callable(getattr(obj, e, None)) for e in EVENTS):
            raise TaskError(f"hook {ref} has none of {', '.join(EVENTS)}")
        hooks.append(Hook(ref, obj, config))
    return Hooks(hooks)


def _takes_config(cls: Any) -> bool:
    try:
        params = inspect.signature(cls).parameters
    except (TypeError, ValueError):
        return True
    return any(
        p.kind in (p.POSITIONAL_ONLY, p.POSITIONAL_OR_KEYWORD, p.VAR_POSITIONAL)
        for p in params.values()
    )
