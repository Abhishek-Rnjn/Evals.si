"""Context-budget mechanics with transformers 4.57.1 ByT5Tokenizer; responses remain scripted.

Token counts cover serialized messages/system/tools, not a provider's private
chat envelope. This does not measure model summary quality or task transfer.
"""

from __future__ import annotations

import argparse
import asyncio
import json
from pathlib import Path
from typing import Any

from transformers import ByT5Tokenizer

from evalsi.types import Usage
from evalsi_harness.context import SUMMARY_SYSTEM, ContextConfig
from evalsi_harness.loop import AgentLoop, LoopConfig
from evalsi_harness.models import ModelError, ModelTurn, ToolCallRequest
from evalsi_harness.tools import FunctionTool

WINDOW = 64000
STEPS = 120
TEXT = (
    "Database replication preserves committed records. 数据库复制保留已提交的记录。 🔍 2026-10-17\n"
    * 120
)


class CountedModel:
    name = "tokenizer-scripted"

    def __init__(self) -> None:
        self.encoding = ByT5Tokenizer()
        self.calls = 0
        self.peak = 0

    async def complete(
        self, messages: Any, tools: Any, *, system: str = "", max_tokens: int = 0
    ) -> ModelTurn:
        payload = json.dumps(
            {"system": system, "messages": messages, "tools": tools},
            ensure_ascii=False,
            default=vars,
        )
        count = len(self.encoding.encode(payload))
        self.peak = max(count, self.peak)
        if count > WINDOW:
            raise ModelError(f"tokenizer window exceeded: {count} > {WINDOW}")
        usage = Usage(input_tokens=count, output_tokens=20)
        if system == SUMMARY_SYSTEM:
            return ModelTurn(
                text="Read every numbered document. Continue and retain the latest results.",
                stop_reason="end_turn",
                usage=usage,
            )
        self.calls += 1
        if self.calls > STEPS:
            return ModelTurn(text="TASK_COMPLETE", stop_reason="end_turn", usage=usage)
        return ModelTurn(
            text="",
            tool_calls=[
                ToolCallRequest(f"call-{self.calls}", "document", json.dumps({"index": self.calls}))
            ],
            stop_reason="tool_use",
            usage=usage,
        )

    async def aclose(self) -> None:
        pass


async def verify() -> dict[str, Any]:
    reports = []
    for strategy in ["none", "clear-tool-results", "compact", "evidence-state", "fresh-rounds"]:
        model = CountedModel()
        context = ContextConfig(
            strategy=strategy,
            window_tokens=WINDOW,
            keep_recent=1,
            keep_turns=3,
            rounds=32,
            round_steps=4,
        )
        loop = AgentLoop(
            model,
            [
                FunctionTool(
                    "document",
                    lambda index: f"[{index}] {TEXT}",
                    input_schema={
                        "type": "object",
                        "properties": {"index": {"type": "integer"}},
                        "required": ["index"],
                    },
                )
            ],
            LoopConfig(max_steps=200, context=context),
            lambda event: None,
        )
        try:
            final = await loop.run([{"role": "user", "content": "Read every document."}])
        except ModelError:
            assert strategy == "none", (strategy, model.peak)
            reports.append(
                {
                    "strategy": strategy,
                    "control": "window exceeded",
                    "calls": model.calls,
                    "peak": model.peak,
                }
            )
        else:
            assert strategy != "none", "the control did not exceed its window"
            assert final.stop_reason == "completed", final
            assert model.calls == STEPS + 1, model.calls
            assert model.peak <= WINDOW, model.peak
            reports.append(
                {
                    "strategy": strategy,
                    "calls": model.calls,
                    "peak": model.peak,
                    "context": final.extra["context"],
                }
            )
        print(json.dumps(reports[-1]), flush=True)
    return {
        "transformers": "4.57.1",
        "encoding": "ByT5Tokenizer UTF-8 bytes",
        "window": WINDOW,
        "model": "scripted",
        "cases": reports,
    }


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    args.report.write_text(json.dumps(asyncio.run(verify()), indent=2) + "\n")
