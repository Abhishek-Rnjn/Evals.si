# Gate an agent in CI

An agent that passes yesterday's tests can fail today's tasks after a prompt edit, a model bump or a new tool. Unit tests rarely notice, because an agent's answer changes from run to run. This guide shows how to put an agent on a fixed set of tasks in every pull request and fail the build when it gets worse.

You will:

1. run an agent on two tasks in a sandbox and see the gate pass,
2. break the agent and see the gate fail with exit code 3,
3. swap in your own agent,
4. make the gate trustworthy when the agent is noisy,
5. run it in GitHub Actions.

Everything in steps 1 and 2 is offline and free: a small script plays the agent, so no API key is needed. The files are in [`examples/agent-gate`](../../examples/agent-gate/).

## What a gate is

A run spec names an agent, a set of tasks and how to grade them. For each task Evals.si builds a fresh sandbox, lets the agent work in it, and then a **checker** the agent never sees grades the end state. A **gate** turns the scores into a verdict: if a gate fails, `evalsi run` exits with code 3, and a CI step that exits non-zero fails the build.

Grading the end state (did the tests pass?) is more reliable than judging the agent's words, and it works for any agent that changes something: code, files or a database.

## 1. Run the gate

```bash
pip install evalsi evalsi-harness
```

You need Python 3.11+. The package carries the server binary and, on Linux, the sandbox, so nothing else is installed. Save the [spec](../../examples/agent-gate/run.yaml) and [tasks](../../examples/agent-gate/tasks.jsonl) from `examples/agent-gate`, then run:

```bash
evalsi run -f run.yaml
echo "exit code: $?"
```

```text
2 records

metric                   n  mean   95% CI          skipped  errors
task-success             2  1.000  [0.342, 1.000]        0       0
task-success.task-score  0  -      -                     0       0

gates:
  [pass] task-success mean min 1.0
exit code: 0
```

The spec has four parts that matter:

```yaml
spec:
  target:
    agent:
      cli:
        command: [python3, agent.py]     # your agent: any command
  dataset: {path: tasks.jsonl}           # one task per line
  environment:
    files: {calc.py: "..."}              # the task's starting state: two bugs
    checker:
      files: {test_hidden.py: "..."}     # written after the agent finishes
      command: [python3, test_hidden.py]
  evaluators:
    - ref: task-success                  # the checker's verdict
  gates:
    - {metric: task-success, min: 1.0}   # every task must pass
```

- **`target.agent.cli`** runs your agent inside the sandbox, in the task's working directory. The instruction from the task goes to the command on standard input, or in place of `{instruction}` in the command.
- **`environment`** is the starting state of every task: files, setup commands and sandbox limits. Here the network is denied.
- **`checker`** is the pass/fail test. Its files are written after the agent finishes, so the agent cannot read or edit the test it is graded on.
- **`gates`** set the bar. `min: 1.0` means every task has to pass.

## 2. Break the agent

Pretend a change made the agent stop fixing `mul`. This copy of the spec makes the stand-in agent write the old behavior back:

```bash
sed 's/return a \* b/return a + b/' run.yaml > regressed.yaml
evalsi run -f regressed.yaml
echo "exit code: $?"
```

```text
2 records

metric                   n  mean   95% CI          skipped  errors
task-success             2  0.000  [0.000, 0.658]        0       0
task-success.task-score  0  -      -                     0       0

gates:
  [FAIL] task-success mean min 1.0 (mean 0 is below 1)
exit code: 3
```

Exit code 3 means a gate failed. Exit code 1 means the run itself could not complete, for example a bad spec. Code 0 means pass. CI treats any non-zero code as a failure, so no extra glue is needed.

## 3. Use your own agent

Replace the stand-in with your agent. `target.agent` takes any of these:

| Your agent is | Spec | How it is driven |
|---|---|---|
| A command-line program | `cli: {command, install, env_from, allow_hosts}` | runs in the sandbox, in the task's directory |
| An HTTP service | `http: {url, body_template, output_path}` | one request per task |
| An A2A agent | `a2a: {url}` | JSON-RPC `message/send` |
| A tool on an MCP server | `mcp: {url, tool, argument}` | one call per message |
| An OpenAI Responses-compatible API | `responses: {base_url, model}` | `POST <base_url>/responses` |
| A model with the built-in agent | `target: {connector, model}` and `harness: {builtin: {...}}` | bash, read and write file tools in the sandbox |

For a command-line agent that calls a model:

```yaml
target:
  agent:
    cli:
      command: [my-agent, --task, "{instruction}"]
      install: ["pip install my-agent"]            # once per environment
      env_from: {ANTHROPIC_API_KEY: AGENT_MODEL_KEY}   # only this key reaches the agent
      allow_hosts: [api.anthropic.com:443]         # the only host it may reach
      output_format: dsh-json                      # optional: its steps become a trajectory
environment:
  setup_network: allow                             # for `install`, during setup only
```

The agent sees only the variables named in `env_from` and reaches only `allow_hosts`. Everything else stays denied, so a misbehaving agent cannot read your other secrets or call out to an arbitrary host. With a `dsh-json` format, the steps of the run (model calls and tool calls with token counts) are recorded, and the trajectory evaluators in the [agent runs guide](agent-runs.md) can score them.

Real tasks usually come from your repository: a checkout with a bug and a test that must pass. An `image` for the environment, setup commands and per-task overrides are described in [agent runs](agent-runs.md#environments). Benchmark datasets such as SWE-bench, τ-bench and Terminal-Bench import directly.

## 4. Make the gate trustworthy

A real agent is not deterministic. One lucky pass or one unlucky fail should not decide a pull request. Three settings help:

```yaml
spec:
  trials: 3                       # run every task three times
  gates:
    - {metric: task-success.pass^3, min: 0.8}
    - {metric: task-success, stat: GATE_STAT_ERROR_RATE, max: 0.05}
  harness:
    builtin:
      budget: {usd: 0.50, wall_clock: 5m}   # a runaway agent stops here
```

- **`trials`** runs each task several times. Evals.si reports `pass@k` (at least one of k trials passed) and `pass^k` (all k passed). A trial in which the agent failed to run counts as not passed.
- **`pass^3 >= 0.8`** means: three trials in a row must pass on at least 80% of the tasks. Use `pass^k` when you need reliability, and `pass@k` when one success is enough.
- **`GATE_STAT_ERROR_RATE`** is the fraction of evaluations that errored, for example because the agent crashed or its model call failed. A metric's mean leaves errored records out, so without this gate an agent that crashes on most tasks and passes the rest still has a high mean. `max: 0.05` fails the run when more than 5% of them error.
- **Budgets** cap cost and time, so a bad prompt cannot spend without limit.

Every mean in the report has a 95% confidence interval. With only a few tasks the interval is wide (above, 2 passes out of 2 is `[0.342, 1.000]`), which is the honest answer to "is it fixed?". Add tasks until the interval is narrow enough for your gate.

Keep the task set growing from real failures. When a run fails, [`evalsi promote`](agent-runs.md) adds the failed records to a dataset on a server, so next time they are part of the gate.

## 5. Run it in GitHub Actions

[`examples/agent-gate/github-actions.yml`](../../examples/agent-gate/github-actions.yml) is a complete workflow. The parts that matter:

```yaml
jobs:
  gate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-python@v5
        with: {python-version: '3.12'}
      - name: Sandbox prerequisites
        run: sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
      - run: pip install evalsi evalsi-harness
      - run: evalsi run -f evals/agent-gate.yaml --output results.json
      - name: Build the report
        if: always()
        run: evalsi report results.json -o report.md
      - run: cat report.md >> "$GITHUB_STEP_SUMMARY"
        if: always()
```

- **The `sysctl` line** is needed on Ubuntu 24.04 runners, which restrict the unprivileged user namespaces the sandbox uses. Without it the sandbox may not start, and Evals.si does not fall back to weaker isolation than the spec asks for.
- **A non-zero exit fails the job**, so the check on the pull request goes red when a gate fails.
- **`if: always()`** keeps the report when the gate fails, which is when you want it. It also lands in the job summary, so the scores are on the pull request's checks page.
- **A model key** for a real agent goes in a repository secret and is passed as an environment variable on the run step. The spec's `env_from` hands it only to the agent.

Make the check required in the repository's branch protection rules, and a pull request that makes the agent worse cannot merge.

If your datasets, judges and keys live on an Evals.si server, run the same spec there with `evalsi run -f ... --server $URL`, authenticated with GitHub's own token and no stored secret: see [`examples/ci`](../../examples/ci/github-actions.yml) and [identity](identity.md#5-ci-github-actions-without-stored-secrets). The [end-to-end guide](end-to-end.md) has recipes for both setups.

## Beyond the pull request

- **Gate a deploy, not just a PR:** `evalsi gate` runs a spec for one release against a candidate and exits 0, 3 or 1. See [integrating an agent harness](integrate-an-agent-harness.md#3-gate-deploys).
- **Watch production:** the same evaluators score live traces from LangGraph, CrewAI, the OpenAI Agents SDK and others, so a metric means the same thing in CI and in production. See [trace sources](trace-sources.md) and [agent frameworks](agent-frameworks.md).
- **Go deeper on grading:** tool-call accuracy, trajectory match, loop detection, policy violations and a judge's review of the diff are in [agent runs](agent-runs.md#grading).
