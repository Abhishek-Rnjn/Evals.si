"""A LlamaIndex function-calling agent, traced by OpenInference.

python record_llamaindex.py openinference
"""

from __future__ import annotations

import asyncio
import os
import sys

from otlp_capture import MOCK_URL, MODEL, QUESTION, Capture, search_docs, tracer_provider


def main() -> None:
    mode = sys.argv[1]
    if mode != "openinference":
        sys.exit(f"unknown mode {mode}")
    capture = Capture()
    os.environ.update(OPENAI_API_KEY="unused")
    provider = tracer_provider(capture, "evalsi-fixture-llamaindex")
    from openinference.instrumentation.llama_index import LlamaIndexInstrumentor

    LlamaIndexInstrumentor().instrument(tracer_provider=provider)

    from llama_index.core.agent.workflow import FunctionAgent
    from llama_index.core.tools import FunctionTool
    from llama_index.llms.openai_like import OpenAILike

    llm = OpenAILike(
        model=MODEL,
        api_base=MOCK_URL,
        api_key="unused",
        is_chat_model=True,
        is_function_calling_model=True,
    )
    agent = FunctionAgent(
        tools=[FunctionTool.from_defaults(search_docs)],
        llm=llm,
        system_prompt="Search the documentation corpus, then write a short report.",
    )
    asyncio.run(_run(agent))
    provider.force_flush()
    capture.wait(spans=3)
    capture.write(f"llamaindex-{mode}")


async def _run(agent: object) -> None:
    await agent.run(QUESTION)  # type: ignore[attr-defined]


if __name__ == "__main__":
    main()
