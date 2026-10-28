# Lesson 1: install both packages and prove readiness

By the end, you will have an isolated installation, a saved diagnostic, and a
clear distinction between local scoring and sandboxed agent execution.
Run commands from the repository root unless a step explicitly changes directory.

## 1. Establish that the machine works

```bash
pwd
python3 --version
git --version
curl -I --max-time 20 https://pypi.org
```

You need Python 3.11 or later. A successful shell command establishes command
execution; a successful HTTPS request establishes reachability to that destination.
Neither proves a model provider, remote cluster or sandbox works. Preserve any
proxy and CA settings supplied by your cloud environment.

Clone the exercise files:

```bash
git clone https://github.com/Abhishek-Rnjn/Evals.si.git
cd Evals.si
```

For an unpublished tutorial PR, check out its branch before looking for these
files. Once merged, the default branch includes them.

## 2. Install the published packages into a fresh environment

```bash
python3 -m venv .venv-onboarding
. .venv-onboarding/bin/activate
python -m pip install 'evalsi[server]==0.3.1' 'evalsi-harness==0.3.1'
python -m pip check
python -c 'from importlib.metadata import version; print(version("evalsi"), version("evalsi-harness"))'
evalsi doctor --format json > doctor.json
cat doctor.json
```

Expected package versions: `0.3.1 0.3.1`. Pinning makes this course reproducible;
upgrade the two packages together when you choose a newer release. The `server`
extra installs Python gRPC worker dependencies; it does not itself guarantee a
usable sandbox. On Windows activate with `.venv-onboarding\Scripts\Activate.ps1`.

Plain `doctor` checks SDK readiness. Optional capabilities may warn while SDK
scoring remains ready. Ask for the capability you actually need:

```bash
evalsi doctor --require agent --format json
```

This stricter command requires a matching harness and server, worker dependencies
and a usable sandbox at `namespaced` isolation or stronger. The mock-tool course
can proceed even when this stricter probe fails. The CLI sandbox starter cannot.
Do not substitute the test helper `LocalSandboxClient`; it provides no isolation.

## 3. Confirm the SDK before introducing a model

```bash
evalsi eval --data examples/quickstart/qa.jsonl \
  --evaluators exact-match,numeric-match,latency --output first-scores.json
```

Expect six records, exact-match mean about 0.333 and numeric-match mean about
0.667 over three scored records, with three numeric skips. This works without a
model key and is a smaller diagnostic than an end-to-end agent run.

## 4. Exercise the actual sandbox, when doctor says it is ready

Create the starter in a new path; `init` refuses an existing directory:

```bash
evalsi init /tmp/my-first-evalsi-agent --template agent
evalsi run -f /tmp/my-first-evalsi-agent/run.yaml --output starter-good.json
# Expected: exit 0, task-success = 1.
evalsi run -f /tmp/my-first-evalsi-agent/regressed.yaml --output starter-bad.json
# Expected: exit 3, task-success = 0.
```

The stand-in CLI agent writes a file, and a checker reads the final file. This is
real sandboxed execution with synthetic code. The negative command intentionally
returns nonzero; run it separately if your shell uses `set -e`.

## 5. Use a separate environment for development features

The SQL lesson requires the database tool environment, added in 0.3.2 after the
0.3.1 release this course pins. Use 0.3.2 or later, or a source checkout as
below. Do not mix the published wheel and editable source in one environment:

```bash
deactivate
cd python
uv sync --locked --all-packages
uv run python -c 'from importlib.metadata import version; print(version("evalsi"), version("evalsi-harness"))'
cd ..
```

The checkout used for this study reports `0.3.1.dev0` for both packages, despite
containing changes newer than release 0.3.1. Version strings alone do not identify
source contents: record `git rev-parse HEAD` too. Use `python/.venv/bin/activate`
for subsequent source commands. Source-only database runs need no server build.
For sandbox or server work from source, build a matching binary using the
[guided setup instructions](../guides/onboarding.md#diagnose-agent-execution).

## Troubleshooting checkpoint

| Symptom | Next check |
|---|---|
| `evalsi` not found | Activate the environment; inspect `command -v evalsi` |
| SDK and harness versions differ | Reinstall a matching pair in a fresh environment |
| Scoring works, agent doctor fails | Read the specific server, worker or sandbox check |
| Source install has no evalsid | Build the matching binary and set `EVALSID` |
| macOS/Windows cannot run sandbox starter | Use a supported Linux host with the required sandbox facilities |
| Installation cannot reach a package host | Check the environment's supported network policy and proxy settings |

Continue to [recorded-output scoring](02-score.md).
