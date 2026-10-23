# Catch a recorded-output regression with Evals.si

A short, reproducible introduction to the published **Evals.si 0.3.1** release.
The video uses actual captured CLI output and generated reports from this package.
Its narration is synthetic; captions and an editable narration script are included.

- [Watch or download the captioned MP4](assets/demo.mp4)
- [Companion report](REPORT.md), [HTML presentation](index.html)
- [Tutorial 1: local evaluation](TUTORIAL-LOCAL.md)
- [Tutorial 2: a GitHub Actions check](TUTORIAL-CI.md)
- [Storyboard and narration](video/SCRIPT.md), [captions](assets/demo.vtt)
- Actual generated reports: [baseline](results/baseline.md), [regressed](results/regressed.md)
- Raw evidence: [baseline JSON](results/baseline.json), [regressed JSON](results/regressed.json), [command transcript](results/transcript.json)

## Reproduce this package

Python 3.11+ and Bash are needed; the recorded execution used Python 3.12 on Linux
amd64. Installation needs PyPI access. Scoring these fixtures then needs no network,
model/API key, running server or sandbox.

```bash
cd examples/launch-demo
bash scripts/setup.sh
.venv/bin/python scripts/run_demo.py
```

The verification harness expects the baseline to exit **0** and the intentionally
regressed run to exit **3**. Its own exit 0 means both controls behaved correctly;
it is not the command to use as a production regression gate. The CI tutorial runs
`evalsi run` directly so a failed gate really fails the step.

These are two synthetic geography records, with prewritten outputs. No agent or
language model is executed, and no actual PR is evaluated in the video. A mean of
1.0 on two fixtures is not a production-quality or statistical safety guarantee.
Replace the data with your application's recorded outputs before using this as
meaningful protection. Review rubric quality, skips, coverage and representative
sample size as well as the score.

## Rebuild the media

The optional renderer needs Node, Chromium and FFmpeg built with the `flite` filter.
It reads the actual `results/transcript.json` and screenshots the actual HTML report.
The scenes are an edited walkthrough, not a real-time performance recording.

```bash
npm ci
CHROMIUM_PATH=/usr/bin/chromium node video/render.mjs
CHROMIUM_PATH=/usr/bin/chromium npm run verify
```

The renderer generates the MP4, WebVTT/SRT captions, storyboard and still images.
No external voice or video service is used. `index.html` and its relative assets
can be hosted together on a static site; they are not automatically deployed by
committing these files. GitHub displays the Markdown report directly and provides
the MP4 for download. Playback can depend on the GitHub client.

## Publication checklist

- Keep the video, captions, tutorials, report, raw evidence and exact package pins together.
- Verify links and both controls from the files actually published.
- Label the synthetic fixtures and synthetic narration in the video description.
- Distinguish a local reproduction from an actual GitHub Actions execution.
- Keep GitHub workflow permissions limited to reading contents; upload only the intended data.
- Before replacing these public fixtures with customer data, review report and artifact exposure.
- Record the final commit with any announcement; do not substitute newer main behavior for 0.3.1.

Suggested announcement:

> A short Evals.si walkthrough: score two recorded outputs, catch a deliberately
> introduced regression, and generate a report you can share. Includes pinned
> commands, passing/failing controls and a GitHub Actions recipe. The fixtures are
> synthetic, and the local scoring runs without model calls. Looking for Python
> teams who want to try this workflow on their own examples.
