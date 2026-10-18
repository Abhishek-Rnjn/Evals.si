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
