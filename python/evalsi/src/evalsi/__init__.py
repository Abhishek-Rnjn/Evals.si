"""Evals.si: evaluate AI agents, RAG pipelines, LLMs and models, from CI to production to training.

Quick start::

    import evalsi

    result = evalsi.evaluate("qa.jsonl", ["exact-match", "llm-judge"],
                             judge=evalsi.JudgeConfig(provider="anthropic",
                                                      model="claude-opus-5-5"))
    print(result.table())
"""

from evalsi._version import __version__
from evalsi.evaluator import (
    EvalContext,
    EvaluatorConfigError,
    MetricSpec,
    Requirements,
    Scope,
    ScoreType,
    SkipRecord,
    evaluator,
)
from evalsi.judges import JudgeConfig
from evalsi.logging import Logger, configure_logging, flush, log
from evalsi.registry import Pack, Registry
from evalsi.results import EvalResult, MetricSummary
from evalsi.runner import aevaluate, evaluate
from evalsi.types import (
    Content,
    EvaluationResult,
    Message,
    Outcome,
    Record,
    Score,
    Step,
    TaskCheck,
    ToolCall,
    ToolUse,
    Trajectory,
    Usage,
)

__all__ = [
    "Content",
    "EvalContext",
    "EvalResult",
    "EvaluationResult",
    "EvaluatorConfigError",
    "JudgeConfig",
    "Logger",
    "Message",
    "MetricSpec",
    "MetricSummary",
    "Outcome",
    "Pack",
    "Record",
    "Registry",
    "Requirements",
    "Scope",
    "Score",
    "ScoreType",
    "SkipRecord",
    "Step",
    "TaskCheck",
    "ToolCall",
    "ToolUse",
    "Trajectory",
    "Usage",
    "__version__",
    "aevaluate",
    "configure_logging",
    "evaluate",
    "evaluator",
    "flush",
    "log",
]
