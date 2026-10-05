"""Evals.si: one entrypoint for evaluating ML models, LLMs and agents.

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
    "ToolCall",
    "ToolUse",
    "Trajectory",
    "Usage",
    "__version__",
    "aevaluate",
    "evaluate",
    "evaluator",
]
