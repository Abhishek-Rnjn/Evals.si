# Runtime and external-service verification

Run from the repository root. Prerequisite readiness, simulated driver tests,
real hardware tests, and customer-deployment certification are distinct results.

```sh
python tests/verification/preflight.py --out /tmp/preflight.json
python tests/verification/preflight.py --require kvm firecracker_binary
python/.venv/bin/python -m pytest tests/verification
```

`--require` exits 2 for missing prerequisites. The KVM check requires a real
character device, API version 12, and successful `KVM_CREATE_VM`; an ordinary
file or readable device node cannot pass. No module loading or device creation
is attempted. The bubblewrap check only tests bare namespace creation. Evals.si's
rlimits, mount rules and functional isolation require its actual sandbox tests.
No token values, kubeconfig contents or secret files are included in the output.

## Budgeted live Claude long-horizon task

Measured outcomes, failed controls and aggregate spending are recorded in the
[dated verification report](../../docs/verification/HORIZON-SERVICES-2026-10-21.md).

The opt-in [runner](long_horizon.py) uses the real `AgentLoop`, compaction and
model cassette with [300 synthetic incident decisions](long-horizon.yaml).
The [task/checker](horizon_task.py) runs in a real pod sandbox. Each decision
classifies current API/database/queue observations under an ordered runbook,
quotes evidence, and advances to the next incident. A release code from the
first incident must survive compactions until the last. Distractor logs include
untrusted instructions. This is not a public benchmark, a coding task, actual
production remediation or human-calibrated quality evidence.

Use a new owned pool and the SDK's `EVALSI_SANDBOX_ADDR=tls://...`,
`EVALSI_SANDBOX_TLS_CA`, `_CERT`, `_KEY` and `_SERVER_NAME` configuration.
The process requires the user's HTTPS `ANTHROPIC_BASE_URL`,
`ANTHROPIC_AUTH_TOKEN` and `ANTHROPIC_DEFAULT_HAIKU_MODEL` (or
`ANTHROPIC_DEFAULT_SONNET_MODEL` for Sonnet 5.5); these remain on the
client, not in sandbox files or the cluster.

```sh
python/.venv/bin/python tests/verification/long_horizon.py \
  --output /private/new-long-horizon-output
python/.venv/bin/python tests/verification/long_horizon.py \
  --replay /private/new-long-horizon-output/cassette.jsonl \
  --output /private/new-replay-output
python/.venv/bin/python -m pytest tests/verification/test_long_horizon.py -q
```

For the [Sonnet profile](long-horizon-sonnet.yaml), use
`--model-family sonnet --config tests/verification/long-horizon-sonnet.yaml
--completion-guard`. This uses a 6k compaction target and the standard bounded
`on_stop` hook to remind the agent if it stops before all incidents are submitted.
It supplies the current incident, not gold answers. The hook allows at most three
continuations. Include the same flags when replaying that cassette.

Replay has no live model or provider HTTP client and needs no Claude credential;
it reexecutes the recorded decisions in a fresh real pod with the same context
configuration. It still requires the pool's mutual-TLS credentials.

The output directory must be new/empty. It retains a model cassette, decisions,
per-HTTP-attempt accounting and summary, including on an error. Do not commit
private provider settings or arbitrary real application data alongside it.
For another paid attempt under the **same** allowance, supply
`--prior-budget /private/previous-output/budget.json`; this carries every
settled charge and uncertain reservation forward rather than resetting spend.
The request transport reserves estimated spend **before every HTTP attempt**,
including automatic retries and compaction calls. Failed/ambiguous attempts
retain their reservation. Ordinary model turns are limited to 1,024 tokens and
summaries to 512. Usage omissions stop the run rather than becoming
zero cost. By default, the ledger refuses a request that would exceed $14.50,
including $0.001 reserved for the smoke check. After explicit user authorization
to increase the total budget to $20, use `--budget-limit 19.5` on a new run or
paid cassette resume with `--prior-budget`; this changes the cutoff without
resetting any spending. The loop separately applies the selected cutoff to
trajectory estimates.

For the AKS follow-up's explicitly authorized $40 cumulative budget, the cutoff
was initially $36 and subsequently raised by the user to **$39.50**. Use
`--budget-limit 39.5 --prior-budget /private/latest-output/budget.json` for
subsequent runs. Never reset the ledger or discard failed reservations.

Rates are deliberately conservative for published Haiku pricing: $2.20 per
million input tokens **including all cache tokens at that same rate**, and
$5.50 per million output tokens.
For Sonnet 5.5, the respective conservative rates are $4.40 and $11 per million.
Historical attempts retain their original rates and charges when changing models.
The pre-request reservation uses serialized
UTF-8 request bytes plus 4,096 tokens of overhead; it is an estimate, not a
provider tokenizer measurement. These controls are **not a gateway-side invoice
cap**. Confirm the user's acceptance of the pricing basis or require a real
provider-enforced spending limit before making a strict billing promise.
See [published pricing](https://platform.claude.com/docs/en/about-claude/pricing).

The task-specific summary profile asks for at most 120 words, preserving the
approved release code and current incident ID. A failed initial control with
unconstrained 512-token summaries must remain a failure, not be erased by a rerun.

Report aggregate input separately from peak per-request input: 300 turns or
more than 200k **cumulative** tokens do not prove a single 200k-token context.
The configured 8k compaction window is an approximate management target, not
a hard tokenizer-enforced input limit. A passing task requires all 300
decisions, >=95% correct actions, exact observation quotes, retained release
code, >=300 model steps and normal completion; retain failed outcomes too.

## Multi-service task files and failure controls

The maintained [API/Redis task](../../examples/multi-service/run.yaml) and
[Terminal-Bench-format fixture](../../examples/multi-service/terminal-bench)
are exercised by [the basic and lifecycle runner](../../deploy/e2e/services.py).
Use its `--cases basic,lifecycle` selection to run service crash/startup
failure, startup/setup cancellation, deadline expiry, SDK-worker SIGKILL and
pool-process SIGKILL/recovery controls without a model token.

Do **not** set `network_policy_enforced: true` merely to make a test start.
The canary and actual cross-trial denial must pass. On the supplied AKS
cluster, unenforced policy blocks safe multi-service trials.

An isolated alternative uses the [single-node kind configuration](services-kind.yaml)
with default CNI disabled and Calico 3.30.3, and the existing
[pool/RBAC/NetworkPolicy manifest](../../deploy/e2e/services-pool.yaml).
The [scratch fixture image](services.Dockerfile) expects cross-built Linux
`horizon-evalsid` and `horizon-evalsi-guest` binaries in its build context.
Use a uniquely named owned cluster, ephemeral TLS and its own kubeconfig.
Never run the pool-kill test on a shared/customer cluster: its runner checks
the Docker node's kind-cluster label against `EVALSI_SERVICES_KIND_CLUSTER`.
Local-kind passes are Kubernetes evidence, not AKS networking certification.

## Real Firecracker

Use a Linux KVM host with Firecracker, `mkfs.ext4`, a compatible guest kernel
(virtio-blk, vsock and ext4), Go, and permission to open `/dev/kvm` read/write.
Obtain the kernel and Firecracker from trusted, checksum-verified releases.
Configure these paths privately; the test builds the real static guest from
this source checkout. The image needs `sh`, `true`, `cat` and `sleep`.

```sh
# Set EVALSI_FIRECRACKER_KERNEL to the actual vmlinux path.
# Set EVALSI_FIRECRACKER_IMAGE to the chosen image reference with @sha256:...
EVALSI_TEST_FIRECRACKER=1 go test -race ./internal/sandbox \
  -run '^TestFirecrackerRealKVM$' -count=1 -v -timeout 7m
```

Explicit opt-in fails if hardware or configuration is missing. Without opt-in,
the test skips; a skip is not KVM verification. There is no fake VM or weaker
driver fallback. The controls cover a real guest boot and vsock command channel,
VM isolation reporting, a host-only file denial, nonzero command exit, real
warm-pool consumption, snapshot/restore consistency and a command timeout.
This does not certify hostile-workload isolation, the jailer, egress policy,
256-VM reward scale, or the stock Kubernetes image/chart deployment.

The manual `firecracker-kvm.yml` workflow uses only a runner labelled
`self-hosted, linux, x64, kvm`; it is not dispatched automatically. Supply the
actual kernel path and digest-pinned image. It records preflight and Go JSON
evidence even on failure. No such runner is provisioned by this change.

## Disposable container for local sandbox controls

The outer workspace can accumulate processes belonging to its shared UID,
which interfere with `RLIMIT_NPROC` before bubblewrap enters its namespace.
Landlock may also be absent from the host kernel. Do not relax the product's
limits or relabel a skipped rung as a pass. A separate root-owned container
can exercise real bubblewrap while keeping those workspace limitations visible.

```sh
docker build --secret id=proxy_ca,src=/etc/ssl/certs/ca-certificates.crt \
  -f tests/verification/Dockerfile -t evalsi-runtime-verification:local .
# Mount the real Go toolchain and its populated module cache read-only.
# The privileged outer container permits nested namespaces; the sandbox under
# test must still enforce its own profile. This is not an unprivileged-pod test.
docker run --rm --privileged --network none \
  -v "$PWD:/src:ro" -v /path/to/go:/usr/local/go:ro \
  -v /path/to/go/pkg/mod:/go/pkg/mod:ro \
  evalsi-runtime-verification:local go test -race ./internal/sandbox -count=1 -v
```

The managed cloud environment additionally requires using its explicit local
Docker socket, preserving proxy defaults, and supplying CA trust as documented
by its runtime. The image's package installation needs network; tests run
without external networking. Kernel-dependent skips must remain visible.

## Free local trace stores

Install each SDK/server in a separate environment: `arize-phoenix==20.20.0`
and `mlflow==3.17.0`. Start disposable loopback-only servers with dedicated
working directories/databases; no cloud account or LLM is needed.

```sh
# In the Phoenix environment, with the local server on port 16006:
python tests/verification/trace_stores.py phoenix seed \
  --endpoint http://127.0.0.1:16006 --project evalsi-verification
EVALSI_TEST_PHOENIX_URL=http://127.0.0.1:16006 \
EVALSI_TEST_PHOENIX_PROJECT=evalsi-verification \
  go test -race ./internal/source/phoenix -run '^TestAgainstALiveServer$' -count=1 -v

# In the MLflow environment, with the local server on port 15000:
python tests/verification/trace_stores.py mlflow seed \
  --endpoint http://127.0.0.1:15000 --project evalsi-verification
# Use the returned experiment ID, not an assumed ID from another database.
EVALSI_TEST_MLFLOW_URL=http://127.0.0.1:15000 \
EVALSI_TEST_MLFLOW_EXPERIMENT=1 \
  go test -race ./internal/source/mlflow -run '^TestAgainstALiveServer$' -count=1 -v

# Supply the matching store, endpoint, project and trace ID returned by seed:
python tests/verification/trace_stores.py phoenix read-back \
  --endpoint http://127.0.0.1:16006 --project evalsi-verification --trace-id TRACE_ID
```

Read-back requires exactly one current `live-test` score of zero after the
connector writes one and then replaces it with zero. Stop the owned local
servers and delete their disposable state after collecting evidence. This is
live API integration coverage, not a hosted performance or UI benchmark.
