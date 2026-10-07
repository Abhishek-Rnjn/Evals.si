# 0013. Rewards from evaluators, composed twice; checkpoints served outside the operator

- **Status:** Accepted, 2026-10-06 (Phase 5)

## Context

Phase 5 makes evaluators usable as RL rewards and evaluates checkpoints as a model trains. Trainers (TRL, verl, OpenRLHF) call rewards from Python, at a rate set by rollout batches, and need a number per rollout even when parts of the reward do not apply. Checkpoint evaluation needs the checkpoint served somewhere an evaluation can reach.

## Decision

1. **A reward is a `RewardSpec` over ordinary evaluators.**
   - Components are any record-scope evaluators, with weights, gates and a floor; the `rl` pack adds verifiers shaped for rewards. There is no separate verifier interface, so an evaluator written for runs is a reward component too, and the reverse.
   - The breakdown per component is always returned, because reward hacking shows up there first.
2. **The rules are composed in two places, held together by shared vectors.**
   - In-process composition (`evalsi.rewards`) lets a trainer score without a server. The Reward Service composes in Go, so components fan out across workers and pools.
   - `testdata/reward_vectors.json` pins the rules, and both implementations are tested against it, as `testdata/stats_vectors.json` does for statistics.
3. **Errors fail the batch by default (`onError: raise`).** A sandbox outage that trained as reward 0 would teach the model the wrong thing, silently. `zero` and `none` are explicit choices. A skipped component (it does not apply) adds nothing and is not an error.
4. **Back-pressure, not rejection.** Calls past `rewards.max_inflight` rollouts wait for capacity until their deadline, so a trainer slows down instead of failing a step. Local sandboxes are capped at the CPU count, because timeouts are wall-clock and overload would fail correct programs.
5. **Checkpoints are served by the evaluating process, not the operator.**
   - Three strategies cover the cases: start `vllm serve` per checkpoint, hot-load LoRA adapters into a shared vLLM, or use an existing endpoint.
   - A run spec's target is pointed at the served checkpoint, so any run spec is a checkpoint suite.
   - The operator does not bring up vLLM per checkpoint yet: on Kubernetes, the LoRA strategy against a vLLM Deployment covers most uses, and GPU scheduling is the cluster's.
6. **The learning curve is paired against the base model.** Each step is compared record by record with the base model's run (a paired interval of the difference), and a regression gate needs a significant drop larger than its tolerance, so noise between checkpoints does not stop training.

## Consequences

- The same verifier code grades evaluation runs and training rollouts, so there is no gap between what was optimised and what is reported.
- Composition changes must touch both implementations and the vectors; the vectors make a mismatch a test failure.
- Embedded checkpoint results live in files under a log directory; with a server they are labelled runs, so access rules, audit and `CompareRuns` apply.
- Kubernetes users run the checkpoint watcher as a Job or beside the trainer; an operator-managed serving resource is a leftover.
