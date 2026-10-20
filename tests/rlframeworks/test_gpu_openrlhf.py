"""Opt-in, one-GPU native OpenRLHF 0.11.2 training with real Evals.si HTTP rewards.

Requires requirements-gpu-openrlhf.txt in the trainer environment, a separate
SDK environment (EVALSI_OPENRLHF_SDK_PYTHON) for the HTTP reward server
(because vLLM 0.29.0's CUTLASS dependency requires protobuf <7 while the SDK
requires protobuf >=7.35), CUDA, and an explicit EVALSI_GPU_MODEL_PATH pointing
to Qwen2.5-0.5B-Instruct.
"""

from __future__ import annotations

import json
import math
import os
import subprocess
import sys
import urllib.request
from importlib.metadata import version
from pathlib import Path

import pytest

pytestmark = pytest.mark.skipif(
    os.environ.get("EVALSI_GPU_TRAINING") != "1",
    reason="set EVALSI_GPU_TRAINING=1 in an isolated one-GPU runtime",
)

_SERVER_SCRIPT = """\
import json
import sys
from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import Any

from evalsi.rewards import RewardResult, load
from evalsi.rewards.openrlhf import make_server

spec_path, audit_path = sys.argv[1], sys.argv[2]


class AuditedReward:
    def __init__(self, spec: str, audit: str) -> None:
        self._inner = load(spec)
        self._audit = Path(audit)

    def score(self, rollouts: Sequence[Mapping[str, Any]]) -> list[RewardResult]:
        results = self._inner.score(rollouts)
        with self._audit.open("a") as stream:
            stream.write(
                json.dumps(
                    {
                        "count": len(rollouts),
                        "completion_chars": [
                            len(str(r.get("completion", ""))) for r in rollouts
                        ],
                        "labels": [r.get("label") for r in rollouts],
                        "totals": [res.total for res in results],
                        "components": [
                            {k: c.value for k, c in res.components.items()}
                            for res in results
                        ],
                    }
                )
                + "\\n"
            )
        return results


server = make_server(AuditedReward(spec_path, audit_path), host="127.0.0.1", port=0)
print(server.server_address[1], flush=True)
server.serve_forever()
"""

_RAY_TRAINER_SCRIPT = """\
import os
import runpy
import site
import sys
import ray

for sp in site.getsitepackages():
    cu_inc = os.path.join(sp, "nvidia", "cu13", "include")
    cu_lib = os.path.join(sp, "nvidia", "cu13", "lib")
    if os.path.isdir(cu_inc):
        os.environ["CPATH"] = f"{cu_inc}:{os.environ.get('CPATH', '')}".rstrip(":")
        os.environ["CPLUS_INCLUDE_PATH"] = (
            f"{cu_inc}:{os.environ.get('CPLUS_INCLUDE_PATH', '')}".rstrip(":")
        )
    if os.path.isdir(cu_lib):
        os.environ["LIBRARY_PATH"] = (
            f"{cu_lib}:{os.environ.get('LIBRARY_PATH', '')}".rstrip(":")
        )
        os.environ["LD_LIBRARY_PATH"] = (
            f"{cu_lib}:{os.environ.get('LD_LIBRARY_PATH', '')}".rstrip(":")
        )
os.environ.setdefault("TORCH_CUDA_ARCH_LIST", "8.0")

ray.init(
    num_cpus=8,
    num_gpus=1,
    object_store_memory=536870912,
    include_dashboard=False,
)
sys.argv = ["openrlhf.cli.train_ppo_ray", *sys.argv[1:]]
runpy.run_module("openrlhf.cli.train_ppo_ray", run_name="__main__")
"""


def _extract_optimizer_stats(optim_ckpt_path: Path) -> tuple[int, float]:
    import torch

    ckpt = torch.load(optim_ckpt_path, map_location="cpu", weights_only=False)
    opt_state = ckpt.get("optimizer_state_dict", ckpt)
    base_state = opt_state.get("base_optimizer_state", opt_state)
    state_map = base_state.get("state", {})
    max_step = 0
    max_exp_avg = 0.0
    for entry in state_map.values():
        if not isinstance(entry, dict):
            continue
        if "step" in entry:
            step_val = entry["step"]
            step_int = int(step_val.item()) if hasattr(step_val, "item") else int(step_val)
            max_step = max(max_step, step_int)
        if "exp_avg" in entry and isinstance(entry["exp_avg"], torch.Tensor):
            tensor = entry["exp_avg"].float()
            assert torch.isfinite(tensor).all(), "non-finite Adam exp_avg"
            if tensor.numel() > 0:
                max_exp_avg = max(max_exp_avg, float(tensor.abs().max().item()))
    return max_step, max_exp_avg


def _cli_args(model: Path, rm_url: str, prompts_path: Path, root: Path) -> list[str]:
    return [
        "--actor.num_nodes",
        "1",
        "--actor.num_gpus_per_node",
        "1",
        "--vllm.num_engines",
        "1",
        "--vllm.tensor_parallel_size",
        "1",
        "--train.colocate_all",
        "--vllm.enforce_eager",
        "--vllm.gpu_memory_utilization",
        "0.25",
        "--actor.model_name_or_path",
        str(model),
        "--reward.remote_url",
        rm_url,
        "--data.prompt_dataset",
        str(prompts_path),
        "--data.input_key",
        "input",
        "--data.label_key",
        "label",
        "--data.apply_chat_template",
        "--train.micro_batch_size",
        "1",
        "--train.batch_size",
        "4",
        "--rollout.batch_size",
        "4",
        "--rollout.micro_batch_size",
        "1",
        "--rollout.n_samples_per_prompt",
        "1",
        "--data.max_samples",
        "4",
        "--train.max_epochs",
        "1",
        "--train.num_episodes",
        "1",
        "--algo.advantage.estimator",
        "reinforce",
        "--algo.advantage.gamma",
        "0.99",
        "--algo.kl.init_coef",
        "0",
        "--ds.zero_stage",
        "2",
        "--ds.param_dtype",
        "bf16",
        "--ds.attn_implementation",
        "sdpa",
        "--actor.adam.lr",
        "0.0001",
        "--actor.lr_warmup_ratio",
        "0",
        "--actor.adam.weight_decay",
        "0",
        "--data.max_len",
        "112",
        "--rollout.max_new_tokens",
        "16",
        "--ckpt.output_dir",
        str(root / "final"),
        "--ckpt.path",
        str(root / "checkpoints"),
        "--ckpt.save_steps",
        "1",
        "--ckpt.save_hf",
        "--logger.tensorboard_dir",
        str(root / "tensorboard"),
    ]


def _verify_saved_checkpoint(
    root: Path, model: Path, calls: Path, sdk_python: Path
) -> dict[str, object]:
    import torch
    from safetensors import safe_open

    rows = [json.loads(line) for line in calls.read_text().splitlines()]
    assert len(rows) == 5, f"expected 1 control + 4 rollout calls, got {len(rows)}"
    assert rows[0]["completion_chars"] == [0, 5]
    assert rows[0]["totals"] == [0.0, 1.0]
    rollout_rows = rows[1:]
    assert all(row["count"] == 1 for row in rollout_rows)
    assert all(row["completion_chars"][0] > 0 for row in rollout_rows)
    assert all(row["totals"] == [1.0] for row in rollout_rows)

    ds_actor_dir = root / "checkpoints" / "_actor"
    assert (ds_actor_dir / "latest").read_text().strip() == "global_step1"
    optim_files = list((ds_actor_dir / "global_step1").glob("*optim_states.pt"))
    assert len(optim_files) == 1, f"expected 1 optim_states.pt, found {optim_files}"
    opt_step, max_exp_avg = _extract_optimizer_stats(optim_files[0])
    assert opt_step == 1
    assert math.isfinite(max_exp_avg)
    assert max_exp_avg > 0.0

    saved_safetensors = root / "final" / "model.safetensors"
    if not saved_safetensors.is_file():
        saved_safetensors = root / "checkpoints" / "global_step1_hf" / "model.safetensors"
    assert saved_safetensors.is_file(), "missing saved HF model.safetensors"

    key = "model.layers.0.self_attn.q_proj.weight"
    with safe_open(str(model / "model.safetensors"), framework="pt", device="cpu") as original:
        before = original.get_tensor(key).float()
    with safe_open(str(saved_safetensors), framework="pt", device="cpu") as updated:
        after = updated.get_tensor(key).float()
    assert torch.isfinite(after).all()
    delta = (after - before).abs()
    changed = int(torch.count_nonzero(delta).item())
    assert changed > 0, "a checkpoint alone does not prove a parameter update"

    sdk_protobuf = subprocess.check_output(
        [
            str(sdk_python),
            "-c",
            "from importlib.metadata import version; print(version('protobuf'))",
        ],
        text=True,
    ).strip()

    return {
        "framework": "openrlhf",
        "versions": {
            package: version(package)
            for package in (
                "openrlhf",
                "vllm",
                "torch",
                "transformers",
                "deepspeed",
                "flash-attn",
                "protobuf",
            )
        },
        "sdk_protobuf_version": sdk_protobuf,
        "gpu": torch.cuda.get_device_name(0),
        "global_step": opt_step,
        "control_rewards": rows[0]["totals"],
        "reward_calls": len(rollout_rows),
        "max_abs_adam_exp_avg": max_exp_avg,
        "parameter": key,
        "changed_elements": changed,
        "max_absolute_change": float(delta.max().item()),
        "weight_decay": 0.0,
    }


def test_native_openrlhf_optimizer_step(tmp_path: Path) -> None:
    import torch

    assert torch.cuda.is_available(), "the opt-in run requires CUDA"
    assert version("openrlhf") == "0.11.2"
    assert version("vllm") == "0.29.0"
    model = Path(os.environ["EVALSI_GPU_MODEL_PATH"]).resolve()
    assert (model / "model.safetensors").is_file(), "provide the real Qwen 0.5B checkpoint"
    sdk_python = Path(
        os.environ.get("EVALSI_OPENRLHF_SDK_PYTHON", "/work/sdk-reward-venv/bin/python")
    )
    assert sdk_python.is_file(), f"missing SDK python at {sdk_python}"

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

    prompts_path = root / "prompts.jsonl"
    prompts_path.write_text(
        "\n".join(
            json.dumps(
                {
                    "input": f"Write two friendly sentences about {topic}.",
                    "label": "",
                }
            )
            for topic in ("gardens", "music", "walking", "reading")
        )
        + "\n"
    )

    server_proc = subprocess.Popen(
        [str(sdk_python), "-c", _SERVER_SCRIPT, str(spec), str(calls)],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    try:
        assert server_proc.stdout is not None
        port_line = server_proc.stdout.readline().strip()
        err_text = server_proc.stderr.read() if server_proc.stderr else ""
        assert port_line.isdigit(), f"reward server failed to start: {err_text}"
        port = int(port_line)
        rm_url = f"http://127.0.0.1:{port}/score"

        req = urllib.request.Request(
            rm_url,
            data=json.dumps(
                {
                    "query": ["prompt:", "prompt:hello"],
                    "prompts": ["prompt:", "prompt:"],
                    "labels": ["", ""],
                }
            ).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=15) as resp:
            control_payload = json.loads(resp.read().decode())
        assert control_payload == {
            "rewards": [0.0, 1.0],
            "scores": [0.0, 1.0],
            "extra_logs": {"nonempty": [0.0, 1.0]},
        }

        cli_args = _cli_args(model, rm_url, prompts_path, root)
        (root / "cli_args.json").write_text(json.dumps(cli_args, indent=2))
        log_path = root / "trainer.log"
        with log_path.open("w") as log:
            completed = subprocess.run(
                [sys.executable, "-c", _RAY_TRAINER_SCRIPT, *cli_args],
                stdout=log,
                stderr=subprocess.STDOUT,
                timeout=1500,
                check=False,
            )
    finally:
        server_proc.terminate()
        try:
            server_proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            server_proc.kill()

    output = log_path.read_text()
    assert completed.returncode == 0, output[-12000:]

    summary = _verify_saved_checkpoint(root, model, calls, sdk_python)
    (root / "summary.json").write_text(json.dumps(summary, indent=2))
    print(json.dumps(summary))
