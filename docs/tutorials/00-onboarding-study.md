# 0. Onboarding study: a first install of Evals.si and its harness

This is a record of installing Evals.si 0.3.1 and `evalsi-harness` 0.3.1 from PyPI
for the first time, as a new user would, and driving the built-in harness with a
fake tool-calling model until every major feature had been exercised. The
[tutorials](README.md) are the cleaned-up path; this page is what happened on the
way, including what did not work.

## Setup

| | |
|---|---|
| Date | 2026-10-24 |
| Machine | an ephemeral cloud Linux container, x86_64, kernel 6.18, no GPU |
| Python | 3.13 system, 3.12 in the virtual environment (via `uv venv -p 3.12`) |
| Network | outbound HTTPS only, through a proxy |
| Packages | `evalsi==0.3.1`, `evalsi-harness==0.3.1` from PyPI; later also the `main` source tree, to check the tutorials against it |
| Model | TinyLM, a fake OpenAI-compatible model written for this study ([`examples/tinylm`](../../examples/tinylm/)) |
| Prior knowledge | the README and the guides; no earlier use of the tool |

### Could the environment reach what it needed?

The first thing checked, before installing anything:

| Target | Result | Needed for |
|---|---|---|
| `pypi.org`, `files.pythonhosted.org` | 200 | installing the packages |
| `api.github.com` (releases) | 200 | checking the latest release |
| `proxy.golang.org` | 200 | building `evalsid` from source (optional) |
| `evalsi.dev` | no connection | reading the docs site; the same pages are in `docs/` |
| Docker | present | not needed: the sandbox is bubblewrap, shipped in the wheel |

Commands ran normally in the container, and nothing Evals.si does at run time needed
the network: the model ran on loopback and the sandbox denied the agent all egress.

## Timeline

Times are from the session's own timestamps, relative to the start.

| Time | Step | Outcome |
|---|---|---|
| 0:00 | environment check (above) | PyPI reachable |
| 0:01 | `uv venv`, `uv pip install evalsi evalsi-harness` | 13 packages in 1.8 s |
| 0:01 | `evalsi doctor` | all OK, including the bundled `evalsid` and `bwrap` |
| 0:02 | README's 30-second agent example | matched the README exactly; 0.3 s |
| 0:02 | `evalsi init` (recorded and agent templates) | exit 0 and 3 as documented; the agent starter ran in a real sandbox in 0.6 s |
| 0:02 to 0:04 | read the harness guides, wrote TinyLM | (time spent writing the model, not fighting the tool) |
| 0:04 | first built-in harness run on TinyLM | **passed first try**, 3 sandboxed tasks in 1.3 s |
| 0:05 | six failure personas | each produced a distinct, explainable signature |
| 0:06 | trials, determinism, reports | worked; trial planner advice misleading (OS-B3) |
| 0:06 | record, then replay with the model stopped | identical scores |
| 0:07 to 0:09 | matrix, profiles, approval gates, faults, injection | worked; found a gate bypass worth documenting (OS-D3) |
| 0:10 | server: `evalsi serve` | needed a config for dataset paths (OS-D4); `serve --help` broken (OS-B2) |
| 0:11 | `evalsi compare` | crashed (OS-B1); the UI compare worked |
| 0:11 | `evalsi promote`, re-run promoted regressions | worked |
| 0:12 | web UI screenshots | worked |
| later | tutorials written, every scenario re-run, `check.sh` against PyPI 0.3.1 and `main` | 18 of 18 as expected on both |

**Time to first passing agent gate, from an empty virtual environment, was under
five minutes, and most of that was writing the fake model.** Nothing needed a
compiler, Docker, Go, a cloud account or an API key.

## What worked well

- **Installation.** Two `pip install`s, 13 dependencies, under two seconds. The
  platform wheel's bundled `evalsid` and `bwrap` meant the sandbox and the server
  needed nothing else.
- **`evalsi doctor`.** It answered "can this machine run agents?" before anything
  failed, and `--require agent` turns the answer into an exit code for CI.
- **The README examples were literally true.** Copying the 30-second example
  produced the documented table to the last digit.
- **The built-in harness accepted a stand-in model with no special casing.** An
  OpenAI-compatible URL was enough, streaming included. Its stream reader handled
  token-by-token tool-call argument deltas, and its error handling turned
  malformed JSON arguments and unknown tool names into tool errors the model could
  read, as a provider would, instead of crashing the run.
- **Honest numbers.** An agent with no tool calls was *skipped* by `tool-errors`,
  not scored 0. A model outage made every result an *error* and the exit code 1, not
  3. Intervals were shown even when they were embarrassingly wide. Each of these is
  the difference between a gate you can trust and one you cannot.
- **The hidden checker.** Three personas (`lazy`, `partial`, `flaky`) reported
  success while failing; the end-state checker caught all of them, and nothing
  else in the metric set did.
- **Replay** reproduced a run with the model switched off, and refused, with a
  precise message, to replay under a changed toolset.
- **Seeded fault injection** gave the same faults on every rerun, so a comparison
  under faults is fair.
- **`promote`** turned a failed run into a re-runnable regression dataset in one
  command, environments and checkers included.
- **Error messages** were specific almost everywhere: the missing `datasets_dir`,
  the missing `bwrap` in a source install, the cassette mismatch.

## Findings

Each finding has a reproduction from the tutorial files. Severity is from a new
user's point of view. Bugs are numbered OS-B and documentation or design
surprises OS-D (onboarding study), so they do not clash with the PRD's
requirement IDs.

### Status

The fixes landed after 0.3.1, in the same pull request as this study (see
"Unreleased" in the [changelog](../../CHANGELOG.md)). The descriptions below are of
0.3.1, which is what a `pip install` gave on the day.

| Finding | Status |
|---|---|
| OS-B1 `compare` crash | **Fixed.** `Client.compare_runs` fills the zeros proto3 JSON leaves out, which also fixes the same gap in the MCP server's and the training loop's server comparisons |
| OS-B2 `serve --help` | **Fixed.** `-h` and `--help` go to `evalsid serve`, which prints its flags |
| OS-B3 trials advisory | **Fixed.** Summaries carry the evaluator's declared range; a bounded metric's variance bound is capped at `(max - min)^2 / 4`; the one-line advice considers only bounded metrics and prefers one that varied on a tie; `evalsi determinism` marks each metric `bounded` |
| OS-B4 broken pipe | **Fixed.** A closed stdout ends the command quietly with exit 141, as a shell does |
| OS-B5 matrix footnote | **Fixed.** Printed only when a row has a star |
| OS-D1 direction | **Fixed for embedded runs:** the table says `(lower is better)` beside such metrics. A server run's summaries do not carry the direction (it is not in the API), so its table cannot say yet |
| OS-D2 `network: deny` | **Documented** in [agent runs](../guides/agent-runs.md#environments) |
| OS-D3 approval pattern | **Documented:** the guide's example is anchored (`*git push*`) and the blind spots of each evaluator are spelled out |
| OS-D4 `datasets_dir` | **Documented** in the README's server section |
| OS-D5 `matrix --gate` exit 1 | **Unchanged, on purpose.** `matrix` uses 0 pass, 1 gate failed, 2 error, which the Harness² guide documents and tests pin; changing it would break scripts that rely on it |
| OS-D6 UI "significant" | **Partly fixed.** The compare page no longer colours a difference pass or fail, and says which way it went; colouring by the metric's direction needs the direction in the API |
| OS-D7 run status | **Open.** Telling a gate failure from an infrastructure failure needs a new run status or field in the API |
| OS-D8 empty rows | **Fixed.** The table hides metrics no record produced, skipped or errored on; the JSON keeps them |
| OS-D9 fork log names | **Documented** with the trial in the name |
| OS-D10 `doctor` sandbox line | **Fixed.** Each backend is shown with its level, for example `bwrap (namespaced), landlock (confined)` |

### Bugs

**OS-B1. `evalsi compare` crashes on most comparisons (high).** The table expects
`diffCi.low` and `diffCi.high`, but the server's JSON omits fields whose value is
zero (proto3 JSON). Any metric with an unchanged mean has a `[0, 0]` interval, so
nearly every comparison crashes:

```text
$ evalsi compare --server http://localhost:8080 run-84c4b55b59df1702 run-d6eaa791961ac3bd
  File ".../evalsi/cli.py", line 1223, in _cmd_compare_rows
    f"[{ci['low']:.3f}, {ci['high']:.3f}]" if ci else "-",
KeyError: 'low'
```

The raw response for an unchanged metric is
`"diffCi": {"level": 0.95, "method": "paired-t"}`. Still present on `main`
(`python/evalsi/src/evalsi/cli.py`, `_cmd_compare_rows`). Proposed fix:
`ci.get('low', 0.0)` and `ci.get('high', 0.0)`; the web UI's compare page already
treats the missing fields as zero. Workaround: `--format json`
([tutorial 8](08-server-ui-and-regressions.md#compare-the-two-runs)).

**OS-B2. `evalsi serve --help` fails (medium).** It is the first thing many people type:

```text
$ evalsi serve --help
evalsi: error: unrecognized arguments: --help
```

`main()` deliberately does not forward `-h`/`--help` to `evalsid`, but the `serve`
subparser is built with `add_help=False`, so nothing handles them. Still present on
`main`. Proposed fix: forward them too (`evalsid serve -h` prints the real flags).
Workaround: `evalsi serve -- -h`.

**OS-B3. The trials advisory gives misleading recommendations (low to medium).** The
`trials:` line under a run with trials, and `evalsi determinism`, estimate trials
needed for a ±margin on each task's mean. Three problems, all on `flaky.yaml` and
`faults.yaml`:

- The margin is absolute, and it is applied to count metrics too:
  `agent-efficiency.agent-tokens would need about 3994242 per task for +/-0.1`
  (token counts are in the thousands, so ±0.1 is meaningless for them).
- With three trials every pass/fail metric gets the worst-case variance bound, so a
  metric that never varied (`run-completed`) ties with one that did
  (`task-success`), and the line names the constant one as the noisiest.
- For `tool-errors`, a metric declared on [0, 1], it reported a variance bound of
  2.282, more than the 0.25 any [0, 1] quantity can have, and recommended 877 trials.

Proposed fix: restrict the line to bounded metrics, cap bounds at the metric's
declared range, and prefer a varying metric when bounds tie.

**OS-B4. Piping `evalsi run` output into `head` prints a traceback (low).**
`evalsi run -f run.yaml | head -4` ends with `BrokenPipeError: [Errno 32] Broken pipe`
from `_print_gates`. Cosmetic, but it looks like a crash in CI logs.

**OS-B5. `evalsi matrix` prints its `*` footnote when no row has a star (cosmetic).**
Seen comparing the `standard` and `minimal` profiles, where the difference was 0.

### Surprises and documentation gaps

**OS-D1. Metric direction is invisible in the table (medium).** `tool-errors` is an
error rate (lower is better); most other metrics are higher-is-better. The README's
first example shows `tool-errors 0.500` with no hint of direction, and a perfect
agent shows `tool-errors 0.000`, which reads as a failing score. `evalsi matrix
--gate` does know the direction (`higher_is_better=False` in the evaluator's
manifest). Suggestion: an arrow or a "(lower is better)" marker in the CLI table
and the report.

**OS-D2. With `network: deny`, attempted egress leaves no trace (medium).** The guide
says "all traffic goes through a logging egress proxy, so refused hosts show up as
policy events". That holds for `network: allowlist`. Under `deny` the sandbox has no
network at all: the agent's `curl` fails at DNS (`Could not resolve host`) and
`policy-violations` records nothing. With `allowlist` and an unrelated allowed host,
the same command produced `egress denied: pypi.org:443`. Suggestion: say so in the
environment docs, and recommend `allowlist` with an empty or minimal list when you
want to *observe* attempts. ([Tutorial 3](03-built-in-harness.md#sneaky-a-gate-you-did-not-write-is-a-gate-that-passes).)

**OS-D3. The approval-gate example's pattern is easy to bypass, and the coverage
evaluator shares the blind spot (medium).** The guide's example rule is
`command: "git push*"`. An agent told no that retries with
`sh -c 'git push origin HEAD --force'` is not matched, and the push **runs**.
`denial-respected` catches it ("1 that ran"); `approval-coverage`, with the same
glob as its `sensitive` pattern, reports "0 sensitive call(s), all approved". The
guide does state that the gate is a pattern match and that `bash -c` can get past
it. Suggestion: write the example as `"*git push*"` (which blocked both attempts in
this study) and repeat the warning next to `approval-coverage`.
([Tutorial 6](06-faults-injection-and-approvals.md#the-pattern-that-lets-it-through).)

**OS-D4. A default server cannot run a spec with a dataset path.** `evalsi serve` with
no config answers `this server does not accept dataset paths; set datasets_dir in its
config, or send records inline`. The message is clear and the default is sensible;
the README's server quickstart could show the one-line config that every local trial
needs.

**OS-D5. Exit codes differ between `run` and `matrix --gate`.** A failed gate is exit 3
from `evalsi run` and `evalsi gate`, but exit 1 from `evalsi matrix --gate`, where 1
otherwise means "could not complete". Scripts that branch on 3 will misread it.

**OS-D6. "Significant" has no direction in the UI compare page.** Lower tokens and cost
in a candidate are shown with the same red ▼ "significant" as a real regression in
`task-success`.

**OS-D7. A gate failure and an infrastructure failure have the same run status.** Both
are `RUN_STATUS_FAILED` in the API and "failed" in the UI; only the gates column
tells them apart. The CLI keeps them apart (exit 3 against exit 1).

**OS-D8. The empty `task-success.task-score` row.** Every run with a pass/fail checker
prints `task-success.task-score  0  -  -`. New users will wonder what failed.
Suggestion: hide numeric sub-metrics that no record produced.

**OS-D9. Fork log names include the trial.** The guide says a fork writes
`<task>.<fork_as>.jsonl`; 0.3.1 wrote `calc.0.partial.jsonl` (`<task>.<trial>.<fork_as>.jsonl`).

**OS-D10. `doctor` lists different sandboxes depending on the flag.** Plain `doctor`
showed `sandbox: bwrap, landlock`; `doctor --require agent` showed `sandbox: bwrap`.
Both are right (Landlock alone is too weak for agents), but the change is unexplained.

## What a new user should know on day one

These are the things the tutorials teach because they were the ones that mattered:

1. Gate on the hidden checker. The agent's own report, and its own self-test, are not evidence.
2. Gate on `run-completed`, the error rate and, if you care, `policy-clean`. A gate you did not write passes.
3. With a real model, use `trials` and gate on `pass^k`.
4. Exit 3 means worse, exit 1 means broken. Skipped is not zero.
5. Record once, replay for free when you change the checker or the evaluators.
6. Approval rules are globs. Anchor them on both sides and take the capability away in the sandbox too.

## Reproducing this study

```bash
python3 -m venv .venv && . .venv/bin/activate
pip install 'evalsi==0.3.1' 'evalsi-harness==0.3.1'
evalsi doctor --require agent
bash examples/tinylm/check.sh
```

`check.sh` re-runs all 18 scenarios and checks their exit codes. The bugs above
reproduce with the commands shown next to each.
