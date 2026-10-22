"""Switching models within a run.

A call that keeps failing moves to the next model, and the run stays on it.
The conversation moves along. Reasoning blocks cannot move as they are: a
provider only accepts its own (Anthropic's thinking is signed, the Responses
API's reasoning is encrypted, and neither can be checked by anyone else), and
so do Anthropic's server tool blocks. What happens to them is a stated rule,
``TargetConfig.reasoning_handoff``:

- ``drop`` (the default): the blocks are removed from the messages the new
  model sees.
- ``summarize-as-text``: the readable part, Anthropic's thinking text or a
  Responses reasoning summary, goes into the assistant message as plain text,
  marked as carried-over reasoning; the signed and encrypted blocks (and any
  redacted thinking) are still dropped, since no other provider can verify
  them.

Every call records what it did on its turn (``ModelTurn.handoff``, the step's
``evalsi.reasoning.handoff``): the rule, the messages and blocks converted and
the characters carried as text. A switch is recorded on the step too. A replay
serves the recorded turns, conversions included, so it stays exact.
"""

from __future__ import annotations

from typing import Any

from evalsi_harness.models import (
    PROVIDER_KEYS,
    ChatModel,
    Message,
    ModelError,
    ModelTurn,
    ToolSpec,
    thinking_text,
)

HANDOFF_RULES = ("drop", "summarize-as-text")

CARRIED = (
    "[Reasoning by {model}, carried over as text: its own reasoning blocks cannot be sent "
    "to another provider]\n{text}\n[End of carried-over reasoning]"
)


def hand_off(
    messages: list[Message], model: str, rule: str = "drop"
) -> tuple[list[Message], dict[str, Any]]:
    """The messages ``model`` may be sent: another model's reasoning and server
    tool blocks dropped, or (``summarize-as-text``) their readable text carried
    as plain text. Also what was done, empty when nothing was."""
    if rule not in HANDOFF_RULES:
        raise ValueError(f"reasoning_handoff must be one of {HANDOFF_RULES}, not {rule!r}")
    out: list[Message] = []
    record: dict[str, Any] = {}
    for message in messages:
        by = message.get("thinking_by")
        if not by or by == model:
            out.append(message)
            continue
        blocks = list(message.get("thinking_blocks") or [])
        stripped = {k: v for k, v in message.items() if k not in PROVIDER_KEYS}
        text = thinking_text(blocks) if rule == "summarize-as-text" else ""
        if text:
            content = str(stripped.get("content") or "")
            carried = CARRIED.format(model=by, text=text)
            stripped["content"] = f"{carried}\n\n{content}" if content else carried
        out.append(stripped)
        if not record:
            record = {"rule": rule, "to": model, "from": [], "messages": 0}
            record.update(blocks_dropped=0, server_blocks_dropped=0, text_chars=0)
        if by not in record["from"]:
            record["from"].append(by)
        record["messages"] += 1
        record["blocks_dropped"] += len(blocks)
        record["server_blocks_dropped"] += sum(
            1
            for b in message.get("anthropic_content") or []
            if b.get("type") not in ("text", "tool_use", "thinking", "redacted_thinking")
        )
        record["text_chars"] += len(text)
    return out, record


def only_own_thinking(messages: list[Message], model: str) -> list[Message]:
    """Messages without the reasoning blocks another model produced (the ``drop`` rule)."""
    return hand_off(messages, model)[0]


class FallbackModel:
    def __init__(self, models: list[ChatModel], *, after: int = 2, handoff: str = "drop") -> None:
        if not models:
            raise ValueError("a fallback chain needs at least one model")
        if handoff not in HANDOFF_RULES:
            raise ValueError(f"reasoning_handoff must be one of {HANDOFF_RULES}, not {handoff!r}")
        self.models = models
        self.after = max(1, after)
        self.handoff = handoff
        self.current = 0
        self.name = models[0].name

    async def complete(
        self,
        messages: list[Message],
        tools: list[ToolSpec],
        *,
        system: str = "",
        max_tokens: int = 0,
    ) -> ModelTurn:
        first = self.current
        if max_tokens:
            # A retry with a raised limit that the provider refuses must not
            # count as the model failing: ask the current model once, and let
            # the caller decide.
            model = self.models[self.current]
            sent, record = hand_off(messages, model.name, self.handoff)
            turn = await model.complete(sent, tools, system=system, max_tokens=max_tokens)
            turn.model = model.name
            turn.handoff = record
            return turn
        last: ModelError | None = None
        while self.current < len(self.models):
            model = self.models[self.current]
            sent, record = hand_off(messages, model.name, self.handoff)
            for _ in range(self.after):
                try:
                    turn = await model.complete(sent, tools, system=system, max_tokens=max_tokens)
                except ModelError as exc:
                    last = exc
                    continue
                turn.model = model.name
                turn.handoff = record
                self.name = model.name
                if self.current != first:
                    turn.switched_from = self.models[first].name
                return turn
            self.current += 1
        self.current = len(self.models) - 1
        assert last is not None
        raise ModelError(f"every model in the fallback chain failed; the last error: {last}")

    async def aclose(self) -> None:
        for model in self.models:
            await model.aclose()
