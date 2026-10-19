# Review and pin a dataset version

These **synthetic recorded outputs** demonstrate refunds, authorization and
duplicate prevention. They do not execute a real agent or establish a real
business policy. The expected decisions are example labels for review.
Requires the unreleased snapshot commands from this checkout; PyPI 0.3.0 does
not include the importer. No model or API key is needed.

From the repository root:

```bash
evalsi dataset inspect examples/dataset-snapshots/support-v1.evalsi.json
evalsi dataset diff examples/dataset-snapshots/support-v1.evalsi.json examples/dataset-snapshots/support-v2.evalsi.json
evalsi run -f examples/dataset-snapshots/gate-v1.yaml
evalsi run -f examples/dataset-snapshots/gate-v2.yaml
```

The first run exits **0**. The second must exit **3**: the recorded answer grants
an unauthorized refund. The diff identifies that record's `output` change without
printing the input, output or reference. Exit **1** indicates a configuration or
integrity error; it is not the expected negative control.

Both specs pin a snapshot's SHA-256. The bundles retain explicit IDs, their owner,
held-out split label, source digest and synthetic provenance. Snapshot creation
refuses an existing path. To reproduce a bundle under a new path:

```bash
evalsi dataset snapshot examples/dataset-snapshots/source-v1.jsonl \
  --name support-refunds --version v1 --owner example-maintainers \
  --split held-out --tag synthetic -o /tmp/support-v1.evalsi.json
```

The printed digest must match the checked-in `gate-v1.yaml`. Same options and
source bytes produce the same identity; there is no creation timestamp.
Artifact JSON whitespace/key ordering does not affect its canonical digest.

Changing the dataset requires reviewing the diff and the new pin. Existing paired
baselines require the same dataset: do not treat a new dataset version's scores as
an improvement of the agent under a fixed test set. Raw bundles contain the full
records; use only approved data and an appropriate storage location.

See the [dataset guide](../../docs/guides/dataset-snapshots.md) for verification,
server operation, support limits and the remaining central-registry work.
