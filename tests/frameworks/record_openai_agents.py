"""An OpenAI Agents SDK agent, traced by OpenInference or OpenLLMetry.

    python record_openai_agents.py openinference|openllmetry

The SDK's own tracing sends traces to OpenAI's backend, not over OTLP; both
instrumentors hook into it, so its processors are replaced rather than
tracing turned off. The mock model speaks chat completions, not the
Responses API, hence OpenAIChatCompletionsModel.
"""

from __future__ import annotations

import os
import sys

from otlp_capture import MOCK_URL, MODEL, QUESTION, Capture, search_docs, tracer_provider


def main() -> None:
    mode = sys.argv[1]
    capture = Capture()
    os.environ.update(OPENAI_API_KEY="unused", OPENAI_BASE_URL=MOCK_URL)
    provider = tracer_provider(capture, "evalsi-fixture-openai-agents")
    from agents import set_trace_processors

    set_trace_processors([])
    if mode == "openinference":
        from openinference.instrumentation.openai_agents import OpenAIAgentsInstrumentor

        OpenAIAgentsInstrumentor().instrument(tracer_provider=provider)
    elif mode == "openllmetry":
        from opentelemetry.instrumentation.openai_agents import OpenAIAgentsInstrumentor

        OpenAIAgentsInstrumentor().instrument(tracer_provider=provider)
    else:
        sys.exit(f"unknown mode {mode}")

    from agents import Agent, OpenAIChatCompletionsModel, Runner, function_tool
    from openai import AsyncOpenAI

    agent = Agent(
        name="Researcher",
        instructions="Search the documentation corpus, then write a short report.",
        tools=[function_tool(search_docs)],
        model=OpenAIChatCompletionsModel(model=MODEL, openai_client=AsyncOpenAI()),
    )
    Runner.run_sync(agent, QUESTION)
    provider.force_flush()
    capture.wait(spans=3)
    capture.write(f"openai-agents-{mode}")


if __name__ == "__main__":
    main()
