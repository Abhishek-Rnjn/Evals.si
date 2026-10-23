# Demo storyboard and narration

The film is an edited walkthrough of actual captured CLI output and a screenshot of the actual generated HTML report. Timing is editorial, not a performance measurement. Voice: FFmpeg/libflite synthetic narration. The fixtures are synthetic recorded outputs, with no live agent or model calls.

## 00:00:00.000 — Catch a regression. Keep the evidence.
Visual: intro. Duration: 15.60 seconds.

Here is a small, reproducible Evals dot S I workflow. We will score two recorded answers, catch a deliberately introduced regression, and generate a report. These are synthetic examples, with no live agent or model calls.

## 00:00:15.600 — Start with the published release
Visual: install. Duration: 17.32 seconds.

Install the published SDK and harness, both pinned to version zero point three point one. The official recorded starter supplies a passing fixture and a negative control. After installation, these evaluations run offline, without a server or sandbox.

## 00:00:32.920 — One changed answer. The same expectation.
Visual: fixtures. Duration: 17.48 seconds.

Each case has a stable identifier, an input, a recorded output, and a reference. The baseline answers both questions correctly. The negative control changes Japan's answer from Tokyo to Kyoto. We have prewritten these outputs; no agent is generating them.

## 00:00:50.400 — The passing control exits zero
Visual: baseline. Duration: 13.72 seconds.

Run the baseline specification and save its result. Both records match, the mean is one, and the evaluator reports no errors or skips. The mean gate and the zero error rate gate pass. The command exits zero.

## 00:01:04.120 — The negative control exits three
Visual: regressed. Duration: 15.60 seconds.

Now evaluate the deliberately regressed fixture. Only one of two records matches. The mean falls to zero point five, so the required mean of one fails. The command exits three: a gate failure, with no evaluator errors.

## 00:01:19.720 — Show the case behind the score
Visual: report. Duration: 19.64 seconds.

Generate an HTML report from the saved result. It records the evaluator, dataset identity, scores, intervals, and gate outcome. The failed Japan example shows the actual recorded output, Kyoto, beside the reference, Tokyo. Keep the raw JSON alongside the report.

## 00:01:39.360 — Let the gate control the CI step
Visual: ci. Duration: 16.92 seconds.

The tutorial includes a GitHub Actions recipe. Run the evaluation directly, so a nonzero exit fails the step. Generate and upload reports even after a failed gate. The equivalent commands were verified locally; this video does not claim an Actions run.

## 00:01:56.280 — Two fixtures demonstrate mechanics
Visual: limits. Duration: 19.08 seconds.

Two synthetic records are a demonstration, not evidence that an application is safe. The confidence intervals are wide. Exact match is not suitable for every task. Replace the fixtures with representative application outputs and independently reviewed expectations before relying on the check.

## 00:02:15.360 — Try both controls on your own machine
Visual: next. Duration: 17.36 seconds.

The video, tutorials, report, raw outputs, and reproduction scripts are published together in the launch demo directory. Start with the local tutorial, then try the CI recipe. We are looking for Python teams who want to evaluate their own examples.
