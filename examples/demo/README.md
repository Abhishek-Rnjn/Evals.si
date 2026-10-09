# Reference demos: Deep Agents and DeepSeek Harness

Two real agents stand in for an agent studio (see [integrate an agent studio](../../docs/guides/integrate-an-agent-studio.md)): [Deep Agents](https://github.com/langchain-ai/deepagents) (`deepagents-code`, and an HTTP service built on `deepagents`) and [DeepSeek Harness](https://github.com/deepseek-ai/dsh) (`dsh`). Each has a run spec per suite, the same file in every mode:

| Suite | What it grades | Needs |
|---|---|---|
| `small-repo-fixes` | 11 SWE-bench-style tasks, graded by hidden tests | nothing: the mock model solves them |
| `deep-research` | a research question over a small corpus, graded for citations and a judge's score | a real model and a judge |
| `terminal-bench` | Terminal-Bench 2 tasks, installing the agent into each task's image | a real model, Docker images |

| Spec | Agent |
|---|---|
| `dsh-<suite>.yaml` | `dsh --profile headless --json`, its run events become the trajectory (`output_format: dsh-json`) |
| `deepagents-<suite>.yaml` | `dcode` headless (small-repo-fixes, terminal-bench) or the HTTP service (deep-research); traces come from MLflow |

The model is reached by name (`evalsi-demo-model:8000`), so nothing in a spec depends on the mode. By default it is a **mock** (`mock-model/`): a deterministic script that solves the small-repo-fix tasks and answers the research question, so the pipeline runs with no key and no cost. Point `agents.model` (Kubernetes) or the `DEMO_*` variables (standalone) at a real model for the other suites.

```text
examples/demo/
  *.yaml            run specs (one per agent and suite)
  fixtures/         datasets and the scripts that generate and verify them
  mock-model/       the deterministic model
  deepagents/       image, HTTP service, dcode wrapper, recorder for the trace fixture
  dsh/              image and configure.sh
  Chart.yaml ...    the Kubernetes demo chart, with overlays/ for each cluster shape
```

## Standalone

On one machine with Python 3.12 or newer, `bubblewrap` (agent runs refuse Landlock), Node 22 and the agents:

```bash
npm i -g @deepseek-ai/dsh@0.2.1-alpha.2
pip install -r examples/demo/deepagents/requirements.txt
sudo install -m755 examples/demo/dsh/configure.sh /usr/local/bin/dsh-demo-configure
sudo install -m755 examples/demo/deepagents/dcode-demo.sh /usr/local/bin/dcode-demo
```

The specs name an image (`environment.image`) for the pod rung; on the host, run them from a copy without it so the sandbox uses the tools installed above.

1. Name the services the specs use, all on this machine:

   ```bash
   echo '127.0.0.1 evalsi-demo-model evalsi-demo-deepagents evalsi-demo-mlflow' | sudo tee -a /etc/hosts
   python3 mock-model/server.py --port 8000 &
   export DEMO_API_KEY=anything      # the mock ignores it
   ```

2. Embedded, no server (results in the terminal; `--output` keeps them):

   ```bash
   cd examples/demo
   evalsi run -f dsh-small-repo-fixes.yaml --output dsh.json
   evalsi run -f deepagents-small-repo-fixes.yaml --output deepagents.json
   evalsi report dsh.json -o dsh-report.html
   ```

3. On a server, which keeps the runs, the trajectories and the web UI:

   ```bash
   evalsi serve --config evalsi.yaml &          # http://127.0.0.1:8081
   evalsi run -f dsh-small-repo-fixes.yaml --server http://127.0.0.1:8081
   evalsi report <run id> --server http://127.0.0.1:8081 -o report.html
   ```

   Open `http://127.0.0.1:8081/ui/` for runs, scores and each trial's trajectory (model calls, tool calls, tokens).

Notes:

- dsh is a Node program that reserves a large address range, so its spec gives the sandbox 4 GiB and `NODE_OPTIONS=--disable-wasm-trap-handler`. A smaller limit makes it abort.
- `dcode` makes one lookup of pypi.org that the sandbox refuses; it is harmless.
- The research suites need a judge: set `judges` and `default_judge` in `evalsi.yaml` (there is no default; nothing is billed by surprise).
- Terminal-Bench 2: `git clone https://github.com/laude-institute/terminal-bench-2 terminal-bench-2` under this directory (it is git-ignored); the specs read `harbor://` from there and install the agent inside each task's image.

## On Kubernetes

The charts, a sandbox pool, an MLflow server, the Deep Agents service and `dsh web` (the studio's UI stand-in), and `kubectl apply` of the same specs. `deploy/e2e/kind-e2e.sh` does exactly this on kind and CI runs it.

```bash
kubectl create namespace evalsi
# The workers read the model's key from this Secret; the mock ignores the value.
kubectl create secret generic evalsi-demo-model-key -n evalsi --from-literal=key=mock

helm install evalsi-crds deploy/helm/evalsi-crds --set operator.namespace=evalsi
helm install evalsi deploy/helm/evalsi -n evalsi \
  -f examples/demo/overlays/evalsi-demo.yaml -f examples/demo/overlays/pod-pool.yaml \
  -f examples/demo/overlays/kind.yaml          # or tainted-nodes.yaml, istio-ambient.yaml
helm install evalsi-demo examples/demo -n evalsi -f examples/demo/overlays/kind.yaml

kubectl apply -n evalsi -f examples/demo/dsh-small-repo-fixes.yaml -f examples/demo/deepagents-small-repo-fixes.yaml
kubectl get evalruns -n evalsi -w
```

What each part does:

- `overlays/evalsi-demo.yaml`: a project `demo`; a credential grant that lets its runs name `DEMO_API_KEY` (the key lives only in the Secret above, never in a pod spec or run spec); S3 (MinIO) for datasets; an MLflow sink. `overlays/pod-pool.yaml`: the sandbox pool on the pod rung, which works with a namespace-only install. Skip it if you run `evalsi-sandboxd` or a `SandboxClass`.
- The demo chart: the mock model, the Deep Agents service (traces to MLflow), `dsh web`, an MLflow server, and a Job that runs `evalsid datasets put` to upload `fixtures/*.jsonl` to S3 where the specs look. Its images are `ghcr.io/abhishek-rnjn/evalsi-demo-{dsh,deepagents}`; on kind, build them (`docker build deepagents`, `docker build dsh`) and `kind load docker-image`.
- Scores: `kubectl port-forward svc/evalsi 8080:8080` and open `/ui/`; `kubectl port-forward svc/evalsi-demo-mlflow 5000:5000` shows the run in the `evalsi-demo` experiment.
- A real model: `--set agents.model.baseURL=https://... --set agents.model.name=... --set agents.model.apiKey=...` on the demo chart, and put the key in `evalsi-demo-model-key`; add the host to each spec's `allow_hosts`.

### Any cluster

Every pod and Service the demo chart adds takes `tolerations`, `nodeSelector`, `podLabels`, `serviceLabels` and `imageRegistry` as values (each component can set its own), and the MLflow volume takes `mlflow.persistence.storageClassName`. The overlays show three shapes:

| Overlay | For |
|---|---|
| `kind.yaml` | one node, no NetworkPolicy enforcement, default storage class |
| `tainted-nodes.yaml` | dedicated tainted nodes and a mirror registry for every image (edit the values at its top) |
| `istio-ambient.yaml` | Istio ambient: Services opt out of the waypoint, the operator's webhook port is permissive, sandbox pods carry the opt-out label |
