# Fine-tuning and RL

Evals.si serves fine-tuning and RL in two places:

- **During training**, evaluators become rewards: a `RewardSpec` composes verifiers into one reward per rollout, with a breakdown per component. TRL, verl and OpenRLHF call it in-process or through the server's Reward Service.
- **Between checkpoints**, every saved checkpoint is evaluated against run specs. The learning curve compares each checkpoint with the base model, record by record, and regression gates fail on significant drops.

Examples are in [`examples/finetuning`](../../examples/finetuning).

## Rewards

### A reward spec

```yaml
apiVersion: evals.si/v1alpha1
kind: RewardSpec
metadata: {name: code-grpo}
spec:
  components:
    - ref: format-check
      name: format
      weight: 0.1
      gate: true
      params: {pattern: "(?s).*```python\\n.*```.*"}
    - ref: code-exec-tests
      name: tests
      weight: 0.9
      sandbox: {minIsolation: namespaced}
  cache: {enabled: true}
```

A component is any record-scope evaluator with its params. The total follows these rules (the Python library and the server share them, checked by `testdata/reward_vectors.json`):

| Field | Meaning |
|---|---|
| `weight` | The component adds `weight × value`; pass/fail counts as 1 or 0. |
| `gate`, `threshold` | A gate must score at least `threshold` (default 1). If any gate fails, or a gate is skipped or errors, the total is `floor` (default 0). |
| `onError` | An errored component (for example, the sandbox is unavailable): `raise` (default) fails the batch; `zero` counts it as 0; `none` gives no total. The default is `raise` because training on a reward of 0 during an outage teaches the model the wrong thing. |
| Skipped components | A component that does not apply (no reference, no test cases) adds nothing and shows as skipped. |
| `clip: [low, high]` | Bounds the total last. |
| `cache` | Component scores are cached by content. Repeated rollouts, which GRPO produces often, cost nothing. |
| `sandbox.minIsolation` | Becomes the `min_isolation` param of components that run code. |
| `judge` | In-process: `JudgeConfig` fields. On a server: the name of a judge configured there. |

### Verifiers (the `rl` pack)

| Evaluator | What it checks |
|---|---|
| `format-check` | The completion matches a regex (default `<think>…</think><answer>…</answer>`), and optionally that given tags each appear exactly once. |
| `math-equiv` | Extracts the final answer (`\boxed{}`, `<answer>`, `#### x`, "the answer is x", else the last number) and compares it with the reference: numbers, fractions, percentages, tuples, and with sympy (`evalsi[math]`) or math-verify installed, symbolic equivalence. |
| `code-exec-tests` | Runs the program against test cases in the sandbox, each case in its own process, and scores the fraction that pass (`all_or_nothing` gives 1 or 0). Cases are stdin/stdout pairs (`tests`, or APPS-style `inputs`/`outputs`) or asserts (MBPP `test_list`, or a test script). The result line carries a random nonce, so the program cannot print a forged result. |
| `overlong-penalty` | DAPO's soft overlong penalty: 0 up to `max_length − buffer`, falling linearly to −1 at `max_length`. |
| `reward-model` | A served reward model: OpenRLHF's protocol, or vLLM's `/pooling` endpoint. Requests made together (within `wait_ms`, up to `max_batch`) go in one call. |

Any other evaluator works as a component too, for example `llm-judge` as a generative reward model, or `json-schema`.

### Rollouts

A rollout is a mapping:

- `prompt` (or `input`) and `completion` (or `output`): text or chat messages;
- the reference from `referenceField`, or the first of `reference`, `ground_truth`, `answer`, `solution` and `label`;
- `completion_tokens`, or a `completion_ids` list, for token counts;
- everything else (test cases, metadata) goes to the record's metadata.

```bash
evalsi rewards score --spec code-reward.yaml --data rollouts.jsonl
```

### TRL

```python
from trl import GRPOConfig, GRPOTrainer
from evalsi.rewards.trl import reward_funcs

funcs, weights = reward_funcs("code-reward.yaml")   # server="https://evalsi..." to score remotely
trainer = GRPOTrainer(
    model=model,
    reward_funcs=funcs,
    args=GRPOConfig(..., reward_weights=weights),
    train_dataset=dataset,   # columns such as `tests` or `answer` reach the reward
)
```

`funcs` holds the total (weight 1) and one function per component with weight 0. TRL therefore logs `rewards/code-grpo/tests/mean` and the other components beside the total, without changing what is trained on. A loaded reward is itself a TRL reward function, if you only want the total.

### verl

```yaml
custom_reward_function:
  path: /path/to/site-packages/evalsi/rewards/verl.py   # python -c "import evalsi.rewards.verl as m; print(m.__file__)"
  name: compute_score              # compute_score_batch with the batch reward manager
  reward_kwargs:
    spec: /path/to/reward.yaml
    server: https://evalsi.example.com   # optional
```

It returns `{"score": total, <component>: value}`. verl trains on `score` and logs the rest.

### OpenRLHF

You can pass `--remote_rm_url` either of two things:

- the file `evalsi/rewards/openrlhf.py`, with `EVALSI_REWARD_SPEC` set;
- the URL of `evalsi rewards serve --spec reward.yaml --port 5000`, which speaks OpenRLHF's remote reward-model protocol.

The completion is the query after its prompt, the label is the reference, and components come back as `extra_logs`.

### On the server: the Reward Service

`RewardService.ScoreRewards` (gRPC, Connect, or `POST /v1alpha1/rewards:score`) takes a spec and a batch of rollouts as records.

- Components run on the same worker path as `Evaluate`: batched, in parallel, on the cpu or sandbox pools. On Kubernetes, code runs in the sandbox pool.
- Component scores are cached in memory (`rewards.cache_size`), and with `rewards.shared_cache` also in the database, so replicas share them (pruned after `rewards.shared_cache_ttl`, default a week).
- Per-project quotas can bound a project's rollouts in flight (`quotas.*.max_reward_rollouts`).
- Calls past `rewards.max_inflight` rollouts wait, which pushes back on the trainer instead of overloading the sandboxes.
- Authorization: the `evaluations.run` permission, with `resource.runs_code` for code components.
- Metrics: `evalsi_reward_*`.

```yaml
# evalsi.yaml
rewards:
  max_rollouts: 16384     # per call
  max_inflight: 65536     # across calls; further calls wait
  cache_size: 1048576     # component scores
```

### Agent tasks as RL environments

`evalsi_harness.gym.TaskEnv` turns an agent task (an environment with an image, setup and a checker) into `reset`/`step` episodes:

- Actions are the sandbox tools (`bash`, `read_file`, `write_file`) or `submit`.
- The checker grades the end state; its score is the reward.
- A checker that cannot run gives no reward, not 0.

Setup is snapshotted once per environment, so `reset` is a restore on rungs that snapshot.

#### As an OpenEnv or verifiers environment

The same tasks can be handed to trainers that speak other environment specs, so one task definition serves evaluation here and training there (`pip install openenv-core==0.3.0` or `verifiers==0.3.1`, the versions tested):

```python
from evalsi_harness.envspecs.openenv import TaskOpenEnv, TaskAction, create_task_app
env = TaskOpenEnv([(record, spec)])             # an openenv Environment
obs = env.reset(seed=0)                          # obs.instruction
obs = env.step(TaskAction(tool="submit"))        # obs.done, obs.reward
app = create_task_app([(record, spec)])          # an OpenEnv server (WebSocket sessions)

from evalsi_harness.envspecs.verifiers import load_environment
vf_env = load_environment([(record, spec)])      # a verifiers StatefulToolEnv
```

Both give the model `bash`, `read_file`, `write_file` and `submit`, and reward the checker's score when the episode ends. In OpenEnv the reward is `None` until then; in verifiers a checker that cannot run raises an `InfraError` that ends the rollout with `state["error"]` set, rather than scoring 0. They are written against `openenv-core` 0.3 and `verifiers` 0.3 and tested against those installs in CI (`tests/envspecs`), including a full verifiers rollout against a scripted chat server. With `client=` given, verifiers rollouts share one environment pool, so setup runs once per environment; without it each rollout connects and pays setup itself. OpenEnv's HTTP `/reset` and `/step` endpoints are stateless by design, so use its WebSocket session.

#### As a Gymnasium or NeMo Gym environment

```python
from evalsi_harness.envspecs.gymnasium import TaskGymEnv
env = TaskGymEnv([(record, spec)])               # a gymnasium.Env (gymnasium==1.4.0)
obs, info = env.reset(seed=0)
obs, reward, terminated, truncated, info = env.step(
    {"tool": "bash", "arguments": '{"command": "pytest -q"}'})

from evalsi_harness.envspecs.nemogym import TaskResourcesServer, dataset_row
row = dataset_row(record, spec)                  # a NeMo Gym dataset row (nemo-gym==0.7.0, Python 3.13+)
```

`submit` ends a Gymnasium episode as `terminated`, the step limit as `truncated`; the checker grades both. `TaskResourcesServer` is a NeMo Gym `GymnasiumServer` (`/reset`, `/step`) for use with its `gymnasium_agent`: each function call in a response is one step. A checker that cannot run raises in both (`InfrastructureError`, a failed request) because a reward of 0 would train on an infrastructure failure. Both are tested against the real packages in CI; for NeMo Gym that includes its own `gymnasium_agent` driving an episode with a scripted model (the server behind a test client), and neither has driven a trainer. `TaskGymEnv` runs its own event loop on a thread, so leave `client` unset and let it connect to the sandbox service itself.

#### Step-level rewards

`TaskEnv(process_rewards=[...])` returns each step's reward in `StepResult.process_reward`, separate from the checker's outcome `reward`. `EvaluatorReward("builtin/loop-detection")` scores the trajectory so far with any record-scope evaluator (by default the change in its score, so a step that does not move it earns 0; `mode="level"` pays the score itself; lower-is-better metrics count negatively), and `ToolErrorPenalty(0.1)` takes a fixed amount off a failed call. A score that cannot be computed is `None`, never 0, and `info["process_errors"]` names evaluator errors. A judge-backed evaluator makes one judge call per step. Whether shaped rewards help training is not something this repository measures.

#### Rollouts, with tokens

```python
from evalsi.targets import TargetConfig
from evalsi_harness.models import create_model
from evalsi_harness.rollout import RolloutPool

model = create_model(TargetConfig(connector="openai-compatible", model="m",
                                  base_url="http://vllm:8000/v1", token_trace=True))
pool = RolloutPool(model, sandbox_client, concurrency=16)
trajectories = await pool.run([(record, spec)] * 8)       # eight rollouts, one setup
t = trajectories[0]
t.reward, t.process_reward, t.turns[0].tokens             # outcome, shaped, token IDs and log-probs
pool.stop()                                               # pause the running ones at a turn boundary
paused = await pool.run(resume=paused_trajectories)       # ...and continue them later
```

`token_trace=True` asks a vLLM server for each turn's prompt token IDs, generated token IDs and log-probabilities (`return_token_ids`, `return_tokens_as_token_ids`, `logprobs`), so a trainer need not re-tokenize. What the server did not return is named in `TokenTrace.missing` and `Trajectory.tokens_complete` is false; a turn served by a fallback model has no tokens at all. It is also a run-spec field (`target.token_trace: true`, with `target.token_trace_server: sglang` for SGLang; the Go server validates both). It was written against vLLM's documented fields and tested against a fake server, and the SGLang parser against SGLang's OpenAI protocol source (`return_token_ids`; `prompt_token_ids` and `response_token_ids` on each choice; on a stream `sglext.input_ids` and `sglext.output_ids`, since SGLang refuses `return_token_ids` there), also with a fake server: there has been no live vLLM or SGLang run. Pausing keeps a snapshot of each episode's sandbox, so it needs a rung that snapshots (it ran on bubblewrap and Landlock in the real-rung tests; Firecracker is untested). On a rung that cannot snapshot (the pod rung) a stopped rollout is `cancelled` with its turns kept, and `pool.run(resume=[...])` resumes it by replaying its recorded tool calls into a fresh sandbox, then continuing from the recorded conversation. Replay is re-execution, so it is refused (the trajectory comes back `error`, saying why, and nothing is set up) when it is not safe: a record whose `metadata.replay_safe` is false, whose `metadata.non_idempotent_tools` names a tool the trajectory called (or is `true`), or whose sandbox has network access (unless `metadata.replay_safe` is true). Observations that differ from the recorded ones are listed in `info["replay"]["diverged"]`; `strict_replay=True` makes the first one an error. Only tested with a fake non-snapshotting sandbox, not on a real pod. Cancelling the `run` destroys every sandbox.

## Evaluating checkpoints

```bash
# Once: the base model
evalsi checkpoints eval Qwen/Qwen3-8B --step base -f forgetting.yaml --training-run grpo-1 \
  --serve lora --base-url http://localhost:8000/v1

# While training: every new checkpoint in the output directory
evalsi checkpoints watch out/ -f forgetting.yaml --training-run grpo-1 \
  --serve lora --base-url http://localhost:8000/v1 --regression math-equiv:0.02

# The learning curve; exits 3 when the latest checkpoint regressed
evalsi checkpoints curve -f forgetting.yaml --training-run grpo-1 --regression math-equiv:0.02
```

```text
step  math-equiv           status
base  0.712                ok
100   0.731 (+0.019)       ok
200   0.684 (-0.028*)      ok
300   0.641 (-0.071!)      regressed
```

### Serving checkpoints with the operator

On Kubernetes the watcher can ask the [operator](kubernetes.md) for a vLLM per checkpoint instead of starting one itself. Run it in the cluster (a Job, or beside the trainer), with `kubectl` and permission to manage `checkpointservers` in the namespace:

```bash
evalsi checkpoints watch /checkpoints/run-1 -f forgetting.yaml --training-run grpo-1 \
  --serve operator --namespace evalsi --image vllm/vllm-openai:v0.11.0 \
  --checkpoint-claim run-1-checkpoints --gpus 1
```

For each checkpoint it applies a `CheckpointServer`, waits for `Ready`, evaluates against `status.endpoint` and deletes the resource, which frees the GPUs. For LoRA adapters, create one `CheckpointServer` with `lora: true` for the base model and use `--serve lora --base-url <its endpoint>` as before. The endpoint is a cluster-internal Service address.


`*` marks a significant change, `!` a regression.

**Serving.** The run spec's target is pointed at the served checkpoint. Three strategies:

- `--serve vllm` starts `vllm serve <checkpoint>` on a free port and stops it afterwards. Use it for full checkpoints.
- `--serve lora` hot-loads the checkpoint as an adapter into a running vLLM, then unloads it, so one base-model server serves every step. Start that vLLM with `--enable-lora` and `VLLM_ALLOW_RUNTIME_LORA_UPDATING=True`.
- `--serve endpoint` uses a model that is already served.

**Checkpoints.** A checkpoint is a directory named `checkpoint-N`, `global_step_N` or `step-N`. It is evaluated once it holds a model and has not changed for `--settle` seconds. `s3://`, `gs://` and `hf://` locations work with fsspec installed, and are downloaded before serving. `mlflow:<model>` watches a registered model's versions (with `MLFLOW_TRACKING_URI`): the step is the version's `step` tag, else its version number, and artifacts the MLflow server proxies are downloaded through it.

**Stopping training from the watcher.** `--stop-file PATH` writes PATH when a checkpoint regresses. `evalsi_callback(None, stop_file=PATH)` in the trainer stops at the next step, and any other training loop can check the file the same way.

**One-shot exit status.** `watch --once` still attempts every discovered
checkpoint. It exits 1 if any evaluation attempted by this invocation
(including `--base`) errors, otherwise 3 if any fails its gates or regresses,
otherwise 0. An error takes precedence over a gate failure. Continuous
watching keeps scanning and reporting failures instead of exiting.

**Where results live.**

- Embedded: under `--log-dir/<training-run>`, one JSON file per step with per-record values for pairing.
- With `--server`: as server runs labelled `training-run` and `training-step`. Comparisons then use `CompareRuns`.

**Regression gates.** `metric[:max_drop]` fails a step when the metric is significantly below the base model's: the paired confidence interval of the difference excludes zero, and the drop exceeds `max_drop`. The run spec's own gates still apply to each step.

**From the trainer.**

```python
from evalsi.training import CheckpointEvaluator, LoRA
from evalsi.training.callback import evalsi_callback

evaluator = CheckpointEvaluator("forgetting.yaml", training_run="grpo-1",
                                serving=LoRA("http://localhost:8000/v1"),
                                regression=["math-equiv:0.02"])
trainer = GRPOTrainer(..., callbacks=[evalsi_callback(evaluator, stop_on_regression=True)])
```

Each save is evaluated on a background thread, one at a time and in order. With `wait=True` the trainer waits for each evaluation. With `stop_on_regression`, training stops at the next step after a regression is found. Pending evaluations finish before `train()` returns.

## Plain PyTorch and Lightning

```python
from evalsi.training import PyTorchCheckpointHook, lightning_checkpoint_callback

with PyTorchCheckpointHook(evaluator) as hook:
  hook.save(model, "checkpoints/step-100.pt", 100, optimizer=optimizer)

callback = lightning_checkpoint_callback(
  evaluator, dirpath="checkpoints", every_n_train_steps=100,
  save_on_train_epoch_end=False,
)
trainer = lightning.Trainer(callbacks=[callback])
```

The PyTorch hook publishes the state dictionary atomically before evaluation;
use a unique path for every step. The file gets the mode your umask gives any
file you create, so a separate evaluation pod under another user can read it.
Under distributed training (DDP) call it on every rank: only rank zero saves and
evaluates. Sharded training such as FSDP must gather the full state on rank zero
before it calls `save`. Lightning evaluation starts after the real
`ModelCheckpoint` save, on global rank zero, and drains at train end or error.
Async mode keeps all checkpoints (`save_top_k=-1`, no `save_last`) so a queued
file cannot be deleted by top-k retention. Both hooks use `CheckpointRunner`;
`wait=True` evaluates synchronously. An evaluator implements
`evaluate(checkpoint, step) -> StepResult`. Supply a serving strategy that reads
state dictionaries: `.pt`/`.ckpt` files are not automatically vLLM models.
Real CPU save/evaluate tests run in separate PyTorch and Lightning environments.

## Logging a local loop to the server

An evaluation run in your own process (`evalsi.evaluate()`, an embedded `evalsi run`, a checkpoint hook) can be stored on the server as a finished run, so it shows beside the cluster's runs in the web UI, `evalsi report` and `CompareRuns`:

```python
import evalsi
from evalsi.client import Client

result = evalsi.evaluate(records, ["exact-match", my_function_evaluator])
with Client("https://evalsi.example.com") as client:
    run = client.log_run(result, name=f"step-{step}", labels={"loop": "sft"},
                         gates=[{"metric": "exact-match", "min": 0.8}])
print(run["status"])   # RUN_STATUS_FAILED when a gate failed
```

Nothing is evaluated again: the server (`RunService.ImportRun`, `POST /v1alpha1/runs:import`) stores the records, results and summaries it is sent, checks the gates against those summaries, and sends `run.finished` and `run.gate_failed` webhooks as for its own runs. The evaluators may be ones the server does not have, such as a function in your script. An imported run has `imported: true` and cannot be resumed. Importing needs `runs.create` in the project, as creating a run does. At most 100000 records and a million results go in one import; a run's outputs keep one record per id, with every trial's results.

## Fine-tuning and RL evaluations

| Concern | How |
|---|---|
| Target-task gain | Your task suite per checkpoint; the curve against the base model. |
| Catastrophic forgetting | General-capability suites ([`forgetting.yaml`](../../examples/finetuning/forgetting.yaml), lm-eval-harness tasks through its adapter) with regression gates. |
| Contamination | `contamination`: n-gram overlap with the training data; Min-K% Prob when the records carry token log-probabilities. |
| Reward hacking | `reward-hacking` over logged rollouts: reward vs a held-out grade (gap, rank correlation), reward vs length, KL to the reference policy. Watch the per-component breakdown during training too. |
| Diversity and mode collapse | `diversity`: distinct-n, self-BLEU and entropy, optionally within samples of one prompt (`group_by`). |
| Calibration | `calibration`: ECE and Brier score of stated confidence. |
| Safety regression | [`safety-regression.yaml`](../../examples/finetuning/safety-regression.yaml): refusal and harmlessness against the base model. |
| Sampling behaviour | [`sampling.yaml`](../../examples/finetuning/sampling.yaml): trials at temperature 1 give pass@k and pass^k. |

## Testing

- **The CI `trainers` job** runs TRL's `GRPOTrainer` on CPU with a tiny model built in the test.
  - Code rewards are scored in the real sandbox.
  - The callback evaluates every checkpoint.
  - It checks the wiring, not learning: a randomly initialised model's outputs fail every test.
- **The CI `rlframeworks` job** runs Evals.si rewards inside verl 0.9.1 and OpenRLHF 0.11.2 themselves, on CPU.
  - verl loads `evalsi/rewards/verl.py` through `custom_reward_function`, and its `NaiveRewardManager` and `BatchRewardManager` score a rollout batch with it.
  - OpenRLHF's single-turn rollout executor calls the reward both as a `--remote_rm_url` file and over HTTP from `evalsi rewards serve`.
  - No training runs: that needs GPUs and an inference engine. The generation engine is a stand-in, and OpenRLHF is installed without its GPU dependencies (flash-attn, deepspeed).
- **The e2e suite** scores the same code rollouts in-process and through the Reward Service, on the real sandbox, and checks that they agree.
- **The load test** measures the Reward Service on a GRPO step's worth of rollouts.
- **Opt-in EKS GPU verification** ran a native verl 0.9.1 step and an
  OpenRLHF **0.9.0** compatibility control with actual model generations,
  Evals.si rewards, nonzero gradients and changed checkpoint weights.
  See the [exact versions, commands and limitations](../verification/EKS-GPU-2026-10-17.md).
  OpenRLHF 0.11.2's full native training stack is not certified by the CPU
  adapter tests or that older control. Its pinned vLLM/CUTLASS stack requires
  protobuf <7, while the SDK requires >=7.35; run the SDK HTTP reward server
  in a separate environment rather than forcing incompatible dependencies.

## Controlled contamination canaries

Create private completion canaries before preparing a controlled training
corpus:

```bash
evalsi canary create --count 10 --output canaries.jsonl --training-output canary-training.txt
```

The text file contains a random prefix followed by a separate 128-bit secret
suffix. Include those lines only in the training corpus whose exposure you
intend to measure. The probe JSONL contains each prefix as `input`, the secret
suffix as the held-out `reference`, and a canary marker. Preserve this probe
file privately; existing output files are refused rather than overwritten.

Probe the trained target through an ordinary `EvalRun` whose dataset is
`canaries.jsonl` and evaluator is `builtin/contamination-canary`. The target's
user message is the prefix alone; the reference must never enter that request.
The evaluator reports `true` when the final output contains the exact private
suffix (higher is worse). It skips records that are not controlled canaries
and refuses a suffix exposed directly in the probe input. For recorded target
outputs, use `evalsi eval --data outputs.jsonl --evaluators contamination-canary`.

A recalled high-entropy suffix is evidence of exposure, including possible
prompt or retrieval leakage. It does not identify a particular training source.
A failure to recall does not establish that the data was absent from training.
The canary controls here are validated locally, without training a live model.
