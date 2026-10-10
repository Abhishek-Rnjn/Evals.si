"""CrewAI, traced three ways: MLflow's autolog exported over OTLP (the
agent-studio path), OpenInference, or OpenLLMetry.

    python record_crewai.py mlflow|openinference|openllmetry
"""

from __future__ import annotations

import os
import sys
import tempfile

from otlp_capture import MOCK_URL, MODEL, QUESTION, Capture, search_docs, tracer_provider


def main() -> None:
    mode = sys.argv[1]
    capture = Capture()
    os.environ.update(
        OPENAI_API_KEY="unused",
        OPENAI_BASE_URL=MOCK_URL,
        CREWAI_DISABLE_TELEMETRY="true",
        CREWAI_TRACING_ENABLED="false",
        OTEL_SERVICE_NAME="evalsi-fixture-crewai",
    )
    if mode == "mlflow":
        work = tempfile.mkdtemp()
        os.environ.update(
            MLFLOW_TRACKING_URI=f"file://{work}/mlruns",
            MLFLOW_ALLOW_FILE_STORE="true",  # somewhere to log; the OTLP export is kept
            MLFLOW_ENABLE_OTLP_EXPORTER="true",
            OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=capture.endpoint,
            OTEL_EXPORTER_OTLP_TRACES_PROTOCOL="http/protobuf",
            OTEL_BSP_SCHEDULE_DELAY="200",
        )
        import mlflow

        # CrewAI 1.x calls OpenAI through its own provider, which the CrewAI
        # autolog does not see: the OpenAI autolog records the model calls.
        mlflow.crewai.autolog()
        mlflow.openai.autolog()
    elif mode == "openinference":
        from openinference.instrumentation.crewai import CrewAIInstrumentor
        from openinference.instrumentation.openai import OpenAIInstrumentor

        provider = tracer_provider(capture, "evalsi-fixture-crewai")
        CrewAIInstrumentor().instrument(tracer_provider=provider)
        OpenAIInstrumentor().instrument(tracer_provider=provider)
    elif mode == "openllmetry":
        from opentelemetry.instrumentation.crewai import CrewAIInstrumentor
        from opentelemetry.instrumentation.openai import OpenAIInstrumentor

        provider = tracer_provider(capture, "evalsi-fixture-crewai")
        CrewAIInstrumentor().instrument(tracer_provider=provider)
        OpenAIInstrumentor().instrument(tracer_provider=provider)
    else:
        sys.exit(f"unknown mode {mode}")

    from crewai import LLM, Agent, Crew, Task
    from crewai.tools import tool

    researcher = Agent(
        role="Researcher",
        goal="Answer research questions from the documentation corpus",
        backstory="You search the corpus before you write.",
        tools=[tool("search_docs")(search_docs)],
        llm=LLM(model=f"openai/{MODEL}", base_url=MOCK_URL, api_key="unused"),
        max_iter=3,
        verbose=False,
    )
    task = Task(description=QUESTION, expected_output="A short report", agent=researcher)
    Crew(agents=[researcher], tasks=[task]).kickoff()
    if mode == "mlflow":
        import mlflow

        mlflow.flush_trace_async_logging()
    capture.wait(spans=3)
    capture.write(f"crewai-{mode}")


if __name__ == "__main__":
    main()
