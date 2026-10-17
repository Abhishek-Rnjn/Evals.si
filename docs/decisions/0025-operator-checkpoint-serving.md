# 0025. The operator can serve checkpoints

- **Status:** Accepted, 2026-10-12. Implemented; bounded GPU serving verified on EKS on 2026-10-17 ([record](../verification/EKS-2026-10-17.md)).

## Context

[Decision 0013](0013-rewards-and-checkpoint-evaluation.md) had the evaluating process serve checkpoints: it starts `vllm serve`, hot-loads a LoRA adapter, or uses an endpoint. On Kubernetes that means the checkpoint watcher, which runs as a Job or beside the trainer, must itself hold a GPU and run vLLM inside its own pod. The operator already runs Deployments for evaluator pools and sandbox pools; a model server is the same shape.

## Decision

1. **A `CheckpointServer` custom resource** (`evals.si/v1alpha1`, namespaced, short name `evckpt`). The operator reconciles it into a Deployment and a Service and writes `status.endpoint`, `status.servedModelName`, `status.readyReplicas` and a `Ready` condition (true once the Deployment has rolled out its current spec and a replica of it passes vLLM's `/health`, so a spec change is not Ready on the old revision's replica).
2. **What the spec holds:** the vLLM `image` and `model` (a path under the mounted checkpoints, or a model id), an optional PVC (`checkpointClaim`, read-only at `/checkpoints`), `lora` (a base model with `--enable-lora` and `VLLM_ALLOW_RUNTIME_LORA_UPDATING=True`), an `apiKeySecret` (the key reaches vLLM through `VLLM_API_KEY` from the Secret, never an argument), resources (GPUs), env, node selector, tolerations, extra arguments, replicas and port. The default command is explicitly `vllm serve`, not the image's entrypoint; `spec.command` overrides it and must accept the same arguments. In v0.11.0 the stock image's API-server entrypoint accepts the positional model without using it as the model to load.
3. **Pod choices:** `Recreate` strategy, so a replacement pod does not wait for GPUs its predecessor holds; `/dev/shm` as a memory volume, which vLLM's processes share through; a startup probe that allows 30 minutes of model loading; privilege escalation off, all capabilities dropped, the runtime default seccomp profile. The root filesystem stays writable and the pod runs as the image's user, because vLLM images run as root and write caches; this is looser than the chart's own pods, and the reason is recorded here.
4. **The watcher drives it through `kubectl`**: `evalsi checkpoints watch|eval --serve operator --namespace NS --image IMAGE [--checkpoint-claim PVC] [--gpus N] [--api-key-secret S]` applies a resource per checkpoint, waits for `Ready`, evaluates against its endpoint and deletes it, also when the evaluation fails. `kubectl` keeps the Python package free of a Kubernetes client dependency; the process must run in the cluster, because the endpoint is a cluster-internal Service address.
5. **Access:** `checkpointservers` is in the aggregated edit role, with `evaluators`: both run an image of the user's choosing. The operator's role gains the resource and its status, and the controller is on by default (`--controllers=...,checkpointserver`).
6. **Helm does not upgrade CRDs**, so an operator running with an older `evalsi-crds` release starts without the controller and logs that, as it does for `TraceSource`.

## Consequences

- Evaluating a full checkpoint on Kubernetes no longer needs vLLM or a GPU in the watcher's pod.
- Serving by the evaluating process stays the default; nothing changes for existing setups.
- The resource and current controller passed bounded live EKS checks with stock vLLM 0.11.0 on an L40S: requested model identity, token generation, Secret authentication, readiness reset on rollout and child deletion. The [follow-up](../verification/EKS-HARNESS-2026-10-17.md) ran the current controller binary inside EKS with namespace-scoped RBAC, not a newly built image. The actual CLI watcher discovered two full Qwen PVC checkpoints, served/scored/deleted them and skipped an unchanged repeat scan. Read-only filesystem controls, synthetic zero-LoRA load/inference/unload, missing model/adapter rejection and deletion after an evaluation exception passed. These are serving/protocol checks, not training-quality certification. Pod Security `restricted` will not admit the stock root-running vLLM image; the live checkpoint namespace used `baseline`.
- The watcher does not pass the API key to the run's target; a target for a keyed server names the variable that holds it.
- A watcher that dies leaves its `CheckpointServer`: it is labelled `app.kubernetes.io/managed-by: evalsi` so it can be found and deleted. There is no garbage collection. The watcher retries a failed delete three times, raises if it still fails after a good evaluation, and otherwise notes the leak on the evaluation's own error and in the log.
