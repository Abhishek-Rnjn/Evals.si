"""The demo agent: a Deep Agents deep agent that researches questions over a small corpus.

It stands in for a studio workflow. The model comes from the environment, so
the same code runs on Anthropic, any OpenAI-compatible endpoint, or the mock
model CI uses:

    DEMO_MODEL=anthropic:claude-sonnet-5-5   ANTHROPIC_API_KEY=...
    DEMO_MODEL=openai:my-model               OPENAI_API_KEY=... OPENAI_BASE_URL=http://host/v1

Set MLFLOW_TRACKING_URI to log every run to MLflow with LangChain autolog.
"""

from __future__ import annotations

import os
import re
from pathlib import Path
from typing import Any

CORPUS = Path(os.environ.get("DEMO_CORPUS", Path(__file__).with_name("corpus")))

SYSTEM_PROMPT = """You are a research assistant. Answer the question with a short report.
Search the corpus with search_docs before you answer, use only what it returns, and
cite each claim as [n] with a numbered list of source ids under a "Sources" heading."""


def search_docs(query: str) -> str:
    """Search the document corpus. Returns the best matching documents with their ids."""
    words = {w for w in re.findall(r"[a-z0-9]+", query.lower()) if len(w) > 2}
    scored = []
    for path in sorted(CORPUS.glob("*.md")):
        text = path.read_text(encoding="utf-8")
        hits = sum(text.lower().count(w) for w in words)
        scored.append((hits, path.stem, text))
    scored.sort(key=lambda s: (-s[0], s[1]))
    best = [s for s in scored if s[0] > 0][:3]
    if not best:
        return "No documents matched."
    return "\n\n".join(f"[{doc_id}]\n{text.strip()}" for _, doc_id, text in best)


def model() -> Any:
    """A chat model from DEMO_MODEL ("provider:name"), with Responses off for OpenAI-compatible servers."""
    from langchain.chat_models import init_chat_model

    spec = os.environ.get("DEMO_MODEL", "openai:mock")
    provider = spec.split(":", 1)[0]
    kwargs: dict[str, Any] = {"temperature": 0}
    if provider == "openai":
        # Most OpenAI-compatible servers speak chat completions, not the Responses API.
        kwargs["use_responses_api"] = os.environ.get("DEMO_OPENAI_RESPONSES", "false") == "true"
    return init_chat_model(spec, **kwargs)


def build_agent() -> Any:
    from deepagents import create_deep_agent

    return create_deep_agent(model=model(), tools=[search_docs], system_prompt=SYSTEM_PROMPT)


def enable_tracing() -> bool:
    """MLflow autolog for LangChain and LangGraph, when MLFLOW_TRACKING_URI is set."""
    if not os.environ.get("MLFLOW_TRACKING_URI"):
        return False
    import mlflow

    mlflow.set_experiment(os.environ.get("MLFLOW_EXPERIMENT_NAME", "evalsi-demo-deepagents"))
    mlflow.langchain.autolog()
    return True
