"""A LangGraph ReAct agent, traced by OpenInference or by LangSmith's OTel export.

python record_langgraph.py openinference|langsmith
"""

from __future__ import annotations

import os
import sys

from otlp_capture import MOCK_URL, MODEL, QUESTION, Capture, search_docs, tracer_provider


def main() -> None:
    mode = sys.argv[1]
    capture = Capture()
    os.environ.update(OPENAI_API_KEY="unused", OPENAI_BASE_URL=MOCK_URL)
    provider = tracer_provider(capture, "evalsi-fixture-langgraph")
    if mode == "openinference":
        from openinference.instrumentation.langchain import LangChainInstrumentor

        LangChainInstrumentor().instrument(tracer_provider=provider)
    elif mode == "langsmith":
        from opentelemetry import trace

        # LangSmith exports to the global tracer provider when OTel-only.
        trace.set_tracer_provider(provider)
        os.environ.update(
            LANGSMITH_TRACING="true",
            LANGSMITH_OTEL_ENABLED="true",
            LANGSMITH_OTEL_ONLY="true",
        )
    else:
        sys.exit(f"unknown mode {mode}")

    from langchain_core.tools import tool
    from langchain_openai import ChatOpenAI
    from langgraph.prebuilt import create_react_agent

    agent = create_react_agent(ChatOpenAI(model=MODEL), [tool(search_docs)])
    agent.invoke({"messages": [{"role": "user", "content": QUESTION}]})
    provider.force_flush()
    capture.wait(spans=3)
    capture.write(f"langgraph-{mode}")


if __name__ == "__main__":
    main()
