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

`*` marks a significant change, `!` a regression.

**Serving.** The run spec's target is pointed at the served checkpoint. Three strategies:

- `--serve vllm` starts `vllm serve <checkpoint>` on a free port and stops it afterwards. Use it for full checkpoints.
- `--serve lora` hot-loads the checkpoint as an adapter into a running vLLM, then unloads it, so one base-model server serves every step. Start that vLLM with `--enable-lora` and `VLLM_ALLOW_RUNTIME_LORA_UPDATING=True`.
- `--serve endpoint` uses a model that is already served.

**Checkpoints.** A checkpoint is a directory named `checkpoint-N`, `global_step_N` or `step-N`. It is evaluated once it holds a model and has not changed for `--settle` seconds. `s3://`, `gs://` and `hf://` locations work with fsspec installed, and are downloaded before serving. `mlflow:<model>` watches a registered model's versions (with `MLFLOW_TRACKING_URI`): the step is the version's `step` tag, else its version number, and artifacts the MLflow server proxies are downloaded through it.

**Stopping training from the watcher.** `--stop-file PATH` writes PATH when a checkpoint regresses. `evalsi_callback(None, stop_file=PATH)` in the trainer stops at the next step, and any other training loop can check the file the same way.

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
- **The e2e suite** scores the same code rollouts in-process and through the Reward Service, on the real sandbox, and checks that they agree.
- **The load test** measures the Reward Service on a GRPO step's worth of rollouts.
