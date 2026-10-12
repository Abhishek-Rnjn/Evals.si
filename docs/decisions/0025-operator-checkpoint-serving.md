# 0025. The operator can serve checkpoints

- **Status:** Accepted, 2026-10-12. Implemented; not yet run on a GPU cluster.

## Context

[Decision 0013](0013-rewards-and-checkpoint-evaluation.md) had the evaluating process serve checkpoints: it starts `vllm serve`, hot-loads a LoRA adapter, or uses an endpoint. On Kubernetes that means the checkpoint watcher, which runs as a Job or beside the trainer, must itself hold a GPU and run vLLM inside its own pod. The operator already runs Deployments for evaluator pools and sandbox pools; a model server is the same shape.

## Decision

1. **A `CheckpointServer` custom resource** (`evals.si/v1alpha1`, namespaced, short name `evckpt`). The operator reconciles it into a Deployment and a Service and writes `status.endpoint`, `status.servedModelName`, `status.readyReplicas` and a `Ready` condition (true once a replica passes vLLM's `/health`).
2. **What the spec holds:** the vLLM `image` and `model` (a path under the mounted checkpoints, or a model id), an optional PVC (`checkpointClaim`, read-only at `/checkpoints`), `lora` (a base model with `--enable-lora` and `VLLM_ALLOW_RUNTIME_LORA_UPDATING=True`), an `apiKeySecret` (the key reaches vLLM through `VLLM_API_KEY` from the Secret, never an argument), resources (GPUs), env, node selector, tolerations, extra arguments, replicas and port.
3. **Pod choices:** `Recreate` strategy, so a replacement pod does not wait for GPUs its predecessor holds; `/dev/shm` as a memory volume, which vLLM's processes share through; a startup probe that allows 30 minutes of model loading; privilege escalation off, all capabilities dropped, the runtime default seccomp profile. The root filesystem stays writable and the pod runs as the image's user, because vLLM images run as root and write caches; this is looser than the chart's own pods, and the reason is recorded here.
4. **The watcher drives it through `kubectl`**: `evalsi checkpoints watch|eval --serve operator --namespace NS --image IMAGE [--checkpoint-claim PVC] [--gpus N] [--api-key-secret S]` applies a resource per checkpoint, waits for `Ready`, evaluates against its endpoint and deletes it, also when the evaluation fails. `kubectl` keeps the Python package free of a Kubernetes client dependency; the process must run in the cluster, because the endpoint is a cluster-internal Service address.
5. **Access:** `checkpointservers` is in the aggregated edit role, with `evaluators`: both run an image of the user's choosing. The operator's role gains the resource and its status, and the controller is on by default (`--controllers=...,checkpointserver`).
6. **Helm does not upgrade CRDs**, so an operator running with an older `evalsi-crds` release starts without the controller and logs that, as it does for `TraceSource`.

## Consequences

- Evaluating a full checkpoint on Kubernetes no longer needs vLLM or a GPU in the watcher's pod.
- Serving by the evaluating process stays the default; nothing changes for existing setups.
- The resource, controller and `kubectl` flow are tested with a fake client and a fake `kubectl`. They have not run against an API server (the envtest suite is not extended), a real vLLM or a GPU node. Treat the probe timings, the security context under Pod Security `restricted` (it will not pass: vLLM images run as root) and PVC access modes as unverified.
- The watcher does not pass the API key to the run's target; a target for a keyed server names the variable that holds it.
- A watcher that dies leaves its `CheckpointServer`: it is labelled `app.kubernetes.io/managed-by: evalsi` so it can be found and deleted. There is no garbage collection.
