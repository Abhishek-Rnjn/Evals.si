"""Context management for long runs: what the built-in agent does when its
conversation approaches the model's window.

A strategy is called before every model call with the conversation, and
returns the conversation to send plus the steps that describe what it did, so
every clear or compaction is on the trajectory and evaluators can score what
was lost. The signature is the one a before-model-call hook has.

``clear-tool-results`` replaces old tool results with a stub (the message and
its tool-call id stay, so every provider's pairing rules hold). ``compact``
clears first, and if that is not enough summarizes the earlier turns with the
agent's own model. Both work in batches: they start at ``trigger`` of the
window and go down to ``target``, so the provider's prompt cache is rewritten
once per batch and not on every turn.

The prompt size is counted from what the model last reported, scaled to the
current conversation by characters, which is far better than a characters
estimate alone, especially for code.
"""

from __future__ import annotations

import json
import time
from collections.abc import Callable
from dataclasses import dataclass, field
from typing import Any, Protocol

from evalsi.types import Content, Step, Usage
from evalsi_harness.events import span_id
from evalsi_harness.models import ChatModel, ModelError, ModelTurn

Message = dict[str, Any]

STRATEGIES = ("none", "clear-tool-results", "compact", "fresh-rounds", "evidence-state")
ROUND_DONE = "TASK_COMPLETE"
PROGRESS_NOTE = "progress.md"
SUMMARY_PREFIX = (
    "[Summary of the earlier work, written by you before the conversation was shortened]\n"
)
STUB = "[tool result cleared to save context: {chars} characters from {tool}]"
# Characters per token before the model has reported any usage.
DEFAULT_TOKENS_PER_CHAR = 0.3

SUMMARY_SYSTEM = (
    "You write the notes an autonomous agent will rely on after its conversation is shortened. "
    "The transcript you are given is the agent's earlier work. Write a faithful, compact summary: "
    "the task and constraints, what has been done and found (exact file paths, commands, errors, "
    "values), what failed and why, decisions taken, and what remains. Do not invent anything and "
    "do not continue the work. Plain text, no preamble."
)


@dataclass(frozen=True)
class ContextConfig:
    strategy: str = "none"
    window_tokens: int = 0
    trigger: float = 0.8
    target: float = 0.5
    keep_recent: int = 4
    keep_turns: int = 6
    summary_max_tokens: int = 4096
    spill_outputs: bool = False
    # fresh-rounds: the most rounds, and the model calls one round may make.
    rounds: int = 5
    round_steps: int = 30

    def __post_init__(self) -> None:
        if self.strategy not in STRATEGIES:
            raise ValueError(f"context strategy must be one of {STRATEGIES}, not {self.strategy!r}")
        windowed = self.strategy in ("clear-tool-results", "compact", "evidence-state")
        if windowed and self.window_tokens < 4000:
            raise ValueError("a context strategy needs window_tokens of at least 4000")
        if self.strategy == "fresh-rounds" and (self.rounds < 1 or self.round_steps < 1):
            raise ValueError("fresh-rounds needs rounds and round_steps of at least 1")
        if not 0 < self.target < self.trigger <= 1:
            raise ValueError("context needs 0 < target < trigger <= 1")
        if min(self.keep_recent, self.keep_turns, self.summary_max_tokens) < 0:
            raise ValueError("keep_recent, keep_turns and summary_max_tokens cannot be negative")

    def describe(self) -> dict[str, Any]:
        """What a recording is only valid for: the strategy and its numbers."""
        if self.strategy == "none":
            return {"strategy": "none"}
        if self.strategy == "fresh-rounds":
            return {
                "strategy": "fresh-rounds",
                "rounds": self.rounds,
                "round_steps": self.round_steps,
            }
        return {
            "strategy": self.strategy,
            "window_tokens": self.window_tokens,
            "trigger": self.trigger,
            "target": self.target,
            "keep_recent": self.keep_recent,
            "keep_turns": self.keep_turns,
            "summary_max_tokens": self.summary_max_tokens,
        }


def context_config(proto: Any) -> ContextConfig:
    """From the spec's ``harness.builtin.context`` (zero values take defaults)."""
    return ContextConfig(
        strategy=proto.strategy or "none",
        window_tokens=proto.window_tokens,
        trigger=proto.trigger or 0.8,
        target=proto.target or 0.5,
        keep_recent=proto.keep_recent or 4,
        keep_turns=proto.keep_turns or 6,
        summary_max_tokens=proto.summary_max_tokens or 4096,
        spill_outputs=proto.spill_outputs,
        rounds=proto.rounds or 5,
        round_steps=proto.round_steps or 30,
    )


def message_chars(message: Message) -> int:
    total = len(str(message.get("content") or ""))
    for call in message.get("tool_calls") or []:
        fn = call.get("function") or {}
        total += len(fn.get("name") or "") + len(fn.get("arguments") or "")
    return total


@dataclass
class ContextStats:
    """What context management did, for the run's record."""

    clears: int = 0
    compactions: int = 0
    results_cleared: int = 0
    chars_removed: int = 0
    tokens_removed: int = 0
    turns_summarized: int = 0
    summary_failures: int = 0
    rounds: int = 0

    def to_dict(self) -> dict[str, int]:
        return dict(vars(self))


@dataclass
class ContextView:
    """What a strategy may use: the model, billing, and the token count."""

    model: ChatModel
    system: str
    tools_chars: int
    bill: Callable[[ModelTurn], None]
    last_input_tokens: int = 0
    last_chars: int = 0

    def chars(self, messages: list[Message]) -> int:
        return len(self.system) + self.tools_chars + sum(message_chars(m) for m in messages)

    def observe(self, messages: list[Message], input_tokens: int | None) -> None:
        """Remember what the model reported for the messages it was sent."""
        if input_tokens:
            self.last_input_tokens = input_tokens
            self.last_chars = self.chars(messages)

    def ratio(self) -> float:
        """Tokens per character, from the model's last report."""
        if self.last_input_tokens and self.last_chars:
            return self.last_input_tokens / self.last_chars
        return DEFAULT_TOKENS_PER_CHAR

    def tokens(self, messages: list[Message]) -> int:
        """The prompt size of ``messages``: the model's last count, scaled by
        characters to the conversation as it is now."""
        return round(self.ratio() * self.chars(messages))


@dataclass
class ContextResult:
    messages: list[Message]
    steps: list[Step] = field(default_factory=list)
    # Messages sent with this call only, after the conversation (a state the
    # strategy keeps outside it).
    transient: list[Message] = field(default_factory=list)


class ContextStrategy(Protocol):
    name: str
    stats: ContextStats

    async def prepare(
        self, messages: list[Message], view: ContextView, *, parent: str = ""
    ) -> ContextResult: ...


class NoStrategy:
    name = "none"

    def __init__(self) -> None:
        self.stats = ContextStats()

    async def prepare(
        self, messages: list[Message], view: ContextView, *, parent: str = ""
    ) -> ContextResult:
        return ContextResult(messages)


def _tool_names(messages: list[Message]) -> dict[str, str]:
    names: dict[str, str] = {}
    for m in messages:
        for call in m.get("tool_calls") or []:
            names[call["id"]] = (call.get("function") or {}).get("name", "tool")
    return names


def clear_tool_results(
    messages: list[Message], goal: Callable[[list[Message]], bool], keep_recent: int
) -> tuple[list[Message], int, int]:
    """Stubs the oldest tool results, never the newest ``keep_recent``, until
    ``goal`` says the conversation is small enough. Returns the messages, the
    number cleared and the characters removed. Messages are copied, not edited."""
    out = list(messages)
    candidates = [i for i, m in enumerate(out) if m.get("role") == "tool" and not m.get("cleared")]
    protected = set(candidates[len(candidates) - keep_recent :]) if keep_recent else set()
    names = _tool_names(out)
    cleared = removed = 0
    for i in candidates:
        if goal(out):
            break
        if i in protected:
            continue
        message = out[i]
        content = str(message.get("content") or "")
        stub = STUB.format(
            chars=len(content), tool=names.get(message.get("tool_call_id", ""), "tool")
        )
        if len(stub) >= len(content):
            continue  # nothing to gain
        out[i] = {**message, "content": stub, "cleared": True}
        cleared += 1
        removed += len(content) - len(stub)
    return out, cleared, removed


def _context_step(
    name: str, parent: str, started: float, attributes: dict[str, Any], usage: Usage | None = None
) -> Step:
    return Step(
        type="agent",
        name=f"context:{name}",
        output=Content(text=json.dumps(attributes, sort_keys=True)),
        span_id=span_id(),
        parent_span_id=parent,
        duration_ms=(time.perf_counter() - started) * 1000,
        usage=usage,
        attributes={f"evalsi.context.{k}": v for k, v in attributes.items()},
    )


class ClearToolResults:
    name = "clear-tool-results"

    def __init__(self, config: ContextConfig) -> None:
        self.config = config
        self.stats = ContextStats()

    async def prepare(
        self, messages: list[Message], view: ContextView, *, parent: str = ""
    ) -> ContextResult:
        c = self.config
        before = view.tokens(messages)
        if before < c.trigger * c.window_tokens:
            return ContextResult(messages)
        started = time.perf_counter()
        out, cleared, removed = clear_tool_results(
            messages, lambda m: view.tokens(m) <= c.target * c.window_tokens, c.keep_recent
        )
        if not cleared:
            return ContextResult(messages)
        after = view.tokens(out)
        self.stats.clears += 1
        self.stats.results_cleared += cleared
        self.stats.chars_removed += removed
        self.stats.tokens_removed += max(before - after, 0)
        step = _context_step(
            "clear-tool-results",
            parent,
            started,
            {
                "tokens_before": before,
                "tokens_after": after,
                "results_cleared": cleared,
                "chars_removed": removed,
            },
        )
        return ContextResult(out, [step])


def _transcript(messages: list[Message], item_limit: int) -> str:
    names = _tool_names(messages)
    lines: list[str] = []
    for m in messages:
        role, content = m.get("role"), str(m.get("content") or "")
        if role == "user":
            lines.append(f"USER: {content[:item_limit]}")
        elif role == "assistant":
            if content:
                lines.append(f"ASSISTANT: {content[:item_limit]}")
            for call in m.get("tool_calls") or []:
                fn = call.get("function") or {}
                lines.append(
                    f"ASSISTANT CALLED {fn.get('name')}({(fn.get('arguments') or '')[:item_limit]})"
                )
        elif role == "tool":
            name = names.get(m.get("tool_call_id", ""), "tool")
            suffix = " (ERROR)" if m.get("is_error") else ""
            lines.append(f"RESULT OF {name}{suffix}: {content[:item_limit]}")
    return "\n".join(lines)


def split_for_compaction(messages: list[Message], keep_turns: int) -> int | None:
    """The index where the verbatim tail starts: the start of the last
    ``keep_turns`` assistant turns, whose tool results follow them. None when
    there is nothing between the task message and the tail to summarize."""
    assistants = [i for i, m in enumerate(messages) if m.get("role") == "assistant"]
    if len(assistants) <= keep_turns:
        return None
    start = assistants[len(assistants) - max(keep_turns, 1)]
    return start if start > 1 else None


class Compact:
    """Clears first; when clearing is not enough, summarizes the earlier turns."""

    name = "compact"

    def __init__(self, config: ContextConfig) -> None:
        self.config = config
        self.stats = ContextStats()

    async def prepare(
        self, messages: list[Message], view: ContextView, *, parent: str = ""
    ) -> ContextResult:
        c = self.config
        window = c.window_tokens
        before = view.tokens(messages)
        if before < c.trigger * window:
            return ContextResult(messages)
        started = time.perf_counter()
        out, cleared, removed = clear_tool_results(
            messages, lambda m: view.tokens(m) <= c.target * window, c.keep_recent
        )
        steps: list[Step] = []
        if cleared:
            self.stats.clears += 1
            self.stats.results_cleared += cleared
            self.stats.chars_removed += removed
            steps.append(
                _context_step(
                    "clear-tool-results",
                    parent,
                    started,
                    {
                        "tokens_before": before,
                        "tokens_after": view.tokens(out),
                        "results_cleared": cleared,
                        "chars_removed": removed,
                    },
                )
            )
        if view.tokens(out) > c.trigger * window:
            compacted = await self._summarize(out, view, parent)
            if compacted is not None:
                out, step = compacted
                steps.append(step)
        after = view.tokens(out)
        self.stats.tokens_removed += max(before - after, 0)
        return ContextResult(out, steps) if steps else ContextResult(messages)

    async def _summarize(
        self, messages: list[Message], view: ContextView, parent: str
    ) -> tuple[list[Message], Step] | None:
        c = self.config
        started = time.perf_counter()
        tail_start = split_for_compaction(messages, c.keep_turns)
        if tail_start is None:
            return None
        old = messages[1:tail_start]
        # The transcript has to fit the window with room for the answer.
        budget_chars = int(c.window_tokens * 0.7 / view.ratio())
        limit = 2000
        text = _transcript(old, limit)
        while len(text) > budget_chars and limit > 100:
            limit //= 2
            text = _transcript(old, limit)
        prompt = f"Transcript of the earlier work:\n\n{text}\n\nWrite the summary now."
        try:
            turn = await view.model.complete(
                [{"role": "user", "content": prompt}],
                [],
                system=SUMMARY_SYSTEM,
                max_tokens=c.summary_max_tokens,
            )
        except ModelError:
            self.stats.summary_failures += 1
            return None
        view.bill(turn)
        summary = turn.text.strip()
        if turn.truncated or not summary:
            # A cut-off summary would be worse than none: keep the cleared conversation.
            self.stats.summary_failures += 1
            return None
        turns = sum(1 for m in old if m.get("role") == "assistant")
        out = [
            messages[0],
            {"role": "user", "content": SUMMARY_PREFIX + summary},
            *messages[tail_start:],
        ]
        self.stats.compactions += 1
        self.stats.turns_summarized += turns
        step = _context_step(
            "compact",
            parent,
            started,
            {
                "tokens_before": view.tokens(messages),
                "tokens_after": view.tokens(out),
                "messages_summarized": len(old),
                "turns_summarized": turns,
                "summary_chars": len(summary),
                "summary_model": turn.model or view.model.name,
            },
            usage=turn.usage,
        )
        return out, step


def make_strategy(config: ContextConfig, evidence: Any = None) -> ContextStrategy:
    if config.strategy == "evidence-state":
        from evalsi_harness.evidence import EvidenceStateStrategy

        return EvidenceStateStrategy(config, evidence)
    if config.strategy == "clear-tool-results":
        return ClearToolResults(config)
    if config.strategy == "compact":
        return Compact(config)
    return NoStrategy()
