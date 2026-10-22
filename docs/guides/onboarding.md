# Guided setup and the GitHub gate action

`evalsi doctor` and `evalsi init` were added in 0.3.1; PyPI 0.3.0 does not have them.

```bash
python -m venv .venv
. .venv/bin/activate
python -m pip install 'evalsi[server]>=0.3.1' 'evalsi-harness>=0.3.1'
evalsi doctor
evalsi init my-evals
cd my-evals
evalsi run -f run.yaml --output results.json
evalsi report results.json -o report.html
```

On Windows, activate with
`.venv\Scripts\Activate.ps1`. Recorded-output scoring works without a server,
sandbox, model or API key. `init` only writes a **new** directory and refuses
existing directories, files and symlinks. All starter cases are synthetic.

The working spec exits **0**. Run `evalsi run -f regressed.yaml` separately:
it must exit **3**, proving that the deliberately incorrect answer is caught.
Exit **1** means an execution/configuration error. Both specs also gate the
evaluation error rate. Replace the synthetic data and thresholds with approved
representative cases before using this as a product release gate.

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

Plain `doctor` requires only SDK scoring and reports missing optional capabilities
as warnings. `--require server` requires a matching executable and worker
dependencies. `--require agent` also requires the matching harness and an available
sandbox driver at `namespaced` isolation or stronger. A Landlock-only (`confined`)
host cannot run the starter and fails agent readiness. Doctor passes
`EVALSI_SANDBOX_CONFIG` to the probe just as local agent execution does.
Enforcement warnings are shown with a generic remedy, keeping raw probe text
(which may contain private addresses) out of the report. A warning does not
change backend availability or establish full isolation.
Required failures exit 1. Probes have bounded timeouts, make no
provider API calls and do not print credentials or raw subprocess errors.

## Maintained GitHub Action

The root [action definition](../../action.yml) wraps the existing run engine and
works with **published 0.3.0 and later**; the `doctor` and `init` commands are not
required at runtime. Use an immutable commit containing the action, such as the
`v0.3.1` tag's commit, in place of `ACTION_COMMIT`. The v0.3.0 tag predates the action.

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
          version: '0.3.1'
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
