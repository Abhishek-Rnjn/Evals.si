# Validate an evaluator against human judgements

**Unreleased.** `evalsi calibrate` measures whether an evaluator agrees with human
pass/fail judgements on fixed recorded outputs. Compare evaluator versions,
judge models, prompts and thresholds before using their scores as evidence about
an AI system. No target model or agent is run during calibration.

The workflow uses the existing [human annotation queues](annotation.md). It adds
immutable label snapshots, tuning-only threshold and candidate selection,
held-out validity, fresh repeated judge calls and portable reports. It is
independent of the Harness² selection-specific `evalsi harness2 calibrate`.

## Freeze the human-labelled cases

Supply JSONL with explicit splits and individual ratings:

```jsonl
{"record":{"id":"t1","input":"Capital of France?","output":"Paris","reference":"Paris"},"split":"tuning","ratings":[{"annotator":"alice","passed":true}]}
{"record":{"id":"h1","input":"Capital of Italy?","output":"Paris","reference":"Rome"},"split":"held-out","ratings":[{"annotator":"alice","passed":false},{"annotator":"bob","passed":false}],"slices":{"language":"en"}}
```

```bash
evalsi calibrate freeze --data labels.jsonl --name correctness --version labels-v1 \
  --question 'Is the response correct, allowing equivalent wording?' --out dataset.json
```

Keep the printed SHA-256 as the reviewed pin. Labels use a strict majority of
non-null `passed` votes. Ties and absent votes remain unlabelled, not failures.
The artifact retains all ratings, comments, source identities and abstentions.
Task references remain task references: human labels are never injected into
evaluator records. Review the records' original metadata for accidental label
leakage before freezing them.

Each record needs a unique explicit ID. Optional `group` identifies an independent
task, conversation, document or other sampling unit; it defaults to the record ID.
Keep dependent examples in the same group. A group cannot span splits, and
identical canonical full inputs must share a group and split. Cases without an
input rely on your declared groups. These checks do not detect paraphrases or
prove semantic independence. Optional `slices` maps names to string values.
Split assignment is explicit; this command does not silently repartition data.

To import queue answers, export them using the current authenticated client:

```bash
evalsi annotate export correctness --project demo --server http://localhost:8080 \
  --out annotations.jsonl
evalsi calibrate freeze --annotations annotations.jsonl --assignments splits.json \
  --name correctness --version labels-v1 --question correct \
  --question-text 'Is the response correct? Use the reference; accept equivalent wording.' \
  --out dataset.json
```

`splits.json` maps every exported **item ID** to an explicit assignment:

```json
{
  "item-id-one": {"split": "tuning", "group": "task-one", "slices": {"language": "en"}},
  "item-id-two": {"split": "held-out", "group": "task-two", "slices": {"language": "en"}}
}
```

`--question` selects a pass/fail question by name; `--question-text` pins its
wording and instructions. The importer reads protobuf JSON, including messages,
trajectory and usage. It ignores stored machine scores as labels. Skipped or
missing answers remain abstentions. Duplicate raters, conflicting item revisions,
unknown assignments and duplicate record IDs fail. Export separate task records
or explicitly prepare distinct record IDs for multiple trials of one source task,
keeping them in one group. Queue answers can be replaced on the server; an export
freezes the observed answers, not a server-side label revision or authorship proof.

Changing a rating, question, record, split or version changes the set's pin.
Publish a new file and label version for corrections. Existing files and symlinks
are never overwritten; files are private by default. JSON inputs and artifacts
are capped at 64 MiB and reject duplicate keys and non-finite values.

## Declare and run the calibration protocol

Create `protocol.json`, substituting the reviewed dataset digest and your explicit
judge model and endpoint:

```json
{
  "schema": "evalsi.calibration-protocol/v1",
  "dataset_sha256": "REPLACE_WITH_REVIEWED_DATASET_SHA256",
  "repeats": 3,
  "concurrency": 8,
  "confidence": 0.95,
  "resamples": 2000,
  "candidates": [
    {
      "name": "current",
      "evaluator": {"ref": "builtin/llm-judge@1.0.0", "params": {"rubric": "correctness"}},
      "metric": "llm-judge",
      "thresholds": [0.5, 0.75, 1.0],
      "pass_when": "gte",
      "judge": {
        "provider": "openai-compatible",
        "model": "your-reviewed-judge-model",
        "base_url": "http://localhost:8000/v1",
        "temperature": 0.5,
        "api_key_env": "CALIBRATION_JUDGE_KEY"
      }
    },
    {
      "name": "alternative-prompt",
      "evaluator": {"ref": "builtin/llm-judge@1.0.0", "params": {"rubric": "Check factual correctness against the reference. Accept paraphrases; reject wrong numbers."}},
      "metric": "llm-judge",
      "thresholds": [0.5, 0.75, 1.0],
      "pass_when": "gte",
      "judge": {
        "provider": "openai-compatible",
        "model": "your-reviewed-judge-model",
        "base_url": "http://localhost:8000/v1",
        "temperature": 0.5,
        "api_key_env": "CALIBRATION_JUDGE_KEY"
      }
    }
  ]
}
```

Use `evalsi catalog --format json` to check installed evaluator versions. Every
candidate names one declared numeric or pass/fail **output score name**; dataset
scope, categorical and structured metrics are rejected. Lower scores can indicate
a pass with `"pass_when": "lte"`. One threshold fixes it in advance. Multiple
thresholds select highest expected accuracy on labelled tuning cases, requiring
both human classes and complete tuning scores. Declared threshold order breaks
ties. Candidate selection also uses tuning only and declared-order ties. The
selection is fixed before any held-out scoring. The tool does not choose another
candidate after seeing held-out results.

```bash
evalsi calibrate run --dataset dataset.json -f protocol.json --out report.json
evalsi calibrate report report.json --out report.html
evalsi calibrate verify report.json --sha256 THE_PRINTED_REPORT_SHA256
```

Judge-backed candidates require explicit configs, with no environment fallback
for model selection. Keys are read from the named environment variable and are
not stored; credential-bearing URLs are rejected. All splits, evaluator bindings,
output names and candidate configs are checked before calls begin. Calibration
always disables Evals.si's judge cache, including for repeated identical prompts.
Real endpoints incur fresh calls and cost. Provider-side caching, retries and
model weight changes are outside the tool's control. Both JSON and next-token
logprob calls are audited with their method, phase/repeat, prompt/system/response
hashes and returned model identities; JSON calls also pin the schema, while
logprob calls retain their one-token request settings and top-logprob limit.
Failed requests retain their error type without inventing response evidence.
Fatal judge configuration errors abort; ordinary per-record failures remain in
the report. CLI exit 3 means incomplete measurements, with the report saved;
exit 1 means configuration, integrity or I/O failure. A low agreement score alone
does not produce an exit failure or a release decision.

## Read validity and uncertainty

For each case, repeats estimate the probability of a judge pass. Accuracy,
precision, recall, FPR, FNR, expected confusion counts and Cohen's kappa compare
that probability with the majority human label. They estimate **single-call**
validity, not the accuracy of a majority-vote judge ensemble. Expected confusion
counts can be fractional. Each case has equal weight; repeats add precision
about its behaviour, not new independent examples.

A case contributes only if every requested repeat produced the selected finite
score. Unresolved human labels, errors, missing scores and skipped records are
explicitly counted. Failure is never converted to zero. Reports retain every
score, explanation, per-case numeric range/standard deviation and pairwise
decision disagreement between repeated calls. Slice tables show class
denominators, case counts and independent group counts. Paired candidate
comparisons use the shared complete cases and report unpaired counts.

Intervals use a deterministic percentile bootstrap of whole independent groups,
preserving dependencies and all repeats. The confidence level, resample count,
seed and method are pinned. Fewer than two groups, undefined denominators,
more than 10% undefined bootstrap samples, or no observed bootstrap variation
leave the interval unavailable with a reason. Perfect agreement on a tiny sample
does not demonstrate zero uncertainty. Bootstrap intervals are approximate;
larger representative held-out samples and enough positive/negative cases are
still necessary. Slice analyses and multiple comparisons are exploratory and
have no multiplicity correction. Human majority labels can be wrong; keep the
disagreements and use the queue's existing inter-rater summaries to investigate.

JSON reports include the exact frozen records/ratings, resolved evaluator refs
and defaults, evaluator source-file and Evals.si source-tree hashes, protocol,
judge settings, timestamps and Python/Evals.si versions. The HTML has no scripts
or external resources and escapes record content and annotations. Both contain
private data; share them deliberately. Digests detect content changes and require
a separately trusted pin to identify an approved artifact. They are not signatures,
model-weight attestations or a complete dependency lockfile. Preserve the
environment lockfile and any third-party evaluator dependencies for reproduction.

## Separate evaluator drift from target improvement

```bash
evalsi calibrate compare before.json after.json --out judge-drift.json
```

Comparison requires the identical frozen set, including target outputs and human
labels, and matching candidate names. It reports changed evaluator definitions,
thresholds and paired held-out accuracy deltas. A changed record or label revision
is rejected because that would confound judge changes with target/data changes.
Evaluate target improvements separately using the frozen validated evaluator.
Repeatedly editing prompts after inspecting the same held-out set spends its
independence; collect another held-out revision before making a new quality claim.

The SDK exposes `freeze_set`, `write_artifact` and `read_artifact` in
`evalsi.calibration_sets`, and sync `calibrate`, async `acalibrate` and
`compare_reports` in `evalsi.calibration`. Installed custom evaluators use the
normal registry; SDK callers can pass a `Registry` explicitly. Notebook callers
can use the sync wrapper or await `acalibrate`.

Run the [offline example](../../examples/judge-calibration/README.md) without a
model or key. Current verification uses synthetic labels and a scripted HTTP
judge; it does **not** certify a deployed judge against real human judgements.
Continuous human scores, multiclass labels, pairwise preferences, learned judge
weights and a server/browser calibration workspace remain future work.
