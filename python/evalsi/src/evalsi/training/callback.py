"""Evaluate checkpoints from inside a Hugging Face ``Trainer`` (and TRL's trainers).

::

    from evalsi.training import CheckpointEvaluator, LoRA
    from evalsi.training.callback import evalsi_callback

    evaluator = CheckpointEvaluator("evals/run.yaml", training_run="grpo-7b",
                                    serving=LoRA("http://vllm:8000/v1"),
                                    regression=["exact-match:0.02"])
    trainer = GRPOTrainer(..., callbacks=[evalsi_callback(evaluator, stop_on_regression=True)])

On every save the new checkpoint is evaluated in a background thread (one
at a time, in order), so training does not wait. With ``wait=True`` the
trainer waits for each evaluation. With ``stop_on_regression`` the trainer
stops at the next step after a checkpoint regresses against the base model.
Pending evaluations finish before ``train()`` returns.
"""

from __future__ import annotations

import logging
import queue
import threading
from pathlib import Path
from typing import Any

from evalsi.training.loop import CheckpointEvaluator, StepResult

logger = logging.getLogger(__name__)


class CheckpointRunner:
    """Evaluates checkpoints one at a time, in order, on a background thread.
    Independent of transformers, so other trainers can drive it."""

    def __init__(self, evaluator: CheckpointEvaluator, *, wait: bool = False) -> None:
        self.evaluator = evaluator
        self.wait = wait
        self.results: list[StepResult] = []
        self._queue: queue.Queue[tuple[str, int] | None] = queue.Queue()
        self._thread: threading.Thread | None = None
        self._lock = threading.Lock()

    def submit(self, checkpoint: str, step: int) -> None:
        if self.wait:
            self._evaluate(checkpoint, step)
            return
        if self._thread is None:
            self._thread = threading.Thread(
                target=self._work, name="evalsi-checkpoints", daemon=True
            )
            self._thread.start()
        self._queue.put((checkpoint, step))

    def _work(self) -> None:
        while (item := self._queue.get()) is not None:
            self._evaluate(*item)

    def _evaluate(self, checkpoint: str, step: int) -> None:
        result = self.evaluator.evaluate(checkpoint, step)
        if result.error:
            logger.warning("evaluating step %s failed: %s", step, result.error)
        with self._lock:
            self.results.append(result)

    @property
    def regressed(self) -> bool:
        with self._lock:
            return any(r.regressed for r in self.results)

    def close(self) -> None:
        """Wait for pending evaluations."""
        if self._thread is not None:
            self._queue.put(None)
            self._thread.join()
            self._thread = None


def evalsi_callback(
    evaluator: CheckpointEvaluator,
    *,
    wait: bool = False,
    stop_on_regression: bool = False,
) -> Any:
    """A ``transformers.TrainerCallback`` that evaluates every saved checkpoint."""
    from transformers import TrainerCallback

    class EvalsiCallback(TrainerCallback):  # type: ignore[misc]
        def __init__(self) -> None:
            self.runner = CheckpointRunner(evaluator, wait=wait)

        def on_save(self, args: Any, state: Any, control: Any, **kwargs: Any) -> Any:
            path = Path(args.output_dir) / f"checkpoint-{state.global_step}"
            if state.is_world_process_zero:
                self.runner.submit(str(path), int(state.global_step))
            return control

        def on_step_end(self, args: Any, state: Any, control: Any, **kwargs: Any) -> Any:
            if stop_on_regression and self.runner.regressed:
                logger.warning("a checkpoint regressed against the base model; stopping")
                control.should_training_stop = True
            return control

        def on_train_end(self, args: Any, state: Any, control: Any, **kwargs: Any) -> Any:
            self.runner.close()
            return control

    return EvalsiCallback()
