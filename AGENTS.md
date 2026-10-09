# Notes for agents working on Evals.si

Read this first. It says where things live, which documents are the source of truth, and what must pass before you push.

## Where things stand

- Phases 0 to 6 of the roadmap in [docs/DESIGN.md](docs/DESIGN.md) are built.
- The next phase is defined in [docs/PRD.md](docs/PRD.md): Evals.si as a service customers run in their own Kubernetes cluster, with three integration modes:
  - push: traces and API calls;
  - pull: connectors that read MLflow, Langfuse and Phoenix;
  - embed: SDK calls, including training loops.

  agent-studio-standalone is the first integration. The reference demos use Deep Agents and DeepSeek Harness as stand-ins for it.
- [docs/IMPLEMENTATION-PROMPT.md](docs/IMPLEMENTATION-PROMPT.md) is the brief for that phase: scope, fixed decisions, rules and the definition of done.
- [docs/verification/](docs/verification/README.md) is a real end-to-end run on kind, AKS and an Istio ambient cluster, with each finding's status. Most findings were fixed in PR #12; the README lists what is still open.
- [docs/LEFTOVERS.md](docs/LEFTOVERS.md) lists what is deferred or never verified.

## Source of truth, in order

1. The code and its tests: the current state.
2. [docs/PRD.md](docs/PRD.md): the target. Requirement IDs (D*, P*, S*, E*, A*, R*) and milestones M1–M5 are defined there.
3. [docs/decisions/](docs/decisions/README.md): why things are the way they are. Write a new record for a significant design choice, in the same format.
4. [docs/DESIGN.md](docs/DESIGN.md): the architecture.
5. [docs/guides/](docs/guides/) and [README.md](README.md): user-facing behaviour. Keep them true when behaviour changes.

Verification findings are named VR-F1…, VR-D1…, VR-R1… so they don't clash with the PRD's D1–D11.

## Layout

| Path | What it is |
|------|------------|
| `proto/` | The API's single source of truth (Connect, gRPC, REST, CRDs). Change it first, then `make proto` |
| `cmd/evalsid`, `internal/` | The Go server: API, runs, ingest, policy engine (`internal/watch`), sandbox, auth, sinks |
| `operator/` | The Kubernetes operator: `EvalRun`, `OnlineEvalPolicy`, `Evaluator`, `SandboxClass`. Run `make operator-gen` after API changes |
| `python/evalsi/` | The Python package: CLI, SDK, evaluator packs, worker, rewards, training hooks |
| `python/adapters/` | Framework and benchmark adapters, each in its own environment |
| `deploy/helm/` | Charts: `evalsi`, `evalsi-crds`, `evalsi-sandboxd`. Chart tests are in `tests/helm` |
| `deploy/e2e/kind-e2e.sh` | The kind end-to-end job CI runs |
| `examples/` | Run specs, policies and configs used by the docs and tests |
| `website/` | The documentation site (Astro Starlight) |

## Checks

Run what CI runs (`.github/workflows/ci.yml`) and make it pass before you push:

- `make check`: gofmt, go vet, go test, ruff, ruff format --check, mypy, pytest, buf lint and buf format. CI also runs `go test -race ./...`.
- `make adapters-check` when you touch `python/adapters/`.
- `make e2e` for server and worker changes.
- `deploy/e2e/kind-e2e.sh` for chart, operator and demo changes, when Docker is available. Say so when you could not run it.

Never skip, disable or weaken a test to get green.

## Conventions

- Match the surrounding code: Go core, Python runtime (decision 0002).
- Sandboxes fail closed: never run code with weaker isolation than asked, and never claim isolation that isn't enforced.
- There is no default judge model; nothing is billed by surprise. Records missing what an evaluator needs are skipped, and evaluator failures are errors, never zeros.
- Charts must work on any conformant cluster. Every pod and Service a chart adds takes tolerations, nodeSelector, labels and the image registry as values (see the "Taints and service meshes" section of docs/guides/kubernetes.md).
- No secrets, API keys, internal hostnames or private registry names in commits.
- When something lands or is deferred, update the PRD's status cells, LEFTOVERS.md, docs/verification/README.md and README.md.
