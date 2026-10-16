# Live CLI agent on kind

In Actions, run the `ci` workflow on the PR branch and enable `cli_agent_e2e`.
The trusted workflow requires the repository's `ANTHROPIC_API_KEY` secret; an
explicit opt-in without the secret fails. Pull requests never run this model
test or receive the secret. The default is off.

The Kubernetes job installs its normal kind stack, then builds the pinned Claude
Code 2.1.296 image and loads it into the kind nodes. At the pod-rung stage it runs
`bash tests/cli-agent/run.sh`, using the restricted namespace's sandbox pool over
mTLS. The harness runs on the CI runner and the real CLI executes inside a pod
sandbox, with only `api.anthropic.com` allowlisted. The key is passed in the CLI
process environment, never in image layers or run files. This is an embedded
run against the remote sandbox pool, not a distributed harness-worker test.

The model gets one tiny JavaScript repair task, a six-turn limit, a four-minute
process timeout and Claude's $1 budget cap. It must edit the file and pass a
checker placed after execution. The verifier independently checks the checker,
pod isolation, parsed model/tool/process steps, captured token usage and the
positive gate. A negative threshold control reuses the observed score without
another model call; it does not claim to test a regressed model. Results are
uploaded as `cli-agent-e2e/results.json`. The artifact is a real model run when
the opt-in executes, not one of the hand-written reader fixtures.

To run the whole pipeline locally with Docker, kind, kubectl, Helm, uv and the
Python workspace synced:

```sh
export ANTHROPIC_API_KEY=... # supply securely in your shell or secret manager
RUN_CLI_AGENT_E2E=1 bash deploy/e2e/kind-e2e.sh
```

`tests/cli-agent/run.sh` can also run against an existing pod sandbox pool using
`CLUSTER`, `CLI_AGENT_NAMESPACE` (default `evalsi-restricted`) and
`EVALSI_CLI_AGENT_ARTIFACTS`. It needs permission to read the pool's worker mTLS
Secret and port-forward its Service. Run it before changing that pool to
Landlock. This addition has offline spec/verifier tests; live verification is
pending until the opt-in workflow completes with a model key.
