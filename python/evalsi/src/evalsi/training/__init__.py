"""The checkpoint evaluation loop: evaluate checkpoints as a model trains.

- :mod:`evalsi.training.serving`: serve a checkpoint (vLLM, LoRA hot-load, an endpoint).
- :mod:`evalsi.training.checkpoints`: find checkpoints a trainer wrote.
- :mod:`evalsi.training.loop`: run a spec per checkpoint, learning curves, regression gates.
- :mod:`evalsi.training.callback`: a Hugging Face ``TrainerCallback`` that does it on save.
"""

from evalsi.training.checkpoints import Checkpoint, find
from evalsi.training.loop import (
    BASE,
    CheckpointEvaluator,
    Comparison,
    RegressionGate,
    StepResult,
    compare,
    format_curve,
)
from evalsi.training.serving import VLLM, Endpoint, LoRA, Served, ServingError

__all__ = [
    "BASE",
    "VLLM",
    "Checkpoint",
    "CheckpointEvaluator",
    "Comparison",
    "Endpoint",
    "LoRA",
    "RegressionGate",
    "Served",
    "ServingError",
    "StepResult",
    "compare",
    "find",
    "format_curve",
]
