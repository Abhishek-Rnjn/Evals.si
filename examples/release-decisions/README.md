# Synthetic production failure to enforced release decision

This example exercises the complete unreleased workflow with two synthetic refund
incidents and a scripted HTTP agent. It uses no paid model or live business tool.
Use the SDK/server/worker from this checkout; published 0.3.0 lacks the feature.

Start a disposable local server from the repository root after `cd python && uv
sync --all-packages`:

```bash
mkdir -p /tmp/evalsi-release-demo
cat > /tmp/evalsi-release-demo/config.yaml <<EOF_CONFIG
listen: 127.0.0.1:8080
data_dir: /tmp/evalsi-release-demo/data
worker:
  command: ["$(pwd)/python/.venv/bin/python", "-m", "evalsi"]
  no_cache: true
EOF_CONFIG
go run ./cmd/evalsid serve --config /tmp/evalsi-release-demo/config.yaml --no-auth
```

In another terminal, choose a **new** output directory:

```bash
python/.venv/bin/python examples/release-decisions/demo.py \
  --server http://127.0.0.1:8080 --output-dir /tmp/refund-release-controls
```

The demo:

1. Ingests two synthetic production failures, redacts their sensitive marker,
   enters expected behavior, reviews each case and publishes their exact revisions.
2. Approves the immutable shared held-out suite and confirms the observed bad
   responses fail ordinary regression gates.
3. Runs a scripted accepted baseline and records its explicit evidence pin and
   declared commit label.
4. Exercises both `evalsi release gate` and the real GitHub Action adapter for
   three candidate attempts: **PASS/0**, **BLOCK/3** and **INSUFFICIENT/1**.
5. Verifies portable report integrity, rejects a tampered report and checks CI
   summaries omit raw source markers and case identities.

Output JSON identifies the two cases, approved dataset and decision outcomes,
pins and exit codes. Reports/policies/specs and local Action output directories
are left under the chosen directory. Server runs and cases remain in the
disposable data directory. The HTTP fixture stops on completion. The script exits
nonzero if any control is unexpected; intentional block/insufficient controls
are expected and asserted.

Anonymous approval is only for this local demo. Authenticated dataset approval
requires a distinct publishing and reviewing principal. Commit labels and model
names are declarations, not signed proof of executed code. Two synthetic records
test workflow mechanics, not real-agent safety or statistical power.

See the [guide](../../docs/guides/release-decisions.md) for permissions, confidence
bounds, policy trust, retention and supported evaluator scopes.
