"""Opt-in, one-GPU native verl training with real Evals.si reward calls.

Requires requirements-gpu-verl.txt, CUDA and an explicit
EVALSI_GPU_MODEL_PATH pointing to Qwen2.5-0.5B-Instruct.
"""

from __future__ import annotations

import json
import math
import os
import re
import subprocess
import sys
from importlib.metadata import version
from pathlib import Path
from typing import Any

import pytest

pytestmark = pytest.mark.skipif(
    os.environ.get("EVALSI_GPU_TRAINING") != "1",
    reason="set EVALSI_GPU_TRAINING=1 in an isolated one-GPU runtime",
)


def audited_score(
    data_source: Any,
    solution_str: str,
    ground_truth: Any,
    extra_info: Any = None,
    *,
    spec: str,
    audit_path: str,
    **kwargs: Any,
) -> dict[str, Any]:
    from evalsi.rewards.verl import compute_score

    result = compute_score(
        data_source, solution_str, ground_truth, extra_info, spec=spec, **kwargs
    )
    with Path(audit_path).open("a") as stream:
        stream.write(
            json.dumps(
                {
                    "source": data_source,
                    "completion_chars": len(solution_str),
                    "result": result,
                }
            )
            + "\n"
        )
    return result


def test_native_verl_optimizer_step(tmp_path: Path) -> None:
    import torch
    from datasets import Dataset
    from evalsi.rewards.verl import compute_score
    from safetensors import safe_open

    assert torch.cuda.is_available(), "the opt-in run requires CUDA"
    model = Path(os.environ["EVALSI_GPU_MODEL_PATH"]).resolve()
    assert (model / "model.safetensors").is_file(), (
        "provide the real Qwen 0.5B checkpoint"
    )
    root = tmp_path.resolve()
    spec = root / "reward.json"
    calls = root / "reward-calls.jsonl"
    spec.write_text(
        json.dumps(
            {
                "apiVersion": "evals.si/v1alpha1",
                "kind": "RewardSpec",
                "metadata": {"name": "gpu-nonempty-control"},
                "spec": {
                    "components": [
                        {
                            "ref": "builtin/regex-match",
                            "name": "nonempty",
                            "weight": 1.0,
                            "params": {"pattern": "\\S"},
                        }
                    ]
                },
            }
        )
    )
    assert compute_score("control", "", "", spec=str(spec))["score"] == 0.0
    assert compute_score("control", "hello", "", spec=str(spec))["score"] == 1.0
    dataset = root / "train.parquet"
    Dataset.from_list(
        [
            {
                "data_source": "evalsi-gpu",
                "prompt": [
                    {
                        "role": "user",
                        "content": f"Write two friendly sentences about {topic}.",
                    }
                ],
                "ability": "integration",
                "reward_model": {"style": "rule", "ground_truth": ""},
                "extra_info": {"index": index},
            }
            for index, topic in enumerate(["gardens", "music", "walking", "reading"])
        ]
    ).to_parquet(str(dataset))
    overrides = [
        "transfer_queue.backend.SimpleStorage.num_data_storage_units=1",
        "algorithm.adv_estimator=reinforce_plus_plus",
        "algorithm.gamma=0.99",
        f"data.train_files={dataset}",
        f"data.val_files={dataset}",
        "data.train_batch_size=4",
        "data.val_batch_size=4",
        "data.max_prompt_length=96",
        "data.max_response_length=16",
        "data.dataloader_num_workers=0",
        f"actor_rollout_ref.model.path={model}",
        "actor_rollout_ref.model.use_remove_padding=False",
        "+actor_rollout_ref.model.override_config.attn_implementation=sdpa",
        "actor_rollout_ref.actor.ppo_mini_batch_size=4",
        "actor_rollout_ref.actor.ppo_micro_batch_size_per_gpu=1",
        "actor_rollout_ref.actor.optim.lr=0.0001",
        "actor_rollout_ref.actor.optim.weight_decay=0.0",
        "actor_rollout_ref.actor.use_kl_loss=False",
        "actor_rollout_ref.actor.use_torch_compile=False",
        "actor_rollout_ref.actor.fsdp_config.use_torch_compile=False",
        "actor_rollout_ref.actor.fsdp_config.model_dtype=bf16",
        "actor_rollout_ref.rollout.name=vllm",
        "actor_rollout_ref.rollout.tensor_model_parallel_size=1",
        "actor_rollout_ref.rollout.gpu_memory_utilization=0.25",
        "actor_rollout_ref.rollout.enforce_eager=True",
        "actor_rollout_ref.rollout.max_model_len=256",
        "actor_rollout_ref.rollout.max_num_batched_tokens=256",
        "actor_rollout_ref.rollout.max_num_seqs=4",
        "actor_rollout_ref.rollout.log_prob_micro_batch_size_per_gpu=1",
        "actor_rollout_ref.rollout.agent.num_workers=1",
        "actor_rollout_ref.rollout.n=1",
        "reward.num_workers=1",
        f"reward.custom_reward_function.path={Path(__file__).resolve()}",
        "reward.custom_reward_function.name=audited_score",
        f"+reward.custom_reward_function.reward_kwargs.spec={spec}",
        f"+reward.custom_reward_function.reward_kwargs.audit_path={calls}",
        "trainer.logger=[console]",
        "trainer.project_name=evalsi-eks",
        "trainer.experiment_name=gpu-one-step",
        "trainer.nnodes=1",
        "trainer.n_gpus_per_node=1",
        "trainer.total_epochs=1",
        "trainer.total_training_steps=1",
        "trainer.val_before_train=False",
        "trainer.test_freq=-1",
        "trainer.save_freq=1",
        "trainer.resume_mode=disable",
        f"trainer.default_local_dir={root / 'checkpoints'}",
        f"trainer.rollout_data_dir={root / 'rollouts'}",
        # Actor lifetime reservations oversubscribe CPUs, not the GPU or pod limit.
        "ray_kwargs.ray_init.num_cpus=8",
        "+ray_kwargs.ray_init.num_gpus=1",
        "+ray_kwargs.ray_init.object_store_memory=536870912",
        f"ray_kwargs.ray_init.runtime_env.py_executable={sys.executable}",
    ]
    (root / "overrides.json").write_text(json.dumps(overrides, indent=2))
    log_path = root / "trainer.log"
    with log_path.open("w") as log:
        completed = subprocess.run(
            [sys.executable, "-m", "verl.trainer.main_ppo", *overrides],
            stdout=log,
            stderr=subprocess.STDOUT,
            timeout=1500,
            check=False,
        )
    output = log_path.read_text()
    assert completed.returncode == 0, output[-12000:]
    rows = [json.loads(line) for line in calls.read_text().splitlines()]
    assert len(rows) == 4
    assert all(row["source"] == "evalsi-gpu" for row in rows)
    assert all(row["completion_chars"] > 0 for row in rows)
    assert all(row["result"] == {"score": 1.0, "nonempty": 1.0} for row in rows)
    match = re.search(r"actor/grad_norm:([0-9.eE+-]+)", output)
    assert match is not None, output[-12000:]
    grad_norm = float(match[1])
    assert math.isfinite(grad_norm) and grad_norm > 0.0
    assert "training/global_step:1" in output
    checkpoint = root / "checkpoints" / "global_step_1" / "actor"
    assert (
        root / "checkpoints" / "latest_checkpointed_iteration.txt"
    ).read_text().strip() == "1"
    assert (checkpoint / "optim_world_size_1_rank_0.pt").stat().st_size > 0
    state = torch.load(
        checkpoint / "model_world_size_1_rank_0.pt",
        map_location="cpu",
        weights_only=True,
    )
    key = "model.layers.0.self_attn.q_proj.weight"
    with safe_open(
        str(model / "model.safetensors"), framework="pt", device="cpu"
    ) as original:
        before = original.get_tensor(key).float()
    after = state[key].float()
    assert torch.isfinite(after).all()
    delta = (after - before).abs()
    changed = torch.count_nonzero(delta).item()
    assert changed > 0, "a checkpoint alone does not prove a parameter update"
    summary = {
        "framework": "verl",
        "versions": {
            package: version(package)
            for package in ("verl", "vllm", "torch", "transformers")
        },
        "gpu": torch.cuda.get_device_name(0),
        "global_step": 1,
        "reward_calls": len(rows),
        "grad_norm": grad_norm,
        "parameter": key,
        "changed_elements": changed,
        "max_absolute_change": delta.max().item(),
        "weight_decay": 0.0,
    }
    (root / "summary.json").write_text(json.dumps(summary, indent=2))
    print(json.dumps(summary))
