# Agent frameworks: trace LangGraph, CrewAI, OpenAI Agents, LlamaIndex or Claude Agent SDK agents

Evals.si grades an agent from its traces. Instrument the agent with one of the usual OpenTelemetry instrumentations, send the spans to the server's OTLP endpoint, and every finished trace becomes a record: the question, the answer, and a trajectory of model calls and tool calls. Online policies score it, and the web UI shows it.

The five frameworks below are tested. For each instrumentation listed, a real run of the framework was recorded and checked in. The run asks one research question, makes one `search_docs` tool call and writes a report, against the demo's mock model. A test checks that each recording becomes the same record ([`internal/ingest/frameworks_test.go`](../../internal/ingest/frameworks_test.go)). The recorders and the exact versions they used are in [`tests/frameworks`](../../tests/frameworks/README.md).

| Framework | Instrumentation | Question and answer | Model calls | Tool calls | Tokens |
|---|---|---|---|---|---|
| LangGraph / LangChain | OpenInference | Yes | Yes, with the tool call asked for | Yes, with arguments and result | Yes |
| | LangSmith's OTel export | Yes | Yes | Yes | Yes |
| | MLflow autolog ([Deep Agents](../../examples/demo/README.md)) | Yes | Yes | Yes | Yes |
| CrewAI | MLflow autolog (CrewAI and OpenAI) | Yes | Yes | Only as the model's request: no tool span | Yes |
| | OpenInference (CrewAI and OpenAI) | Yes | Yes | Yes | Yes |
| | OpenLLMetry (CrewAI and OpenAI) | Yes | Yes | Only as the model's request | Yes |
| OpenAI Agents SDK | OpenInference | Yes | Yes | Yes | Yes |
| | OpenLLMetry | Question only: no model output is recorded | Yes, input only | Yes | No |
| LlamaIndex | OpenInference | Yes | Yes | Yes | Not for streamed calls |
| Claude Agent SDK | OpenInference | Yes | One agent span with the whole conversation | Yes | Yes, on the agent span |
| | Claude Code's own traces (beta) | Question only | Yes, without content | Name and result | Yes |

"Only as the model's request" is enough for the tool evaluators: when a trace has no tool spans, they read the tool calls the model asked for, and so do policy selectors (`"search_docs" in tools`). An instrumentation that records no answer gives records without an output. Evaluators that need one skip them rather than score them zero. Use OpenInference for those two frameworks if you need answers graded.

Every snippet below sends to an OTLP/HTTP endpoint. Set it, and the ingest key, the usual way:

```bash
export OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://evals.example.com/v1/traces
export OTEL_EXPORTER_OTLP_TRACES_HEADERS="authorization=Bearer $EVALSI_INGEST_KEY"
```

For the instrumentors (everything but MLflow, LangSmith and Claude Code's own traces), create a tracer provider with that exporter:

```python
from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor

provider = TracerProvider(resource=Resource.create({"service.name": "research-agent"}))
provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
```

## LangGraph and LangChain

OpenInference:

```python
from openinference.instrumentation.langchain import LangChainInstrumentor

LangChainInstrumentor().instrument(tracer_provider=provider)
```

LangSmith's OTel export, without sending anything to LangSmith:

```python
from opentelemetry import trace

trace.set_tracer_provider(provider)
# LANGSMITH_TRACING=true LANGSMITH_OTEL_ENABLED=true LANGSMITH_OTEL_ONLY=true
```

The record's input and output are the graph's first and last messages. LangChain's message formats (OpenAI-style, LangChain's own, its dumped and serialized forms) are all read. MLflow's LangChain autolog with its OTLP exporter works as well: it is what the [Deep Agents demo](../../examples/demo/README.md) uses.

## CrewAI

CrewAI 1.x calls the model through its own provider and runs tools inside that call. Instrument the OpenAI client as well as CrewAI, or the model calls are missing.

MLflow, the agent-studio path:

```python
import mlflow

# MLFLOW_ENABLE_OTLP_EXPORTER=true, plus the OTEL_EXPORTER_OTLP_* variables above
mlflow.crewai.autolog()
mlflow.openai.autolog()
```

OpenInference, or OpenLLMetry (the same two instrumentors from `opentelemetry.instrumentation.crewai` and `opentelemetry.instrumentation.openai`):

```python
from openinference.instrumentation.crewai import CrewAIInstrumentor
from openinference.instrumentation.openai import OpenAIInstrumentor

CrewAIInstrumentor().instrument(tracer_provider=provider)
OpenAIInstrumentor().instrument(tracer_provider=provider)
```

Set `CREWAI_DISABLE_TELEMETRY=true` so CrewAI's own telemetry stays out of your traces. The record's answer is the crew's `raw` output.

## OpenAI Agents SDK

```python
from agents import set_trace_processors
from openinference.instrumentation.openai_agents import OpenAIAgentsInstrumentor

set_trace_processors([])  # stop the export to OpenAI's backend; the instrumentor still sees every span
OpenAIAgentsInstrumentor().instrument(tracer_provider=provider)
```

Do not turn the SDK's tracing off: the instrumentor reads the SDK's own trace events. OpenLLMetry's instrumentor (`opentelemetry.instrumentation.openai_agents`) records the tool calls and the model's input but no model output, so its records have no answer.

## LlamaIndex

```python
from openinference.instrumentation.llama_index import LlamaIndexInstrumentor

LlamaIndexInstrumentor().instrument(tracer_provider=provider)
```

LlamaIndex nests a streamed model call inside another, and marks the step that prepares a tool call as a model call too. Only the innermost call counts as a model call, so a trace has as many as the model answered. Token counts are missing for streamed calls unless the model reports usage in the stream.

## Claude Agent SDK

OpenInference:

```python
from openinference.instrumentation.claude_agent_sdk import ClaudeAgentSDKInstrumentor

ClaudeAgentSDKInstrumentor().instrument(tracer_provider=provider)
```

It records one agent span per query, with the prompt, the answer, the tool calls the model asked for and the tokens, and one span per tool call. So a record has the conversation and the tools, but no separate model calls.

The SDK runs the Claude Code CLI, which has its own traces (a beta). Pass the variables to the CLI through `ClaudeAgentOptions(env=...)`:

```text
CLAUDE_CODE_ENABLE_TELEMETRY=1  CLAUDE_CODE_ENHANCED_TELEMETRY_BETA=1
OTEL_TRACES_EXPORTER=otlp  OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf  OTEL_EXPORTER_OTLP_ENDPOINT=...
OTEL_LOG_USER_PROMPTS=1  OTEL_LOG_TOOL_DETAILS=1
```

These give model calls with tokens, tools with their results, and the prompt. They do not include the model's output, so the records have no answer. If the environment that starts the CLI has a `TRACEPARENT` marked as not sampled, the CLI records nothing.

## Other frameworks

AutoGen, Pydantic AI, Google ADK, Semantic Kernel and custom loops work through the same conventions (OTel GenAI, OpenInference, OpenLLMetry) without a tested profile. Look at a few stored traces in the web UI first. To add a framework profile, record a fixture with a recorder like the ones in `tests/frameworks`, add its expectations to the test, and change [`internal/ingest/profiles.go`](../../internal/ingest/profiles.go) until it passes.
