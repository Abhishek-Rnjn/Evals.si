# Human annotation

Human annotation answers two questions that automated metrics can't answer
on their own. First, how good are these outputs according to the people
whose judgment counts? Second, does our judge agree with them? A queue
holds records and a rubric. Annotators work through the records, and
evalsid summarizes their answers like any other metric, with confidence
intervals. It also compares those answers with the evaluator scores that
the source run gave the same records.

## Set up a queue

A queue lives in a project. Its rubric is a list of questions, each of one
of these kinds:

| Kind | Answer | Summarized as |
|---|---|---|
| `pass_fail` | yes or no | a proportion, with a Wilson interval |
| `score` | a number between `min` and `max` | a mean, with a t interval |
| `label` | one of `options` | counts per option |
| `text` | free text | not summarized |

```bash
evalsi annotate create -f examples/annotation/helpfulness.yaml --server $EVALSID
```

`annotations_per_item` (default 1) is how many people answer each item.
Set it to 2 or more to measure agreement between annotators.
`compare_metric` names a metric of the source run (`llm-judge`,
`llm-judge.helpfulness`, `exact-match`), and the queue's statistics then
report how well people agree with it.

## Fill it

Items come from a run's results, filtered by the same CEL condition that
`evalsi promote` uses. They can also come straight from a dataset file:

```bash
# Everything the judge was unsure about, one item per record.
evalsi annotate add helpfulness --project support --server $EVALSID \
  --run $RUN_ID --when 'scores["llm-judge.helpfulness"] > 0.3 && scores["llm-judge.helpfulness"] < 0.7'
evalsi annotate add helpfulness --project support --server $EVALSID --records sample.jsonl --limit 200
```

An item carries the record (input, output, reference, context and
trajectory) along with the run's scores for it. Annotators don't see
those scores unless they ask (`--show-scores`).

## Annotate

```bash
evalsi annotate start helpfulness --project support --server $EVALSID
```

Each annotator gets the next item they have not yet answered, held for
them under a lease (30 minutes by default; `--lease` changes it). An item
is offered only while its answers plus other annotators' live claims fall
short of `annotations_per_item`. People working at the same time therefore
share the work, and nobody answers the same item twice. In the prompt,
`s` skips an item (a skip does not count as an answer) and `q` stops.
Answering an item again replaces your earlier answer.

The same flow is available over the API (`NextItem`, then
`SubmitAnnotation`), so labelling tools can be built on it. It is also
available over REST: `POST /v1alpha1/queues/{queue}:next` and
`POST /v1alpha1/queues/{queue}/annotations`.

## Read the results

```bash
evalsi annotate stats helpfulness --project support --server $EVALSID
evalsi annotate export helpfulness --project support --server $EVALSID -o answers.jsonl
```

`stats` shows progress, answers per annotator, and the following for each
question:

- **The summary.** Each item's answers are first averaged across its
  annotators, then summarized across items.
- **Agreement between annotators.** This is Krippendorff's alpha (nominal
  for pass/fail and labels, interval for scores), computed over items with
  two or more answers. 1 is perfect agreement and 0 is chance. Below about
  0.67, the rubric is usually too vague to calibrate anything against.
- **Agreement with `compare_metric`.** For pass/fail questions this is
  accuracy and Cohen's kappa against the metric thresholded at 0.5. For
  score questions it is Pearson's r and the mean absolute error.

`export` writes every answer as one JSON line, with its item, so you can
analyze it elsewhere or use it as training data for a judge.

## Access

| Permission | Allows | Built-in roles |
|---|---|---|
| `annotations.read` | see queues, items, answers and statistics | viewer and up, `annotator` |
| `annotations.write` | claim and answer items | editor and up, `annotator` |
| `annotations.manage` | create and delete queues, add items (audited) | editor and up |

Bind the `annotator` role to the people who label. They don't need to be
able to run evaluations:

```yaml
rbac:
  projects:
    support:
      annotator: [group:corp/support-leads]
```

Adding items from a run also requires `runs.read` on that run, and the run
must be in the queue's project.
