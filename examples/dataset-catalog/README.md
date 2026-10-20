# Synthetic incident to shared approved regression suite

This unreleased example uses the source SDK/server. All data is synthetic; the
candidate is a scripted HTTP fixture, not a real agent or a refund application.
It needs no model API keys and makes no live business calls.

From the repository root, install the development environment (`cd python && uv
sync --all-packages`) and start a disposable server with the source worker:

```bash
mkdir -p /tmp/evalsi-catalog-demo
cat > /tmp/evalsi-catalog-demo/config.yaml <<EOF_CONFIG
listen: 127.0.0.1:8080
data_dir: /tmp/evalsi-catalog-demo/data
worker:
  command: ["$(pwd)/python/.venv/bin/python", "-m", "evalsi"]
EOF_CONFIG
go run ./cmd/evalsid serve --config /tmp/evalsi-catalog-demo/config.yaml --no-auth
```

In another terminal:

```bash
python/.venv/bin/python examples/dataset-catalog/demo.py \
  --server http://127.0.0.1:8080 --output /tmp/catalog-refunds.evalsi.json
```

Use a new destination in an existing directory. The demo captures a bad refund,
redacts its input and approves an explicit expected response. It publishes that
case revision as an immutable shared draft, confirms draft admission is rejected,
approves the exact pin and checks a wrong pin is rejected before execution.
It then runs the recorded bad output (failed gate) and the scripted corrected
candidate (successful gate), checking the actual target was called and the
stored run retains the dataset identity. It downloads a portable snapshot,
retires the version and confirms new admission is blocked while previous
results remain queryable. A unique dataset name allows reruns.

The demo exits nonzero on an unexpected control and prints a JSON report. It
leaves the version retired and the source case/trace and run evidence in the
disposable server; stop the server and remove its demo data when finished.
It permits anonymous review only because authentication is disabled locally.
Authenticated deployments require a different publishing and approving principal.

See the [catalog guide](../../docs/guides/dataset-catalog.md) for authorization,
integrity, retention and size limits. These synthetic controls do not establish
real-agent safety or enterprise readiness.
