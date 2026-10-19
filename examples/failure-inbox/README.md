# Synthetic incident to regression gate

This unreleased example uses the source SDK/server. Every case and identifier
is synthetic. The corrected candidate is a scripted HTTP fixture, not a model
or a real refund application.

From the repository root, start a disposable local server with the source worker:

```bash
mkdir -p /tmp/evalsi-inbox-demo
cat > /tmp/evalsi-inbox-demo/config.yaml <<EOF
listen: 127.0.0.1:8080
data_dir: /tmp/evalsi-inbox-demo/data
worker:
  command: ["$(pwd)/python/.venv/bin/python", "-m", "evalsi"]
EOF
go run ./cmd/evalsid serve --config /tmp/evalsi-inbox-demo/config.yaml --no-auth
```

In another terminal, using the source SDK's Python environment:

```bash
python/.venv/bin/python examples/failure-inbox/demo.py \
  --server http://127.0.0.1:8080 --output /tmp/refund-incidents-v1.evalsi.json
```

The demo expects a local server with authentication disabled and default-project
access. Use the normal authenticated CLI/browser workflow for team deployments.
Its output directory must exist, and the destination must be new.

The demo logs a synthetic bad refund, captures it, replaces the sensitive input
and trajectory with a reviewed case, assigns it, approves its specific revision
and checks that recapture preserves the review. It invokes `evalsi inbox export`
to create a private digest-pinned snapshot, verifies that the removed marker is
absent, then runs two controls:

- The observed unauthorized refund fails the exact-match gate: exit **3**.
- A local HTTP target returning the reviewed expected response passes: exit **0**.

Both controls use the same pinned dataset. JSON output identifies the case,
reviewed revision, snapshot digest and exit codes. The script exits nonzero if
either control is wrong. It writes the snapshot, a run spec and two result
files next to the chosen output; the local HTTP fixture stops on completion.
The server retains the synthetic case until you delete it through the inbox.

See the [failure inbox guide](../../docs/guides/failure-inbox.md) for permissions,
concurrency, redaction, retention and API/CLI support limits.
