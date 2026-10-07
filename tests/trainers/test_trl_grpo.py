"""TRL GRPO with Evals.si rewards and checkpoint evaluation, on CPU.

A tiny randomly initialised Qwen2 model (built here, nothing downloaded)
trains for a few GRPO steps with a code reward scored in the real sandbox,
and every saved checkpoint is evaluated by the Evals.si TrainerCallback.
The model's outputs are noise; what is tested is the wiring: TRL calls the
reward functions with its batches, logs each component, and the callback
evaluates each checkpoint in order.

Needs torch, trl, transformers and datasets (see the trainers job in
.github/workflows/ci.yml) and EVALSID pointing at evalsid.
"""

from __future__ import annotations

import contextlib
import json
import os
import shutil
from collections.abc import Iterator
from pathlib import Path

import pytest

pytest.importorskip("trl")

from datasets import Dataset  # noqa: E402
from tokenizers import Tokenizer, decoders, models, pre_tokenizers, trainers  # noqa: E402
from transformers import PreTrainedTokenizerFast, Qwen2Config, Qwen2ForCausalLM  # noqa: E402
from trl import GRPOConfig, GRPOTrainer  # noqa: E402

from evalsi.rewards.trl import reward_funcs  # noqa: E402
from evalsi.training import CheckpointEvaluator, Served  # noqa: E402
from evalsi.training.callback import evalsi_callback  # noqa: E402

PROMPT = "Write a Python program that reads n and prints 2n."
TESTS = [{"input": "2", "output": "4"}, {"input": "5", "output": "10"}]


def tiny_model() -> tuple[Qwen2ForCausalLM, PreTrainedTokenizerFast]:
    corpus = [PROMPT, "```python\nprint(int(input()) * 2)\n```", "def f(n): return n * 2"] * 50
    tok = Tokenizer(models.BPE(unk_token="<unk>"))
    tok.pre_tokenizer = pre_tokenizers.ByteLevel(add_prefix_space=False)
    tok.decoder = decoders.ByteLevel()
    tok.train_from_iterator(
        corpus,
        trainers.BpeTrainer(
            vocab_size=300,
            special_tokens=["<unk>", "<pad>", "<eos>"],
            initial_alphabet=pre_tokenizers.ByteLevel.alphabet(),
        ),
    )
    tokenizer = PreTrainedTokenizerFast(
        tokenizer_object=tok,
        unk_token="<unk>",
        pad_token="<pad>",
        eos_token="<eos>",
        model_input_names=["input_ids", "attention_mask"],
    )
    config = Qwen2Config(
        vocab_size=len(tokenizer),
        hidden_size=32,
        intermediate_size=64,
        num_hidden_layers=2,
        num_attention_heads=2,
        num_key_value_heads=1,
        max_position_embeddings=128,
        pad_token_id=tokenizer.pad_token_id,
        eos_token_id=tokenizer.eos_token_id,
    )
    return Qwen2ForCausalLM(config), tokenizer


class Recorded:
    """Serving that records which checkpoints it was asked to serve, and
    points the run at a model that answers every question correctly."""

    def __init__(self, base_url: str) -> None:
        self.base_url = base_url
        self.checkpoints: list[str] = []

    @contextlib.contextmanager
    def serve(self, checkpoint: str, name: str) -> Iterator[Served]:
        assert Path(checkpoint, "config.json").is_file(), checkpoint
        self.checkpoints.append(checkpoint)
        yield Served(self.base_url, name)


def test_grpo_with_evalsi_rewards_and_checkpoints(tmp_path: Path) -> None:
    if not (os.environ.get("EVALSID") or shutil.which("evalsid")):
        pytest.skip("set EVALSID: code rewards run in the evalsid sandbox")
    from evalsi_testing_model import serve_echo_model

    spec = {
        "name": "code",
        "components": [
            {"ref": "python-syntax", "name": "syntax", "weight": 0.2},
            {"ref": "code-exec-tests", "name": "tests", "weight": 0.8},
        ],
    }
    funcs, weights = reward_funcs(spec)
    run_spec = tmp_path / "run.yaml"
    run_spec.write_text(
        json.dumps(
            {
                "apiVersion": "evals.si/v1alpha1",
                "kind": "EvalRun",
                "metadata": {"name": "ckpt"},
                "spec": {
                    "target": {"connector": "openai-compatible", "model": "m", "base_url": "http://x"},
                    "dataset": {"inline": {"records": [{"id": "a", "input": {"text": "4"}, "reference": {"text": "4"}}]}},
                    "evaluators": [{"ref": "exact-match"}],
                },
            }
        )
    )
    with serve_echo_model() as base_url:
        serving = Recorded(base_url)
        evaluator = CheckpointEvaluator(
            run_spec, training_run="grpo", serving=serving, log_dir=tmp_path / "log"
        )
        model, tokenizer = tiny_model()
        data = Dataset.from_list([{"prompt": PROMPT, "tests": TESTS}] * 8)
        args = GRPOConfig(
            output_dir=str(tmp_path / "out"),
            per_device_train_batch_size=4,
            num_generations=4,
            max_completion_length=12,
            max_steps=2,
            save_steps=1,
            logging_steps=1,
            report_to=[],
            use_cpu=True,
            reward_weights=weights,
            temperature=1.0,
        )
        trainer = GRPOTrainer(
            model=model,
            processing_class=tokenizer,
            reward_funcs=funcs,
            args=args,
            train_dataset=data,
            callbacks=[evalsi_callback(evaluator)],
        )
        trainer.train()

    logs = [h for h in trainer.state.log_history if "reward" in h]
    assert len(logs) == 2
    for key in ("rewards/code/mean", "rewards/code/syntax/mean", "rewards/code/tests/mean"):
        assert key in logs[0], sorted(logs[0])
    # Every completion was scored; random tokens fail the tests, never error.
    results = funcs[0].last_results  # type: ignore[attr-defined]
    assert len(results) == 4
    assert all(r.components["tests"].status == "scored" for r in results)
    # Both checkpoints were served and evaluated, in order.
    assert [Path(c).name for c in serving.checkpoints] == ["checkpoint-1", "checkpoint-2"]
    steps = [(r.step, r.summaries["exact-match"]["mean"]) for r in evaluator.history()]
    assert steps == [("1", 1.0), ("2", 1.0)]
