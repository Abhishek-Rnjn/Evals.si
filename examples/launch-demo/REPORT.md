# A recorded-output regression: Evals.si 0.3.1

This report accompanies the [demo video](assets/demo.mp4) and the
[local](TUTORIAL-LOCAL.md) / [CI](TUTORIAL-CI.md) tutorials. It describes a bounded,
reproducible demonstration, using the published release rather than a source install.

## Question

Can a known wrong recorded answer cause a gate to fail, while retaining enough
evidence to identify the failed case and share the result?

## Setup

- Published `evalsi==0.3.1` and `evalsi-harness==0.3.1` in a fresh venv.
- Python 3.12 on Linux amd64; [exact installed dependencies](results/environment.txt).
- Two synthetic geography questions, one trial per case.
- Built-in `exact-match` with the captured default normalization settings.
- Mean threshold ≥ 1.0 and evaluator error rate ≤ 0.0.
- No generation target, configured judge, model call, server or sandbox.

The baseline and negative control keep the same IDs, questions and references.
Only Japan's recorded output changes, from `Tokyo` to `Kyoto`. The two dataset
digests therefore differ, as expected: the files include their recorded outputs.
This is a simple fixed-suite gate demonstration, not the separate paired
release-policy workflow.

## Observed results

| Measurement | Baseline | Deliberately regressed control |
| --- | ---: | ---: |
| Scored records | 2 | 2 |
| Exact-match mean | 1.000 | 0.500 |
| 95% Wilson interval | [0.342, 1.000] | [0.095, 0.905] |
| Evaluator errors | 0 | 0 |
| Skipped records | 0 | 0 |
| Mean gate | Pass | Fail |
| Error-rate gate | Pass | Pass |
| CLI exit | 0 | 3 |
| HTML and Markdown report generation | Exit 0 | Exit 0 |

The failed example is **japan**: input `Capital of Japan?`, output `Kyoto`,
reference `Tokyo`. The report surfaces that case under the lowest-scoring examples.
The negative result is intentional and was not edited into a passing run.

Evidence:
- [Baseline JSON](results/baseline.json) and [Markdown report](results/baseline.md).
- [Negative-control JSON](results/regressed.json) and [Markdown report](results/regressed.md).
- [Actual command output and exit codes](results/transcript.json).
- [Independent official-starter walkthrough check](results/tutorial-validation.json).
- Downloadable HTML: [baseline](results/baseline.html), [negative control](results/regressed.html).

## What was verified

1. A fresh installation of the two pinned published packages.
2. Both demo controls, including exact record counts, score means, zero errors/skips,
   expected case IDs, package version and gate outcomes from the actual result JSON.
3. HTML and Markdown generation even after the intentional gate failure.
4. The local tutorial's official `evalsi init` path, independently run in a fresh directory.
5. Local execution of both fixture modes and report generation used by the CI recipe.
   Hosted GitHub Actions execution is not claimed; record its run URLs when performed.
6. Video technical properties, captions and static-page links, as recorded in
   [publication validation](results/publication-validation.json).

The [reproduction script](scripts/run_demo.py) returns 0 only after both expected
controls and semantic checks pass. A production CI gate should run `evalsi run`
directly; the template does so and preserves its nonzero exit status.

## Interpretation and limits

The observed behavior establishes that this configuration catches this known
recorded-output regression and preserves inspectable artifacts. It does not
establish agent quality, judge quality, production safety, generalization or a
statistically significant difference between real applications. Two cases produce
wide, overlapping intervals. Nonsignificance is not equivalence, and this threshold
gate does not perform a formal paired comparison.

No live agent, model provider, actual customer incident, deployment, cloud service,
sandbox isolation or real PR was evaluated. Exact-match is a demonstration metric;
many useful answers paraphrase their reference, and business actions need state/tool
checks. A mean and error-rate gate alone do not prove complete required coverage.
Refresh outputs from the real application, review skips, dataset suitability,
critical cases and evaluator validity before relying on a release check.

Artifacts here contain only synthetic public examples. Reports in a real project
can disclose prompts, outputs and references; choose access and retention controls
before uploading them to CI artifacts or a public site.

## Video production and publication

The film is an edited walkthrough using the actual CLI transcript and an actual
generated HTML-report screenshot, with explanatory scenes. Its duration and scene
holds are editorial, not execution-time measurements. Narration uses the local
FFmpeg/libflite `slt` synthetic voice. Full narration is visible in the video;
English captions are also supplied as WebVTT, SRT and an MP4 subtitle track.
No external voice service, paid model or stock footage was used.

The [storyboard](video/SCRIPT.md), [scene source](video/scenes.json),
[renderer](video/render.mjs) and [production manifest](assets/production.json)
are included. The manifest binds the film to the captured results using SHA-256.
Hashes detect changes relative to those values; they are not execution attestations.

The branch publishes this report, tutorials, video, captions and raw evidence
together. [index.html](index.html) is ready for static hosting with its relative
assets. A branch push does not deploy a website or upload a YouTube video.

## Reproduce

```bash
cd examples/launch-demo
bash scripts/setup.sh
.venv/bin/python scripts/run_demo.py
```

For the no-source-install quickstart, use [tutorial 1](TUTORIAL-LOCAL.md).
Reproducing a run updates generated timestamps and durations; its expected scores,
record IDs and exit codes remain the same. Rebuild the film afterwards if the
published evidence changes. Do not present stale production hashes as matching
newly regenerated results.
