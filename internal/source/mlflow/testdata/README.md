# MLflow 3.17.0 fixtures

Responses recorded from `mlflow server` 3.17.0 (SQLite backend, experiment `1`) after logging one trace
(`agent` → `llm`, `calc`) with `@mlflow.trace`, then creating and patching one assessment through the
REST API (`/api/3.0/mlflow/traces/...`). They are what the connector's contract tests replay
(decision 0016, item 21): span and trace IDs inside `get`/`batchGet` are base64 (protobuf JSON), the
trace-level ID is `tr-<hex>`, and attributes are decoded OTLP `AnyValue` JSON.
