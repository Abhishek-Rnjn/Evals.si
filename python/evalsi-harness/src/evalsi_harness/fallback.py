"""Switching models within a run.

A call that keeps failing moves to the next model, and the run stays on it.
The conversation moves along. Reasoning blocks are the one thing that cannot:
a provider only accepts its own, so they are dropped from the messages the new
model sees (the rule is stated here, and the switch is recorded on the step).
"""

from __future__ import annotations

from evalsi_harness.models import ChatModel, Message, ModelError, ModelTurn, ToolSpec


def only_own_thinking(messages: list[Message], model: str) -> list[Message]:
    """Messages without the reasoning blocks another model produced."""
    out: list[Message] = []
    for message in messages:
        by = message.get("thinking_by")
        if by and by != model:
            out.append(
                {k: v for k, v in message.items() if k not in ("thinking_blocks", "thinking_by")}
            )
        else:
            out.append(message)
    return out


class FallbackModel:
    def __init__(self, models: list[ChatModel], *, after: int = 2) -> None:
        if not models:
            raise ValueError("a fallback chain needs at least one model")
        self.models = models
        self.after = max(1, after)
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
            turn = await self.models[self.current].complete(
                only_own_thinking(messages, self.models[self.current].name),
                tools,
                system=system,
                max_tokens=max_tokens,
            )
            turn.model = self.models[self.current].name
            return turn
        last: ModelError | None = None
        while self.current < len(self.models):
            model = self.models[self.current]
            sent = only_own_thinking(messages, model.name)
            for _ in range(self.after):
                try:
                    turn = await model.complete(sent, tools, system=system, max_tokens=max_tokens)
                except ModelError as exc:
                    last = exc
                    continue
                turn.model = model.name
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
