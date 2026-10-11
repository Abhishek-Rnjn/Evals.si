import json
from pathlib import Path

import httpx
import torch

from evalsi import Logger
from evalsi.training import PyTorchCheckpointHook
from evalsi.training.loop import StepResult


class TensorEvaluator:
    def __init__(self) -> None:
        self.steps: list[int] = []

    def evaluate(self, checkpoint: str, step: int) -> StepResult:
        state = torch.load(checkpoint, map_location="cpu", weights_only=True)
        assert state["global_step"] == step
        assert state["state_dict"]
        assert all(torch.isfinite(value).all() for value in state["state_dict"].values())
        self.steps.append(step)
        return StepResult("cpu-test", str(step), checkpoint)


def test_real_pytorch_checkpoint_hook(tmp_path: Path) -> None:
    model = torch.nn.Linear(2, 1)
    optimizer = torch.optim.SGD(model.parameters(), lr=0.1)
    evaluator = TensorEvaluator()
    with PyTorchCheckpointHook(evaluator) as hook:
        for step in range(1, 3):
            optimizer.zero_grad()
            model(torch.ones(1, 2)).square().mean().backward()
            optimizer.step()
            hook.save(model, tmp_path / f"step-{step}.pt", step, optimizer=optimizer)
    assert evaluator.steps == [1, 2]


def test_real_lightning_checkpoint_hook(tmp_path: Path) -> None:
    import lightning.pytorch as pl
    from torch.utils.data import DataLoader, TensorDataset

    from evalsi.training import lightning_checkpoint_callback

    class Model(pl.LightningModule):
        def __init__(self) -> None:
            super().__init__()
            self.layer = torch.nn.Linear(2, 1)

        def training_step(self, batch: tuple[torch.Tensor], batch_idx: int) -> torch.Tensor:
            return self.layer(batch[0]).square().mean()

        def configure_optimizers(self) -> torch.optim.Optimizer:
            return torch.optim.SGD(self.parameters(), lr=0.1)

    evaluator = TensorEvaluator()
    callback = lightning_checkpoint_callback(
        evaluator, dirpath=str(tmp_path), every_n_train_steps=1, save_on_train_epoch_end=False
    )
    trainer = pl.Trainer(
        accelerator="cpu",
        devices=1,
        max_steps=2,
        logger=False,
        enable_progress_bar=False,
        enable_model_summary=False,
        callbacks=[callback],
    )
    trainer.fit(Model(), DataLoader(TensorDataset(torch.ones(4, 2)), batch_size=2))
    assert evaluator.steps == [1, 2]


def test_real_tensor_logging() -> None:
    received: list[dict] = []

    def send(request: httpx.Request) -> httpx.Response:
        received.extend(json.loads(request.content)["resourceSpans"][0]["scopeSpans"][0]["spans"])
        return httpx.Response(200, json={})

    with Logger("http://localhost", transport=httpx.MockTransport(send)) as logger:
        future = logger.log(torch.tensor([1.0, 2.0], requires_grad=True), torch.tensor([3.0]))
    assert future.result()
    attributes = {item["key"]: item["value"]["stringValue"] for item in received[0]["attributes"]}
    assert json.loads(attributes["input.value"]) == [1.0, 2.0]
    assert json.loads(attributes["output.value"]) == [3.0]


def test_real_pandas_and_huggingface_batches() -> None:
    import pandas as pd
    from datasets import Dataset

    received: list[dict] = []

    def send(request: httpx.Request) -> httpx.Response:
        received.extend(json.loads(request.content)["resourceSpans"][0]["scopeSpans"][0]["spans"])
        return httpx.Response(200, json={})

    records = [{"input": "question", "output": "answer"} for _ in range(2)]
    with Logger("http://localhost", transport=httpx.MockTransport(send)) as logger:
        futures = logger.log_batch(pd.DataFrame(records))
        futures += logger.log_batch(Dataset.from_list(records))
    assert all(future.result() for future in futures)
    assert len(received) == 4
