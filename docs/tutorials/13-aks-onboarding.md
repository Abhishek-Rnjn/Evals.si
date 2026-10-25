# Onboarding on AKS: fake tools, real models, and honest failure controls

This continues the [Mac and kind re-verification](12-macos-reverification.md).
We used the supplied AKS cluster, selected its stronger node, installed the
published SDK and harness afresh, and exercised real Linux sandboxes. We then
compared Haiku with Sonnet and checked storage, recovery and release decisions.
The [measured report](../verification/AKS-ONBOARDING-2026-10-25.md) retains failures,
cost accounting and the distinction between local clients and in-cluster runs.

This is a **disposable synthetic lab**, not a production deployment or a fresh
Helm-release certificate. The commands below adapt the tested setup to a new
namespace; they do not recreate another person's cluster credentials. Start
with [the install course](01-install.md) if the SDK itself is new to you.

## 1. Choose the machine before choosing the test

Set an explicit kubeconfig on every command. Do not change the default context:

```bash
export KUBECONFIG=/absolute/path/to/your-authorized-kubeconfig
export NS=evalsi-onboarding-lab
kubectl --kubeconfig "$KUBECONFIG" get nodes \
  -o custom-columns='NAME:.metadata.name,SKU:.metadata.labels.node\.kubernetes\.io/instance-type,CPU:.status.allocatable.cpu,MEMORY:.status.allocatable.memory,TAINTS:.spec.taints'
kubectl --kubeconfig "$KUBECONFIG" top nodes
```

Our choice was `Standard_D8s_v5`, an 8-vCPU/32-GiB class node with about
7.82 allocatable CPUs and 26.3 GiB allocatable memory. The other two nodes
were smaller, system-tainted D4 nodes. More RAM did not make all throughput
targets pass: resource selection and a performance certificate are different.

The [lab manifest](../../tests/verification/aks-onboarding-lab.yaml) carries the
tested node selector, infra toleration, deadlines and resource bounds. Adapt
the SKU and toleration to your authorized cluster. If multiple nodes have that
SKU, this selector chooses a class, not a particular least-loaded node.

The `worker` pod is not privileged. The separate `pool` is a **privileged outer
container**, used only to test nested bubblewrap and KVM with synthetic data.
Neither mounts the host filesystem or a service-account token. Privilege is
nevertheless powerful; obtain explicit permission before creating it. Do not
use this pod configuration for untrusted production tenants.

Refuse an existing namespace rather than overwriting it:

```bash
set -euo pipefail
existing="$(kubectl --kubeconfig "$KUBECONFIG" get namespace "$NS" \
  --ignore-not-found -o name)"
test -z "$existing" || { echo "Choose a new owned namespace" >&2; exit 1; }
kubectl --kubeconfig "$KUBECONFIG" create \
  -f tests/verification/aks-onboarding-lab.yaml
kubectl --kubeconfig "$KUBECONFIG" -n "$NS" wait \
  --for=condition=Ready pod/worker pod/pool --timeout=180s
```

If you change `NS`, change the namespace and ownership labels throughout the
manifest and use that same ownership value in the probes and cleanup below.
All data lives in `emptyDir`: **copy evidence before deleting the namespace**.
No CNI, RuntimeClass, shared node or cluster-wide controller is installed here.

## 2. Install the published SDK and harness entirely inside AKS

Install the observed missing prerequisites in the disposable outer pool:

```bash
kubectl --kubeconfig "$KUBECONFIG" -n "$NS" exec pool -- sh -c \
  'apt-get update && apt-get install -y --no-install-recommends bubblewrap git ca-certificates'
kubectl --kubeconfig "$KUBECONFIG" -n "$NS" exec pool -- \
  python -m venv /work/published
kubectl --kubeconfig "$KUBECONFIG" -n "$NS" exec pool -- \
  /work/published/bin/python -m pip install 'evalsi==0.3.1' 'evalsi-harness==0.3.1'
kubectl --kubeconfig "$KUBECONFIG" -n "$NS" exec pool -- \
  /work/published/bin/evalsi doctor --require agent
kubectl --kubeconfig "$KUBECONFIG" -n "$NS" cp examples/tinylm pool:/work/tinylm
kubectl --kubeconfig "$KUBECONFIG" -n "$NS" exec pool -- sh -c \
  'PATH=/work/published/bin:$PATH EVALSI=/work/published/bin/evalsi bash /work/tinylm/check.sh'
```

The fresh 0.3.1 installation passed strict doctor and **all 18 scenarios**,
including deliberately failing gates, stopped-model errors, record and replay.
“All scenarios passed” means each expected exit code and result shape matched,
not that every deliberately bad agent succeeded. TinyLM needs no provider key.

The nonprivileged worker refused strict agent readiness on this cluster.
That is a useful negative control: the SDK did not silently replace a required
namespaced sandbox with unconfined execution.

## 3. Keep source and published environments separate

For current-source helpers on your workstation:

```bash
(cd python && uv sync --all-packages)
PRIVATE="$(mktemp -d)"
chmod 700 "$PRIVATE"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath -o "$PRIVATE/evalsid" ./cmd/evalsid
kubectl --kubeconfig "$KUBECONFIG" -n "$NS" cp "$PRIVATE/evalsid" pool:/work/evalsid
kubectl --kubeconfig "$KUBECONFIG" -n "$NS" exec pool -- chmod 755 /work/evalsid
```

Keep this Linux binary distinct from the bundled Mac executable and the
published 0.3.1 binary. Our current-source Python worker reported `0.3.1.dev0`;
the published environment reported 0.3.1. Do not mix the installation evidence.

If running the compiled Go e2e binary inside the pod, also supply a matching
**Linux Go toolchain**. Its Wasm test builds code at runtime: a cross-compiled
test binary still carrying a Mac `GOROOT` failed. We restored the checksum-
verified Linux toolchain with `go mod download`, copied it into `/work/go`,
and reran with `GOROOT=/work/go` and `/work/go/bin` on `PATH`.

## 4. Connect a local harness to the real AKS pool

Use fresh short-lived mTLS certificates. These example names are not secrets:

```bash
umask 077
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -subj /CN=onboarding-ca -keyout "$PRIVATE/ca.key" -out "$PRIVATE/ca.crt"
openssl req -newkey rsa:2048 -nodes -subj /CN=localhost \
  -keyout "$PRIVATE/server.key" -out "$PRIVATE/server.csr"
printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\nextendedKeyUsage=serverAuth\n' \
  > "$PRIVATE/server.ext"
openssl x509 -req -days 1 -in "$PRIVATE/server.csr" \
  -CA "$PRIVATE/ca.crt" -CAkey "$PRIVATE/ca.key" -CAcreateserial \
  -extfile "$PRIVATE/server.ext" -out "$PRIVATE/server.crt"
openssl req -newkey rsa:2048 -nodes -subj /CN=onboarding-client \
  -keyout "$PRIVATE/client.key" -out "$PRIVATE/client.csr"
printf 'extendedKeyUsage=clientAuth\n' > "$PRIVATE/client.ext"
openssl x509 -req -days 1 -in "$PRIVATE/client.csr" \
  -CA "$PRIVATE/ca.crt" -CAkey "$PRIVATE/ca.key" -CAcreateserial \
  -extfile "$PRIVATE/client.ext" -out "$PRIVATE/client.crt"
printf 'sandbox:\n  ladder: [bwrap]\n  max_sandboxes: 8\n' > "$PRIVATE/pool.yaml"
for file in ca.crt server.crt server.key; do
  kubectl --kubeconfig "$KUBECONFIG" -n "$NS" cp "$PRIVATE/$file" "pool:/work/$file"
done
kubectl --kubeconfig "$KUBECONFIG" -n "$NS" cp "$PRIVATE/pool.yaml" pool:/work/pool.yaml
```

Only the CA **certificate**, server certificate and server key go into the
pool. Keep the CA private key, client private key and all provider credentials
on the workstation.

In one terminal, run the owned server in the foreground:

```bash
kubectl --kubeconfig "$KUBECONFIG" -n "$NS" exec pool -- \
  /work/evalsid sandbox serve --config=/work/pool.yaml \
  --listen=tcp://0.0.0.0:7443 --tls-cert=/work/server.crt \
  --tls-key=/work/server.key --client-ca=/work/ca.crt \
  --allow-client=onboarding-client
```

In another terminal, forward only to loopback:

```bash
kubectl --kubeconfig "$KUBECONFIG" -n "$NS" \
  port-forward --address=127.0.0.1 pod/pool 17443:7443
```

On the client, export:

```bash
export EVALSI_SANDBOX_ADDR=tls://localhost:17443
export EVALSI_SANDBOX_TLS_CA="$PRIVATE/ca.crt"
export EVALSI_SANDBOX_TLS_CERT="$PRIVATE/client.crt"
export EVALSI_SANDBOX_TLS_KEY="$PRIVATE/client.key"
EVALSI="$PWD/python/.venv/bin/evalsi" bash examples/tinylm/check.sh
```

This also passed all 18 scenarios, but it is **not the same topology** as
section 2: the source SDK, TinyLM and harness are local; tools/checkers execute
through the AKS pool. Starting with two sandbox slots was too small for the
suite's concurrency; eight slots resolved that setup failure. Git was also
required by the real coding fixture.

## 5. Use Haiku, then compare Sonnet without resetting spending

Load your authorized `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, and explicit
`ANTHROPIC_DEFAULT_HAIKU_MODEL` / `ANTHROPIC_DEFAULT_SONNET_MODEL` variables
privately. Never print or copy credentials into the pod.

The [live helper](../../tests/verification/aks_followup_live.py) runs the
[synthetic refund suite](../../examples/support-refund/README.md), including
the real shop CLI, state checker and approval hooks. It reserves cost before
**every HTTP attempt** and retains failed/uncertain reservations.

The following starts a new $10 campaign **only after user authorization**:

```bash
python/.venv/bin/python tests/verification/aks_followup_live.py \
  --model-family haiku --driver bwrap --trials 1 --budget-limit 10 \
  --output "$PRIVATE/haiku"
python/.venv/bin/python tests/verification/aks_followup_live.py \
  --model-family haiku --driver bwrap --approval-controls \
  --prior-budget "$PRIVATE/haiku/budget.json" --budget-limit 10 \
  --output "$PRIVATE/haiku-controls"
```

Our Haiku campaign found a genuine business-check miss: on
`ask-for-more-than-paid`, it refunded nothing instead of the expected capped
$25. The gated timeout case and no-answer control also failed their business
checker **intentionally** because approval policy blocked the refund; applicable
approval/denial scores passed. Do not combine those different failure meanings.

To compare Sonnet on three cases, with the **latest ledger from every prior
attempt**, including failed experiments:

```bash
python/.venv/bin/python tests/verification/aks_followup_live.py \
  --model-family sonnet --thinking enabled --driver bwrap \
  --baseline-only --trials 3 \
  --cases full-refund ask-for-more-than-paid gateway-timeout-after-write \
  --image mirror.gcr.io/library/python@sha256:d879ed88565a7a3c614b980b369d4a5025a9d057b71f93d09fc6d0b2ca823a18 \
  --prior-budget "$PRIVATE/haiku-controls/budget.json" --budget-limit 10 \
  --output "$PRIVATE/sonnet-thinking"
```

If you ran an intervening experiment, replace `--prior-budget` with **its**
written ledger. Never go back to an older ledger because a run failed.
The dated study also includes adaptive-low and adaptive-high attempts; its
final ledger carries those charges too.

Why the pinned public mirror? Docker Hub rate-limited repeated tag lookups
after six successful Sonnet trajectories. An unpacked root does not make a
mutable tag offline-safe. The verified mirror served the **same image digest**,
and the pinned retry completed 9/9 trajectories and three exact business-
checker replays with zero provider requests.

### Thinking blocks are evidence, not a checkbox

We explicitly tried:

| Configuration | Metered response limit | Observed result on the configured route |
|---|---:|---|
| `--thinking adaptive --effort low` | 1,024 | Real tool calls, no thinking blocks |
| Adaptive, high effort, two-turn multiplication control | 1,024 | Correct tool result, no thinking blocks; reported `thinking_tokens: 0` |
| `--thinking enabled` with a 1,024-token thinking budget | 2,048 | 9/9 refund checks, 21 tool calls, no thinking blocks; all 30 responses reported `thinking_tokens: 0` |

Explicit thinking opts into a tested 2,048-token ledger cap; the historical
default remains 1,024. The **total dollar cutoff is unchanged**. Sonnet's
final retry used 47,293 input tokens including caches and 3,372 output tokens.
The entire new Haiku/Sonnet campaign, including failed attempts, conservatively
estimated **$2.12206730 across 184 HTTP attempts** under the $10 authorization.
This is not a provider-enforced invoice guarantee.

The raw response is inspected before harness parsing. Thus this route did
**not expose thinking** in these tests; a configured flag does not establish
that the model thought, nor does visible prose establish signed thinking.
Live signed-thinking round-trip verification remains unavailable here.
The provider exposed structured `tool_use` blocks, **not a separate tool-token
counter or token-ID/logprob trace**. Tool arguments consume ordinary output
tokens. Do not rename total output tokens to “tool tokens.”

These are three synthetic records repeated three times, not nine independent
tasks or a model-quality certificate. Model and thinking configuration changed
together; this is not a causal ablation of thinking.

## 6. Test the cluster's actual limits

Run selected-node capability probes only in your labelled owned namespace:

```bash
python/.venv/bin/python tests/verification/aks_followup_probes.py \
  --namespace "$NS" --node-sku Standard_D8s_v5 \
  --owner-label onboarding-lab --output "$PRIVATE/capabilities.json"
```

Our default-deny network probe **still connected**. A Kata pod was admitted
but stayed unscheduled with zero matching nodes. Do not claim policy enforcement
from an accepted manifest, and do not install a cluster-wide CNI/runtime as an
unannounced onboarding “fix.”

The [dated report](../verification/AKS-ONBOARDING-2026-10-25.md) gives commands
and boundaries for the deeper checks:

- Nine load replicates: four failed the unchanged stored-throughput threshold;
  all passed freshness.
- Six SIGKILL drills: every previously observed stored trace survived, but
  **220 acknowledged, still-buffered traces were lost per trial**.
- Current-source PostgreSQL restart/read/write, exact ClickHouse/JetStream
  dependency restores, real KVM and jailed Firecracker boot/snapshot controls.
- A [real OS write-error probe](../../tests/verification/aks_followup_filesystem.py):
  strict audit returned HTTP 503 and did not apply the mutation. This is not a
  power-loss or read-only-remount test.
- Browser release decisions: **BLOCK**, **PASS**, and **INSUFFICIENT** are all
  correct outcomes for their controls. Three records with two trials do not
  become six independent records.

## 7. Save evidence, then remove only your resources

The original session saved logs, model results and a quiesced SQLite workbench
backup privately before cleanup. The user's downloaded `release-check.sh`
was inspected for its baseline and policy pins, not executed or modified.
Download automation had timed out; the native download was confirmed by the
user and verified on disk. Keep those two kinds of evidence distinct.

For your own lab, copy the result directories you actually created before
deleting `emptyDir` data. Close your foreground server and port-forward
terminals. Check ownership before deleting anything:

```bash
owner="$(kubectl --kubeconfig "$KUBECONFIG" get namespace "$NS" \
  -o jsonpath='{.metadata.labels.evalsi\.verification}')"
test "$owner" = onboarding-lab || { echo "Ownership mismatch" >&2; exit 1; }
kubectl --kubeconfig "$KUBECONFIG" delete namespace "$NS" --wait=true --timeout=180s
kubectl --kubeconfig "$KUBECONFIG" get namespace "$NS" --ignore-not-found
```

Remove only the exact ephemeral private keys you created once no longer needed;
do not recursively delete a workspace or someone else's credentials. The
temporary localhost workbench address stops working after cleanup; the
downloaded script's pinned policy remains useful evidence, not a durable server.
