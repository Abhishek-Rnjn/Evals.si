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
import os
import queue
import tempfile
import threading
from pathlib import Path
from typing import Any, Protocol

from evalsi.training.loop import CheckpointEvaluator, StepResult

logger = logging.getLogger(__name__)


class CheckpointEvaluation(Protocol):
    def evaluate(self, checkpoint: str, step: int) -> StepResult: ...


class CheckpointRunner:
    """Evaluates checkpoints one at a time, in order, on a background thread.
    Independent of transformers, so other trainers can drive it."""

    def __init__(self, evaluator: CheckpointEvaluation, *, wait: bool = False) -> None:
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


class PyTorchCheckpointHook:
    """Atomically save a state dict and evaluate only the published checkpoint."""

    def __init__(self, evaluator: CheckpointEvaluation, *, wait: bool = False) -> None:
        self.runner = CheckpointRunner(evaluator, wait=wait)

    def save(self, model: Any, path: str | Path, step: int, *, optimizer: Any = None) -> None:
        import torch

        path = Path(path)
        path.parent.mkdir(parents=True, exist_ok=True)
        if path.exists():
            raise FileExistsError("use a unique checkpoint path for every step")
        state = {"state_dict": model.state_dict(), "global_step": step}
        if optimizer is not None:
            state["optimizer_state_dict"] = optimizer.state_dict()
        descriptor, temporary = tempfile.mkstemp(dir=path.parent, prefix=".evalsi-")
        try:
            with os.fdopen(descriptor, "wb") as checkpoint_file:
                torch.save(state, checkpoint_file)
                checkpoint_file.flush()
                os.fsync(checkpoint_file.fileno())
            os.replace(temporary, path)
        finally:
            Path(temporary).unlink(missing_ok=True)
        self.runner.submit(str(path), step)

    def close(self) -> None:
        self.runner.close()

    def __enter__(self) -> PyTorchCheckpointHook:
        return self

    def __exit__(self, *_: object) -> None:
        self.close()


def lightning_checkpoint_callback(
    evaluator: CheckpointEvaluation, *, wait: bool = False, **options: Any
) -> Any:
    """A Lightning ModelCheckpoint that evaluates after the actual file save.

    Async evaluations keep every checkpoint: deleting a previous top-k file
    while its evaluation is queued is unsafe. Supply a serving strategy that
    understands PyTorch/Lightning state dicts; these files are not vLLM models.
    """
    from lightning.pytorch.callbacks import ModelCheckpoint

    options.setdefault("save_top_k", -1)
    if not wait and (options["save_top_k"] != -1 or options.get("save_last")):
        raise ValueError("async checkpoint evaluation requires save_top_k=-1 and save_last=False")

    class EvalsiModelCheckpoint(ModelCheckpoint):  # type: ignore[misc]
        def __init__(self) -> None:
            super().__init__(**options)
            self.runner = CheckpointRunner(evaluator, wait=wait)

        def _save_checkpoint(self, trainer: Any, filepath: str) -> None:
            super()._save_checkpoint(trainer, filepath)
            if trainer.is_global_zero:
                self.runner.submit(filepath, int(trainer.global_step))

        def on_train_end(self, trainer: Any, pl_module: Any) -> None:
            super().on_train_end(trainer, pl_module)
            self.runner.close()

        def on_exception(self, trainer: Any, pl_module: Any, exception: BaseException) -> None:
            self.runner.close()

    return EvalsiModelCheckpoint()


def evalsi_callback(
    evaluator: CheckpointEvaluator | None,
    *,
    wait: bool = False,
    stop_on_regression: bool = False,
    stop_file: str | None = None,
) -> Any:
    """A ``transformers.TrainerCallback`` that evaluates every saved checkpoint.

    ``stop_file`` stops training when that file appears, for example the one
    ``evalsi checkpoints watch --stop-file`` writes when a checkpoint
    regresses (the evaluator may then be None: the watcher evaluates)."""
    from transformers import TrainerCallback

    class EvalsiCallback(TrainerCallback):  # type: ignore[misc]
        def __init__(self) -> None:
            self.runner = CheckpointRunner(evaluator, wait=wait) if evaluator else None

        def on_save(self, args: Any, state: Any, control: Any, **kwargs: Any) -> Any:
            path = Path(args.output_dir) / f"checkpoint-{state.global_step}"
            if self.runner is not None and state.is_world_process_zero:
                self.runner.submit(str(path), int(state.global_step))
            return control

        def on_step_end(self, args: Any, state: Any, control: Any, **kwargs: Any) -> Any:
            if stop_on_regression and self.runner is not None and self.runner.regressed:
                logger.warning("a checkpoint regressed against the base model; stopping")
                control.should_training_stop = True
            if stop_file and Path(stop_file).exists():
                logger.warning("%s exists (a checkpoint regressed); stopping", stop_file)
                control.should_training_stop = True
            return control

        def on_train_end(self, args: Any, state: Any, control: Any, **kwargs: Any) -> Any:
            if self.runner is not None:
                self.runner.close()
            return control

    return EvalsiCallback()
