# Runtime verification follow-up, 2026-10-18

This follows the [50-row inventory](LEFTOVERS-2026-10-17.md) and the
[paused EKS handoff](EKS-PAUSED-2026-10-18.md). Historical passes remain
evidence for their recorded platforms. This session does not recreate the
customer cluster or certify another cloud's kernel, network or identity.

## Current scope

The managed workspace is Linux x86-64 with three logical CPUs, about 10 GiB
RAM, a 32-GiB filesystem, Docker 28.4.0 and Go 1.26.9. There is no `/dev/kvm`,
Firecracker binary, GPU device, configured cluster, writable delegated cgroup,
or configured model/cloud identity. Bare bubblewrap namespaces work; the
product profile under this workspace UID hits `Resource temporarily unavailable`
before entering its namespace. The kernel reports Landlock unavailable.
Those failed controls are retained as limitations, not counted as passes.

The new [preflight](../../tests/verification/preflight.py) checks KVM's actual
API and VM creation, not merely a file's presence. The opt-in
`TestFirecrackerRealKVM` requires real hardware and a digest-pinned image,
and cannot fall back to fakefc, bubblewrap or unconfined execution. The
[manual workflow](../../.github/workflows/firecracker-kvm.yml) requires an
existing self-hosted KVM runner; adding it does not provision one. Its
positive hardware path remains unverified here. A requested run on this
workspace fails explicitly on absent `/dev/kvm`.

The [reproduction guide](../../tests/verification/README.md) covers the
preflight, real-KVM controls, disposable-container controls and free trace
stores. [Competitor benchmarks](../../tests/benchmarks/README.md) have separate
correctness and performance scopes. No simulated driver test becomes new
hardware evidence, and no local SDK result becomes a hosted-service claim.

## Checks completed in this workspace

| Check | Observed result |
| --- | --- |
| Real-KVM prerequisite and explicit opt-in | Device absent; opt-in fails at `/dev/kvm`. No hardware pass. |
| Source sandbox suite in disposable Debian container, `go test -race` | Passed in 35.798 s. Real bwrap file/network/ptrace/timeout/memory/snapshot controls passed. Fake Firecracker and fake Kubernetes tests remain contract coverage. |
| Container skips | Real KVM opt-in, absent Landlock and undelegated cgroup controls skipped. No unprivileged-pod or KVM certification. |
| Phoenix 20.20.0 live connector | OTLP seed, List/Fetch/mapping and two feedback writes passed with race detection. Stable annotation ID, exactly one persisted latest score of zero, independently read back. |
| MLflow 3.17.0 live connector | Real SDK trace seed, List/Fetch and replacement feedback passed with race detection. Independently read exactly one latest score of zero. |
| Local correctness controls | Eight tests passed, including fake-device rejection and benchmark missing/duplicate/NaN/normalization controls. |
| `make check` | Passed: Go formatting/vet/tests, Ruff/mypy, 935 Python tests (three skips), protobuf lint/format. |
| Workflow validation | Actionlint passed with the explicit custom `kvm` runner label configured. |
| Hosted free-tier access | LangSmith unauthenticated listing returned 401. Braintrust anonymous project listing returned 200, but no authenticated workspace/experiment was established; create probes returned 400. No hosted benchmark or persisted cloud score is claimed. |

Both free local trace-store servers used separate disposable SQLite state and
synthetic data, then were stopped before timing. Their live checks exercise the
source connector, not the entire policy/worker loop or provider UI. Hosted free
allowances are described by the vendors, but this workspace has no associated
account/API key; free pricing does not supply an authenticated test workspace.

## Complete remaining-check disposition

IDs retain the original inventory. “Retain” means its existing bounded pass
stands; it does not close the broader item. No historical inventory is rewritten.

| ID | Item | Next useful verification / present constraint |
| --- | --- | --- |
| L01 | Stock Firecracker | Real KVM test/workflow prepared; hardware absent. Stock image/chart still needs its own KVM-cluster run. |
| L02 | User-namespaced bwrap pools | Retain EKS pass; local container controls are narrower than `hostUsers:false` Kubernetes. |
| L03 | Calico + ambient egress | Retain leak reproduction and opt-out control; native ambient enforcement needs the actual mesh. |
| L04 | EKS admission / hostNetwork | Retain bounded live EKS/kind controls; overlay routing needs the target cluster. |
| L05 | STRICT mTLS webhooks | Retain real kind/sidecar exception controls; do not call them an EKS rerun. |
| L06 | PR #12 platform recheck | AKS, waypoint and stock KVM deployment still require those platforms. |
| L07 | Trace sources / M3 | Free MLflow and Phoenix live APIs rechecked, persisted score replacement read back; ClickHouse PR #94 retained. Real LangSmith, Databricks and SageMaker need workspaces/identity. |
| L08 | Reference demos | Full real-model research/Terminal-Bench suites need a model/judge and task capacity. |
| L09 | Frontier model handling | Native Responses, cache hits, retries/fallback and signed-thinking repair need real providers. Retain failed cache controls. |
| L10 | Long-horizon H2 | Real 300-step/200k-token quality, queue failover and external side effects remain; scripted unit tests are insufficient. |
| L11 | Persistent shell | Implementation required. |
| L12 | CLI agents H3 | Other real products, held-out/transfer experiments need their CLIs/models; ACP/A2A push/sweeps remain unbuilt. |
| L13 | Environments H4 | Retain protected-checker controls; browser/desktop and clean-snapshot checker remain unbuilt. |
| L14 | Framework profiles | Retain ten recorder configurations; native Responses, handoffs, retries/errors need live framework/provider runs. |
| L15 | 10k concurrent trials | Dedicated sized cluster and actual agent-trial load, not unary API requests. |
| L16 | Real SWE-bench Verified | Actual image/task/checker execution remains distinct from synthetic fixtures and agent-quality claims. |
| L17 | Unprivileged cgroups | Actual writable delegation and enforcement required; unavailable in outer workspace. |
| L18 | Kata | Real KVM and an installed RuntimeClass required. |
| L19 | Private webhook destinations | Default/allowlist decision and implementation required. |
| L20 | Arbitrary server SQL | Intentionally not planned; server-side slicing is separate. |
| L21 | Platform wheels | Retain Linux release evidence; macOS/Windows sandbox bundles are unbuilt. |
| L22 | Retention | Per-label rules, deletion grace/backup/log erasure are implementation work. |
| L23 | Prior-review limits | Upload double-spooling and SDK failed-batch retry are known limits, not missing infrastructure tests. |
| L24 | Pipeline/schedule certification | Retain local two-API/MinIO checks; AWS S3, HA/failover, callbacks and production load remain. |
| L25 | Sandbox quota boundaries | Existing documented scope; broader accounting would need implementation. |
| L26 | Keyless sandbox image verification | Implementation required; release signature verification is a different path. |
| L27 | Enterprise audit/delegation | Implementation/design work; an outside compliance certification is not a unit test. |
| L28 | LongHarness/Evo2 follow-ups | Human-calibrated judges and real held-out experiments require data/models; unfinished features remain. |
| L29 | agentgateway feedback | Routing-contract decision and implementation required. |
| L30 | Separate process roles | Intentional current architecture; change only after relevant load evidence. |
| L31 | Pod snapshots | Implementation required. |
| L32 | Firecracker tap/nftables | Intentionally uses vsock/proxy; not planned. |
| L33 | Caller rate limits | Design and implementation required. |
| L34 | Reversible migrations | Retain N-1 bounded-reader control; dual-version writes/migration framework remain. |
| L35 | Restore/PITR | Retain PostgreSQL WAL drill; managed/off-site, ClickHouse/JetStream and production RPO/RTO remain. |
| L36 | Judge injection | Retain six live controls; broader model-specific and human-labelled calibration needs models/data. |
| L37 | UI accessibility | Retain automated axe/keyboard checks; manual screen-reader audit remains. |
| L38 | Penetration test | Independent tester and scope required. |
| L39 | Second maintainer | Human onboarding required. |
| L40 | BFCL memory/web | Implementation required. |
| L41 | Native tau2 user | Current structured-output adapter has a documented different contract. |
| L42 | Terminal-Bench subset | Retain bounded multi-service import evidence; refused/multistage tasks remain unsupported. |
| L43 | FrontierCode/CursorBench | Verifiable dataset access required; private tasks cannot be synthesized. |
| L44 | verl/OpenRLHF training | Retain real verl 0.9.1/OpenRLHF 0.9.0 updates; current 0.11.2, quality and scale need compatible GPU runtime. |
| L45 | 4,096 rollout target | Requires 256 warm microVMs and sized KVM capacity; no extrapolation from a small batch. |
| L46 | Operator checkpoints | Retain PVC/LoRA controls; published fixed-image deployment remains separate from source-built binaries. |
| L47 | Generic checkpoint serving | Serving contract/implementation required. |
| L48 | Environment-spec training | Native trainer-backed run requires GPU/model; sharing setup across verifiers rollouts is separately unbuilt. |
| L49 | Coding product over MCP | Retain real CLI/MCP evidence; secret-backed GitHub workflow and other products need their setup. |
| L50 | Wasm IO/Azure ML/SQL | Intentional unsupported capabilities/provider API restrictions. |

No M4/M5, scale, stock-KVM deployment or hosted-platform certification is
closed by this follow-up. The actionable next hardware check is L01; the
next credential-dependent check is the existing disposable-project LangSmith
workflow. Provisioning paid infrastructure is outside this local run.
