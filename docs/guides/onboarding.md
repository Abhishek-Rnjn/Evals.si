# Get your first useful evaluation

For a full hands-on course with a fake tool-calling model, failure controls and replay, start with the [first-time onboarding tutorials](../tutorials/README.md).

Start with the **published Python SDK**. You do not need a source checkout,
Evals.si server, harness, Docker, Kubernetes, provider account or API key to
evaluate recorded answers.

```bash
python -m venv .venv
. .venv/bin/activate
python -m pip install evalsi
```

On Windows, activate with
`.venv\Scripts\Activate.ps1`.

Save these two lines as `cases.jsonl`, then run the commands below:

```jsonl
{"id":"correct","input":"Return window?","output":"30 days","reference":"30 days"}
{"id":"regression","input":"Return window?","output":"7 days","reference":"30 days"}
```

```bash
evalsi eval --data cases.jsonl --evaluators exact-match --output results.json
evalsi report results.json -o report.html
```

Open `report.html`. One answer matches and one does not: the mean is **0.5**.
Replace the rows with your application's recorded answers and approved expected
answers, then repeat the same command. Exact match checks literal answer equality;
it is not semantic correctness, tool-state verification or a release policy.
Two synthetic examples cannot establish application quality.

`evalsi eval` returning **0** means scoring completed, **not** that every check
passed. Inspect the report. Configuration/data failures are execution errors,
not evidence that an application passed. Results and HTML contain the supplied
inputs, outputs and explanations: keep them private unless approved for sharing.
Nothing here uploads them or calls a model.

## Run your own application through `init`

The application template, decision-first report additions and scoped doctor
behavior below were added in 0.3.2. Install 0.3.2 or later
(`python -m pip install 'evalsi>=0.3.2'`); 0.3.1 does not have the template.

With it installed in the same environment as your application:

```bash
evalsi init my-evals --template application
cd my-evals
python evaluate.py --limit 1 --output results-first.json
python evaluate.py --output results.json
evalsi report results.json -o report.html
```

The starter writes `application.py`, `evaluate.py`, `cases.jsonl`, a README and
`.gitignore`. All cases are synthetic and run offline. `init` only writes a
**new** directory, refuses existing directories/files/symlinks, and does not
import your application, contact providers or request credentials.

Prove that a real regression is caught:

```bash
python evaluate.py --callable application:regressed --output results-regressed.json
evalsi report results-regressed.json -o report-regressed.html
python evaluate.py --callable application:regressed --case japan --output results-japan.json
```

The first and last commands deliberately exit **3**. The report names the
affected case; `--case japan` reproduces just that case. In this generated runner,
exit **0** means all exact-match checks passed, **3** means a check failed,
**1** means an execution/configuration error, and **2** means invalid CLI usage.
These exits are distinct from the recorded-scoring CLI's completion status.

For recognized project-local cases, the report also provides validated JSON
argument arrays for reproducing failed checks and execution errors in the
original environment. These are hints, **not authenticated provenance**:
review the callable and cases before running anything copied from a report.
External/unsafe paths omit the hint rather than inventing a command. Use a new
`--output` path when reproducing so you do not overwrite the full result.

To evaluate your code:

1. Implement an importable synchronous function taking and returning a nonempty
   `str`. For example, expose your application as `my_app:answer`.
   Install your application's package into this same environment, or put a
   thin adapter inside the generated directory; `init` does not alter import
   paths or install your application.
2. Replace `cases.jsonl` with approved cases containing unique `id`, string
   `input` and string `reference` fields. All three must remain nonempty after
   stripping whitespace; comparison preserves the original whitespace exactly.
   Use `--cases PATH` for another file.
3. Try one case before spending time or provider budget on the full set:

```bash
python evaluate.py --callable my_app:answer --limit 1 --output first.json
python evaluate.py --callable my_app:answer --output candidate.json
evalsi report candidate.json -o candidate.html
```

You can persist the callable when scaffolding with
`evalsi init my-evals --template application --callable my_app:answer`.
For an existing HTTP service, wrap your approved protocol using the SDK's
existing `httpx` dependency, for example:

```python
import os

import httpx

def answer(prompt: str) -> str:
    response = httpx.post(
        os.environ["MY_APPLICATION_URL"], json={"input": prompt}, timeout=30
    )
    response.raise_for_status()
    return response.json()["output"]
```

This example assumes a JSON request `{"input":"..."}` and response
`{"output":"..."}`; adapt it to your API. There is no new automatic HTTP protocol
discovery or dedicated endpoint wizard. Do not put credentials in URLs or cases.
Your callable executes with **your normal Python process permissions**: this
route is not sandboxed and does not impose provider spending limits. Calling
your application can contact real services and incur its usual costs.

The report separates failed checks from errors, skips and incomplete evidence;
it does not invent a release threshold. Use meaningful, reviewed evaluators and
representative cases before graduating to release decisions or CI.

## Optional YAML execution and agent templates

The existing recorded and agent templates remain available in 0.3.1 and later.
They use the run engine; they are not prerequisites for the SDK flow above.

```bash
python -m pip install 'evalsi[server]>=0.3.1' 'evalsi-harness>=0.3.1'
evalsi init run-evals
cd run-evals
evalsi run -f run.yaml --output results.json
evalsi run -f regressed.yaml --output results-regressed.json
```

The working spec exits **0**, and the deliberately regressed spec exits **3**.
Exit **1** means an execution/configuration error. Both specs also gate the
evaluation error rate. Replace synthetic cases and thresholds before treating
either as a product release gate.

## Diagnose agent execution

```bash
evalsi doctor --require agent --format json
evalsi init agent-evals --template agent
cd agent-evals
evalsi run -f run.yaml --output results.json
evalsi run -f regressed.yaml --output results-regressed.json
```

The agent writes a file; a checker verifies its final state in a namespaced
Linux sandbox with networking denied. The negative control omits the file and
must exit 3. It makes no model calls. Replace the CLI and environment files with
your agent and controlled task setup; see [agent runs](agent-runs.md).

Agent execution requires matching SDK, harness and `evalsid` versions, worker
dependencies and a usable Linux sandbox. Source installs do not contain the
server binary: build a matching `evalsid` using the repository build instructions
and set `EVALSID` to its absolute path. For a development checkout, run this from
the repository root with the virtual environment active and Go installed:

```bash
CGO_ENABLED=0 go build \
  -ldflags "-X github.com/abhishek-rnjn/evals.si/internal/version.Version=$(python -c 'import evalsi; print(evalsi.__version__)')" \
  -o bin/evalsid ./cmd/evalsid
export EVALSID="$PWD/bin/evalsid"
```

On macOS/Windows use a supported Linux host
or container with sandbox prerequisites. A generic container alone does not grant
user namespaces or KVM. Doctor reports the actual local probe; it cannot certify
a remote cluster or guarantee that every task can run.

From 0.3.2, plain `doctor` checks **only SDK scoring**: it does not inspect
or launch a server, harness or sandbox. Use `evalsi doctor --all` to explicitly
inspect optional execution capabilities. 0.3.1 reported optional
capabilities by default; `doctor` and `init` do not exist in 0.3.0.
`--require server` requires a matching executable and worker
dependencies. `--require agent` also requires the matching harness and an available
sandbox driver at `namespaced` isolation or stronger. A Landlock-only (`confined`)
host cannot run the starter and fails agent readiness. Doctor passes
`EVALSI_SANDBOX_CONFIG` to the probe just as local agent execution does.
Enforcement warnings are shown with a generic remedy, keeping raw probe text
(which may contain private addresses) out of the report. A warning does not
change backend availability or establish full isolation.
Required failures exit 1. Probes have bounded timeouts, make no
provider API calls and do not print credentials or raw subprocess errors.

## What remains to be proved

Fresh-wheel automation runs these flows without a source checkout or optional
execution packages; this is an installation/behavior check, not a user study.
The [unfamiliar-developer study protocol](onboarding-usability.md) defines how
humans should measure finding a useful result on their own application,
assistance, elapsed time, comprehension and repeat use. The under-15-minute
target is **not an observed success rate**.

## Maintained GitHub Action

The root [action definition](../../action.yml) wraps the existing run engine and
works with **published 0.3.0 and later**; the `doctor` and `init` commands are not
required at runtime. Use an immutable commit containing the action, such as the
`v0.3.2` tag's commit, in place of `ACTION_COMMIT`. The v0.3.0 tag predates the action.

```yaml
name: Agent evaluation
on: [pull_request]
permissions:
  contents: read
jobs:
  gate:
    runs-on: ubuntu-latest
    timeout-minutes: 10
    steps:
      - uses: actions/checkout@v7
      - uses: Abhishek-Rnjn/Evals.si@ACTION_COMMIT
        with:
          spec: run.yaml
          working-directory: my-evals
          version: '0.3.2'
```

Check in the starter's `run.yaml` and dataset, then adapt them to your application.
The spec must declare explicit gates. The action installs matching `evalsi[server]` and `evalsi-harness`, runs the spec
once, appends a metrics summary and propagates exit 0/3/1. It does not retry or
select the best run. Inputs are passed as arguments, not interpolated into shell
commands. For live agents, provision sandbox prerequisites and credentials explicitly.
Use GitHub-hosted Linux runners or equivalently configured self-hosted runners;
GitHub Enterprise Server's separate artifact service and disconnected runners
are outside this action's supported scope.

The summary includes candidate commit, spec/dataset hashes, SDK version, scored
sample counts, means, intervals, errors, skips and gate decisions. It excludes raw
records, explanations and free-text errors; choose metric labels appropriate for
your repository's visibility. Raw results and process logs remain in a temporary
runner directory. Outputs are `exit-code` and `results` (the local JSON path).

Artifact upload is **off by default**. Set `artifact-name` to a unique name per job
to upload raw results and process logs with seven-day retention, even on failure.
These may contain private inputs, outputs and errors; apply your organization's
repository visibility and data-handling policy before enabling it. A JSON result
may be missing when execution fails. Artifact and installation failures fail the
action too. `install: 'false'` uses an existing compatible Python installation.

### Advisory baseline comparison

Set `baseline: baseline.json`, relative to `working-directory`, to compare with
previously approved results supplied by your own trusted checkout/artifact step.
Run both sides with the same package version. The action checks matching dataset
hashes, evaluator identities/parameters, judge and summary configuration, the
complete environment checker configuration (including hidden files), trial
count, SDK/API versions and complete record/trial coverage. Each run must cover
every declared dataset record, trial and configured record-scoped evaluator;
shared skips, errors, missing rows and duplicates are rejected. Numeric metrics
that are produced must cover every record/trial; wholly unproduced optional
metrics are excluded from that requirement. Dataset-scoped evaluators and
partially populated numeric metrics are outside this conservative baseline
comparison contract. Target/agent
changes are allowed. A mismatched, malformed or incomplete baseline fails the
action with exit 1. Dataset hashes include recorded outputs; editing them therefore
requires a new baseline. Live agent candidates can change while retaining the
same task dataset.

The table shows paired mean differences with 95% intervals; repeated trials are
clustered by record. Insufficient independent records produce an unavailable
interval, and tiny samples remain weak evidence. Comparisons are **advisory**:
only the gates in your spec enforce thresholds. Configure meaningful business,
error-rate, cost and latency gates explicitly; an improved average alone does not
establish release safety.

This is an initial slice of PROD-01/PROD-02/GTM-02 in the
[product roadmap](../PRODUCT-ROADMAP.md), not a claim of enterprise qualification,
signed evidence, immutable dataset governance or completed customer onboarding.
The [CI guide](gate-an-agent-in-ci.md) explains the underlying gate workflow.

For the next dataset-lifecycle slice, [create and pin a dataset snapshot](dataset-snapshots.md)
and review its item-level diff before accepting a new test-set version.
